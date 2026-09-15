#!/usr/bin/env bash
set -euo pipefail

repo_root="$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)"
fixture_root="$(mktemp -d)"
trap 'rm -rf "$fixture_root"' EXIT
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
expect_failure() {
  local expected="$1"; shift
  local output
  if output="$("$@" 2>&1)"; then fail "expected command to fail: $*"; fi
  [[ "$output" == *"$expected"* ]] || fail "expected '$expected', got: $output"
}

payload="$fixture_root/payload"
workspace="$fixture_root/workspace"
mkdir -p "$payload/bin" "$payload/workspace/bin" "$workspace"
install -m 0755 "$repo_root/nix/controller-workspace-init.sh" "$payload/bin/sub2api-workspace-init"
printf 'name: sub2api-environment\n' >"$payload/workspace/Pulumi.yaml"
printf '#!/usr/bin/env bash\n' >"$payload/workspace/bin/pulumi-program"
chmod +x "$payload/workspace/bin/pulumi-program"

"$payload/bin/sub2api-workspace-init" "$workspace"
[[ -L "$workspace/Pulumi.yaml" && -L "$workspace/bin" ]] || fail 'workspace init did not link cwd contract'
"$payload/bin/sub2api-workspace-init" "$workspace"
rm "$workspace/Pulumi.yaml"
printf 'user configuration\n' >"$workspace/Pulumi.yaml"
expect_failure 'refusing to replace existing' "$payload/bin/sub2api-workspace-init" "$workspace"

rm -f "$workspace/Pulumi.yaml" "$workspace/bin"
mkdir "$workspace/bin"
expect_failure 'refusing to replace existing' "$payload/bin/sub2api-workspace-init" "$workspace"
[[ ! -e "$workspace/Pulumi.yaml" && ! -L "$workspace/Pulumi.yaml" ]] || fail 'partial workspace initialization occurred'

printf 'controller workspace fixture tests passed\n'
