-- Operator bootstrap of the first workspace and admin (ADR 0006).
-- The privileged deployment bootstrap creates the role pm_app_bootstrap. This
-- migration checks the role, adds the once-only bootstrap record and the
-- workspace audit record, and gives the role one write path: EXECUTE on the
-- definer function pm.operator_bootstrap. The role has no table grant.
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
    -- The SHA-256 of the canonical operator input. pm.operator_bootstrap
    -- computes it from the values that it writes. A retry compares it.
    input_digest text NOT NULL CHECK (input_digest ~ '^sha256:[0-9a-f]{64}$'),
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

REVOKE ALL ON TABLE pm.workspace_namespaces, pm.identities, pm.auth_bootstrap_identities,
    pm.memberships, pm.provider_accounts FROM pm_app_bootstrap;

-- FORCE RLS also applies to the owning migrator. These policies serve only the
-- definer function below. The function sets the scope settings from its own
-- checked input. The older permissive policies (pm_scope, pm_org_scope,
-- pm_deployment_scope) then cover its inserts into the older tables. The read
-- policies let it see every row of the organization, in every workspace and
-- every deployment, for its refusal of an organization that has rows.
CREATE POLICY pm_operator_bootstrap_read ON pm.bootstrap_records
    FOR SELECT TO pm_app_migrator
    USING (org_id = NULLIF(current_setting('pm.org_id', true), ''));
CREATE POLICY pm_operator_bootstrap_write ON pm.bootstrap_records
    FOR INSERT TO pm_app_migrator
    WITH CHECK (
        deployment_id = NULLIF(current_setting('pm.deployment_id', true), '')
        AND org_id = NULLIF(current_setting('pm.org_id', true), '')
    );
CREATE POLICY pm_operator_bootstrap_read ON pm.workspace_audit_entries
    FOR SELECT TO pm_app_migrator
    USING (org_id = NULLIF(current_setting('pm.org_id', true), ''));
CREATE POLICY pm_operator_bootstrap_write ON pm.workspace_audit_entries
    FOR INSERT TO pm_app_migrator
    WITH CHECK (
        org_id = NULLIF(current_setting('pm.org_id', true), '')
        AND workspace_id = NULLIF(current_setting('pm.workspace_id', true), '')
    );
CREATE POLICY pm_operator_bootstrap_read ON pm.workspace_namespaces
    FOR SELECT TO pm_app_migrator
    USING (org_id = NULLIF(current_setting('pm.org_id', true), ''));
CREATE POLICY pm_operator_bootstrap_read ON pm.memberships
    FOR SELECT TO pm_app_migrator
    USING (org_id = NULLIF(current_setting('pm.org_id', true), ''));
-- pm_deployment_scope shows only the login bindings of the set deployment.
-- This read shows the bindings of the organization in every deployment.
CREATE POLICY pm_operator_bootstrap_read ON pm.auth_bootstrap_identities
    FOR SELECT TO pm_app_migrator
    USING (org_id = NULLIF(current_setting('pm.org_id', true), ''));

-- pm.operator_bootstrap writes one bootstrap: the bootstrap record, the
-- workspace, the admin identity, its login binding, the admin membership, the
-- provider accounts and the audit record. It refuses an organization that has
-- a row in any of these tables, and an admin login that has a binding in the
-- deployment. It returns one word:
--   bootstrapped       it wrote the rows.
--   scope_same_input   the scope has a bootstrap of the same input. Nothing written.
--   scope_other_input  the scope has another bootstrap, or the organization has
--                      rows. Nothing written.
--   login_bound        the admin issuer and subject have a binding. Nothing written.
-- The input digest is the SHA-256 of the canonical JSON of the operator input
-- (organization, workspace, workspace name, admin issuer and subject, and the
-- connector and display name of each provider account, in input order). The
-- function computes it from the values that it writes, so a caller cannot
-- choose it.
CREATE FUNCTION pm.operator_bootstrap(
    bootstrap_deployment_id text,
    bootstrap_org_id text,
    bootstrap_workspace_id text,
    bootstrap_workspace_name text,
    bootstrap_admin_identity_id text,
    bootstrap_admin_issuer text,
    bootstrap_admin_subject text,
    bootstrap_membership_id text,
    bootstrap_role text,
    bootstrap_permissions text[],
    bootstrap_account_ids text[],
    bootstrap_account_connectors text[],
    bootstrap_account_names text[],
    bootstrap_account_capacities bigint[],
    bootstrap_audit_id text,
    bootstrap_action text
)
RETURNS text
LANGUAGE plpgsql
VOLATILE
SECURITY DEFINER
SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    account_count integer;
    computed_digest text;
    stored_digest text;
    conflict_name text;
