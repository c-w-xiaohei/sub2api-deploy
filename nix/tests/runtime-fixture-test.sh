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
if ! command -v nix >/dev/null 2>&1; then
  printf 'runtime fixture tests skipped: nix is unavailable\n'
  exit 0
fi
nix() { command nix --extra-experimental-features 'nix-command flakes fetch-tree' "$@"; }

project_sha='aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'
host_release="sub2api-host-controller@sha256:$(printf '%s' "$project_sha" | sha256sum | cut -d ' ' -f 1)"

# A release lock with no published entries is intentionally not installable,
# for both formal environments.
expect_failure 'controller release is unavailable' \
  nix eval --impure --expr "let runtime = import $repo_root/nix/runtime-lib.nix { releaseLock = { schemaVersion = 2; }; }; in runtime.controllerArtifact"
expect_failure 'controller release is unavailable' \
  nix eval --impure --expr "let runtime = import $repo_root/nix/runtime-lib.nix { releaseLock = { schemaVersion = 2; }; }; in runtime.hostPayloadFor \"x86_64-linux\""
expect_failure 'controller release is unavailable' \
  nix eval --raw "path:$repo_root#packages.x86_64-linux.host-environment"
expect_failure 'controller release is unavailable' \
  nix eval --raw "path:$repo_root#packages.aarch64-linux.host-environment"

# The release lock only identifies the exact prebuilt project candidate.
cat >"$fixture_root/invalid-release.json" <<'JSON'
{"schemaVersion":2,"controller":{"version":"latest","projectCommit":"0000000000000000000000000000000000000000","url":"https://example.invalid/mutable.tar.gz","narHash":"sha256-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="}}
JSON
expect_failure 'controller version must be an immutable release tag' \
  nix eval --impure --expr "let runtime = import $repo_root/nix/runtime-lib.nix { releaseLock = builtins.fromJSON (builtins.readFile $fixture_root/invalid-release.json); }; in runtime.controllerArtifact"

cat >"$fixture_root/cross-bound-release.json" <<'JSON'
{"schemaVersion":2,"controller":{"version":"v1.2.3","projectCommit":"0000000000000000000000000000000000000000","url":"https://github.com/c-w-xiaohei/sub2api-deploy/releases/download/v9.9.9/sub2api-controller-1111111111111111111111111111111111111111.tar.gz","narHash":"sha256-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="}}
JSON
expect_failure 'controller URL must match version and projectCommit' \
  nix eval --impure --expr "let runtime = import $repo_root/nix/runtime-lib.nix { releaseLock = builtins.fromJSON (builtins.readFile $fixture_root/cross-bound-release.json); }; in runtime.controllerArtifact"

# A non-empty lock may use only an explicitly injected Nix store candidate in
# local tests; formal releases use the same candidate after fetchTree.outPath.
candidate_root="$fixture_root/candidate"
mkdir -p "$candidate_root/artifacts/sub2api-host"
printf 'x86 fixture\n' >"$candidate_root/artifacts/sub2api-host/sub2api-host-linux-amd64"
printf 'arm fixture\n' >"$candidate_root/artifacts/sub2api-host/sub2api-host-linux-arm64"
amd64_hash="$(sha256sum "$candidate_root/artifacts/sub2api-host/sub2api-host-linux-amd64" | cut -d ' ' -f 1)"
arm64_hash="$(sha256sum "$candidate_root/artifacts/sub2api-host/sub2api-host-linux-arm64" | cut -d ' ' -f 1)"
cat >"$candidate_root/artifacts/sub2api-host/manifest.json" <<JSON
{"schemaVersion":1,"release":"$host_release","linux-amd64":{"path":"sub2api-host-linux-amd64","sha256":"$amd64_hash","size":$(stat -c %s "$candidate_root/artifacts/sub2api-host/sub2api-host-linux-amd64")},"linux-arm64":{"path":"sub2api-host-linux-arm64","sha256":"$arm64_hash","size":$(stat -c %s "$candidate_root/artifacts/sub2api-host/sub2api-host-linux-arm64")}}
JSON
candidate_store="$(nix --offline store add-path "$candidate_root")"
cat >"$fixture_root/valid-release.json" <<JSON
{"schemaVersion":2,"controller":{"version":"v1.2.3","projectCommit":"$project_sha","hostRelease":"$host_release","url":"https://github.com/c-w-xiaohei/sub2api-deploy/releases/download/v1.2.3/sub2api-controller-$project_sha.tar.gz","narHash":"sha256-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=","hostPayload":{"x86_64-linux":{"path":"artifacts/sub2api-host/sub2api-host-linux-amd64","sha256":"$amd64_hash"},"aarch64-linux":{"path":"artifacts/sub2api-host/sub2api-host-linux-arm64","sha256":"$arm64_hash"}}}}
JSON
result="$(nix eval --impure --json --expr "let runtime = import $repo_root/nix/runtime-lib.nix { releaseLock = builtins.fromJSON (builtins.readFile $fixture_root/valid-release.json); candidateOverride = builtins.storePath \"$candidate_store\"; }; in { candidate = builtins.toString runtime.controllerPayload; host = runtime.hostPayloadFor \"aarch64-linux\"; }")"
[[ "$result" == *"$candidate_store"* && "$result" == *"$host_release"* ]] || fail 'non-empty override composition did not preserve candidate and release identity'
sed "s/$host_release/sub2api-host-controller@sha256:$(printf '%064d' 0)/" "$fixture_root/valid-release.json" >"$fixture_root/wrong-host-release.json"
expect_failure 'controller hostRelease must bind to projectCommit' \
  nix eval --impure --expr "let runtime = import $repo_root/nix/runtime-lib.nix { releaseLock = builtins.fromJSON (builtins.readFile $fixture_root/wrong-host-release.json); candidateOverride = builtins.storePath \"$candidate_store\"; }; in runtime.hostPayloadFor \"aarch64-linux\""
cp -R "$candidate_root" "$fixture_root/wrong-candidate"
sed -i "s/$host_release/sub2api-host-controller@sha256:$(printf '%064d' 0)/" "$fixture_root/wrong-candidate/artifacts/sub2api-host/manifest.json"
wrong_candidate_store="$(nix --offline store add-path "$fixture_root/wrong-candidate")"
expect_failure 'candidate Host manifest does not match controller hostRelease' \
  nix eval --impure --expr "let runtime = import $repo_root/nix/runtime-lib.nix { releaseLock = builtins.fromJSON (builtins.readFile $fixture_root/valid-release.json); candidateOverride = builtins.storePath \"$wrong_candidate_store\"; }; in runtime.controllerPayload"

printf 'runtime fixture tests passed\n'
