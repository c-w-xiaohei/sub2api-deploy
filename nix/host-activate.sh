#!/usr/bin/env bash
# Explicit, bounded Ubuntu/systemd integration for a prebuilt Host payload.
set -euo pipefail
umask 077
PATH=/usr/sbin:/usr/bin:/sbin:/bin
export PATH

activation_root=""
root_path() { printf '%s%s\n' "$activation_root" "$1"; }
fail() { printf 'sub2api-host-activate: %s\n' "$*" >&2; exit 1; }

action="configure"
[[ "$#" -le 1 ]] || fail "usage: sub2api-host-activate [--configure|--start|--restart]"
case "${1:-}" in
  ""|--configure) ;;
  --start) action="start" ;;
  --restart) action="restart" ;;
  *) fail "usage: sub2api-host-activate [--configure|--start|--restart]" ;;
esac

[[ "${EUID}" -eq 0 ]] || fail "must be run as root"
[[ -z "${SUB2API_HOST_ACTIVATION_TEST_ROOT:-}${SUB2API_HOST_RUNTIME_ROOT:-}" ]] || fail "test runtime overrides are not accepted"
runtime_root="$(CDPATH= cd -- "$(dirname -- "$(readlink -f -- "${BASH_SOURCE[0]}")")/.." && pwd -P)"
[[ -d "$runtime_root" && ! -L "$runtime_root" ]] || fail "runtime payload must be a real directory"
expected_owner=0

[[ "$(stat -c %u "$runtime_root")" == "$expected_owner" ]] || fail "runtime payload must be root-owned"
[[ -z "$(find "$runtime_root" -xdev -type l -print -quit)" ]] || fail "runtime payload must not contain symlinks"
[[ -z "$(find "$runtime_root" -xdev \( -type f -o -type d \) -perm /022 -print -quit)" ]] || fail "runtime payload must not be group or world writable"

os_release="$(root_path /etc/os-release)"
[[ -r "$os_release" ]] || fail "Ubuntu 24.04 os-release is required"
# shellcheck disable=SC1090
source "$os_release"
[[ "${ID:-}" == ubuntu && "${VERSION_ID:-}" == 24.04 ]] || fail "only Ubuntu 24.04 with systemd is supported"
[[ -d "$(root_path /run/systemd/system)" ]] || fail "systemd is required"

for binary in docker dockerd containerd runc nft ssh; do
  [[ -x "$runtime_root/bin/$binary" && ! -L "$runtime_root/bin/$binary" ]] || fail "runtime payload misses safe bin/$binary"
done

unit_dir="$(root_path /etc/systemd/system)"
docker_config="$(root_path /etc/docker/daemon.json)"
manifest="$(root_path /etc/sub2api-nix-host/ownership.sha256)"
pending="$(root_path /etc/sub2api-nix-host/activation.pending)"
profile="$(root_path /nix/var/nix/profiles/sub2api-host)"
data_root="$(root_path /var/lib/docker)"
unit="$unit_dir/sub2api-nix-docker.service"
socket="$unit_dir/sub2api-nix-docker.socket"
nix_env="$(root_path /nix/var/nix/profiles/default/bin/nix-env)"
systemctl_bin="$(root_path /bin/systemctl)"
[[ -x "$nix_env" ]] || fail "Nix root profile tool is unavailable: $nix_env"
[[ -x "$systemctl_bin" ]] || fail "systemctl is unavailable: $systemctl_bin"

generated_manifest="$runtime_root/share/sub2api-runtime/activation-manifest"
[[ -f "$generated_manifest" && ! -L "$generated_manifest" ]] || fail "runtime payload misses generated activation manifest"
[[ -d "$runtime_root/etc/sub2api-nix-host/files" && ! -L "$runtime_root/etc/sub2api-nix-host/files" ]] || fail "runtime payload misses generated activation files"

