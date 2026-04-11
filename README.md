# m8

PostgreSQL migration tool. Mate for your database.

m8 is a PostgreSQL-specific migration tool with three migration types -- versioned, repeatable, and schema (declarative) -- built as a single static binary.

## Installation

### Homebrew (macOS/Linux)

```bash
brew install ags-slc/tap/m8
```

### Go

```bash
go install github.com/ags-slc/m8@latest
```

### Binary

Download from [GitHub Releases](https://github.com/ags-slc/m8/releases).

## Quick Start

```bash
# Show what would be applied
m8 plan --database mydb --user postgres

# Apply pending migrations
m8 apply --database mydb --user postgres

# Show migration status
m8 status --database mydb --user postgres

# Adopt m8 on an existing database
m8 baseline --all --database mydb --user postgres
```

## Migration Files

Place SQL files in a `migrations/` directory (configurable with `--migrations-dir`):

```
migrations/
├── V20260411_001__create_extensions.sql       # Versioned: run once, in order
├── V20260411_002__create_schemas.sql          # Versioned: run once, in order
├── S__users.sql                               # Schema: desired state, auto-diffed
├── S__orders.sql                              # Schema: desired state, auto-diffed
├── R__proc_refresh_invoice_detail.sql         # Repeatable: re-run on change
├── R__grants.sql                              # Repeatable: re-run on change
└── R__cron_schedules.sql                      # Repeatable: re-run on change
```

### Migration Types

| Type | Prefix | Behavior | Use for |
|------|--------|----------|---------|
| **Versioned** | `V{timestamp}__` | Run once in order, tracked by checksum | One-time DDL (extensions, schemas, data migrations) |
| **Schema** | `S__` | Desired state diffed against live DB, auto-generates ALTER | Tables, indexes, constraints (things that need ALTER) |
| **Repeatable** | `R__` | Re-run whenever file content changes | Procedures, functions, views, grants, pg_cron schedules |

**Apply order:** Versioned -> Schema -> Repeatable

### Schema Migrations (S__)

Edit the desired state, m8 figures out the diff:

```sql
-- S__users.sql
CREATE TABLE users (
    id         BIGSERIAL PRIMARY KEY,
    email      TEXT NOT NULL,
    name       TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_users_email ON users (email);
```

Add a column? Just edit the file:

```sql
-- S__users.sql (updated)
CREATE TABLE users (
    id         BIGSERIAL PRIMARY KEY,
    email      TEXT NOT NULL,
    name       TEXT,
    phone      TEXT,                               -- added
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_users_email ON users (email);
CREATE INDEX idx_users_phone ON users (phone);     -- added
```

m8 computes the minimal ALTER statements automatically using [pg-schema-diff](https://github.com/stripe/pg-schema-diff):

```
$ m8 plan
Schema migrations:
  ~ S__users.sql
    ALTER TABLE users ADD COLUMN phone TEXT;
    CREATE INDEX CONCURRENTLY idx_users_phone ON users (phone);
```

## SQL Directives

Control migration behavior with special comments:

```sql
-- m8:no-transaction
CREATE INDEX CONCURRENTLY idx_users_email ON users (email);
```

```sql
-- m8:lock-timeout 5s
ALTER TABLE large_table ADD COLUMN new_col TEXT;
```

## Environment Variables

m8 supports standard PostgreSQL environment variables and `.env` files:

| Variable | Flag | Default |
|----------|------|---------|
| `PGHOST` | `--host` | localhost |
| `PGPORT` | `--port` | 5432 |
| `PGDATABASE` | `--database` | -- |
| `PGUSER` | `--user` | -- |
| `PGPASSWORD` | `--password` | -- |
| `PGSSLMODE` | `--sslmode` | prefer |
| `DATABASE_URL` | `--database-url` | -- |

## Design

See the [Architecture Decision Record](https://github.com/ags-slc/data-hub/blob/main/docs/adr-002-custom-migration-tool-m8.md) for design rationale and alternatives analysis.

## License

Apache License 2.0 -- see [LICENSE](LICENSE).
