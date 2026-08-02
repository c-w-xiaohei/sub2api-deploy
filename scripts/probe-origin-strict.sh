#!/usr/bin/env bash
set -euo pipefail

domain="${1:?domain is required}"
origin_ip="${2:?origin IP is required}"
path="${3:?probe path is required}"
retries="${PROBE_RETRIES:-30}"
delay="${PROBE_DELAY_SECONDS:-2}"
for ((attempt = 1; attempt <= retries; attempt++)); do
  if curl --fail --silent --show-error --noproxy '*' --max-time 10 \
    --resolve "${domain}:443:${origin_ip}" "https://${domain}${path}" >/dev/null; then
    exit 0
  fi
  [[ "$attempt" -lt "$retries" ]] && sleep "$delay"
done
exit 1
