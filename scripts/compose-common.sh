#!/usr/bin/env bash

# Every Compose invocation uses the same project, env file, profiles, and files.
COMPOSE=(
  docker compose
  --project-name sub2api
  --env-file runtime/runtime.env
  --profile postgres
  --profile redis
  --profile app
  -f compose/upstream.yml
  -f compose/override.yml
  -f compose/edge.yml
)

runtime_value() {
  node scripts/read-runtime-env.cjs "$1"
}

stop_service() {
  local service="$1"
  "${COMPOSE[@]}" stop --timeout 30 "$service" >/dev/null 2>&1 || true
  local container
  for container in $("${COMPOSE[@]}" ps -q "$service" 2>/dev/null); do
    for _ in 1 2 3 4 5 6; do
      [[ "$(docker inspect -f '{{.State.Running}}' "$container" 2>/dev/null || true)" != "true" ]] && break
      sleep 1
    done
    [[ "$(docker inspect -f '{{.State.Running}}' "$container" 2>/dev/null || true)" != "true" ]] || return 1
  done
}
