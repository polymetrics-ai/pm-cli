-- Warehouse dataset membership: one registered segment ledger, one cursor, one
-- retained snapshot set and one retention registry. A dataset's rows are the
-- rows the manifest registered; an object that exists in shared storage without
-- a ledger row is never membership.

-- +goose Up
-- +goose StatementBegin
DO $$
BEGIN
    IF current_database() <> 'pm_app' OR current_user <> 'pm_app_migrator' THEN
        RAISE EXCEPTION 'warehouse segment schema requires pm_app_migrator on pm_app';
    END IF;
    IF to_regrole('pm_app_runtime') IS NULL THEN
        RAISE EXCEPTION 'required runtime role is missing';
    END IF;
    IF EXISTS (
        SELECT 1 FROM pg_roles WHERE rolname = 'pm_app_runtime'
          AND (rolsuper OR rolbypassrls OR rolcreatedb OR rolcreaterole OR rolreplication)
    ) OR pg_has_role('pm_app_runtime', 'pm_app_migrator', 'MEMBER') THEN
        RAISE EXCEPTION 'runtime role has prohibited PostgreSQL authority';
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM pm.control_schema WHERE singleton AND revision = 'pm.control/v18'
    ) THEN
        RAISE EXCEPTION 'warehouse segment migration requires the worker retry schema';
    END IF;
END
$$;

CREATE TABLE pm.warehouse_datasets (
    org_id text NOT NULL,
    workspace_id text NOT NULL,
    dataset_id text NOT NULL CHECK (dataset_id <> ''),
    connector_id text NOT NULL CHECK (connector_id <> ''),
    connection_id text NOT NULL CHECK (connection_id <> ''),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (org_id, workspace_id, dataset_id),
    FOREIGN KEY (org_id, workspace_id, connection_id)
        REFERENCES pm.connections (org_id, workspace_id, connection_id) ON DELETE CASCADE
);

-- One immutable registered segment per dataset ordinal. The ordinal is the
-- manifest revision the publication advanced, so a registerable row can never
-- skip a revision and can never name an artifact the manifest did not register.
CREATE TABLE pm.warehouse_segments (
    org_id text NOT NULL,
    workspace_id text NOT NULL,
    dataset_id text NOT NULL CHECK (dataset_id <> ''),
    segment_ordinal bigint NOT NULL CHECK (segment_ordinal > 0),
    manifest_revision bigint NOT NULL,
    artifact_id text NOT NULL CHECK (artifact_id <> ''),
    object_attempt_key text NOT NULL CHECK (object_attempt_key <> ''),
    row_count bigint NOT NULL CHECK (row_count >= 0),
    column_fingerprint text NOT NULL CHECK (column_fingerprint ~ '^sha256:[0-9a-f]{64}$'),
    schema_document jsonb NOT NULL CHECK (jsonb_typeof(schema_document) = 'object'),
    state text NOT NULL DEFAULT 'live' CHECK (state IN ('live', 'retired', 'collected')),
    published_at timestamptz NOT NULL,
    retired_at timestamptz,
    collected_at timestamptz,
    PRIMARY KEY (org_id, workspace_id, dataset_id, segment_ordinal),
    UNIQUE (org_id, workspace_id, artifact_id),
    CHECK (manifest_revision = segment_ordinal),
    CHECK ((state = 'live') = (retired_at IS NULL AND collected_at IS NULL)),
    CHECK ((state = 'collected') = (collected_at IS NOT NULL)),
    FOREIGN KEY (org_id, workspace_id, dataset_id)
        REFERENCES pm.warehouse_datasets (org_id, workspace_id, dataset_id) ON DELETE CASCADE,
    FOREIGN KEY (org_id, workspace_id, artifact_id, object_attempt_key)
        REFERENCES pm.artifacts (org_id, workspace_id, artifact_id, object_attempt_key) ON DELETE CASCADE
);

CREATE FUNCTION pm.preserve_warehouse_segment() RETURNS trigger
    LANGUAGE plpgsql SECURITY INVOKER
    SET search_path = pg_catalog AS $$