BEGIN
    -- Each text is nonempty, at most 512 bytes, with no outer space and no
    -- control character. The lists have no NULL item and equal account lengths.
    account_count := cardinality(bootstrap_account_ids);
    IF bootstrap_permissions IS NULL OR cardinality(bootstrap_permissions) NOT BETWEEN 1 AND 64
       OR account_count IS NULL OR account_count > 64
       OR cardinality(bootstrap_account_connectors) IS DISTINCT FROM account_count
       OR cardinality(bootstrap_account_names) IS DISTINCT FROM account_count
       OR cardinality(bootstrap_account_capacities) IS DISTINCT FROM account_count
       OR array_position(bootstrap_account_capacities, NULL) IS NOT NULL
       OR EXISTS (SELECT 1 FROM unnest(bootstrap_account_capacities) AS capacity WHERE capacity <= 0)
       OR EXISTS (
           SELECT 1
           FROM unnest(ARRAY[
               bootstrap_deployment_id, bootstrap_org_id, bootstrap_workspace_id, bootstrap_workspace_name,
               bootstrap_admin_identity_id, bootstrap_admin_issuer, bootstrap_admin_subject,
               bootstrap_membership_id, bootstrap_role, bootstrap_audit_id, bootstrap_action
           ] || bootstrap_permissions || bootstrap_account_ids || bootstrap_account_connectors
             || bootstrap_account_names) AS item(value)
           WHERE item.value IS NULL OR item.value = '' OR octet_length(item.value) > 512
              OR btrim(item.value) <> item.value OR item.value ~ '[[:cntrl:]]'
       ) THEN
        RAISE EXCEPTION USING ERRCODE = '22023', MESSAGE = 'operator bootstrap input is invalid';
    END IF;

    computed_digest := 'sha256:' || encode(sha256(convert_to(jsonb_build_object(
        'org_id', bootstrap_org_id,
        'workspace_id', bootstrap_workspace_id,
        'workspace_name', bootstrap_workspace_name,
        'admin_issuer', bootstrap_admin_issuer,
        'admin_subject', bootstrap_admin_subject,
        'provider_accounts', COALESCE((
            SELECT jsonb_agg(
                jsonb_build_object('connector_id', account.connector_id, 'display_name', account.display_name)
                ORDER BY account.position)
            FROM unnest(bootstrap_account_connectors, bootstrap_account_names)
                WITH ORDINALITY AS account(connector_id, display_name, position)
        ), '[]'::jsonb)
    )::text, 'UTF8')), 'hex');

    -- The scope settings come from the checked input, not from the caller.
    PERFORM set_config('pm.deployment_id', bootstrap_deployment_id, true),
            set_config('pm.org_id', bootstrap_org_id, true),
            set_config('pm.workspace_id', bootstrap_workspace_id, true),
            set_config('pm.requested_actor_id', 'bootstrap', true),
            set_config('pm.requested_actor_kind', 'operator', true),
            set_config('pm.effective_actor_id', 'bootstrap', true),
            set_config('pm.effective_actor_kind', 'operator', true);

    SELECT stored.input_digest INTO stored_digest
    FROM pm.bootstrap_records AS stored
    WHERE stored.deployment_id = bootstrap_deployment_id AND stored.org_id = bootstrap_org_id;
    IF FOUND THEN
        RETURN CASE WHEN stored_digest = computed_digest THEN 'scope_same_input' ELSE 'scope_other_input' END;
    END IF;
    IF EXISTS (SELECT 1 FROM pm.bootstrap_records AS existing WHERE existing.org_id = bootstrap_org_id)
       OR EXISTS (SELECT 1 FROM pm.workspace_namespaces AS existing WHERE existing.org_id = bootstrap_org_id)
       OR EXISTS (SELECT 1 FROM pm.identities AS existing WHERE existing.org_id = bootstrap_org_id)
       OR EXISTS (SELECT 1 FROM pm.auth_bootstrap_identities AS existing WHERE existing.org_id = bootstrap_org_id)
       OR EXISTS (SELECT 1 FROM pm.memberships AS existing WHERE existing.org_id = bootstrap_org_id)
       OR EXISTS (SELECT 1 FROM pm.provider_accounts AS existing WHERE existing.org_id = bootstrap_org_id)
       OR EXISTS (SELECT 1 FROM pm.workspace_audit_entries AS existing WHERE existing.org_id = bootstrap_org_id) THEN
        RETURN 'scope_other_input';
    END IF;
    IF EXISTS (
        SELECT 1 FROM pm.auth_bootstrap_identities AS binding
        WHERE binding.deployment_id = bootstrap_deployment_id
          AND binding.issuer = bootstrap_admin_issuer AND binding.subject = bootstrap_admin_subject
    ) THEN
        RETURN 'login_bound';
    END IF;

    -- The bootstrap record comes first: its primary key is where a concurrent
    -- bootstrap of the same scope waits and then fails. A unique violation
    -- undoes every insert of this block.
    BEGIN
        INSERT INTO pm.bootstrap_records (deployment_id, org_id, workspace_id, admin_identity_id, membership_id, input_digest)
        VALUES (bootstrap_deployment_id, bootstrap_org_id, bootstrap_workspace_id, bootstrap_admin_identity_id,
                bootstrap_membership_id, computed_digest);
        INSERT INTO pm.workspace_namespaces (org_id, workspace_id, name)
        VALUES (bootstrap_org_id, bootstrap_workspace_id, bootstrap_workspace_name);
        INSERT INTO pm.identities (org_id, identity_id, kind, issuer, subject, revision)
        VALUES (bootstrap_org_id, bootstrap_admin_identity_id, 'user', bootstrap_admin_issuer, bootstrap_admin_subject, 1);
        INSERT INTO pm.auth_bootstrap_identities (
            deployment_id, issuer, subject, org_id, principal_id, principal_kind, can_approve_devices, enabled
        ) VALUES (bootstrap_deployment_id, bootstrap_admin_issuer, bootstrap_admin_subject, bootstrap_org_id,
                  bootstrap_admin_identity_id, 'user', true, true);
        INSERT INTO pm.memberships (
            org_id, workspace_id, membership_id, identity_id, role,
            resource_grants, permissions, connection_ids, operation_ids, policy_epoch, control_revision
        ) VALUES (
            bootstrap_org_id, bootstrap_workspace_id, bootstrap_membership_id, bootstrap_admin_identity_id, bootstrap_role,
            ARRAY(SELECT 'permission:' || permission FROM unnest(bootstrap_permissions) AS permission
                  ORDER BY ('permission:' || permission) COLLATE "C"),
            bootstrap_permissions, ARRAY[]::text[], ARRAY[]::text[], 1, 1
        );
        INSERT INTO pm.provider_accounts (org_id, provider_account_id, provider, account_witness, capacity, revision, display_name)
        SELECT bootstrap_org_id, account.account_id, account.connector_id, account.account_id, account.capacity, 1,
               account.display_name
        FROM unnest(bootstrap_account_ids, bootstrap_account_connectors, bootstrap_account_capacities, bootstrap_account_names)
            AS account(account_id, connector_id, capacity, display_name);
        INSERT INTO pm.workspace_audit_entries (
            org_id, workspace_id, audit_id, requested_actor_id, requested_actor_kind,
            effective_actor_id, effective_actor_kind, action, target_ref, payload
        ) VALUES (
            bootstrap_org_id, bootstrap_workspace_id, bootstrap_audit_id, 'bootstrap', 'operator',
            'bootstrap', 'operator', bootstrap_action, bootstrap_workspace_id,
            jsonb_build_object(
                'references', jsonb_build_array(
                    jsonb_build_object('name', 'deployment', 'ref', bootstrap_deployment_id),
                    jsonb_build_object('name', 'admin_identity', 'ref', bootstrap_admin_identity_id),
                    jsonb_build_object('name', 'membership', 'ref', bootstrap_membership_id)
                ),
                'counts', jsonb_build_array(jsonb_build_object('name', 'provider_accounts', 'value', account_count)),
                'reason_codes', '[]'::jsonb
            )
        );
    EXCEPTION WHEN unique_violation THEN
        GET STACKED DIAGNOSTICS conflict_name = CONSTRAINT_NAME;
        IF conflict_name = 'bootstrap_records_pkey' THEN
            SELECT stored.input_digest INTO stored_digest
            FROM pm.bootstrap_records AS stored
            WHERE stored.deployment_id = bootstrap_deployment_id AND stored.org_id = bootstrap_org_id;
            RETURN CASE WHEN stored_digest = computed_digest THEN 'scope_same_input' ELSE 'scope_other_input' END;
        ELSIF conflict_name = 'auth_bootstrap_identities_pkey' THEN
            RETURN 'login_bound';
        END IF;
        RAISE;
    END;
    RETURN 'bootstrapped';
