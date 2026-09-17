-- Immutable applied run budgets and exact approval-presence input retention.
-- Goose owns the transaction; each section is one SQL batch.

-- +goose Up
-- +goose StatementBegin
DO $$
BEGIN
    IF current_database() <> 'pm_app' OR current_user <> 'pm_app_migrator' THEN
        RAISE EXCEPTION 'run budget schema requires pm_app_migrator on pm_app';
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
        WHERE singleton AND revision = 'pm.control/v6'
    ) THEN
        RAISE EXCEPTION 'run budget migration requires the preceding run input schema';
    END IF;
END
$$;

-- Every v6 input snapshot was admitted before approval_ref existed, so its
-- exact wire presence was necessarily absent rather than null or a string.
UPDATE pm.run_input_snapshots
SET snapshot = jsonb_set(snapshot, '{approval_ref}', '{"presence":"absent"}'::jsonb, true)
WHERE NOT snapshot ? 'approval_ref';

ALTER TABLE pm.run_input_snapshots
    DROP CONSTRAINT run_input_snapshot_document_shape;
ALTER TABLE pm.run_input_snapshots
    ADD CONSTRAINT run_input_snapshot_document_shape CHECK (
        jsonb_typeof(snapshot) = 'object'
        AND snapshot ?& ARRAY[
            'scope', 'run_id', 'input_ref', 'connection_id', 'input',
            'result_controls', 'approval_ref'
        ]::text[]
        AND (snapshot - ARRAY[
            'scope', 'run_id', 'input_ref', 'connection_id', 'input',
            'result_controls', 'approval_ref'
        ]::text[]) = '{}'::jsonb
    ),
    ADD CONSTRAINT run_input_snapshot_approval_ref CHECK (
        jsonb_typeof(snapshot->'approval_ref') = 'object'
        AND CASE snapshot->'approval_ref'->>'presence'
            WHEN 'absent' THEN ((snapshot->'approval_ref'::text) - 'presence'::text) = '{}'::jsonb
            WHEN 'null' THEN ((snapshot->'approval_ref'::text) - 'presence'::text) = '{}'::jsonb
            WHEN 'value' THEN
                (snapshot->'approval_ref') ?& ARRAY['presence', 'type', 'text']::text[]
                AND ((snapshot->'approval_ref') - ARRAY['presence', 'type', 'text']::text[]) = '{}'::jsonb
                AND (snapshot->'approval_ref'->>'type') IS NOT DISTINCT FROM 'string'
                AND jsonb_typeof(snapshot->'approval_ref'->'text') = 'string'
                AND (snapshot->'approval_ref'->>'text') <> ''
            ELSE false
        END
    );

