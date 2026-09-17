-- Encrypted credential lifecycle and account-wide refresh fencing.
-- Goose owns the transaction; each section is one SQL batch.

-- +goose Up
-- +goose StatementBegin
DO $$
BEGIN
    IF current_database() <> 'pm_app' OR current_user <> 'pm_app_migrator' THEN
        RAISE EXCEPTION 'credential lifecycle schema requires pm_app_migrator on pm_app';
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
    IF NOT EXISTS (
        SELECT 1 FROM pm.control_schema
        WHERE singleton AND revision = 'pm.control/v3'
    ) THEN
        RAISE EXCEPTION 'credential migration requires the preceding identity schema';
    END IF;
END
$$;

-- Historical encrypted versions must remain valid evidence after a current
-- connection or account witness changes. Replace the two current-row foreign
-- keys with identity-only historical references. Application code still checks
-- the current connection/account/witness before every resolution or refresh.
DO $$
DECLARE
    constraint_name name;
BEGIN
    SELECT candidate.conname
    INTO constraint_name
    FROM pg_constraint AS candidate
    WHERE candidate.conrelid = 'pm.encrypted_secrets'::regclass
      AND candidate.confrelid = 'pm.connections'::regclass
      AND candidate.contype = 'f'
      AND (
          SELECT array_agg(attribute.attname::text ORDER BY key_column.ordinality)
          FROM unnest(candidate.conkey) WITH ORDINALITY AS key_column(attnum, ordinality)
          JOIN pg_attribute AS attribute
            ON attribute.attrelid = candidate.conrelid
           AND attribute.attnum = key_column.attnum
      ) = ARRAY[
          'org_id', 'workspace_id', 'connection_id',
          'provider_account_id', 'credential_ref'
      ];
    IF constraint_name IS NULL THEN
        RAISE EXCEPTION 'encrypted_secrets current-connection foreign key is missing';
    END IF;
    EXECUTE format('ALTER TABLE pm.encrypted_secrets DROP CONSTRAINT %I', constraint_name);

    SELECT candidate.conname
    INTO constraint_name
    FROM pg_constraint AS candidate
    WHERE candidate.conrelid = 'pm.encrypted_secrets'::regclass
      AND candidate.confrelid = 'pm.provider_accounts'::regclass
      AND candidate.contype = 'f'
      AND (
          SELECT array_agg(attribute.attname::text ORDER BY key_column.ordinality)
          FROM unnest(candidate.conkey) WITH ORDINALITY AS key_column(attnum, ordinality)
          JOIN pg_attribute AS attribute
            ON attribute.attrelid = candidate.conrelid
           AND attribute.attnum = key_column.attnum
      ) = ARRAY['org_id', 'provider_account_id', 'account_witness'];
    IF constraint_name IS NULL THEN
        RAISE EXCEPTION 'encrypted_secrets current-account-witness foreign key is missing';
    END IF;
    EXECUTE format('ALTER TABLE pm.encrypted_secrets DROP CONSTRAINT %I', constraint_name);
END
$$;

ALTER TABLE pm.encrypted_secrets
    ADD CONSTRAINT encrypted_secrets_connection_history_fk
        FOREIGN KEY (org_id, workspace_id, connection_id)
        REFERENCES pm.connections (org_id, workspace_id, connection_id),
    ADD CONSTRAINT encrypted_secrets_account_history_fk
        FOREIGN KEY (org_id, provider_account_id)
        REFERENCES pm.provider_accounts (org_id, provider_account_id),
    ADD CONSTRAINT encrypted_secrets_lifecycle_binding_unique UNIQUE (
        org_id, workspace_id, credential_ref, ciphertext_version,
        connection_id, provider_account_id, account_witness, auth_epoch
    );

