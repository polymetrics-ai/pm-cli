-- Durable workflow handoff and projection repair use the existing PM outbox.
-- Goose owns the transaction; each section is one SQL batch.

-- +goose Up
-- +goose StatementBegin
DO $$
BEGIN
    IF current_database() <> 'pm_app' OR current_user <> 'pm_app_migrator' THEN
        RAISE EXCEPTION 'workflow reconciliation schema requires pm_app_migrator on pm_app';
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
        WHERE singleton AND revision = 'pm.control/v8'
    ) THEN
        RAISE EXCEPTION 'workflow reconciliation migration requires the preceding run-control schema';
    END IF;
END
$$;

CREATE TABLE pm.workflow_command_intents (
    org_id text NOT NULL,
    workspace_id text NOT NULL,
    run_id text NOT NULL,
    command_id text NOT NULL,
    kind text NOT NULL CHECK (kind IN ('start_run', 'ensure_workflow_binding', 'repair_projection')),
    namespace text NOT NULL CHECK (namespace <> ''),
    task_queue text NOT NULL CHECK (task_queue = 'pm-control'),
    workflow_type text NOT NULL CHECK (workflow_type <> ''),
    workflow_id text NOT NULL CHECK (workflow_id <> ''),
    origin text NOT NULL CHECK (origin IN ('api', 'schedule', 'recovery')),
    generation bigint NOT NULL CHECK (generation > 0),
    execution_timeout_nanoseconds bigint NOT NULL CHECK (execution_timeout_nanoseconds > 0),
    budget_digest text NOT NULL CHECK (budget_digest ~ '^sha256:[0-9a-f]{64}$'),
    command_digest text NOT NULL CHECK (command_digest ~ '^sha256:[0-9a-f]{64}$'),
    sealed_at timestamptz NOT NULL,
    PRIMARY KEY (org_id, workspace_id, run_id, command_id),
    FOREIGN KEY (org_id, workspace_id, run_id, command_id)
        REFERENCES pm.outbox_commands (org_id, workspace_id, run_id, command_id) ON DELETE CASCADE
);

CREATE TABLE pm.workflow_reconciliation_witnesses (
    org_id text NOT NULL,
    workspace_id text NOT NULL,
    run_id text NOT NULL,
    command_id text NOT NULL,
    witness_id text NOT NULL,
    generation bigint NOT NULL CHECK (generation > 0),
    workflow_id text NOT NULL CHECK (workflow_id <> ''),
    temporal_run_id text,
    kind text NOT NULL CHECK (kind IN (
        'never_bound_no_dispatch', 'binding_mismatch',
        'prior_bound_not_found', 'closed_history'
    )),
    run_revision bigint NOT NULL CHECK (run_revision > 0),
    claim_fence bigint NOT NULL CHECK (claim_fence > 0),
    observed_at timestamptz NOT NULL,
    PRIMARY KEY (org_id, workspace_id, run_id, command_id, witness_id),
    FOREIGN KEY (org_id, workspace_id, run_id, command_id)
        REFERENCES pm.outbox_commands (org_id, workspace_id, run_id, command_id) ON DELETE CASCADE,
    CHECK (temporal_run_id IS NULL OR temporal_run_id <> '')
);

ALTER TABLE pm.workflow_command_intents ENABLE ROW LEVEL SECURITY;
ALTER TABLE pm.workflow_command_intents FORCE ROW LEVEL SECURITY;
CREATE POLICY pm_scope ON pm.workflow_command_intents
    USING (
        org_id = NULLIF(current_setting('pm.org_id', true), '')
        AND workspace_id = NULLIF(current_setting('pm.workspace_id', true), '')
    )
    WITH CHECK (
        org_id = NULLIF(current_setting('pm.org_id', true), '')
        AND workspace_id = NULLIF(current_setting('pm.workspace_id', true), '')
    );
CREATE POLICY pm_migrator_read ON pm.workflow_command_intents
    FOR SELECT TO pm_app_migrator USING (true);

ALTER TABLE pm.workflow_reconciliation_witnesses ENABLE ROW LEVEL SECURITY;
ALTER TABLE pm.workflow_reconciliation_witnesses FORCE ROW LEVEL SECURITY;
CREATE POLICY pm_scope ON pm.workflow_reconciliation_witnesses
    USING (
        org_id = NULLIF(current_setting('pm.org_id', true), '')
        AND workspace_id = NULLIF(current_setting('pm.workspace_id', true), '')
    )
    WITH CHECK (
        org_id = NULLIF(current_setting('pm.org_id', true), '')
        AND workspace_id = NULLIF(current_setting('pm.workspace_id', true), '')
    );
CREATE POLICY pm_migrator_read ON pm.workflow_reconciliation_witnesses
    FOR SELECT TO pm_app_migrator USING (true);

