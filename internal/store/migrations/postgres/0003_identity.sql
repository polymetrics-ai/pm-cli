-- Authenticated identity and scoped mutation authority.
-- Goose owns the transaction; each section is one SQL batch.

-- +goose Up
-- +goose StatementBegin
DO $$
BEGIN
    IF current_database() <> 'pm_app' OR current_user <> 'pm_app_migrator' THEN
        RAISE EXCEPTION 'identity schema integration requires pm_app_migrator on pm_app';
    END IF;
    IF to_regrole('pm_app_runtime') IS NULL THEN
        RAISE EXCEPTION 'required runtime role is missing';
    END IF;
    IF EXISTS (
        SELECT 1 FROM pg_roles
        WHERE rolname = 'pm_app_runtime'
          AND (rolsuper OR rolbypassrls OR rolcreatedb OR rolcreaterole OR rolreplication)
    ) OR pg_has_role('pm_app_runtime', 'pm_app_migrator', 'MEMBER') THEN
        RAISE EXCEPTION 'runtime role has prohibited PostgreSQL authority';
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM pm.control_schema
        WHERE singleton AND revision = 'pm.control/v2'
    ) THEN
        RAISE EXCEPTION 'identity migration requires the preceding control schema';
    END IF;
END
$$;

CREATE FUNCTION pm.text_array_is_strictly_sorted(items text[])
RETURNS boolean
LANGUAGE sql
IMMUTABLE
STRICT
PARALLEL SAFE
SET search_path = pg_catalog
AS $$
    SELECT array_position($1, NULL) IS NULL
       AND NOT EXISTS (
           SELECT 1
           FROM unnest($1) WITH ORDINALITY AS item(value, position)
           WHERE item.position > 1
             AND item.value <= $1[(item.position - 1)::integer]
       )
$$;
REVOKE ALL ON FUNCTION pm.text_array_is_strictly_sorted(text[]) FROM PUBLIC, pm_app_runtime;
GRANT EXECUTE ON FUNCTION pm.text_array_is_strictly_sorted(text[]) TO pm_app_runtime;

ALTER TABLE pm.memberships
    ADD COLUMN resource_grants text[] NOT NULL DEFAULT ARRAY[]::text[],
    ADD COLUMN permissions text[] NOT NULL DEFAULT ARRAY[]::text[],
    ADD COLUMN connection_ids text[] NOT NULL DEFAULT ARRAY[]::text[],
    ADD COLUMN operation_ids text[] NOT NULL DEFAULT ARRAY[]::text[],
    ADD COLUMN control_revision bigint NOT NULL DEFAULT 1 CHECK (control_revision > 0),
    ADD CONSTRAINT membership_resource_grants_sorted CHECK (pm.text_array_is_strictly_sorted(resource_grants)),
    ADD CONSTRAINT membership_permissions_sorted CHECK (pm.text_array_is_strictly_sorted(permissions)),
    ADD CONSTRAINT membership_connections_sorted CHECK (pm.text_array_is_strictly_sorted(connection_ids)),
    ADD CONSTRAINT membership_operations_sorted CHECK (pm.text_array_is_strictly_sorted(operation_ids));

ALTER TABLE pm.connections
    ADD COLUMN display_name text NOT NULL DEFAULT 'connection' CHECK (display_name <> ''),
    ADD COLUMN state text NOT NULL DEFAULT 'active' CHECK (state IN ('active', 'revoked')),
    ADD COLUMN revoked_at timestamptz,
    ADD COLUMN updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    ADD CONSTRAINT connection_revocation_state_agrees CHECK ((state = 'active' AND revoked_at IS NULL) OR (state = 'revoked' AND revoked_at IS NOT NULL));

ALTER TABLE pm.identity_mutation_keys
    ALTER COLUMN result_ref DROP NOT NULL,
    ALTER COLUMN result_revision DROP NOT NULL,
    ADD COLUMN expected_revision bigint NOT NULL DEFAULT 1 CHECK (expected_revision > 0),
    ADD COLUMN receipt_state text NOT NULL DEFAULT 'compacted' CHECK (receipt_state IN ('pending', 'resolved', 'compacted')),
    ADD COLUMN response_status integer,
    ADD COLUMN safe_result jsonb,
    ADD COLUMN resolved_at timestamptz,
    ADD COLUMN response_expires_at timestamptz,
    ADD COLUMN compacted_at timestamptz,
    ADD COLUMN updated_at timestamptz NOT NULL DEFAULT clock_timestamp();

CREATE POLICY pm_identity_mutation_migration_write ON pm.identity_mutation_keys
    FOR UPDATE TO pm_app_migrator
    USING (true)
    WITH CHECK (true);
