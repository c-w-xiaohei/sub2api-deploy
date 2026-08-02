#!/usr/bin/env bash
set -euo pipefail
umask 077

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"
source scripts/compose-common.sh

image="${1:?image is required}"
domain="${2:?domain is required}"
state_file="runtime/deploy-state.json"
POSTGRES_MODE="${POSTGRES_MODE:-$(runtime_value POSTGRES_MODE)}"
REDIS_MODE="${REDIS_MODE:-$(runtime_value REDIS_MODE)}"
npx --no-install tsx scripts/deployment-mode.ts check "$state_file" "$POSTGRES_MODE" "$REDIS_MODE"
drain_seconds="${DRAIN_SECONDS:-}"
APP_PROBE_PATH="${APP_PROBE_PATH:-$(runtime_value APP_PROBE_PATH)}"
DRAIN_SECONDS="${DRAIN_SECONDS:-$(runtime_value DRAIN_SECONDS)}"
export APP_PROBE_PATH DRAIN_SECONDS
drain_seconds="${DRAIN_SECONDS:-10}"
public_probe_path="/health"

if [[ -f "$state_file" ]]; then
  active_slot="$(node -e 'const s=require("./runtime/deploy-state.json"); process.stdout.write(s.activeSlot)')"
else
  active_slot=blue
fi
previous_image="$(node -e 'const s=require("./runtime/deploy-state.json"); process.stdout.write(s.activeImage)')"
if [[ "$active_slot" == blue ]]; then inactive_slot=green; else inactive_slot=blue; fi

# The inactive volume must be stopped before copying install/config markers.
stop_service "sub2api-${inactive_slot}"
mkdir -p "runtime/data/${active_slot}" "runtime/data/${inactive_slot}"
if [[ -f "runtime/data/${active_slot}/config.yaml" ]]; then cp -p "runtime/data/${active_slot}/config.yaml" "runtime/data/${inactive_slot}/config.yaml"; fi
if [[ -f "runtime/data/${active_slot}/.installed" ]]; then cp -p "runtime/data/${active_slot}/.installed" "runtime/data/${inactive_slot}/.installed"; fi

export SUB2API_IMAGE="$image" SLOT="$inactive_slot" SLOT_DATA_DIR="$inactive_slot" AUTO_SETUP=false
"${COMPOSE[@]}" up -d --wait --wait-timeout 120 "sub2api-${inactive_slot}"

internal_probe_url="http://127.0.0.1:8080${APP_PROBE_PATH:?APP_PROBE_PATH is required}"
probe_retries="${PROBE_RETRIES:-30}"
for ((attempt = 1; attempt <= probe_retries; attempt++)); do
  if "${COMPOSE[@]}" exec -T "sub2api-${inactive_slot}" wget -q -T 10 -O /dev/null "$internal_probe_url"; then break; fi
  if [[ "$attempt" -eq "$probe_retries" ]]; then
    stop_service "sub2api-${inactive_slot}"
    exit 1
  fi
  sleep "${PROBE_DELAY_SECONDS:-2}"
done

cp runtime/dynamic/active.yml runtime/dynamic/active.yml.before-switch
npx --no-install tsx scripts/render-route.ts write traefik/dynamic/active.yml runtime/dynamic/active.yml "$domain" "$inactive_slot"
if ! bash scripts/probe-origin.sh "$domain" "$public_probe_path"; then
  mv -f runtime/dynamic/active.yml.before-switch runtime/dynamic/active.yml
  stop_service "sub2api-${inactive_slot}"
  exit 1
fi

sleep "$drain_seconds"
stop_service "sub2api-${active_slot}"
state_json="$(node -e 'const s=require("./runtime/deploy-state.json"); s.previousSlot=s.activeSlot; s.previousImage=s.activeImage; s.activeSlot=process.argv[1]; s.activeImage=process.argv[2]; process.stdout.write(JSON.stringify(s))' "$inactive_slot" "$image")"
npx --no-install tsx scripts/write-deploy-state.ts write runtime/deploy-state.json "$state_json"
