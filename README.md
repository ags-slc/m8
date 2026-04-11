# m8

PostgreSQL migration tool. Mate for your database.

m8 is a PostgreSQL-specific migration tool with four migration types -- schema, logic, permissions, and ops -- built as a single static binary.

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

## Migration Layout

Organize SQL files in four directories:

```
migrations/
├── schema/                                    # Desired state (auto-diffed)
│   ├── public/                                # ← PostgreSQL schema name
│   │   ├── users.sql
│   │   └── orders.sql
│   └── materialized/
│       ├── rpt_invoice_detail.sql
│       └── rpt_order_detail.sql
├── logic/                                     # Re-applied on change
│   ├── proc_refresh_invoice_detail.sql
│   ├── proc_refresh_invoice_summary.sql
│   └── view_payout_summary.sql
├── permissions/                               # Re-applied on change
│   ├── grants_materialized.sql
│   └── default_privileges.sql
└── ops/                                       # One-time, sequential
    ├── 20260411_001__create_extensions.sql
    └── 20260411_002__create_hypertables.sql
```

### Migration Types

| Type | Folder | Behavior | Use for |
|------|--------|----------|---------|
| **Schema** | `schema/{pg_schema}/` | Desired state diffed against live DB per PG schema | Tables, indexes, constraints |
| **Logic** | `logic/` | Re-run whenever file content changes | Procedures, functions, views, triggers, pg_cron |
| **Permissions** | `permissions/` | Re-run whenever file content changes | Grants, revokes, roles, default privileges |
| **Ops** | `ops/` | Run once in timestamp order | Extensions, hypertables, data migrations |

**Apply order:** Ops → Schema → Logic → Permissions

### Schema Migrations

The `schema/` folder mirrors your PostgreSQL schemas. Each subfolder targets a specific PG schema:

```sql
-- schema/public/users.sql
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
-- schema/public/users.sql (updated)
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
Plan: 1 migration(s) to apply.

  ~ schema/public/users.sql (schema)
    ALTER TABLE "public"."users" ADD COLUMN "phone" text
    CREATE INDEX CONCURRENTLY idx_users_phone ON public.users USING btree (phone)
    ⚠ INDEX_BUILD
```

### Legacy Flat Layout

m8 also supports a flat directory with prefixed filenames for backward compatibility:

```
migrations/
├── V20260411_001__create_extensions.sql    # V__ = ops
├── S__users.sql                           # S__ = schema (defaults to public)
└── R__grants.sql                          # R__ = logic
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
