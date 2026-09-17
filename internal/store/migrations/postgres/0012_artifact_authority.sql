-- Artifact availability is gated by one trusted workspace connection binding.
-- Existing unbound rows remain readable only to internal metadata owners and fail
-- closed at public authorization until a real owner republishes them.

-- +goose Up
-- +goose StatementBegin
DO $$
BEGIN
    IF current_database() <> 'pm_app' OR current_user <> 'pm_app_migrator' THEN
        RAISE EXCEPTION 'artifact authority schema requires pm_app_migrator on pm_app';
    END IF;
    IF to_regrole('pm_app_runtime') IS NULL THEN
        RAISE EXCEPTION 'required runtime role is missing';
    END IF;
    IF EXISTS (
        SELECT 1 FROM pg_roles
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
        WHERE singleton AND revision = 'pm.control/v11'
    ) THEN
        RAISE EXCEPTION 'artifact authority migration requires the event watermark schema';
    END IF;
END
$$;

ALTER TABLE pm.artifacts
    ADD COLUMN connection_id text,
    ADD CONSTRAINT artifacts_connection_id_valid
        CHECK (connection_id IS NULL OR connection_id <> ''),
    ADD CONSTRAINT artifacts_connection_authority_fk
        FOREIGN KEY (org_id, workspace_id, connection_id)
        REFERENCES pm.connections (org_id, workspace_id, connection_id);

ALTER TABLE pm.control_schema DROP CONSTRAINT control_schema_revision_check;
UPDATE pm.control_schema
SET revision = 'pm.control/v12', installed_at = clock_timestamp()
WHERE singleton AND revision = 'pm.control/v11';
ALTER TABLE pm.control_schema
    ADD CONSTRAINT control_schema_revision_check CHECK (revision = 'pm.control/v12');
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- FORCE RLS also applies to the owning migrator. Open only the cross-scope read
-- needed to refuse a downgrade that would erase retained connection authority.
CREATE POLICY pm_artifact_authority_migration_select ON pm.artifacts
    FOR SELECT TO pm_app_migrator USING (true);

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pm.artifacts WHERE connection_id IS NOT NULL) THEN
        RAISE EXCEPTION 'cannot roll back retained artifact connection authority';
    END IF;
END
$$;

DROP POLICY pm_artifact_authority_migration_select ON pm.artifacts;

ALTER TABLE pm.artifacts
    DROP CONSTRAINT artifacts_connection_authority_fk,
    DROP CONSTRAINT artifacts_connection_id_valid,
    DROP COLUMN connection_id;

ALTER TABLE pm.control_schema DROP CONSTRAINT control_schema_revision_check;
UPDATE pm.control_schema
SET revision = 'pm.control/v11', installed_at = clock_timestamp()
WHERE singleton AND revision = 'pm.control/v12';
ALTER TABLE pm.control_schema
    ADD CONSTRAINT control_schema_revision_check CHECK (revision = 'pm.control/v11');
-- +goose StatementEnd
