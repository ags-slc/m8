# Changelog

Notable changes to m8. Format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/);
versions follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Fixed

- **The dependency import harvested the whole grant matrix, which it can never
  apply.** Rebuilding a dependency schema means asking pg-schema-diff for an
  empty-to-live diff, and that emits privileges along with shape — one statement
  per privilege per grantee. A throwaway database holds none of those roles, so
  every one failed with `42704` and was skipped. Against the database that
  surfaced this, a schema of 84 relations produced **5268 statements**, and the
  plan reported 5268 skipped objects with a `GRANT` as the example. Privileges
  are now dropped from the harvest: no generated statement's success depends on
  a grant existing, so the rebuild does not need them. A two-relation fixture
  went from 15 harvested statements to 8.

  This also *hid* real problems. The report named only the first skipped
  statement, and with grants in the list the first was always a grant — an
  object that genuinely could not be built was invisible behind thousands of
  them.

- **The seeding cap counted relations while the cost is statements.**
  `maxSeededRelations` (200) is checked against `pg_class` entries of relkind
  `r,p,v,m,f`; in the case above that returned 84, comfortably under the cap,
  while the harvest went on to emit and execute 5268 statements against every
  temp database the retry creates. The relation count stays as a cheap
  pre-filter — one catalog query, and it rules out a CDC schema before the
  expensive diff runs at all — and a `maxSeededStatements` cap (2000) now bounds
  what the harvest actually produced.

- **The skip count was multiplied by the number of temp databases.** The seed is
  applied to every temp database a validation run creates, and each failure was
  recorded again, so one unbuildable view was reported as two objects. Skips are
  now deduplicated: the count means "objects that could not be built", not
  "failures observed".

- **Skips are reported by class.** `2 CREATE VIEW, 1 CREATE TABLE (first: …)`
  rather than a raw total plus one arbitrary example, so a single genuinely
  unbuildable table is not buried under noise.

## [Unreleased]

### Fixed

- **m8 refused a plan without saying it had tried to rescue it.** `0.3.2` moved
  the explanation of a failed dependency import into its own `RecoveryNote`
  field, because folding it into `ValidationSkippedReason` meant `firstLine`
  truncated it away — but only the plan renderer was taught to print it. Three
  of the four places that render the reason still dropped the note, including
  the refusal raised under `--fail-on-unvalidated` / `require_shadow`. That is
  the message an operator is actually stopped by, and the configuration this
  whole recovery exists to unblock: m8 declined the plan, showed one truncated
  line of a pg-schema-diff error, and said nothing about what it had imported,
  skipped, or given up on. The note now appears in the refusal, in `status`
  output, and in the apply summary.

- **The refusal's advice was stale.** It stated that validation "fails when an
  object in this schema is defined in terms of another schema" — the exact case
  m8 has tried to handle since `0.3.1`. It now says validation imports the
  schemas a rebuild depends on and reports what actually went wrong.

## [0.3.2] - 2026-08-31

### Fixed

- **The dependency import introduced in 0.3.1 gave up too easily.** Importing a
  schema into the validation rebuild was all-or-nothing: if any object in it
  could not be created, the whole recovery failed and the plan degraded. That is
  the normal case, not the exceptional one — a dependency schema usually holds
  objects of its own that reach into a third schema. Found on the first real
  target: `materialized` reaches into `app_admin`, which holds a view over a CDC
  schema, so the import failed and the plan was refused exactly as it had been
  before 0.3.1.

  Objects the import cannot create are now **skipped**, and the plan reports how
  many and which. The seed is scaffolding, not the thing under test: what has to
  hold is that the target schema rebuilds and the plan converges against it. A
  skipped object cannot make a bad plan look good — if it was one the target
  needed, the rebuild fails and the plan degrades as before.

  This also makes dependency **cycles** recoverable, which 0.3.1 listed as a
  hard limit: the back-reference is simply one of the objects that gets skipped.

- **The explanation of a failed recovery was invisible.** It was appended to
  `ValidationSkippedReason`, which is a multi-line pg-schema-diff error that
  every caller renders as `firstLine(...)` — so the appended text was always
  truncated away. Diagnosing the `app_admin` failure above needed a database
  session precisely because of this. The explanation now travels in its own
  `RecoveryNote` field and is printed on its own line, whether the recovery
  succeeded with skips or failed outright.

## [0.3.1] - 2026-08-31

### Fixed

