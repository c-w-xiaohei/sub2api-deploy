#!/usr/bin/env bash
# CI-only: execute the production compositor with imported prebuilt fixture inputs.
set -euo pipefail

root="$(mktemp -d)"
trap 'rm -rf "$root"' EXIT
repo_root="$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)"
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
nix_bin="$(command -v nix)"
nix_offline() {
  "$nix_bin" --extra-experimental-features 'nix-command flakes fetch-tree' \
    --offline --option max-jobs 1 --option builders '' --option substituters '' "$@"
}
nix_prefetch() {
  "$nix_bin" --extra-experimental-features 'nix-command flakes fetch-tree' \
    --option max-jobs 0 --option builders '' "$@"
}

fixture_home="$root/home"
fixture_payload="$root/payload"
fixture_host_payload="$root/host-payload"
fixture_flake="$root/fixture-flake"
profile="$root/profile"
mkdir -p "$fixture_home" "$fixture_payload/bin" "$fixture_payload/libexec" "$fixture_payload/workspace/bin" \
  "$fixture_payload/artifacts/sub2api-host" \
  "$fixture_host_payload/bin" "$fixture_host_payload/etc/sub2api-nix-host" "$fixture_host_payload/libexec" \
  "$fixture_host_payload/share/sub2api-runtime" "$fixture_flake"

cat >"$fixture_payload/bin/sub2api-deploy" <<'SH'
#!/bin/sh
set -eu
printf 'profile-install-fixture-ran:%s:%s\n' "$(sops)" "$(ssh)"
SH
chmod 0755 "$fixture_payload/bin/sub2api-deploy"
for binary in go pulumi pulumi-resource-sub2api-host sops ssh; do
  printf '#!/bin/sh\nprintf "%%s\\n" "payload-%s"\n' "$binary" >"$fixture_payload/bin/$binary"
  chmod 0755 "$fixture_payload/bin/$binary"
done
printf 'name: sub2api-environment\n' >"$fixture_payload/workspace/Pulumi.yaml"
printf '#!/usr/bin/env bash\nexit 0\n' >"$fixture_payload/workspace/bin/pulumi-program"
chmod 0755 "$fixture_payload/workspace/bin/pulumi-program"
printf '%s\n' fixture-manifest >"$fixture_payload/artifacts/sub2api-host/manifest.json"
for architecture in amd64 arm64; do
  printf '%s\n' "fixture-host-$architecture" >"$fixture_payload/artifacts/sub2api-host/sub2api-host-linux-$architecture"
  chmod 0755 "$fixture_payload/artifacts/sub2api-host/sub2api-host-linux-$architecture"
done
for binary in docker dockerd containerd runc nft ssh; do
  printf '#!/usr/bin/env bash\nexit 0\n' >"$fixture_host_payload/bin/$binary"
  chmod 0755 "$fixture_host_payload/bin/$binary"
done

# Fetch complete prebuilt composition-tool closures. max-jobs=0 forbids a
# source build when cache.nixos.org does not have them.
fixture_shell="$(nix_prefetch build --no-link --print-out-paths nixpkgs#bash | rg -- '-bash-interactive-[^/]+$' | rg -v -- '-man$' | head -n1)"
fixture_coreutils="$(nix_prefetch build --no-link --print-out-paths nixpkgs#coreutils | head -n1)"
[[ -x "$fixture_shell/bin/bash" && -x "$fixture_coreutils/bin/cp" ]] || fail 'prebuilt Nix composition tools are unavailable'
fixture_store_path="$(HOME="$fixture_home" nix_offline store add-path --name fixture-controller-payload "$fixture_payload")"
fixture_host_store_path="$(HOME="$fixture_home" nix_offline store add-path --name fixture-host-payload "$fixture_host_payload")"

cat >"$fixture_flake/flake.nix" <<EOF
{
  outputs = { self }:
    let
      compositor = import ${repo_root}/nix/environments.nix;
      payload = builtins.storePath "${fixture_store_path}";
      hostPayload = builtins.storePath "${fixture_host_store_path}";
      environment = compositor {
        system = "x86_64-linux";
        role = "controller";
        inherit payload;
        toolPaths = { shell = builtins.storePath "${fixture_shell}/bin/bash"; coreutils = builtins.storePath "${fixture_coreutils}/bin"; };
        controllerWorkspaceInit = ${repo_root}/nix/controller-workspace-init.sh;
        hostActivate = ${repo_root}/nix/host-activate.sh;
      };
      hostEnvironment = compositor {
        system = "x86_64-linux";
        role = "host-environment";
        payload = hostPayload;
        toolPaths = { shell = builtins.storePath "${fixture_shell}/bin/bash"; coreutils = builtins.storePath "${fixture_coreutils}/bin"; };
        controllerWorkspaceInit = ${repo_root}/nix/controller-workspace-init.sh;
        hostActivate = ${repo_root}/nix/host-activate.sh;
      };
    in {
      runtimePaths.x86_64-linux.controller = environment;
      runtimePaths.x86_64-linux.host-environment = hostEnvironment;
      apps.x86_64-linux.fixture = { type = "app"; program = "\${environment}/bin/sub2api-deploy"; };
    };
}
EOF

