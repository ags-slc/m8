# m8

PostgreSQL migration tool. Mate for your database.

m8 is a PostgreSQL-specific migration tool built as a single static Go binary. It organizes migrations into four directories -- **schema**, **logic**, **permissions**, and **ops** -- each with behavior matched to the type of database object it manages.

## Why m8?

Every existing migration tool requires escape hatches for real-world PostgreSQL deployments. m8 was built after evaluating pg_schema, Atlas, pgroll, Sqitch, Flyway, dbmate, and golang-migrate -- and finding that none could handle the full surface area of a PostgreSQL database using TimescaleDB, pg_cron, extensions, and CDC-replicated schemas without a sidecar system.

### How m8 compares

| Feature | m8 | pg_schema | Atlas | pgroll | Sqitch | Flyway | dbmate |
|---------|-----|-----------|-------|--------|--------|--------|--------|
| Declarative schema diffing | Yes | Yes | Yes | No | No | No | No |
| Native SQL (no DSL) | Yes | Yes | Partial (HCL) | No (JSON) | Yes | Yes | Yes |
| Extensions / TimescaleDB | Yes | No | Pro only | Escape hatch | Yes | Yes | Yes |
| Repeatable migrations | Yes | No | No | No | No | Yes | No |
| Advisory locking | Yes | No | No | No | Yes | No | No |
| Checksum tracking | Yes | No | Yes | No | Yes | Yes | No |
| Single static binary | Yes | Yes | Yes | Yes | No (Perl) | No (JVM) | Yes |
| Fully open source | Yes | Yes | Open-core | Yes | Yes | Open-core | Yes |
| PostgreSQL-only | Yes | Yes | No (16+ DBs) | Yes | No | No | No |

### What's wrong with the alternatives?

**pg_schema** cannot manage `CREATE EXTENSION`, `CREATE SCHEMA`, `CREATE ROLE`, pg_cron, or any TimescaleDB DDL. No custom SQL hooks. Would cover ~40% of the migration surface area.

**Atlas** is open-core with an accelerating paywall -- extension support is Pro-only, linting moved to paid in Oct 2025. Multi-database design means PostgreSQL-specific features aren't first-class.

**pgroll** requires all migrations in JSON/YAML DSL. The raw SQL escape hatch loses zero-downtime guarantees. Write amplification from bidirectional triggers. Overkill for databases that don't need rolling deployments.

**Sqitch** has the best design of any migration tool -- but it's written in Perl with a deep CPAN dependency tree. Homebrew installs break on Perl upgrades. No dry-run mode. No single-binary distribution.

**Flyway** requires a JVM. Undo/rollback is paywalled. Multi-database, so PostgreSQL-specific features aren't prioritized.

**dbmate** and **golang-migrate** are simple and lightweight but lack declarative diffing, repeatable migrations, advisory locking, and checksums.

### What m8 takes from each

| Source | What m8 adopted |
|--------|----------------|
| **Sqitch** | Native SQL, advisory locking, in-database state tracking, named targets |
| **Flyway** | Repeatable migrations (re-run on content change) |
| **pg-schema-diff** (Stripe) | Auto `CREATE INDEX CONCURRENTLY`, `NOT VALID` constraints, hazard warnings |
| **dbmate** | `.env` file loading, clean CLI design |
| **pgroll** | Brownfield adoption (`m8 sync`), in-database state with no external files |

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
m8 sync --database mydb --user postgres

# Mark all files as applied without running them
m8 baseline --all --database mydb --user postgres

# Scaffold a new migration file
m8 new schema public/users
m8 new logic proc_refresh_invoices
m8 new permissions grants_readonly
m8 new ops "create extensions"
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

### Safe by Default

m8 only manages objects you declare. Tables, procedures, and grants that exist in the database but aren't in your migration files are never touched -- never dropped, never altered.

Use `--strict` to opt in to exact-match mode, where schema diffs include DROP statements for undeclared objects.

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

m8 also auto-detects `CREATE INDEX CONCURRENTLY` and runs those migrations outside a transaction automatically.

## Commands

| Command | Description |
|---------|-------------|
| `m8 apply` | Apply pending migrations (ops → schema → logic → permissions) |
| `m8 plan` | Show what would be applied without making changes (exit code 2 if pending) |
| `m8 status` | Show applied, pending, changed, and drifted migrations |
| `m8 sync` | One-time convergence for brownfield adoption |
| `m8 baseline` | Mark migrations as applied without running them |
| `m8 new` | Scaffold a new migration file in the correct folder |
| `m8 version` | Print version information |

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

## License

Apache License 2.0 -- see [LICENSE](LICENSE).
