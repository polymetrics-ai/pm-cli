-- +goose Up

-- +goose StatementBegin
DO $$
BEGIN
    IF current_database() <> 'pm_app' OR current_user <> 'pm_app_migrator' THEN
        RAISE EXCEPTION 'control migration requires pm_app_migrator on pm_app';
    END IF;
    IF to_regrole('pm_app_runtime') IS NULL THEN
        RAISE EXCEPTION 'required runtime role is missing';
    END IF;
    IF EXISTS (
        SELECT 1
        FROM pg_roles
        WHERE rolname = 'pm_app_runtime'
          AND (rolsuper OR rolbypassrls OR rolcreatedb OR rolcreaterole OR rolreplication)
    ) THEN
        RAISE EXCEPTION 'runtime role has prohibited PostgreSQL authority';
    END IF;
    IF pg_has_role('pm_app_runtime', 'pm_app_migrator', 'MEMBER') THEN
        RAISE EXCEPTION 'runtime role must not inherit or assume migrator ownership';
    END IF;
END
$$;
-- +goose StatementEnd

REVOKE CREATE, TEMPORARY ON DATABASE pm_app FROM PUBLIC, pm_app_runtime;
REVOKE ALL ON SCHEMA pm FROM PUBLIC, pm_app_runtime;
ALTER DEFAULT PRIVILEGES FOR ROLE pm_app_migrator IN SCHEMA pm
    REVOKE ALL ON TABLES FROM PUBLIC, pm_app_runtime;
ALTER DEFAULT PRIVILEGES FOR ROLE pm_app_migrator IN SCHEMA pm
    REVOKE ALL ON SEQUENCES FROM PUBLIC, pm_app_runtime;
GRANT USAGE ON SCHEMA pm TO pm_app_runtime;

