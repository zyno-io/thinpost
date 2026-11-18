#!/bin/sh
# Mounted into /docker-entrypoint-initdb.d/ of the postgres service.
set -eu
psql -v ON_ERROR_STOP=1 --username postgres --dbname postgres \
  -v "owner_pw=$(cat /pki/pg-owner-password)" \
  -v "runtime_pw=$(cat /pki/pg-runtime-password)" \
  -f /qs/init.sql
