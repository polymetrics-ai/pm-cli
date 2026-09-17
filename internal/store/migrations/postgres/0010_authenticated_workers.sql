-- Authenticated workflow activities derive tenant authority from sealed workflow
-- metadata and retain immutable worker policy/control/projection receipts.
-- Goose owns the transaction; each section is one SQL batch.

-- +goose Up
-- +goose StatementBegin
DO $$
BEGIN
    IF current_database() <> 'pm_app' OR current_user <> 'pm_app_migrator' THEN
        RAISE EXCEPTION 'authenticated worker schema requires pm_app_migrator on pm_app';
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
        WHERE singleton AND revision = 'pm.control/v9'
    ) THEN
        RAISE EXCEPTION 'authenticated worker migration requires the preceding workflow reconciliation schema';
    END IF;
END
$$;

-- Legacy snapshots remain byte-identical and are intentionally unusable by the
-- worker. New writes must carry the admitted lane; no migration guesses one.
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
            'scope', 'run_id', 'input_ref', 'lane', 'connection_id', 'input',
            'result_controls', 'approval_ref'
        ]::text[]) = '{}'::jsonb
    ),
    ADD CONSTRAINT run_input_snapshot_lane CHECK (
        NOT (snapshot ? 'lane')
        OR (
            jsonb_typeof(snapshot->'lane') = 'string'
            AND snapshot->>'lane' IN (
                'direct_read', 'direct_write', 'etl', 'reverse_etl',
                'binary_download', 'binary_upload', 'sync_transport'
            )
        )
    );

CREATE FUNCTION pm.require_run_input_lane()
RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog, pg_temp
AS $$
BEGIN
    IF NOT (NEW.snapshot ? 'lane') THEN
        RAISE EXCEPTION USING
            ERRCODE = '23514',
            MESSAGE = 'new run input snapshots require the admitted lane';
    END IF;
    RETURN NEW;
END
$$;
REVOKE ALL ON FUNCTION pm.require_run_input_lane() FROM PUBLIC, pm_app_runtime;
CREATE TRIGGER run_input_snapshot_requires_lane
BEFORE INSERT OR UPDATE OF snapshot ON pm.run_input_snapshots
FOR EACH ROW EXECUTE FUNCTION pm.require_run_input_lane();
CREATE POLICY pm_migrator_read ON pm.run_input_snapshots
    FOR SELECT TO pm_app_migrator USING (true);

CREATE TABLE pm.workflow_run_policies (
    org_id text NOT NULL,
    workspace_id text NOT NULL,
    run_id text NOT NULL,
    sealed_by_command_id text NOT NULL,
    sealed_generation bigint NOT NULL CHECK (sealed_generation > 0),
    schedule_to_close_nanoseconds bigint NOT NULL CHECK (schedule_to_close_nanoseconds > 0),
    start_to_close_nanoseconds bigint NOT NULL CHECK (start_to_close_nanoseconds > 0),
    heartbeat_nanoseconds bigint NOT NULL CHECK (heartbeat_nanoseconds > 0),
    transport_nanoseconds bigint NOT NULL CHECK (transport_nanoseconds > 0),
    sealed_at timestamptz NOT NULL,
    PRIMARY KEY (org_id, workspace_id, run_id),
    FOREIGN KEY (org_id, workspace_id, run_id)
        REFERENCES pm.runs (org_id, workspace_id, run_id) ON DELETE CASCADE,
    FOREIGN KEY (org_id, workspace_id, run_id, sealed_by_command_id)
        REFERENCES pm.outbox_commands (org_id, workspace_id, run_id, command_id) ON DELETE CASCADE,
    CHECK (schedule_to_close_nanoseconds > start_to_close_nanoseconds),
    CHECK (heartbeat_nanoseconds < start_to_close_nanoseconds),
    CHECK (transport_nanoseconds < start_to_close_nanoseconds)
);

CREATE TABLE pm.workflow_command_receipts (
    org_id text NOT NULL,
    workspace_id text NOT NULL,
    run_id text NOT NULL,
    generation bigint NOT NULL CHECK (generation > 0),
    command_id text NOT NULL,
    applied boolean NOT NULL,
    cancel_requested boolean NOT NULL,
    run_revision bigint NOT NULL CHECK (run_revision > 0),
    applied_at timestamptz NOT NULL,
    PRIMARY KEY (org_id, workspace_id, run_id, generation, command_id),
    FOREIGN KEY (org_id, workspace_id, run_id, command_id)
        REFERENCES pm.outbox_commands (org_id, workspace_id, run_id, command_id) ON DELETE CASCADE,
    CHECK (applied AND cancel_requested)
);

