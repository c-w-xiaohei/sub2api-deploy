#!/bin/sh
# The test image intentionally emits no environment, password, or client output.
set -eu
: "${DATABASE_HOST:?}" "${DATABASE_PORT:?}" "${DATABASE_USER:?}" "${DATABASE_PASSWORD:?}" "${DATABASE_DBNAME:?}" "${DATABASE_SSLMODE:?}"
: "${REDIS_HOST:?}" "${REDIS_PORT:?}" "${REDIS_USERNAME:?}" "${REDIS_PASSWORD:?}" "${REDIS_DB:?}" "${REDIS_ENABLE_TLS:?}"
: "${LIVE_PROGRESS_ID:?}"
rm -f "/app/data/.live-$LIVE_PROGRESS_ID-start" "/app/data/.live-$LIVE_PROGRESS_ID-postgres" "/app/data/.live-$LIVE_PROGRESS_ID-redis" "/app/data/.live-$LIVE_PROGRESS_ID-http"
: > "/app/data/.live-$LIVE_PROGRESS_ID-start"
i=0
postgres=failed
redis=failed
while :; do
  if [ "$postgres" != ready ] && PGPASSWORD="$DATABASE_PASSWORD" psql "host=$DATABASE_HOST port=$DATABASE_PORT dbname=$DATABASE_DBNAME user=$DATABASE_USER sslmode=$DATABASE_SSLMODE connect_timeout=3" -X -tAc 'SELECT 1' >/dev/null 2>&1; then
    : > "/app/data/.live-$LIVE_PROGRESS_ID-postgres"
    postgres=ready
  fi
  if [ "$redis" != ready ] && REDISCLI_AUTH="$REDIS_PASSWORD" redis-cli --user "$REDIS_USERNAME" -h "$REDIS_HOST" -p "$REDIS_PORT" -n "$REDIS_DB" PING 2>/dev/null | grep -qx PONG; then
    : > "/app/data/.live-$LIVE_PROGRESS_ID-redis"
    redis=ready
  fi
  [ "$postgres" = ready ] && [ "$redis" = ready ] && break
  i=$((i + 1))
  [ "$i" -lt 20 ] || exit 1
  sleep 1
done
mkdir -p /srv
: > /srv/ready
busybox httpd -f -p 8080 -h /srv &
httpd=$!
trap 'kill "$httpd" 2>/dev/null || true' EXIT INT TERM
i=0
until wget -q -O /dev/null http://127.0.0.1:8080/ready; do
  kill -0 "$httpd" 2>/dev/null || wait "$httpd"
  i=$((i + 1))
  [ "$i" -lt 20 ] || exit 1
  sleep 1
done
: > "/app/data/.live-$LIVE_PROGRESS_ID-http"
wait "$httpd"
