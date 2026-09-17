-- Durable run-control responses share the existing scoped mutation receipt engine.
-- Goose owns the transaction; each section is one SQL batch.

-- +goose Up
-- +goose StatementBegin
DO $$
BEGIN
    IF current_database() <> 'pm_app' OR current_user <> 'pm_app_migrator' THEN
        RAISE EXCEPTION 'run control schema requires pm_app_migrator on pm_app';
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
        WHERE singleton AND revision = 'pm.control/v7'
    ) THEN
        RAISE EXCEPTION 'run control migration requires the preceding admission schema';
    END IF;
END
$$;

ALTER TABLE pm.identity_mutation_keys
    DROP CONSTRAINT identity_mutation_response_status_valid,
    ADD CONSTRAINT identity_mutation_response_status_valid
        CHECK (response_status IS NULL OR response_status IN (200, 201, 202));

ALTER TABLE pm.control_schema DROP CONSTRAINT control_schema_revision_check;
UPDATE pm.control_schema
SET revision = 'pm.control/v8', installed_at = clock_timestamp()
WHERE singleton AND revision = 'pm.control/v7';
ALTER TABLE pm.control_schema
    ADD CONSTRAINT control_schema_revision_check CHECK (revision = 'pm.control/v8');
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM pm.identity_mutation_keys
        WHERE response_status = 202
    ) THEN
        RAISE EXCEPTION 'cannot roll back run control receipts while durable 202 responses exist';
    END IF;
END
$$;

ALTER TABLE pm.identity_mutation_keys
    DROP CONSTRAINT identity_mutation_response_status_valid,
    ADD CONSTRAINT identity_mutation_response_status_valid
        CHECK (response_status IS NULL OR response_status IN (200, 201));

ALTER TABLE pm.control_schema DROP CONSTRAINT control_schema_revision_check;
UPDATE pm.control_schema
SET revision = 'pm.control/v7', installed_at = clock_timestamp()
WHERE singleton AND revision = 'pm.control/v8';
ALTER TABLE pm.control_schema
    ADD CONSTRAINT control_schema_revision_check CHECK (revision = 'pm.control/v7');
-- +goose StatementEnd
