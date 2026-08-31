# m8

PostgreSQL migration tool. Mate for your database.

m8 is a PostgreSQL-specific migration tool built as a single static Go binary. It organizes migrations into five directories -- **schema**, **logic**, **permissions**, **ops**, and **data** -- each with behavior matched to the type of database object it manages.

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
| **Sqitch** | Native SQL, advisory locking, in-database state tracking |
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

#### Verify the download

Releases are signed with cosign keyless signing. The signature covers
`checksums.txt`, so verify the bundle, then check your archive against it:

```bash
cosign verify-blob \
  --bundle checksums.txt.sigstore.json \
  --certificate-identity-regexp 'https://github.com/ags-slc/m8/.*' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  checksums.txt

shasum -a 256 --ignore-missing -c checksums.txt   # sha256sum -c on Linux
```

Verifying the bundle alone only authenticates a checksums file you never
compared against, so both steps are needed. Verification requires **cosign >=
v2.4.2**: the signature is a single `.sigstore.json` bundle, not a `.pem`/`.sig`
pair. `v0.1.0` is unsigned.

## Quick Start

```bash
# Bootstrap from an existing database
m8 dump --database mydb --user postgres

# Show what would be applied
m8 plan --database mydb --user postgres

# Apply pending migrations
m8 apply --database mydb --user postgres

# Show migration status
m8 status --database mydb --user postgres

# One-time convergence for brownfield adoption
m8 sync --database mydb --user postgres

# Scaffold a new migration file
m8 new schema public/users
m8 new logic proc_refresh_invoices
m8 new permissions grants_readonly
m8 new ops "create extensions"
m8 new data "backfill destination country"
```

## Migration Layout

Organize SQL files in five directories:

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
├── ops/                                       # One-time, sequential -- runs FIRST
│   ├── 20260411_001__create_extensions.sql
│   └── 20260411_002__create_hypertables.sql
└── data/                                      # One-time, sequential -- runs LAST
    └── 20260412_001__backfill_country.sql