BEGIN
    IF ROW(NEW.org_id, NEW.workspace_id, NEW.dataset_id, NEW.segment_ordinal,
           NEW.manifest_revision, NEW.artifact_id, NEW.object_attempt_key,
           NEW.row_count, NEW.column_fingerprint, NEW.schema_document, NEW.published_at)
       IS DISTINCT FROM
       ROW(OLD.org_id, OLD.workspace_id, OLD.dataset_id, OLD.segment_ordinal,
           OLD.manifest_revision, OLD.artifact_id, OLD.object_attempt_key,
           OLD.row_count, OLD.column_fingerprint, OLD.schema_document, OLD.published_at) THEN
        RAISE EXCEPTION 'warehouse segment identity is immutable';
    END IF;
    IF OLD.state = NEW.state THEN
        RETURN NEW;
    END IF;
    IF OLD.state = 'live' AND NEW.state IN ('retired', 'collected') THEN
        RETURN NEW;
    END IF;
    IF OLD.state = 'retired' AND NEW.state = 'collected' THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'warehouse segment transition is invalid';
END
$$;
CREATE TRIGGER warehouse_segment_immutable
    BEFORE UPDATE ON pm.warehouse_segments
    FOR EACH ROW EXECUTE FUNCTION pm.preserve_warehouse_segment();
REVOKE ALL ON FUNCTION pm.preserve_warehouse_segment() FROM PUBLIC;

-- The cursor can only name one registered ordinal, so a cursor over a gap is
-- not representable.
CREATE TABLE pm.warehouse_dataset_cursors (
    org_id text NOT NULL,
    workspace_id text NOT NULL,
    dataset_id text NOT NULL CHECK (dataset_id <> ''),
    cursor_ordinal bigint NOT NULL CHECK (cursor_ordinal > 0),
    manifest_revision bigint NOT NULL CHECK (manifest_revision >= cursor_ordinal),
    advanced_at timestamptz NOT NULL,
    PRIMARY KEY (org_id, workspace_id, dataset_id),
    FOREIGN KEY (org_id, workspace_id, dataset_id, cursor_ordinal)
        REFERENCES pm.warehouse_segments (org_id, workspace_id, dataset_id, segment_ordinal) ON DELETE CASCADE
);

CREATE TABLE pm.warehouse_snapshots (
    org_id text NOT NULL,
    workspace_id text NOT NULL,
    snapshot_id text NOT NULL CHECK (snapshot_id <> ''),
    dataset_id text NOT NULL CHECK (dataset_id <> ''),
    manifest_revision bigint NOT NULL CHECK (manifest_revision > 0),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (org_id, workspace_id, snapshot_id),
    FOREIGN KEY (org_id, workspace_id, dataset_id)
        REFERENCES pm.warehouse_datasets (org_id, workspace_id, dataset_id) ON DELETE CASCADE
);

-- Retention reachability: a segment named here by a snapshot, plan, proof,
-- backup or reservation is never collected. Rows are append-only.
CREATE TABLE pm.warehouse_segment_references (
    org_id text NOT NULL,
    workspace_id text NOT NULL,
    dataset_id text NOT NULL CHECK (dataset_id <> ''),
    segment_ordinal bigint NOT NULL CHECK (segment_ordinal > 0),
    holder_kind text NOT NULL CHECK (holder_kind IN ('snapshot', 'plan', 'proof', 'backup', 'reservation')),
    holder_id text NOT NULL CHECK (holder_id <> ''),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (org_id, workspace_id, dataset_id, segment_ordinal, holder_kind, holder_id),
    FOREIGN KEY (org_id, workspace_id, dataset_id, segment_ordinal)
        REFERENCES pm.warehouse_segments (org_id, workspace_id, dataset_id, segment_ordinal) ON DELETE CASCADE
);

DO $$
DECLARE scoped_table text;
BEGIN
    FOREACH scoped_table IN ARRAY ARRAY[
        'warehouse_datasets', 'warehouse_segments', 'warehouse_dataset_cursors',
        'warehouse_snapshots', 'warehouse_segment_references'
    ]
    LOOP
        EXECUTE format('ALTER TABLE pm.%I ENABLE ROW LEVEL SECURITY', scoped_table);
        EXECUTE format('ALTER TABLE pm.%I FORCE ROW LEVEL SECURITY', scoped_table);
        EXECUTE format(
            'CREATE POLICY pm_scope ON pm.%I USING (' ||
            'org_id = NULLIF(current_setting(''pm.org_id'', true), '''') AND ' ||
            'workspace_id = NULLIF(current_setting(''pm.workspace_id'', true), '''')' ||
            ') WITH CHECK (' ||
            'org_id = NULLIF(current_setting(''pm.org_id'', true), '''') AND ' ||
            'workspace_id = NULLIF(current_setting(''pm.workspace_id'', true), '''')' ||
            ')', scoped_table);
    END LOOP;
