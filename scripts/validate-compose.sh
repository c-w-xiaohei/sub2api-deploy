#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

edge_env="$(mktemp)"
trap 'rm -f "$edge_env"' EXIT
cat > "$edge_env" <<EOF
TRAEFIK_IMAGE=traefik:v3.3.3
CLOUDFLARE_DNS_API_TOKEN=not-used-in-verification
ACME_EMAIL=ops@example.com
EOF
docker compose --project-name sub2api-edge --env-file "$edge_env" -f compose/edge.yml config >/dev/null
rm -f "$edge_env"
trap - EXIT

for site_id in code2 code3; do
  for pair in docker-docker neon-docker docker-upstash neon-upstash; do
    postgres_mode="${pair%-*}"
    redis_mode="${pair#*-}"
    env_file="$(mktemp)"
    trap 'rm -f "$env_file"' EXIT
    cat > "$env_file" <<EOF
SUB2API_IMAGE=weishaw/sub2api@sha256:abcdef1234567890
SITE_RUNTIME_ROOT=${ROOT_DIR}/runtime/sites/${site_id}
BLUE_EDGE_ALIAS=sub2api-${site_id}-blue
GREEN_EDGE_ALIAS=sub2api-${site_id}-green
SLOT=blue
SLOT_DATA_DIR=blue
AUTO_SETUP=false
DOMAIN=${site_id}.example.com
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
ADMIN_EMAIL=admin@example.com
EOF
    profiles=()
    [[ "$postgres_mode" == docker ]] && profiles+=(--profile postgres)
    [[ "$redis_mode" == docker ]] && profiles+=(--profile redis)
    docker compose --project-name "sub2api-${site_id}" --env-file "$env_file" "${profiles[@]}" --profile app -f compose/upstream.yml -f compose/site.yml config >/dev/null
    rm -f "$env_file"
    trap - EXIT
  done
done
