-- Immutable worker retry lineage keeps one physical attempt per dispatch while
-- preserving the original logical effect, settlement and attempt witnesses.

-- +goose Up
-- +goose StatementBegin
DO $$
BEGIN
    IF current_database() <> 'pm_app' OR current_user <> 'pm_app_migrator' THEN
        RAISE EXCEPTION 'worker retry schema requires pm_app_migrator on pm_app';
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
        SELECT 1 FROM pm.control_schema WHERE singleton AND revision = 'pm.control/v17'
    ) THEN
        RAISE EXCEPTION 'worker retry migration requires the durable continuation schema';
    END IF;
END
$$;

ALTER TABLE pm.dispatches ADD COLUMN attempt_number bigint;
CREATE POLICY pm_worker_retry_migration_select ON pm.dispatches
    FOR SELECT TO pm_app_migrator USING (true);
CREATE POLICY pm_worker_retry_migration_update ON pm.dispatches
    FOR UPDATE TO pm_app_migrator USING (true) WITH CHECK (true);
UPDATE pm.dispatches SET attempt_number = 1;
DROP POLICY pm_worker_retry_migration_update ON pm.dispatches;
DROP POLICY pm_worker_retry_migration_select ON pm.dispatches;
ALTER TABLE pm.dispatches
    ALTER COLUMN attempt_number SET NOT NULL,
    ADD CONSTRAINT dispatches_attempt_number_valid CHECK (
        attempt_number BETWEEN 1 AND 4294967295
    ),
    ADD CONSTRAINT dispatches_effect_attempt_number_key UNIQUE (
        org_id, workspace_id, run_id, task_id, effect_digest, attempt_number
    );

-- A dispatch is one physical attempt. A replay always receives a new dispatch
-- identity instead of appending another physical attempt to an old dispatch.
ALTER TABLE pm.dispatch_attempts
    ADD CONSTRAINT dispatch_attempts_one_physical_attempt UNIQUE (
        org_id, workspace_id, run_id, task_id, dispatch_id
    );

CREATE TABLE pm.worker_retries (
    org_id text NOT NULL,
    workspace_id text NOT NULL,
    run_id text NOT NULL,
    task_id text NOT NULL,
    child_dispatch_id text NOT NULL,
    parent_dispatch_id text NOT NULL,
    parent_attempt_id text NOT NULL,
    parent_attempt_receipt_id text NOT NULL,
    parent_attempt jsonb NOT NULL CHECK (
        jsonb_typeof(parent_attempt) = 'object'
        AND parent_attempt ?& ARRAY[
            'attempt_id', 'dispatch_id', 'receipt_ref', 'recorded_at'
        ]::text[]
        AND parent_attempt - ARRAY[
            'attempt_id', 'dispatch_id', 'receipt_ref', 'recorded_at'
        ]::text[] = '{}'::jsonb
        AND parent_attempt->>'attempt_id' = parent_attempt_id
        AND parent_attempt->>'dispatch_id' = parent_dispatch_id
        AND parent_attempt->>'receipt_ref' = parent_attempt_receipt_id
    ),
    settlement_receipt_id text NOT NULL,
    basis text NOT NULL CHECK (
        basis IN ('definitive_unsent', 'source_declared_safe')
    ),
    not_before timestamptz NOT NULL CHECK (
        isfinite(not_before)
        AND not_before >= timestamptz '0001-01-01 00:00:00+00'
    ),
    input_digest text NOT NULL CHECK (
        input_digest ~ '^sha256:[0-9a-f]{64}$'
    ),
    PRIMARY KEY (org_id, workspace_id, run_id, task_id, child_dispatch_id),
    UNIQUE (org_id, workspace_id, run_id, task_id, parent_dispatch_id),
    CHECK (child_dispatch_id <> parent_dispatch_id),
    FOREIGN KEY (org_id, workspace_id, run_id, task_id, child_dispatch_id)
        REFERENCES pm.dispatches (
            org_id, workspace_id, run_id, task_id, dispatch_id
        ) ON DELETE CASCADE,
    FOREIGN KEY (org_id, workspace_id, run_id, task_id, parent_dispatch_id)
        REFERENCES pm.dispatches (
            org_id, workspace_id, run_id, task_id, dispatch_id
        ) ON DELETE CASCADE,
    FOREIGN KEY (
        org_id, workspace_id, run_id, task_id,
        parent_dispatch_id, parent_attempt_id
    ) REFERENCES pm.dispatch_attempts (
        org_id, workspace_id, run_id, task_id, dispatch_id, attempt_id
    ),
    FOREIGN KEY (org_id, workspace_id, run_id, parent_attempt_receipt_id)
        REFERENCES pm.receipts (org_id, workspace_id, run_id, receipt_id),
    FOREIGN KEY (org_id, workspace_id, run_id, settlement_receipt_id)
        REFERENCES pm.receipts (org_id, workspace_id, run_id, receipt_id)
);

