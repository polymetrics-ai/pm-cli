-- Durable event cursors require a per-run high watermark that survives retention.
-- Goose owns the transaction; each section is one SQL batch.

-- +goose Up
-- +goose StatementBegin
DO $$
BEGIN
    IF current_database() <> 'pm_app' OR current_user <> 'pm_app_migrator' THEN
        RAISE EXCEPTION 'event watermark schema requires pm_app_migrator on pm_app';
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
        WHERE singleton AND revision = 'pm.control/v10'
    ) THEN
        RAISE EXCEPTION 'event watermark migration requires the authenticated worker schema';
    END IF;
END
$$;

ALTER TABLE pm.runs
    ADD COLUMN last_event_sequence bigint NOT NULL DEFAULT 0,
    ADD CONSTRAINT runs_last_event_sequence_valid CHECK (last_event_sequence >= 0);

-- FORCE RLS also applies to the owning migrator. Open only the command-specific
-- cross-scope visibility needed by this transactional backfill.
CREATE POLICY pm_event_watermark_migration_select ON pm.runs
    FOR SELECT TO pm_app_migrator USING (true);
CREATE POLICY pm_event_watermark_migration_update ON pm.runs
    FOR UPDATE TO pm_app_migrator USING (true) WITH CHECK (true);
CREATE POLICY pm_event_watermark_migration_select ON pm.server_events
    FOR SELECT TO pm_app_migrator USING (true);

UPDATE pm.runs AS run
SET last_event_sequence = COALESCE((
    SELECT MAX(event.sequence)
    FROM pm.server_events AS event
    WHERE event.org_id = run.org_id
      AND event.workspace_id = run.workspace_id
      AND event.run_id = run.run_id
), 0);

DROP POLICY pm_event_watermark_migration_select ON pm.server_events;
DROP POLICY pm_event_watermark_migration_update ON pm.runs;
DROP POLICY pm_event_watermark_migration_select ON pm.runs;

GRANT UPDATE (last_event_sequence) ON pm.runs TO pm_app_runtime;

ALTER TABLE pm.control_schema DROP CONSTRAINT control_schema_revision_check;
UPDATE pm.control_schema
SET revision = 'pm.control/v11', installed_at = clock_timestamp()
WHERE singleton AND revision = 'pm.control/v10';
ALTER TABLE pm.control_schema
    ADD CONSTRAINT control_schema_revision_check CHECK (revision = 'pm.control/v11');
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
CREATE POLICY pm_event_watermark_migration_select ON pm.runs
    FOR SELECT TO pm_app_migrator USING (true);
CREATE POLICY pm_event_watermark_migration_select ON pm.server_events
    FOR SELECT TO pm_app_migrator USING (true);

DO $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM pm.runs AS run
        WHERE run.last_event_sequence <> COALESCE((
            SELECT MAX(event.sequence)
            FROM pm.server_events AS event
            WHERE event.org_id = run.org_id
              AND event.workspace_id = run.workspace_id
              AND event.run_id = run.run_id
        ), 0)
    ) THEN
        RAISE EXCEPTION 'cannot roll back event watermarks after retained event history has a gap';
    END IF;
END
$$;

DROP POLICY pm_event_watermark_migration_select ON pm.server_events;
DROP POLICY pm_event_watermark_migration_select ON pm.runs;

REVOKE UPDATE (last_event_sequence) ON pm.runs FROM pm_app_runtime;
ALTER TABLE pm.runs
    DROP CONSTRAINT runs_last_event_sequence_valid,
    DROP COLUMN last_event_sequence;

ALTER TABLE pm.control_schema DROP CONSTRAINT control_schema_revision_check;
UPDATE pm.control_schema
SET revision = 'pm.control/v10', installed_at = clock_timestamp()
WHERE singleton AND revision = 'pm.control/v11';
ALTER TABLE pm.control_schema
    ADD CONSTRAINT control_schema_revision_check CHECK (revision = 'pm.control/v10');
-- +goose StatementEnd
