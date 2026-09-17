-- +goose Up

-- +goose StatementBegin
DO $$
BEGIN
    IF current_database() <> 'pm_app' OR current_user <> 'pm_app_migrator' THEN
        RAISE EXCEPTION 'tenancy migration requires pm_app_migrator on pm_app';
    END IF;
    IF to_regrole('pm_app_runtime') IS NULL THEN
        RAISE EXCEPTION 'required runtime role is missing';
    END IF;
    IF EXISTS (
        SELECT 1
        FROM pg_roles
        WHERE rolname = 'pm_app_runtime'
          AND (rolsuper OR rolbypassrls OR rolcreatedb OR rolcreaterole OR rolreplication)
    ) THEN
        RAISE EXCEPTION 'runtime role has prohibited PostgreSQL authority';
    END IF;
    IF pg_has_role('pm_app_runtime', 'pm_app_migrator', 'MEMBER') THEN
        RAISE EXCEPTION 'runtime role must not inherit or assume migrator ownership';
    END IF;
END
$$;
-- +goose StatementEnd

ALTER TABLE pm.control_schema DROP CONSTRAINT control_schema_revision_check;
UPDATE pm.control_schema
SET revision = 'pm.control/v2', installed_at = clock_timestamp()
WHERE singleton AND revision = 'pm.control/v1';
ALTER TABLE pm.control_schema
    ADD CONSTRAINT control_schema_revision_check CHECK (revision = 'pm.control/v2');

CREATE TABLE pm.workspace_namespaces (
    org_id text NOT NULL CHECK (org_id <> ''),
    workspace_id text NOT NULL CHECK (workspace_id <> ''),
    name text NOT NULL CHECK (name <> ''),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    tombstoned_at timestamptz,
    PRIMARY KEY (org_id, workspace_id),
    CHECK (tombstoned_at IS NULL OR tombstoned_at >= created_at)
);

-- +goose StatementBegin
CREATE FUNCTION pm.enforce_workspace_tombstone_immutable()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF OLD.tombstoned_at IS NOT NULL
       AND NEW.tombstoned_at IS DISTINCT FROM OLD.tombstoned_at THEN
        RAISE EXCEPTION USING
            ERRCODE = '23514',
            MESSAGE = 'workspace tombstone is immutable';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

REVOKE ALL ON FUNCTION pm.enforce_workspace_tombstone_immutable()
    FROM PUBLIC, pm_app_runtime;
CREATE TRIGGER workspace_tombstone_immutable
BEFORE UPDATE OF tombstoned_at ON pm.workspace_namespaces
FOR EACH ROW
EXECUTE FUNCTION pm.enforce_workspace_tombstone_immutable();

CREATE TABLE pm.identities (
    org_id text NOT NULL CHECK (org_id <> ''),
    identity_id text NOT NULL CHECK (identity_id <> ''),
    kind text NOT NULL CHECK (kind IN ('user', 'service_account')),
    issuer text,
    subject text,
    revision bigint NOT NULL CHECK (revision > 0),
    PRIMARY KEY (org_id, identity_id),
    UNIQUE (org_id, issuer, subject),
    CHECK (
        (kind = 'user' AND issuer IS NOT NULL AND issuer <> '' AND subject IS NOT NULL AND subject <> '')
        OR
        (kind = 'service_account' AND issuer IS NULL AND subject IS NULL)
    )
);

CREATE TABLE pm.memberships (
    org_id text NOT NULL,
    workspace_id text NOT NULL,
    membership_id text NOT NULL CHECK (membership_id <> ''),
    identity_id text NOT NULL CHECK (identity_id <> ''),
    role text NOT NULL CHECK (role <> ''),
    policy_epoch bigint NOT NULL CHECK (policy_epoch > 0),
    expires_at timestamptz,
    revoked_at timestamptz,
    PRIMARY KEY (org_id, workspace_id, membership_id),
    UNIQUE (org_id, workspace_id, identity_id),
    FOREIGN KEY (org_id, workspace_id)
        REFERENCES pm.workspace_namespaces (org_id, workspace_id),
    FOREIGN KEY (org_id, identity_id)
        REFERENCES pm.identities (org_id, identity_id)
);