-- Native referential integrity remains effective after the transaction clears
-- its RLS scope. An invoker trigger deferred until COMMIT cannot query the
-- scoped retry row at that point.
ALTER TABLE pm.dispatches
    ADD COLUMN retry_link_dispatch_id text GENERATED ALWAYS AS (
        CASE WHEN attempt_number > 1 THEN dispatch_id END
    ) STORED,
    ADD CONSTRAINT dispatches_retry_membership_fk FOREIGN KEY (
        org_id, workspace_id, run_id, task_id, retry_link_dispatch_id
    ) REFERENCES pm.worker_retries (
        org_id, workspace_id, run_id, task_id, child_dispatch_id
    ) DEFERRABLE INITIALLY DEFERRED;

CREATE FUNCTION pm.validate_worker_retry_insert() RETURNS trigger
    LANGUAGE plpgsql SECURITY INVOKER
    SET search_path = pg_catalog, pm AS $$
DECLARE
    parent_number bigint;
    child_number bigint;
    parent_effect text;
    child_effect text;
    parent_intent jsonb;
    child_intent jsonb;
    parent_state text;
    child_state text;
    stored_attempt jsonb;
    settlement_kind text;
BEGIN
    SELECT attempt_number, effect_digest, intent, state
      INTO parent_number, parent_effect, parent_intent, parent_state
      FROM pm.dispatches
     WHERE org_id = NEW.org_id
       AND workspace_id = NEW.workspace_id
       AND run_id = NEW.run_id
       AND task_id = NEW.task_id
       AND dispatch_id = NEW.parent_dispatch_id
     FOR KEY SHARE;
    SELECT attempt_number, effect_digest, intent, state
      INTO child_number, child_effect, child_intent, child_state
      FROM pm.dispatches
     WHERE org_id = NEW.org_id
       AND workspace_id = NEW.workspace_id
       AND run_id = NEW.run_id
       AND task_id = NEW.task_id
       AND dispatch_id = NEW.child_dispatch_id
     FOR KEY SHARE;
    IF parent_number IS NULL OR child_number IS NULL
       OR parent_number >= 4294967295
       OR child_number <> parent_number + 1
       OR parent_effect IS DISTINCT FROM child_effect
       OR parent_state <> 'possibly_dispatched'
       OR child_state <> 'prepared'
       OR parent_intent->>'dispatch_id' IS DISTINCT FROM NEW.parent_dispatch_id
       OR child_intent->>'dispatch_id' IS DISTINCT FROM NEW.child_dispatch_id
       OR parent_intent->>'attempt_id' IS DISTINCT FROM NEW.parent_attempt_id
       OR child_intent->>'attempt_id' IS NOT DISTINCT FROM NEW.parent_attempt_id
       OR child_intent - ARRAY['dispatch_id', 'attempt_id']::text[]
          IS DISTINCT FROM
          parent_intent - ARRAY['dispatch_id', 'attempt_id']::text[] THEN
        RAISE EXCEPTION 'worker retry dispatch lineage is invalid';
    END IF;

    IF EXISTS (
        SELECT 1
          FROM pm.dispatch_attempts
         WHERE org_id = NEW.org_id
           AND workspace_id = NEW.workspace_id
           AND run_id = NEW.run_id
           AND task_id = NEW.task_id
           AND dispatch_id = NEW.child_dispatch_id
    ) THEN
        RAISE EXCEPTION 'worker retry child already has a physical attempt';
    END IF;

    SELECT attempt
      INTO stored_attempt
      FROM pm.dispatch_attempts
     WHERE org_id = NEW.org_id
       AND workspace_id = NEW.workspace_id
       AND run_id = NEW.run_id
       AND task_id = NEW.task_id
       AND dispatch_id = NEW.parent_dispatch_id
       AND attempt_id = NEW.parent_attempt_id;
    IF stored_attempt IS DISTINCT FROM NEW.parent_attempt THEN
        RAISE EXCEPTION 'worker retry parent attempt witness is invalid';
    END IF;

    SELECT kind
      INTO settlement_kind
      FROM pm.receipts
     WHERE org_id = NEW.org_id
       AND workspace_id = NEW.workspace_id
       AND run_id = NEW.run_id
       AND receipt_id = NEW.settlement_receipt_id;
    IF settlement_kind IS DISTINCT FROM 'worker_settlement' THEN
        RAISE EXCEPTION 'worker retry settlement receipt is invalid';
    END IF;

    IF NOT EXISTS (
        SELECT 1
          FROM pm.dispatch_members AS member
         WHERE member.org_id = NEW.org_id
           AND member.workspace_id = NEW.workspace_id
           AND member.run_id = NEW.run_id
           AND member.task_id = NEW.task_id
           AND member.dispatch_id = NEW.parent_dispatch_id
    ) OR EXISTS (
        SELECT 1
          FROM pm.dispatch_members AS member
          LEFT JOIN pm.member_outcomes AS outcome
            ON outcome.org_id = member.org_id
           AND outcome.workspace_id = member.workspace_id
           AND outcome.run_id = member.run_id
           AND outcome.task_id = member.task_id
           AND outcome.dispatch_id = member.dispatch_id
           AND outcome.member_id = member.member_id
         WHERE member.org_id = NEW.org_id
           AND member.workspace_id = NEW.workspace_id
           AND member.run_id = NEW.run_id
           AND member.task_id = NEW.task_id
           AND member.dispatch_id = NEW.parent_dispatch_id
           AND outcome.member_id IS NULL
    ) THEN
        RAISE EXCEPTION 'worker retry parent settlement is incomplete';
    END IF;

    IF EXISTS (
        SELECT 1
          FROM pm.member_outcomes AS outcome
         WHERE outcome.org_id = NEW.org_id
           AND outcome.workspace_id = NEW.workspace_id
           AND outcome.run_id = NEW.run_id
           AND outcome.task_id = NEW.task_id
           AND outcome.dispatch_id = NEW.parent_dispatch_id
           AND (
               (NEW.basis = 'definitive_unsent'
                AND outcome.state IS DISTINCT FROM 'unsent')
               OR
               (NEW.basis = 'source_declared_safe'
                AND outcome.state NOT IN ('rejected', 'unknown'))
               OR
               (outcome.state IN ('unsent', 'unknown')
                AND NOT COALESCE(
                    outcome.outcome->'assertion_refs'
                        @> jsonb_build_array(NEW.parent_attempt_receipt_id),
                    false
                ))
           )
    ) THEN
        RAISE EXCEPTION 'worker retry basis does not match exact parent outcomes';
    END IF;
    RETURN NEW;
