# Security Policy

## Supported versions

Only the latest release receives fixes.

## Reporting a vulnerability

Report privately through GitHub: **Security → Report a vulnerability** on
<https://github.com/ags-slc/m8>. Please do not open a public issue.

This is a single-maintainer project. Expect a first response within a week, and
a fix -- or an explanation of why there will not be one -- in the release that
follows.

## Threat model

m8 is a developer and CI tool, not a service. It has no network listener and no
privileged daemon. The boundaries that matter:

- **The target database is not trusted input.** `m8 dump` reads catalog names
  and writes them to disk, and `m8 apply` executes SQL that a dump produced.
  Anyone who can create an object in a dumped database can choose its name.
  A path-traversal issue in exactly this boundary was fixed in v0.3.0.
- **Migration files are trusted.** `ops/` and `data/` content is executed
  verbatim. Whatever reviews a pull request is what gates them.
- **Connection strings and passwords** come from flags, `PG*` environment
  variables, `.env`, or `.m8.yaml`. m8 does not log them.