CREATE TABLE pm.credential_lifecycles (
    org_id text NOT NULL,
    workspace_id text NOT NULL,
    connection_id text NOT NULL CHECK (connection_id <> ''),
    credential_ref text NOT NULL CHECK (credential_ref <> ''),
    provider_account_id text NOT NULL CHECK (provider_account_id <> ''),
    account_witness text NOT NULL CHECK (account_witness <> ''),
    connection_revision bigint NOT NULL CHECK (connection_revision > 0),
    account_revision bigint NOT NULL CHECK (account_revision > 0),
    current_ciphertext_version bigint NOT NULL CHECK (current_ciphertext_version > 0),
    status text NOT NULL CHECK (
        status IN ('active', 'refreshing', 'reauthorization_required')
    ),
    auth_epoch bigint NOT NULL CHECK (auth_epoch > 0),
    revision bigint NOT NULL CHECK (revision > 0),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (org_id, workspace_id, connection_id),
    UNIQUE (org_id, workspace_id, credential_ref),
    UNIQUE (
        org_id, workspace_id, connection_id, credential_ref,
        current_ciphertext_version, revision
    ),
    FOREIGN KEY (org_id, workspace_id)
        REFERENCES pm.workspace_namespaces (org_id, workspace_id),
    FOREIGN KEY (org_id, workspace_id, connection_id)
        REFERENCES pm.connections (org_id, workspace_id, connection_id),
    FOREIGN KEY (org_id, provider_account_id)
        REFERENCES pm.provider_accounts (org_id, provider_account_id),
    FOREIGN KEY (
        org_id, workspace_id, credential_ref, current_ciphertext_version,
        connection_id, provider_account_id, account_witness, auth_epoch
    ) REFERENCES pm.encrypted_secrets (
        org_id, workspace_id, credential_ref, ciphertext_version,
        connection_id, provider_account_id, account_witness, auth_epoch
    )
);

CREATE TABLE pm.credential_mutation_receipts (
    org_id text NOT NULL,
    workspace_id text NOT NULL,
    principal_id text NOT NULL CHECK (principal_id <> ''),
    principal_kind text NOT NULL CHECK (principal_kind IN ('user', 'service_account')),
    connection_id text NOT NULL CHECK (connection_id <> ''),
    idempotency_key text NOT NULL CHECK (idempotency_key <> ''),
    intent_fingerprint text NOT NULL CHECK (
        intent_fingerprint ~ '^hmac-sha256:[0-9a-f]{64}$'
    ),
    credential_ref text NOT NULL CHECK (credential_ref <> ''),
    result_ciphertext_version bigint NOT NULL CHECK (result_ciphertext_version > 0),
    result_status text NOT NULL CHECK (result_status = 'active'),
    result_lifecycle_revision bigint NOT NULL CHECK (result_lifecycle_revision > 0),
    recorded_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (
        org_id, workspace_id, principal_id, principal_kind,
        connection_id, idempotency_key
    ),
    FOREIGN KEY (org_id, workspace_id)
        REFERENCES pm.workspace_namespaces (org_id, workspace_id),
    FOREIGN KEY (org_id, principal_id)
        REFERENCES pm.identities (org_id, identity_id),
    FOREIGN KEY (org_id, workspace_id, connection_id)
        REFERENCES pm.connections (org_id, workspace_id, connection_id),
    FOREIGN KEY (
        org_id, workspace_id, credential_ref, result_ciphertext_version
    ) REFERENCES pm.encrypted_secrets (
        org_id, workspace_id, credential_ref, ciphertext_version
    )
);

