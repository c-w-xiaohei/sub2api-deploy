#!/usr/bin/env bash
# Exercise shell callers against a fake runtime command; no Go compiler is used.
set -euo pipefail
root="$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)"
fixture="$(mktemp -d)"
trap 'rm -rf "$fixture"' EXIT
mkdir -p "$fixture/bin" "$fixture/templates/traefik/dynamic" "$fixture/runtime"

cat > "$fixture/bin/sub2api-deploy" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
shift
case "$1 ${2:-}" in
  'json-field --stdin')
    input="$(cat)"
    [[ "$input" == '{"serverName":"www.cloudflare.com","target":"host.docker.internal:8443"}' ]]
    case "$3" in serverName) printf www.cloudflare.com ;; target) printf host.docker.internal:8443 ;; *) exit 1 ;; esac
    ;;
  'edge-env ')
    printf '{"TRAEFIK_IMAGE":"traefik:test","ACME_EMAIL":"ops@example.test"}\n'
    ;;
  'dotenv write')
    cat >/dev/null
    mkdir -p "$(dirname "$3")"
    : > "$3"
    chmod 600 "$3"
    ;;
  'edge write')
    [[ "$7" == www.cloudflare.com && "$8" == host.docker.internal:8443 ]]
    mkdir -p "$3/dynamic"
    : > "$3/traefik.yml"
    : > "$3/dynamic/00-sing-box.yml"
    chmod 600 "$3/traefik.yml" "$3/dynamic/00-sing-box.yml"
    ;;
  *) printf 'unexpected runtime invocation: %s\n' "$*" >&2; exit 1 ;;
esac
EOF
cat > "$fixture/bin/docker" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
[[ "$*" == *'up -d --wait --wait-timeout 300 traefik' ]]
EOF
chmod 755 "$fixture/bin/sub2api-deploy" "$fixture/bin/docker"

EDGE_RUNTIME_ROOT="$fixture/runtime" \
EDGE_NETWORK_NAME=sub2api-edge \
TRAEFIK_IMAGE=traefik:test \
ACME_EMAIL=ops@example.test \
CLOUDFLARE_API_TOKEN=token \
SING_BOX_CONFIG='{"serverName":"www.cloudflare.com","target":"host.docker.internal:8443"}' \
PATH="$fixture/bin:$PATH" \
  /bin/bash "$root/scripts/reconcile-edge.sh"

for path in "$fixture/runtime/edge.env" "$fixture/runtime/traefik.yml" "$fixture/runtime/dynamic/00-sing-box.yml" "$fixture/runtime/acme.json"; do
  [[ -f "$path" && "$(stat -c %a "$path")" == 600 ]]
done

cat > "$fixture/bin/sub2api-deploy" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
shift
case "$1 ${2:-}" in
  'deployment-mode check') ;;
  read-env\ *)
    case "$3" in POSTGRES_MODE|REDIS_MODE) printf docker ;; *) exit 1 ;; esac
    ;;
  read-state\ *)
    case "$3" in activeSlot) printf blue ;; activeImage) printf old-image ;; previousSlot) printf green ;; previousImage) printf old-image ;; *) exit 1 ;; esac
    ;;
  'route write')
    printf 'new route\n' > "$4"
    ;;
  transition-state\ *|swap-state\ *)
    printf '{"activeSlot":"green","activeImage":"new-image","previousSlot":"blue","previousImage":"old-image"}'
    ;;
  'state write')
    printf '%s\n' "$4" > "$3"
    chmod 600 "$3"
    ;;
  *) printf 'unexpected slot runtime invocation: %s\n' "$*" >&2; exit 1 ;;
esac
EOF
cat > "$fixture/bin/docker" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >> "$FAKE_LOG"
case "$*" in
  *'exec -T sub2api-green wget'*|*'exec -T sub2api-blue wget'*) exit 0 ;;
  'inspect -f {{.State.Running}} '*) printf false ;;
  *) exit 0 ;;
esac
EOF
cat > "$fixture/bin/sleep" <<'EOF'
#!/usr/bin/env bash
exit 0
EOF
cat > "$fixture/bin/bash" <<'EOF'
#!/bin/bash
if [[ "$1" == scripts/probe-origin.sh ]]; then exit "${PROBE_RESULT:-0}"; fi
exec /bin/bash "$@"
EOF
chmod 755 "$fixture/bin/sub2api-deploy" "$fixture/bin/docker" "$fixture/bin/sleep" "$fixture/bin/bash"
mkdir -p "$fixture/site/data/blue" "$fixture/site/data/green"
printf 'POSTGRES_MODE="docker"\nREDIS_MODE="docker"\n' > "$fixture/site/runtime.env"
: > "$fixture/site/app.env"
printf 'old route\n' > "$fixture/site/route.yml"
printf '{"activeSlot":"blue","activeImage":"old-image","previousSlot":"green","previousImage":"older-image","postgresMode":"docker","redisMode":"docker"}\n' > "$fixture/site/state.json"
SITE_DEPLOY_STATE_PATH="$fixture/site/state.json" SITE_RUNTIME_ROOT="$fixture/site" SITE_APP_ENV_PATH="$fixture/site/app.env" COMPOSE_PROJECT_NAME=sub2api-code2 EDGE_NETWORK_NAME=sub2api-edge SITE_ROUTE_PATH="$fixture/site/route.yml" SITE_ID=code2 BLUE_EDGE_ALIAS=sub2api-blue GREEN_EDGE_ALIAS=sub2api-green POSTGRES_MODE=docker REDIS_MODE=docker APP_PROBE_PATH=/ready DRAIN_SECONDS=0 FAKE_LOG="$fixture/slot.log" PATH="$fixture/bin:$PATH" bash "$root/scripts/switch-slot.sh" new-image code2.example.test
grep -q 'stop --timeout 30 sub2api-blue' "$fixture/slot.log"
[[ "$(cat "$fixture/site/route.yml")" == 'new route' ]]

printf 'old route\n' > "$fixture/site/route.yml"
if PROBE_RESULT=1 SITE_DEPLOY_STATE_PATH="$fixture/site/state.json" SITE_RUNTIME_ROOT="$fixture/site" SITE_APP_ENV_PATH="$fixture/site/app.env" COMPOSE_PROJECT_NAME=sub2api-code2 EDGE_NETWORK_NAME=sub2api-edge SITE_ROUTE_PATH="$fixture/site/route.yml" SITE_ID=code2 BLUE_EDGE_ALIAS=sub2api-blue GREEN_EDGE_ALIAS=sub2api-green POSTGRES_MODE=docker REDIS_MODE=docker APP_PROBE_PATH=/ready DRAIN_SECONDS=0 FAKE_LOG="$fixture/slot-failure.log" PATH="$fixture/bin:$PATH" bash "$root/scripts/switch-slot.sh" new-image code2.example.test >/dev/null 2>&1; then
  printf 'switch-slot accepted a failed public probe\n' >&2
  exit 1
fi
[[ "$(cat "$fixture/site/route.yml")" == 'old route' ]]
printf 'Legacy runtime caller fixture passed\n'
