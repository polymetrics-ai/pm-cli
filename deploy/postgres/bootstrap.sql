\set ON_ERROR_STOP on
\set QUIET on
SET client_min_messages = warning;

DO $roles$
DECLARE
    role_name text;
    password_file text;
    role_password text;
BEGIN
    FOR role_name, password_file IN
        SELECT * FROM (VALUES
            ('pm_app_migrator', '/var/lib/postgresql/pm-stack-secrets/pm-app-migrator-password'),
            ('pm_app_runtime', '/var/lib/postgresql/pm-stack-secrets/pm-app-runtime-password'),
            ('temporal_migrator', '/var/lib/postgresql/pm-stack-secrets/temporal-migrator-password'),
            ('temporal_runtime', '/var/lib/postgresql/pm-stack-secrets/temporal-runtime-password'),
            ('temporal_visibility_migrator', '/var/lib/postgresql/pm-stack-secrets/temporal-visibility-migrator-password'),
            ('temporal_visibility_runtime', '/var/lib/postgresql/pm-stack-secrets/temporal-visibility-runtime-password'),
            ('pm_objects_migrator', '/var/lib/postgresql/pm-stack-secrets/objects-migrator-password'),
            ('pm_objects_runtime', '/var/lib/postgresql/pm-stack-secrets/objects-runtime-password'),
            ('pm_denied', '/var/lib/postgresql/pm-stack-secrets/denied-password')
        ) AS roles(name, secret_file)
    LOOP
        role_password := btrim(pg_read_file(password_file));
        IF role_password = '' THEN
            RAISE EXCEPTION 'empty password file for role %', role_name;
        END IF;
        IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = role_name) THEN
            EXECUTE format(
                'ALTER ROLE %I LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOREPLICATION PASSWORD %L',
                role_name,
                role_password
            );
        ELSE
            EXECUTE format(
                'CREATE ROLE %I LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOREPLICATION PASSWORD %L',
                role_name,
                role_password
            );
        END IF;
    END LOOP;
END
$roles$;

SELECT 'CREATE DATABASE pm_app OWNER pm_app_migrator'
WHERE NOT EXISTS (SELECT 1 FROM pg_database WHERE datname = 'pm_app')\gexec
SELECT 'CREATE DATABASE temporal OWNER temporal_migrator'
WHERE NOT EXISTS (SELECT 1 FROM pg_database WHERE datname = 'temporal')\gexec
SELECT 'CREATE DATABASE temporal_visibility OWNER temporal_visibility_migrator'
WHERE NOT EXISTS (SELECT 1 FROM pg_database WHERE datname = 'temporal_visibility')\gexec
SELECT 'CREATE DATABASE pm_objects_meta OWNER pm_objects_migrator'
WHERE NOT EXISTS (SELECT 1 FROM pg_database WHERE datname = 'pm_objects_meta')\gexec

ALTER DATABASE pm_app OWNER TO pm_app_migrator;
ALTER DATABASE temporal OWNER TO temporal_migrator;
ALTER DATABASE temporal_visibility OWNER TO temporal_visibility_migrator;
ALTER DATABASE pm_objects_meta OWNER TO pm_objects_migrator;

REVOKE CONNECT ON DATABASE pm_app FROM PUBLIC;
REVOKE CREATE, TEMPORARY ON DATABASE pm_app FROM PUBLIC, pm_app_runtime;
REVOKE CONNECT ON DATABASE temporal FROM PUBLIC;
REVOKE CONNECT ON DATABASE temporal_visibility FROM PUBLIC;
REVOKE CONNECT ON DATABASE pm_objects_meta FROM PUBLIC;
GRANT CONNECT ON DATABASE pm_app TO pm_app_migrator, pm_app_runtime;
GRANT CONNECT ON DATABASE temporal TO temporal_migrator, temporal_runtime;
GRANT CONNECT ON DATABASE temporal_visibility TO temporal_visibility_migrator, temporal_visibility_runtime;
GRANT CONNECT ON DATABASE pm_objects_meta TO pm_objects_migrator, pm_objects_runtime;