- **A schema containing a view that reaches into another schema can now be
  changed at all.** Plan validation rebuilds the current schema in a throwaway
  database; because each diff is scoped to one PostgreSQL schema, that rebuild
  failed on any object defined in terms of another one, and the plan came back
  `PLAN_NOT_VALIDATED`. With `--fail-on-unvalidated` (implied by
  `require_shadow`) the plan was then refused — so the FIRST real change to such
  a schema was unshippable, and the only way forward was disabling the check
  globally.

  m8 now asks the catalog which schemas the target's objects reach into,
  imports those into the throwaway database, and validates again. The plan
  output names what it imported (`ℹ validated against a rebuild that imported:
  …`).

  Best effort by construction: it can turn a refused plan into a validated one,
  never the reverse. It gives up and leaves the previous degrade in place — with
  the reason appended — on a dependency cycle, on a dependency over 200
  relations, and on a reference made through a function call rather than a
  relation (recorded against `pg_proc`, which this does not chase).

## [0.3.0] - 2026-08-29

> ### Upgrade before your next `m8 dump` of a database others can write to
>
> **In `v0.1.0` and `v0.2.0`, a name in the target database chose where `m8 dump`
> wrote.** Schema, table, function and view names all become path components, a
> quoted PostgreSQL identifier may legally contain `/` and `..`, and nothing
> sanitized them -- so an object named `../../../../x` wrote outside the
> migrations tree, and the `MkdirAll` on the schema component created whatever
> directories it took to get there. Planting such a name needs `CREATE` on one
> schema and nothing else: the dump's introspection applies no ownership or ACL
> filter.
>
> Escaping the tree is the smaller half. A name of `../ops/20260101_001__x` lands
> in `ops/`, whose contents `apply` runs verbatim rather than diffing them as
> declarative state, and overwriting a reviewed `logic/` file is exactly what
> makes the next `apply` **re-run** it -- the checksum in `_m8.history` is a
> re-run trigger, not a tamper gate. Either way SQL written by an unprivileged
> database user is executed by the role that runs your migrations, in every
> environment the repository reaches.
>
> It is not remote and not unattended: an operator has to run `m8 dump`, commit
> the result, and apply it. It is fixed in this release. **If you dumped with
> `v0.1.0` or `v0.2.0` against a database you do not fully control, re-run
> `m8 dump` on this release and read the diff before applying** -- especially any
> file in `ops/`, `logic/` or `permissions/` you do not remember reviewing.

### Added

- **A fifth migration phase, `data/`, applied last.** One-time and checksummed
  like `ops/`, but it runs *after* `schema/`, `logic/` and `permissions/` have
  converged, so a migration in it can read and write the objects the same apply
  introduced. Apply order is now **ops → schema → logic → permissions → data**.

  `ops/` runs first, which made a whole class of migration impossible to ship in
  one release: backfilling a column `schema/` adds, `CALL`ing a procedure
  `logic/` creates, seeding or commenting a new object. In `ops/` such a file
  runs before its target exists, finds nothing, and no-ops -- and m8 records it
  applied, permanently, because `ops/` is one-time. What it carried is lost. The
  only ways out were to split the change across two releases or to have an
  operator run the file by hand against production afterwards, which is the
  thing a migration pipeline exists to remove.

  `data/` also has its own version namespace, so a release may add an `ops/` and
  a `data/` file bearing the same timestamp.

  `m8 new data "<name>"` scaffolds one. `plan`, `status` and `apply` report
  `data/` alongside the other phases, and a pending `data/` file makes `plan`
  exit 2 like any other pending change.

  `sync` and `baseline` **baseline** `data/` rather than run it, exactly as they
  do `ops/`. Brownfield adoption meets a database whose data is already whatever
  it is; replaying a backfill over it is at best wasted work. A `data/` file
  written after adoption has to go through `apply` -- `sync` and `baseline` will
  record it applied without executing it.

  **Upgrade every runner before the first `data/` file merges.** `v0.2.0` and
  earlier do not know the folder exists -- they discover nothing in it, report
  the release fully applied, and exit 0 from `plan` when a `data/` migration is
  the only pending work. Nothing is destroyed, because no history row is written
  and the file applies on the first run of a new enough binary. But a CI gate
  keyed on `plan`'s exit code goes green while the backfill has not run.

### Changed

- `_m8.history`'s `type` CHECK now admits `'data'`, and carries the explicit
  name `m8_history_type_check`. Existing installs carry PostgreSQL's
  auto-generated `history_type_check`, which would have let a `data/` migration
  run and then rejected its history row -- applied but unrecorded, the worst
  possible order. **They are upgraded in place** by `EnsureSchema` on the next
  `apply`, `status`, `sync` or `baseline`; `plan` stays read-only and changes
  nothing. The upgrade is keyed on the constraint name, so it happens once, and
  is safe against two m8 processes racing into it.