```

### Migration Types

| Type | Folder | Behavior | Use for |
|------|--------|----------|---------|
| **Schema** | `schema/{pg_schema}/` | Desired state diffed against live DB per PG schema | Tables, indexes, constraints |
| **Logic** | `logic/` | Re-run whenever file content changes | Procedures, functions, views, triggers, pg_cron |
| **Permissions** | `permissions/` | Re-run whenever file content changes | Grants, revokes, roles, default privileges |
| **Ops** | `ops/` | Run once in timestamp order, **before** schema/ | Extensions, hypertables, session/database settings |
| **Data** | `data/` | Run once in timestamp order, **after** everything | Backfills, seeds, one-time DML, comments on new objects |

**Apply order:** Ops → Schema → Logic → Permissions → Data

### ops/ or data/?

Both run once, in timestamp order, and are checksummed; they differ only in
*when*. The question to ask is what the file needs to already exist.

`ops/` runs before the schema diff, so it is for the things a schema needs in
order to be created at all -- extensions, database settings, a hypertable
conversion, anything that must precede a table.

`data/` runs after `schema/`, `logic/` and `permissions/` have converged, so it
is for the things that need the release to have happened -- backfilling a column
`schema/` just added, `CALL`ing a procedure `logic/` just created, seeding a
table, commenting a view.

Putting the second kind in `ops/` does not fail loudly. The file runs before its
target exists, finds nothing, and -- because these files are usually written to
guard themselves -- no-ops. m8 then records it applied, permanently, since `ops/`
is one-time and checksummed. What the file carried is simply lost, and the only
remedies are splitting the change across two releases or having someone run the
file by hand afterwards. That is the case `data/` exists to remove.

A `data/` migration whose procedure `COMMIT`s per batch needs
`-- m8:no-transaction`, like any other migration that manages its own
transactions.

### Schema Migrations

The `schema/` folder mirrors your PostgreSQL schemas. Each subfolder targets a specific PG schema. Non-public schemas are created automatically (`CREATE SCHEMA IF NOT EXISTS`) -- no ops/ migration needed.

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

## Bootstrapping an Existing Database

Use `m8 dump` to export an existing database into the m8 folder layout:

```bash
m8 dump --database mydb --user postgres
```

This generates:
- `schema/{pg_schema}/*.sql` -- one CREATE TABLE per table, with indexes and constraints
- `logic/*.sql` -- one file per function, procedure, and view (CREATE OR REPLACE)
- `permissions/grants_{schema}.sql` -- per schema, in this order: `GRANT ... ON
  SCHEMA`, the `REVOKE ... FROM PUBLIC` statements that undo PostgreSQL's
  defaults, then relation, column, sequence, and routine grants

Then run `m8 plan` to verify the dump produces a clean diff (no pending changes).

To limit to specific schemas:

```bash
m8 dump --database mydb --user postgres --schema public --schema materialized
```

### What the dump captures

Identifiers are always quoted, so mixed case, spaces, and reserved words
(`order`, `select`) survive the round trip. Unquoted, a foreign-key target does
not error -- it resolves to a different table.

A schema or table whose name is not usable as a single path component makes
`m8 dump` refuse the run rather than write the file somewhere else -- rename the
object, or pass `--stdout`, which prints the DDL instead of writing anything.
Function and view names are instead sanitized into filenames and
hash-disambiguated on collision, so a `logic/` file's name may differ from the
catalog name while the quoted identifier inside it does not.

Privileges are read from the catalog (`relacl`, `attacl`, `nspacl`, `proacl`),
not from `information_schema`, whose views are filtered to grants the *dumping*
role can see. The capture covers schema `USAGE`/`CREATE`, relation and
column-level grants, sequences, routine `EXECUTE`, `WITH GRANT OPTION`, and the
`REVOKE ... FROM PUBLIC` statements a rebuilt database would otherwise hand back
-- a `SECURITY DEFINER` function is `EXECUTE`-able by `PUBLIC` the moment it is
recreated, so losing the revoke is a privilege escalation, not just a gap.

**Not captured:** materialized views. They have no `CREATE OR REPLACE` form, so
they cannot be re-applied idempotently the way a `logic/` file must be. `m8 dump`
names them and refuses rather than leaving them out silently; pass
`--allow-unsupported` to skip them deliberately.

Also not captured: **triggers, row-level security (the flag and its policies),
and replica identity**, plus roles themselves, event triggers, and
`ALTER DEFAULT PRIVILEGES`.

> **Triggers, RLS, and replica identity, and `--strict`.** pg-schema-diff *does*
> introspect these, so on a database bootstrapped by `m8 dump` the desired state
> never mentions them and the raw diff proposes removing them. In default
> (non-strict) mode m8 drops those statements: if nothing in a `schema/` folder
> declares a trigger, no generated statement may drop one — and likewise for
> policies, RLS, and replica identity. A folder that *does* declare triggers gets
> the normal treatment for that class, because then a trigger the files omit
> really is one the desired state says should not exist.
>
> **`--strict` has no such protection, by design** — it means "these files are the
> whole truth". Do not run `--strict` against a database whose triggers, RLS
> policies, or `REPLICA IDENTITY FULL` (logical replication / CDC) are not
> declared in the migration files: it will remove them.

## Commands

| Command | Description |
|---------|-------------|
| `m8 apply` | Apply pending migrations (ops → schema → logic → permissions → data) |
| `m8 plan` | Show what would be applied without making changes (exit code 2 if pending) |
| `m8 status` | Show applied, pending, changed, and drifted migrations |
| `m8 sync` | One-time convergence for brownfield adoption (`ops/` and `data/` are baselined, not run) |
| `m8 baseline` | Mark migrations as applied without running them |
| `m8 dump` | Export database objects to migration files |
| `m8 new` | Scaffold a new migration file in the correct folder |
| `m8 version` | Print version information |

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

### Timeouts on generated schema statements

pg-schema-diff attaches a `lock_timeout` and a `statement_timeout` to every
statement it generates, derived from that statement's hazards: 3 seconds for
ordinary DDL, 20 minutes for a concurrent index build or a table drop. m8
applies both, session-level, around each statement and resets them afterwards.
(Session-level, not `SET LOCAL`: the plan contains `CREATE INDEX CONCURRENTLY`,
which cannot run inside a transaction, and `SET LOCAL` outside one does
nothing.)

A `lock_timeout` means a schema statement that cannot get its `ACCESS EXCLUSIVE`
lock **fails fast instead of queueing behind a long transaction** — which on a
busy primary is what stops one DDL statement from blocking every reader behind
it. If a legitimate statement needs longer than the derived value, raise it:

```bash
m8 apply --statement-timeout 5m --lock-timeout 10s
```

```yaml
statement_timeout: 5m
lock_timeout: 10s
```

Either override replaces the derived value for **every** generated statement,
including the 20-minute index-build allowance, so prefer the flag for a one-off.

## Configuration

### .m8.yaml

Create a `.m8.yaml` in your project root to persist connection settings:

```yaml
database: mydb
host: localhost
port: 5432
user: postgres
sslmode: prefer
migrations_dir: migrations
```

Safety settings for a production target (see
[Refusing to degrade](#refusing-to-degrade-require_shadow---fail-on-unvalidated)):

```yaml
require_shadow: true
fail_on_unvalidated: true
```

Or use a connection URL:

```yaml
database_url: postgres://user:pass@localhost:5432/mydb?sslmode=prefer
```

**Priority order:** CLI flags > environment variables > `.m8.yaml` > defaults

The boolean safety settings are the exception: `strict`, `fail_on_unvalidated`
and `require_shadow` are OR-ed across sources, so a lower-priority source can
turn one on and a higher-priority one cannot turn it back off. `--strict=false`
does not clear `strict: true` in `.m8.yaml`, and `require_shadow` forces
`fail_on_unvalidated` on unconditionally. Clear the setting at its source.

### Environment Variables

m8 supports standard PostgreSQL environment variables and `.env` files:

| Variable | Flag | Config key | Default |
|----------|------|------------|---------|
| `PGHOST` | `--host` | `host` | localhost |
| `PGPORT` | `--port` | `port` | 5432 |
| `PGDATABASE` | `--database` | `database` | -- |
| `PGUSER` | `--user` | `user` | -- |
| `PGPASSWORD` | `--password` | `password` | -- |
| `PGSSLMODE` | `--sslmode` | `sslmode` | prefer |
| `DATABASE_URL` | `--database-url` | `database_url` | -- |
| `SHADOW_DATABASE_URL` | `--shadow-url` | `shadow_url` | -- |
| `M8_REQUIRE_SHADOW` | -- | `require_shadow` | false |
| `M8_FAIL_ON_UNVALIDATED` | `--fail-on-unvalidated` | `fail_on_unvalidated` | false |

A `.m8.yaml` that exists but cannot be parsed is a **fatal error**, not a
warning: falling back to an empty config would turn every safety setting in the
file off at exactly the moment the file is wrong. A *missing* `.m8.yaml` is
still fine. Boolean environment overrides must parse — `M8_REQUIRE_SHADOW=ture`
is an error rather than a silent `false`.

### Shadow Instance for Schema Diffing

To compute schema diffs, pg-schema-diff creates and drops temporary databases
(named `pgschemadiff_tmp_*`) to parse your desired DDL and validate the generated
plan. By default these are created **on the target instance** — including for
`m8 plan`, which is therefore *not* side-effect-free.

Against a production primary this churns `CREATE`/`DROP DATABASE` on the live
cluster, which can be disruptive (and on some PostgreSQL versions can leave
invalid databases behind if a drop is interrupted). Point m8 at a separate,
non-production instance to host these temp databases:

```yaml
database_url: postgres://user:pass@prod-host:5432/mydb
shadow_url:   postgres://user:pass@shadow-host:5432/postgres   # isolated instance
```

**The shadow must be able to faithfully build your schema.** Temp databases are
created from `template0`, so the shadow instance needs:

- `CREATE DATABASE` privilege for the connecting role.
- The **same major PostgreSQL version** as the target (plan validation runs your
  DDL there; version-specific syntax/behavior must match).
- **The same extensions available** and the same `shared_preload_libraries` as
  the target, if your schema references extension-provided types/functions
  (TimescaleDB, PostGIS, `pg_cron`, etc.). The cleanest way to guarantee this is
  to point the shadow at a restore/clone of the target rather than an empty
  instance.
- **PostgreSQL 13 or later**, since cleanup issues `DROP DATABASE ... WITH
  (FORCE)`. On older versions the sweeps log a warning and do nothing; diffing
  itself still works.

Both the temp databases pg-schema-diff creates and m8's own cleanup statements
are anchored to the database named in `shadow_url`, so the shadow host does not
need a separate `postgres` database — some managed providers don't offer one, or
don't let your role connect to it. (pg-schema-diff defaults that anchor to
`postgres`; m8 overrides it.)

If the shadow is **explicitly configured** but the differ can't initialize
(unreachable, bad credentials, missing privilege), m8 fails loudly rather than
silently skipping schema migrations. With no shadow configured it warns and
falls back to the target; either way the engine refuses to apply when schema
migrations exist but cannot be diffed.

### Refusing to degrade (`require_shadow`, `--fail-on-unvalidated`)

Two settings turn m8's degrades into refusals. Set both in any repository whose
target is a production primary:

```yaml
require_shadow: true        # or M8_REQUIRE_SHADOW=true
fail_on_unvalidated: true   # implied by require_shadow
```

`require_shadow` refuses to run at all when no shadow instance is configured,
*before* opening a session on the target, rather than falling back to
`CREATE`/`DROP DATABASE` churn on the live cluster.

`fail_on_unvalidated` (`--fail-on-unvalidated`, implied by `require_shadow`)
covers the other degrade. pg-schema-diff validates a plan by rebuilding the
current schema in a throwaway database and replaying the generated statements
against it. Because m8 scopes each diff to one PostgreSQL schema, that rebuild
cannot resolve an object whose definition reaches outside it — a view over
another schema's tables, for instance.

**m8 first tries to recover.** It asks the catalog which schemas the target's
objects actually reach into, imports those into the throwaway database, and runs
the validation again. When that works the plan is genuinely validated, and the
output says what had to be imported to get there:

```
  ~ schema/materialized/rpt_invoice_detail.sql (schema)
    ℹ validated against a rebuild that imported: app_admin
    ALTER TABLE "materialized"."rpt_invoice_detail" ADD COLUMN "destination_country_code" text
```

The recovery is best effort by construction: it can turn a refused plan into a
validated one, never the reverse. It gives up — leaving exactly the
`PLAN_NOT_VALIDATED` degrade that came before it, with the reason appended — when

- the dependency is a **cycle** (the other schema reaches back into this one, so
  it cannot be imported first);
- the dependency is **too large** to import cheaply (over 200 relations — a
  dependency that big is a sign the schema boundary is wrong, not that the cap
  is too low);
- the object reaches out through a **function call** rather than a relation
  reference, which the catalog records against `pg_proc` and this does not chase.

Why it matters: without the recovery, the *first* real schema change to a schema
containing such a view is refused outright, and the only way forward is turning
the check off for everything.

Only that one phase degrades. "The generated DDL does not execute" and "the plan
does not converge to the desired state" always abort, in `plan` and `apply`
alike.

`PLAN_NOT_VALIDATED` is printed by both `plan` and `apply`. On its own it
affects no exit code, so a CI gate cannot see it; `--fail-on-unvalidated` turns
it into a non-zero exit from `plan` and a refusal to run the statements in
`apply`.

**Cleanup.** At startup (for `plan`/`apply`/`sync` only) m8 drops any *invalid*
orphaned `pgschemadiff_tmp_*` databases (the residue of an interrupted drop) on
whichever instance hosts temp databases. When a dedicated shadow is configured,
it additionally reclaims *valid* temp databases older than one hour that have no
active connections — abandoned leftovers from a killed process.

As a command exits — including after a Ctrl-C — m8 reclaims the temp databases
*it* created, by name, on a context detached from the cancelled one. That covers
both kinds of residue an interrupted run leaves: a drop interrupted mid-flight,
which leaves the database invalid, and one that was never sent at all, which
leaves it valid and which no age-based sweep would touch for an hour. Going by
name means this can never reach a database another m8 process owns, so it is
safe when several runs share one shadow. Read-only commands (`status`,
`baseline`) never touch temp databases.

## License

Apache License 2.0 -- see [LICENSE](LICENSE).
