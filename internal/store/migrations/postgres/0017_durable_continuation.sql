-- Artifact publication purpose is sealed before object I/O, accepted run
-- controls retain bounded cap and continuation authority, and verified local
-- completion can release unused provider capacity without claiming dispatch.

-- +goose Up
-- +goose StatementBegin
DO $$
BEGIN
    IF current_database() <> 'pm_app' OR current_user <> 'pm_app_migrator' THEN
        RAISE EXCEPTION 'durable continuation schema requires pm_app_migrator on pm_app';
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
        SELECT 1 FROM pm.control_schema WHERE singleton AND revision = 'pm.control/v16'
    ) THEN
        RAISE EXCEPTION 'durable continuation migration requires the artifact purpose schema';
    END IF;
END
$$;

-- Migration 16 needed a default to backfill the artifact registry. New writes
-- must always declare their purpose instead of inheriting that migration aid.
ALTER TABLE pm.artifacts ALTER COLUMN purpose DROP DEFAULT;

-- A verified local-tail completion did not dispatch physical work. Preserve
-- that fact in a distinct terminal state while releasing its reserved capacity.
ALTER TABLE pm.account_permits
    DROP CONSTRAINT account_permits_state_check,
    DROP CONSTRAINT account_permits_check1,
    ADD CONSTRAINT account_permits_state_check CHECK (
        state IN ('reserved', 'dispatched', 'unknown', 'resolved', 'expired', 'released')
    ),
    ADD CONSTRAINT account_permits_check1 CHECK (
        (state IN ('reserved', 'dispatched', 'unknown') AND resolved_at IS NULL)
        OR
        (state IN ('resolved', 'expired', 'released')
            AND resolved_at IS NOT NULL AND resolved_at >= reserved_at)
    );

-- Every pre-v17 reservation could publish only through the public path. Preserve
-- that meaning explicitly, then leave no default for new reservations.
ALTER TABLE pm.object_reservations
    ADD COLUMN purpose text NOT NULL DEFAULT 'public';
ALTER TABLE pm.object_reservations
    ALTER COLUMN purpose DROP DEFAULT,
    ADD CONSTRAINT object_reservations_purpose_valid
        CHECK (purpose IN ('public', 'continuation'));

CREATE OR REPLACE FUNCTION pm.preserve_object_reservation() RETURNS trigger
    LANGUAGE plpgsql SECURITY INVOKER
    SET search_path = pg_catalog, pm AS $$
BEGIN
    IF ROW(NEW.org_id, NEW.workspace_id, NEW.reservation_id, NEW.connection_id,
           NEW.artifact_id, NEW.purpose, NEW.maximum_bytes, NEW.reserved_at)
       IS DISTINCT FROM
       ROW(OLD.org_id, OLD.workspace_id, OLD.reservation_id, OLD.connection_id,
           OLD.artifact_id, OLD.purpose, OLD.maximum_bytes, OLD.reserved_at) THEN
        RAISE EXCEPTION 'object reservation identity is immutable';
    END IF;
    IF OLD.state = 'held' AND NEW.state IN ('bound', 'released') THEN
        RETURN NEW;
    END IF;
    IF OLD.state = 'bound' AND NEW.state = 'published'
       AND ROW(NEW.object_attempt_key, NEW.sha256, NEW.byte_length, NEW.media_type, NEW.bound_at)
           IS NOT DISTINCT FROM
           ROW(OLD.object_attempt_key, OLD.sha256, OLD.byte_length, OLD.media_type, OLD.bound_at) THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'object reservation transition is invalid';
END
$$;

ALTER TABLE pm.artifacts
    ADD CONSTRAINT artifacts_publication_purpose_key
        UNIQUE (org_id, workspace_id, artifact_id, object_attempt_key, purpose);
ALTER TABLE pm.object_reservations
    ADD CONSTRAINT object_reservations_publication_purpose_fkey
        FOREIGN KEY (org_id, workspace_id, published_artifact_id, object_attempt_key, purpose)
        REFERENCES pm.artifacts (
            org_id, workspace_id, artifact_id, object_attempt_key, purpose
        );
