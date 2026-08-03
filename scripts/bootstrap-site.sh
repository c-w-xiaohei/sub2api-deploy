#!/usr/bin/env bash
set -euo pipefail
umask 077

: "${SITE_ID:?SITE_ID is required}"
: "${SITE_RUNTIME_ROOT:?SITE_RUNTIME_ROOT is required}"
: "${SITE_RUNTIME_ENV_PATH:?SITE_RUNTIME_ENV_PATH is required}"
: "${SITE_DEPLOY_STATE_PATH:?SITE_DEPLOY_STATE_PATH is required}"
: "${SITE_BOOTSTRAP_MARKER_PATH:?SITE_BOOTSTRAP_MARKER_PATH is required}"
: "${BLUE_DATA_PATH:?BLUE_DATA_PATH is required}"
: "${GREEN_DATA_PATH:?GREEN_DATA_PATH is required}"
: "${COMPOSE_PROJECT_NAME:?COMPOSE_PROJECT_NAME is required}"
: "${SITE_ROUTE_PATH:?SITE_ROUTE_PATH is required}"
: "${EDGE_NETWORK_NAME:?EDGE_NETWORK_NAME is required}"
: "${BLUE_EDGE_ALIAS:?BLUE_EDGE_ALIAS is required}"
: "${DOMAIN:?DOMAIN is required}"
: "${ORIGIN_IP:?ORIGIN_IP is required}"
: "${SUB2API_IMAGE:?SUB2API_IMAGE is required}"
: "${RUNTIME_JSON:?RUNTIME_JSON is required}"
: "${POSTGRES_MODE:?POSTGRES_MODE is required}"
: "${REDIS_MODE:?REDIS_MODE is required}"
: "${CONFIGURED_SITE_IDS:?CONFIGURED_SITE_IDS is required}"
: "${HOST_STATE_PATH:?HOST_STATE_PATH is required}"

npx --no-install tsx scripts/host-preflight.ts check "$CONFIGURED_SITE_IDS" "$HOST_STATE_PATH"
npx --no-install tsx src/deployment-preflight.ts check "$SITE_DEPLOY_STATE_PATH" "$SITE_BOOTSTRAP_MARKER_PATH" "$POSTGRES_MODE" "$REDIS_MODE"
[[ ! -f "$SITE_DEPLOY_STATE_PATH" ]] || exit 0
mkdir -p "$BLUE_DATA_PATH" "$GREEN_DATA_PATH"
printf '%s' "$RUNTIME_JSON" | npx --no-install tsx scripts/render-runtime-env.ts write "$SITE_RUNTIME_ENV_PATH" --slot=blue --slot-data-dir=blue
export SLOT=blue SLOT_DATA_DIR=blue AUTO_SETUP=true
source scripts/site-compose-common.sh

data_services=()
[[ "$POSTGRES_MODE" == docker ]] && data_services+=(postgres)
[[ "$REDIS_MODE" == docker ]] && data_services+=(redis)
if ((${#data_services[@]})); then "${SITE_COMPOSE[@]}" up -d --wait --wait-timeout 300 "${data_services[@]}"; fi
"${SITE_COMPOSE[@]}" up -d --wait --wait-timeout 300 sub2api-blue
previous_route="$SITE_ROUTE_PATH.before-bootstrap"
had_route=false
[[ -f "$SITE_ROUTE_PATH" ]] && had_route=true
[[ -f "$SITE_ROUTE_PATH" ]] && cp "$SITE_ROUTE_PATH" "$previous_route"
npx --no-install tsx scripts/render-site-route.ts write traefik/dynamic/site.yml "$SITE_ROUTE_PATH" "$SITE_ID" "$DOMAIN" blue "$BLUE_EDGE_ALIAS"
if ! bash scripts/probe-origin-strict.sh "$DOMAIN" "$ORIGIN_IP" /health || ! bash scripts/probe-origin.sh "$DOMAIN" /health; then
  if [[ "$had_route" == true ]]; then mv -f "$previous_route" "$SITE_ROUTE_PATH"; else rm -f "$SITE_ROUTE_PATH"; fi
  exit 1
fi
rm -f "$previous_route"
npx --no-install tsx scripts/write-deploy-state.ts write "$SITE_DEPLOY_STATE_PATH" "{\"activeSlot\":\"blue\",\"activeImage\":\"$SUB2API_IMAGE\",\"postgresMode\":\"$POSTGRES_MODE\",\"redisMode\":\"$REDIS_MODE\"}"
npx --no-install tsx scripts/write-bootstrap-marker.ts write "$SITE_BOOTSTRAP_MARKER_PATH"
