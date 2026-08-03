#!/usr/bin/env bash
set -euo pipefail

: "${SITE_DEPLOY_STATE_PATH:?SITE_DEPLOY_STATE_PATH is required}"
: "${SUB2API_IMAGE:?SUB2API_IMAGE is required}"
: "${DOMAIN:?DOMAIN is required}"
if [[ ! -f "$SITE_DEPLOY_STATE_PATH" ]]; then
  exec bash scripts/bootstrap-site.sh
fi
source scripts/site-compose-common.sh
npx --no-install tsx scripts/deployment-mode.ts check "$SITE_DEPLOY_STATE_PATH" "${POSTGRES_MODE:?POSTGRES_MODE is required}" "${REDIS_MODE:?REDIS_MODE is required}"
active_image="$(node -e 'const s=require(process.argv[1]); process.stdout.write(s.activeImage)' "$SITE_DEPLOY_STATE_PATH")"
[[ "$active_image" == "$SUB2API_IMAGE" ]] && exit 0
bash scripts/switch-slot.sh "$SUB2API_IMAGE" "$DOMAIN"
