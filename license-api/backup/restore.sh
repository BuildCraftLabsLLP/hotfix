#!/bin/bash
# Restore a pg-backup dump into a PostgreSQL database (see README.md).
#   restore.sh <license-db-….dump.age> <age key file> <target DATABASE_URL>
# Needs age and pg_restore 18 (this image has both). Replaces the tables in
# the target in one transaction: on any error nothing changes.
set -euo pipefail
[ $# -eq 3 ] || { echo "usage: restore.sh <dump.age> <age key file> <target DATABASE_URL>" >&2; exit 2; }
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
age --decrypt --identity "$2" --output "$work/db.dump" "$1"
pg_restore --list "$work/db.dump" > /dev/null
pg_restore --clean --if-exists --no-owner --no-privileges --single-transaction --exit-on-error --dbname "$3" "$work/db.dump"
echo "restore.sh: restored $(basename "$1")"
