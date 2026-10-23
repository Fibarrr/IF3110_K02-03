CREATE TABLE IF NOT EXISTS ac_users (
    id BIGSERIAL PRIMARY KEY,
    username TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    is_platform_admin BOOLEAN NOT NULL DEFAULT FALSE,
    disabled BOOLEAN NOT NULL DEFAULT FALSE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS ac_groups (
    id BIGSERIAL PRIMARY KEY,
    name TEXT NOT NULL UNIQUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS ac_group_members (
    group_id BIGINT NOT NULL REFERENCES ac_groups(id) ON DELETE CASCADE,
    user_id BIGINT NOT NULL REFERENCES ac_users(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (group_id, user_id)
);

CREATE TABLE IF NOT EXISTS ac_database_grants (
    group_id BIGINT NOT NULL REFERENCES ac_groups(id) ON DELETE CASCADE,
    database_id TEXT NOT NULL,
    access_level TEXT NOT NULL CHECK (access_level IN ('view_only', 'view_edit')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (group_id, database_id)
);

CREATE TABLE IF NOT EXISTS ac_sessions (
    id BIGSERIAL PRIMARY KEY,
    user_id BIGINT NOT NULL REFERENCES ac_users(id) ON DELETE CASCADE,
    token_hash BYTEA NOT NULL UNIQUE,
    previous_token_hash BYTEA,
    previous_valid_until TIMESTAMPTZ,
    csrf_token TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    rotated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at TIMESTAMPTZ NOT NULL,
    revoked_at TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS ac_sessions_user_id_idx ON ac_sessions(user_id);
CREATE INDEX IF NOT EXISTS ac_group_members_user_id_idx ON ac_group_members(user_id);
CREATE INDEX IF NOT EXISTS ac_database_grants_database_id_idx ON ac_database_grants(database_id);
