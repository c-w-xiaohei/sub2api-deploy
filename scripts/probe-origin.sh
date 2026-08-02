#!/usr/bin/env bash
set -euo pipefail

domain="${1:?domain is required}"
path="${2:-/health}"
retries="${PROBE_RETRIES:-30}"
delay="${PROBE_DELAY_SECONDS:-2}"

for ((attempt = 1; attempt <= retries; attempt++)); do
  if curl --fail --silent --show-error --max-time 10 "https://${domain}${path}" >/dev/null; then
    exit 0
  fi
  sleep "$delay"
done

exit 1