CREATE TABLE pm.run_budget_snapshots (
    org_id text NOT NULL,
    workspace_id text NOT NULL,
    run_id text NOT NULL,
    budget_ref text NOT NULL CHECK (budget_ref <> ''),
    revision bigint NOT NULL CHECK (revision > 0),
    digest text NOT NULL CHECK (digest ~ '^sha256:[0-9a-f]{64}$'),
    snapshot jsonb NOT NULL,
    PRIMARY KEY (org_id, workspace_id, run_id),
    UNIQUE (org_id, workspace_id, budget_ref),
    FOREIGN KEY (org_id, workspace_id, run_id)
        REFERENCES pm.run_specs (org_id, workspace_id, run_id) ON DELETE CASCADE,
    CONSTRAINT run_budget_snapshot_document_shape CHECK (
        jsonb_typeof(snapshot) = 'object'
        AND snapshot ?& ARRAY[
            'scope', 'run_id', 'budget_ref', 'revision', 'policy', 'digest'
        ]::text[]
        AND (snapshot - ARRAY[
            'scope', 'run_id', 'budget_ref', 'revision', 'policy', 'digest'
        ]::text[]) = '{}'::jsonb
    ),
    CONSTRAINT run_budget_snapshot_scope_binding CHECK (
        jsonb_typeof(snapshot->'scope') = 'object'
        AND (snapshot->'scope') ?& ARRAY['org_id', 'workspace_id']::text[]
        AND ((snapshot->'scope') - ARRAY['org_id', 'workspace_id']::text[]) = '{}'::jsonb
        AND (snapshot->'scope'->>'org_id') IS NOT DISTINCT FROM org_id
        AND (snapshot->'scope'->>'workspace_id') IS NOT DISTINCT FROM workspace_id
    ),
    CONSTRAINT run_budget_snapshot_identity_binding CHECK (
        (snapshot->>'run_id') IS NOT DISTINCT FROM run_id
        AND (snapshot->>'budget_ref') IS NOT DISTINCT FROM budget_ref
        AND CASE WHEN jsonb_typeof(snapshot->'revision') = 'number'
            THEN (snapshot->>'revision')::numeric = revision
            ELSE false
        END
        AND (snapshot->>'digest') IS NOT DISTINCT FROM digest
    ),
    CONSTRAINT run_budget_snapshot_policy_shape CHECK (
        jsonb_typeof(snapshot->'policy') = 'object'
        AND (snapshot->'policy') ?& ARRAY[
            'pending', 'run_lifetime', 'input_max_bytes', 'workspace_active_runs',
            'workflow_payload_bytes', 'workflow_fanout'
        ]::text[]
        AND ((snapshot->'policy') - ARRAY[
            'pending', 'run_lifetime', 'input_max_bytes', 'workspace_active_runs',
            'workflow_payload_bytes', 'workflow_fanout'
        ]::text[]) = '{}'::jsonb
        AND jsonb_typeof(snapshot->'policy'->'pending') = 'object'
        AND (snapshot->'policy'->'pending') ?& ARRAY[
            'global_pending', 'workspace_pending', 'workspace_max_pending_age'
        ]::text[]
        AND ((snapshot->'policy'->'pending') - ARRAY[
            'global_pending', 'workspace_pending', 'workspace_max_pending_age'
        ]::text[]) = '{}'::jsonb
    ),
    CONSTRAINT run_budget_snapshot_positive_bounds CHECK (
        CASE
            WHEN jsonb_typeof(snapshot->'policy'->'run_lifetime') = 'number'
             AND jsonb_typeof(snapshot->'policy'->'input_max_bytes') = 'number'
             AND jsonb_typeof(snapshot->'policy'->'workspace_active_runs') = 'number'
             AND jsonb_typeof(snapshot->'policy'->'workflow_payload_bytes') = 'number'
             AND jsonb_typeof(snapshot->'policy'->'workflow_fanout') = 'number'
             AND jsonb_typeof(snapshot->'policy'->'pending'->'global_pending') = 'number'
             AND jsonb_typeof(snapshot->'policy'->'pending'->'workspace_pending') = 'number'
             AND jsonb_typeof(snapshot->'policy'->'pending'->'workspace_max_pending_age') = 'number'
            THEN (snapshot->'policy'->>'run_lifetime')::numeric > 0
             AND (snapshot->'policy'->>'input_max_bytes')::numeric > 0
             AND (snapshot->'policy'->>'workspace_active_runs')::numeric > 0
             AND (snapshot->'policy'->>'workflow_payload_bytes')::numeric > 0
             AND (snapshot->'policy'->>'workflow_fanout')::numeric > 0
             AND (snapshot->'policy'->'pending'->>'global_pending')::numeric > 0
             AND (snapshot->'policy'->'pending'->>'workspace_pending')::numeric > 0
             AND (snapshot->'policy'->'pending'->>'workspace_pending')::numeric
                 <= (snapshot->'policy'->'pending'->>'global_pending')::numeric
             AND (snapshot->'policy'->'pending'->>'workspace_max_pending_age')::numeric > 0
            ELSE false
        END
    )
);

