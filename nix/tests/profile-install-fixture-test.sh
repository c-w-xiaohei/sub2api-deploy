#!/usr/bin/env bash
set -euo pipefail

root="$(mktemp -d)"
trap 'rm -rf "$root"' EXIT
repo_root="$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)"
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
host_release="sub2api-host-controller@sha256:$(printf '%s' fixture-project | sha256sum | cut -d ' ' -f 1)"
if ! nix_bin="$(command -v nix)"; then
  printf 'profile install fixture tests skipped: nix is unavailable\n'
  exit 0
fi
nix_command() {
  HOME="$home" "$nix_bin" --extra-experimental-features 'nix-command flakes fetch-tree' "$@"
}

payload="$root/payload"
home="$root/home"
mkdir -p "$payload/bin" "$payload/artifacts/sub2api-host" "$payload/scripts/pulumi-plugins/cloudflare" \
  "$payload/scripts/pulumi-plugins/upstash" "$home"

for binary in sub2api-deploy go pulumi-program pulumi-resource-sub2api-host; do
  printf '#!/bin/sh\nprintf "fixture-%s\\n"\n' "$binary" > "$payload/bin/$binary"
  chmod 0755 "$payload/bin/$binary"
done
printf 'name: sub2api-environment\nruntime:\n  name: go\n  options:\n    binary: ./bin/pulumi-program\n' > "$payload/Pulumi.yaml"
printf 'module github.com/c-w-xiaohei/sub2api-deploy\n' > "$payload/go.mod"
printf '{}\n' > "$payload/scripts/pulumi-plugins/cloudflare/pulumi-plugin.json"
printf '{}\n' > "$payload/scripts/pulumi-plugins/upstash/pulumi-plugin.json"
cat > "$payload/artifacts/sub2api-host/sub2api-host-linux-amd64" <<'SH'
#!/bin/sh
set -eu
case "$1" in
  probe)
    digest="$(sha256sum "$0" | cut -d ' ' -f 1)"
    release="$(cat "$(dirname "$0")/../share/sub2api-host/release")"
    printf 's2p2:Linux\namd64\nmid1:%064d\n%s\n%s\n' 0 "$digest" "$release"
    ;;
  stdio)
    docker --version >/dev/null
    nft --version >/dev/null
    printf 'fixture-stdio\n'
    ;;
  *)
    exit 2
    ;;
esac
SH
chmod 0555 "$payload/artifacts/sub2api-host/sub2api-host-linux-amd64"
cp "$payload/artifacts/sub2api-host/sub2api-host-linux-amd64" "$payload/artifacts/sub2api-host/sub2api-host-linux-arm64"
chmod 0555 "$payload/artifacts/sub2api-host/sub2api-host-linux-arm64"
source_digest="$(sha256sum "$payload/artifacts/sub2api-host/sub2api-host-linux-amd64" | cut -d ' ' -f 1)"
payload_store="$(nix_command --offline --option max-jobs 1 --option builders '' store add-path --name fixture-project-payload "$payload")"
host_payload_store="$(nix_command --offline --option max-jobs 1 --option builders '' store add-path --name fixture-host-payload "$payload/artifacts/sub2api-host/sub2api-host-linux-amd64")"