END
$$;

REVOKE ALL ON TABLE pm.warehouse_datasets, pm.warehouse_segments,
    pm.warehouse_dataset_cursors, pm.warehouse_snapshots,
    pm.warehouse_segment_references FROM PUBLIC, pm_app_runtime;
GRANT SELECT ON TABLE pm.warehouse_datasets, pm.warehouse_segments,
    pm.warehouse_dataset_cursors, pm.warehouse_snapshots,
    pm.warehouse_segment_references TO pm_app_runtime;
GRANT INSERT (org_id, workspace_id, dataset_id, connector_id, connection_id)
    ON pm.warehouse_datasets TO pm_app_runtime;
-- created_at is never written by the runtime: the column default owns it.
GRANT INSERT (org_id, workspace_id, dataset_id, segment_ordinal, manifest_revision, artifact_id,
              object_attempt_key, row_count, column_fingerprint, schema_document, state, published_at)
    ON pm.warehouse_segments TO pm_app_runtime;
GRANT UPDATE (state, retired_at, collected_at) ON pm.warehouse_segments TO pm_app_runtime;
GRANT INSERT (org_id, workspace_id, dataset_id, cursor_ordinal, manifest_revision, advanced_at)
    ON pm.warehouse_dataset_cursors TO pm_app_runtime;
GRANT UPDATE (cursor_ordinal, manifest_revision, advanced_at)
    ON pm.warehouse_dataset_cursors TO pm_app_runtime;
GRANT INSERT (org_id, workspace_id, snapshot_id, dataset_id, manifest_revision)
    ON pm.warehouse_snapshots TO pm_app_runtime;
GRANT INSERT (org_id, workspace_id, dataset_id, segment_ordinal, holder_kind, holder_id)
    ON pm.warehouse_segment_references TO pm_app_runtime;

ALTER TABLE pm.control_schema DROP CONSTRAINT control_schema_revision_check;
UPDATE pm.control_schema SET revision = 'pm.control/v19', installed_at = clock_timestamp()
WHERE singleton AND revision = 'pm.control/v18';
ALTER TABLE pm.control_schema
    ADD CONSTRAINT control_schema_revision_check CHECK (revision = 'pm.control/v19');
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- FORCE RLS applies to the owning migrator, so refusal checks need an explicit
-- cross-scope read over exactly the tables whose rows prove retained membership.
CREATE POLICY pm_warehouse_migration_select ON pm.warehouse_datasets
    FOR SELECT TO pm_app_migrator USING (true);
CREATE POLICY pm_warehouse_migration_select ON pm.warehouse_segments
    FOR SELECT TO pm_app_migrator USING (true);
CREATE POLICY pm_warehouse_migration_select ON pm.warehouse_snapshots
    FOR SELECT TO pm_app_migrator USING (true);
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pm.warehouse_datasets)
       OR EXISTS (SELECT 1 FROM pm.warehouse_segments)
       OR EXISTS (SELECT 1 FROM pm.warehouse_snapshots) THEN
        RAISE EXCEPTION 'cannot roll back retained warehouse dataset membership';
    END IF;
END
$$;
DROP POLICY pm_warehouse_migration_select ON pm.warehouse_datasets;
DROP POLICY pm_warehouse_migration_select ON pm.warehouse_segments;
DROP POLICY pm_warehouse_migration_select ON pm.warehouse_snapshots;

DROP TABLE pm.warehouse_segment_references;
DROP TABLE pm.warehouse_snapshots;
DROP TABLE pm.warehouse_dataset_cursors;
DROP TRIGGER warehouse_segment_immutable ON pm.warehouse_segments;
DROP FUNCTION pm.preserve_warehouse_segment();
DROP TABLE pm.warehouse_segments;
DROP TABLE pm.warehouse_datasets;

ALTER TABLE pm.control_schema DROP CONSTRAINT control_schema_revision_check;
UPDATE pm.control_schema SET revision = 'pm.control/v18', installed_at = clock_timestamp()
WHERE singleton AND revision = 'pm.control/v19';
ALTER TABLE pm.control_schema
    ADD CONSTRAINT control_schema_revision_check CHECK (revision = 'pm.control/v18');
-- +goose StatementEnd