owned_hash() {
  local kind="$1" path="$2" hash
  [[ -f "$manifest" && -f "$path" && ! -L "$path" ]] || return 1
  hash="$(sha256sum "$path" | cut -d ' ' -f 1)"
  grep -Fqx -- "$hash $kind $path" "$manifest"
}
owned_data_root() { [[ -f "$manifest" ]] && grep -Fqx -- "managed-data-root $data_root" "$manifest"; }
pending_owned() {
  local hash path
  [[ -f "$pending" && ! -L "$pending" ]] || return 1
  grep -Fqx -- "payload $runtime_root" "$pending" || return 1
  # Bind every recoverable target to this exact candidate, not to arbitrary
  # paths or hashes supplied by a truncated/stale transaction record.
  cmp -s <(grep '^target ' "$pending") <(candidate_targets) || return 1
  while read -r hash path; do
    [[ "$hash" == target ]] && continue
    [[ ! -e "$path" && ! -L "$path" ]] && continue
    [[ -f "$path" && ! -L "$path" && "$(sha256sum "$path" | cut -d ' ' -f 1)" == "$hash" ]] || return 1
  done < <(grep '^target ' "$pending" | cut -d ' ' -f 2-)
  return 0
}
candidate_targets() {
  while read -r mode hash source destination; do
    [[ -n "$mode" ]] || continue
    [[ "$destination" == /etc/systemd/system/sub2api-nix-docker.service || "$destination" == /etc/systemd/system/sub2api-nix-docker.socket || "$destination" == /etc/docker/daemon.json ]] || fail "activation manifest has an unmanaged destination"
    printf 'target %s %s\n' "$hash" "$(root_path "$destination")"
  done <"$generated_manifest"
}
active_owned_socket() {
  [[ -S "$(root_path /run/docker.sock)" && ! -L "$(root_path /run/docker.sock)" ]] &&
    owned_hash file "$unit" && owned_hash file "$socket" &&
    "$systemctl_bin" is-active --quiet sub2api-nix-docker.socket &&
    [[ "$("$systemctl_bin" show --value -p FragmentPath sub2api-nix-docker.socket)" == "$socket" ]] &&
    [[ "$("$systemctl_bin" show --value -p Listen sub2api-nix-docker.socket)" == "/run/docker.sock (Stream)" ]]
}

safe_ancestor() {
  local path="$1"
  while [[ "$path" != / ]]; do
    [[ ! -L "$path" && "$(stat -c %u "$path")" == "$expected_owner" ]] || fail "unsafe ancestor: $path"
    [[ -z "$(find "$path" -maxdepth 0 -perm /022 -print)" ]] || fail "writable ancestor: $path"
    path="$(dirname "$path")"
  done
}

# All preflight decisions occur before any system file or profile mutation.
for path in "$unit_dir" "$(dirname "$docker_config")" "$(dirname "$manifest")" "$(dirname "$profile")"; do
  [[ -e "$path" ]] && safe_ancestor "$path" || safe_ancestor "$(dirname "$path")"
done
for path in "$manifest" "$pending"; do
  [[ ! -e "$path" && ! -L "$path" ]] || { [[ -f "$path" && ! -L "$path" && "$(stat -c %u "$path")" == "$expected_owner" && "$(stat -c %a "$path")" == 600 ]] || fail "unsafe activation record: $path"; }
done
for path in "$unit" "$socket"; do
  [[ ! -e "$path" && ! -L "$path" ]] || owned_hash file "$path" || pending_owned || fail "refusing to adopt existing Docker unit/socket: $path"
done
for path in \
  "$(root_path /etc/systemd/system/docker.service)" "$(root_path /etc/systemd/system/docker.socket)" \
  "$(root_path /run/systemd/system/docker.service)" "$(root_path /run/systemd/system/docker.socket)" \
  "$(root_path /usr/lib/systemd/system/docker.service)" "$(root_path /usr/lib/systemd/system/docker.socket)" \
  "$(root_path /lib/systemd/system/docker.service)" "$(root_path /lib/systemd/system/docker.socket)"; do
  [[ ! -e "$path" && ! -L "$path" ]] || fail "refusing to compete with existing Docker unit/socket: $path"
done
for name in docker.service docker.socket docker; do
  loaded_fragment="$("$systemctl_bin" show --value -p FragmentPath "$name" 2>/dev/null || true)"
  [[ -z "$loaded_fragment" || "$loaded_fragment" == n/a ]] || fail "refusing loaded foreign systemd unit: $name"
