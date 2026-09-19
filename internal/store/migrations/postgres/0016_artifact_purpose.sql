-- Public artifacts and server-only continuation artifacts share one immutable
-- registry. Existing registrations retain their public meaning explicitly.

-- +goose Up
-- +goose StatementBegin
DO $$
BEGIN
    IF current_database() <> 'pm_app' OR current_user <> 'pm_app_migrator' THEN
        RAISE EXCEPTION 'artifact purpose schema requires pm_app_migrator on pm_app';
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
        SELECT 1 FROM pm.control_schema WHERE singleton AND revision = 'pm.control/v15'
    ) THEN
        RAISE EXCEPTION 'artifact purpose migration requires the HTTP run budget schema';
    END IF;
END
$$;

ALTER TABLE pm.artifacts
    ADD COLUMN purpose text NOT NULL DEFAULT 'public',
    ADD CONSTRAINT artifacts_purpose_valid CHECK (
        purpose IN ('public', 'continuation')
        AND (purpose = 'public' OR connection_id IS NOT NULL)
    );

CREATE FUNCTION pm.preserve_artifact_registration() RETURNS trigger
    LANGUAGE plpgsql SECURITY INVOKER
    SET search_path = pg_catalog AS $$
BEGIN
    IF ROW(NEW.org_id, NEW.workspace_id, NEW.artifact_id, NEW.object_attempt_key,
           NEW.sha256, NEW.byte_length, NEW.media_type, NEW.backend_generation,
           NEW.connection_id, NEW.object_ref, NEW.registered_at, NEW.purpose)
       IS DISTINCT FROM
       ROW(OLD.org_id, OLD.workspace_id, OLD.artifact_id, OLD.object_attempt_key,
           OLD.sha256, OLD.byte_length, OLD.media_type, OLD.backend_generation,
           OLD.connection_id, OLD.object_ref, OLD.registered_at, OLD.purpose) THEN
        RAISE EXCEPTION 'artifact registration is immutable';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER artifact_registration_immutable
    BEFORE UPDATE ON pm.artifacts
    FOR EACH ROW EXECUTE FUNCTION pm.preserve_artifact_registration();
REVOKE ALL ON FUNCTION pm.preserve_artifact_registration() FROM PUBLIC;

ALTER TABLE pm.control_schema DROP CONSTRAINT control_schema_revision_check;
UPDATE pm.control_schema SET revision = 'pm.control/v16', installed_at = clock_timestamp()
WHERE singleton AND revision = 'pm.control/v15';
ALTER TABLE pm.control_schema
    ADD CONSTRAINT control_schema_revision_check CHECK (revision = 'pm.control/v16');
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- FORCE RLS applies to the owning migrator. Open only the cross-scope read
-- needed to refuse erasing any retained server-only continuation registration.
CREATE POLICY pm_artifact_purpose_migration_select ON pm.artifacts
    FOR SELECT TO pm_app_migrator USING (true);
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pm.artifacts WHERE purpose = 'continuation') THEN
        RAISE EXCEPTION 'cannot roll back retained continuation artifacts';
    END IF;
END
$$;
DROP POLICY pm_artifact_purpose_migration_select ON pm.artifacts;

DROP TRIGGER artifact_registration_immutable ON pm.artifacts;
DROP FUNCTION pm.preserve_artifact_registration();
ALTER TABLE pm.artifacts
    DROP CONSTRAINT artifacts_purpose_valid,
    DROP COLUMN purpose;

ALTER TABLE pm.control_schema DROP CONSTRAINT control_schema_revision_check;
UPDATE pm.control_schema SET revision = 'pm.control/v15', installed_at = clock_timestamp()
WHERE singleton AND revision = 'pm.control/v16';
ALTER TABLE pm.control_schema
    ADD CONSTRAINT control_schema_revision_check CHECK (revision = 'pm.control/v15');
-- +goose StatementEnd