\connect pm_app
REVOKE ALL ON SCHEMA public FROM PUBLIC;
CREATE SCHEMA IF NOT EXISTS pm AUTHORIZATION pm_app_migrator;
ALTER SCHEMA pm OWNER TO pm_app_migrator;
CREATE TABLE IF NOT EXISTS pm.stack_schema (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    revision text NOT NULL
);
ALTER TABLE pm.stack_schema OWNER TO pm_app_migrator;
INSERT INTO pm.stack_schema (singleton, revision)
VALUES (true, 'pm.stack/v1')
ON CONFLICT (singleton) DO UPDATE SET revision = EXCLUDED.revision;
GRANT USAGE ON SCHEMA pm TO pm_app_runtime;
REVOKE ALL ON pm.stack_schema FROM pm_app_runtime;
GRANT SELECT ON pm.stack_schema TO pm_app_runtime;
ALTER DEFAULT PRIVILEGES FOR ROLE pm_app_migrator IN SCHEMA pm
    REVOKE ALL ON TABLES FROM pm_app_runtime;
ALTER DEFAULT PRIVILEGES FOR ROLE pm_app_migrator IN SCHEMA pm
    REVOKE ALL ON SEQUENCES FROM pm_app_runtime;

\connect temporal
REVOKE ALL ON SCHEMA public FROM PUBLIC;
GRANT USAGE, CREATE ON SCHEMA public TO temporal_migrator;
GRANT USAGE ON SCHEMA public TO temporal_runtime;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO temporal_runtime;
GRANT USAGE, SELECT, UPDATE ON ALL SEQUENCES IN SCHEMA public TO temporal_runtime;
ALTER DEFAULT PRIVILEGES FOR ROLE temporal_migrator IN SCHEMA public
    GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO temporal_runtime;
ALTER DEFAULT PRIVILEGES FOR ROLE temporal_migrator IN SCHEMA public
    GRANT USAGE, SELECT, UPDATE ON SEQUENCES TO temporal_runtime;

\connect temporal_visibility
REVOKE ALL ON SCHEMA public FROM PUBLIC;
GRANT USAGE, CREATE ON SCHEMA public TO temporal_visibility_migrator;
GRANT USAGE ON SCHEMA public TO temporal_visibility_runtime;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO temporal_visibility_runtime;
GRANT USAGE, SELECT, UPDATE ON ALL SEQUENCES IN SCHEMA public TO temporal_visibility_runtime;
ALTER DEFAULT PRIVILEGES FOR ROLE temporal_visibility_migrator IN SCHEMA public
    GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO temporal_visibility_runtime;
ALTER DEFAULT PRIVILEGES FOR ROLE temporal_visibility_migrator IN SCHEMA public
    GRANT USAGE, SELECT, UPDATE ON SEQUENCES TO temporal_visibility_runtime;

\connect pm_objects_meta
REVOKE ALL ON SCHEMA public FROM PUBLIC;
CREATE SCHEMA IF NOT EXISTS pm_objects AUTHORIZATION pm_objects_migrator;
ALTER SCHEMA pm_objects OWNER TO pm_objects_migrator;
CREATE TABLE IF NOT EXISTS pm_objects.filemeta (
    dirhash bigint,
    name varchar(65535) COLLATE "C",
    directory varchar(65535),
    meta bytea,
    PRIMARY KEY (dirhash, name)
);
CREATE TABLE IF NOT EXISTS pm_objects."{{ARTIFACT_BUCKET}}" (
    dirhash bigint,
    name varchar(65535) COLLATE "C",
    directory varchar(65535),
    meta bytea,
    PRIMARY KEY (dirhash, name)
);
ALTER TABLE pm_objects.filemeta OWNER TO pm_objects_migrator;
ALTER TABLE pm_objects."{{ARTIFACT_BUCKET}}" OWNER TO pm_objects_migrator;
GRANT USAGE, CREATE ON SCHEMA pm_objects TO pm_objects_runtime;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA pm_objects TO pm_objects_runtime;
ALTER DEFAULT PRIVILEGES FOR ROLE pm_objects_migrator IN SCHEMA pm_objects
    GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO pm_objects_runtime;