- **`m8 dump` settles a logic filename before it writes it.** Function and view
  file names now route through a sanitizer -- every run of characters outside
  `[A-Za-z0-9_.-]` becomes `_`, with the existing hash dedup separating whatever
  that collapses together. `$` and non-ASCII letters are both legal in a
  PostgreSQL identifier, so an object named `v_orders$raw` or `vista_café` gets a
  different filename than it did in `v0.2.0`. Re-dumping an existing tree leaves
  the old file beside the new one: delete the superseded `logic/` files, or the
  next `apply` re-runs the object under its new name while the old file stays
  recorded as applied. Schema and table names are refused rather than sanitized
  -- see **Security**.

### Fixed

- **`dump` silently omitted every schema and function whose name starts with
  `pg` followed by any single character.** `ListSchemas` and `ListFunctions`
  filtered on `NOT LIKE 'pg_%'` with the `_` unescaped, where it is a
  single-character wildcard -- so `pgboss`, `pgagent` and anything shaped like
  them were excluded along with the `pg_` catalog names the filter was aimed at.
  A schema dropped this way took every table, view, function and grant in it
  with no error and no warning, and a baseline taken from such a database was
  quietly incomplete. Both patterns are now `'pg\_%'`, as the grant queries in
  `internal/dump/permissions.go` already were. Re-dumping on this release adds
  files for objects it previously ignored, and the next `m8 plan` will propose
  to manage them.

### Security

- **`dump` and `new` can no longer be steered outside the migrations
  directory.** Every mkdir and open now goes through `os.Root` confined to the
  migrations tree, which also defeats a symlink that a lexical path check would
  miss. Names are settled before they are joined, because confinement alone is
  not enough: `filepath.Join("schema", "../ops")` cleans to `ops`, which is
  perfectly local and perfectly executable.

  Schema and table names **fail closed** -- `dump` refuses the run rather than
  merge two objects into one filename, because there is no dedup pass on that
  path. `--stdout` still prints such a database. Function and view names are
  sanitized instead, which is safe only because logic filenames already end in a
  hash-based dedup. `m8 new` built paths out of `argv` the same way
  (`m8 new logic ../../../x`) and now validates through the same helper.

  This is a behaviour change as well as a fix: a table literally named `a/b`
  used to dump, and now fails the run with exit 1.

## [0.2.0] - 2026-08-25

First release with production experience behind it. `v0.1.0` was adopted against a
live PostgreSQL cluster (~160 objects across four migration folders, two target
databases), and almost everything below was found by that adoption rather than by
review.

> ### Upgrade immediately if you run `v0.1.0` against anything you care about
>
> **`v0.1.0`'s `plan` writes to the target database.** pg-schema-diff creates and
> drops `pgschemadiff_tmp_*` databases to parse desired DDL and validate a plan,
> and `v0.1.0` did that on the target itself — so `m8 plan`, the command whose
> whole purpose is to be read-only, churned `CREATE DATABASE` / `DROP DATABASE`
> on the primary. It also bootstrapped its own `_m8` state schema during a plan.
>
> Both are fixed. A `plan` no longer writes anything to its target, and
> `require_shadow` lets a repository refuse to run at all rather than silently
> degrade to the old behaviour.

### Added

- `require_shadow` config setting. Refuses to plan or apply without a separate
  shadow instance, instead of warning and falling back to the target. Implies
  `fail_on_unvalidated`.
- `--fail-on-unvalidated` / `M8_FAIL_ON_UNVALIDATED`, so CI can refuse a schema
  diff whose plan could not be validated.
- Reclamation of stale orphan temp databases on a dedicated shadow, and cleanup
  on `SIGINT` / `SIGTERM` rather than leaking them on interrupt.
- Release distribution hardening: signing, Windows builds, Homebrew.

### Fixed — `plan` is read-only, and scoped

- Temp databases are anchored to the configured shadow and never created on the
  target. `plan` no longer bootstraps its own state schema.
- The schema differ is gated to diffing commands only, and fails loudly on a bad
  shadow rather than proceeding without one.
- The engine refuses to silently skip schema migrations when the differ is
  unavailable.
- Diffs are scoped to the target schema. Unscoped introspection made `plan`
  unusably slow on a large database and pulled in unrelated schemas.
