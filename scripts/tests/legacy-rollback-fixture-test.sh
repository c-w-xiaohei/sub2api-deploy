#!/usr/bin/env bash
set -euo pipefail

root="$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)"
tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT
bin="$tmp/bin"; runtime="$tmp/runtime"; log="$tmp/calls"; mkdir -p "$bin" "$runtime"
cp -a "$root/scripts" "$tmp/scripts"; cp -a "$root/compose" "$tmp/compose"
printf '{"activeSlot":"green","previousSlot":"blue","previousImage":"old","postgresMode":"neon","redisMode":"upstash"}\n' > "$runtime/state.json"
printf 'POSTGRES_MODE=neon\nREDIS_MODE=upstash\n' > "$runtime/runtime.env"
printf 'current-route\n' > "$runtime/route.yml"
cat > "$bin/sub2api-deploy" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail; printf 'RUNTIME %s\n' "$*" >> "$FAKE_LOG"
case "$2" in
read-env) grep -E "^$4=" "$3" | cut -d= -f2- ;;
deployment-mode) [[ "$3" == check ]] && exit 0 ;;
read-state) python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))[sys.argv[2]])' "$3" "$4" ;;
route) [[ "${FAIL_ROUTE:-}" != 1 ]]; printf 'new-route\n' > "$5" ;;
swap-state) printf '{"activeSlot":"blue","previousSlot":"green","previousImage":"new","postgresMode":"neon","redisMode":"upstash"}' ;;
state) printf '%s\n' "$5" > "$4" ;;
*) exit 97 ;;
esac
EOF
cat > "$bin/docker" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail; printf 'DOCKER %s\n' "$*" >> "$FAKE_LOG"
if [[ "$1" == inspect ]]; then printf false; exit 0; fi
if [[ "$1" == compose ]]; then
  case " ${*} " in
    *' stop --timeout 30 sub2api-blue '*|*' stop --timeout 30 sub2api-green '*) exit 0 ;;
    *' ps -q sub2api-blue '*|*' ps -q sub2api-green '*) printf 'slot-container\n'; exit 0 ;;
    *' up -d --wait --wait-timeout 300 sub2api-blue '*) exit 0 ;;
    *' exec -T sub2api-blue wget '*) [[ "${FAIL_INTERNAL:-}" != 1 ]]; exit ;;
  esac
fi
exit 94
EOF
cat > "$bin/bash" <<'EOF'
#!/bin/bash
if [[ "$1" == scripts/probe-origin.sh ]]; then printf 'PROBE\n' >> "$FAKE_LOG"; [[ "${FAIL_PROBE:-}" != 1 ]]; exit; fi
exec /bin/bash "$@"
EOF
chmod 755 "$bin"/*
run() { (cd "$tmp"; PATH="$bin:$PATH" FAKE_LOG="$log" SITE_ID=code2 SITE_RUNTIME_ROOT="$runtime" SITE_APP_ENV_PATH="$runtime/app.env" COMPOSE_PROJECT_NAME=site-code2 SITE_ROUTE_PATH="$runtime/route.yml" EDGE_NETWORK_NAME=sub2api-edge BLUE_EDGE_ALIAS=code2-blue GREEN_EDGE_ALIAS=code2-green SITE_DEPLOY_STATE_PATH="$runtime/state.json" POSTGRES_MODE=neon REDIS_MODE=upstash APP_PROBE_PATH=/ready PROBE_RETRIES=1 PROBE_DELAY_SECONDS=0 bash scripts/rollback-slot.sh code2.example.test); }
run
[[ "$(cat "$runtime/route.yml")" == new-route ]] || { printf 'success did not replace route\n' >&2; exit 1; }
grep -q '"activeSlot":"blue"' "$runtime/state.json"
printf 'current-route\n' > "$runtime/route.yml"
printf '{"activeSlot":"green","previousSlot":"blue","previousImage":"old","postgresMode":"neon","redisMode":"upstash"}\n' > "$runtime/state.json"
if FAIL_ROUTE=1 run; then printf 'failed route write accepted\n' >&2; exit 1; fi
[[ "$(cat "$runtime/route.yml")" == current-route ]] || { printf 'route changed after route-write failure\n' >&2; exit 1; }
grep -q '"activeSlot":"green"' "$runtime/state.json"
if FAIL_PROBE=1 run; then printf 'failed public probe accepted\n' >&2; exit 1; fi
[[ "$(cat "$runtime/route.yml")" == current-route ]] || { printf 'route was not restored after probe failure\n' >&2; exit 1; }
grep -q 'stop --timeout 30 sub2api-blue' "$log"
if FAIL_INTERNAL=1 run; then printf 'failed internal probe accepted\n' >&2; exit 1; fi
grep -q 'stop --timeout 30 sub2api-blue' "$log"
printf 'legacy rollback fixture passed\n'