UPDATE pm.identity_mutation_keys
SET expected_revision = result_revision,
    receipt_state = 'compacted',
    resolved_at = recorded_at,
    response_expires_at = recorded_at,
    compacted_at = recorded_at,
    updated_at = recorded_at;
DROP POLICY pm_identity_mutation_migration_write ON pm.identity_mutation_keys;

ALTER TABLE pm.identity_mutation_keys
    ADD CONSTRAINT identity_mutation_result_ref_valid CHECK (result_ref IS NULL OR result_ref <> ''),
    ADD CONSTRAINT identity_mutation_result_revision_valid CHECK (result_revision IS NULL OR result_revision > 0),
    ADD CONSTRAINT identity_mutation_response_status_valid CHECK (response_status IS NULL OR response_status IN (200, 201)),
    ADD CONSTRAINT identity_mutation_receipt_shape CHECK (
        (receipt_state = 'pending' AND result_ref IS NULL AND result_revision IS NULL AND response_status IS NULL AND safe_result IS NULL AND resolved_at IS NULL AND response_expires_at IS NULL AND compacted_at IS NULL)
        OR
        (receipt_state = 'resolved' AND result_ref IS NOT NULL AND result_revision IS NOT NULL AND response_status IS NOT NULL AND safe_result IS NOT NULL AND resolved_at IS NOT NULL AND response_expires_at IS NOT NULL AND compacted_at IS NULL)
        OR
        (receipt_state = 'compacted' AND result_ref IS NOT NULL AND result_revision IS NOT NULL AND response_status IS NULL AND safe_result IS NULL AND (
            (resolved_at IS NULL AND response_expires_at IS NULL AND compacted_at IS NULL)
            OR
            (resolved_at IS NOT NULL AND response_expires_at IS NOT NULL AND compacted_at IS NOT NULL)
        ))
    );

CREATE TABLE pm.auth_bootstrap_identities (
    deployment_id text NOT NULL CHECK (deployment_id <> ''),
    issuer text NOT NULL CHECK (issuer <> ''),
    subject text NOT NULL CHECK (subject <> ''),
    org_id text NOT NULL CHECK (org_id <> ''),
    principal_id text NOT NULL CHECK (principal_id <> ''),
    principal_kind text NOT NULL CHECK (principal_kind IN ('user', 'service_account')),
    can_approve_devices boolean NOT NULL DEFAULT false,
    enabled boolean NOT NULL DEFAULT true,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    disabled_at timestamptz,
    PRIMARY KEY (deployment_id, issuer, subject),
    FOREIGN KEY (org_id, principal_id) REFERENCES pm.identities (org_id, identity_id),
    CHECK ((enabled AND disabled_at IS NULL) OR (NOT enabled AND disabled_at IS NOT NULL))
);

CREATE TABLE pm.pm_sessions (
    deployment_id text NOT NULL CHECK (deployment_id <> ''),
    session_id text NOT NULL CHECK (session_id <> ''),
    org_id text NOT NULL CHECK (org_id <> ''),
    principal_id text NOT NULL CHECK (principal_id <> ''),
    principal_kind text NOT NULL CHECK (principal_kind IN ('user', 'service_account')),
    transport text NOT NULL CHECK (transport IN ('browser', 'device')),
    token_hash bytea NOT NULL CHECK (octet_length(token_hash) = 32),
    csrf_hash bytea,
    authenticated_at timestamptz,
    bootstrap_issuer text CHECK (bootstrap_issuer IS NULL OR bootstrap_issuer <> ''),
    bootstrap_subject text CHECK (bootstrap_subject IS NULL OR bootstrap_subject <> ''),
    issued_at timestamptz NOT NULL,
    expires_at timestamptz NOT NULL,
    revoked_at timestamptz,
    PRIMARY KEY (deployment_id, session_id),
    UNIQUE (deployment_id, token_hash),
    FOREIGN KEY (org_id, principal_id) REFERENCES pm.identities (org_id, identity_id),
    CHECK (expires_at > issued_at),
    CHECK ((transport = 'browser' AND csrf_hash IS NOT NULL AND octet_length(csrf_hash) = 32) OR (transport = 'device' AND csrf_hash IS NULL)),
    CHECK (authenticated_at IS NULL OR (transport = 'browser' AND authenticated_at <= issued_at)),
    CHECK ((bootstrap_issuer IS NULL) = (bootstrap_subject IS NULL)),
    CHECK (bootstrap_issuer IS NULL OR transport = 'browser')
);

CREATE INDEX pm_sessions_principal_active_idx
    ON pm.pm_sessions (deployment_id, org_id, principal_id, principal_kind, expires_at)
    WHERE revoked_at IS NULL;