CREATE TABLE pm.control_schema (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    revision text NOT NULL CHECK (revision = 'pm.control/v1'),
    installed_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

INSERT INTO pm.control_schema (singleton, revision)
VALUES (true, 'pm.control/v1');

CREATE TABLE pm.run_specs (
    org_id text NOT NULL,
    workspace_id text NOT NULL,
    run_id text NOT NULL,
    intent_digest text NOT NULL CHECK (intent_digest ~ '^sha256:[0-9a-f]{64}$'),
    idempotency_fingerprint text NOT NULL CHECK (idempotency_fingerprint ~ '^sha256:[0-9a-f]{64}$'),
    spec jsonb NOT NULL CHECK (jsonb_typeof(spec) = 'object'),
    accepted_at timestamptz NOT NULL,
    PRIMARY KEY (org_id, workspace_id, run_id)
);

CREATE TABLE pm.runs (
    org_id text NOT NULL,
    workspace_id text NOT NULL,
    run_id text NOT NULL,
    state text NOT NULL CHECK (state IN (
        'accepted', 'queued', 'running', 'awaiting_approval', 'blocked',
        'reconciling', 'succeeded', 'partial', 'failed', 'cancelled'
    )),
    revision bigint NOT NULL CHECK (revision > 0),
    record jsonb NOT NULL CHECK (jsonb_typeof(record) = 'object'),
    updated_at timestamptz NOT NULL,
    PRIMARY KEY (org_id, workspace_id, run_id),
    FOREIGN KEY (org_id, workspace_id, run_id)
        REFERENCES pm.run_specs (org_id, workspace_id, run_id) ON DELETE CASCADE
);

CREATE TABLE pm.admission_keys (
    org_id text NOT NULL,
    workspace_id text NOT NULL,
    principal_id text NOT NULL,
    operation_id text NOT NULL,
    target_id text NOT NULL,
    idempotency_key text NOT NULL,
    intent_digest text NOT NULL CHECK (intent_digest ~ '^sha256:[0-9a-f]{64}$'),
    run_id text NOT NULL,
    accepted_at timestamptz NOT NULL,
    PRIMARY KEY (org_id, workspace_id, principal_id, operation_id, target_id, idempotency_key),
    UNIQUE (org_id, workspace_id, run_id),
    FOREIGN KEY (org_id, workspace_id, run_id)
        REFERENCES pm.run_specs (org_id, workspace_id, run_id) ON DELETE CASCADE
);

CREATE TABLE pm.pending_reservations (
    org_id text NOT NULL,
    workspace_id text NOT NULL,
    run_id text NOT NULL,
    accepted_at timestamptz NOT NULL,
    released_at timestamptz,
    release_reason text,
    PRIMARY KEY (org_id, workspace_id, run_id),
    CHECK ((released_at IS NULL AND release_reason IS NULL) OR (released_at IS NOT NULL AND release_reason IS NOT NULL)),
    FOREIGN KEY (org_id, workspace_id, run_id)
        REFERENCES pm.run_specs (org_id, workspace_id, run_id) ON DELETE CASCADE
);

CREATE TABLE pm.tasks (
    org_id text NOT NULL,
    workspace_id text NOT NULL,
    run_id text NOT NULL,
    task_id text NOT NULL,
    state text NOT NULL CHECK (state IN (
        'accepted', 'queued', 'running', 'awaiting_approval', 'blocked',
        'reconciling', 'succeeded', 'partial', 'failed', 'cancelled'
    )),
    revision bigint NOT NULL CHECK (revision > 0),
    task jsonb NOT NULL CHECK (jsonb_typeof(task) = 'object'),
    updated_at timestamptz NOT NULL,
    PRIMARY KEY (org_id, workspace_id, run_id, task_id),
    FOREIGN KEY (org_id, workspace_id, run_id)
        REFERENCES pm.runs (org_id, workspace_id, run_id) ON DELETE CASCADE
);

CREATE TABLE pm.task_leases (
    org_id text NOT NULL,
    workspace_id text NOT NULL,
    run_id text NOT NULL,
    task_id text NOT NULL,
    owner_id text NOT NULL,
    fence bigint NOT NULL CHECK (fence > 0),
    claimed_at timestamptz NOT NULL,
    lease_expires_at timestamptz NOT NULL,
    PRIMARY KEY (org_id, workspace_id, run_id, task_id),
    CHECK (lease_expires_at > claimed_at),
    FOREIGN KEY (org_id, workspace_id, run_id, task_id)
        REFERENCES pm.tasks (org_id, workspace_id, run_id, task_id) ON DELETE CASCADE
);

CREATE TABLE pm.artifacts (
    org_id text NOT NULL,
    workspace_id text NOT NULL,
    artifact_id text NOT NULL,
    object_attempt_key text NOT NULL,
    sha256 text NOT NULL CHECK (sha256 ~ '^[0-9a-f]{64}$'),
    byte_length bigint NOT NULL CHECK (byte_length >= 0),
    media_type text NOT NULL,
    backend_generation text NOT NULL,
    object_ref jsonb NOT NULL CHECK (jsonb_typeof(object_ref) = 'object'),
    registered_at timestamptz NOT NULL,
    PRIMARY KEY (org_id, workspace_id, artifact_id),
    UNIQUE (org_id, workspace_id, object_attempt_key),
    UNIQUE (org_id, workspace_id, artifact_id, object_attempt_key)
);

CREATE TABLE pm.manifests (
    org_id text NOT NULL,
    workspace_id text NOT NULL,
    resource_id text NOT NULL,
    revision bigint NOT NULL CHECK (revision > 0),
    artifact_id text NOT NULL,
    object_attempt_key text NOT NULL,
    committed_at timestamptz NOT NULL,
    PRIMARY KEY (org_id, workspace_id, resource_id),
    FOREIGN KEY (org_id, workspace_id, artifact_id, object_attempt_key)
        REFERENCES pm.artifacts (org_id, workspace_id, artifact_id, object_attempt_key)
);

CREATE TABLE pm.effects (
    org_id text NOT NULL,
    workspace_id text NOT NULL,
    run_id text NOT NULL,
    task_id text NOT NULL,
    effect_digest text NOT NULL CHECK (effect_digest ~ '^sha256:[0-9a-f]{64}$'),
    state text NOT NULL CHECK (state IN ('unsent', 'succeeded', 'unchanged', 'rejected', 'unknown')),
    revision bigint NOT NULL CHECK (revision > 0),
    effect_record jsonb NOT NULL CHECK (jsonb_typeof(effect_record) = 'object'),
    updated_at timestamptz NOT NULL,
    PRIMARY KEY (org_id, workspace_id, run_id, task_id, effect_digest),
    FOREIGN KEY (org_id, workspace_id, run_id, task_id)
        REFERENCES pm.tasks (org_id, workspace_id, run_id, task_id) ON DELETE CASCADE
);

CREATE TABLE pm.dispatches (
    org_id text NOT NULL,
    workspace_id text NOT NULL,
    run_id text NOT NULL,
    task_id text NOT NULL,
    dispatch_id text NOT NULL,
    effect_digest text NOT NULL,
    state text NOT NULL CHECK (state IN ('prepared', 'possibly_dispatched')),
    intent jsonb NOT NULL CHECK (jsonb_typeof(intent) = 'object'),
    prepared_at timestamptz NOT NULL,
    possibly_dispatched_at timestamptz,
    PRIMARY KEY (org_id, workspace_id, run_id, task_id, dispatch_id),
    FOREIGN KEY (org_id, workspace_id, run_id, task_id, effect_digest)
        REFERENCES pm.effects (org_id, workspace_id, run_id, task_id, effect_digest) ON DELETE CASCADE
);

CREATE TABLE pm.dispatch_members (
    org_id text NOT NULL,
    workspace_id text NOT NULL,
    run_id text NOT NULL,
    task_id text NOT NULL,
    dispatch_id text NOT NULL,
    member_id text NOT NULL,
    ordinal integer NOT NULL CHECK (ordinal >= 0),
    PRIMARY KEY (org_id, workspace_id, run_id, task_id, dispatch_id, member_id),
    UNIQUE (org_id, workspace_id, run_id, task_id, dispatch_id, ordinal),
    FOREIGN KEY (org_id, workspace_id, run_id, task_id, dispatch_id)
        REFERENCES pm.dispatches (org_id, workspace_id, run_id, task_id, dispatch_id) ON DELETE CASCADE
);

CREATE TABLE pm.receipts (
    org_id text NOT NULL,
    workspace_id text NOT NULL,
    run_id text NOT NULL,
    receipt_id text NOT NULL,
    kind text NOT NULL,
    payload jsonb NOT NULL CHECK (jsonb_typeof(payload) = 'object'),
    recorded_at timestamptz NOT NULL,
    PRIMARY KEY (org_id, workspace_id, run_id, receipt_id),
    FOREIGN KEY (org_id, workspace_id, run_id)
        REFERENCES pm.runs (org_id, workspace_id, run_id) ON DELETE CASCADE
);

CREATE TABLE pm.dispatch_attempts (
    org_id text NOT NULL,
    workspace_id text NOT NULL,
    run_id text NOT NULL,
    task_id text NOT NULL,
    dispatch_id text NOT NULL,
    attempt_id text NOT NULL,
    receipt_id text NOT NULL,
    attempt jsonb NOT NULL CHECK (jsonb_typeof(attempt) = 'object'),
    recorded_at timestamptz NOT NULL,
    PRIMARY KEY (org_id, workspace_id, run_id, task_id, dispatch_id, attempt_id),
    FOREIGN KEY (org_id, workspace_id, run_id, task_id, dispatch_id)
        REFERENCES pm.dispatches (org_id, workspace_id, run_id, task_id, dispatch_id) ON DELETE CASCADE,
    FOREIGN KEY (org_id, workspace_id, run_id, receipt_id)
        REFERENCES pm.receipts (org_id, workspace_id, run_id, receipt_id)
);

CREATE TABLE pm.member_outcomes (
    org_id text NOT NULL,
    workspace_id text NOT NULL,
    run_id text NOT NULL,
    task_id text NOT NULL,
    dispatch_id text NOT NULL,
    member_id text NOT NULL,
    state text NOT NULL CHECK (state IN ('unsent', 'succeeded', 'unchanged', 'rejected', 'unknown')),
    outcome jsonb NOT NULL CHECK (jsonb_typeof(outcome) = 'object'),
    observed_at timestamptz NOT NULL,
    PRIMARY KEY (org_id, workspace_id, run_id, task_id, dispatch_id, member_id),
    FOREIGN KEY (org_id, workspace_id, run_id, task_id, dispatch_id, member_id)
        REFERENCES pm.dispatch_members (org_id, workspace_id, run_id, task_id, dispatch_id, member_id) ON DELETE CASCADE
);

CREATE TABLE pm.audit_entries (
    org_id text NOT NULL,
    workspace_id text NOT NULL,
    run_id text NOT NULL,
    audit_id text NOT NULL,
    requested_actor_id text NOT NULL,
    requested_actor_kind text NOT NULL,
    effective_actor_id text NOT NULL,
    effective_actor_kind text NOT NULL,
    action text NOT NULL,
    target_ref text NOT NULL,
    payload jsonb NOT NULL CHECK (jsonb_typeof(payload) = 'object'),
    recorded_at timestamptz NOT NULL,
    PRIMARY KEY (org_id, workspace_id, run_id, audit_id),
    FOREIGN KEY (org_id, workspace_id, run_id)
        REFERENCES pm.runs (org_id, workspace_id, run_id) ON DELETE CASCADE
);

CREATE TABLE pm.server_events (
    org_id text NOT NULL,
    workspace_id text NOT NULL,
    run_id text NOT NULL,
    sequence bigint NOT NULL CHECK (sequence > 0),
    event_id text NOT NULL,
    state_revision bigint NOT NULL CHECK (state_revision > 0),
    event_type text NOT NULL,
    safe_payload jsonb NOT NULL CHECK (jsonb_typeof(safe_payload) = 'object'),
    committed_at timestamptz NOT NULL,
    PRIMARY KEY (org_id, workspace_id, run_id, sequence),
    UNIQUE (org_id, workspace_id, run_id, event_id),
    FOREIGN KEY (org_id, workspace_id, run_id)
        REFERENCES pm.runs (org_id, workspace_id, run_id) ON DELETE CASCADE
);

CREATE TABLE pm.outbox_commands (
    org_id text NOT NULL,
    workspace_id text NOT NULL,
    run_id text NOT NULL,
    command_id text NOT NULL,
    kind text NOT NULL,
    payload_ref text NOT NULL,
    state text NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'dispatched', 'acknowledged', 'unknown')),
    created_at timestamptz NOT NULL,
    available_at timestamptz NOT NULL,
    PRIMARY KEY (org_id, workspace_id, run_id, command_id),
    FOREIGN KEY (org_id, workspace_id, run_id)
        REFERENCES pm.runs (org_id, workspace_id, run_id) ON DELETE CASCADE
);