END
$$;

-- The role has one grant in schema pm: EXECUTE on this function. No other
-- role may execute it.
REVOKE ALL ON FUNCTION pm.operator_bootstrap(
    text, text, text, text, text, text, text, text, text, text[], text[], text[], text[], bigint[], text, text
) FROM PUBLIC, pm_app_runtime;
GRANT USAGE ON SCHEMA pm TO pm_app_bootstrap;
GRANT EXECUTE ON FUNCTION pm.operator_bootstrap(
    text, text, text, text, text, text, text, text, text, text[], text[], text[], text[], bigint[], text, text
) TO pm_app_bootstrap;

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

REVOKE ALL ON FUNCTION pm.operator_bootstrap(
    text, text, text, text, text, text, text, text, text, text[], text[], text[], text[], bigint[], text, text
) FROM PUBLIC, pm_app_runtime, pm_app_bootstrap;
DROP FUNCTION pm.operator_bootstrap(
    text, text, text, text, text, text, text, text, text, text[], text[], text[], text[], bigint[], text, text
);
DROP POLICY pm_operator_bootstrap_read ON pm.auth_bootstrap_identities;
DROP POLICY pm_operator_bootstrap_read ON pm.memberships;
DROP POLICY pm_operator_bootstrap_read ON pm.workspace_namespaces;

REVOKE ALL ON pm.provider_accounts, pm.memberships, pm.auth_bootstrap_identities,
    pm.identities, pm.workspace_namespaces FROM pm_app_bootstrap;
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
