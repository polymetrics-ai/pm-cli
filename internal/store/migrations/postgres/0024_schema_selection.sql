-- Schema selection (ADR 0007, #285 slice 285c): one selected complete
-- candidate for each connection and resource, with its selection revision.
-- The revision is 1 while no row exists. Each selection adds 1 in the same
-- transaction as the compare and set and one workspace audit record, so a row
-- always holds 2 or more. The audit record uses the runtime insert grant and
-- the pm_runtime_workspace_audit_insert policy of migration 0022.

-- +goose Up
-- +goose StatementBegin
DO $$
BEGIN
    IF current_database() <> 'pm_app' OR current_user <> 'pm_app_migrator' THEN
        RAISE EXCEPTION 'schema selection schema requires pm_app_migrator on pm_app';
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
        SELECT 1 FROM pm.control_schema WHERE singleton AND revision = 'pm.control/v23'
    ) THEN
        RAISE EXCEPTION 'schema selection migration requires the schema discovery schema';
    END IF;
END
$$;

-- A selection names a candidate of the same connection and resource. This key
-- lets the selection reference the resource and the digest together.
ALTER TABLE pm.schema_snapshots
    ADD CONSTRAINT schema_snapshots_resource_ref_key
    UNIQUE (org_id, workspace_id, connection_id, resource, schema_ref);

-- One row for each connection and resource after its first selection. The
-- runtime role can insert a row and change its reference and revision. It
-- cannot change the key columns or remove a row.
CREATE TABLE pm.schema_selections (
    org_id text NOT NULL,
    workspace_id text NOT NULL,
    connection_id text NOT NULL CHECK (connection_id <> ''),
    resource text NOT NULL CHECK (resource <> '' AND octet_length(resource) <= 512),
    schema_ref text NOT NULL CHECK (schema_ref ~ '^sha256:[0-9a-f]{64}$'),
    selection_revision bigint NOT NULL CHECK (selection_revision > 1),
    selected_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (org_id, workspace_id, connection_id, resource),
    FOREIGN KEY (org_id, workspace_id, connection_id)
        REFERENCES pm.connections (org_id, workspace_id, connection_id) ON DELETE CASCADE,
    FOREIGN KEY (org_id, workspace_id, connection_id, resource, schema_ref)
        REFERENCES pm.schema_snapshots (org_id, workspace_id, connection_id, resource, schema_ref)
);

ALTER TABLE pm.schema_selections ENABLE ROW LEVEL SECURITY;
ALTER TABLE pm.schema_selections FORCE ROW LEVEL SECURITY;
CREATE POLICY pm_scope ON pm.schema_selections
    USING (
        org_id = NULLIF(current_setting('pm.org_id', true), '')
        AND workspace_id = NULLIF(current_setting('pm.workspace_id', true), '')
    )
    WITH CHECK (
        org_id = NULLIF(current_setting('pm.org_id', true), '')
        AND workspace_id = NULLIF(current_setting('pm.workspace_id', true), '')
    );

REVOKE ALL ON TABLE pm.schema_selections FROM PUBLIC, pm_app_runtime;
GRANT SELECT ON TABLE pm.schema_selections TO pm_app_runtime;
-- selected_at is set by its default on insert and by the selection update.
GRANT INSERT (org_id, workspace_id, connection_id, resource, schema_ref, selection_revision)
    ON pm.schema_selections TO pm_app_runtime;
GRANT UPDATE (schema_ref, selection_revision, selected_at)
    ON pm.schema_selections TO pm_app_runtime;

ALTER TABLE pm.control_schema DROP CONSTRAINT control_schema_revision_check;
UPDATE pm.control_schema SET revision = 'pm.control/v24', installed_at = clock_timestamp()
WHERE singleton AND revision = 'pm.control/v23';
ALTER TABLE pm.control_schema
    ADD CONSTRAINT control_schema_revision_check CHECK (revision = 'pm.control/v24');
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- FORCE RLS applies to the owning migrator, so the refusal check needs an
-- explicit cross-scope read of the retained selections. A selection is
-- application truth, so the down step never drops one.
CREATE POLICY pm_schema_selection_migration_select ON pm.schema_selections
    FOR SELECT TO pm_app_migrator USING (true);
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pm.schema_selections) THEN
        RAISE EXCEPTION 'cannot roll back retained schema selections';
    END IF;
END
$$;
DROP POLICY pm_schema_selection_migration_select ON pm.schema_selections;

DROP TABLE pm.schema_selections;
ALTER TABLE pm.schema_snapshots DROP CONSTRAINT schema_snapshots_resource_ref_key;

ALTER TABLE pm.control_schema DROP CONSTRAINT control_schema_revision_check;
UPDATE pm.control_schema SET revision = 'pm.control/v23', installed_at = clock_timestamp()
WHERE singleton AND revision = 'pm.control/v24';
ALTER TABLE pm.control_schema
    ADD CONSTRAINT control_schema_revision_check CHECK (revision = 'pm.control/v23');
-- +goose StatementEnd
