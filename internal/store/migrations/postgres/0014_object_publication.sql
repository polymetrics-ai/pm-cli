-- Immutable byte attempts are charged before object I/O. A published reservation
-- and the existing immutable artifact registry form one PostgreSQL receipt.

-- +goose Up
-- +goose StatementBegin
DO $$
BEGIN
    IF current_database() <> 'pm_app' OR current_user <> 'pm_app_migrator' THEN
        RAISE EXCEPTION 'object publication schema requires pm_app_migrator on pm_app';
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
        SELECT 1 FROM pm.control_schema WHERE singleton AND revision = 'pm.control/v13'
    ) THEN
        RAISE EXCEPTION 'object publication migration requires the account cooldown schema';
    END IF;
END
$$;

CREATE TABLE pm.object_org_capacity (
    org_id text PRIMARY KEY CHECK (org_id <> ''),
    reserved_bytes bigint NOT NULL DEFAULT 0 CHECK (reserved_bytes >= 0),
    reserved_attempts bigint NOT NULL DEFAULT 0 CHECK (reserved_attempts >= 0)
);

CREATE TABLE pm.object_workspace_capacity (
    org_id text NOT NULL,
    workspace_id text NOT NULL,
    reserved_bytes bigint NOT NULL DEFAULT 0 CHECK (reserved_bytes >= 0),
    reserved_attempts bigint NOT NULL DEFAULT 0 CHECK (reserved_attempts >= 0),
    PRIMARY KEY (org_id, workspace_id),
    FOREIGN KEY (org_id, workspace_id)
        REFERENCES pm.workspace_namespaces (org_id, workspace_id)
);

CREATE TABLE pm.object_reservations (
    org_id text NOT NULL,
    workspace_id text NOT NULL,
    reservation_id text NOT NULL CHECK (reservation_id <> ''),
    connection_id text NOT NULL CHECK (connection_id <> ''),
    artifact_id text NOT NULL CHECK (artifact_id <> ''),
    maximum_bytes bigint NOT NULL CHECK (maximum_bytes > 0),
    charged_bytes bigint NOT NULL CHECK (charged_bytes >= 0 AND charged_bytes <= maximum_bytes),
    state text NOT NULL DEFAULT 'held' CHECK (state IN ('held', 'bound', 'published', 'released')),
    object_attempt_key text,
    sha256 text,
    byte_length bigint,
    media_type text,
    reserved_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    bound_at timestamptz,
    published_at timestamptz,
    released_at timestamptz,
    published_artifact_id text,
    PRIMARY KEY (org_id, workspace_id, reservation_id),
    UNIQUE (org_id, workspace_id, object_attempt_key),
    FOREIGN KEY (org_id, workspace_id, connection_id)
        REFERENCES pm.connections (org_id, workspace_id, connection_id),
    FOREIGN KEY (org_id, workspace_id, published_artifact_id, object_attempt_key)
        REFERENCES pm.artifacts (org_id, workspace_id, artifact_id, object_attempt_key),
    CHECK (
        (state IN ('held', 'released') AND object_attempt_key IS NULL AND sha256 IS NULL
         AND byte_length IS NULL AND media_type IS NULL AND bound_at IS NULL)
        OR
        (state IN ('bound', 'published') AND object_attempt_key IS NOT NULL AND object_attempt_key <> ''
         AND sha256 IS NOT NULL AND sha256 ~ '^[0-9a-f]{64}$'
         AND byte_length IS NOT NULL AND byte_length >= 0 AND byte_length <= maximum_bytes
         AND media_type IS NOT NULL AND media_type <> '' AND bound_at IS NOT NULL)
    ),
    CHECK (
        (state = 'held' AND charged_bytes = maximum_bytes)
        OR (state = 'released' AND charged_bytes = 0)
        OR (state IN ('bound', 'published') AND charged_bytes = byte_length)
    ),
    CHECK ((state = 'published') = (published_at IS NOT NULL)),
    CHECK ((state = 'published') = (published_artifact_id IS NOT NULL)),
    CHECK (published_artifact_id IS NULL OR published_artifact_id = artifact_id),
    CHECK ((state = 'released') = (released_at IS NOT NULL))
);

