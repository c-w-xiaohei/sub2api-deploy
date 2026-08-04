#!/usr/bin/env bash

: "${EDGE_RUNTIME_ROOT:?EDGE_RUNTIME_ROOT is required}"
EDGE_RUNTIME_ROOT="$(realpath -m "$EDGE_RUNTIME_ROOT")"
export EDGE_RUNTIME_ROOT

EDGE_COMPOSE=(
  docker compose
  --project-name sub2api-edge
  --env-file "$EDGE_RUNTIME_ROOT/edge.env"
  -f compose/edge.yml
)
