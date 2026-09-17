-- Durable pending-admission authority shared by all API and worker replicas.
-- Goose owns the transaction; each section is one SQL batch.

-- +goose Up
-- +goose StatementBegin
DO $$
BEGIN
    IF current_database() <> 'pm_app' OR current_user <> 'pm_app_migrator' THEN
        RAISE EXCEPTION 'run execution schema requires pm_app_migrator on pm_app';
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
        WHERE singleton AND revision = 'pm.control/v4'
    ) THEN
        RAISE EXCEPTION 'run execution migration requires the preceding credential schema';
    END IF;
END
$$;

-- This aggregate contains no tenant identifiers or run payloads. Reservation
-- rows remain canonical and retain their existing forced workspace policies.
CREATE TABLE pm.pending_admission_state (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    pending_count bigint NOT NULL CHECK (pending_count >= 0)
);

-- Preserve pre-existing outstanding reservations without inventing a new
-- accepted identity, resetting its age, or retaining a cross-tenant read policy.
CREATE POLICY pm_pending_migration_read ON pm.pending_reservations
    FOR SELECT TO pm_app_migrator USING (true);
INSERT INTO pm.pending_admission_state (singleton, pending_count)
SELECT true, count(*) FROM pm.pending_reservations WHERE released_at IS NULL;
DROP POLICY pm_pending_migration_read ON pm.pending_reservations;

ALTER TABLE pm.pending_admission_state ENABLE ROW LEVEL SECURITY;
ALTER TABLE pm.pending_admission_state FORCE ROW LEVEL SECURITY;
CREATE POLICY pm_scoped_pending_authority ON pm.pending_admission_state
    USING (
        NULLIF(current_setting('pm.org_id', true), '') IS NOT NULL
        AND NULLIF(current_setting('pm.workspace_id', true), '') IS NOT NULL
    )
    WITH CHECK (
        NULLIF(current_setting('pm.org_id', true), '') IS NOT NULL
        AND NULLIF(current_setting('pm.workspace_id', true), '') IS NOT NULL
    );
REVOKE ALL ON TABLE pm.pending_admission_state FROM PUBLIC, pm_app_runtime;
GRANT SELECT ON TABLE pm.pending_admission_state TO pm_app_runtime;
GRANT UPDATE (pending_count) ON pm.pending_admission_state TO pm_app_runtime;

CREATE INDEX pending_reservations_outstanding_workspace_idx
    ON pm.pending_reservations (org_id, workspace_id, accepted_at)
    WHERE released_at IS NULL;

CREATE FUNCTION pm.maintain_pending_admission_count() RETURNS trigger
    LANGUAGE plpgsql SECURITY INVOKER
    SET search_path = pg_catalog
AS $$
DECLARE
    delta integer := 0;
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF NEW.released_at IS NULL THEN
            delta := 1;
        END IF;
    ELSIF TG_OP = 'DELETE' THEN
        IF OLD.released_at IS NULL THEN
            delta := -1;
        END IF;
    ELSE
        IF OLD.released_at IS NOT NULL
           AND NEW.released_at IS DISTINCT FROM OLD.released_at THEN
            RAISE EXCEPTION 'a released pending reservation cannot be reopened or retimed'
                USING ERRCODE = '23514';
        END IF;
        IF OLD.released_at IS NULL AND NEW.released_at IS NOT NULL THEN
            delta := -1;
        END IF;
    END IF;
    IF delta <> 0 THEN
        UPDATE pm.pending_admission_state
        SET pending_count = pending_count + delta
        WHERE singleton;
        IF NOT FOUND THEN
            RAISE EXCEPTION 'scoped pending-admission authority is unavailable'
                USING ERRCODE = '42501';
        END IF;
    END IF;
    IF TG_OP = 'DELETE' THEN
        RETURN OLD;
    END IF;
    RETURN NEW;
END
$$;
REVOKE ALL ON FUNCTION pm.maintain_pending_admission_count() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION pm.maintain_pending_admission_count() TO pm_app_runtime;
CREATE TRIGGER pending_admission_count
    AFTER INSERT OR UPDATE OR DELETE ON pm.pending_reservations
    FOR EACH ROW EXECUTE FUNCTION pm.maintain_pending_admission_count();