CREATE INDEX task_leases_expiry_idx ON pm.task_leases (lease_expires_at);
CREATE INDEX dispatches_effect_idx ON pm.dispatches (org_id, workspace_id, run_id, task_id, effect_digest);
CREATE INDEX events_replay_idx ON pm.server_events (org_id, workspace_id, run_id, sequence);
CREATE INDEX outbox_pending_idx ON pm.outbox_commands (state, available_at) WHERE state = 'pending';

-- +goose StatementBegin
DO $$
DECLARE
    scoped_table text;
BEGIN
    FOREACH scoped_table IN ARRAY ARRAY[
        'run_specs', 'runs', 'admission_keys', 'pending_reservations', 'tasks',
        'task_leases', 'artifacts', 'manifests', 'effects', 'dispatches',
        'dispatch_members', 'receipts', 'dispatch_attempts', 'member_outcomes',
        'audit_entries', 'server_events', 'outbox_commands'
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
            ')',
            scoped_table
        );
        EXECUTE format('REVOKE ALL ON TABLE pm.%I FROM PUBLIC, pm_app_runtime', scoped_table);
        EXECUTE format('GRANT SELECT ON TABLE pm.%I TO pm_app_runtime', scoped_table);
    END LOOP;
END
$$;
-- +goose StatementEnd