REVOKE ALL ON TABLE pm.workflow_command_intents FROM PUBLIC, pm_app_runtime;
REVOKE ALL ON TABLE pm.workflow_reconciliation_witnesses FROM PUBLIC, pm_app_runtime;
GRANT SELECT, INSERT ON TABLE pm.workflow_command_intents TO pm_app_runtime;
GRANT SELECT, INSERT ON TABLE pm.workflow_reconciliation_witnesses TO pm_app_runtime;

-- FORCE RLS also applies to the table owner. These policies are used only while
-- the migrator-owned definer function performs its bounded route claim.
CREATE POLICY pm_workflow_route_select ON pm.outbox_commands
    FOR SELECT TO pm_app_migrator
    USING (kind IN ('start_run', 'ensure_workflow_binding', 'repair_projection'));
CREATE POLICY pm_workflow_route_update ON pm.outbox_commands
    FOR UPDATE TO pm_app_migrator
    USING (kind IN ('start_run', 'ensure_workflow_binding', 'repair_projection'))
    WITH CHECK (kind IN ('start_run', 'ensure_workflow_binding', 'repair_projection'));
CREATE POLICY pm_workflow_route_actor ON pm.run_specs
    FOR SELECT TO pm_app_migrator
    USING (
        EXISTS (
            SELECT 1
            FROM pm.outbox_commands AS route_command
            WHERE route_command.org_id = run_specs.org_id
              AND route_command.workspace_id = run_specs.workspace_id
              AND route_command.run_id = run_specs.run_id
              AND route_command.kind IN ('start_run', 'ensure_workflow_binding', 'repair_projection')
        )
    );

CREATE FUNCTION pm.claim_workflow_outbox(
    route_owner_id text,
    route_limit integer,
    route_ttl interval
)
RETURNS TABLE (
    org_id text,
    workspace_id text,
    run_id text,
    command_id text,
    kind text,
    payload_ref text,
    owner_id text,
    fence bigint,
    claimed_at timestamptz,
    expires_at timestamptz,
    actor jsonb
)
LANGUAGE plpgsql
VOLATILE
SECURITY DEFINER
SET search_path = pg_catalog, pg_temp
AS $$
BEGIN
    IF route_owner_id IS NULL
       OR route_owner_id = ''
       OR octet_length(route_owner_id) > 512
       OR btrim(route_owner_id) <> route_owner_id
       OR route_owner_id ~ '[[:cntrl:]]' THEN
        RAISE EXCEPTION USING ERRCODE = '22023', MESSAGE = 'workflow route owner is invalid';
    END IF;
    IF route_limit IS NULL OR route_limit < 1 OR route_limit > 32 THEN
        RAISE EXCEPTION USING ERRCODE = '22023', MESSAGE = 'workflow route limit is invalid';
    END IF;
    IF route_ttl IS NULL OR route_ttl <= interval '0' OR route_ttl > interval '5 minutes' THEN
        RAISE EXCEPTION USING ERRCODE = '22023', MESSAGE = 'workflow route TTL is invalid';
    END IF;

    RETURN QUERY
    WITH claim_clock AS (
        SELECT clock_timestamp() AS value
    ), candidates AS (
        SELECT command.org_id, command.workspace_id, command.run_id, command.command_id
        FROM pm.outbox_commands AS command
        CROSS JOIN claim_clock
        WHERE command.kind IN ('start_run', 'ensure_workflow_binding', 'repair_projection')
          AND command.state = 'pending'
          AND command.acknowledged_at IS NULL
          AND command.available_at <= claim_clock.value
          AND command.claim_fence < 9223372036854775807
          AND (
              (command.claim_owner IS NULL AND command.claim_expires_at IS NULL)
              OR command.claim_expires_at <= claim_clock.value
          )
        ORDER BY command.available_at, command.created_at,
                 command.org_id, command.workspace_id, command.run_id, command.command_id
        FOR UPDATE OF command SKIP LOCKED
        LIMIT route_limit
    ), claimed AS (
        UPDATE pm.outbox_commands AS command
        SET claim_owner = route_owner_id,
            claim_fence = command.claim_fence + 1,
            claim_expires_at = claim_clock.value + route_ttl
        FROM candidates, claim_clock
        WHERE command.org_id = candidates.org_id
          AND command.workspace_id = candidates.workspace_id
          AND command.run_id = candidates.run_id
          AND command.command_id = candidates.command_id
        RETURNING command.org_id, command.workspace_id, command.run_id,
                  command.command_id, command.kind, command.payload_ref,
                  command.claim_owner, command.claim_fence, command.claim_expires_at
    )
    SELECT claimed.org_id, claimed.workspace_id, claimed.run_id,
           claimed.command_id, claimed.kind, claimed.payload_ref,
           claimed.claim_owner, claimed.claim_fence,
           claim_clock.value, claimed.claim_expires_at,
           accepted.spec->'actor'
    FROM claimed
    CROSS JOIN claim_clock
    JOIN pm.run_specs AS accepted
      ON accepted.org_id = claimed.org_id
     AND accepted.workspace_id = claimed.workspace_id
     AND accepted.run_id = claimed.run_id
    ORDER BY claimed.org_id, claimed.workspace_id, claimed.run_id, claimed.command_id;
