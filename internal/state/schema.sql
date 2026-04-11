CREATE SCHEMA IF NOT EXISTS _m8;

CREATE TABLE IF NOT EXISTS _m8.history (
    id            BIGSERIAL PRIMARY KEY,
    version       TEXT,
    name          TEXT NOT NULL,
    type          TEXT NOT NULL CHECK (type IN ('versioned', 'repeatable', 'schema')),
    checksum      TEXT NOT NULL,
    applied_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    execution_ms  BIGINT NOT NULL DEFAULT 0,
    applied_by    TEXT NOT NULL DEFAULT current_user,
    success       BOOLEAN NOT NULL DEFAULT true
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_history_version_success
    ON _m8.history (version)
    WHERE success = true AND type = 'versioned';

CREATE INDEX IF NOT EXISTS idx_history_repeatable_latest
    ON _m8.history (name, applied_at DESC)
    WHERE success = true AND type IN ('repeatable', 'schema');

COMMENT ON SCHEMA _m8 IS 'm8 migration state -- do not modify manually';
COMMENT ON TABLE _m8.history IS 'm8 migration history';