-- One row per organization/provider account coordinates every workspace alias.
-- workspace_id and connection_id identify the current owner; RLS admits only a
-- scoped workspace that currently has a connection alias to the same account,
-- while application authorization must still admit the exact effect.
CREATE TABLE pm.credential_refresh_leases (
    org_id text NOT NULL,
    workspace_id text NOT NULL,
    provider_account_id text NOT NULL CHECK (provider_account_id <> ''),
    lease_id text NOT NULL CHECK (lease_id <> ''),
    connection_id text NOT NULL CHECK (connection_id <> ''),
    credential_ref text NOT NULL CHECK (credential_ref <> ''),
    account_witness text NOT NULL CHECK (account_witness <> ''),
    ciphertext_version bigint NOT NULL CHECK (ciphertext_version > 0),
    owner_id text NOT NULL CHECK (owner_id <> ''),
    fence bigint NOT NULL CHECK (fence > 0),
    state text NOT NULL CHECK (
        state IN ('claimed', 'dispatched', 'completed', 'failed', 'uncertain')
    ),
    claimed_at timestamptz NOT NULL,
    expires_at timestamptz NOT NULL CHECK (expires_at > claimed_at),
    resolved_at timestamptz,
    result_ciphertext_version bigint CHECK (result_ciphertext_version > 0),
    CHECK (resolved_at IS NULL OR resolved_at >= claimed_at),
    PRIMARY KEY (org_id, provider_account_id),
    UNIQUE (org_id, lease_id),
    FOREIGN KEY (org_id, workspace_id)
        REFERENCES pm.workspace_namespaces (org_id, workspace_id),
    FOREIGN KEY (org_id, workspace_id, connection_id)
        REFERENCES pm.connections (org_id, workspace_id, connection_id),
    FOREIGN KEY (org_id, provider_account_id)
        REFERENCES pm.provider_accounts (org_id, provider_account_id),
    FOREIGN KEY (
        org_id, workspace_id, credential_ref, ciphertext_version
    ) REFERENCES pm.encrypted_secrets (
        org_id, workspace_id, credential_ref, ciphertext_version
    ),
    FOREIGN KEY (
        org_id, workspace_id, credential_ref, result_ciphertext_version
    ) REFERENCES pm.encrypted_secrets (
        org_id, workspace_id, credential_ref, ciphertext_version
    ),
    CHECK (
        (state IN ('claimed', 'dispatched')
            AND resolved_at IS NULL
            AND result_ciphertext_version IS NULL)
        OR
        (state = 'completed'
            AND resolved_at IS NOT NULL
            AND result_ciphertext_version IS NOT NULL
            AND result_ciphertext_version = ciphertext_version + 1)
        OR
        (state IN ('failed', 'uncertain')
            AND resolved_at IS NOT NULL
            AND result_ciphertext_version IS NULL)
    )
);

CREATE INDEX credential_refresh_leases_owner_state_idx
    ON pm.credential_refresh_leases (
        org_id, workspace_id, connection_id, state, expires_at
    );

ALTER TABLE pm.credential_lifecycles ENABLE ROW LEVEL SECURITY;
ALTER TABLE pm.credential_lifecycles FORCE ROW LEVEL SECURITY;
CREATE POLICY pm_scope ON pm.credential_lifecycles
    USING (
        org_id = NULLIF(current_setting('pm.org_id', true), '')
        AND workspace_id = NULLIF(current_setting('pm.workspace_id', true), '')
    )
    WITH CHECK (
        org_id = NULLIF(current_setting('pm.org_id', true), '')
        AND workspace_id = NULLIF(current_setting('pm.workspace_id', true), '')
    );

ALTER TABLE pm.credential_mutation_receipts ENABLE ROW LEVEL SECURITY;
ALTER TABLE pm.credential_mutation_receipts FORCE ROW LEVEL SECURITY;
CREATE POLICY pm_scope ON pm.credential_mutation_receipts
    USING (
        org_id = NULLIF(current_setting('pm.org_id', true), '')
        AND workspace_id = NULLIF(current_setting('pm.workspace_id', true), '')
    )
    WITH CHECK (
        org_id = NULLIF(current_setting('pm.org_id', true), '')
        AND workspace_id = NULLIF(current_setting('pm.workspace_id', true), '')
    );

ALTER TABLE pm.credential_refresh_leases ENABLE ROW LEVEL SECURITY;
ALTER TABLE pm.credential_refresh_leases FORCE ROW LEVEL SECURITY;

-- Active aliases may observe the account-wide lease. Its exact owner may
-- continue to observe it after revocation so a dispatched effect can be
-- narrowed to uncertainty and fixture cleanup can remain scoped.
CREATE POLICY pm_account_select ON pm.credential_refresh_leases
    FOR SELECT
    USING (
        org_id = NULLIF(current_setting('pm.org_id', true), '')
        AND EXISTS (
            SELECT 1
            FROM pm.connections AS scoped_connection
            WHERE scoped_connection.org_id = credential_refresh_leases.org_id
              AND scoped_connection.workspace_id =
                  NULLIF(current_setting('pm.workspace_id', true), '')
              AND scoped_connection.provider_account_id =
                  credential_refresh_leases.provider_account_id
              AND (
                  scoped_connection.state = 'active'
                  OR (
                      credential_refresh_leases.workspace_id =
                          NULLIF(current_setting('pm.workspace_id', true), '')
                      AND scoped_connection.connection_id =
                          credential_refresh_leases.connection_id
                  )
              )
        )
    );

