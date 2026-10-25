CREATE TABLE IF NOT EXISTS mg_migrations (
    id BIGSERIAL PRIMARY KEY,
    database_id TEXT NOT NULL REFERENCES dm_databases(id) ON DELETE CASCADE,
    version BIGINT NOT NULL,
    name TEXT NOT NULL,
    sql_text TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('draft', 'running', 'applied', 'failed')),
    created_by BIGINT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    executed_by BIGINT,
    executed_at TIMESTAMPTZ,
    last_error TEXT NOT NULL DEFAULT '',
    UNIQUE (database_id, version)
);

CREATE INDEX IF NOT EXISTS mg_migrations_history_idx
    ON mg_migrations (database_id, version DESC);