expression="let
  flake = builtins.getFlake \"path:$repo_root\";
  pkgs = import flake.inputs.nixpkgs { system = \"x86_64-linux\"; };
  compose = import $repo_root/nix/environments.nix;
  common = { inherit pkgs; controllerWorkspaceInit = $repo_root/nix/controller-workspace-init.sh; hostActivate = $repo_root/nix/host-activate.sh; };
in {
  controller = compose (common // { role = \"controller\"; projectPayload = builtins.storePath \"$payload_store\"; });
  host = compose (common // { role = \"host-environment\"; hostPayload = { path = builtins.storePath \"$host_payload_store\"; release = \"$host_release\"; }; });
}"
controller="$(nix_command build --impure --no-link --print-out-paths --expr "$expression.controller")"
host="$(nix_command build --impure --no-link --print-out-paths --expr "$expression.host")"

[[ -x "$controller/bin/sub2api-deploy" && -x "$controller/bin/pulumi" && -x "$controller/bin/sops" && -x "$controller/bin/ssh" ]] || fail 'controller environment is incomplete'
[[ -x "$controller/libexec/pulumi" && -x "$controller/libexec/pulumi-resource-sub2api-host" ]] || fail 'Controller executable misses attached Pulumi siblings'
[[ "$("$controller/bin/go")" == fixture-go ]] || fail 'Controller Go shim cannot find its declared shell'
[[ "$("$controller/bin/pulumi" version)" == v3.256.0 ]] || fail 'Controller does not expose Pulumi 3.256.0'
[[ -x "$controller/workspace/bin/plugins/resource-cloudflare-v6.18.0/pulumi-resource-cloudflare" ]] || fail 'Cloudflare provider is missing'
[[ -x "$controller/workspace/bin/plugins/resource-upstash-v0.5.0/pulumi-resource-upstash" ]] || fail 'Upstash provider is missing'
workspace="$root/workspace"; mkdir "$workspace"
"$controller/bin/sub2api-workspace-init" "$workspace"
[[ "$(readlink -f "$workspace/Pulumi.yaml")" == "$controller/workspace/Pulumi.yaml" ]] || fail 'workspace initializer did not link Pulumi.yaml'

for binary in docker dockerd containerd runc nft iptables ip6tables ssh sub2api-host sub2api-host-activate; do
  [[ -x "$host/bin/$binary" ]] || fail "Host environment misses $binary"
done
[[ -f "$host/libexec/sub2api-host" && ! -L "$host/libexec/sub2api-host" ]] || fail 'Host payload is not a regular libexec file'
cmp -s "$payload/artifacts/sub2api-host/sub2api-host-linux-amd64" "$host/libexec/sub2api-host" || fail 'Host payload bytes changed during composition'
[[ -f "$host/bin/sub2api-host" && ! -L "$host/bin/sub2api-host" ]] || fail 'Host command is not a regular wrapper file'
[[ -x "$host/bin/sub2api-host" ]] || fail 'Host wrapper is not executable'
if cmp -s "$host/bin/sub2api-host" "$host/libexec/sub2api-host"; then
  fail 'Host command is not a distinct wrapper'
fi
mkdir -p "$root/decoy"
printf '#!/bin/sh\nexit 1\n' > "$root/decoy/docker"
printf '#!/bin/sh\nexit 1\n' > "$root/decoy/nft"
chmod 0755 "$root/decoy/docker" "$root/decoy/nft"
probe="$(cd "$root" && PATH="$root/decoy:$root:/usr/bin:/bin" "$host/bin/sub2api-host" probe)"
[[ "$(printf '%s\n' "$probe" | sed -n '4p')" == "$source_digest" ]] || fail 'Host wrapper probe did not report the source payload digest'
[[ "$(printf '%s\n' "$probe" | sed -n '5p')" == "$host_release" ]] || fail 'Host wrapper probe did not report the release identity'
[[ "$(cd "$root" && PATH="$root/decoy:$root:/usr/bin:/bin" "$host/bin/sub2api-host" stdio)" == fixture-stdio ]] || fail 'Host stdio could not locate controlled-PATH runtime tools'
[[ "$(<"$host/share/sub2api-host/release")" == "$host_release" ]] || fail 'Host environment lost the exact release identity'
[[ ! -e "$host/bin/pulumi-resource-sub2api-host" ]] || fail 'Host environment unexpectedly contains a Pulumi Provider'
unit="$host/etc/sub2api-nix-host/files/sub2api-nix-docker.service"
grep -Fq "ExecStart=$host/bin/dockerd" "$unit" || fail 'Docker unit is not bound to the final Host environment'
grep -Fq 'etc/sub2api-nix-host/files/sub2api-nix-docker.service /etc/systemd/system/sub2api-nix-docker.service' "$host/share/sub2api-runtime/activation-manifest" || fail 'activation manifest omits Docker unit'

profile="$root/profile"
nix_command profile add --profile "$profile" "$controller"
[[ -x "$profile/bin/sub2api-deploy" ]] || fail 'profile install did not expose Controller'
[[ "$("$profile/bin/sub2api-deploy")" == fixture-sub2api-deploy ]] || fail 'profile-installed Controller did not run'

host_profile="$root/host-profile"
nix_command profile add --profile "$host_profile" "$host"
[[ -x "$host_profile/bin/sub2api-host" ]] || fail 'profile install did not expose Host wrapper'
profile_probe="$(cd "$root" && PATH="$root/decoy:$root:/usr/bin:/bin" "$host_profile/bin/sub2api-host" probe)"
[[ "$(printf '%s\n' "$profile_probe" | sed -n '4p')" == "$source_digest" ]] || fail 'profile-installed Host probe did not report the source payload digest'
[[ "$(printf '%s\n' "$profile_probe" | sed -n '5p')" == "$host_release" ]] || fail 'profile-installed Host probe did not report the release identity'
[[ "$(cd "$root" && PATH="$root/decoy:$root:/usr/bin:/bin" "$host_profile/bin/sub2api-host" stdio)" == fixture-stdio ]] || fail 'profile-installed Host stdio could not locate controlled-PATH runtime tools'

printf 'profile install fixture tests passed\n'