CREATE TABLE pm.workflow_projection_receipts (
    org_id text NOT NULL,
    workspace_id text NOT NULL,
    run_id text NOT NULL,
    generation bigint NOT NULL CHECK (generation > 0),
    projection_key text NOT NULL CHECK (projection_key <> ''),
    run_revision bigint NOT NULL CHECK (run_revision > 0),
    run_state text NOT NULL CHECK (run_state IN (
        'accepted', 'queued', 'running', 'awaiting_approval', 'blocked',
        'reconciling', 'succeeded', 'partial', 'failed', 'cancelled'
    )),
    projected_at timestamptz NOT NULL,
    PRIMARY KEY (org_id, workspace_id, run_id, generation, projection_key),
    FOREIGN KEY (org_id, workspace_id, run_id)
        REFERENCES pm.runs (org_id, workspace_id, run_id) ON DELETE CASCADE
);

ALTER TABLE pm.workflow_run_policies ENABLE ROW LEVEL SECURITY;
ALTER TABLE pm.workflow_run_policies FORCE ROW LEVEL SECURITY;
CREATE POLICY pm_scope ON pm.workflow_run_policies
    USING (
        org_id = NULLIF(current_setting('pm.org_id', true), '')
        AND workspace_id = NULLIF(current_setting('pm.workspace_id', true), '')
    )
    WITH CHECK (
        org_id = NULLIF(current_setting('pm.org_id', true), '')
        AND workspace_id = NULLIF(current_setting('pm.workspace_id', true), '')
    );
CREATE POLICY pm_migrator_read ON pm.workflow_run_policies
    FOR SELECT TO pm_app_migrator USING (true);

ALTER TABLE pm.workflow_command_receipts ENABLE ROW LEVEL SECURITY;
ALTER TABLE pm.workflow_command_receipts FORCE ROW LEVEL SECURITY;
CREATE POLICY pm_scope ON pm.workflow_command_receipts
    USING (
        org_id = NULLIF(current_setting('pm.org_id', true), '')
        AND workspace_id = NULLIF(current_setting('pm.workspace_id', true), '')
    )
    WITH CHECK (
        org_id = NULLIF(current_setting('pm.org_id', true), '')
        AND workspace_id = NULLIF(current_setting('pm.workspace_id', true), '')
    );
CREATE POLICY pm_migrator_read ON pm.workflow_command_receipts
    FOR SELECT TO pm_app_migrator USING (true);

ALTER TABLE pm.workflow_projection_receipts ENABLE ROW LEVEL SECURITY;
ALTER TABLE pm.workflow_projection_receipts FORCE ROW LEVEL SECURITY;
CREATE POLICY pm_scope ON pm.workflow_projection_receipts
    USING (
        org_id = NULLIF(current_setting('pm.org_id', true), '')
        AND workspace_id = NULLIF(current_setting('pm.workspace_id', true), '')
    )
    WITH CHECK (
        org_id = NULLIF(current_setting('pm.org_id', true), '')
        AND workspace_id = NULLIF(current_setting('pm.workspace_id', true), '')
    );
CREATE POLICY pm_migrator_read ON pm.workflow_projection_receipts
    FOR SELECT TO pm_app_migrator USING (true);

REVOKE ALL ON TABLE pm.workflow_run_policies FROM PUBLIC, pm_app_runtime;
REVOKE ALL ON TABLE pm.workflow_command_receipts FROM PUBLIC, pm_app_runtime;
REVOKE ALL ON TABLE pm.workflow_projection_receipts FROM PUBLIC, pm_app_runtime;
GRANT SELECT, INSERT ON TABLE pm.workflow_run_policies TO pm_app_runtime;
GRANT SELECT, INSERT ON TABLE pm.workflow_command_receipts TO pm_app_runtime;
GRANT SELECT, INSERT ON TABLE pm.workflow_projection_receipts TO pm_app_runtime;

-- FORCE RLS also applies to the migrator owner. This policy exposes only runs
-- backed by a sealed workflow-start/binding intent to the bounded definer route.
CREATE POLICY pm_worker_route_select ON pm.runs
    FOR SELECT TO pm_app_migrator
    USING (
        EXISTS (
            SELECT 1
            FROM pm.workflow_command_intents AS route_intent
            WHERE route_intent.org_id = runs.org_id
              AND route_intent.workspace_id = runs.workspace_id
              AND route_intent.run_id = runs.run_id
              AND route_intent.kind IN ('start_run', 'ensure_workflow_binding')
        )
    );

