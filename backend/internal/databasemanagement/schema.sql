CREATE TABLE IF NOT EXISTS dm_databases (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL UNIQUE,
    host TEXT NOT NULL,
    port INTEGER NOT NULL DEFAULT 5432 CHECK (port BETWEEN 1 AND 65535),
    database_name TEXT NOT NULL,
    username TEXT NOT NULL,
    password TEXT NOT NULL DEFAULT '',
    ssl_mode TEXT NOT NULL DEFAULT 'disable' CHECK (ssl_mode IN ('disable', 'require', 'verify-ca', 'verify-full')),
    description TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