CREATE TABLE pm.oidc_login_challenges (
    deployment_id text NOT NULL CHECK (deployment_id <> ''),
    challenge_id text NOT NULL CHECK (challenge_id <> ''),
    state_hash bytea NOT NULL CHECK (octet_length(state_hash) = 32),
    nonce_hash bytea NOT NULL CHECK (octet_length(nonce_hash) = 32),
    verifier_hash bytea NOT NULL CHECK (octet_length(verifier_hash) = 32),
    origin text NOT NULL CHECK (origin <> ''),
    host text NOT NULL CHECK (host <> ''),
    reauthentication_requested boolean NOT NULL DEFAULT false,
    expires_at timestamptz NOT NULL,
    claim_id text,
    claimed_at timestamptz,
    consumed_at timestamptz,
    rejected_at timestamptz,
    rejection_reason text,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (deployment_id, challenge_id),
    UNIQUE (deployment_id, state_hash),
    UNIQUE (deployment_id, claim_id),
    CHECK ((claim_id IS NULL AND claimed_at IS NULL) OR (claim_id IS NOT NULL AND claim_id <> '' AND claimed_at IS NOT NULL)),
    CHECK (NOT (consumed_at IS NOT NULL AND rejected_at IS NOT NULL)),
    CHECK ((rejected_at IS NULL AND rejection_reason IS NULL) OR (rejected_at IS NOT NULL AND rejection_reason IS NOT NULL AND rejection_reason <> ''))
);

CREATE TABLE pm.device_start_windows (
    deployment_id text NOT NULL CHECK (deployment_id <> ''),
    dimension_kind text NOT NULL CHECK (dimension_kind IN ('client', 'device')),
    dimension_hash bytea NOT NULL CHECK (octet_length(dimension_hash) = 32),
    window_started_at timestamptz NOT NULL,
    starts bigint NOT NULL CHECK (starts > 0),
    updated_at timestamptz NOT NULL,
    PRIMARY KEY (deployment_id, dimension_kind, dimension_hash)
);

CREATE TABLE pm.device_login_challenges (
    deployment_id text NOT NULL CHECK (deployment_id <> ''),
    challenge_id text NOT NULL CHECK (challenge_id <> ''),
    device_hash bytea NOT NULL CHECK (octet_length(device_hash) = 32),
    client_hash bytea NOT NULL CHECK (octet_length(client_hash) = 32),
    proof_hash bytea NOT NULL CHECK (octet_length(proof_hash) = 32),
    user_code_hash bytea NOT NULL CHECK (octet_length(user_code_hash) = 32),
    state text NOT NULL CHECK (state IN ('pending', 'approved', 'denied', 'expired', 'consumed')),
    poll_interval_seconds bigint NOT NULL CHECK (poll_interval_seconds > 0),
    poll_count bigint NOT NULL DEFAULT 0 CHECK (poll_count >= 0),
    next_poll_at timestamptz NOT NULL,
    expires_at timestamptz NOT NULL,
    approved_org_id text,
    approved_principal_id text,
    approved_principal_kind text CHECK (approved_principal_kind IS NULL OR approved_principal_kind IN ('user', 'service_account')),
    decided_at timestamptz,
    consumed_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (deployment_id, challenge_id),
    UNIQUE (deployment_id, proof_hash),
    UNIQUE (deployment_id, user_code_hash),
    FOREIGN KEY (approved_org_id, approved_principal_id) REFERENCES pm.identities (org_id, identity_id),
    CHECK ((approved_org_id IS NULL) = (approved_principal_id IS NULL) AND (approved_org_id IS NULL) = (approved_principal_kind IS NULL)),
    CHECK ((state = 'pending' AND decided_at IS NULL AND consumed_at IS NULL) OR (state IN ('approved', 'denied', 'expired') AND decided_at IS NOT NULL AND consumed_at IS NULL) OR (state = 'consumed' AND decided_at IS NOT NULL AND consumed_at IS NOT NULL)),
    CHECK ((state IN ('approved', 'consumed')) = (approved_org_id IS NOT NULL))
);

CREATE TABLE pm.service_accounts (
    org_id text NOT NULL,
    workspace_id text NOT NULL,
    service_account_id text NOT NULL CHECK (service_account_id <> ''),
    display_name text CHECK (display_name IS NULL OR length(display_name) <= 256),
    permissions text[] NOT NULL,
    connection_ids text[] NOT NULL,
    operation_ids text[] NOT NULL,
    policy_epoch bigint NOT NULL CHECK (policy_epoch > 0),
    control_revision bigint NOT NULL CHECK (control_revision > 0),
    expires_at timestamptz NOT NULL,
    revoked_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (org_id, workspace_id, service_account_id),
    FOREIGN KEY (org_id, workspace_id) REFERENCES pm.workspace_namespaces (org_id, workspace_id),
    FOREIGN KEY (org_id, service_account_id) REFERENCES pm.identities (org_id, identity_id),
    CHECK (pm.text_array_is_strictly_sorted(permissions)),
    CHECK (pm.text_array_is_strictly_sorted(connection_ids)),
    CHECK (pm.text_array_is_strictly_sorted(operation_ids))
);