CREATE FUNCTION pm.resolve_workflow_activity(
    route_run_id text,
    route_command_id text,
    route_namespace text,
    route_workflow_id text,
    route_generation bigint,
    route_intent_digest text,
    route_temporal_run_id text
)
RETURNS TABLE (
    org_id text,
    workspace_id text,
    actor jsonb
)
LANGUAGE plpgsql
STABLE
SECURITY DEFINER
SET search_path = pg_catalog, pg_temp
AS $$
BEGIN
    IF route_run_id IS NULL OR route_run_id = '' OR octet_length(route_run_id) > 512
       OR btrim(route_run_id) <> route_run_id OR route_run_id ~ '[[:cntrl:]]'
       OR route_command_id IS NULL OR route_command_id = '' OR octet_length(route_command_id) > 512
       OR btrim(route_command_id) <> route_command_id OR route_command_id ~ '[[:cntrl:]]'
       OR route_namespace IS NULL OR route_namespace = '' OR octet_length(route_namespace) > 512
       OR btrim(route_namespace) <> route_namespace OR route_namespace ~ '[[:cntrl:]]'
       OR route_workflow_id IS NULL OR route_workflow_id = '' OR octet_length(route_workflow_id) > 512
       OR btrim(route_workflow_id) <> route_workflow_id OR route_workflow_id ~ '[[:cntrl:]]'
       OR route_temporal_run_id IS NULL OR route_temporal_run_id = '' OR octet_length(route_temporal_run_id) > 512
       OR btrim(route_temporal_run_id) <> route_temporal_run_id OR route_temporal_run_id ~ '[[:cntrl:]]'
       OR route_generation IS NULL OR route_generation < 1
       OR route_intent_digest IS NULL
       OR route_intent_digest !~ '^sha256:[0-9a-f]{64}$' THEN
        RAISE EXCEPTION USING ERRCODE = '22023', MESSAGE = 'workflow activity route is invalid';
    END IF;

    RETURN QUERY
    SELECT intent.org_id, intent.workspace_id, accepted.spec->'actor'
    FROM pm.workflow_command_intents AS intent
    JOIN pm.outbox_commands AS command
      ON command.org_id = intent.org_id
     AND command.workspace_id = intent.workspace_id
     AND command.run_id = intent.run_id
     AND command.command_id = intent.command_id
    JOIN pm.run_specs AS accepted
      ON accepted.org_id = intent.org_id
     AND accepted.workspace_id = intent.workspace_id
     AND accepted.run_id = intent.run_id
    JOIN pm.runs AS durable_run
      ON durable_run.org_id = intent.org_id
     AND durable_run.workspace_id = intent.workspace_id
     AND durable_run.run_id = intent.run_id
    WHERE intent.run_id = route_run_id
      AND intent.command_id = route_command_id
      AND intent.kind IN ('start_run', 'ensure_workflow_binding')
      AND intent.namespace = route_namespace
      AND intent.workflow_id = route_workflow_id
      AND intent.generation = route_generation
      AND intent.command_digest = route_intent_digest
      AND command.kind = intent.kind
      AND command.payload_ref = intent.run_id
      AND accepted.spec->'budget'->>'digest' = intent.budget_digest
      AND durable_run.record->'spec' = accepted.spec
      AND durable_run.record->'workflow_binding'->>'run_id' = intent.run_id
      AND durable_run.record->'workflow_binding'->>'workflow_id' = intent.workflow_id
      AND (durable_run.record->'workflow_binding'->>'generation')::bigint = intent.generation
      AND jsonb_typeof(durable_run.record->'workflow_binding'->'observations') = 'array'
      AND jsonb_array_length(durable_run.record->'workflow_binding'->'observations') > 0
      AND durable_run.record->'workflow_binding'->'observations'->-1->>'temporal_run_id'
          = route_temporal_run_id;
END
$$;

REVOKE ALL ON FUNCTION pm.resolve_workflow_activity(text, text, text, text, bigint, text, text)
    FROM PUBLIC, pm_app_runtime;
GRANT EXECUTE ON FUNCTION pm.resolve_workflow_activity(text, text, text, text, bigint, text, text)
    TO pm_app_runtime;

