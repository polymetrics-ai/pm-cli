-- Operator bootstrap of the first workspace and admin (ADR 0006).
-- The privileged deployment bootstrap creates the role pm_app_bootstrap. This
-- migration checks the role, adds the once-only bootstrap record and the
-- workspace audit record, and gives the role INSERT only on the bootstrap rows.
-- The running pm-server never connects as pm_app_bootstrap.

-- +goose Up
-- +goose StatementBegin
DO $$
BEGIN
    IF current_database() <> 'pm_app' OR current_user <> 'pm_app_migrator' THEN
        RAISE EXCEPTION 'operator bootstrap schema requires pm_app_migrator on pm_app';
    END IF;
    IF to_regrole('pm_app_runtime') IS NULL THEN
        RAISE EXCEPTION 'required runtime role is missing';
    END IF;
    IF EXISTS (
        SELECT 1 FROM pg_roles WHERE rolname = 'pm_app_runtime'
          AND (rolsuper OR rolbypassrls OR rolcreatedb OR rolcreaterole OR rolreplication)
    ) OR pg_has_role('pm_app_runtime', 'pm_app_migrator', 'MEMBER') THEN
        RAISE EXCEPTION 'runtime role has prohibited PostgreSQL authority';
    END IF;
    IF to_regrole('pm_app_bootstrap') IS NULL THEN
        RAISE EXCEPTION 'required bootstrap role is missing';
    END IF;
    IF EXISTS (
        SELECT 1 FROM pg_roles WHERE rolname = 'pm_app_bootstrap'
          AND (rolsuper OR rolbypassrls OR rolcreatedb OR rolcreaterole OR rolreplication)
    ) OR pg_has_role('pm_app_bootstrap', 'pm_app_migrator', 'MEMBER')
      OR pg_has_role('pm_app_bootstrap', 'pm_app_runtime', 'MEMBER')
      OR pg_has_role('pm_app_migrator', 'pm_app_bootstrap', 'MEMBER')
      OR pg_has_role('pm_app_runtime', 'pm_app_bootstrap', 'MEMBER') THEN
        RAISE EXCEPTION 'bootstrap role has prohibited PostgreSQL authority';
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM pm.control_schema WHERE singleton AND revision = 'pm.control/v20'
    ) THEN
        RAISE EXCEPTION 'operator bootstrap migration requires the account permit expiry schema';
    END IF;
END
$$;

-- One bootstrap for each deployment and organization. The primary key refuses
-- a second bootstrap of the same scope, also a concurrent one.
CREATE TABLE pm.bootstrap_records (
    deployment_id text NOT NULL CHECK (deployment_id <> ''),
    org_id text NOT NULL CHECK (org_id <> ''),
    workspace_id text NOT NULL CHECK (workspace_id <> ''),
    admin_identity_id text NOT NULL CHECK (admin_identity_id <> ''),
    membership_id text NOT NULL CHECK (membership_id <> ''),
    bootstrapped_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    CONSTRAINT bootstrap_records_pkey PRIMARY KEY (deployment_id, org_id),
    -- The bootstrap record is written first, so its references wait for the
    -- end of the transaction.
    FOREIGN KEY (org_id, workspace_id)
        REFERENCES pm.workspace_namespaces (org_id, workspace_id) DEFERRABLE INITIALLY DEFERRED,
    FOREIGN KEY (org_id, admin_identity_id)
        REFERENCES pm.identities (org_id, identity_id) DEFERRABLE INITIALLY DEFERRED,
    FOREIGN KEY (org_id, workspace_id, membership_id)
        REFERENCES pm.memberships (org_id, workspace_id, membership_id) DEFERRABLE INITIALLY DEFERRED
);

-- Audit records of a workspace that do not belong to a run. pm.audit_entries
-- keeps the audit records of runs.
CREATE TABLE pm.workspace_audit_entries (
    org_id text NOT NULL CHECK (org_id <> ''),
    workspace_id text NOT NULL CHECK (workspace_id <> ''),
    audit_id text NOT NULL CHECK (audit_id <> ''),
    requested_actor_id text NOT NULL CHECK (requested_actor_id <> ''),
    requested_actor_kind text NOT NULL CHECK (requested_actor_kind IN ('user', 'service_account', 'operator')),
    effective_actor_id text NOT NULL CHECK (effective_actor_id <> ''),
    effective_actor_kind text NOT NULL CHECK (effective_actor_kind IN ('user', 'service_account', 'operator')),
    action text NOT NULL CHECK (action <> ''),
    target_ref text NOT NULL CHECK (target_ref <> ''),
    payload jsonb NOT NULL CHECK (jsonb_typeof(payload) = 'object'),
    recorded_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (org_id, workspace_id, audit_id),
    FOREIGN KEY (org_id, workspace_id)
        REFERENCES pm.workspace_namespaces (org_id, workspace_id)
);

