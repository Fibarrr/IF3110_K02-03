-- Serialize startup DDL across concurrent cmd/api and cmd/worker processes.
SELECT pg_advisory_xact_lock(311032);

CREATE TABLE IF NOT EXISTS mg_migrations (
    id BIGSERIAL PRIMARY KEY,
    database_id TEXT NOT NULL REFERENCES dm_databases(id) ON DELETE CASCADE,
    version BIGINT NOT NULL,
    name TEXT NOT NULL,
    sql_text TEXT NOT NULL DEFAULT '',
    up_sql TEXT NOT NULL DEFAULT '',
    down_sql TEXT NOT NULL DEFAULT '',
    reversible BOOLEAN NOT NULL DEFAULT TRUE,
    irreversible_reason TEXT NOT NULL DEFAULT '',
    execution_mode TEXT NOT NULL DEFAULT 'standard',
    batch_config JSONB NOT NULL DEFAULT '{}'::jsonb,
    current_revision INT NOT NULL DEFAULT 1,
    has_executed BOOLEAN NOT NULL DEFAULT FALSE,
    active_execution_id BIGINT,
    status TEXT NOT NULL DEFAULT 'draft',
    created_by BIGINT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    executed_by BIGINT,
    executed_at TIMESTAMPTZ,
    last_error TEXT NOT NULL DEFAULT '',
    UNIQUE (database_id, version)
);

-- Upgrade existing Milestone 1 tables safely without losing data.
ALTER TABLE mg_migrations ADD COLUMN IF NOT EXISTS up_sql TEXT NOT NULL DEFAULT '';
ALTER TABLE mg_migrations ADD COLUMN IF NOT EXISTS down_sql TEXT NOT NULL DEFAULT '';
ALTER TABLE mg_migrations ADD COLUMN IF NOT EXISTS reversible BOOLEAN NOT NULL DEFAULT TRUE;
ALTER TABLE mg_migrations ADD COLUMN IF NOT EXISTS irreversible_reason TEXT NOT NULL DEFAULT '';
ALTER TABLE mg_migrations ADD COLUMN IF NOT EXISTS execution_mode TEXT NOT NULL DEFAULT 'standard';
ALTER TABLE mg_migrations ADD COLUMN IF NOT EXISTS batch_config JSONB NOT NULL DEFAULT '{}'::jsonb;
ALTER TABLE mg_migrations ADD COLUMN IF NOT EXISTS current_revision INT NOT NULL DEFAULT 1;
ALTER TABLE mg_migrations ADD COLUMN IF NOT EXISTS has_executed BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE mg_migrations ADD COLUMN IF NOT EXISTS active_execution_id BIGINT;

UPDATE mg_migrations SET up_sql = sql_text WHERE up_sql = '' AND sql_text <> '';
UPDATE mg_migrations SET has_executed = TRUE WHERE status IN ('applied', 'failed') AND has_executed = FALSE;

DO $$
DECLARE
    constraint_rec RECORD;
    has_m2_status BOOLEAN := FALSE;
    has_m2_mode BOOLEAN := FALSE;
BEGIN
    SELECT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'mg_migrations'::regclass AND conname = 'mg_migrations_status_check'
          AND pg_get_constraintdef(oid) LIKE '%partially_reverted%'
    ) INTO has_m2_status;

    IF NOT has_m2_status THEN
        FOR constraint_rec IN
            SELECT conname FROM pg_constraint
            WHERE conrelid = 'mg_migrations'::regclass AND contype = 'c'
              AND pg_get_constraintdef(oid) LIKE '%status%'
        LOOP
            EXECUTE format('ALTER TABLE mg_migrations DROP CONSTRAINT IF EXISTS %I', constraint_rec.conname);
        END LOOP;
        ALTER TABLE mg_migrations
            ADD CONSTRAINT mg_migrations_status_check
            CHECK (status IN (
                'draft', 'pending_review', 'approved', 'queued', 'running',
                'applied', 'rolled_back', 'failed', 'partial', 'partially_reverted', 'uncertain'
            ));
    END IF;

    SELECT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'mg_migrations'::regclass AND conname = 'mg_migrations_mode_check'
    ) INTO has_m2_mode;
    IF NOT has_m2_mode THEN
        ALTER TABLE mg_migrations
            ADD CONSTRAINT mg_migrations_mode_check
            CHECK (execution_mode IN ('standard', 'batch'));
    END IF;
