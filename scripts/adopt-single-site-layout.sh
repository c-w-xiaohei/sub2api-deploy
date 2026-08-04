#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

usage() { printf 'usage: %s --environment ENVIRONMENT --site code2 [--host-state PATH] [--prepare-preview|--apply|--rollback|--retire-journal]\n' "$0" >&2; exit 2; }
environment=""; site=""; host_state="runtime/host-state.json"; mode=dry-run
while (($#)); do case "$1" in --environment) environment="${2:-}"; shift 2 ;; --site) site="${2:-}"; shift 2 ;; --host-state) host_state="${2:-}"; shift 2 ;; --prepare-preview) mode=prepare-preview; shift ;; --apply) mode=apply; shift ;; --rollback) mode=rollback; shift ;; --retire-journal) mode=retire-journal; shift ;; *) usage ;; esac; done
[[ -n "$environment" && "$site" == code2 ]] || usage
runtime_root="$(dirname "$host_state")"; legacy_state="$runtime_root/deploy-state.json"; legacy_state_backup="$runtime_root/deploy-state.json.before-adoption"; legacy_acme="$runtime_root/acme.json"
edge_root="$runtime_root/edge"; edge_env="$edge_root/edge.env"; edge_static="$edge_root/traefik.yml"; edge_singbox="$edge_root/dynamic/00-sing-box.yml"; edge_acme="$edge_root/acme.json"; route="$edge_root/dynamic/site-code2.yml"; route_backup="$route.before-adoption"; journal="$runtime_root/adopt-single-site-layout.journal"

require_legacy() { [[ -f "$legacy_state" && ! -e "$host_state" ]] || { printf 'expected legacy deploy state and no host state\n' >&2; exit 1; }; }
require_apply_inputs() { : "${TRAEFIK_IMAGE:?TRAEFIK_IMAGE is required}" "${CLOUDFLARE_API_TOKEN:?CLOUDFLARE_API_TOKEN is required}" "${ACME_EMAIL:?ACME_EMAIL is required}" "${SING_BOX_SERVER_NAME:?SING_BOX_SERVER_NAME is required}" "${SING_BOX_TARGET:?SING_BOX_TARGET is required}" "${DOMAIN:?DOMAIN is required}" "${ORIGIN_IP:?ORIGIN_IP is required}" "${APP_PROBE_PATH:?APP_PROBE_PATH is required}" "${SING_BOX_VERIFY_COMMAND:?SING_BOX_VERIFY_COMMAND is required}" "${POSTGRES_MODE:?POSTGRES_MODE is required}" "${REDIS_MODE:?REDIS_MODE is required}"; [[ "$POSTGRES_MODE" == docker || "$POSTGRES_MODE" == neon ]] || { printf 'POSTGRES_MODE must be docker or neon\n' >&2; exit 1; }; [[ "$REDIS_MODE" == docker || "$REDIS_MODE" == upstash ]] || { printf 'REDIS_MODE must be docker or upstash\n' >&2; exit 1; }; }
slot() { node -e 'const x=JSON.parse(require("fs").readFileSync(process.argv[1], "utf8")); if(!["blue","green"].includes(x.activeSlot)) process.exit(1); process.stdout.write(x.activeSlot)' "$legacy_state"; }
one_legacy() { local value; value="$(docker ps -q --filter label=com.docker.compose.project=sub2api --filter "label=com.docker.compose.service=$1" --filter status=running)"; [[ -n "$value" && "$value" != *$'\n'* ]] || { printf 'expected exactly one running labeled legacy %s container\n' "$1" >&2; exit 1; }; printf '%s' "$value"; }
container_labels() { docker inspect -f '{{index .Config.Labels "com.docker.compose.project"}}/{{index .Config.Labels "com.docker.compose.service"}}' "$1"; }
network_labels() { docker network inspect -f '{{index .Labels "com.docker.compose.project"}}' sub2api-edge; }
exists_container() { docker inspect "$1" >/dev/null 2>&1; }
exists_network() { docker network inspect sub2api-edge >/dev/null 2>&1; }

declare -A j=()
read_journal() {
  [[ -f "$journal" ]] || { printf 'journal is required\n' >&2; exit 1; }; j=()
  local line key value; local -A allowed=([version]=1 [environment]=1 [site]=1 [host_state]=1 [legacy_project]=1 [legacy_traefik]=1 [active_slot]=1 [active_app]=1 [edge_project]=1 [edge_root]=1 [route]=1 [route_backup]=1 [route_preexisting]=1 [route_backup_intent]=1 [route_backup_created]=1 [route_write_intent]=1 [edge_network]=1 [network_intent]=1 [network_created]=1 [attachment_intent]=1 [attachment_created]=1 [edge_container]=1 [edge_container_preexisting]=1 [edge_container_intent]=1 [edge_dynamic_dir_created]=1 [edge_env_intent]=1 [edge_env_created]=1 [edge_static_intent]=1 [edge_static_created]=1 [edge_singbox_intent]=1 [edge_singbox_created]=1 [acme_destination_preexisting]=1 [acme_intent]=1 [acme_created]=1 [legacy_state_backup]=1 [legacy_state_backup_intent]=1 [legacy_state_backup_created]=1 [legacy_state_adopted]=1 [state]=1)
  while IFS= read -r line; do [[ "$line" == *=* ]] || { printf 'journal is malformed\n' >&2; exit 1; }; key="${line%%=*}"; value="${line#*=}"; [[ -v "allowed[$key]" && ! -v "j[$key]" && -n "$value" && "$value" != *$'\r'* ]] || { printf 'journal has unknown, duplicate, or malformed field\n' >&2; exit 1; }; j[$key]="$value"; done < "$journal"
  for key in "${!allowed[@]}"; do [[ -v "j[$key]" ]] || case "$key" in legacy_state_backup) j[$key]="$legacy_state_backup" ;; legacy_state_backup_intent|legacy_state_backup_created|legacy_state_adopted) j[$key]=false ;; *) printf 'journal is incomplete\n' >&2; exit 1 ;; esac; done
  [[ "${j[version]}" == 1 && "${j[environment]}" == "$environment" && "${j[site]}" == code2 && "${j[host_state]}" == "$host_state" && "${j[legacy_project]}" == sub2api && "${j[edge_project]}" == sub2api-edge && "${j[edge_root]}" == "$edge_root" && "${j[route]}" == "$route" && "${j[route_backup]}" == "$route_backup" && "${j[legacy_state_backup]}" == "$legacy_state_backup" && "${j[edge_network]}" == sub2api-edge && "${j[edge_container_preexisting]}" == false ]] || { printf 'journal does not match this host\n' >&2; exit 1; }
  [[ "${j[legacy_traefik]}" =~ ^[a-f0-9]{12,64}$ && "${j[active_app]}" =~ ^[a-f0-9]{12,64}$ && "${j[active_slot]}" =~ ^(blue|green)$ && ( "${j[edge_container]}" == uncreated || "${j[edge_container]}" =~ ^[a-f0-9]{12,64}$ ) && "${j[state]}" =~ ^(prepared|pending|completing|complete)$ ]] || { printf 'journal identity is invalid\n' >&2; exit 1; }
  for key in route_preexisting route_backup_intent route_backup_created route_write_intent network_intent network_created attachment_intent attachment_created edge_container_intent edge_dynamic_dir_created edge_env_intent edge_env_created edge_static_intent edge_static_created edge_singbox_intent edge_singbox_created acme_destination_preexisting acme_intent acme_created legacy_state_backup_intent legacy_state_backup_created legacy_state_adopted; do [[ "${j[$key]}" =~ ^(true|false)$ ]] || { printf 'journal boolean is invalid\n' >&2; exit 1; }; done
}
set_journal() { local key="$1" value="$2" tmp="$journal.$$.next" line; while IFS= read -r line; do [[ "$line" == "$key="* ]] && line="$key=$value"; printf '%s\n' "$line"; done < "$journal" > "$tmp"; chmod 600 "$tmp"; mv -f "$tmp" "$journal"; read_journal; }
write_journal() { local tmp="$journal.$$.tmp"; (umask 077; printf 'version=1\nenvironment=%s\nsite=code2\nhost_state=%s\nlegacy_project=sub2api\nlegacy_traefik=%s\nactive_slot=%s\nactive_app=%s\nedge_project=sub2api-edge\nedge_root=%s\nroute=%s\nroute_backup=%s\nroute_preexisting=%s\nroute_backup_intent=false\nroute_backup_created=false\nroute_write_intent=false\nedge_network=sub2api-edge\nnetwork_intent=false\nnetwork_created=false\nattachment_intent=false\nattachment_created=false\nedge_container=uncreated\nedge_container_preexisting=false\nedge_container_intent=false\nedge_dynamic_dir_created=false\nedge_env_intent=false\nedge_env_created=false\nedge_static_intent=false\nedge_static_created=false\nedge_singbox_intent=false\nedge_singbox_created=false\nacme_destination_preexisting=%s\nacme_intent=false\nacme_created=false\nlegacy_state_backup=%s\nlegacy_state_backup_intent=false\nlegacy_state_backup_created=false\nlegacy_state_adopted=false\nstate=prepared\n' "$environment" "$host_state" "$legacy_traefik" "$active_slot" "$active_app" "$edge_root" "$route" "$route_backup" "$route_preexisting" "$acme_preexisting" "$legacy_state_backup" > "$tmp"); chmod 600 "$tmp"; mv -f "$tmp" "$journal"; }
mapping_state() { node -e 'const x=JSON.parse(require("fs").readFileSync(process.argv[1], "utf8")); const want=process.argv[2]; const ok=x.version===1&&x.sites?.length===1&&x.sites[0]==="code2"&&x.legacyCode2?.runtimeRoot==="runtime"&&x.legacyCode2?.composeProject==="sub2api"&&x.legacyCode2?.routeLayout==="flat"&&(want==="either"||x.legacyCode2?.handoverComplete===(want==="complete")); process.exit(ok?0:1)' "$host_state" "$1"; }
rollback_pair() { case "${j[state]}" in prepared) [[ ! -e "$host_state" ]] || mapping_state pending ;; pending) mapping_state pending ;; completing) mapping_state either ;; complete) mapping_state complete ;; *) return 1 ;; esac; }
validate_owned_resources() {
  [[ "$(container_labels "${j[legacy_traefik]}")" == sub2api/traefik && "$(container_labels "${j[active_app]}")" == "sub2api/sub2api-${j[active_slot]}" ]] || { printf 'journaled legacy container labels changed\n' >&2; exit 1; }
  if [[ "${j[edge_container]}" == uncreated && "${j[edge_container_intent]}" == true ]]; then
    local discovered; discovered="$(docker ps -aq --filter label=com.docker.compose.project=sub2api-edge --filter label=com.docker.compose.service=traefik)"
    if [[ -n "$discovered" ]]; then [[ "$discovered" != *$'\n'* && "$(container_labels "$discovered")" == sub2api-edge/traefik ]] || { printf 'intent-only Edge container identity is unsafe\n' >&2; exit 1; }; j[edge_container]="$discovered"; fi
  fi
  if [[ "${j[edge_container]}" != uncreated ]] && exists_container "${j[edge_container]}"; then [[ "$(container_labels "${j[edge_container]}")" == sub2api-edge/traefik ]] || { printf 'journaled Edge container labels changed\n' >&2; exit 1; }; fi
  if exists_network; then [[ "${j[network_intent]}" == true && "$(network_labels)" == sub2api-edge ]] || { printf 'Edge network is not journal-owned\n' >&2; exit 1; }; elif [[ "${j[network_created]}" == true ]]; then :; fi
  if [[ "${j[route_preexisting]}" == true ]]; then [[ -f "$route_backup" ]] || { printf 'required route backup is missing\n' >&2; exit 1; }; elif [[ "${j[route_backup_created]}" == true ]]; then { printf 'unexpected route backup ownership\n' >&2; exit 1; }; fi
  # An attachment intent can be interrupted before connect succeeds. Both an
  # absent and present attachment are recoverable; present is revalidated below.
  if [[ "${j[attachment_intent]}" == true ]] && exists_network; then docker inspect -f '{{with index .NetworkSettings.Networks "sub2api-edge"}}{{.NetworkID}}{{end}}' "${j[active_app]}" >/dev/null; fi
}
restore() {
  validate_owned_resources
  if [[ "${j[edge_container]}" != uncreated ]] && exists_container "${j[edge_container]}"; then docker stop "${j[edge_container]}"; fi
  docker start "${j[legacy_traefik]}"; [[ "$(docker inspect -f '{{.State.Running}}' "${j[legacy_traefik]}")" == true ]]
  if [[ "${j[attachment_intent]}" == true ]] && exists_network; then
    if docker inspect -f '{{with index .NetworkSettings.Networks "sub2api-edge"}}{{.NetworkID}}{{end}}' "${j[active_app]}" | grep -q .; then docker network disconnect sub2api-edge "${j[active_app]}"; fi
  fi
  if [[ "${j[route_preexisting]}" == true ]]; then [[ -f "$route_backup" ]] && mv -f "$route_backup" "$route"; else rm -f "$route"; fi
  [[ "${j[edge_env_intent]}" == true && -f "$edge_env" ]] && rm -f "$edge_env"; [[ "${j[edge_static_intent]}" == true && -f "$edge_static" ]] && rm -f "$edge_static"; [[ "${j[edge_singbox_intent]}" == true && -f "$edge_singbox" ]] && rm -f "$edge_singbox"; [[ "${j[acme_intent]}" == true && "${j[acme_destination_preexisting]}" == false && -f "$edge_acme" ]] && rm -f "$edge_acme"; [[ "${j[edge_dynamic_dir_created]}" == true ]] && rmdir "$edge_root/dynamic" 2>/dev/null || true
  if [[ "${j[edge_container]}" != uncreated ]] && exists_container "${j[edge_container]}"; then docker rm "${j[edge_container]}"; fi
  if [[ "${j[network_intent]}" == true ]] && exists_network; then [[ "$(network_labels)" == sub2api-edge ]] || { printf 'Edge network ownership changed\n' >&2; exit 1; }; docker network rm sub2api-edge; fi
  if [[ "${j[legacy_state_backup_created]}" == true && -f "${j[legacy_state_backup]}" ]]; then cp -p "${j[legacy_state_backup]}" "$legacy_state"; rm -f "$legacy_state_backup"; fi
  rm -f "$host_state"
}
stage_edge() {
  if [[ ! -d "$edge_root/dynamic" ]]; then set_journal edge_dynamic_dir_created true; mkdir -p "$edge_root/dynamic"; fi
  set_journal edge_env_intent true; TRAEFIK_IMAGE="$TRAEFIK_IMAGE" CLOUDFLARE_API_TOKEN="$CLOUDFLARE_API_TOKEN" ACME_EMAIL="$ACME_EMAIL" EDGE_RUNTIME_ROOT="$edge_root" node -e 'process.stdout.write(JSON.stringify({TRAEFIK_IMAGE:process.env.TRAEFIK_IMAGE,CLOUDFLARE_DNS_API_TOKEN:process.env.CLOUDFLARE_API_TOKEN,ACME_EMAIL:process.env.ACME_EMAIL,EDGE_RUNTIME_ROOT:process.env.EDGE_RUNTIME_ROOT}))' | npx --no-install tsx scripts/render-runtime-env.ts write "$edge_env"; set_journal edge_env_created true
  set_journal edge_static_intent true; set_journal edge_singbox_intent true; npx --no-install tsx scripts/render-edge-config.ts write "$edge_root" traefik/traefik.yml traefik/dynamic/sing-box.yml "$ACME_EMAIL" "$SING_BOX_SERVER_NAME" "$SING_BOX_TARGET"; set_journal edge_static_created true; set_journal edge_singbox_created true
  if [[ "${j[acme_destination_preexisting]}" == false ]]; then set_journal acme_intent true; [[ -f "$legacy_acme" ]] && cp -p "$legacy_acme" "$edge_acme" || : > "$edge_acme"; chmod 600 "$edge_acme"; set_journal acme_created true; fi
}