CREATE TABLE pm.service_account_credentials (
    deployment_id text NOT NULL CHECK (deployment_id <> ''),
    org_id text NOT NULL,
    workspace_id text NOT NULL,
    credential_id text NOT NULL CHECK (credential_id <> ''),
    service_account_id text NOT NULL CHECK (service_account_id <> ''),
    secret_hash bytea NOT NULL CHECK (octet_length(secret_hash) = 32),
    issued_at timestamptz NOT NULL,
    expires_at timestamptz NOT NULL,
    revoked_at timestamptz,
    PRIMARY KEY (deployment_id, credential_id),
    UNIQUE (deployment_id, secret_hash),
    FOREIGN KEY (org_id, workspace_id, service_account_id) REFERENCES pm.service_accounts (org_id, workspace_id, service_account_id),
    CHECK (expires_at > issued_at)
);

CREATE TABLE pm.delegations (
    org_id text NOT NULL,
    workspace_id text NOT NULL,
    delegation_id text NOT NULL CHECK (delegation_id <> ''),
    requested_actor_id text NOT NULL CHECK (requested_actor_id <> ''),
    requested_actor_kind text NOT NULL CHECK (requested_actor_kind = 'service_account'),
    effective_actor_id text NOT NULL CHECK (effective_actor_id <> ''),
    effective_actor_kind text NOT NULL CHECK (effective_actor_kind = 'user'),
    parent_grant_ref text NOT NULL CHECK (parent_grant_ref <> ''),
    parent_connection_ids text[] NOT NULL,
    parent_operation_ids text[] NOT NULL,
    parent_approval_refs text[] NOT NULL,
    parent_expires_at timestamptz NOT NULL,
    connection_ids text[] NOT NULL,
    operation_ids text[] NOT NULL,
    policy_epoch bigint NOT NULL CHECK (policy_epoch > 0),
    control_revision bigint NOT NULL CHECK (control_revision > 0),
    expires_at timestamptz NOT NULL,
    revoked_at timestamptz,
    hop smallint NOT NULL CHECK (hop = 1),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (org_id, workspace_id, delegation_id),
    FOREIGN KEY (org_id, requested_actor_id) REFERENCES pm.identities (org_id, identity_id),
    FOREIGN KEY (org_id, effective_actor_id) REFERENCES pm.identities (org_id, identity_id),
    FOREIGN KEY (org_id, workspace_id, parent_grant_ref) REFERENCES pm.service_accounts (org_id, workspace_id, service_account_id),
    CHECK (requested_actor_id <> effective_actor_id),
    CHECK (expires_at <= parent_expires_at),
    CHECK (pm.text_array_is_strictly_sorted(parent_connection_ids)),
    CHECK (pm.text_array_is_strictly_sorted(parent_operation_ids)),
    CHECK (pm.text_array_is_strictly_sorted(parent_approval_refs)),
    CHECK (pm.text_array_is_strictly_sorted(connection_ids)),
    CHECK (pm.text_array_is_strictly_sorted(operation_ids)),
    CHECK (connection_ids <@ parent_connection_ids),
    CHECK (operation_ids <@ parent_operation_ids)
);

CREATE TABLE pm.identity_collection_revisions (
    org_id text NOT NULL,
    workspace_id text NOT NULL,
    membership_revision bigint NOT NULL DEFAULT 1 CHECK (membership_revision > 0),
    service_account_revision bigint NOT NULL DEFAULT 1 CHECK (service_account_revision > 0),
    delegation_revision bigint NOT NULL DEFAULT 1 CHECK (delegation_revision > 0),
    connection_revision bigint NOT NULL DEFAULT 1 CHECK (connection_revision > 0),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (org_id, workspace_id),
    FOREIGN KEY (org_id, workspace_id) REFERENCES pm.workspace_namespaces (org_id, workspace_id)
);

