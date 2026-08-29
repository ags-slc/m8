CREATE SCHEMA IF NOT EXISTS _m8;

CREATE TABLE IF NOT EXISTS _m8.history (
    id            BIGSERIAL PRIMARY KEY,
    version       TEXT,
    name          TEXT NOT NULL,
    type          TEXT NOT NULL CONSTRAINT m8_history_type_check
                      CHECK (type IN ('ops', 'schema', 'logic', 'permissions', 'data')),
    pg_schema     TEXT,
    checksum      TEXT NOT NULL,
    applied_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    execution_ms  BIGINT NOT NULL DEFAULT 0,
    applied_by    TEXT NOT NULL DEFAULT current_user,
    success       BOOLEAN NOT NULL DEFAULT true
);

-- Widen the type CHECK on installs created before data/ existed. Those carry
-- PostgreSQL's auto-generated `history_type_check`, which rejects 'data' and
-- would fail the first data/ migration's history row -- after the migration had
-- already run. Keyed on the NAMED constraint so this is a no-op forever after.
--
-- The guard is a probe followed by a mutation, so two m8 processes can both
-- pass it: `status` calls EnsureSchema without the advisory lock that `apply`
-- takes, and the upgrade window is exactly when everyone runs both. The loser
-- blocks on the winner's ALTER, then finds the constraint already there and
-- raises 42710, which would abort the whole bootstrap. Swallow that one error
-- -- it means the upgrade this block exists to perform has happened.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = '_m8.history'::regclass
          AND conname = 'm8_history_type_check'
    ) THEN
        ALTER TABLE _m8.history DROP CONSTRAINT IF EXISTS history_type_check;
        ALTER TABLE _m8.history ADD CONSTRAINT m8_history_type_check
            CHECK (type IN ('ops', 'schema', 'logic', 'permissions', 'data'));
    END IF;
EXCEPTION
    WHEN duplicate_object THEN NULL;
END $$;

CREATE UNIQUE INDEX IF NOT EXISTS idx_history_version_success
    ON _m8.history (version)
    WHERE success = true AND type = 'ops';

-- data/ versions are one-time too, and live in their own namespace: a data/
-- file may carry the same timestamp as an ops/ file without colliding.
CREATE UNIQUE INDEX IF NOT EXISTS idx_history_data_version_success
    ON _m8.history (version)
    WHERE success = true AND type = 'data';

CREATE INDEX IF NOT EXISTS idx_history_latest_by_name
    ON _m8.history (name, type, applied_at DESC)
    WHERE success = true;

COMMENT ON SCHEMA _m8 IS 'm8 migration state -- do not modify manually';
COMMENT ON TABLE _m8.history IS 'm8 migration history';