if [[ "$mode" == dry-run ]]; then require_legacy; printf 'dry-run: no files, Docker, network, or host state will be changed\n'; exit 0; fi
if [[ "$mode" == prepare-preview ]]; then require_legacy; npx --no-install tsx scripts/write-host-state.ts write-legacy "$host_state" code2 pending; exit 0; fi
if [[ "$mode" == rollback ]]; then read_journal; rollback_pair || { printf 'journal and host state are not a recoverable pair\n' >&2; exit 1; }; restore; exit 0; fi
if [[ "$mode" == retire-journal ]]; then
  read_journal
  [[ "${j[state]}" == complete ]] && mapping_state complete || { printf 'only a completed code2-only adoption journal may be retired\n' >&2; exit 1; }
  mv -f "$journal" "$journal.retired"
  [[ "${j[legacy_state_backup_created]}" == true ]] && rm -f "${j[legacy_state_backup]}"
  exit 0
fi
require_apply_inputs
[[ -f "$legacy_state" ]] || exit 1
[[ ! -e "$host_state" ]] || mapping_state pending || { printf 'apply requires absent or pending code2-only host state\n' >&2; exit 1; }
active_slot="$(slot)"; legacy_traefik="$(one_legacy traefik)"; active_app="$(one_legacy "sub2api-$active_slot")"
[[ ! -e "$edge_env" && ! -e "$edge_static" && ! -e "$edge_singbox" ]] || { printf 'edge runtime already exists; refusing overwrite\n' >&2; exit 1; }
exists_network && { printf 'sub2api-edge network already exists; refusing ownership assumption\n' >&2; exit 1; }
existing_edge="$(docker ps -aq --filter label=com.docker.compose.project=sub2api-edge --filter label=com.docker.compose.service=traefik)"; [[ -z "$existing_edge" ]] || { printf 'Edge container already exists\n' >&2; exit 1; }
route_preexisting=false; [[ -f "$route" ]] && route_preexisting=true; acme_preexisting=false; [[ -f "$edge_acme" ]] && acme_preexisting=true
write_journal; trap 'read_journal; rollback_pair; restore' ERR
if node -e 'const s=JSON.parse(require("fs").readFileSync(process.argv[1], "utf8")); process.exit(s.postgresMode && s.redisMode ? 0 : 1)' "$legacy_state"; then
  npx --no-install tsx scripts/deployment-mode.ts check "$legacy_state" "$POSTGRES_MODE" "$REDIS_MODE"