CREATE POLICY pm_active_owner_insert ON pm.credential_refresh_leases
    FOR INSERT
    WITH CHECK (
        org_id = NULLIF(current_setting('pm.org_id', true), '')
        AND workspace_id = NULLIF(current_setting('pm.workspace_id', true), '')
        AND EXISTS (
            SELECT 1
            FROM pm.connections AS scoped_connection
            WHERE scoped_connection.org_id = credential_refresh_leases.org_id
              AND scoped_connection.workspace_id =
                  credential_refresh_leases.workspace_id
              AND scoped_connection.connection_id =
                  credential_refresh_leases.connection_id
              AND scoped_connection.provider_account_id =
                  credential_refresh_leases.provider_account_id
              AND scoped_connection.state = 'active'
        )
    );

CREATE POLICY pm_account_update ON pm.credential_refresh_leases
    FOR UPDATE
    USING (
        org_id = NULLIF(current_setting('pm.org_id', true), '')
        AND EXISTS (
            SELECT 1
            FROM pm.connections AS scoped_connection
            WHERE scoped_connection.org_id = credential_refresh_leases.org_id
              AND scoped_connection.workspace_id =
                  NULLIF(current_setting('pm.workspace_id', true), '')
              AND scoped_connection.provider_account_id =
                  credential_refresh_leases.provider_account_id
              AND (
                  scoped_connection.state = 'active'
                  OR (
                      credential_refresh_leases.workspace_id =
                          NULLIF(current_setting('pm.workspace_id', true), '')
                      AND scoped_connection.connection_id =
                          credential_refresh_leases.connection_id
                  )
              )
        )
    )
    WITH CHECK (
        org_id = NULLIF(current_setting('pm.org_id', true), '')
        AND workspace_id = NULLIF(current_setting('pm.workspace_id', true), '')
        AND EXISTS (
            SELECT 1
            FROM pm.connections AS scoped_connection
            WHERE scoped_connection.org_id = credential_refresh_leases.org_id
              AND scoped_connection.workspace_id =
                  credential_refresh_leases.workspace_id
              AND scoped_connection.connection_id =
                  credential_refresh_leases.connection_id
              AND scoped_connection.provider_account_id =
                  credential_refresh_leases.provider_account_id
              AND (
                  scoped_connection.state = 'active'
                  OR credential_refresh_leases.state = 'uncertain'
              )
        )
    );

CREATE POLICY pm_account_delete ON pm.credential_refresh_leases
    FOR DELETE
    USING (
        org_id = NULLIF(current_setting('pm.org_id', true), '')
        AND EXISTS (
            SELECT 1
            FROM pm.connections AS scoped_connection
            WHERE scoped_connection.org_id = credential_refresh_leases.org_id
              AND scoped_connection.workspace_id =
                  NULLIF(current_setting('pm.workspace_id', true), '')
              AND scoped_connection.provider_account_id =
                  credential_refresh_leases.provider_account_id
              AND (
                  scoped_connection.state = 'active'
                  OR (
                      credential_refresh_leases.workspace_id =
                          NULLIF(current_setting('pm.workspace_id', true), '')
                      AND scoped_connection.connection_id =
                          credential_refresh_leases.connection_id
                  )
              )
        )
    );

REVOKE ALL ON TABLE pm.credential_lifecycles
    FROM PUBLIC, pm_app_runtime;
REVOKE ALL ON TABLE pm.credential_mutation_receipts
    FROM PUBLIC, pm_app_runtime;
REVOKE ALL ON TABLE pm.credential_refresh_leases
    FROM PUBLIC, pm_app_runtime;