installable="path:$fixture_flake#runtimePaths.x86_64-linux.controller"
generated_path="$(HOME="$fixture_home" nix_offline build --impure --no-link --print-out-paths "$installable")"
test -x "$generated_path/bin/sub2api-workspace-init" || fail 'controller workspace initializer was not generated'
for binary in pulumi pulumi-resource-sub2api-host; do
  [[ -x "$generated_path/libexec/$binary" ]] || fail "controller sibling $binary was not preserved"
  cmp -s "$fixture_payload/bin/$binary" "$generated_path/libexec/$binary" || fail "controller sibling $binary bytes changed"
done
for artifact in manifest.json sub2api-host-linux-amd64 sub2api-host-linux-arm64; do
  workspace_artifact="$generated_path/workspace/artifacts/sub2api-host/$artifact"
  root_artifact="$generated_path/artifacts/sub2api-host/$artifact"
  [[ -f "$workspace_artifact" && ! -L "$workspace_artifact" ]] || fail "workspace artifact $artifact is not a real file"
  cmp -s "$workspace_artifact" "$root_artifact" || fail "workspace artifact $artifact does not preserve the root artifact bytes"
done
[[ -x "$generated_path/workspace/artifacts/sub2api-host/sub2api-host-linux-amd64" && -x "$generated_path/workspace/artifacts/sub2api-host/sub2api-host-linux-arm64" ]] || fail 'workspace Host artifacts lost executable mode'
[[ "$(head -n1 "$generated_path/bin/sub2api-workspace-init")" == "#!$fixture_shell/bin/bash" ]] || fail 'launcher does not use declared prebuilt shell'
workspace="$root/workspace"
mkdir "$workspace"
mkdir "$root/untrusted"
for binary in sops ssh; do
  printf '#!/bin/sh\nprintf "untrusted-%s\\n"\n' "$binary" >"$root/untrusted/$binary"
  chmod 0755 "$root/untrusted/$binary"
done
PATH="$root/untrusted" "$generated_path/bin/sub2api-workspace-init" "$workspace"
[[ "$(readlink -f "$workspace/Pulumi.yaml")" == "$generated_path/workspace/Pulumi.yaml" ]] || fail 'fixed-path workspace initializer did not create Pulumi.yaml'
[[ "$(readlink -f "$workspace/bin")" == "$generated_path/workspace/bin" ]] || fail 'fixed-path workspace initializer did not create bin'

host_path="$(HOME="$fixture_home" nix_offline build --impure --no-link --print-out-paths "path:$fixture_flake#runtimePaths.x86_64-linux.host-environment")"
unit="$host_path/etc/sub2api-nix-host/files/sub2api-nix-docker.service"
manifest="$host_path/share/sub2api-runtime/activation-manifest"
rg -Fq "ExecStart=$fixture_host_store_path/bin/dockerd" "$unit" || fail 'generated unit did not bind selected Docker payload'
rg -Fq 'etc/sub2api-nix-host/files/sub2api-nix-docker.service /etc/systemd/system/sub2api-nix-docker.service' "$manifest" || fail 'generated manifest omitted Docker unit'
read -r unit_mode unit_hash unit_source unit_destination < <(awk '$4 == "/etc/systemd/system/sub2api-nix-docker.service"' "$manifest")
[[ "$unit_mode" == 0644 ]] || fail 'manifest unit mode is not explicit'
[[ "$unit_hash" == "$(sha256sum "$unit" | cut -d ' ' -f1)" ]] || fail 'manifest unit hash does not match generated file'
[[ "$unit_source" == etc/sub2api-nix-host/files/sub2api-nix-docker.service ]] || fail 'manifest unit source is wrong'
[[ "$unit_destination" == /etc/systemd/system/sub2api-nix-docker.service ]] || fail 'manifest unit destination is wrong'

HOME="$fixture_home" nix_offline profile install --impure --profile "$profile" "$installable"
profile_executable="$profile/bin/sub2api-deploy"
test -x "$profile_executable" || fail 'profile install did not expose generated launcher'
expected_output='profile-install-fixture-ran:payload-sops:payload-ssh'
[[ "$(HOME="$fixture_home" PATH="$root/untrusted" "$profile_executable")" == "$expected_output" ]] || fail 'installed launcher did not bind payload tools'
[[ "$(HOME="$fixture_home" PATH="$root/untrusted" nix_offline run --impure "path:$fixture_flake#fixture")" == "$expected_output" ]] || fail 'flake app did not bind payload tools'

deriver="$(HOME="$fixture_home" nix-store --query --deriver "$fixture_store_path")"
[[ -z "$deriver" || "$deriver" == unknown-deriver ]] || fail 'fixture payload has a derivation'
if HOME="$fixture_home" nix-store --query --references "$generated_path" | rg -q '\.drv$'; then
  fail 'generated environment references a derivation'
fi
[[ "$(readlink -f "$profile_executable")" == "$generated_path/bin/sub2api-deploy" ]] || fail 'profile executable does not resolve to generated environment'

printf 'profile install fixture tests passed\n'