CREATE FUNCTION pm.resolve_workflow_continuation(
    route_run_id text,
    route_command_id text,
    route_namespace text,
    route_workflow_id text,
    route_generation bigint,
    route_intent_digest text,
    route_temporal_run_id text,
    route_first_execution_run_id text,
    route_previous_run_id text,
    route_workflow_type text,
    route_task_queue text,
    route_execution_timeout_nanoseconds bigint
)
RETURNS TABLE (
    org_id text,
    workspace_id text,
    actor jsonb
)
LANGUAGE plpgsql
STABLE
SECURITY DEFINER
SET search_path = pg_catalog, pg_temp
AS $$
BEGIN
    IF route_run_id IS NULL OR route_run_id = '' OR octet_length(route_run_id) > 512
       OR btrim(route_run_id) <> route_run_id OR route_run_id ~ '[[:cntrl:]]'
       OR route_command_id IS NULL OR route_command_id = '' OR octet_length(route_command_id) > 512
       OR btrim(route_command_id) <> route_command_id OR route_command_id ~ '[[:cntrl:]]'
       OR route_namespace IS NULL OR route_namespace = '' OR octet_length(route_namespace) > 512
       OR btrim(route_namespace) <> route_namespace OR route_namespace ~ '[[:cntrl:]]'
       OR route_workflow_id IS NULL OR route_workflow_id = '' OR octet_length(route_workflow_id) > 512
       OR btrim(route_workflow_id) <> route_workflow_id OR route_workflow_id ~ '[[:cntrl:]]'
       OR route_temporal_run_id IS NULL OR route_temporal_run_id = '' OR octet_length(route_temporal_run_id) > 512
       OR btrim(route_temporal_run_id) <> route_temporal_run_id OR route_temporal_run_id ~ '[[:cntrl:]]'
       OR route_first_execution_run_id IS NULL OR route_first_execution_run_id = ''
       OR octet_length(route_first_execution_run_id) > 512
       OR btrim(route_first_execution_run_id) <> route_first_execution_run_id
       OR route_first_execution_run_id ~ '[[:cntrl:]]'
       OR route_previous_run_id IS NULL OR route_previous_run_id = ''
       OR octet_length(route_previous_run_id) > 512
       OR btrim(route_previous_run_id) <> route_previous_run_id
       OR route_previous_run_id ~ '[[:cntrl:]]'
       OR route_temporal_run_id = route_first_execution_run_id
       OR route_temporal_run_id = route_previous_run_id
       OR route_workflow_type <> 'pm.run.v1'
       OR route_task_queue <> 'pm-control'
       OR route_generation IS NULL OR route_generation < 1
       OR route_execution_timeout_nanoseconds IS NULL OR route_execution_timeout_nanoseconds < 1
       OR route_intent_digest IS NULL
       OR route_intent_digest !~ '^sha256:[0-9a-f]{64}$' THEN
        RAISE EXCEPTION USING ERRCODE = '22023', MESSAGE = 'workflow continuation route is invalid';
    END IF;

    RETURN QUERY
    SELECT intent.org_id, intent.workspace_id, accepted.spec->'actor'
    FROM pm.workflow_command_intents AS intent
    JOIN pm.outbox_commands AS command
      ON command.org_id = intent.org_id
     AND command.workspace_id = intent.workspace_id
     AND command.run_id = intent.run_id
     AND command.command_id = intent.command_id
    JOIN pm.run_specs AS accepted
      ON accepted.org_id = intent.org_id
     AND accepted.workspace_id = intent.workspace_id
     AND accepted.run_id = intent.run_id
    JOIN pm.runs AS durable_run
      ON durable_run.org_id = intent.org_id
     AND durable_run.workspace_id = intent.workspace_id
     AND durable_run.run_id = intent.run_id
    WHERE intent.run_id = route_run_id
      AND intent.command_id = route_command_id
      AND intent.kind IN ('start_run', 'ensure_workflow_binding')
      AND intent.namespace = route_namespace
      AND intent.workflow_id = route_workflow_id
      AND intent.generation = route_generation
      AND intent.command_digest = route_intent_digest
      AND intent.workflow_type IN ('pm.run.v1', 'pm.scheduled-run.v1')
      AND intent.task_queue = route_task_queue
      AND intent.execution_timeout_nanoseconds = route_execution_timeout_nanoseconds
      AND command.kind = intent.kind
      AND command.payload_ref = intent.run_id
      AND accepted.spec->'budget'->>'digest' = intent.budget_digest
      AND durable_run.record->'spec' = accepted.spec
      AND durable_run.record->'workflow_binding'->>'run_id' = intent.run_id
      AND durable_run.record->'workflow_binding'->>'workflow_id' = intent.workflow_id
      AND (durable_run.record->'workflow_binding'->>'generation')::bigint = intent.generation
      AND jsonb_typeof(durable_run.record->'workflow_binding'->'observations') = 'array'
      AND jsonb_array_length(durable_run.record->'workflow_binding'->'observations') > 0
      AND durable_run.record->'workflow_binding'->'observations'->0->>'temporal_run_id'
          = route_first_execution_run_id
      AND (
          (
              durable_run.record->'workflow_binding'->'observations'->-1->>'temporal_run_id'
                  = route_previous_run_id
              AND NOT EXISTS (
                  SELECT 1
                  FROM jsonb_array_elements(
                      durable_run.record->'workflow_binding'->'observations'
                  ) AS observation(value)
                  WHERE observation.value->>'temporal_run_id' = route_temporal_run_id
              )
          )
          OR (
              jsonb_array_length(durable_run.record->'workflow_binding'->'observations') > 1
              AND durable_run.record->'workflow_binding'->'observations'->-1->>'temporal_run_id'
                  = route_temporal_run_id
              AND durable_run.record->'workflow_binding'->'observations'
                      ->(jsonb_array_length(
                          durable_run.record->'workflow_binding'->'observations'
                      ) - 2)->>'temporal_run_id'
                  = route_previous_run_id
          )
      );