CREATE FUNCTION pm.require_run_budget_spec_binding() RETURNS trigger
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
          AND accepted.spec->'budget' = jsonb_build_object(
              'budget_ref', NEW.budget_ref,
              'revision', NEW.revision,
              'digest', NEW.digest
          )
    ) THEN
        RAISE EXCEPTION 'run budget is not bound to the accepted RunSpec'
            USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END
$$;
REVOKE ALL ON FUNCTION pm.require_run_budget_spec_binding() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION pm.require_run_budget_spec_binding() TO pm_app_runtime;
CREATE TRIGGER run_budget_snapshot_requires_spec_binding
    BEFORE INSERT ON pm.run_budget_snapshots
    FOR EACH ROW EXECUTE FUNCTION pm.require_run_budget_spec_binding();

ALTER TABLE pm.run_budget_snapshots ENABLE ROW LEVEL SECURITY;
ALTER TABLE pm.run_budget_snapshots FORCE ROW LEVEL SECURITY;
CREATE POLICY pm_scope ON pm.run_budget_snapshots
    USING (
        org_id = NULLIF(current_setting('pm.org_id', true), '')
        AND workspace_id = NULLIF(current_setting('pm.workspace_id', true), '')
    )
    WITH CHECK (
        org_id = NULLIF(current_setting('pm.org_id', true), '')
        AND workspace_id = NULLIF(current_setting('pm.workspace_id', true), '')
    );
REVOKE ALL ON TABLE pm.run_budget_snapshots FROM PUBLIC, pm_app_runtime;
GRANT SELECT ON TABLE pm.run_budget_snapshots TO pm_app_runtime;
GRANT INSERT (org_id, workspace_id, run_id, budget_ref, revision, digest, snapshot)
    ON pm.run_budget_snapshots TO pm_app_runtime;

ALTER TABLE pm.control_schema DROP CONSTRAINT control_schema_revision_check;
UPDATE pm.control_schema
SET revision = 'pm.control/v7', installed_at = clock_timestamp()
WHERE singleton AND revision = 'pm.control/v6';
ALTER TABLE pm.control_schema
    ADD CONSTRAINT control_schema_revision_check CHECK (revision = 'pm.control/v7');
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TRIGGER run_budget_snapshot_requires_spec_binding ON pm.run_budget_snapshots;
REVOKE ALL ON TABLE pm.run_budget_snapshots FROM PUBLIC, pm_app_runtime;
DROP TABLE pm.run_budget_snapshots;
REVOKE ALL ON FUNCTION pm.require_run_budget_spec_binding() FROM PUBLIC, pm_app_runtime;
DROP FUNCTION pm.require_run_budget_spec_binding();

ALTER TABLE pm.run_input_snapshots
    DROP CONSTRAINT run_input_snapshot_approval_ref,
    DROP CONSTRAINT run_input_snapshot_document_shape;
UPDATE pm.run_input_snapshots
SET snapshot = snapshot - 'approval_ref';
ALTER TABLE pm.run_input_snapshots
    ADD CONSTRAINT run_input_snapshot_document_shape CHECK (
        jsonb_typeof(snapshot) = 'object'
        AND snapshot ?& ARRAY[
            'scope', 'run_id', 'input_ref', 'connection_id', 'input', 'result_controls'
        ]::text[]
        AND (snapshot - ARRAY[
            'scope', 'run_id', 'input_ref', 'connection_id', 'input', 'result_controls'
        ]::text[]) = '{}'::jsonb
    );

ALTER TABLE pm.control_schema DROP CONSTRAINT control_schema_revision_check;
UPDATE pm.control_schema
SET revision = 'pm.control/v6', installed_at = clock_timestamp()
WHERE singleton AND revision = 'pm.control/v7';
ALTER TABLE pm.control_schema
    ADD CONSTRAINT control_schema_revision_check CHECK (revision = 'pm.control/v6');
-- +goose StatementEnd
