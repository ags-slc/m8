# PostgreSQL with pg_partman, for the tests that exercise m8's pg_partman support.
# The Debian-based official image ships the PGDG apt repository, which carries
# pg_partman for every supported major version.
FROM postgres:16
RUN apt-get update \
 && apt-get install -y --no-install-recommends postgresql-16-partman \
 && rm -rf /var/lib/apt/lists/*