CREATE TABLE pm.run_execution_grants (
    org_id text NOT NULL,
    workspace_id text NOT NULL,
    run_id text NOT NULL,
    grant_ref text NOT NULL CHECK (grant_ref <> ''),
    grant_epoch bigint NOT NULL CHECK (grant_epoch > 0),
    grant_record jsonb NOT NULL CHECK (jsonb_typeof(grant_record) = 'object'),
    revoked_at timestamptz,
    PRIMARY KEY (org_id, workspace_id, run_id),
    UNIQUE (org_id, workspace_id, grant_ref),
    FOREIGN KEY (org_id, workspace_id, run_id)
        REFERENCES pm.run_specs (org_id, workspace_id, run_id) ON DELETE CASCADE,
    CHECK ((grant_record->>'grant_ref') IS NOT DISTINCT FROM grant_ref),
    CHECK ((grant_record->>'grant_epoch')::numeric IS NOT DISTINCT FROM grant_epoch::numeric),
    CHECK ((grant_record->'scope'->>'org_id') IS NOT DISTINCT FROM org_id),
    CHECK ((grant_record->'scope'->>'workspace_id') IS NOT DISTINCT FROM workspace_id)
);
ALTER TABLE pm.run_execution_grants ENABLE ROW LEVEL SECURITY;
ALTER TABLE pm.run_execution_grants FORCE ROW LEVEL SECURITY;
CREATE POLICY pm_scope ON pm.run_execution_grants
    USING (
        org_id = NULLIF(current_setting('pm.org_id', true), '')
        AND workspace_id = NULLIF(current_setting('pm.workspace_id', true), '')
    )
    WITH CHECK (
        org_id = NULLIF(current_setting('pm.org_id', true), '')
        AND workspace_id = NULLIF(current_setting('pm.workspace_id', true), '')
    );
REVOKE ALL ON TABLE pm.run_execution_grants FROM PUBLIC, pm_app_runtime;
GRANT SELECT ON TABLE pm.run_execution_grants TO pm_app_runtime;
GRANT INSERT (org_id, workspace_id, run_id, grant_ref, grant_epoch, grant_record)
    ON pm.run_execution_grants TO pm_app_runtime;
GRANT UPDATE (revoked_at) ON pm.run_execution_grants TO pm_app_runtime;

CREATE FUNCTION pm.preserve_run_grant_revocation() RETURNS trigger
    LANGUAGE plpgsql SECURITY INVOKER
    SET search_path = pg_catalog
AS $$
BEGIN
    IF OLD.revoked_at IS NOT NULL AND NEW.revoked_at IS DISTINCT FROM OLD.revoked_at THEN
        RAISE EXCEPTION 'execution grant revocation cannot be cleared or retimed'
            USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END
$$;
REVOKE ALL ON FUNCTION pm.preserve_run_grant_revocation() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION pm.preserve_run_grant_revocation() TO pm_app_runtime;
CREATE TRIGGER run_grant_revocation_is_final
    BEFORE UPDATE ON pm.run_execution_grants
    FOR EACH ROW EXECUTE FUNCTION pm.preserve_run_grant_revocation();

-- The existing key row remains the sole admission replay/refusal authority.
-- Legacy safe responses are recoverable from retained columns; no historical
-- resolution time is inferred or fabricated for them.
ALTER TABLE pm.admission_keys
    ADD COLUMN safe_response jsonb,
    ADD COLUMN resolved_at timestamptz,
    ADD COLUMN response_compacted_at timestamptz;
CREATE POLICY pm_admission_receipt_migration ON pm.admission_keys
    FOR ALL TO pm_app_migrator USING (true) WITH CHECK (true);
UPDATE pm.admission_keys
SET safe_response = jsonb_build_object(
    'run_id', run_id, 'intent_digest', intent_digest, 'accepted_at', accepted_at
);
DROP POLICY pm_admission_receipt_migration ON pm.admission_keys;
ALTER TABLE pm.admission_keys
    ADD CONSTRAINT admission_response_lifecycle CHECK (
        (safe_response IS NOT NULL AND jsonb_typeof(safe_response) = 'object'
            AND response_compacted_at IS NULL)
        OR (safe_response IS NULL AND resolved_at IS NOT NULL
            AND response_compacted_at IS NOT NULL
            AND response_compacted_at >= resolved_at + interval '168 hours')
    ),
    ADD CONSTRAINT admission_response_binding CHECK (
        safe_response IS NULL OR (
            (safe_response->>'run_id') IS NOT DISTINCT FROM run_id
            AND (safe_response->>'intent_digest') IS NOT DISTINCT FROM intent_digest
            AND (safe_response->>'accepted_at')::timestamptz IS NOT DISTINCT FROM accepted_at
        )
    );
REVOKE INSERT ON pm.admission_keys FROM pm_app_runtime;
GRANT INSERT (
    org_id, workspace_id, principal_id, operation_id, target_id, idempotency_key,
    intent_digest, run_id, accepted_at, safe_response
) ON pm.admission_keys TO pm_app_runtime;
GRANT UPDATE (safe_response, resolved_at, response_compacted_at)
    ON pm.admission_keys TO pm_app_runtime;

CREATE FUNCTION pm.preserve_admission_receipt() RETURNS trigger
    LANGUAGE plpgsql SECURITY INVOKER
    SET search_path = pg_catalog