GRANT INSERT (purpose) ON pm.object_reservations TO pm_app_runtime;

ALTER TABLE pm.run_input_snapshots
    DROP CONSTRAINT run_input_snapshot_result_controls;
ALTER TABLE pm.run_input_snapshots
    ADD CONSTRAINT run_input_snapshot_result_controls CHECK (
        jsonb_typeof(snapshot->'result_controls') = 'object'
        AND ((snapshot->'result_controls') - ARRAY[
            'selection', 'filter', 'total_record_cap', 'continuation'
        ]::text[]) = '{}'::jsonb
        AND CASE
            WHEN NOT ((snapshot->'result_controls') ? 'total_record_cap') THEN true
            WHEN jsonb_typeof(snapshot->'result_controls'->'total_record_cap') = 'number' THEN
                (snapshot->'result_controls'->>'total_record_cap')::numeric
                    BETWEEN 1 AND 18446744073709551615
                AND trunc((snapshot->'result_controls'->>'total_record_cap')::numeric)
                    = (snapshot->'result_controls'->>'total_record_cap')::numeric
            ELSE false
        END
        AND CASE
            WHEN NOT ((snapshot->'result_controls') ? 'continuation') THEN true
            WHEN jsonb_typeof(snapshot->'result_controls'->'continuation') = 'object' THEN
                (
                    (snapshot->'result_controls'->'continuation') ?&
                        ARRAY['presence']::text[]
                    AND ((snapshot->'result_controls'->'continuation') -
                        ARRAY['presence']::text[]) = '{}'::jsonb
                    AND jsonb_typeof(snapshot->'result_controls'->'continuation'->'presence') = 'string'
                    AND (snapshot->'result_controls'->'continuation'->>'presence') = 'absent'
                )
                OR
                (
                    (snapshot->'result_controls'->'continuation') ?&
                        ARRAY['presence', 'type', 'text']::text[]
                    AND ((snapshot->'result_controls'->'continuation') -
                        ARRAY['presence', 'type', 'text']::text[]) = '{}'::jsonb
                    AND jsonb_typeof(snapshot->'result_controls'->'continuation'->'presence') = 'string'
                    AND (snapshot->'result_controls'->'continuation'->>'presence') = 'value'
                    AND jsonb_typeof(snapshot->'result_controls'->'continuation'->'type') = 'string'
                    AND (snapshot->'result_controls'->'continuation'->>'type') = 'string'
                    AND jsonb_typeof(snapshot->'result_controls'->'continuation'->'text') = 'string'
                    AND octet_length(snapshot->'result_controls'->'continuation'->>'text')
                        BETWEEN 1 AND 4096
                )
            ELSE false
        END
    );

ALTER TABLE pm.control_schema DROP CONSTRAINT control_schema_revision_check;
UPDATE pm.control_schema SET revision = 'pm.control/v17', installed_at = clock_timestamp()
WHERE singleton AND revision = 'pm.control/v16';
ALTER TABLE pm.control_schema
    ADD CONSTRAINT control_schema_revision_check CHECK (revision = 'pm.control/v17');
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- The migrator needs only the bounded cross-scope reads used to refuse erasing
-- retained v17 authority. The policies disappear in the same transaction.
CREATE POLICY pm_continuation_v17_migration_select ON pm.object_reservations
    FOR SELECT TO pm_app_migrator USING (true);
CREATE POLICY pm_continuation_v17_migration_select ON pm.run_input_snapshots
    FOR SELECT TO pm_app_migrator USING (true);
CREATE POLICY pm_continuation_v17_migration_select ON pm.receipts
    FOR SELECT TO pm_app_migrator USING (true);
CREATE POLICY pm_continuation_v17_migration_select ON pm.account_permits
    FOR SELECT TO pm_app_migrator USING (true);
DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM pm.account_permits WHERE state = 'released'
    ) OR EXISTS (
        SELECT 1 FROM pm.object_reservations WHERE purpose = 'continuation'
    ) OR EXISTS (
        SELECT 1
        FROM pm.run_input_snapshots
        WHERE (snapshot->'result_controls') ?| ARRAY['total_record_cap', 'continuation']::text[]
    ) OR EXISTS (
        SELECT 1
        FROM pm.receipts AS receipt
        CROSS JOIN LATERAL jsonb_array_elements(
            CASE
                WHEN jsonb_typeof(receipt.payload->'references') = 'array'
                THEN receipt.payload->'references'
                ELSE '[]'::jsonb
            END
        ) AS reference(value)
        WHERE jsonb_typeof(reference.value) = 'object'
          AND reference.value->>'name' = 'continuation_artifact'
    ) THEN
        RAISE EXCEPTION 'cannot roll back retained durable continuation authority';
    END IF;
END
$$;
DROP POLICY pm_continuation_v17_migration_select ON pm.receipts;
DROP POLICY pm_continuation_v17_migration_select ON pm.run_input_snapshots;
DROP POLICY pm_continuation_v17_migration_select ON pm.object_reservations;
DROP POLICY pm_continuation_v17_migration_select ON pm.account_permits;

ALTER TABLE pm.run_input_snapshots
    DROP CONSTRAINT run_input_snapshot_result_controls;
ALTER TABLE pm.run_input_snapshots
    ADD CONSTRAINT run_input_snapshot_result_controls CHECK (
        jsonb_typeof(snapshot->'result_controls') = 'object'
        AND ((snapshot->'result_controls') - ARRAY['selection', 'filter']::text[]) = '{}'::jsonb
    );

ALTER TABLE pm.account_permits
    DROP CONSTRAINT account_permits_state_check,
    DROP CONSTRAINT account_permits_check1,
    ADD CONSTRAINT account_permits_state_check CHECK (
        state IN ('reserved', 'dispatched', 'unknown', 'resolved', 'expired')
    ),
    ADD CONSTRAINT account_permits_check1 CHECK (
        (state IN ('reserved', 'dispatched', 'unknown') AND resolved_at IS NULL)
        OR
        (state IN ('resolved', 'expired')
            AND resolved_at IS NOT NULL AND resolved_at >= reserved_at)
    );

ALTER TABLE pm.object_reservations
    DROP CONSTRAINT object_reservations_publication_purpose_fkey;
ALTER TABLE pm.artifacts
    DROP CONSTRAINT artifacts_publication_purpose_key;

CREATE OR REPLACE FUNCTION pm.preserve_object_reservation() RETURNS trigger
    LANGUAGE plpgsql SECURITY INVOKER
    SET search_path = pg_catalog, pm AS $$
BEGIN
    IF ROW(NEW.org_id, NEW.workspace_id, NEW.reservation_id, NEW.connection_id,
           NEW.artifact_id, NEW.maximum_bytes, NEW.reserved_at)
       IS DISTINCT FROM
       ROW(OLD.org_id, OLD.workspace_id, OLD.reservation_id, OLD.connection_id,
           OLD.artifact_id, OLD.maximum_bytes, OLD.reserved_at) THEN
        RAISE EXCEPTION 'object reservation identity is immutable';
    END IF;
    IF OLD.state = 'held' AND NEW.state IN ('bound', 'released') THEN
        RETURN NEW;
    END IF;
    IF OLD.state = 'bound' AND NEW.state = 'published'
       AND ROW(NEW.object_attempt_key, NEW.sha256, NEW.byte_length, NEW.media_type, NEW.bound_at)
           IS NOT DISTINCT FROM
           ROW(OLD.object_attempt_key, OLD.sha256, OLD.byte_length, OLD.media_type, OLD.bound_at) THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'object reservation transition is invalid';
END
$$;
ALTER TABLE pm.object_reservations
    DROP CONSTRAINT object_reservations_purpose_valid,
    DROP COLUMN purpose;
ALTER TABLE pm.artifacts ALTER COLUMN purpose SET DEFAULT 'public';

ALTER TABLE pm.control_schema DROP CONSTRAINT control_schema_revision_check;
UPDATE pm.control_schema SET revision = 'pm.control/v16', installed_at = clock_timestamp()
WHERE singleton AND revision = 'pm.control/v17';
ALTER TABLE pm.control_schema
    ADD CONSTRAINT control_schema_revision_check CHECK (revision = 'pm.control/v16');
-- +goose StatementEnd