DROP POLICY pm_scope ON pm.workspace_namespaces;
CREATE POLICY pm_workspace_or_principal_scope ON pm.workspace_namespaces
USING (
    org_id = NULLIF(current_setting('pm.org_id', true), '')
    AND (
        workspace_id = NULLIF(current_setting('pm.workspace_id', true), '')
        OR (
            NULLIF(current_setting('pm.workspace_id', true), '') IS NULL
            AND NULLIF(current_setting('pm.requested_actor_id', true), '') = NULLIF(current_setting('pm.effective_actor_id', true), '')
            AND NULLIF(current_setting('pm.requested_actor_kind', true), '') = 'user'
            AND EXISTS (
                SELECT 1 FROM pm.memberships AS own_membership
                WHERE own_membership.org_id = workspace_namespaces.org_id
                  AND own_membership.workspace_id = workspace_namespaces.workspace_id
                  AND own_membership.identity_id = NULLIF(current_setting('pm.requested_actor_id', true), '')
                  AND own_membership.revoked_at IS NULL
                  AND (own_membership.expires_at IS NULL OR own_membership.expires_at > clock_timestamp())
            )
        )
    )
)
WITH CHECK (
    org_id = NULLIF(current_setting('pm.org_id', true), '')
    AND workspace_id = NULLIF(current_setting('pm.workspace_id', true), '')
);

DROP POLICY pm_scope ON pm.memberships;
CREATE POLICY pm_membership_or_principal_scope ON pm.memberships
USING (
    org_id = NULLIF(current_setting('pm.org_id', true), '')
    AND (
        workspace_id = NULLIF(current_setting('pm.workspace_id', true), '')
        OR (
            NULLIF(current_setting('pm.workspace_id', true), '') IS NULL
            AND identity_id = NULLIF(current_setting('pm.requested_actor_id', true), '')
            AND NULLIF(current_setting('pm.requested_actor_id', true), '') = NULLIF(current_setting('pm.effective_actor_id', true), '')
            AND NULLIF(current_setting('pm.requested_actor_kind', true), '') = 'user'
        )
    )
)
WITH CHECK (
    org_id = NULLIF(current_setting('pm.org_id', true), '')
    AND workspace_id = NULLIF(current_setting('pm.workspace_id', true), '')
);

DROP POLICY pm_org_scope ON pm.identities;
CREATE POLICY pm_identity_or_principal_scope ON pm.identities
USING (
    org_id = NULLIF(current_setting('pm.org_id', true), '')
    AND (
        NULLIF(current_setting('pm.workspace_id', true), '') IS NOT NULL
        OR (
            identity_id = NULLIF(current_setting('pm.requested_actor_id', true), '')
            AND NULLIF(current_setting('pm.requested_actor_id', true), '') = NULLIF(current_setting('pm.effective_actor_id', true), '')
            AND NULLIF(current_setting('pm.requested_actor_kind', true), '') = kind
        )
    )
)
WITH CHECK (
    org_id = NULLIF(current_setting('pm.org_id', true), '')
    AND NULLIF(current_setting('pm.workspace_id', true), '') IS NOT NULL
);

DO $$
DECLARE
    deployment_table text;
    scoped_table text;
BEGIN
    FOREACH deployment_table IN ARRAY ARRAY[
        'auth_bootstrap_identities', 'oidc_login_challenges',
        'device_start_windows', 'device_login_challenges'
    ]
    LOOP
        EXECUTE format('ALTER TABLE pm.%I ENABLE ROW LEVEL SECURITY', deployment_table);
        EXECUTE format('ALTER TABLE pm.%I FORCE ROW LEVEL SECURITY', deployment_table);
        EXECUTE format(
            'CREATE POLICY pm_deployment_scope ON pm.%I USING (' ||
            'deployment_id = NULLIF(current_setting(''pm.deployment_id'', true), '''')' ||
            ') WITH CHECK (' ||
            'deployment_id = NULLIF(current_setting(''pm.deployment_id'', true), '''')' ||
            ')', deployment_table
        );
        EXECUTE format('REVOKE ALL ON TABLE pm.%I FROM PUBLIC, pm_app_runtime', deployment_table);
        EXECUTE format('GRANT SELECT ON TABLE pm.%I TO pm_app_runtime', deployment_table);
    END LOOP;

    FOREACH scoped_table IN ARRAY ARRAY[
        'service_accounts', 'delegations', 'identity_collection_revisions'
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
            ')', scoped_table
        );
        EXECUTE format('REVOKE ALL ON TABLE pm.%I FROM PUBLIC, pm_app_runtime', scoped_table);
        EXECUTE format('GRANT SELECT ON TABLE pm.%I TO pm_app_runtime', scoped_table);
    END LOOP;
END
$$;
ALTER TABLE pm.pm_sessions ENABLE ROW LEVEL SECURITY;
ALTER TABLE pm.pm_sessions FORCE ROW LEVEL SECURITY;
CREATE POLICY pm_session_read ON pm.pm_sessions
    FOR SELECT
    USING (deployment_id = NULLIF(current_setting('pm.deployment_id', true), ''));
CREATE POLICY pm_session_insert ON pm.pm_sessions
    FOR INSERT
    WITH CHECK (deployment_id = NULLIF(current_setting('pm.deployment_id', true), ''));
