#!/usr/bin/env bash
# Static contract test: production composition owns parameter-to-file mapping.
set -euo pipefail

repo_root="$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)"
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
environment="$repo_root/nix/environments.nix"
activation="$repo_root/nix/host-activate.sh"
nix_workflow="$repo_root/.github/workflows/nix-runtime.yml"
release_workflow="$repo_root/.github/workflows/release.yml"

grep -Fq 'Environment=PATH=%s/bin' "$environment" || fail 'Docker unit does not bind the composed environment PATH'
grep -Fq 'ExecStart=%s/bin/dockerd --config-file=/etc/docker/daemon.json --data-root=/var/lib/docker -H fd://' "$environment" || fail 'Docker unit is not parameterized from the composed environment'
if grep -Eq 'toolPaths|tools/bin|source-inventory|ld-musl|lib/nft' "$environment"; then
  fail 'environment still contains the private runtime-distribution layer'
fi
grep -Fq 'SocketMode=0600' "$environment" || fail 'Docker socket configuration is missing'
grep -Fq "'sub2api-nix-docker.service /etc/systemd/system/sub2api-nix-docker.service'" "$environment" || fail 'unit is absent from fixed managed manifest'
grep -Fq "printf '0644 %s etc/sub2api-nix-host/files/%s %s" "$environment" || fail 'manifest does not include an explicit file mode'
grep -Fq "'daemon.json /etc/docker/daemon.json'" "$environment" || fail 'daemon config is absent from fixed managed manifest'
grep -Fq 'generated_manifest="$runtime_root/share/sub2api-runtime/activation-manifest"' "$activation" || fail 'activation does not consume generated manifest'
grep -Fq 'bin/sub2api-host' "$activation" || fail 'activation does not preflight sub2api-host'
grep -Fq 'libexec/sub2api-host' "$activation" || fail 'activation does not preflight the real Host payload'
grep -Fq 'share/sub2api-host/release' "$activation" || fail 'activation does not preflight Host release metadata'
grep -Fq 'hostPayload' "$environment" || fail 'Host composition does not require the selected Host payload'
grep -Fq 'cp ${hostPayload.path} "$out/libexec/sub2api-host"' "$environment" || fail 'Host composition does not preserve the selected Host payload in libexec'
grep -Fq 'makeWrapper "$out/libexec/sub2api-host" "$out/bin/sub2api-host"' "$environment" || fail 'Host command is not a wrapper around the libexec payload'
grep -Fq 'composed_sha256="$(sha256sum "$host/libexec/sub2api-host"' "$nix_workflow" || fail 'Nix Host evidence hashes the wrapper instead of the libexec payload'
if grep -Fq 'composed_sha256="$(sha256sum "$host/bin/sub2api-host"' "$nix_workflow"; then
  fail 'Nix Host evidence still hashes the wrapper'
fi
grep -Fq 'host_sha="$(sha256sum "$host/libexec/sub2api-host"' "$release_workflow" || fail 'release candidate check hashes the wrapper instead of the libexec payload'
grep -Fq 'cmp -- "$candidate_payload/artifacts/sub2api-host/sub2api-host-linux-amd64" "$host/libexec/sub2api-host"' "$release_workflow" || fail 'release x86 check does not compare the candidate Host bytes to libexec payload'
if grep -Fq 'host_sha="$(sha256sum "$host/bin/sub2api-host"' "$release_workflow"; then
  fail 'release candidate check still hashes the wrapper'
fi
if grep -Fq 'unit_contents=' "$activation" || grep -Fq 'socket_contents=' "$activation"; then
  fail 'activation still constructs systemd units'
fi
printf 'environment fixture tests passed\n'