GRANT INSERT ON pm.run_specs, pm.admission_keys, pm.artifacts,
    pm.dispatch_members, pm.receipts, pm.dispatch_attempts, pm.member_outcomes,
    pm.audit_entries, pm.server_events, pm.runs, pm.pending_reservations,
    pm.tasks, pm.task_leases, pm.manifests, pm.effects, pm.dispatches,
    pm.outbox_commands TO pm_app_runtime;
GRANT UPDATE (state, revision, record, updated_at) ON pm.runs TO pm_app_runtime;
GRANT UPDATE (released_at, release_reason) ON pm.pending_reservations TO pm_app_runtime;
GRANT UPDATE (state, revision, task, updated_at) ON pm.tasks TO pm_app_runtime;
GRANT UPDATE (owner_id, fence, claimed_at, lease_expires_at) ON pm.task_leases TO pm_app_runtime;
GRANT UPDATE (revision, artifact_id, object_attempt_key, committed_at) ON pm.manifests TO pm_app_runtime;
GRANT UPDATE (state, revision, effect_record, updated_at) ON pm.effects TO pm_app_runtime;
GRANT UPDATE (state, possibly_dispatched_at) ON pm.dispatches TO pm_app_runtime;
GRANT UPDATE (state, available_at) ON pm.outbox_commands TO pm_app_runtime;

REVOKE ALL ON TABLE pm.control_schema FROM PUBLIC, pm_app_runtime;
GRANT SELECT ON TABLE pm.control_schema TO pm_app_runtime;
REVOKE ALL ON TABLE pm.control_schema_migrations FROM PUBLIC, pm_app_runtime;
REVOKE ALL ON SEQUENCE pm.control_schema_migrations_id_seq FROM PUBLIC, pm_app_runtime;
