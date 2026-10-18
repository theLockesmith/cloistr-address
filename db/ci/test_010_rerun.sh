#!/bin/sh
# Reproduces production's ownership for migration 010 and proves it reruns
# cleanly. In production tenant_members is owned by postgres and migrations
# run as the non-superuser cloistr; CI's database superuser is cloistr, so a
# separate non-superuser (migrator) plays cloistr's part here.
#
#   PGHOST/PGUSER/PGPASSWORD/PGDATABASE: the CI superuser connection.
#   $1: the 010 file to test (default db/migrations/010_up.sql).
set -eu
f=${1:-db/migrations/010_up.sql}
db=rerun010_$$
psql -v ON_ERROR_STOP=1 -q -c "CREATE DATABASE $db"
trap 'psql -q -c "DROP DATABASE IF EXISTS $db" >/dev/null 2>&1 || true' EXIT
psql -v ON_ERROR_STOP=1 -q -c "DO \$\$BEGIN IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname='migrator') THEN CREATE ROLE migrator LOGIN PASSWORD 'migrator'; END IF; END\$\$"
# First apply as the superuser (production's tables were created by postgres).
PGDATABASE=$db psql -v ON_ERROR_STOP=1 -q -f db/test_schema.sql
PGDATABASE=$db psql -v ON_ERROR_STOP=1 -q -f "$f"
PGDATABASE=$db psql -v ON_ERROR_STOP=1 -q -c "GRANT ALL ON SCHEMA public TO migrator; GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO migrator; ALTER FUNCTION effective_quota(CHAR(64), VARCHAR(50)) OWNER TO migrator"
# Every pod start reapplies it as the non-owner. That must succeed.
echo "rerun of $f as non-owner:"
PGDATABASE=$db PGUSER=migrator PGPASSWORD=migrator psql -v ON_ERROR_STOP=1 -q -f "$f"
echo "OK: $f reruns cleanly as a non-owner"