AS $$
BEGIN
    -- The non-inheritable owner retains explicit migration/fixture authority;
    -- runtime cannot assume this role or select arbitrary retention timestamps.
    IF current_user = 'pm_app_migrator' THEN
        RETURN NEW;
    END IF;
    IF OLD.resolved_at IS NOT NULL AND NEW.resolved_at IS DISTINCT FROM OLD.resolved_at THEN
        RAISE EXCEPTION 'admission resolution cannot be cleared or retimed'
            USING ERRCODE = '23514';
    END IF;
    IF OLD.response_compacted_at IS NOT NULL
       AND NEW.response_compacted_at IS DISTINCT FROM OLD.response_compacted_at THEN
        RAISE EXCEPTION 'admission response compaction is final'
            USING ERRCODE = '23514';
    END IF;
    IF NEW.safe_response IS NOT NULL AND NEW.safe_response IS DISTINCT FROM OLD.safe_response THEN
        RAISE EXCEPTION 'admission response cannot be rewritten or regenerated'
            USING ERRCODE = '23514';
    END IF;
    IF OLD.resolved_at IS NULL AND NEW.resolved_at IS NOT NULL THEN
        IF NOT EXISTS (
            SELECT 1 FROM pm.runs AS run
            WHERE run.org_id = NEW.org_id AND run.workspace_id = NEW.workspace_id
              AND run.run_id = NEW.run_id
              AND run.state IN ('succeeded', 'partial', 'failed', 'cancelled')
              AND (run.record->>'unknown_effect_count')::numeric = 0
        ) THEN
            RAISE EXCEPTION 'unresolved run cannot start admission response expiry'
                USING ERRCODE = '23514';
        END IF;
        NEW.resolved_at := clock_timestamp();
    END IF;
    IF OLD.safe_response IS NOT NULL AND NEW.safe_response IS NULL THEN
        IF OLD.resolved_at IS NULL
           OR OLD.resolved_at > clock_timestamp() - interval '168 hours' THEN
            RAISE EXCEPTION 'admission response retention has not elapsed'
                USING ERRCODE = '23514';
        END IF;
        NEW.response_compacted_at := clock_timestamp();
    END IF;
    RETURN NEW;
END
$$;
REVOKE ALL ON FUNCTION pm.preserve_admission_receipt() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION pm.preserve_admission_receipt() TO pm_app_runtime;
CREATE TRIGGER admission_receipt_is_retained
    BEFORE UPDATE ON pm.admission_keys
    FOR EACH ROW EXECUTE FUNCTION pm.preserve_admission_receipt();

ALTER TABLE pm.control_schema DROP CONSTRAINT control_schema_revision_check;
UPDATE pm.control_schema
SET revision = 'pm.control/v5', installed_at = clock_timestamp()
WHERE singleton AND revision = 'pm.control/v4';
ALTER TABLE pm.control_schema
    ADD CONSTRAINT control_schema_revision_check CHECK (revision = 'pm.control/v5');
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
CREATE POLICY pm_admission_receipt_downgrade ON pm.admission_keys
    FOR SELECT TO pm_app_migrator USING (true);
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pm.admission_keys WHERE response_compacted_at IS NOT NULL) THEN
        RAISE EXCEPTION 'cannot downgrade retained admission refusal guards';
    END IF;
END
$$;
DROP POLICY pm_admission_receipt_downgrade ON pm.admission_keys;
DROP TRIGGER admission_receipt_is_retained ON pm.admission_keys;
DROP FUNCTION pm.preserve_admission_receipt();
REVOKE UPDATE (safe_response, resolved_at, response_compacted_at)
    ON pm.admission_keys FROM pm_app_runtime;
ALTER TABLE pm.admission_keys
    DROP CONSTRAINT admission_response_lifecycle,
    DROP CONSTRAINT admission_response_binding,
    DROP COLUMN safe_response,
    DROP COLUMN resolved_at,
    DROP COLUMN response_compacted_at;
GRANT INSERT ON pm.admission_keys TO pm_app_runtime;
REVOKE ALL ON TABLE pm.run_execution_grants FROM PUBLIC, pm_app_runtime;
DROP TABLE pm.run_execution_grants;
DROP FUNCTION pm.preserve_run_grant_revocation();
DROP TRIGGER pending_admission_count ON pm.pending_reservations;
DROP FUNCTION pm.maintain_pending_admission_count();
DROP INDEX pm.pending_reservations_outstanding_workspace_idx;
REVOKE ALL ON TABLE pm.pending_admission_state FROM PUBLIC, pm_app_runtime;
DROP TABLE pm.pending_admission_state;

ALTER TABLE pm.control_schema DROP CONSTRAINT control_schema_revision_check;
UPDATE pm.control_schema
SET revision = 'pm.control/v4', installed_at = clock_timestamp()
WHERE singleton AND revision = 'pm.control/v5';
ALTER TABLE pm.control_schema
    ADD CONSTRAINT control_schema_revision_check CHECK (revision = 'pm.control/v4');
-- +goose StatementEnd
