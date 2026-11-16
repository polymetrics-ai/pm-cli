-- The revisioned and audited membership update (#284, slice 284c).
-- pm-server adds connections to a membership and writes one workspace audit
-- record in the same transaction. This migration gives the runtime role the
-- update of the two grant columns and the insert of workspace audit records in
-- its own scope and with its own actors. It creates no role and no table.

-- +goose Up
-- +goose StatementBegin
DO $$
BEGIN
    IF current_database() <> 'pm_app' OR current_user <> 'pm_app_migrator' THEN
        RAISE EXCEPTION 'membership update schema requires pm_app_migrator on pm_app';
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
    IF NOT EXISTS (
        SELECT 1 FROM pm.control_schema WHERE singleton AND revision = 'pm.control/v21'
    ) THEN
        RAISE EXCEPTION 'membership update migration requires the operator bootstrap schema';
    END IF;
END
$$;

-- The update adds connection grants. It never changes the identity, the role,
-- the permissions, the operations or the expiry of a membership.
GRANT UPDATE (resource_grants, connection_ids) ON pm.memberships TO pm_app_runtime;

-- INSERT only. The runtime role cannot read, change or remove an audit record.
GRANT INSERT (org_id, workspace_id, audit_id, requested_actor_id, requested_actor_kind,
    effective_actor_id, effective_actor_kind, action, target_ref, payload)
    ON pm.workspace_audit_entries TO pm_app_runtime;

-- Each runtime insert names the scope and the actors of its own transaction.
CREATE POLICY pm_runtime_workspace_audit_insert ON pm.workspace_audit_entries
    FOR INSERT TO pm_app_runtime
    WITH CHECK (
        org_id = NULLIF(current_setting('pm.org_id', true), '')
        AND workspace_id = NULLIF(current_setting('pm.workspace_id', true), '')
        AND requested_actor_id = NULLIF(current_setting('pm.requested_actor_id', true), '')
        AND requested_actor_kind = NULLIF(current_setting('pm.requested_actor_kind', true), '')
        AND effective_actor_id = NULLIF(current_setting('pm.effective_actor_id', true), '')
        AND effective_actor_kind = NULLIF(current_setting('pm.effective_actor_kind', true), '')
    );

ALTER TABLE pm.control_schema DROP CONSTRAINT control_schema_revision_check;
UPDATE pm.control_schema SET revision = 'pm.control/v22', installed_at = clock_timestamp()
WHERE singleton AND revision = 'pm.control/v21';
ALTER TABLE pm.control_schema
    ADD CONSTRAINT control_schema_revision_check CHECK (revision = 'pm.control/v22');
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- The down step removes the authority only. It keeps every membership and
-- every audit record. pm.workspace_audit_entries belongs to 0021.
DROP POLICY pm_runtime_workspace_audit_insert ON pm.workspace_audit_entries;
REVOKE INSERT ON pm.workspace_audit_entries FROM pm_app_runtime;
REVOKE UPDATE (resource_grants, connection_ids) ON pm.memberships FROM pm_app_runtime;

ALTER TABLE pm.control_schema DROP CONSTRAINT control_schema_revision_check;
UPDATE pm.control_schema SET revision = 'pm.control/v21', installed_at = clock_timestamp()
WHERE singleton AND revision = 'pm.control/v22';
ALTER TABLE pm.control_schema
    ADD CONSTRAINT control_schema_revision_check CHECK (revision = 'pm.control/v21');
-- +goose StatementEnd
