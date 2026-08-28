# Changelog

Notable changes to m8. Format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/);
versions follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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

  `m8 new data "<name>"` scaffolds one.

### Changed

- `_m8.history`'s `type` CHECK now admits `'data'`, and carries the explicit
  name `m8_history_type_check`. **Existing installs are upgraded in place** by
  `EnsureSchema` on the next command: they carry PostgreSQL's auto-generated
  `history_type_check`, which would have let a `data/` migration run and then
  rejected its history row -- applied but unrecorded, the worst possible order.
  The upgrade is keyed on the constraint name, so it happens once.

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

[0.2.0]: https://github.com/ags-slc/m8/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/ags-slc/m8/releases/tag/v0.1.0
