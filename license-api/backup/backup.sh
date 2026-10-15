#!/bin/bash
# Nightly backup of the licensing database: Railway cron service "pg-backup"
# (see README.md). Dumps PostgreSQL, checks the dump is a readable archive,
# encrypts it to an age public key (the private key never touches Railway),
# uploads it to the Railway bucket, and deletes dumps older than KEEP_DAYS.
# Exits non-zero on any failure, so a failed night shows up in Railway.
#
# Env: DATABASE_URL, BACKUP_AGE_RECIPIENT (age1…), and the bucket's BUCKET,
# ENDPOINT, ACCESS_KEY_ID, SECRET_ACCESS_KEY, REGION; optional KEEP_DAYS (30),
# S3_ADDRESSING_STYLE (virtual, as Railway buckets use; CI's MinIO uses path).
set -euo pipefail

: "${DATABASE_URL:?}" "${BACKUP_AGE_RECIPIENT:?}" "${BUCKET:?}" "${ENDPOINT:?}" "${ACCESS_KEY_ID:?}" "${SECRET_ACCESS_KEY:?}"
KEEP_DAYS="${KEEP_DAYS:-30}"
PREFIX="license-db-"

export AWS_ACCESS_KEY_ID="$ACCESS_KEY_ID" AWS_SECRET_ACCESS_KEY="$SECRET_ACCESS_KEY" AWS_DEFAULT_REGION="${REGION:-auto}"
# S3-compatible store: only send/check checksums when an operation requires them.
export AWS_REQUEST_CHECKSUM_CALCULATION=when_required AWS_RESPONSE_CHECKSUM_VALIDATION=when_required
s3api() { aws --endpoint-url "$ENDPOINT" s3api "$@"; }

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
export AWS_CONFIG_FILE="$work/aws.config"
printf '[default]\ns3 =\n  addressing_style = %s\n' "${S3_ADDRESSING_STYLE:-virtual}" > "$AWS_CONFIG_FILE"

name="${PREFIX}$(date -u +%Y-%m-%dT%H%MZ).dump.age"

pg_dump --format=custom --no-owner --no-privileges --file="$work/db.dump" "$DATABASE_URL"
tables=$(pg_restore --list "$work/db.dump" | grep -c " TABLE " || true)
[ "$tables" -gt 0 ] || { echo "pg-backup: dump has no tables" >&2; exit 1; }
age --encrypt --recipient "$BACKUP_AGE_RECIPIENT" --output "$work/db.dump.age" "$work/db.dump"

s3api put-object --bucket "$BUCKET" --key "$name" --body "$work/db.dump.age" > /dev/null
size=$(s3api head-object --bucket "$BUCKET" --key "$name" --query ContentLength --output text)
[ "$size" = "$(stat -c %s "$work/db.dump.age")" ] || { echo "pg-backup: uploaded size mismatch" >&2; exit 1; }
echo "pg-backup: uploaded $name ($size bytes, $tables tables)"

# Prune: names sort by date, so anything before the cutoff's name is older.
cutoff="${PREFIX}$(date -u -d "@$(($(date +%s) - KEEP_DAYS * 86400))" +%Y-%m-%dT%H%MZ)"
s3api list-objects-v2 --bucket "$BUCKET" --prefix "$PREFIX" --query 'Contents[].Key' --output text | tr '\t' '\n' |
  while read -r key; do
    if [ -n "$key" ] && [ "$key" != None ] && [ "$key" != "$name" ] && [[ "$key" < "$cutoff" ]]; then
      s3api delete-object --bucket "$BUCKET" --key "$key" > /dev/null
      echo "pg-backup: deleted $key (older than $KEEP_DAYS days)"
    fi
  done
