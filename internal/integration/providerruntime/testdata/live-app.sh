#!/bin/sh
# The test image intentionally emits no environment, password, or client output.
set -eu
: "${DATABASE_HOST:?}" "${DATABASE_PORT:?}" "${DATABASE_USER:?}" "${DATABASE_PASSWORD:?}" "${DATABASE_DBNAME:?}" "${DATABASE_SSLMODE:?}"
: "${REDIS_HOST:?}" "${REDIS_PORT:?}" "${REDIS_USERNAME:?}" "${REDIS_PASSWORD:?}" "${REDIS_DB:?}" "${REDIS_ENABLE_TLS:?}"
i=0
until PGPASSWORD="$DATABASE_PASSWORD" psql "host=$DATABASE_HOST port=$DATABASE_PORT dbname=$DATABASE_DBNAME user=$DATABASE_USER sslmode=$DATABASE_SSLMODE connect_timeout=3" -X -tAc 'SELECT 1' >/dev/null 2>&1 \
  && redis-cli --user "$REDIS_USERNAME" --pass "$REDIS_PASSWORD" -h "$REDIS_HOST" -p "$REDIS_PORT" -n "$REDIS_DB" PING 2>/dev/null \
  | grep -qx PONG; do
  i=$((i + 1))
  [ "$i" -lt 20 ] || exit 1
  sleep 1
done
mkdir -p /srv
: > /srv/ready
exec busybox httpd -f -p 8080 -h /srv
