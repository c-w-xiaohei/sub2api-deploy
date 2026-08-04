#!/usr/bin/env bash

# Site Compose owns one application project; the Edge project is deliberately separate.
: "${SITE_ID:?SITE_ID is required}"
: "${SITE_RUNTIME_ROOT:?SITE_RUNTIME_ROOT is required}"
: "${SITE_APP_ENV_PATH:?SITE_APP_ENV_PATH is required}"
: "${COMPOSE_PROJECT_NAME:?COMPOSE_PROJECT_NAME is required}"
: "${SITE_ROUTE_PATH:?SITE_ROUTE_PATH is required}"

if [[ ! "$SITE_ID" =~ ^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$ ]]; then
  printf 'SITE_ID must match ^[a-z0-9](?:[a-z0-9-]{0,30}[a-z0-9])?$\n' >&2
  return 1 2>/dev/null || exit 1
fi

: "${EDGE_NETWORK_NAME:?EDGE_NETWORK_NAME is required}"
SITE_RUNTIME_ROOT="$(realpath -m "$SITE_RUNTIME_ROOT")"
SITE_APP_ENV_PATH="$(realpath -m "$SITE_APP_ENV_PATH")"
export SITE_RUNTIME_ROOT

site_runtime_value() {
  local key="$1"
  node scripts/read-runtime-env.cjs "$SITE_RUNTIME_ROOT/runtime.env" "$key"
}

SITE_COMPOSE=(
  docker compose
  --project-name "$COMPOSE_PROJECT_NAME"
  --env-file "$SITE_RUNTIME_ROOT/runtime.env"
  --env-file "$SITE_APP_ENV_PATH"
  --profile app
  -f compose/upstream.yml
  -f compose/site.yml
)

case "$(site_runtime_value POSTGRES_MODE)" in
  docker) SITE_COMPOSE+=(--profile postgres) ;;
  neon) ;;
  *) printf 'POSTGRES_MODE in %s/runtime.env must be docker or neon\n' "$SITE_RUNTIME_ROOT" >&2; return 1 2>/dev/null || exit 1 ;;
esac

case "$(site_runtime_value REDIS_MODE)" in
  docker) SITE_COMPOSE+=(--profile redis) ;;
  upstash) ;;
  *) printf 'REDIS_MODE in %s/runtime.env must be docker or upstash\n' "$SITE_RUNTIME_ROOT" >&2; return 1 2>/dev/null || exit 1 ;;
esac

site_stop_service() {
  local service="$1"
  "${SITE_COMPOSE[@]}" stop --timeout 30 "$service" >/dev/null
  local containers container running
  if ! containers="$("${SITE_COMPOSE[@]}" ps -q "$service")"; then
    return 1
  fi
  for container in $containers; do
    for _ in 1 2 3 4 5 6; do
      if ! running="$(docker inspect -f '{{.State.Running}}' "$container")"; then
        return 1
      fi
      [[ "$running" != "true" ]] && break
      sleep 1
    done
    if ! running="$(docker inspect -f '{{.State.Running}}' "$container")"; then
      return 1
    fi
    [[ "$running" != "true" ]] || return 1
  done
}
