#!/usr/bin/env bash
set -euo pipefail
umask 077

: "${EDGE_RUNTIME_ROOT:?EDGE_RUNTIME_ROOT is required}"
: "${EDGE_NETWORK_NAME:?EDGE_NETWORK_NAME is required}"
: "${TRAEFIK_IMAGE:?TRAEFIK_IMAGE is required}"
: "${ACME_EMAIL:?ACME_EMAIL is required}"
: "${CLOUDFLARE_API_TOKEN:?CLOUDFLARE_API_TOKEN is required}"
: "${SING_BOX_CONFIG:?SING_BOX_CONFIG is required}"

source scripts/edge-compose-common.sh

sing_box_server_name="$(node -e 'const value=JSON.parse(process.argv[1]); process.stdout.write(value.serverName)' "$SING_BOX_CONFIG")"
sing_box_target="$(node -e 'const value=JSON.parse(process.argv[1]); process.stdout.write(value.target)' "$SING_BOX_CONFIG")"
mkdir -p "$EDGE_RUNTIME_ROOT/dynamic"
node -e 'process.stdout.write(JSON.stringify({TRAEFIK_IMAGE: process.env.TRAEFIK_IMAGE, ACME_EMAIL: process.env.ACME_EMAIL, CLOUDFLARE_DNS_API_TOKEN: process.env.CLOUDFLARE_API_TOKEN, EDGE_RUNTIME_ROOT: process.env.EDGE_RUNTIME_ROOT}))' \
  | npx --no-install tsx scripts/render-runtime-env.ts write "$EDGE_RUNTIME_ROOT/edge.env"
npx --no-install tsx scripts/render-edge-config.ts write "$EDGE_RUNTIME_ROOT" traefik/traefik.yml traefik/dynamic/sing-box.yml "$ACME_EMAIL" "$sing_box_server_name" "$sing_box_target"
if [[ ! -f "$EDGE_RUNTIME_ROOT/acme.json" ]]; then
  temporary="$EDGE_RUNTIME_ROOT/.acme.$$.tmp"
  : > "$temporary"
  chmod 600 "$temporary"
  mv -f "$temporary" "$EDGE_RUNTIME_ROOT/acme.json"
fi
chmod 600 "$EDGE_RUNTIME_ROOT/acme.json"

"${EDGE_COMPOSE[@]}" up -d --wait --wait-timeout 300 traefik
