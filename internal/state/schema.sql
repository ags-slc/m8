CREATE SCHEMA IF NOT EXISTS _m8;

CREATE TABLE IF NOT EXISTS _m8.history (
    id            BIGSERIAL PRIMARY KEY,
    version       TEXT,
    name          TEXT NOT NULL,
    type          TEXT NOT NULL CHECK (type IN ('ops', 'schema', 'logic', 'permissions')),
    pg_schema     TEXT,
    checksum      TEXT NOT NULL,
    applied_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    execution_ms  BIGINT NOT NULL DEFAULT 0,
    applied_by    TEXT NOT NULL DEFAULT current_user,
    success       BOOLEAN NOT NULL DEFAULT true
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_history_version_success
    ON _m8.history (version)
    WHERE success = true AND type = 'ops';

CREATE INDEX IF NOT EXISTS idx_history_latest_by_name
    ON _m8.history (name, type, applied_at DESC)
    WHERE success = true;

COMMENT ON SCHEMA _m8 IS 'm8 migration state -- do not modify manually';
COMMENT ON TABLE _m8.history IS 'm8 migration history';