- Schema **creation** is left to the engine instead of appearing in the diff, so
  a plan no longer proposes `CREATE SCHEMA` for schemas it is about to create.
- A dumped baseline no longer plans away triggers, RLS policies, or CDC
  artifacts it did not capture — previously a clean baseline produced a
  destructive plan.
- pg-schema-diff's own sequence DDL is kept rather than synthesized, and the
  sequences a `SERIAL` column's generated DDL depends on are created.
- Plan validation degrades only on the phase that scoping legitimately breaks
  (a cross-schema view that cannot be rebuilt in isolation), not on any failure.

### Fixed — `dump` captures what it claims to

- Every identifier routes through a shared quoter, and dumped DDL is
  schema-qualified. Unqualified DDL landed in `public` and produced spurious
  `DROP TABLE` statements for every reference table on the next plan.
- Generated columns are emitted as `GENERATED ALWAYS AS (…) STORED` instead of
  `DEFAULT`, which failed with `0A000 cannot use column reference in DEFAULT
  expression` and took every other file in the folder down with it.
- Whitespace inside generated-column expressions is preserved.
- Grants are read from `relacl` / `proacl` / `nspacl`, not the role-filtered
  `information_schema`, which silently under-reports. Adds schema grants,
  `PUBLIC` revokes, column-level grants, sequences, grant option, and `EXECUTE`
  on functions and procedures. Column lists bind to their privilege, and
  revokes are no longer over-reported.
- Overloaded functions no longer overwrite each other's logic file; the overload
  suffix is reduced to the arguments that actually distinguish them. Logic
  filenames are a function of the object, not its position in the dump.
- Materialized views are captured rather than skipped. Views are
  schema-qualified, keep their `reloptions`, and are no longer
  double-terminated.
- Extension-owned tables are skipped.
- `GRANT` targets in `permissions/` are schema-qualified.

### Fixed — exit codes and failure reporting

- A plan that **cannot be computed** now exits 1 (a real failure) instead of 2
  (“changes are pending”). CI gates treat 2 as success, so an undiffable
  migration used to pass review and fail during `apply`, on `main`, after merge.
- A failure in one migration folder fails the whole run, rather than being
  attributed to whichever folder happened to come last.
- An unvalidated plan is reported on `apply`, not only on `plan`, and is warned
  about even when the diff is empty.
- An unparseable `.m8.yaml` is fatal instead of being ignored; settings resolve
  once.
- `require_shadow` is checked **before** opening a session on the target, so a
  misconfigured run never connects to production at all.

### Fixed — connections

- `statement_timeout` applies to every target connection, not just the first.
- Schema statement timeouts use `SET`, not `SET LOCAL`.
- The differ's connection embeds `statement_timeout=0`; the sweep connection is
  pinned and connection-string option ordering is corrected.
- The introspection pool is bounded. It was unbounded, which produced mid-diff
  connection failures against a database with many objects.

### Build & CI

- Runtime base moved to Alpine 3.24; the Docker build uses the Go version
  `go.mod` requires (the published image previously could not build at all).
- CI takes its Go version from `go.mod` rather than a pinned string, runs on
  pull requests into any branch, and no longer truncates lint findings.
- Every action moved off the deprecated Node 20 runtime.
- Release signing migrated to cosign v3's bundle format; GoReleaser pinned,
  container tests serialized, cross-compilation moved into CI. Signatures are
  now a single `checksums.txt.sigstore.json` bundle rather than a `.pem`/`.sig`
  pair, so **verification requires cosign >= v2.4.2**:

  ```
  cosign verify-blob \
    --bundle checksums.txt.sigstore.json \
    --certificate-identity-regexp 'https://github.com/ags-slc/m8/.*' \
    --certificate-oidc-issuer https://token.actions.githubusercontent.com \
    checksums.txt
  ```

  (`v0.1.0` was unsigned — it shipped no `signs` block and no signature
  assets, so there is nothing to migrate from.)
- staticcheck and errcheck findings addressed in non-test code.

## [0.1.0] - 2026-04-12

Initial release. The m8 CLI scaffold: `plan`, `apply`, `dump`, `baseline`,
`sync`, `status`, `new`, and `version`. Superseded by `0.2.0` — see the upgrade
note above before running it against anything you care about.

[0.3.2]: https://github.com/ags-slc/m8/compare/v0.3.1...v0.3.2
[0.3.1]: https://github.com/ags-slc/m8/compare/v0.3.0...v0.3.1
[0.3.0]: https://github.com/ags-slc/m8/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/ags-slc/m8/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/ags-slc/m8/releases/tag/v0.1.0
