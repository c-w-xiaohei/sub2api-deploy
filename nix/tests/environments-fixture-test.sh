#!/usr/bin/env bash
# Static contract test: production composition owns parameter-to-file mapping.
set -euo pipefail

repo_root="$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)"
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
environment="$repo_root/nix/environments.nix"
activation="$repo_root/nix/host-activate.sh"

grep -Fq 'ExecStart=${payload}/bin/dockerd --config-file=/etc/docker/daemon.json --data-root=/var/lib/docker -H fd://' "$environment" || fail 'Docker unit is not parameterized from the selected payload'
grep -Fq 'SocketMode=0600' "$environment" || fail 'Docker socket configuration is missing'
grep -Fq "'sub2api-nix-docker.service /etc/systemd/system/sub2api-nix-docker.service'" "$environment" || fail 'unit is absent from fixed managed manifest'
grep -Fq "printf '0644 %s etc/sub2api-nix-host/files/%s %s" "$environment" || fail 'manifest does not include an explicit file mode'
grep -Fq "'daemon.json /etc/docker/daemon.json'" "$environment" || fail 'daemon config is absent from fixed managed manifest'
grep -Fq 'generated_manifest="$runtime_root/share/sub2api-runtime/activation-manifest"' "$activation" || fail 'activation does not consume generated manifest'
if grep -Fq 'unit_contents=' "$activation" || grep -Fq 'socket_contents=' "$activation"; then
  fail 'activation still constructs systemd units'
fi
printf 'environment fixture tests passed\n'