END
$$;

CREATE TRIGGER worker_retry_insert_is_sealed
    BEFORE INSERT ON pm.worker_retries
    FOR EACH ROW EXECUTE FUNCTION pm.validate_worker_retry_insert();

CREATE FUNCTION pm.preserve_worker_retry() RETURNS trigger
    LANGUAGE plpgsql SECURITY INVOKER
    SET search_path = pg_catalog AS $$
BEGIN
    IF NEW IS DISTINCT FROM OLD THEN
        RAISE EXCEPTION 'worker retry lineage is immutable';
    END IF;
    RETURN NEW;
END
$$;

CREATE TRIGGER worker_retry_is_immutable
    BEFORE UPDATE ON pm.worker_retries
    FOR EACH ROW EXECUTE FUNCTION pm.preserve_worker_retry();

REVOKE ALL ON FUNCTION pm.validate_worker_retry_insert() FROM PUBLIC, pm_app_runtime;
REVOKE ALL ON FUNCTION pm.preserve_worker_retry() FROM PUBLIC, pm_app_runtime;
ALTER TABLE pm.worker_retries ENABLE ROW LEVEL SECURITY;
ALTER TABLE pm.worker_retries FORCE ROW LEVEL SECURITY;
CREATE POLICY pm_scope ON pm.worker_retries
    USING (
        org_id = NULLIF(current_setting('pm.org_id', true), '')
        AND workspace_id = NULLIF(current_setting('pm.workspace_id', true), '')
    )
    WITH CHECK (
        org_id = NULLIF(current_setting('pm.org_id', true), '')
        AND workspace_id = NULLIF(current_setting('pm.workspace_id', true), '')
    );
