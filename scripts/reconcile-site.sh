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
: "${GREEN_EDGE_ALIAS:?GREEN_EDGE_ALIAS is required}"
: "${DOMAIN:?DOMAIN is required}"
: "${ORIGIN_IP:?ORIGIN_IP is required}"
: "${RUNTIME_JSON:?RUNTIME_JSON is required}"
: "${POSTGRES_MODE:?POSTGRES_MODE is required}"
: "${REDIS_MODE:?REDIS_MODE is required}"
: "${APP_PROBE_PATH:?APP_PROBE_PATH is required}"
: "${CONFIGURED_SITE_IDS:?CONFIGURED_SITE_IDS is required}"
: "${HOST_STATE_PATH:?HOST_STATE_PATH is required}"

npx --no-install tsx scripts/host-preflight.ts check "$CONFIGURED_SITE_IDS" "$HOST_STATE_PATH"
npx --no-install tsx src/deployment-preflight.ts check "$SITE_DEPLOY_STATE_PATH" "$SITE_BOOTSTRAP_MARKER_PATH" "$POSTGRES_MODE" "$REDIS_MODE"
[[ -f "$SITE_DEPLOY_STATE_PATH" ]] || exit 0
npx --no-install tsx scripts/deployment-mode.ts check "$SITE_DEPLOY_STATE_PATH" "$POSTGRES_MODE" "$REDIS_MODE"
active_slot="$(node -e 'const s=require(process.argv[1]); process.stdout.write(s.activeSlot)' "$SITE_DEPLOY_STATE_PATH")"
SUB2API_IMAGE="$(node -e 'const s=require(process.argv[1]); process.stdout.write(s.activeImage)' "$SITE_DEPLOY_STATE_PATH")"
mkdir -p "$BLUE_DATA_PATH" "$GREEN_DATA_PATH"
printf '%s' "$RUNTIME_JSON" | npx --no-install tsx scripts/render-runtime-env.ts write "$SITE_RUNTIME_ENV_PATH" --slot="$active_slot" --slot-data-dir="$active_slot"
export SUB2API_IMAGE SLOT="$active_slot" SLOT_DATA_DIR="$active_slot" AUTO_SETUP=true
source scripts/site-compose-common.sh

data_services=()
[[ "$POSTGRES_MODE" == docker ]] && data_services+=(postgres)
[[ "$REDIS_MODE" == docker ]] && data_services+=(redis)
if ((${#data_services[@]})); then "${SITE_COMPOSE[@]}" up -d --wait --wait-timeout 300 "${data_services[@]}"; fi
"${SITE_COMPOSE[@]}" up -d --wait --wait-timeout 300 "sub2api-$active_slot"
alias="$BLUE_EDGE_ALIAS"; [[ "$active_slot" == green ]] && alias="$GREEN_EDGE_ALIAS"
previous_route="$SITE_ROUTE_PATH.before-reconcile"
had_route=false
[[ -f "$SITE_ROUTE_PATH" ]] && had_route=true
[[ -f "$SITE_ROUTE_PATH" ]] && cp "$SITE_ROUTE_PATH" "$previous_route"
npx --no-install tsx scripts/render-site-route.ts write traefik/dynamic/site.yml "$SITE_ROUTE_PATH" "$SITE_ID" "$DOMAIN" "$active_slot" "$alias"
if ! bash scripts/probe-origin-strict.sh "$DOMAIN" "$ORIGIN_IP" /health || ! bash scripts/probe-origin.sh "$DOMAIN" /health; then
  if [[ "$had_route" == true ]]; then mv -f "$previous_route" "$SITE_ROUTE_PATH"; else rm -f "$SITE_ROUTE_PATH"; fi
  exit 1
fi
rm -f "$previous_route"
