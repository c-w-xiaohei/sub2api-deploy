#!/usr/bin/env bash
set -euo pipefail

state_file="runtime/deploy-state.json"
[[ -f "$state_file" ]] || exit 0
postgres_mode="${POSTGRES_MODE:-$(node scripts/read-runtime-env.cjs POSTGRES_MODE)}"
redis_mode="${REDIS_MODE:-$(node scripts/read-runtime-env.cjs REDIS_MODE)}"
npx --no-install tsx scripts/deployment-mode.ts check "$state_file" "$postgres_mode" "$redis_mode"
active_image="$(node -e 'const s=require("./runtime/deploy-state.json"); process.stdout.write(s.activeImage)')"
if [[ "$active_image" == "${SUB2API_IMAGE:?SUB2API_IMAGE is required}" ]]; then
  exit 0
fi
bash scripts/switch-slot.sh "${SUB2API_IMAGE:?SUB2API_IMAGE is required}" "${DOMAIN:-$(node scripts/read-runtime-env.cjs DOMAIN)}"