END $$;

CREATE INDEX IF NOT EXISTS mg_migrations_history_idx ON mg_migrations (database_id, version DESC);

CREATE TABLE IF NOT EXISTS mg_migration_revisions (
    id BIGSERIAL PRIMARY KEY,
    migration_id BIGINT NOT NULL REFERENCES mg_migrations(id) ON DELETE CASCADE,
    revision INT NOT NULL,
    name TEXT NOT NULL,
    up_sql TEXT NOT NULL,
    down_sql TEXT NOT NULL DEFAULT '',
    reversible BOOLEAN NOT NULL DEFAULT TRUE,
    irreversible_reason TEXT NOT NULL DEFAULT '',
    execution_mode TEXT NOT NULL DEFAULT 'standard' CHECK (execution_mode IN ('standard', 'batch')),
    batch_config JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_by BIGINT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (migration_id, revision)
);

INSERT INTO mg_migration_revisions (
    migration_id, revision, name, up_sql, down_sql, reversible, irreversible_reason, execution_mode, batch_config, created_by, created_at
)
SELECT id, current_revision, name, up_sql, down_sql, reversible, irreversible_reason, execution_mode, batch_config, created_by, created_at
FROM mg_migrations m
WHERE NOT EXISTS (
    SELECT 1 FROM mg_migration_revisions r
    WHERE r.migration_id = m.id AND r.revision = m.current_revision
);