CREATE TABLE pm.identity_mutation_keys (
    org_id text NOT NULL,
    workspace_id text NOT NULL,
    principal_id text NOT NULL CHECK (principal_id <> ''),
    operation_id text NOT NULL CHECK (operation_id <> ''),
    target_id text NOT NULL CHECK (target_id <> ''),
    idempotency_key text NOT NULL CHECK (idempotency_key <> ''),
    intent_digest text NOT NULL CHECK (intent_digest ~ '^sha256:[0-9a-f]{64}$'),
    result_ref text NOT NULL CHECK (result_ref <> ''),
    result_revision bigint NOT NULL CHECK (result_revision > 0),
    recorded_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (
        org_id, workspace_id, principal_id, operation_id, target_id, idempotency_key
    ),
    FOREIGN KEY (org_id, workspace_id)
        REFERENCES pm.workspace_namespaces (org_id, workspace_id),
    FOREIGN KEY (org_id, principal_id)
        REFERENCES pm.identities (org_id, identity_id)
);

CREATE TABLE pm.provider_accounts (
    org_id text NOT NULL CHECK (org_id <> ''),
    provider_account_id text NOT NULL CHECK (provider_account_id <> ''),
    provider text NOT NULL CHECK (provider <> ''),
    account_witness text NOT NULL CHECK (account_witness <> ''),
    capacity bigint NOT NULL CHECK (capacity > 0),
    reserved_units bigint NOT NULL DEFAULT 0,
    revision bigint NOT NULL CHECK (revision > 0),
    PRIMARY KEY (org_id, provider_account_id),
    UNIQUE (org_id, provider, account_witness),
    UNIQUE (org_id, provider_account_id, account_witness),
    CHECK (reserved_units >= 0 AND reserved_units <= capacity)
);

CREATE TABLE pm.connections (
    org_id text NOT NULL,
    workspace_id text NOT NULL,
    connection_id text NOT NULL CHECK (connection_id <> ''),
    connector_id text NOT NULL CHECK (connector_id <> ''),
    provider_account_id text NOT NULL CHECK (provider_account_id <> ''),
    revision bigint NOT NULL CHECK (revision > 0),
    credential_ref text NOT NULL,
    PRIMARY KEY (org_id, workspace_id, connection_id),
    UNIQUE (org_id, workspace_id, connection_id, provider_account_id),
    UNIQUE (org_id, workspace_id, connection_id, provider_account_id, credential_ref),
    FOREIGN KEY (org_id, workspace_id)
        REFERENCES pm.workspace_namespaces (org_id, workspace_id),
    FOREIGN KEY (org_id, provider_account_id)
        REFERENCES pm.provider_accounts (org_id, provider_account_id)
);

CREATE TABLE pm.encrypted_secrets (
    org_id text NOT NULL,
    workspace_id text NOT NULL,
    credential_ref text NOT NULL CHECK (credential_ref <> ''),
    ciphertext_version bigint NOT NULL CHECK (ciphertext_version > 0),
    connection_id text NOT NULL CHECK (connection_id <> ''),
    provider_account_id text NOT NULL CHECK (provider_account_id <> ''),
    account_witness text NOT NULL CHECK (account_witness <> ''),
    key_ref text NOT NULL CHECK (key_ref <> ''),
    scopes jsonb NOT NULL CHECK (jsonb_typeof(scopes) = 'array'),
    auth_epoch bigint NOT NULL CHECK (auth_epoch > 0),
    ciphertext bytea NOT NULL CHECK (octet_length(ciphertext) > 0 AND octet_length(ciphertext) <= 1048576),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (org_id, workspace_id, credential_ref, ciphertext_version),
    FOREIGN KEY (org_id, workspace_id, connection_id, provider_account_id, credential_ref)
        REFERENCES pm.connections (
            org_id, workspace_id, connection_id, provider_account_id, credential_ref
        ),
    FOREIGN KEY (org_id, provider_account_id, account_witness)
        REFERENCES pm.provider_accounts (org_id, provider_account_id, account_witness)
);

-- These are scoped identity roots only. Later plan/approval owners supply the
-- immutable content and execution policy; this migration supplies no execution engine.
CREATE TABLE pm.plan_bindings (
    org_id text NOT NULL,
    workspace_id text NOT NULL,
    plan_id text NOT NULL CHECK (plan_id <> ''),
    plan_revision text NOT NULL CHECK (plan_revision <> ''),
    binding_digest text NOT NULL CHECK (binding_digest ~ '^sha256:[0-9a-f]{64}$'),
    recorded_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (org_id, workspace_id, plan_id, plan_revision),
    FOREIGN KEY (org_id, workspace_id)
        REFERENCES pm.workspace_namespaces (org_id, workspace_id)
);