ALTER TABLE pm.provider_accounts
    ADD COLUMN display_name text,
    ADD CONSTRAINT provider_account_display_name_valid CHECK (display_name IS NULL OR display_name <> '');

ALTER TABLE pm.bootstrap_records ENABLE ROW LEVEL SECURITY;
ALTER TABLE pm.bootstrap_records FORCE ROW LEVEL SECURITY;
ALTER TABLE pm.workspace_audit_entries ENABLE ROW LEVEL SECURITY;
ALTER TABLE pm.workspace_audit_entries FORCE ROW LEVEL SECURITY;
REVOKE ALL ON TABLE pm.bootstrap_records, pm.workspace_audit_entries FROM PUBLIC, pm_app_runtime, pm_app_bootstrap;

CREATE POLICY pm_bootstrap_scope ON pm.bootstrap_records
    TO pm_app_bootstrap
    USING (
        deployment_id = NULLIF(current_setting('pm.deployment_id', true), '')
        AND org_id = NULLIF(current_setting('pm.org_id', true), '')
    )
    WITH CHECK (
        deployment_id = NULLIF(current_setting('pm.deployment_id', true), '')
        AND org_id = NULLIF(current_setting('pm.org_id', true), '')
    );
CREATE POLICY pm_bootstrap_scope ON pm.workspace_audit_entries
    FOR INSERT TO pm_app_bootstrap
    WITH CHECK (
        org_id = NULLIF(current_setting('pm.org_id', true), '')
        AND workspace_id = NULLIF(current_setting('pm.workspace_id', true), '')
    );

-- The existing permissive policies already limit these inserts to the scope
-- settings. These restrictive policies add the deployment and the organization
-- of the bootstrap for the bootstrap role only. They never widen a policy.
CREATE POLICY pm_bootstrap_insert_scope ON pm.workspace_namespaces
    AS RESTRICTIVE FOR INSERT TO pm_app_bootstrap
    WITH CHECK (
        org_id = NULLIF(current_setting('pm.org_id', true), '')
        AND workspace_id = NULLIF(current_setting('pm.workspace_id', true), '')
    );
CREATE POLICY pm_bootstrap_insert_scope ON pm.identities
    AS RESTRICTIVE FOR INSERT TO pm_app_bootstrap
    WITH CHECK (org_id = NULLIF(current_setting('pm.org_id', true), ''));
CREATE POLICY pm_bootstrap_insert_scope ON pm.auth_bootstrap_identities
    AS RESTRICTIVE FOR INSERT TO pm_app_bootstrap
    WITH CHECK (
        deployment_id = NULLIF(current_setting('pm.deployment_id', true), '')
        AND org_id = NULLIF(current_setting('pm.org_id', true), '')
    );
CREATE POLICY pm_bootstrap_insert_scope ON pm.memberships
    AS RESTRICTIVE FOR INSERT TO pm_app_bootstrap
    WITH CHECK (
        org_id = NULLIF(current_setting('pm.org_id', true), '')
        AND workspace_id = NULLIF(current_setting('pm.workspace_id', true), '')
    );
CREATE POLICY pm_bootstrap_insert_scope ON pm.provider_accounts
    AS RESTRICTIVE FOR INSERT TO pm_app_bootstrap
    WITH CHECK (org_id = NULLIF(current_setting('pm.org_id', true), ''));

-- INSERT only on the bootstrap rows, and SELECT only on the columns of the two
-- scope checks. No UPDATE, DELETE or TRUNCATE, and no other table.
GRANT USAGE ON SCHEMA pm TO pm_app_bootstrap;
GRANT EXECUTE ON FUNCTION pm.text_array_is_strictly_sorted(text[]) TO pm_app_bootstrap;
GRANT SELECT (deployment_id, org_id) ON pm.bootstrap_records TO pm_app_bootstrap;
GRANT SELECT (deployment_id, issuer, subject) ON pm.auth_bootstrap_identities TO pm_app_bootstrap;
GRANT INSERT (deployment_id, org_id, workspace_id, admin_identity_id, membership_id)
    ON pm.bootstrap_records TO pm_app_bootstrap;
