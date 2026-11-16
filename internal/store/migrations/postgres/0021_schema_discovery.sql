-- Schema discovery (ADR 0007): the accepted intent of each discovery run and
-- the immutable complete candidate snapshots that discoveries store. A
-- discovery never selects: the selection is a separate later record.

-- +goose Up
-- +goose StatementBegin
DO $$
BEGIN
    IF current_database() <> 'pm_app' OR current_user <> 'pm_app_migrator' THEN
        RAISE EXCEPTION 'schema discovery schema requires pm_app_migrator on pm_app';
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
        SELECT 1 FROM pm.control_schema WHERE singleton AND revision = 'pm.control/v20'
    ) THEN
        RAISE EXCEPTION 'schema discovery migration requires the operator bootstrap schema';
    END IF;
END
$$;

-- One row for each admitted discovery run. It records the declared relation
-- binding that admission resolved, so the worker can refuse a declaration that
-- changed before any provider I/O. Rows are immutable for the runtime role.
CREATE TABLE pm.schema_discoveries (
    org_id text NOT NULL,
    workspace_id text NOT NULL,
    run_id text NOT NULL CHECK (run_id <> ''),
    connection_id text NOT NULL CHECK (connection_id <> ''),
    connector_id text NOT NULL CHECK (connector_id <> ''),
    resource text NOT NULL CHECK (resource <> '' AND octet_length(resource) <= 512),
    relation_binding text NOT NULL CHECK (relation_binding <> '' AND octet_length(relation_binding) <= 512),
    provider_account_id text NOT NULL CHECK (provider_account_id <> '' AND octet_length(provider_account_id) <= 512),
    permission_revision text NOT NULL CHECK (permission_revision <> '' AND octet_length(permission_revision) <= 512),
    source_revision text NOT NULL CHECK (source_revision <> '' AND octet_length(source_revision) <= 512),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (org_id, workspace_id, run_id),
    FOREIGN KEY (org_id, workspace_id, run_id)
        REFERENCES pm.run_specs (org_id, workspace_id, run_id) ON DELETE CASCADE,
    FOREIGN KEY (org_id, workspace_id, connection_id)
        REFERENCES pm.connections (org_id, workspace_id, connection_id) ON DELETE CASCADE
);

CREATE INDEX schema_discoveries_connection_idx
    ON pm.schema_discoveries (org_id, workspace_id, connection_id, resource);

-- One immutable complete candidate snapshot. Its key is its content digest
-- (schema_ref) within its connection; the digest covers the whole binding, so
-- the binding columns repeat facts of the document for scoped reads. Only a
-- complete discovery is stored.
CREATE TABLE pm.schema_snapshots (
    org_id text NOT NULL,
    workspace_id text NOT NULL,
    connection_id text NOT NULL CHECK (connection_id <> ''),
    schema_ref text NOT NULL CHECK (schema_ref ~ '^sha256:[0-9a-f]{64}$'),
    connector_id text NOT NULL CHECK (connector_id <> ''),
    provider_account_id text NOT NULL CHECK (provider_account_id <> ''),
    permission_revision text NOT NULL CHECK (permission_revision <> ''),
    resource text NOT NULL CHECK (resource <> '' AND octet_length(resource) <= 512),
    source_revision text NOT NULL CHECK (source_revision <> ''),
    complete boolean NOT NULL CHECK (complete),
    field_count integer NOT NULL CHECK (field_count BETWEEN 0 AND 4096),
    document jsonb NOT NULL,
    discovered_by_run_id text NOT NULL CHECK (discovered_by_run_id <> ''),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (org_id, workspace_id, connection_id, schema_ref),
    FOREIGN KEY (org_id, workspace_id, connection_id)
        REFERENCES pm.connections (org_id, workspace_id, connection_id) ON DELETE CASCADE,
    FOREIGN KEY (org_id, workspace_id, discovered_by_run_id)
        REFERENCES pm.schema_discoveries (org_id, workspace_id, run_id),
    CONSTRAINT schema_snapshot_document_bound CHECK (
        jsonb_typeof(document) = 'object'
        AND octet_length(document::text) <= 4194304
        AND jsonb_typeof(document->'fields') = 'array'
        AND jsonb_array_length(document->'fields') = field_count
    ),
    CONSTRAINT schema_snapshot_document_binding CHECK (
        (document->>'fingerprint') IS NOT DISTINCT FROM schema_ref
        AND (document->'completeness'->>'complete') IS NOT DISTINCT FROM 'true'
        AND (document->'binding'->>'org_id') IS NOT DISTINCT FROM org_id
        AND (document->'binding'->>'workspace_id') IS NOT DISTINCT FROM workspace_id
        AND (document->'binding'->>'connector_id') IS NOT DISTINCT FROM connector_id
        AND (document->'binding'->>'connection_id') IS NOT DISTINCT FROM connection_id
        AND (document->'binding'->>'provider_account_id') IS NOT DISTINCT FROM provider_account_id
        AND (document->'binding'->>'permission_revision') IS NOT DISTINCT FROM permission_revision
        AND (document->'binding'->>'resource') IS NOT DISTINCT FROM resource
        AND (document->'binding'->>'source_revision') IS NOT DISTINCT FROM source_revision
    )
);