CREATE POLICY pm_session_update ON pm.pm_sessions
    FOR UPDATE
    USING (
        deployment_id = NULLIF(current_setting('pm.deployment_id', true), '')
        AND org_id = NULLIF(current_setting('pm.org_id', true), '')
    )
    WITH CHECK (
        deployment_id = NULLIF(current_setting('pm.deployment_id', true), '')
        AND org_id = NULLIF(current_setting('pm.org_id', true), '')
    );
CREATE POLICY pm_session_delete ON pm.pm_sessions
    FOR DELETE TO pm_app_migrator
    USING (deployment_id = NULLIF(current_setting('pm.deployment_id', true), ''));
REVOKE ALL ON TABLE pm.pm_sessions FROM PUBLIC, pm_app_runtime;
GRANT SELECT ON TABLE pm.pm_sessions TO pm_app_runtime;

ALTER TABLE pm.service_account_credentials ENABLE ROW LEVEL SECURITY;
ALTER TABLE pm.service_account_credentials FORCE ROW LEVEL SECURITY;
CREATE POLICY pm_service_credential_read ON pm.service_account_credentials
    FOR SELECT
    USING (deployment_id = NULLIF(current_setting('pm.deployment_id', true), ''));
CREATE POLICY pm_service_credential_insert ON pm.service_account_credentials
    FOR INSERT
    WITH CHECK (
        deployment_id = NULLIF(current_setting('pm.deployment_id', true), '')
        AND org_id = NULLIF(current_setting('pm.org_id', true), '')
        AND workspace_id = NULLIF(current_setting('pm.workspace_id', true), '')
    );
CREATE POLICY pm_service_credential_update ON pm.service_account_credentials
    FOR UPDATE
    USING (
        deployment_id = NULLIF(current_setting('pm.deployment_id', true), '')
        AND org_id = NULLIF(current_setting('pm.org_id', true), '')
        AND workspace_id = NULLIF(current_setting('pm.workspace_id', true), '')
    )
    WITH CHECK (
        deployment_id = NULLIF(current_setting('pm.deployment_id', true), '')
        AND org_id = NULLIF(current_setting('pm.org_id', true), '')
        AND workspace_id = NULLIF(current_setting('pm.workspace_id', true), '')
    );
CREATE POLICY pm_service_credential_delete ON pm.service_account_credentials
    FOR DELETE TO pm_app_migrator
    USING (
        deployment_id = NULLIF(current_setting('pm.deployment_id', true), '')
        AND org_id = NULLIF(current_setting('pm.org_id', true), '')
        AND workspace_id = NULLIF(current_setting('pm.workspace_id', true), '')
    );
REVOKE ALL ON TABLE pm.service_account_credentials FROM PUBLIC, pm_app_runtime;
GRANT SELECT ON TABLE pm.service_account_credentials TO pm_app_runtime;


REVOKE ALL ON TABLE pm.workspace_namespaces, pm.memberships, pm.identities,
    pm.identity_mutation_keys, pm.connections FROM PUBLIC, pm_app_runtime;
GRANT SELECT ON TABLE pm.workspace_namespaces, pm.memberships, pm.identities,
    pm.identity_mutation_keys, pm.connections TO pm_app_runtime;

GRANT INSERT (deployment_id, session_id, org_id, principal_id, principal_kind, transport, token_hash, csrf_hash, authenticated_at, bootstrap_issuer, bootstrap_subject, issued_at, expires_at, revoked_at)
    ON pm.pm_sessions TO pm_app_runtime;
GRANT UPDATE (revoked_at) ON pm.pm_sessions TO pm_app_runtime;
GRANT INSERT (deployment_id, challenge_id, state_hash, nonce_hash, verifier_hash, origin, host, expires_at, reauthentication_requested)
    ON pm.oidc_login_challenges TO pm_app_runtime;
GRANT UPDATE (claim_id, claimed_at, consumed_at, rejected_at, rejection_reason)
    ON pm.oidc_login_challenges TO pm_app_runtime;
GRANT INSERT (deployment_id, dimension_kind, dimension_hash, window_started_at, starts, updated_at)
    ON pm.device_start_windows TO pm_app_runtime;
GRANT UPDATE (window_started_at, starts, updated_at) ON pm.device_start_windows TO pm_app_runtime;
GRANT INSERT (deployment_id, challenge_id, device_hash, client_hash, proof_hash, user_code_hash, state, poll_interval_seconds, next_poll_at, expires_at)
    ON pm.device_login_challenges TO pm_app_runtime;
GRANT UPDATE (state, poll_count, next_poll_at, approved_org_id, approved_principal_id, approved_principal_kind, decided_at, consumed_at)
    ON pm.device_login_challenges TO pm_app_runtime;