done
if [[ -e "$(dirname "$docker_config")" ]]; then
  [[ ! -e "$docker_config" && -z "$(ls -A "$(dirname "$docker_config")")" ]] || owned_hash file "$docker_config" || pending_owned || fail "refusing to adopt existing Docker configuration: $(dirname "$docker_config")"
fi
if [[ -e "$data_root" ]] && ! owned_data_root; then
  fail "refusing to adopt existing Docker data root: $data_root"
fi
socket_path="$(root_path /run/docker.sock)"
if [[ -e "$socket_path" || -L "$socket_path" ]]; then
  active_owned_socket || fail "refusing to adopt existing Docker socket: $socket_path"
fi

mkdir -p "$unit_dir" "$(dirname "$docker_config")" "$(dirname "$manifest")" "$(dirname "$profile")"
umask 077
recovering=0
[[ ! -e "$pending" ]] || recovering=1
temporary_pending="$(mktemp -p "$(dirname "$pending")" .activation.XXXXXX)"
{
  printf 'payload %s\n' "$runtime_root"
  printf 'stage files\n'
  candidate_targets
} >"$temporary_pending"
mv -f "$temporary_pending" "$pending"
chmod 0600 "$pending"
changed=0
write_owned() {
  local mode="$1" hash="$2" source="$3" destination="$4" temporary
  [[ "$mode" == 0644 && "$source" != /* && "$destination" == /* ]] || fail "invalid generated activation manifest entry"
  source="$runtime_root/$source"
  [[ -f "$source" && ! -L "$source" && "$(sha256sum "$source" | cut -d ' ' -f 1)" == "$hash" ]] || fail "generated activation file digest mismatch: $source"
  destination="$(root_path "$destination")"
  temporary="$(mktemp -p "$(dirname "$destination")" ".$(basename "$destination").XXXXXX")"
  cp -- "$source" "$temporary"
  if [[ ! -f "$destination" ]] || ! cmp -s "$temporary" "$destination"; then mv -f "$temporary" "$destination"; changed=1; else rm -f "$temporary"; fi
  chmod "$mode" "$destination"
}
while read -r mode hash source destination; do
  [[ -n "$mode" ]] || continue
  write_owned "$mode" "$hash" "$source" "$destination"
done <"$generated_manifest"

# Profile generations remain GC roots. Do not delete the prior generation: an
# already-running daemon may still execute its old closure until maintenance.
profile_target=""
[[ -e "$profile" || -L "$profile" ]] && profile_target="$(readlink -f -- "$profile")"
if [[ "$profile_target" != "$runtime_root" ]]; then "$nix_env" -p "$profile" --set "$runtime_root"; fi
sed -i 's/^stage .*/stage systemd/' "$pending"
temporary_manifest="$(mktemp -p "$(dirname "$manifest")" ".$(basename "$manifest").XXXXXX")"
{
  sha256sum "$unit" | awk -v path="$unit" '{print $1 " file " path}'
  sha256sum "$socket" | awk -v path="$socket" '{print $1 " file " path}'
  sha256sum "$docker_config" | awk -v path="$docker_config" '{print $1 " file " path}'
  printf 'managed-data-root %s\n' "$data_root"
} >"$temporary_manifest"
if [[ ! -f "$manifest" ]] || ! cmp -s "$temporary_manifest" "$manifest"; then mv -f "$temporary_manifest" "$manifest"; else rm -f "$temporary_manifest"; fi
chmod 0600 "$manifest"
if [[ "$changed" -eq 1 || "$recovering" -eq 1 ]]; then "$systemctl_bin" daemon-reload; "$systemctl_bin" enable sub2api-nix-docker.socket; fi
rm -f "$pending"
case "$action" in
  configure) ;;
  start) "$systemctl_bin" start sub2api-nix-docker.socket ;;
  restart) "$systemctl_bin" restart sub2api-nix-docker.service ;;
esac
printf 'sub2api-host-activate: root profile and owned Docker integration are installed\n'