else
  [[ ! -e "$legacy_state_backup" ]] || { printf 'legacy deploy-state backup already exists; refusing overwrite\n' >&2; exit 1; }
  set_journal legacy_state_backup_intent true
  [[ -f "$legacy_state_backup" ]] || cp -p "$legacy_state" "$legacy_state_backup"
  set_journal legacy_state_backup_created true
  npx --no-install tsx scripts/deployment-mode.ts adopt "$legacy_state" "$POSTGRES_MODE" "$REDIS_MODE"
  set_journal legacy_state_adopted true
fi
npx --no-install tsx scripts/write-host-state.ts write-legacy "$host_state" code2 pending; set_journal state pending
stage_edge; if [[ "$route_preexisting" == true ]]; then set_journal route_backup_intent true; cp -p "$route" "$route_backup"; set_journal route_backup_created true; fi
set_journal network_intent true; set_journal edge_container_intent true; EDGE_RUNTIME_ROOT="$edge_root" TRAEFIK_IMAGE="$TRAEFIK_IMAGE" CLOUDFLARE_DNS_API_TOKEN="$CLOUDFLARE_API_TOKEN" ACME_EMAIL="$ACME_EMAIL" docker compose --project-name sub2api-edge --env-file "$edge_env" -f compose/edge.yml create traefik
exists_network && [[ "$(network_labels)" == sub2api-edge ]] || exit 1; set_journal network_created true
edge_container="$(docker ps -aq --filter label=com.docker.compose.project=sub2api-edge --filter label=com.docker.compose.service=traefik)"; [[ -n "$edge_container" && "$edge_container" != *$'\n'* && "$(container_labels "$edge_container")" == sub2api-edge/traefik ]] || exit 1; set_journal edge_container "$edge_container"
set_journal attachment_intent true; docker network connect --alias "sub2api-$active_slot" sub2api-edge "$active_app"; set_journal attachment_created true
set_journal route_write_intent true; npx --no-install tsx scripts/render-site-route.ts write traefik/dynamic/site.yml "$route" code2 "$DOMAIN" "$active_slot" "sub2api-$active_slot"
docker stop "$legacy_traefik"; EDGE_RUNTIME_ROOT="$edge_root" TRAEFIK_IMAGE="$TRAEFIK_IMAGE" CLOUDFLARE_DNS_API_TOKEN="$CLOUDFLARE_API_TOKEN" ACME_EMAIL="$ACME_EMAIL" docker compose --project-name sub2api-edge --env-file "$edge_env" -f compose/edge.yml start traefik
bash scripts/probe-origin-strict.sh "$DOMAIN" "$ORIGIN_IP" "$APP_PROBE_PATH"; bash scripts/probe-origin.sh "$DOMAIN" "$APP_PROBE_PATH"; bash -c "$SING_BOX_VERIFY_COMMAND"
set_journal state completing; npx --no-install tsx scripts/write-host-state.ts write-legacy "$host_state" code2 complete; set_journal state complete; trap - ERR