REVOKE ALL ON TABLE pm.worker_retries FROM PUBLIC, pm_app_runtime;
GRANT SELECT ON TABLE pm.worker_retries TO pm_app_runtime;
GRANT INSERT (
    org_id, workspace_id, run_id, task_id, child_dispatch_id,
    parent_dispatch_id, parent_attempt_id, parent_attempt_receipt_id,
    parent_attempt, settlement_receipt_id, basis, not_before, input_digest
) ON pm.worker_retries TO pm_app_runtime;

ALTER TABLE pm.control_schema DROP CONSTRAINT control_schema_revision_check;
UPDATE pm.control_schema SET revision = 'pm.control/v18', installed_at = clock_timestamp()
WHERE singleton AND revision = 'pm.control/v17';
ALTER TABLE pm.control_schema
    ADD CONSTRAINT control_schema_revision_check CHECK (revision = 'pm.control/v18');
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
CREATE POLICY pm_worker_retry_migration_select ON pm.worker_retries
    FOR SELECT TO pm_app_migrator USING (true);
CREATE POLICY pm_worker_retry_migration_select ON pm.dispatches
    FOR SELECT TO pm_app_migrator USING (true);
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pm.worker_retries)
       OR EXISTS (SELECT 1 FROM pm.dispatches WHERE attempt_number <> 1) THEN
        RAISE EXCEPTION 'cannot roll back retained worker retry lineage';
    END IF;
END
$$;
DROP POLICY pm_worker_retry_migration_select ON pm.worker_retries;
DROP POLICY pm_worker_retry_migration_select ON pm.dispatches;

REVOKE ALL ON TABLE pm.worker_retries FROM PUBLIC, pm_app_runtime;
ALTER TABLE pm.dispatches DROP CONSTRAINT dispatches_retry_membership_fk;
DROP TRIGGER worker_retry_is_immutable ON pm.worker_retries;
DROP TRIGGER worker_retry_insert_is_sealed ON pm.worker_retries;
DROP FUNCTION pm.validate_worker_retry_insert();
DROP FUNCTION pm.preserve_worker_retry();
DROP TABLE pm.worker_retries;
ALTER TABLE pm.dispatch_attempts
    DROP CONSTRAINT dispatch_attempts_one_physical_attempt;
ALTER TABLE pm.dispatches
    DROP CONSTRAINT dispatches_effect_attempt_number_key,
    DROP CONSTRAINT dispatches_attempt_number_valid,
    DROP COLUMN retry_link_dispatch_id,
    DROP COLUMN attempt_number;

ALTER TABLE pm.control_schema DROP CONSTRAINT control_schema_revision_check;
UPDATE pm.control_schema SET revision = 'pm.control/v17', installed_at = clock_timestamp()
WHERE singleton AND revision = 'pm.control/v18';
ALTER TABLE pm.control_schema
    ADD CONSTRAINT control_schema_revision_check CHECK (revision = 'pm.control/v17');
-- +goose StatementEnd