CREATE TABLE IF NOT EXISTS mg_review_requests (
    id BIGSERIAL PRIMARY KEY,
    database_id TEXT NOT NULL REFERENCES dm_databases(id) ON DELETE CASCADE,
    migration_id BIGINT NOT NULL REFERENCES mg_migrations(id) ON DELETE CASCADE,
    revision INT NOT NULL,
    operation TEXT NOT NULL CHECK (operation IN ('apply', 'rollback', 'forward', 'resume', 'cancel_batch')),
    target_execution_id BIGINT,
    target_job_id BIGINT,
    requested_by BIGINT NOT NULL,
    requested_by_username TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL CHECK (status IN ('pending_review', 'approved', 'rejected', 'invalidated', 'queued', 'completed')),
    comment TEXT NOT NULL DEFAULT '',
    invalidation_reason TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS mg_review_requests_db_idx ON mg_review_requests (database_id, created_at DESC);

CREATE TABLE IF NOT EXISTS mg_review_approvals (
    request_id BIGINT NOT NULL REFERENCES mg_review_requests(id) ON DELETE CASCADE,
    group_id BIGINT NOT NULL,
    group_name TEXT NOT NULL,
    access_level TEXT NOT NULL,
    leader_user_id BIGINT NOT NULL,
    leader_username TEXT NOT NULL,
    decision TEXT NOT NULL CHECK (decision IN ('pending', 'approve', 'reject')),
    comment TEXT NOT NULL DEFAULT '',
    decided_by BIGINT,
    decided_at TIMESTAMPTZ,
    PRIMARY KEY (request_id, group_id)
);

CREATE TABLE IF NOT EXISTS mg_review_audit_events (
    id BIGSERIAL PRIMARY KEY,
    request_id BIGINT NOT NULL REFERENCES mg_review_requests(id) ON DELETE CASCADE,
    database_id TEXT NOT NULL,
    group_id BIGINT,
    actor_user_id BIGINT NOT NULL,
    actor_username TEXT NOT NULL DEFAULT '',
    event_type TEXT NOT NULL,
    decision TEXT NOT NULL DEFAULT '',
    comment TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS mg_review_audit_req_idx ON mg_review_audit_events (request_id, id ASC);

CREATE TABLE IF NOT EXISTS mg_jobs (
    id BIGSERIAL PRIMARY KEY,
    database_id TEXT NOT NULL REFERENCES dm_databases(id) ON DELETE CASCADE,
    migration_id BIGINT NOT NULL REFERENCES mg_migrations(id) ON DELETE CASCADE,
    review_request_id BIGINT NOT NULL REFERENCES mg_review_requests(id) ON DELETE CASCADE,
    revision INT NOT NULL,
    operation TEXT NOT NULL CHECK (operation IN ('apply', 'rollback', 'forward', 'resume', 'cancel_batch')),
    target_execution_id BIGINT,
    parent_job_id BIGINT,
    root_job_id BIGINT,
    requested_by BIGINT NOT NULL,
    requested_by_username TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL CHECK (status IN ('queued', 'running', 'succeeded', 'failed', 'partial', 'partially_reverted', 'uncertain')),
    idempotency_key TEXT NOT NULL UNIQUE,
    attempt INT NOT NULL DEFAULT 0,
    max_attempts INT NOT NULL DEFAULT 3,
    next_run_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    worker_id TEXT NOT NULL DEFAULT '',
    total_rows_estimated BIGINT NOT NULL DEFAULT 0,
    committed_batches INT NOT NULL DEFAULT 0,
    committed_rows BIGINT NOT NULL DEFAULT 0,
    last_cursor BIGINT NOT NULL DEFAULT 0,
    max_cursor BIGINT NOT NULL DEFAULT 0,
    error_summary TEXT NOT NULL DEFAULT '',
    trace_context JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    started_at TIMESTAMPTZ,
    finished_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

ALTER TABLE mg_jobs ADD COLUMN IF NOT EXISTS root_job_id BIGINT;
UPDATE mg_jobs SET root_job_id = id WHERE root_job_id IS NULL;

CREATE INDEX IF NOT EXISTS mg_jobs_queue_idx ON mg_jobs (status, next_run_at, id);
CREATE INDEX IF NOT EXISTS mg_jobs_db_idx ON mg_jobs (database_id, created_at DESC);
CREATE INDEX IF NOT EXISTS mg_jobs_root_idx ON mg_jobs (root_job_id);

CREATE TABLE IF NOT EXISTS mg_executions (
    id BIGSERIAL PRIMARY KEY,
    database_id TEXT NOT NULL REFERENCES dm_databases(id) ON DELETE CASCADE,
    migration_id BIGINT NOT NULL REFERENCES mg_migrations(id) ON DELETE CASCADE,
    job_id BIGINT REFERENCES mg_jobs(id) ON DELETE SET NULL,
    review_request_id BIGINT REFERENCES mg_review_requests(id) ON DELETE SET NULL,
    migration_version BIGINT NOT NULL,
    migration_name TEXT NOT NULL,
    revision INT NOT NULL,
    operation TEXT NOT NULL CHECK (operation IN ('apply', 'rollback', 'forward', 'resume', 'cancel_batch')),
    target_execution_id BIGINT,
    attempt INT NOT NULL DEFAULT 1,
    executed_by BIGINT NOT NULL,
    executed_by_username TEXT NOT NULL DEFAULT '',
    sql_executed TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('running', 'succeeded', 'failed', 'partial', 'partially_reverted', 'uncertain')),
    committed_batches INT NOT NULL DEFAULT 0,
    committed_rows BIGINT NOT NULL DEFAULT 0,
    error_summary TEXT NOT NULL DEFAULT '',
    started_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    finished_at TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS mg_executions_db_idx ON mg_executions (database_id, id ASC);

CREATE TABLE IF NOT EXISTS mg_batch_checkpoints (
    job_id BIGINT NOT NULL REFERENCES mg_jobs(id) ON DELETE CASCADE,
    execution_id BIGINT NOT NULL REFERENCES mg_executions(id) ON DELETE CASCADE,
    batch_no INT NOT NULL,
    direction TEXT NOT NULL CHECK (direction IN ('up', 'down')),
    start_cursor BIGINT NOT NULL,
    end_cursor BIGINT NOT NULL,
    rows_affected BIGINT NOT NULL DEFAULT 0,
    status TEXT NOT NULL CHECK (status IN ('committed', 'reverted')),
    committed_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (job_id, direction, batch_no)
);

CREATE TABLE IF NOT EXISTS mg_db_locks (
    database_id TEXT PRIMARY KEY REFERENCES dm_databases(id) ON DELETE CASCADE,
    owner_worker_id TEXT NOT NULL,
    job_id BIGINT NOT NULL,
    acquired_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    heartbeat_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    lease_until TIMESTAMPTZ NOT NULL
);
