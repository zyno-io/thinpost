#!/bin/sh
# One-shot database preparation, in the order docs/recovery.md describes:
# bootstrap-database -> migrate -> grant-runtime -> open the admission gate.
# Runs in the control plane's network namespace so PostgreSQL and the S3
# emulator are on loopback. Safe to re-run.
set -eu

OWNER_PW="$(cat /pki/pg-owner-password)"
GEN=00000000000000000000000000000001
RUN=00000000000000000000000000000002
APP_ID="$(cat /pki/app-id)"
export THINPOST_ALLOW_INSECURE_LOCAL_DB=1 THINPOST_ALLOW_INSECURE_LOCAL_S3=1
export PGPASSWORD="$OWNER_PW" PGOPTIONS="-c search_path=thinpost"
psql_owner() { psql -h 127.0.0.1 -U thinpost_owner -d thinpost -v ON_ERROR_STOP=1 -qAt "$@"; }
control() { /usr/local/bin/thinpost-control "$@"; }

echo "db-init: waiting for PostgreSQL and the S3 emulator"
i=0
until pg_isready -q -h 127.0.0.1 -U thinpost_owner -d thinpost && curl -fs http://127.0.0.1:4566/_ministack/health >/dev/null; do
  i=$((i + 1)); [ "$i" -lt 120 ] || { echo "services did not become ready" >&2; exit 1; }
  sleep 1
done

echo "db-init: creating the object bucket"
curl -fs -X PUT --aws-sigv4 "aws:amz:us-east-1:s3" --user "quickstart:$(cat /pki/s3-secret)" \
  http://127.0.0.1:4566/thinpost -o /dev/null || curl -fs -I --aws-sigv4 "aws:amz:us-east-1:s3" --user "quickstart:$(cat /pki/s3-secret)" http://127.0.0.1:4566/thinpost -o /dev/null

BASE="postgres://thinpost_owner:$OWNER_PW@127.0.0.1:5432/thinpost?sslmode=disable"
echo "db-init: bootstrap-database"
THINPOST_DATABASE_URL="$BASE" control bootstrap-database --database thinpost --schema thinpost --runtime-role thinpost_runtime
export THINPOST_DATABASE_URL="$BASE&search_path=thinpost"
echo "db-init: migrate"
control migrate
echo "db-init: grant-runtime"
control grant-runtime --role thinpost_runtime

echo "db-init: seeding the quickstart app"
# New apps start with every quota at zero, which refuses all submissions.
psql_owner -c "INSERT INTO apps(id,name,recipient_daily_limit,pending_message_limit,submission_rate_per_minute,submission_burst,submission_tokens) VALUES ('$APP_ID','Quickstart',1000,1000,600,100,100) ON CONFLICT DO NOTHING"

gate() { psql_owner -c "SELECT $1 FROM runtime_gate WHERE singleton"; }
if [ "$(gate mode)" != open ]; then
  echo "db-init: opening the admission gate (generation $GEN)"
  control recovery begin --generation "$GEN" --recovery-id "$RUN" --actor quickstart-db-init \
    --expected-revision "$(gate revision)" --workers-fenced --database-isolated >/dev/null
  control recovery quarantine --generation "$GEN" --recovery-id "$RUN" --actor quickstart-db-init \
    --workers-fenced --database-isolated >/dev/null
  control recovery open --generation "$GEN" --recovery-id "$RUN" --actor quickstart-db-init \
    --expected-revision "$(gate revision)" --workers-fenced --database-isolated --evidence-reviewed >/dev/null
fi
echo "db-init: gate is $(gate mode)"
