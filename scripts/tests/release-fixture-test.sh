#!/usr/bin/env bash
set -euo pipefail

root="$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)"
temporary="$(mktemp -d)"
trap 'rm -rf "$temporary"' EXIT
target_sha="0123456789abcdef0123456789abcdef01234567"
sha="$(printf '%s' "$target_sha" | sha256sum | cut -d' ' -f1)"
components="$temporary/components"
bundle="$temporary/bundle"
mkdir -p "$components"

for name in sub2api-deploy pulumi-program pulumi-resource-sub2api-host; do
  printf '#!/usr/bin/env bash\nexit 0\n' > "$components/$name"
  chmod 0755 "$components/$name"
done
for architecture in amd64 arm64; do
  machine=62
  [[ "$architecture" == arm64 ]] && machine=183
  printf '\177ELF\002\001\001\000\000\000\000\000\000\000\000\000\002\000' > "$components/sub2api-host-linux-$architecture"
  printf "$(printf '\\%03o' "$((machine % 256))")$(printf '\\%03o' "$((machine / 256))")" >> "$components/sub2api-host-linux-$architecture"
  chmod 0755 "$components/sub2api-host-linux-$architecture"
done

bash "$root/scripts/release-bundle.sh" assemble "$bundle" "$components" "sub2api-host-controller@sha256:$sha"
jq -e --arg release "sub2api-host-controller@sha256:$sha" '.release == $release' "$bundle/artifacts/sub2api-host/manifest.json" >/dev/null
bash "$root/scripts/release-bundle.sh" verify-host-artifacts "$bundle"
printf 'tampered' >> "$bundle/artifacts/sub2api-host/sub2api-host-linux-amd64"
if bash "$root/scripts/release-bundle.sh" verify-host-artifacts "$bundle" >/dev/null 2>&1; then
  printf 'tampered host artifact was accepted\n' >&2
  exit 1
fi
printf 'release manifest fixture passed\n'
