-- Immutable logical invocation inputs bound to accepted RunSpecs.
-- Goose owns the transaction; each section is one SQL batch.

-- +goose Up
-- +goose StatementBegin
DO $$
BEGIN
    IF current_database() <> 'pm_app' OR current_user <> 'pm_app_migrator' THEN
        RAISE EXCEPTION 'run input schema requires pm_app_migrator on pm_app';
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
        WHERE singleton AND revision = 'pm.control/v5'
    ) THEN
        RAISE EXCEPTION 'run input migration requires the preceding run execution schema';
    END IF;
END
$$;

-- Existing runs deliberately receive no fabricated input. Every inserted row
-- must come from an explicit snapshot bound to an accepted RunSpec.
CREATE TABLE pm.run_input_snapshots (
    org_id text NOT NULL,
    workspace_id text NOT NULL,
    run_id text NOT NULL,
    input_ref text NOT NULL CHECK (input_ref <> ''),
    connection_id text NOT NULL CHECK (connection_id <> ''),
    snapshot jsonb NOT NULL,
    PRIMARY KEY (org_id, workspace_id, run_id),
    UNIQUE (org_id, workspace_id, input_ref),
    FOREIGN KEY (org_id, workspace_id, run_id)
        REFERENCES pm.run_specs (org_id, workspace_id, run_id) ON DELETE CASCADE,
    CONSTRAINT run_input_snapshot_document_shape CHECK (
        jsonb_typeof(snapshot) = 'object'
        AND snapshot ?& ARRAY[
            'scope', 'run_id', 'input_ref', 'connection_id', 'input', 'result_controls'
        ]::text[]
        AND (snapshot - ARRAY[
            'scope', 'run_id', 'input_ref', 'connection_id', 'input', 'result_controls'
        ]::text[]) = '{}'::jsonb
    ),
    CONSTRAINT run_input_snapshot_scope_binding CHECK (
        jsonb_typeof(snapshot->'scope') = 'object'
        AND (snapshot->'scope') ?& ARRAY['org_id', 'workspace_id']::text[]
        AND ((snapshot->'scope') - ARRAY['org_id', 'workspace_id']::text[]) = '{}'::jsonb
        AND (snapshot->'scope'->>'org_id') IS NOT DISTINCT FROM org_id
        AND (snapshot->'scope'->>'workspace_id') IS NOT DISTINCT FROM workspace_id
    ),
    CONSTRAINT run_input_snapshot_identity_binding CHECK (
        (snapshot->>'run_id') IS NOT DISTINCT FROM run_id
        AND (snapshot->>'input_ref') IS NOT DISTINCT FROM input_ref
        AND (snapshot->>'connection_id') IS NOT DISTINCT FROM connection_id
    ),
    CONSTRAINT run_input_snapshot_typed_object CHECK (
        jsonb_typeof(snapshot->'input') = 'object'
        AND (snapshot->'input') ?& ARRAY['presence', 'type', 'fields']::text[]
        AND ((snapshot->'input') - ARRAY['presence', 'type', 'fields']::text[]) = '{}'::jsonb
        AND (snapshot->'input'->>'presence') IS NOT DISTINCT FROM 'value'
        AND (snapshot->'input'->>'type') IS NOT DISTINCT FROM 'object'
        AND jsonb_typeof(snapshot->'input'->'fields') = 'object'
    ),
    CONSTRAINT run_input_snapshot_result_controls CHECK (
        jsonb_typeof(snapshot->'result_controls') = 'object'
        AND ((snapshot->'result_controls') - ARRAY['selection', 'filter']::text[]) = '{}'::jsonb
    )
);

CREATE INDEX run_input_snapshots_connection_idx
    ON pm.run_input_snapshots (org_id, workspace_id, connection_id, run_id);

CREATE FUNCTION pm.require_run_input_spec_binding() RETURNS trigger
    LANGUAGE plpgsql SECURITY INVOKER
    SET search_path = pg_catalog
AS $$
BEGIN
    IF NOT EXISTS (
        SELECT 1
        FROM pm.run_specs AS accepted
        WHERE accepted.org_id = NEW.org_id
          AND accepted.workspace_id = NEW.workspace_id
          AND accepted.run_id = NEW.run_id
          AND jsonb_typeof(accepted.spec->'input_refs') = 'array'
          AND (accepted.spec->'input_refs') ? NEW.input_ref
    ) THEN
        RAISE EXCEPTION 'run input reference is not bound to the accepted RunSpec'
            USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END
$$;
REVOKE ALL ON FUNCTION pm.require_run_input_spec_binding() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION pm.require_run_input_spec_binding() TO pm_app_runtime;
CREATE TRIGGER run_input_snapshot_requires_spec_binding
    BEFORE INSERT ON pm.run_input_snapshots
    FOR EACH ROW EXECUTE FUNCTION pm.require_run_input_spec_binding();

ALTER TABLE pm.run_input_snapshots ENABLE ROW LEVEL SECURITY;
ALTER TABLE pm.run_input_snapshots FORCE ROW LEVEL SECURITY;
CREATE POLICY pm_scope ON pm.run_input_snapshots
    USING (
        org_id = NULLIF(current_setting('pm.org_id', true), '')
        AND workspace_id = NULLIF(current_setting('pm.workspace_id', true), '')
    )
    WITH CHECK (
        org_id = NULLIF(current_setting('pm.org_id', true), '')
        AND workspace_id = NULLIF(current_setting('pm.workspace_id', true), '')
    );
REVOKE ALL ON TABLE pm.run_input_snapshots FROM PUBLIC, pm_app_runtime;
GRANT SELECT ON TABLE pm.run_input_snapshots TO pm_app_runtime;
GRANT INSERT (org_id, workspace_id, run_id, input_ref, connection_id, snapshot)
    ON pm.run_input_snapshots TO pm_app_runtime;

ALTER TABLE pm.control_schema DROP CONSTRAINT control_schema_revision_check;
UPDATE pm.control_schema
SET revision = 'pm.control/v6', installed_at = clock_timestamp()
WHERE singleton AND revision = 'pm.control/v5';
ALTER TABLE pm.control_schema
    ADD CONSTRAINT control_schema_revision_check CHECK (revision = 'pm.control/v6');
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TRIGGER run_input_snapshot_requires_spec_binding ON pm.run_input_snapshots;
REVOKE ALL ON TABLE pm.run_input_snapshots FROM PUBLIC, pm_app_runtime;
DROP TABLE pm.run_input_snapshots;
REVOKE ALL ON FUNCTION pm.require_run_input_spec_binding() FROM PUBLIC, pm_app_runtime;
DROP FUNCTION pm.require_run_input_spec_binding();

ALTER TABLE pm.control_schema DROP CONSTRAINT control_schema_revision_check;
UPDATE pm.control_schema
SET revision = 'pm.control/v5', installed_at = clock_timestamp()
WHERE singleton AND revision = 'pm.control/v6';
ALTER TABLE pm.control_schema
    ADD CONSTRAINT control_schema_revision_check CHECK (revision = 'pm.control/v5');
-- +goose StatementEnd
