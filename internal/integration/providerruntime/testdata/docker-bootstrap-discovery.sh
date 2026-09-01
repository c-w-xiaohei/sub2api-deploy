#!/bin/sh
set -eu

# This shim is reachable only through the helper's isolated PATH. It permits
# exactly Runtime.Bootstrap's ownership discovery calls and rejects all effects.
container_format=$(printf '{{.Names}}\t{{index .Labels "sub2api.host"}}')
network_format=$(printf '{{.Name}}\t{{index .Labels "sub2api.host"}}')
if [ "$#" -eq 7 ] && [ "$1" = container ] && [ "$2" = ls ] && [ "$3" = --all ] && [ "$4" = --filter ] && [ "$5" = label=sub2api.host ] && [ "$6" = --format ] && [ "$7" = "$container_format" ]; then
  printf 'bootstrap-container-discovery\n' >> "$PROVIDER_RUNTIME_DOCKER_LOG"
  exit 0
fi
if [ "$#" -eq 6 ] && [ "$1" = network ] && [ "$2" = ls ] && [ "$3" = --filter ] && [ "$4" = label=sub2api.host ] && [ "$5" = --format ] && [ "$6" = "$network_format" ]; then
  printf 'bootstrap-network-discovery\n' >> "$PROVIDER_RUNTIME_DOCKER_LOG"
  exit 0
fi
printf 'unexpected docker bootstrap command\n' >&2
exit 64
