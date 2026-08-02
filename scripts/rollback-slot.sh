#!/usr/bin/env bash
set -euo pipefail
umask 077

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"
source scripts/compose-common.sh
COMPOSE=("${COMPOSE[@]}")
APP_PROBE_PATH="$(runtime_value APP_PROBE_PATH)"
export APP_PROBE_PATH
state_file="runtime/deploy-state.json"
[[ -f "$state_file" ]] || { printf '%s\n' "no deployment state" >&2; exit 1; }
POSTGRES_MODE="${POSTGRES_MODE:-$(runtime_value POSTGRES_MODE)}"
REDIS_MODE="${REDIS_MODE:-$(runtime_value REDIS_MODE)}"
npx --no-install tsx scripts/deployment-mode.ts check "$state_file" "$POSTGRES_MODE" "$REDIS_MODE"
previous_slot="$(node -e 'const s=require("./runtime/deploy-state.json"); if (!s.previousSlot || !s.previousImage) process.exit(1); process.stdout.write(s.previousSlot)')"
previous_image="$(node -e 'const s=require("./runtime/deploy-state.json"); process.stdout.write(s.previousImage)')"
active_slot="$(node -e 'const s=require("./runtime/deploy-state.json"); process.stdout.write(s.activeSlot)')"
domain="${1:?domain is required}"

stop_service "sub2api-${previous_slot}"
export SUB2API_IMAGE="$previous_image" SLOT="$previous_slot" SLOT_DATA_DIR="$previous_slot" AUTO_SETUP=false
bash scripts/deploy-compose.sh
internal_probe_url="http://127.0.0.1:8080${APP_PROBE_PATH:?APP_PROBE_PATH is required}"
probe_retries="${PROBE_RETRIES:-30}"
for ((attempt = 1; attempt <= probe_retries; attempt++)); do
  if "${COMPOSE[@]}" exec -T "sub2api-${previous_slot}" wget -q -T 10 -O /dev/null "$internal_probe_url"; then
    break
  fi
  if [[ "$attempt" -eq "$probe_retries" ]]; then
    stop_service "sub2api-${previous_slot}"
    exit 1
  fi
  sleep "${PROBE_DELAY_SECONDS:-2}"
done
cp runtime/dynamic/active.yml runtime/dynamic/active.yml.before-rollback
npx --no-install tsx scripts/render-route.ts write traefik/dynamic/active.yml runtime/dynamic/active.yml "$domain" "$previous_slot"
if ! bash scripts/probe-origin.sh "$domain" "/health"; then
  mv -f runtime/dynamic/active.yml.before-rollback runtime/dynamic/active.yml
  stop_service "sub2api-${previous_slot}"
  exit 1
fi
stop_service "sub2api-${active_slot}"
state_json="$(node -e 'const s=require("./runtime/deploy-state.json"); const old={activeSlot:s.activeSlot,activeImage:s.activeImage}; s.activeSlot=s.previousSlot; s.activeImage=s.previousImage; s.previousSlot=old.activeSlot; s.previousImage=old.activeImage; process.stdout.write(JSON.stringify(s))')"
npx --no-install tsx scripts/write-deploy-state.ts write runtime/deploy-state.json "$state_json"