CREATE FUNCTION pm.preserve_object_reservation() RETURNS trigger
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
CREATE TRIGGER object_reservation_immutable
    BEFORE UPDATE ON pm.object_reservations
    FOR EACH ROW EXECUTE FUNCTION pm.preserve_object_reservation();
REVOKE ALL ON FUNCTION pm.preserve_object_reservation() FROM PUBLIC;

ALTER TABLE pm.object_org_capacity ENABLE ROW LEVEL SECURITY;
ALTER TABLE pm.object_org_capacity FORCE ROW LEVEL SECURITY;
CREATE POLICY pm_org_scope ON pm.object_org_capacity
    USING (org_id = NULLIF(current_setting('pm.org_id', true), '')
           AND NULLIF(current_setting('pm.workspace_id', true), '') IS NOT NULL)
    WITH CHECK (org_id = NULLIF(current_setting('pm.org_id', true), '')
                AND NULLIF(current_setting('pm.workspace_id', true), '') IS NOT NULL);

DO $$
DECLARE scoped_table text;
BEGIN
    FOREACH scoped_table IN ARRAY ARRAY['object_workspace_capacity', 'object_reservations']
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

REVOKE ALL ON TABLE pm.object_org_capacity, pm.object_workspace_capacity,
    pm.object_reservations FROM PUBLIC, pm_app_runtime;
GRANT SELECT ON TABLE pm.object_org_capacity, pm.object_workspace_capacity,
    pm.object_reservations TO pm_app_runtime;
GRANT INSERT (org_id) ON pm.object_org_capacity TO pm_app_runtime;
GRANT INSERT (org_id, workspace_id) ON pm.object_workspace_capacity TO pm_app_runtime;
GRANT UPDATE (reserved_bytes, reserved_attempts)
    ON pm.object_org_capacity, pm.object_workspace_capacity TO pm_app_runtime;
GRANT INSERT (org_id, workspace_id, reservation_id, connection_id, artifact_id, maximum_bytes, charged_bytes)
    ON pm.object_reservations TO pm_app_runtime;
GRANT UPDATE (state, charged_bytes, object_attempt_key, sha256, byte_length, media_type, bound_at,
              published_at, released_at, published_artifact_id)
    ON pm.object_reservations TO pm_app_runtime;

ALTER TABLE pm.control_schema DROP CONSTRAINT control_schema_revision_check;
UPDATE pm.control_schema SET revision = 'pm.control/v14', installed_at = clock_timestamp()
WHERE singleton AND revision = 'pm.control/v13';
ALTER TABLE pm.control_schema
    ADD CONSTRAINT control_schema_revision_check CHECK (revision = 'pm.control/v14');
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
CREATE POLICY pm_object_publication_migration_select ON pm.object_reservations
    FOR SELECT TO pm_app_migrator USING (true);
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pm.object_reservations) THEN
        RAISE EXCEPTION 'cannot roll back retained object reservation or publication receipts';
    END IF;
END
$$;
DROP TABLE pm.object_reservations;
DROP FUNCTION pm.preserve_object_reservation();
DROP TABLE pm.object_workspace_capacity;
DROP TABLE pm.object_org_capacity;
ALTER TABLE pm.control_schema DROP CONSTRAINT control_schema_revision_check;
UPDATE pm.control_schema SET revision = 'pm.control/v13', installed_at = clock_timestamp()
WHERE singleton AND revision = 'pm.control/v14';
ALTER TABLE pm.control_schema
    ADD CONSTRAINT control_schema_revision_check CHECK (revision = 'pm.control/v13');
-- +goose StatementEnd
