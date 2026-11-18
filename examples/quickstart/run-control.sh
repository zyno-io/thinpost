#!/pki/busybox sh
# Entry point of the control service. The release image has no shell, so a
# static busybox copied into the pki volume by pki-init runs this wrapper. It
# only reads generated values into the environment, then execs the real binary.
set -eu
. /pki/runtime.env
# The shell has no other tools on PATH; `read` is a builtin.
read -r runtime_password </pki/pg-runtime-password
read -r THINPOST_S3_SECRET_KEY </pki/s3-secret
THINPOST_DATABASE_URL="postgres://thinpost_runtime:${runtime_password}@127.0.0.1:5432/thinpost?sslmode=disable&search_path=thinpost"
export THINPOST_CONTROL_JOURNAL_ID THINPOST_DATABASE_URL THINPOST_S3_SECRET_KEY
exec /usr/local/bin/thinpost-control