GRANT INSERT (org_id, workspace_id, name) ON pm.workspace_namespaces TO pm_app_bootstrap;
GRANT INSERT (org_id, identity_id, kind, issuer, subject, revision) ON pm.identities TO pm_app_bootstrap;
GRANT INSERT (deployment_id, issuer, subject, org_id, principal_id, principal_kind, can_approve_devices, enabled)
    ON pm.auth_bootstrap_identities TO pm_app_bootstrap;
GRANT INSERT (org_id, workspace_id, membership_id, identity_id, role, resource_grants, permissions,
    connection_ids, operation_ids, policy_epoch, control_revision)
    ON pm.memberships TO pm_app_bootstrap;
GRANT INSERT (org_id, provider_account_id, provider, account_witness, capacity, revision, display_name)
    ON pm.provider_accounts TO pm_app_bootstrap;
GRANT INSERT (org_id, workspace_id, audit_id, requested_actor_id, requested_actor_kind,
    effective_actor_id, effective_actor_kind, action, target_ref, payload)
    ON pm.workspace_audit_entries TO pm_app_bootstrap;

ALTER TABLE pm.control_schema DROP CONSTRAINT control_schema_revision_check;
UPDATE pm.control_schema SET revision = 'pm.control/v21', installed_at = clock_timestamp()
WHERE singleton AND revision = 'pm.control/v20';
ALTER TABLE pm.control_schema
    ADD CONSTRAINT control_schema_revision_check CHECK (revision = 'pm.control/v21');
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- FORCE RLS applies to the owning migrator, so the refusal check needs an
-- explicit read over the tables whose rows prove a retained bootstrap.
CREATE POLICY pm_bootstrap_migration_select ON pm.bootstrap_records
    FOR SELECT TO pm_app_migrator USING (true);
CREATE POLICY pm_bootstrap_migration_select ON pm.workspace_audit_entries
    FOR SELECT TO pm_app_migrator USING (true);
CREATE POLICY pm_bootstrap_migration_select ON pm.provider_accounts
    FOR SELECT TO pm_app_migrator USING (true);
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pm.bootstrap_records)
       OR EXISTS (SELECT 1 FROM pm.workspace_audit_entries)
       OR EXISTS (SELECT 1 FROM pm.provider_accounts WHERE display_name IS NOT NULL) THEN
        RAISE EXCEPTION 'cannot roll back retained operator bootstrap records';
    END IF;
END
$$;
DROP POLICY pm_bootstrap_migration_select ON pm.provider_accounts;

DROP POLICY pm_bootstrap_insert_scope ON pm.provider_accounts;
DROP POLICY pm_bootstrap_insert_scope ON pm.memberships;
DROP POLICY pm_bootstrap_insert_scope ON pm.auth_bootstrap_identities;
DROP POLICY pm_bootstrap_insert_scope ON pm.identities;
DROP POLICY pm_bootstrap_insert_scope ON pm.workspace_namespaces;

REVOKE ALL ON pm.provider_accounts, pm.memberships, pm.auth_bootstrap_identities,
    pm.identities, pm.workspace_namespaces FROM pm_app_bootstrap;
REVOKE EXECUTE ON FUNCTION pm.text_array_is_strictly_sorted(text[]) FROM pm_app_bootstrap;
REVOKE USAGE ON SCHEMA pm FROM pm_app_bootstrap;

DROP TABLE pm.workspace_audit_entries;
DROP TABLE pm.bootstrap_records;

ALTER TABLE pm.provider_accounts
    DROP CONSTRAINT provider_account_display_name_valid,
    DROP COLUMN display_name;

ALTER TABLE pm.control_schema DROP CONSTRAINT control_schema_revision_check;
UPDATE pm.control_schema SET revision = 'pm.control/v20', installed_at = clock_timestamp()
WHERE singleton AND revision = 'pm.control/v21';
ALTER TABLE pm.control_schema
    ADD CONSTRAINT control_schema_revision_check CHECK (revision = 'pm.control/v20');
-- +goose StatementEnd