END
$$;

REVOKE ALL ON FUNCTION pm.claim_workflow_outbox(text, integer, interval)
    FROM PUBLIC, pm_app_runtime;
GRANT EXECUTE ON FUNCTION pm.claim_workflow_outbox(text, integer, interval)
    TO pm_app_runtime;

CREATE FUNCTION pm.enforce_workflow_binding_progression()
RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    old_binding jsonb;
    new_binding jsonb;
    old_observations jsonb;
    new_observations jsonb;
    observation_index integer;
BEGIN
    old_binding := OLD.record->'workflow_binding';
    new_binding := NEW.record->'workflow_binding';
    IF old_binding IS NULL OR new_binding IS NULL
       OR (old_binding - 'observations') IS DISTINCT FROM (new_binding - 'observations') THEN
        RAISE EXCEPTION USING
            ERRCODE = '23514',
            MESSAGE = 'workflow binding core is immutable';
    END IF;

    old_observations := old_binding->'observations';
    new_observations := new_binding->'observations';
    IF jsonb_typeof(old_observations) IS DISTINCT FROM 'array'
       OR jsonb_typeof(new_observations) IS DISTINCT FROM 'array'
       OR jsonb_array_length(new_observations) < jsonb_array_length(old_observations) THEN
        RAISE EXCEPTION USING
            ERRCODE = '23514',
            MESSAGE = 'workflow observations are append-only';
    END IF;

    IF jsonb_array_length(old_observations) > 0 THEN
        FOR observation_index IN 0..jsonb_array_length(old_observations) - 1 LOOP
            IF old_observations->observation_index IS DISTINCT FROM new_observations->observation_index THEN
                RAISE EXCEPTION USING
                    ERRCODE = '23514',
                    MESSAGE = 'workflow observations are append-only';
            END IF;
        END LOOP;
    END IF;
    RETURN NEW;
END
$$;

REVOKE ALL ON FUNCTION pm.enforce_workflow_binding_progression()
    FROM PUBLIC, pm_app_runtime;
CREATE TRIGGER workflow_binding_progression
BEFORE UPDATE OF record ON pm.runs
FOR EACH ROW
EXECUTE FUNCTION pm.enforce_workflow_binding_progression();

ALTER TABLE pm.control_schema DROP CONSTRAINT control_schema_revision_check;
UPDATE pm.control_schema
SET revision = 'pm.control/v9', installed_at = clock_timestamp()
WHERE singleton AND revision = 'pm.control/v8';
ALTER TABLE pm.control_schema
    ADD CONSTRAINT control_schema_revision_check CHECK (revision = 'pm.control/v9');
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pm.workflow_command_intents)
       OR EXISTS (SELECT 1 FROM pm.workflow_reconciliation_witnesses)
       OR EXISTS (
           SELECT 1 FROM pm.outbox_commands
           WHERE kind IN ('ensure_workflow_binding', 'repair_projection')
       ) THEN
        RAISE EXCEPTION 'cannot roll back workflow reconciliation schema while durable workflow records exist';
    END IF;
END
$$;

DROP TRIGGER workflow_binding_progression ON pm.runs;
DROP FUNCTION pm.enforce_workflow_binding_progression();
REVOKE ALL ON FUNCTION pm.claim_workflow_outbox(text, integer, interval)
    FROM PUBLIC, pm_app_runtime;
DROP FUNCTION pm.claim_workflow_outbox(text, integer, interval);
DROP POLICY pm_workflow_route_actor ON pm.run_specs;
DROP POLICY pm_workflow_route_update ON pm.outbox_commands;
DROP POLICY pm_workflow_route_select ON pm.outbox_commands;
DROP TABLE pm.workflow_reconciliation_witnesses;
DROP TABLE pm.workflow_command_intents;

ALTER TABLE pm.control_schema DROP CONSTRAINT control_schema_revision_check;
UPDATE pm.control_schema
SET revision = 'pm.control/v8', installed_at = clock_timestamp()
WHERE singleton AND revision = 'pm.control/v9';
ALTER TABLE pm.control_schema
    ADD CONSTRAINT control_schema_revision_check CHECK (revision = 'pm.control/v8');
-- +goose StatementEnd
