-- Account permit expiry. A reservation that finds its provider account short of
-- capacity expires the reserved permits of that account whose expiry has passed.
-- The workspaces of one organization share the account, so a bounded
-- migrator-owned definer function reaches the permits of every workspace of the
-- installed organization.

-- +goose Up
-- +goose StatementBegin
DO $$
BEGIN
    IF current_database() <> 'pm_app' OR current_user <> 'pm_app_migrator' THEN
        RAISE EXCEPTION 'account permit expiry schema requires pm_app_migrator on pm_app';
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
        SELECT 1 FROM pm.control_schema WHERE singleton AND revision = 'pm.control/v19'
    ) THEN
        RAISE EXCEPTION 'account permit expiry migration requires the warehouse segment schema';
    END IF;
END
$$;

-- FORCE RLS also applies to the owning migrator. These two policies serve only
-- the definer function below, and both are limited to the installed
-- organization. The SELECT policy also covers expired rows, because an UPDATE
-- must be able to read the row that it writes.
CREATE POLICY pm_permit_expiry_select ON pm.account_permits
    FOR SELECT TO pm_app_migrator
    USING (
        org_id = NULLIF(current_setting('pm.org_id', true), '')
        AND state IN ('reserved', 'expired')
    );
CREATE POLICY pm_permit_expiry_update ON pm.account_permits
    FOR UPDATE TO pm_app_migrator
    USING (
        org_id = NULLIF(current_setting('pm.org_id', true), '')
        AND state = 'reserved'
    )
    WITH CHECK (
        org_id = NULLIF(current_setting('pm.org_id', true), '')
        AND state = 'expired'
    );

CREATE FUNCTION pm.expire_unused_account_permits(
    expiry_org_id text,
    expiry_provider_account_id text
)
RETURNS bigint
LANGUAGE plpgsql
VOLATILE
SECURITY DEFINER
SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    expiry_time timestamptz;
    expiry_units bigint;
BEGIN
    IF expiry_org_id IS NULL OR expiry_org_id = '' OR octet_length(expiry_org_id) > 512
       OR btrim(expiry_org_id) <> expiry_org_id OR expiry_org_id ~ '[[:cntrl:]]'
       OR expiry_provider_account_id IS NULL OR expiry_provider_account_id = ''
       OR octet_length(expiry_provider_account_id) > 512
       OR btrim(expiry_provider_account_id) <> expiry_provider_account_id
       OR expiry_provider_account_id ~ '[[:cntrl:]]' THEN
        RAISE EXCEPTION USING ERRCODE = '22023', MESSAGE = 'account permit expiry target is invalid';
    END IF;

    -- The target must be the installed organization, and the installed
    -- workspace must reach the account through one of its connections. The
    -- connection read uses the ordinary workspace scope policy.
    IF expiry_org_id IS DISTINCT FROM NULLIF(current_setting('pm.org_id', true), '')
       OR NOT EXISTS (
           SELECT 1
           FROM pm.connections AS connection
           WHERE connection.org_id = expiry_org_id
             AND connection.workspace_id = NULLIF(current_setting('pm.workspace_id', true), '')
             AND connection.provider_account_id = expiry_provider_account_id
       ) THEN
        RAISE EXCEPTION USING ERRCODE = '42501',
            MESSAGE = 'account permit expiry target is outside the installed scope';
    END IF;

    -- The caller already holds this row lock. Taking it here keeps the lock
    -- order of every permit transition: the account row, then permit rows.
    PERFORM 1
    FROM pm.provider_accounts AS account
    WHERE account.org_id = expiry_org_id
      AND account.provider_account_id = expiry_provider_account_id
    FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION USING ERRCODE = '42501',
            MESSAGE = 'account permit expiry target is outside the installed scope';
    END IF;

    -- The conditions of ExpireAccountPermit, applied to each locked current
    -- row: still reserved, unresolved, and past its expiry on the database
    -- clock. No other column changes. The number of reserved permits of one
    -- account cannot exceed its capacity, which bounds this statement.
    expiry_time := clock_timestamp();
    WITH expired AS (
        UPDATE pm.account_permits AS permit
        SET state = 'expired', resolved_at = expiry_time
        WHERE permit.org_id = expiry_org_id
          AND permit.provider_account_id = expiry_provider_account_id
          AND permit.state = 'reserved'
          AND permit.resolved_at IS NULL
          AND permit.expires_at <= expiry_time
        RETURNING permit.units
    )
    SELECT COALESCE(sum(expired.units), 0)::bigint INTO expiry_units FROM expired;

    IF expiry_units > 0 THEN
        UPDATE pm.provider_accounts AS account
        SET reserved_units = account.reserved_units - expiry_units
        WHERE account.org_id = expiry_org_id
          AND account.provider_account_id = expiry_provider_account_id
          AND account.reserved_units >= expiry_units;
        IF NOT FOUND THEN
            RAISE EXCEPTION USING ERRCODE = '23514',
                MESSAGE = 'stored provider account reservation is inconsistent';
        END IF;
    END IF;
    RETURN expiry_units;
END
$$;

REVOKE ALL ON FUNCTION pm.expire_unused_account_permits(text, text)
    FROM PUBLIC, pm_app_runtime;
GRANT EXECUTE ON FUNCTION pm.expire_unused_account_permits(text, text)
    TO pm_app_runtime;

ALTER TABLE pm.control_schema DROP CONSTRAINT control_schema_revision_check;
UPDATE pm.control_schema SET revision = 'pm.control/v20', installed_at = clock_timestamp()
WHERE singleton AND revision = 'pm.control/v19';
ALTER TABLE pm.control_schema
    ADD CONSTRAINT control_schema_revision_check CHECK (revision = 'pm.control/v20');
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- Expired permits are valid rows in the earlier schema, so no retained data
-- blocks the rollback. After it, no reservation expires unused permits.
REVOKE ALL ON FUNCTION pm.expire_unused_account_permits(text, text)
    FROM PUBLIC, pm_app_runtime;
DROP FUNCTION pm.expire_unused_account_permits(text, text);
DROP POLICY pm_permit_expiry_update ON pm.account_permits;
DROP POLICY pm_permit_expiry_select ON pm.account_permits;

ALTER TABLE pm.control_schema DROP CONSTRAINT control_schema_revision_check;
UPDATE pm.control_schema SET revision = 'pm.control/v19', installed_at = clock_timestamp()
WHERE singleton AND revision = 'pm.control/v20';
ALTER TABLE pm.control_schema
    ADD CONSTRAINT control_schema_revision_check CHECK (revision = 'pm.control/v19');
-- +goose StatementEnd
