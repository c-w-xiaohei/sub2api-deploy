#!/usr/bin/env bash
set -euo pipefail
umask 077

source scripts/site-compose-common.sh
APP_PROBE_PATH="${APP_PROBE_PATH:?APP_PROBE_PATH is required}"
export APP_PROBE_PATH
state_file="${SITE_DEPLOY_STATE_PATH:?SITE_DEPLOY_STATE_PATH is required}"
[[ -f "$state_file" ]] || { printf '%s\n' "no deployment state" >&2; exit 1; }
POSTGRES_MODE="${POSTGRES_MODE:?POSTGRES_MODE is required}"
REDIS_MODE="${REDIS_MODE:?REDIS_MODE is required}"
npx --no-install tsx scripts/deployment-mode.ts check "$state_file" "$POSTGRES_MODE" "$REDIS_MODE"
previous_slot="$(node -e 'const s=JSON.parse(require("fs").readFileSync(process.argv[1], "utf8")); if (!s.previousSlot || !s.previousImage) process.exit(1); process.stdout.write(s.previousSlot)' "$state_file")"
previous_image="$(node -e 'const s=JSON.parse(require("fs").readFileSync(process.argv[1], "utf8")); process.stdout.write(s.previousImage)' "$state_file")"
active_slot="$(node -e 'const s=JSON.parse(require("fs").readFileSync(process.argv[1], "utf8")); process.stdout.write(s.activeSlot)' "$state_file")"
domain="${1:?domain is required}"

site_stop_service "sub2api-${previous_slot}"
export SUB2API_IMAGE="$previous_image" SLOT="$previous_slot" SLOT_DATA_DIR="$previous_slot" AUTO_SETUP=false
"${SITE_COMPOSE[@]}" up -d --wait --wait-timeout 300 "sub2api-${previous_slot}"
internal_probe_url="http://127.0.0.1:8080${APP_PROBE_PATH:?APP_PROBE_PATH is required}"
probe_retries="${PROBE_RETRIES:-30}"
for ((attempt = 1; attempt <= probe_retries; attempt++)); do
  if "${SITE_COMPOSE[@]}" exec -T "sub2api-${previous_slot}" wget -q -T 10 -O /dev/null "$internal_probe_url"; then
    break
  fi
  if [[ "$attempt" -eq "$probe_retries" ]]; then
    site_stop_service "sub2api-${previous_slot}"
    exit 1
  fi
  sleep "${PROBE_DELAY_SECONDS:-2}"
done
previous_route="$SITE_ROUTE_PATH.before-rollback"
had_route=false
[[ -f "$SITE_ROUTE_PATH" ]] && had_route=true
[[ -f "$SITE_ROUTE_PATH" ]] && cp "$SITE_ROUTE_PATH" "$previous_route"
edge_alias="$BLUE_EDGE_ALIAS"; [[ "$previous_slot" == green ]] && edge_alias="$GREEN_EDGE_ALIAS"
npx --no-install tsx scripts/render-site-route.ts write traefik/dynamic/site.yml "$SITE_ROUTE_PATH" "$SITE_ID" "$domain" "$previous_slot" "$edge_alias"
if ! bash scripts/probe-origin.sh "$domain" "/health"; then
  if [[ "$had_route" == true ]]; then mv -f "$previous_route" "$SITE_ROUTE_PATH"; else rm -f "$SITE_ROUTE_PATH"; fi
  site_stop_service "sub2api-${previous_slot}"
  exit 1
fi
rm -f "$previous_route"
site_stop_service "sub2api-${active_slot}"
state_json="$(node -e 'const s=JSON.parse(require("fs").readFileSync(process.argv[1], "utf8")); const old={activeSlot:s.activeSlot,activeImage:s.activeImage}; s.activeSlot=s.previousSlot; s.activeImage=s.previousImage; s.previousSlot=old.activeSlot; s.previousImage=old.activeImage; process.stdout.write(JSON.stringify(s))' "$state_file")"
npx --no-install tsx scripts/write-deploy-state.ts write "$state_file" "$state_json"
