#!/bin/bash
# Make the release's fixed Pulumi workspace layout available without changing cwd.
set -euo pipefail

[[ "$#" -eq 1 ]] || { printf 'usage: sub2api-workspace-init <workspace>\n' >&2; exit 2; }
script_path="${BASH_SOURCE[0]}"
[[ "$script_path" == /* && ! -L "$script_path" ]] || { printf 'sub2api-workspace-init: invoke by absolute non-symlink path\n' >&2; exit 1; }
script_dir="${script_path%/*}"
payload_root="$(CDPATH= cd -- "$script_dir/.." && pwd -P)"
workspace="$1"
[[ -d "$workspace" ]] || { printf 'sub2api-workspace-init: workspace does not exist: %s\n' "$workspace" >&2; exit 1; }
source_workspace="$payload_root/workspace"
[[ -f "$source_workspace/Pulumi.yaml" && -d "$source_workspace/bin" ]] || {
  printf 'sub2api-workspace-init: controller payload workspace is incomplete\n' >&2; exit 1;
}

check_link() {
  local destination="$1" target="$2"
  if [[ -e "$destination" || -L "$destination" ]]; then
    [[ -L "$destination" && "$(readlink -f -- "$destination")" == "$(readlink -f -- "$target")" ]] || {
      printf 'sub2api-workspace-init: refusing to replace existing %s\n' "$destination" >&2; exit 1;
    }
  fi
}
install_link() {
  local destination="$1" target="$2"
  [[ -e "$destination" || -L "$destination" ]] && return
  ln -s -- "$target" "$destination"
}
check_link "$workspace/Pulumi.yaml" "$source_workspace/Pulumi.yaml"
check_link "$workspace/bin" "$source_workspace/bin"
install_link "$workspace/Pulumi.yaml" "$source_workspace/Pulumi.yaml"
install_link "$workspace/bin" "$source_workspace/bin"