CREATE TABLE pm.approval_bindings (
    org_id text NOT NULL,
    workspace_id text NOT NULL,
    approval_id text NOT NULL CHECK (approval_id <> ''),
    plan_id text NOT NULL CHECK (plan_id <> ''),
    plan_revision text NOT NULL CHECK (plan_revision <> ''),
    binding_digest text NOT NULL CHECK (binding_digest ~ '^sha256:[0-9a-f]{64}$'),
    recorded_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (org_id, workspace_id, approval_id),
    UNIQUE (org_id, workspace_id, approval_id, plan_id, plan_revision),
    FOREIGN KEY (org_id, workspace_id, plan_id, plan_revision)
        REFERENCES pm.plan_bindings (org_id, workspace_id, plan_id, plan_revision)
);

CREATE TABLE pm.run_plan_bindings (
    org_id text NOT NULL,
    workspace_id text NOT NULL,
    run_id text NOT NULL CHECK (run_id <> ''),
    plan_id text NOT NULL CHECK (plan_id <> ''),
    plan_revision text NOT NULL CHECK (plan_revision <> ''),
    approval_id text NOT NULL CHECK (approval_id <> ''),
    bound_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (org_id, workspace_id, run_id),
    FOREIGN KEY (org_id, workspace_id, run_id)
        REFERENCES pm.run_specs (org_id, workspace_id, run_id),
    FOREIGN KEY (org_id, workspace_id, plan_id, plan_revision)
        REFERENCES pm.plan_bindings (org_id, workspace_id, plan_id, plan_revision),
    FOREIGN KEY (org_id, workspace_id, approval_id, plan_id, plan_revision)
        REFERENCES pm.approval_bindings (
            org_id, workspace_id, approval_id, plan_id, plan_revision
        )
);

CREATE TABLE pm.account_permits (
    org_id text NOT NULL,
    workspace_id text NOT NULL,
    permit_id text NOT NULL CHECK (permit_id <> ''),
    connection_id text NOT NULL CHECK (connection_id <> ''),
    provider_account_id text NOT NULL CHECK (provider_account_id <> ''),
    units bigint NOT NULL CHECK (units > 0),
    state text NOT NULL CHECK (state IN ('reserved', 'dispatched', 'unknown', 'resolved', 'expired')),
    fence bigint NOT NULL CHECK (fence > 0),
    owner_id text NOT NULL CHECK (owner_id <> ''),
    reserved_at timestamptz NOT NULL,
    expires_at timestamptz NOT NULL,
    resolved_at timestamptz,
    PRIMARY KEY (org_id, workspace_id, permit_id),
    CHECK (expires_at > reserved_at),
    CHECK (
        (state IN ('reserved', 'dispatched', 'unknown') AND resolved_at IS NULL)
        OR
        (state IN ('resolved', 'expired') AND resolved_at IS NOT NULL AND resolved_at >= reserved_at)
    ),
    FOREIGN KEY (org_id, workspace_id, connection_id, provider_account_id)
        REFERENCES pm.connections (org_id, workspace_id, connection_id, provider_account_id)
);

ALTER TABLE pm.manifests
    ADD CONSTRAINT manifests_scoped_revision_unique
    UNIQUE (org_id, workspace_id, resource_id, revision);

