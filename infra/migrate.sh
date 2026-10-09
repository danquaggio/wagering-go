#!/bin/sh
set -eu
case "${1:-up}" in
 up)
  exists="$(psql -X -A -t -c "SELECT to_regclass('public.schema_migrations') IS NOT NULL")"
  if [ "$exists" = t ]; then
    version="$(psql -X -A -t -c 'SELECT max(version) FROM schema_migrations')"
    [ "$version" = 1 ] || { echo "Unexpected migration version"; exit 1; }
    echo "Migration 001 already applied"
  else
    psql -X -v ON_ERROR_STOP=1 -f /migrations/001_init.up.sql
  fi;;
 down)
  [ "${CONFIRM_DESTRUCTIVE_DOWN:-}" = yes ] || { echo "Set CONFIRM_DESTRUCTIVE_DOWN=yes for a disposable database"; exit 1; }
  psql -X -v ON_ERROR_STOP=1 -f /migrations/001_init.down.sql;;
 *) echo "Usage: migrate.sh up|down"; exit 1;;
esac