GRANT INSERT (org_id, workspace_id, service_account_id, display_name, permissions, connection_ids, operation_ids, policy_epoch, control_revision, expires_at, revoked_at)
    ON pm.service_accounts TO pm_app_runtime;
GRANT UPDATE (policy_epoch, control_revision, revoked_at) ON pm.service_accounts TO pm_app_runtime;
GRANT INSERT (deployment_id, org_id, workspace_id, credential_id, service_account_id, secret_hash, issued_at, expires_at, revoked_at)
    ON pm.service_account_credentials TO pm_app_runtime;
GRANT UPDATE (revoked_at) ON pm.service_account_credentials TO pm_app_runtime;
GRANT INSERT (org_id, workspace_id, delegation_id, requested_actor_id, requested_actor_kind, effective_actor_id, effective_actor_kind, parent_grant_ref, parent_connection_ids, parent_operation_ids, parent_approval_refs, parent_expires_at, connection_ids, operation_ids, policy_epoch, control_revision, expires_at, revoked_at, hop)
    ON pm.delegations TO pm_app_runtime;
GRANT UPDATE (policy_epoch, control_revision, revoked_at) ON pm.delegations TO pm_app_runtime;
GRANT INSERT (org_id, workspace_id, membership_revision, service_account_revision, delegation_revision, connection_revision)
    ON pm.identity_collection_revisions TO pm_app_runtime;
GRANT UPDATE (membership_revision, service_account_revision, delegation_revision, connection_revision, updated_at)
    ON pm.identity_collection_revisions TO pm_app_runtime;

GRANT INSERT (org_id, workspace_id, name) ON pm.workspace_namespaces TO pm_app_runtime;
GRANT UPDATE (tombstoned_at) ON pm.workspace_namespaces TO pm_app_runtime;
GRANT INSERT (org_id, identity_id, kind, issuer, subject, revision) ON pm.identities TO pm_app_runtime;
GRANT INSERT (org_id, workspace_id, membership_id, identity_id, role, resource_grants, permissions, connection_ids, operation_ids, policy_epoch, control_revision, expires_at, revoked_at)
    ON pm.memberships TO pm_app_runtime;
GRANT UPDATE (policy_epoch, control_revision, revoked_at) ON pm.memberships TO pm_app_runtime;
GRANT INSERT (org_id, workspace_id, principal_id, operation_id, target_id, idempotency_key, intent_digest, result_ref, result_revision, expected_revision, receipt_state, updated_at)
    ON pm.identity_mutation_keys TO pm_app_runtime;
GRANT UPDATE (receipt_state, result_ref, result_revision, response_status, safe_result, resolved_at, response_expires_at, compacted_at, updated_at)
    ON pm.identity_mutation_keys TO pm_app_runtime;
GRANT INSERT (org_id, workspace_id, connection_id, connector_id, provider_account_id, revision, credential_ref, display_name, state)
    ON pm.connections TO pm_app_runtime;
GRANT UPDATE (credential_ref, display_name, state, revision, revoked_at, updated_at)
    ON pm.connections TO pm_app_runtime;

ALTER TABLE pm.control_schema DROP CONSTRAINT control_schema_revision_check;
UPDATE pm.control_schema
SET revision = 'pm.control/v3', installed_at = clock_timestamp()
WHERE singleton AND revision = 'pm.control/v2';
ALTER TABLE pm.control_schema
    ADD CONSTRAINT control_schema_revision_check CHECK (revision = 'pm.control/v3');
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
REVOKE ALL ON TABLE pm.auth_bootstrap_identities, pm.pm_sessions,
    pm.oidc_login_challenges, pm.device_start_windows, pm.device_login_challenges,
    pm.service_accounts, pm.service_account_credentials, pm.delegations,
    pm.identity_collection_revisions FROM PUBLIC, pm_app_runtime;

DROP TABLE pm.identity_collection_revisions;
DROP TABLE pm.delegations;
DROP TABLE pm.service_account_credentials;
DROP TABLE pm.service_accounts;
DROP TABLE pm.device_login_challenges;
DROP TABLE pm.device_start_windows;
DROP TABLE pm.oidc_login_challenges;
DROP TABLE pm.pm_sessions;
DROP TABLE pm.auth_bootstrap_identities;

DROP POLICY pm_workspace_or_principal_scope ON pm.workspace_namespaces;
CREATE POLICY pm_scope ON pm.workspace_namespaces
USING (
    org_id = NULLIF(current_setting('pm.org_id', true), '')
    AND workspace_id = NULLIF(current_setting('pm.workspace_id', true), '')
)
WITH CHECK (
    org_id = NULLIF(current_setting('pm.org_id', true), '')
    AND workspace_id = NULLIF(current_setting('pm.workspace_id', true), '')
);

