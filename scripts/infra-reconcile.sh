#!/usr/bin/env bash
set -euo pipefail

state_file="runtime/deploy-state.json"
marker_file="runtime/bootstrap.marker"
had_state=false
npx --no-install tsx src/deployment-preflight.ts check "$PWD" "${POSTGRES_MODE:?POSTGRES_MODE is required}" "${REDIS_MODE:?REDIS_MODE is required}"
if [[ -f "$state_file" ]]; then
  had_state=true
  npx --no-install tsx scripts/deployment-mode.ts check "$state_file" "${POSTGRES_MODE:?POSTGRES_MODE is required}" "${REDIS_MODE:?REDIS_MODE is required}"
  active_slot="$(node -e 'const s=require("./runtime/deploy-state.json"); process.stdout.write(s.activeSlot)')"
  export SLOT="$active_slot" SLOT_DATA_DIR="$active_slot" AUTO_SETUP=false
  export SUB2API_IMAGE="$(node -e 'const s=require("./runtime/deploy-state.json"); process.stdout.write(s.activeImage)')"
else
  export SLOT=blue SLOT_DATA_DIR=blue AUTO_SETUP=true
fi

# This command reconciles runtime, data profiles, Traefik, and only the first slot.
bash scripts/deploy-compose.sh

if [[ "$had_state" == true ]]; then
  cp runtime/dynamic/active.yml runtime/dynamic/active.yml.before-infra
  trap 'mv -f runtime/dynamic/active.yml.before-infra runtime/dynamic/active.yml 2>/dev/null || true' ERR
  npx --no-install tsx scripts/render-route.ts write traefik/dynamic/active.yml runtime/dynamic/active.yml "$DOMAIN" "$active_slot"
  bash scripts/probe-origin-strict.sh "$DOMAIN" "$ORIGIN_IP" "/health"
  if [[ ! -f "$marker_file" ]]; then
    npx --no-install tsx scripts/write-bootstrap-marker.ts write "$marker_file"
  fi
  rm -f runtime/dynamic/active.yml.before-infra
  trap - ERR
fi
