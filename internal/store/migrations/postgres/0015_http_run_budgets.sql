-- +goose Up
-- +goose StatementBegin
ALTER TABLE pm.run_budget_snapshots DROP CONSTRAINT run_budget_snapshot_policy_shape;
ALTER TABLE pm.run_budget_snapshots ADD CONSTRAINT run_budget_snapshot_policy_shape CHECK (
    jsonb_typeof(snapshot->'policy') = 'object'
    AND (snapshot->'policy') ?& ARRAY[
        'pending', 'run_lifetime', 'input_max_bytes', 'workspace_active_runs',
        'workflow_payload_bytes', 'workflow_fanout'
    ]::text[]
    AND ((snapshot->'policy') - ARRAY[
        'pending', 'run_lifetime', 'input_max_bytes', 'workspace_active_runs',
        'workflow_payload_bytes', 'workflow_fanout', 'http'
    ]::text[]) = '{}'::jsonb
    AND jsonb_typeof(snapshot->'policy'->'pending') = 'object'
    AND (snapshot->'policy'->'pending') ?& ARRAY[
        'global_pending', 'workspace_pending', 'workspace_max_pending_age'
    ]::text[]
    AND ((snapshot->'policy'->'pending') - ARRAY[
        'global_pending', 'workspace_pending', 'workspace_max_pending_age'
    ]::text[]) = '{}'::jsonb
);
ALTER TABLE pm.run_budget_snapshots ADD CONSTRAINT run_budget_snapshot_http_bounds CHECK (
    CASE
        WHEN NOT ((snapshot->'policy') ? 'http') THEN true
        WHEN jsonb_typeof(snapshot->'policy'->'http') = 'object' THEN
            (snapshot->'policy'->'http') ?& ARRAY[
                'max_request_bytes', 'max_response_bytes', 'max_dns_answers',
                'max_attempts', 'max_retry_delay', 'selected_origin'
            ]::text[]
            AND ((snapshot->'policy'->'http') - ARRAY[
                'max_request_bytes', 'max_response_bytes', 'max_dns_answers',
                'max_attempts', 'max_retry_delay', 'selected_origin'
            ]::text[]) = '{}'::jsonb
            AND jsonb_typeof(snapshot->'policy'->'http'->'selected_origin') = 'string'
            AND octet_length(snapshot->'policy'->'http'->>'selected_origin') BETWEEN 1 AND 4096
            AND CASE
                WHEN jsonb_typeof(snapshot->'policy'->'http'->'max_request_bytes') = 'number'
                 AND jsonb_typeof(snapshot->'policy'->'http'->'max_response_bytes') = 'number'
                 AND jsonb_typeof(snapshot->'policy'->'http'->'max_dns_answers') = 'number'
                 AND jsonb_typeof(snapshot->'policy'->'http'->'max_attempts') = 'number'
                 AND jsonb_typeof(snapshot->'policy'->'http'->'max_retry_delay') = 'number'
                THEN (snapshot->'policy'->'http'->>'max_request_bytes')::numeric BETWEEN 1 AND 9223372036854775806
                 AND (snapshot->'policy'->'http'->>'max_response_bytes')::numeric BETWEEN 1 AND 9223372036854775806
                 AND (snapshot->'policy'->'http'->>'max_dns_answers')::numeric BETWEEN 1 AND 4294967295
                 AND (snapshot->'policy'->'http'->>'max_attempts')::numeric BETWEEN 1 AND 4294967295
                 AND (snapshot->'policy'->'http'->>'max_retry_delay')::numeric BETWEEN 1 AND 9223372036854775807
                 AND trunc((snapshot->'policy'->'http'->>'max_request_bytes')::numeric) = (snapshot->'policy'->'http'->>'max_request_bytes')::numeric
                 AND trunc((snapshot->'policy'->'http'->>'max_response_bytes')::numeric) = (snapshot->'policy'->'http'->>'max_response_bytes')::numeric
                 AND trunc((snapshot->'policy'->'http'->>'max_dns_answers')::numeric) = (snapshot->'policy'->'http'->>'max_dns_answers')::numeric
                 AND trunc((snapshot->'policy'->'http'->>'max_attempts')::numeric) = (snapshot->'policy'->'http'->>'max_attempts')::numeric
                 AND trunc((snapshot->'policy'->'http'->>'max_retry_delay')::numeric) = (snapshot->'policy'->'http'->>'max_retry_delay')::numeric
                ELSE false
            END
        ELSE false
    END
);
ALTER TABLE pm.control_schema DROP CONSTRAINT control_schema_revision_check;
UPDATE pm.control_schema SET revision = 'pm.control/v15', installed_at = clock_timestamp()
WHERE singleton AND revision = 'pm.control/v14';
ALTER TABLE pm.control_schema
    ADD CONSTRAINT control_schema_revision_check CHECK (revision = 'pm.control/v15');
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
CREATE POLICY pm_http_budget_migration_select ON pm.run_budget_snapshots
    FOR SELECT TO pm_app_migrator USING (true);
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pm.run_budget_snapshots WHERE (snapshot->'policy') ? 'http') THEN
        RAISE EXCEPTION 'cannot roll back retained HTTP execution budgets';
    END IF;
END
$$;
DROP POLICY pm_http_budget_migration_select ON pm.run_budget_snapshots;
ALTER TABLE pm.run_budget_snapshots DROP CONSTRAINT run_budget_snapshot_http_bounds;
ALTER TABLE pm.run_budget_snapshots DROP CONSTRAINT run_budget_snapshot_policy_shape;
ALTER TABLE pm.run_budget_snapshots ADD CONSTRAINT run_budget_snapshot_policy_shape CHECK (
    jsonb_typeof(snapshot->'policy') = 'object'
    AND (snapshot->'policy') ?& ARRAY[
        'pending', 'run_lifetime', 'input_max_bytes', 'workspace_active_runs',
        'workflow_payload_bytes', 'workflow_fanout'
    ]::text[]
    AND ((snapshot->'policy') - ARRAY[
        'pending', 'run_lifetime', 'input_max_bytes', 'workspace_active_runs',
        'workflow_payload_bytes', 'workflow_fanout'
    ]::text[]) = '{}'::jsonb
    AND jsonb_typeof(snapshot->'policy'->'pending') = 'object'
    AND (snapshot->'policy'->'pending') ?& ARRAY[
        'global_pending', 'workspace_pending', 'workspace_max_pending_age'
    ]::text[]
    AND ((snapshot->'policy'->'pending') - ARRAY[
        'global_pending', 'workspace_pending', 'workspace_max_pending_age'
    ]::text[]) = '{}'::jsonb
);
ALTER TABLE pm.control_schema DROP CONSTRAINT control_schema_revision_check;
UPDATE pm.control_schema SET revision = 'pm.control/v14', installed_at = clock_timestamp()
WHERE singleton AND revision = 'pm.control/v15';
ALTER TABLE pm.control_schema
    ADD CONSTRAINT control_schema_revision_check CHECK (revision = 'pm.control/v14');
-- +goose StatementEnd
