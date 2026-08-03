#!/usr/bin/env bash
set -euo pipefail
umask 077

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"
source scripts/compose-common.sh
if [[ -f runtime/deploy-state.json ]]; then
  npx --no-install tsx scripts/deployment-mode.ts check runtime/deploy-state.json \
    "${POSTGRES_MODE:-$(runtime_value POSTGRES_MODE)}" "${REDIS_MODE:-$(runtime_value REDIS_MODE)}"
fi
mkdir -p runtime runtime/data/blue runtime/data/green
mkdir -p runtime/dynamic
touch runtime/acme.json
chmod 600 runtime/acme.json

requested_image="${SUB2API_IMAGE:-}"
requested_slot="${SLOT:-blue}"
requested_data_dir="${SLOT_DATA_DIR:-$requested_slot}"
requested_auto_setup="${AUTO_SETUP:-}"

tmp_env="runtime/runtime.env.tmp.$$"
trap 'rm -f "$tmp_env"' EXIT
if [[ -n "${RUNTIME_JSON:-}" ]]; then
  render_args=(--slot="$requested_slot" --slot-data-dir="$requested_data_dir")
  if [[ "$requested_auto_setup" == "false" ]]; then
    printf '%s' "$RUNTIME_JSON" | npx --no-install tsx scripts/render-runtime-env.ts --auto-setup=false "${render_args[@]}" > "$tmp_env"
  else
    printf '%s' "$RUNTIME_JSON" | npx --no-install tsx scripts/render-runtime-env.ts "${render_args[@]}" > "$tmp_env"
  fi
else
  cp runtime/runtime.env "$tmp_env"
fi
chmod 600 "$tmp_env"
mv -f "$tmp_env" runtime/runtime.env

[[ -n "$requested_image" ]] && export SUB2API_IMAGE="$requested_image"
export SLOT="$requested_slot" SLOT_DATA_DIR="$requested_data_dir"
[[ -n "$requested_auto_setup" ]] && export AUTO_SETUP="$requested_auto_setup"

POSTGRES_MODE="${POSTGRES_MODE:-$(runtime_value POSTGRES_MODE)}"
REDIS_MODE="${REDIS_MODE:-$(runtime_value REDIS_MODE)}"
CLOUDFLARE_DNS_API_TOKEN="${CLOUDFLARE_DNS_API_TOKEN:-$(runtime_value CLOUDFLARE_DNS_API_TOKEN)}"
DOMAIN="${DOMAIN:-$(runtime_value DOMAIN)}"
TRAEFIK_IMAGE="${TRAEFIK_IMAGE:-$(runtime_value TRAEFIK_IMAGE)}"
ACME_EMAIL="${ACME_EMAIL:-$(runtime_value ACME_EMAIL)}"
ORIGIN_IP="${ORIGIN_IP:-$(runtime_value ORIGIN_IP)}"
APP_PROBE_PATH="${APP_PROBE_PATH:-$(runtime_value APP_PROBE_PATH)}"
AUTO_SETUP="${AUTO_SETUP:-$(runtime_value AUTO_SETUP)}"
DRAIN_SECONDS="${DRAIN_SECONDS:-$(runtime_value DRAIN_SECONDS)}"
export POSTGRES_MODE REDIS_MODE DOMAIN TRAEFIK_IMAGE ACME_EMAIL ORIGIN_IP APP_PROBE_PATH AUTO_SETUP DRAIN_SECONDS

: "${SUB2API_IMAGE:?SUB2API_IMAGE is required}"
: "${POSTGRES_MODE:?POSTGRES_MODE is required}"
: "${REDIS_MODE:?REDIS_MODE is required}"
: "${DOMAIN:?DOMAIN is required}"
: "${TRAEFIK_IMAGE:?TRAEFIK_IMAGE is required}"
: "${CLOUDFLARE_DNS_API_TOKEN:?CLOUDFLARE_DNS_API_TOKEN is required}"
: "${ACME_EMAIL:?ACME_EMAIL is required}"
: "${ORIGIN_IP:?ORIGIN_IP is required}"
: "${APP_PROBE_PATH:?APP_PROBE_PATH is required}"

npx --no-install tsx scripts/render-traefik-config.ts write traefik/traefik.yml traefik/generated.yml "$ACME_EMAIL"

case "$requested_slot" in
  blue|green) slot_service="sub2api-${requested_slot}" ;;
  *) printf '%s\n' "SLOT must be blue or green" >&2; exit 1 ;;
esac

if [[ ! -f runtime/deploy-state.json ]]; then
  npx --no-install tsx scripts/render-route.ts write traefik/dynamic/active.yml runtime/dynamic/active.yml "$DOMAIN" "$requested_slot"
fi

data_services=()
[[ "$POSTGRES_MODE" == "docker" ]] && data_services+=(postgres)
[[ "$REDIS_MODE" == "docker" ]] && data_services+=(redis)
[[ "$POSTGRES_MODE" != "docker" ]] && "${COMPOSE[@]}" stop postgres >/dev/null 2>&1 || true
[[ "$REDIS_MODE" != "docker" ]] && "${COMPOSE[@]}" stop redis >/dev/null 2>&1 || true
if ((${#data_services[@]})); then
  "${COMPOSE[@]}" up -d --wait --wait-timeout 300 "${data_services[@]}"
fi
"${COMPOSE[@]}" up -d --wait --wait-timeout 300 --force-recreate traefik
"${COMPOSE[@]}" up -d --wait --wait-timeout 300 "$slot_service"

if [[ "${AUTO_SETUP:-false}" == "true" ]]; then
  printf '%s' "$RUNTIME_JSON" | npx --no-install tsx scripts/render-runtime-env.ts --auto-setup=false \
    --slot="$requested_slot" --slot-data-dir="$requested_data_dir" > "$tmp_env"
  chmod 600 "$tmp_env"
  mv -f "$tmp_env" runtime/runtime.env
  "${COMPOSE[@]}" up -d --wait --wait-timeout 300 --force-recreate "$slot_service"
fi
if [[ ! -f runtime/deploy-state.json ]]; then
  bash scripts/probe-origin-strict.sh "$DOMAIN" "$ORIGIN_IP" "/health"
fi

if [[ ! -f runtime/deploy-state.json ]]; then
  npx --no-install tsx scripts/write-deploy-state.ts write runtime/deploy-state.json "$(printf '%s' "{\"activeSlot\":\"${SLOT}\",\"activeImage\":\"${SUB2API_IMAGE}\",\"postgresMode\":\"${POSTGRES_MODE}\",\"redisMode\":\"${REDIS_MODE}\"}")"
  npx --no-install tsx scripts/write-bootstrap-marker.ts write runtime/bootstrap.marker
fi