DROP POLICY pm_membership_or_principal_scope ON pm.memberships;
CREATE POLICY pm_scope ON pm.memberships
USING (
    org_id = NULLIF(current_setting('pm.org_id', true), '')
    AND workspace_id = NULLIF(current_setting('pm.workspace_id', true), '')
)
WITH CHECK (
    org_id = NULLIF(current_setting('pm.org_id', true), '')
    AND workspace_id = NULLIF(current_setting('pm.workspace_id', true), '')
);

DROP POLICY pm_identity_or_principal_scope ON pm.identities;
CREATE POLICY pm_org_scope ON pm.identities
USING (
    org_id = NULLIF(current_setting('pm.org_id', true), '')
    AND NULLIF(current_setting('pm.workspace_id', true), '') IS NOT NULL
)
WITH CHECK (
    org_id = NULLIF(current_setting('pm.org_id', true), '')
    AND NULLIF(current_setting('pm.workspace_id', true), '') IS NOT NULL
);

REVOKE ALL ON TABLE pm.workspace_namespaces, pm.memberships, pm.identities,
    pm.identity_mutation_keys, pm.connections FROM PUBLIC, pm_app_runtime;
GRANT SELECT ON TABLE pm.workspace_namespaces, pm.memberships, pm.identities,
    pm.identity_mutation_keys, pm.connections TO pm_app_runtime;
GRANT INSERT (org_id, workspace_id, name) ON pm.workspace_namespaces TO pm_app_runtime;
GRANT UPDATE (tombstoned_at) ON pm.workspace_namespaces TO pm_app_runtime;
GRANT INSERT (org_id, identity_id, kind, issuer, subject, revision) ON pm.identities TO pm_app_runtime;
GRANT INSERT (org_id, workspace_id, membership_id, identity_id, role, policy_epoch, expires_at, revoked_at)
    ON pm.memberships TO pm_app_runtime;
GRANT INSERT (org_id, workspace_id, principal_id, operation_id, target_id, idempotency_key, intent_digest, result_ref, result_revision)
    ON pm.identity_mutation_keys TO pm_app_runtime;
GRANT INSERT (org_id, workspace_id, connection_id, connector_id, provider_account_id, revision, credential_ref)
    ON pm.connections TO pm_app_runtime;

CREATE POLICY pm_identity_mutation_migration_read ON pm.identity_mutation_keys
    FOR SELECT TO pm_app_migrator
    USING (true);
DO $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM pm.identity_mutation_keys
        WHERE result_ref IS NULL OR result_revision IS NULL
    ) THEN
        RAISE EXCEPTION 'cannot roll back identity receipts while unresolved mutations exist';
    END IF;
END
$$;
DROP POLICY pm_identity_mutation_migration_read ON pm.identity_mutation_keys;
ALTER TABLE pm.identity_mutation_keys
    DROP CONSTRAINT identity_mutation_receipt_shape,
    DROP CONSTRAINT identity_mutation_response_status_valid,
    DROP CONSTRAINT identity_mutation_result_revision_valid,
    DROP CONSTRAINT identity_mutation_result_ref_valid,
    DROP COLUMN updated_at,
    DROP COLUMN compacted_at,
    DROP COLUMN response_expires_at,
    DROP COLUMN resolved_at,
    DROP COLUMN safe_result,
    DROP COLUMN response_status,
    DROP COLUMN receipt_state,
    DROP COLUMN expected_revision,
    ALTER COLUMN result_revision SET NOT NULL,
    ALTER COLUMN result_ref SET NOT NULL;

ALTER TABLE pm.connections
    DROP CONSTRAINT connection_revocation_state_agrees,
    DROP COLUMN updated_at,
    DROP COLUMN revoked_at,
    DROP COLUMN state,
    DROP COLUMN display_name;

ALTER TABLE pm.memberships
    DROP CONSTRAINT membership_operations_sorted,
    DROP CONSTRAINT membership_connections_sorted,
    DROP CONSTRAINT membership_permissions_sorted,
    DROP CONSTRAINT membership_resource_grants_sorted,
    DROP COLUMN control_revision,
    DROP COLUMN operation_ids,
    DROP COLUMN connection_ids,
    DROP COLUMN permissions,
    DROP COLUMN resource_grants;
DROP FUNCTION pm.text_array_is_strictly_sorted(text[]);

ALTER TABLE pm.control_schema DROP CONSTRAINT control_schema_revision_check;
UPDATE pm.control_schema
SET revision = 'pm.control/v2', installed_at = clock_timestamp()
WHERE singleton AND revision = 'pm.control/v3';
ALTER TABLE pm.control_schema
    ADD CONSTRAINT control_schema_revision_check CHECK (revision = 'pm.control/v2');
-- +goose StatementEnd