CREATE INDEX schema_snapshots_resource_idx
    ON pm.schema_snapshots (org_id, workspace_id, connection_id, resource);

DO $$
DECLARE scoped_table text;
BEGIN
    FOREACH scoped_table IN ARRAY ARRAY['schema_discoveries', 'schema_snapshots']
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

-- The runtime role reads and inserts only. It never updates or deletes a
-- discovery intent or a candidate, so both stay immutable.
REVOKE ALL ON TABLE pm.schema_discoveries, pm.schema_snapshots FROM PUBLIC, pm_app_runtime;
GRANT SELECT ON TABLE pm.schema_discoveries, pm.schema_snapshots TO pm_app_runtime;
-- created_at is never written by the runtime: the column default owns it.
GRANT INSERT (org_id, workspace_id, run_id, connection_id, connector_id, resource, relation_binding,
              provider_account_id, permission_revision, source_revision)
    ON pm.schema_discoveries TO pm_app_runtime;
GRANT INSERT (org_id, workspace_id, connection_id, schema_ref, connector_id, provider_account_id,
              permission_revision, resource, source_revision, complete, field_count, document,
              discovered_by_run_id)
    ON pm.schema_snapshots TO pm_app_runtime;

ALTER TABLE pm.control_schema DROP CONSTRAINT control_schema_revision_check;
UPDATE pm.control_schema SET revision = 'pm.control/v21', installed_at = clock_timestamp()
WHERE singleton AND revision = 'pm.control/v20';
ALTER TABLE pm.control_schema
    ADD CONSTRAINT control_schema_revision_check CHECK (revision = 'pm.control/v21');
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- FORCE RLS applies to the owning migrator, so the refusal check needs an
-- explicit cross-scope read of the retained candidates and intents.
CREATE POLICY pm_schema_discovery_migration_select ON pm.schema_discoveries
    FOR SELECT TO pm_app_migrator USING (true);
CREATE POLICY pm_schema_discovery_migration_select ON pm.schema_snapshots
    FOR SELECT TO pm_app_migrator USING (true);
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pm.schema_discoveries)
       OR EXISTS (SELECT 1 FROM pm.schema_snapshots) THEN
        RAISE EXCEPTION 'cannot roll back retained schema discoveries or candidate snapshots';
    END IF;
END
$$;
DROP POLICY pm_schema_discovery_migration_select ON pm.schema_discoveries;
DROP POLICY pm_schema_discovery_migration_select ON pm.schema_snapshots;

DROP TABLE pm.schema_snapshots;
DROP TABLE pm.schema_discoveries;

ALTER TABLE pm.control_schema DROP CONSTRAINT control_schema_revision_check;
UPDATE pm.control_schema SET revision = 'pm.control/v20', installed_at = clock_timestamp()
WHERE singleton AND revision = 'pm.control/v21';
ALTER TABLE pm.control_schema
    ADD CONSTRAINT control_schema_revision_check CHECK (revision = 'pm.control/v20');
-- +goose StatementEnd