CREATE TABLE pm.checkpoints (
    org_id text NOT NULL,
    workspace_id text NOT NULL,
    connection_id text NOT NULL CHECK (connection_id <> ''),
    resource_id text NOT NULL CHECK (resource_id <> ''),
    source_generation text NOT NULL CHECK (source_generation <> ''),
    schema_fingerprint text NOT NULL CHECK (schema_fingerprint ~ '^sha256:[0-9a-f]{64}$'),
    partition_key text NOT NULL CHECK (partition_key <> ''),
    continuation_present boolean NOT NULL,
    continuation_kind text NOT NULL,
    continuation_bytes bytea,
    tie_breaker text NOT NULL,
    unread_tail_ref text NOT NULL,
    committed_manifest_id text NOT NULL CHECK (committed_manifest_id <> ''),
    committed_manifest_revision bigint NOT NULL CHECK (committed_manifest_revision > 0),
    revision bigint NOT NULL CHECK (revision > 0),
    writer_run_id text NOT NULL CHECK (writer_run_id <> ''),
    writer_task_id text NOT NULL CHECK (writer_task_id <> ''),
    writer_owner_id text NOT NULL CHECK (writer_owner_id <> ''),
    writer_fence bigint NOT NULL CHECK (writer_fence > 0),
    committed_at timestamptz NOT NULL,
    PRIMARY KEY (org_id, workspace_id, connection_id, resource_id),
    CHECK (
        (NOT continuation_present AND continuation_kind = '' AND continuation_bytes IS NULL)
        OR
        (continuation_present AND continuation_kind <> '' AND continuation_bytes IS NOT NULL AND octet_length(continuation_bytes) <= 1048576)
    ),
    FOREIGN KEY (org_id, workspace_id, connection_id)
        REFERENCES pm.connections (org_id, workspace_id, connection_id),
    FOREIGN KEY (org_id, workspace_id, committed_manifest_id, committed_manifest_revision)
        REFERENCES pm.manifests (org_id, workspace_id, resource_id, revision)
        DEFERRABLE INITIALLY DEFERRED,
    FOREIGN KEY (org_id, workspace_id, writer_run_id, writer_task_id)
        REFERENCES pm.tasks (org_id, workspace_id, run_id, task_id)
);

ALTER TABLE pm.outbox_commands
    ADD COLUMN claim_owner text,
    ADD COLUMN claim_fence bigint NOT NULL DEFAULT 0 CHECK (claim_fence >= 0),
    ADD COLUMN claim_expires_at timestamptz,
    ADD COLUMN acknowledged_at timestamptz,
    ADD CONSTRAINT outbox_claim_owner_expiry_agree CHECK (
        (claim_owner IS NULL AND claim_expires_at IS NULL)
        OR
        (claim_owner IS NOT NULL AND claim_owner <> '' AND claim_expires_at IS NOT NULL)
    );

CREATE INDEX account_permits_account_state_idx
    ON pm.account_permits (org_id, provider_account_id, state, expires_at);
CREATE INDEX checkpoints_manifest_idx
    ON pm.checkpoints (org_id, workspace_id, committed_manifest_id, committed_manifest_revision);
CREATE INDEX outbox_claimable_idx
    ON pm.outbox_commands (state, available_at, claim_expires_at)
    WHERE acknowledged_at IS NULL;

-- +goose StatementBegin
DO $$
DECLARE
    scoped_table text;
BEGIN
    FOREACH scoped_table IN ARRAY ARRAY[
        'workspace_namespaces', 'memberships', 'identity_mutation_keys',
        'connections', 'encrypted_secrets', 'plan_bindings', 'approval_bindings',
        'run_plan_bindings', 'account_permits', 'checkpoints'
    ]
    LOOP
        EXECUTE format('ALTER TABLE pm.%I ENABLE ROW LEVEL SECURITY', scoped_table);
        EXECUTE format('ALTER TABLE pm.%I FORCE ROW LEVEL SECURITY', scoped_table);
        EXECUTE format(
            'CREATE POLICY pm_scope ON pm.%I USING (' ||
            'org_id = NULLIF(current_setting(''pm.org_id'', true), '''') AND ' ||
            'workspace_id = NULLIF(current_setting(''pm.workspace_id'', true), '''')' ||
            ') WITH CHECK (' ||
            'org_id = NULLIF(current_setting(''pm.org_id'', true), '''') AND ' ||
            'workspace_id = NULLIF(current_setting(''pm.workspace_id'', true), '''')' ||
            ')',
            scoped_table
        );
        EXECUTE format('REVOKE ALL ON TABLE pm.%I FROM PUBLIC, pm_app_runtime', scoped_table);
        EXECUTE format('GRANT SELECT ON TABLE pm.%I TO pm_app_runtime', scoped_table);
    END LOOP;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
DO $$
DECLARE
    org_table text;
