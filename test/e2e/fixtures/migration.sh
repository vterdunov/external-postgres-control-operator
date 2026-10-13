#!/bin/sh
umask 077
PGPASSFILE=$(mktemp)
trap 'rm -f "$PGPASSFILE"' EXIT
export PGPASSFILE
load_connection() {
  PGHOST=$(cat /connection/host)
  PGPORT=$(cat /connection/port)
  PGDATABASE=$(cat /connection/dbname)
  PGUSER=$(cat /connection/user)
  PGSSLMODE=$(cat /connection/sslmode)
  cp /connection/pgpass "$PGPASSFILE"
  export PGHOST PGPORT PGDATABASE PGUSER PGSSLMODE
}
attempt=0
until load_connection && psql -X -v ON_ERROR_STOP=1 -Atc 'SELECT 1' >/dev/null 2>&1; do
  attempt=$((attempt + 1))
  if [ "$attempt" -ge 150 ]; then
    echo "Application connection retry limit reached"
    exit 1
  fi
  echo "Waiting for application connection"
  sleep 1
done
echo "Application connection established"
until [ -f /migration-gate/ready ]; do sleep 1; done
echo "Migration gate observed"
command -v timeout >/dev/null
# The child shell expands its own connection environment on each SQL attempt.
# shellcheck disable=SC2016
timeout --kill-after=1s 60s sh -ceu '
load_connection() {
  PGHOST=$(cat /connection/host)
  PGPORT=$(cat /connection/port)
  PGDATABASE=$(cat /connection/dbname)
  PGUSER=$(cat /connection/user)
  PGSSLMODE=$(cat /connection/sslmode)
  cp /connection/pgpass "$PGPASSFILE"
  export PGHOST PGPORT PGDATABASE PGUSER PGSSLMODE
}
until load_connection && psql -X -v ON_ERROR_STOP=1 -Atc \
  "CREATE TABLE app.orders(value integer); INSERT INTO app.orders VALUES (42)" >/dev/null 2>&1; do
  echo "Waiting for migration SQL"
  sleep 1
done
'
echo "Migration completed"
