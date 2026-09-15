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

sing_box_server_name="$(printf '%s' "$SING_BOX_CONFIG" | sub2api-deploy runtime json-field --stdin serverName)"
sing_box_target="$(printf '%s' "$SING_BOX_CONFIG" | sub2api-deploy runtime json-field --stdin target)"
mkdir -p "$EDGE_RUNTIME_ROOT/dynamic"
sub2api-deploy runtime edge-env \
  | sub2api-deploy runtime dotenv write "$EDGE_RUNTIME_ROOT/edge.env"
sub2api-deploy runtime edge write "$EDGE_RUNTIME_ROOT" traefik/traefik.yml traefik/dynamic/sing-box.yml "$ACME_EMAIL" "$sing_box_server_name" "$sing_box_target"
if [[ ! -f "$EDGE_RUNTIME_ROOT/acme.json" ]]; then
  temporary="$EDGE_RUNTIME_ROOT/.acme.$$.tmp"
  : > "$temporary"
  chmod 600 "$temporary"
  mv -f "$temporary" "$EDGE_RUNTIME_ROOT/acme.json"
fi
chmod 600 "$EDGE_RUNTIME_ROOT/acme.json"

"${EDGE_COMPOSE[@]}" up -d --wait --wait-timeout 300 traefik
