#!/usr/bin/env bash
set -euo pipefail

: "${CONFIGURED_SITE_IDS:?CONFIGURED_SITE_IDS is required}"
: "${HOST_STATE_PATH:?HOST_STATE_PATH is required}"
: "${SITE_DEPLOY_STATE_PATHS:?SITE_DEPLOY_STATE_PATHS is required}"
npx --no-install tsx scripts/host-preflight.ts check "$CONFIGURED_SITE_IDS" "$HOST_STATE_PATH"
IFS=',' read -r -a site_ids <<< "$CONFIGURED_SITE_IDS"
IFS=',' read -r -a state_paths <<< "$SITE_DEPLOY_STATE_PATHS"
[[ "${#site_ids[@]}" == "${#state_paths[@]}" ]] || { printf 'SITE_DEPLOY_STATE_PATHS must provide one path per configured Site\n' >&2; exit 1; }
for index in "${!site_ids[@]}"; do
  site_id="${site_ids[$index]}"
  site_id="${site_id//[[:space:]]/}"
  [[ -n "$site_id" ]] || { printf 'configured Site IDs must not contain empty values\n' >&2; exit 1; }
  state_path="${state_paths[$index]//[[:space:]]/}"
  [[ -n "$state_path" ]] || { printf 'Site %s has no explicit deploy state path\n' "$site_id" >&2; exit 1; }
  [[ -f "$state_path" ]] || { printf 'Site %s has no deploy state at %s; host state is not finalized\n' "$site_id" "$state_path" >&2; exit 1; }
done
npx --no-install tsx scripts/write-host-state.ts write "$HOST_STATE_PATH" "$CONFIGURED_SITE_IDS"