BEGIN
    FOREACH org_table IN ARRAY ARRAY['identities', 'provider_accounts']
    LOOP
        EXECUTE format('ALTER TABLE pm.%I ENABLE ROW LEVEL SECURITY', org_table);
        EXECUTE format('ALTER TABLE pm.%I FORCE ROW LEVEL SECURITY', org_table);
        EXECUTE format(
            'CREATE POLICY pm_org_scope ON pm.%I USING (' ||
            'org_id = NULLIF(current_setting(''pm.org_id'', true), '''') AND ' ||
            'NULLIF(current_setting(''pm.workspace_id'', true), '''') IS NOT NULL' ||
            ') WITH CHECK (' ||
            'org_id = NULLIF(current_setting(''pm.org_id'', true), '''') AND ' ||
            'NULLIF(current_setting(''pm.workspace_id'', true), '''') IS NOT NULL' ||
            ')',
            org_table
        );
        EXECUTE format('REVOKE ALL ON TABLE pm.%I FROM PUBLIC, pm_app_runtime', org_table);
        EXECUTE format('GRANT SELECT ON TABLE pm.%I TO pm_app_runtime', org_table);
    END LOOP;
END
$$;
-- +goose StatementEnd

GRANT INSERT (org_id, workspace_id, name)
    ON pm.workspace_namespaces TO pm_app_runtime;
GRANT UPDATE (tombstoned_at)
    ON pm.workspace_namespaces TO pm_app_runtime;
GRANT INSERT (org_id, identity_id, kind, issuer, subject, revision)
    ON pm.identities TO pm_app_runtime;
GRANT INSERT (
    org_id, workspace_id, membership_id, identity_id, role,
    policy_epoch, expires_at, revoked_at
) ON pm.memberships TO pm_app_runtime;
GRANT INSERT (
    org_id, workspace_id, principal_id, operation_id, target_id,
    idempotency_key, intent_digest, result_ref, result_revision
) ON pm.identity_mutation_keys TO pm_app_runtime;
GRANT INSERT (
    org_id, provider_account_id, provider, account_witness, capacity, revision
) ON pm.provider_accounts TO pm_app_runtime;
GRANT UPDATE (reserved_units)
    ON pm.provider_accounts TO pm_app_runtime;
GRANT INSERT (
    org_id, workspace_id, connection_id, connector_id,
    provider_account_id, revision, credential_ref
) ON pm.connections TO pm_app_runtime;
GRANT INSERT (
    org_id, workspace_id, credential_ref, ciphertext_version,
    connection_id, provider_account_id, account_witness, key_ref,
    scopes, auth_epoch, ciphertext
) ON pm.encrypted_secrets TO pm_app_runtime;
GRANT INSERT (org_id, workspace_id, plan_id, plan_revision, binding_digest)
    ON pm.plan_bindings TO pm_app_runtime;
GRANT INSERT (
    org_id, workspace_id, approval_id, plan_id, plan_revision, binding_digest
) ON pm.approval_bindings TO pm_app_runtime;
GRANT INSERT (
    org_id, workspace_id, run_id, plan_id, plan_revision, approval_id
) ON pm.run_plan_bindings TO pm_app_runtime;
GRANT INSERT (
    org_id, workspace_id, permit_id, connection_id, provider_account_id,
    units, state, fence, owner_id, reserved_at, expires_at, resolved_at
) ON pm.account_permits TO pm_app_runtime;
GRANT UPDATE (state, fence, owner_id, expires_at, resolved_at)
    ON pm.account_permits TO pm_app_runtime;
GRANT INSERT (
    org_id, workspace_id, connection_id, resource_id,
    source_generation, schema_fingerprint, partition_key,
    continuation_present, continuation_kind, continuation_bytes,
    tie_breaker, unread_tail_ref,
    committed_manifest_id, committed_manifest_revision, revision,
    writer_run_id, writer_task_id, writer_owner_id, writer_fence, committed_at
) ON pm.checkpoints TO pm_app_runtime;
GRANT UPDATE (
    source_generation, schema_fingerprint, partition_key,
    continuation_present, continuation_kind, continuation_bytes,
    tie_breaker, unread_tail_ref,
    committed_manifest_id, committed_manifest_revision, revision,
    writer_run_id, writer_task_id, writer_owner_id, writer_fence, committed_at
) ON pm.checkpoints TO pm_app_runtime;
REVOKE INSERT ON pm.outbox_commands FROM pm_app_runtime;
GRANT INSERT (
    org_id, workspace_id, run_id, command_id,
    kind, payload_ref, created_at, available_at
) ON pm.outbox_commands TO pm_app_runtime;
GRANT UPDATE (claim_owner, claim_fence, claim_expires_at, acknowledged_at)
    ON pm.outbox_commands TO pm_app_runtime;