GRANT SELECT ON TABLE pm.credential_lifecycles TO pm_app_runtime;
GRANT INSERT (
    org_id, workspace_id, connection_id, credential_ref,
    provider_account_id, account_witness,
    connection_revision, account_revision, current_ciphertext_version,
    status, auth_epoch, revision, updated_at
) ON pm.credential_lifecycles TO pm_app_runtime;
GRANT UPDATE (
    credential_ref, provider_account_id, account_witness,
    connection_revision, account_revision, current_ciphertext_version,
    status, auth_epoch, revision, updated_at
) ON pm.credential_lifecycles TO pm_app_runtime;

GRANT SELECT ON TABLE pm.credential_mutation_receipts TO pm_app_runtime;
GRANT INSERT (
    org_id, workspace_id, principal_id, principal_kind,
    connection_id, idempotency_key, intent_fingerprint,
    credential_ref, result_ciphertext_version, result_status,
    result_lifecycle_revision, recorded_at
) ON pm.credential_mutation_receipts TO pm_app_runtime;

GRANT SELECT ON TABLE pm.credential_refresh_leases TO pm_app_runtime;
GRANT INSERT (
    org_id, workspace_id, provider_account_id, lease_id,
    connection_id, credential_ref, account_witness, ciphertext_version,
    owner_id, fence, state, claimed_at, expires_at,
    resolved_at, result_ciphertext_version
) ON pm.credential_refresh_leases TO pm_app_runtime;
GRANT UPDATE (
    workspace_id, lease_id, connection_id, credential_ref,
    account_witness, ciphertext_version, owner_id, fence, state,
    claimed_at, expires_at, resolved_at, result_ciphertext_version
) ON pm.credential_refresh_leases TO pm_app_runtime;

ALTER TABLE pm.control_schema DROP CONSTRAINT control_schema_revision_check;
UPDATE pm.control_schema
SET revision = 'pm.control/v4', installed_at = clock_timestamp()
WHERE singleton AND revision = 'pm.control/v3';
ALTER TABLE pm.control_schema
    ADD CONSTRAINT control_schema_revision_check CHECK (revision = 'pm.control/v4');
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- Down is intentionally explicit and may refuse if current connection/account
-- mutations have made historical ciphertext incompatible with the old current-
-- row foreign keys. PM's release policy does not treat Down as rollback.

REVOKE ALL ON TABLE pm.credential_refresh_leases
    FROM PUBLIC, pm_app_runtime;
REVOKE ALL ON TABLE pm.credential_mutation_receipts
    FROM PUBLIC, pm_app_runtime;
REVOKE ALL ON TABLE pm.credential_lifecycles
    FROM PUBLIC, pm_app_runtime;

DROP TABLE pm.credential_refresh_leases;
DROP TABLE pm.credential_mutation_receipts;
DROP TABLE pm.credential_lifecycles;

ALTER TABLE pm.encrypted_secrets
    DROP CONSTRAINT encrypted_secrets_lifecycle_binding_unique,
    DROP CONSTRAINT encrypted_secrets_account_history_fk,
    DROP CONSTRAINT encrypted_secrets_connection_history_fk,
    ADD CONSTRAINT encrypted_secrets_connection_current_fk
        FOREIGN KEY (
            org_id, workspace_id, connection_id,
            provider_account_id, credential_ref
        ) REFERENCES pm.connections (
            org_id, workspace_id, connection_id,
            provider_account_id, credential_ref
        ),
    ADD CONSTRAINT encrypted_secrets_account_witness_current_fk
        FOREIGN KEY (org_id, provider_account_id, account_witness)
        REFERENCES pm.provider_accounts (
            org_id, provider_account_id, account_witness
        );

ALTER TABLE pm.control_schema DROP CONSTRAINT control_schema_revision_check;
UPDATE pm.control_schema
SET revision = 'pm.control/v3', installed_at = clock_timestamp()
WHERE singleton AND revision = 'pm.control/v4';
ALTER TABLE pm.control_schema
    ADD CONSTRAINT control_schema_revision_check CHECK (revision = 'pm.control/v3');
-- +goose StatementEnd
