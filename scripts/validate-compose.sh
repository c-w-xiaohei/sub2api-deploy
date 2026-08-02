#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"
for pair in docker-docker neon-docker docker-upstash neon-upstash; do
  postgres_mode="${pair%-*}"
  redis_mode="${pair#*-}"
  env_file="$(mktemp)"
  trap 'rm -f "$env_file"' EXIT
  cat > "$env_file" <<EOF
SUB2API_IMAGE=weishaw/sub2api@sha256:abcdef1234567890
TRAEFIK_IMAGE=traefik:v3.3.3
SLOT=blue
SLOT_DATA_DIR=blue
BLUE_CONTAINER_NAME=sub2api-blue
GREEN_CONTAINER_NAME=sub2api-green
POSTGRES_CONTAINER_NAME=sub2api-postgres
REDIS_CONTAINER_NAME=sub2api-redis
AUTO_SETUP=false
DOMAIN=sub2api.example.com
DATABASE_HOST=$([[ "$postgres_mode" == neon ]] && printf neon.example.com || printf postgres)
DATABASE_PORT=5432
DATABASE_USER=sub2api
DATABASE_PASSWORD=not-used-in-verification
DATABASE_DBNAME=sub2api
DATABASE_SSLMODE=$([[ "$postgres_mode" == neon ]] && printf require || printf disable)
POSTGRES_USER=sub2api
POSTGRES_PASSWORD=not-used-in-verification
POSTGRES_DB=sub2api
REDIS_HOST=$([[ "$redis_mode" == upstash ]] && printf redis.example.com || printf redis)
REDIS_PORT=6379
REDIS_PASSWORD=not-used-in-verification
REDIS_ENABLE_TLS=$([[ "$redis_mode" == upstash ]] && printf true || printf false)
CLOUDFLARE_DNS_API_TOKEN=not-used-in-verification
ACME_EMAIL=ops@example.com
ADMIN_EMAIL=admin@example.com
EOF
  profiles=()
  [[ "$postgres_mode" == docker ]] && profiles+=(--profile postgres)
  [[ "$redis_mode" == docker ]] && profiles+=(--profile redis)
  docker compose --env-file "$env_file" "${profiles[@]}" --profile app -f compose/upstream.yml -f compose/override.yml -f compose/edge.yml config >/dev/null
  rm -f "$env_file"
  trap - EXIT
done
