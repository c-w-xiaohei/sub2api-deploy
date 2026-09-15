#!/usr/bin/env bash
# These tests require Nix and run only in CI or an explicitly prepared local Nix host.
set -euo pipefail

repo_root="$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)"
fixture_root="$(mktemp -d)"
trap 'rm -rf "$fixture_root"' EXIT

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

expect_failure() {
  local expected="$1"
  shift
  local output
  if output="$("$@" 2>&1)"; then
    fail "expected command to fail: $*"
  fi
  [[ "$output" == *"$expected"* ]] || fail "expected failure containing '$expected', got: $output"
}
nix() { command nix --extra-experimental-features 'nix-command flakes fetch-tree' "$@"; }

# A release lock with no published entries is intentionally not installable.
expect_failure 'runtime artifact unavailable' nix eval --raw "path:$repo_root#runtimePaths.x86_64-linux.controller"
expect_failure 'runtime artifact unavailable' nix eval --raw "path:$repo_root#runtimePaths.x86_64-linux.host-environment"
[[ "$(nix eval --json "path:$repo_root#packages.aarch64-linux" --apply builtins.attrNames)" == '["host-environment"]' ]] ||
  fail 'aarch64 packages must expose only the evidenced Host environment'

# The lock evaluator is the source of truth for role/system and immutable URL rules.
cat >"$fixture_root/invalid-release.json" <<'JSON'
{"schemaVersion":1,"artifacts":[{"role":"controller","system":"x86_64-linux","version":"latest","asset":"../runtime.tar.gz?mutable=1","archiveSha256":"sha256-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=","narHash":"sha256-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=","projectCommit":"0000000000000000000000000000000000000000","requiredPaths":["bin/sub2api-deploy","bin/pulumi","bin/pulumi-language-go","bin/sops","bin/ssh","bin/sub2api-workspace-init","workspace/Pulumi.yaml","share/sub2api-runtime/inventory.json"]}]}
JSON
expect_failure 'artifact version must be an immutable vX.Y.Z release tag' \
  nix eval --impure --expr "let runtime = import $repo_root/nix/runtime-lib.nix { releaseLock = builtins.fromJSON (builtins.readFile $fixture_root/invalid-release.json); }; in runtime.artifactFor \"controller\" \"x86_64-linux\""

printf 'runtime fixture tests passed\n'
