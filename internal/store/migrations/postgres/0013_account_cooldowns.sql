-- Provider Retry-After observations belong to the canonical account, not to a
-- worker process or one connection alias. Existing permit/effect truth is intact.

-- +goose Up
-- +goose StatementBegin
DO $$
BEGIN
    IF current_database() <> 'pm_app' OR current_user <> 'pm_app_migrator' THEN
        RAISE EXCEPTION 'account cooldown schema requires pm_app_migrator on pm_app';
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
        WHERE singleton AND revision = 'pm.control/v12'
    ) THEN
        RAISE EXCEPTION 'account cooldown migration requires the artifact authority schema';
    END IF;
END
$$;

ALTER TABLE pm.provider_accounts
    ADD COLUMN cooldown_until timestamptz,
    ADD CONSTRAINT provider_account_cooldown_valid CHECK (
        cooldown_until IS NULL OR (
            isfinite(cooldown_until)
            AND cooldown_until >= timestamptz '0001-01-01 00:00:00+00'
            AND cooldown_until <= timestamptz '9999-12-31 23:59:59.999999+00'
        )
    );

GRANT UPDATE (cooldown_until) ON pm.provider_accounts TO pm_app_runtime;

ALTER TABLE pm.control_schema DROP CONSTRAINT control_schema_revision_check;
UPDATE pm.control_schema
SET revision = 'pm.control/v13', installed_at = clock_timestamp()
WHERE singleton AND revision = 'pm.control/v12';
ALTER TABLE pm.control_schema
    ADD CONSTRAINT control_schema_revision_check CHECK (revision = 'pm.control/v13');
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
CREATE POLICY pm_account_cooldown_migration_select ON pm.provider_accounts
    FOR SELECT TO pm_app_migrator USING (true);

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pm.provider_accounts WHERE cooldown_until IS NOT NULL) THEN
        RAISE EXCEPTION 'cannot roll back retained provider account cooldowns';
    END IF;
END
$$;

DROP POLICY pm_account_cooldown_migration_select ON pm.provider_accounts;

REVOKE UPDATE (cooldown_until) ON pm.provider_accounts FROM pm_app_runtime;

ALTER TABLE pm.provider_accounts
    DROP CONSTRAINT provider_account_cooldown_valid,
    DROP COLUMN cooldown_until;

ALTER TABLE pm.control_schema DROP CONSTRAINT control_schema_revision_check;
UPDATE pm.control_schema
SET revision = 'pm.control/v12', installed_at = clock_timestamp()
WHERE singleton AND revision = 'pm.control/v13';
ALTER TABLE pm.control_schema
    ADD CONSTRAINT control_schema_revision_check CHECK (revision = 'pm.control/v12');
-- +goose StatementEnd
