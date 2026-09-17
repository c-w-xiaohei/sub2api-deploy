#!/usr/bin/env bash
set -euo pipefail

[[ "$#" -eq 1 ]] || { printf 'usage: install-runtime.sh {controller|host-environment}\n' >&2; exit 2; }
role="$1"
case "$role" in
  controller|host-environment) ;;
  *) printf 'install-runtime.sh: unsupported role: %s\n' "$role" >&2; exit 2 ;;
esac

script_dir="$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
flake="path:${script_dir%/*}"
nix_command() { nix --extra-experimental-features 'nix-command flakes fetch-tree' "$@"; }
system="$(nix_command eval --impure --raw --expr builtins.currentSystem)"
inputs="$(nix_command eval --raw "$flake#runtimeInputs.$system.$role" --apply 'paths: builtins.concatStringsSep "\n" paths')"
[[ -n "$inputs" ]] || { printf 'install-runtime.sh: runtime input list is empty\n' >&2; exit 1; }

while IFS= read -r path; do
  [[ -n "$path" ]] || continue
  nix_command copy --from https://cache.nixos.org "$path"
done <<< "$inputs"

nix_command eval --json "$flake#runtimeSources.$system.$role" >/dev/null

profile_args=()
if [[ "$role" == host-environment ]]; then
  profile_args=(--profile /nix/var/nix/profiles/sub2api-host)
fi
nix_command profile add "${profile_args[@]}" --offline --option substituters '' "$flake#$role"