END
$$;

REVOKE ALL ON FUNCTION pm.resolve_workflow_continuation(
    text, text, text, text, bigint, text, text, text, text, text, text, bigint
) FROM PUBLIC, pm_app_runtime;
GRANT EXECUTE ON FUNCTION pm.resolve_workflow_continuation(
    text, text, text, text, bigint, text, text, text, text, text, text, bigint
) TO pm_app_runtime;

ALTER TABLE pm.control_schema DROP CONSTRAINT control_schema_revision_check;
UPDATE pm.control_schema
SET revision = 'pm.control/v10', installed_at = clock_timestamp()
WHERE singleton AND revision = 'pm.control/v9';
ALTER TABLE pm.control_schema
    ADD CONSTRAINT control_schema_revision_check CHECK (revision = 'pm.control/v10');
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pm.workflow_run_policies)
       OR EXISTS (SELECT 1 FROM pm.workflow_command_receipts)
       OR EXISTS (SELECT 1 FROM pm.workflow_projection_receipts)
       OR EXISTS (
           SELECT 1 FROM pm.run_input_snapshots WHERE snapshot ? 'lane'
       ) THEN
        RAISE EXCEPTION 'cannot roll back authenticated worker schema while v10 records exist';
    END IF;
END
$$;

DROP POLICY pm_migrator_read ON pm.run_input_snapshots;
DROP TRIGGER run_input_snapshot_requires_lane ON pm.run_input_snapshots;
REVOKE ALL ON FUNCTION pm.require_run_input_lane() FROM PUBLIC, pm_app_runtime;
DROP FUNCTION pm.require_run_input_lane();
ALTER TABLE pm.run_input_snapshots
    DROP CONSTRAINT run_input_snapshot_lane,
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
    );

ALTER TABLE pm.control_schema DROP CONSTRAINT control_schema_revision_check;
UPDATE pm.control_schema
SET revision = 'pm.control/v9', installed_at = clock_timestamp()
WHERE singleton AND revision = 'pm.control/v10';
ALTER TABLE pm.control_schema
    ADD CONSTRAINT control_schema_revision_check CHECK (revision = 'pm.control/v9');

REVOKE ALL ON FUNCTION pm.resolve_workflow_continuation(
    text, text, text, text, bigint, text, text, text, text, text, text, bigint
) FROM PUBLIC, pm_app_runtime;
DROP FUNCTION pm.resolve_workflow_continuation(
    text, text, text, text, bigint, text, text, text, text, text, text, bigint
);
REVOKE ALL ON FUNCTION pm.resolve_workflow_activity(text, text, text, text, bigint, text, text)
    FROM PUBLIC, pm_app_runtime;
DROP FUNCTION pm.resolve_workflow_activity(text, text, text, text, bigint, text, text);
DROP POLICY pm_worker_route_select ON pm.runs;

REVOKE ALL ON TABLE pm.workflow_projection_receipts FROM PUBLIC, pm_app_runtime;
REVOKE ALL ON TABLE pm.workflow_command_receipts FROM PUBLIC, pm_app_runtime;
REVOKE ALL ON TABLE pm.workflow_run_policies FROM PUBLIC, pm_app_runtime;
DROP TABLE pm.workflow_projection_receipts;
DROP TABLE pm.workflow_command_receipts;
DROP TABLE pm.workflow_run_policies;
-- +goose StatementEnd
