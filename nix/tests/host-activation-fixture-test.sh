#!/usr/bin/env bash
set -euo pipefail
repo_root="$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)"
root="$(mktemp -d)"; trap 'rm -rf "$root"' EXIT
log="$root/commands.log"
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
expect_failure() { local expected="$1"; shift; local output; if output="$("$@" 2>&1)"; then fail "expected failure"; fi; [[ "$output" == *"$expected"* ]] || fail "$output"; }

# Test copy has fixed paths. Production code has no test-environment escape hatch.
mkdir -p "$root/payload/bin" "$root"/{etc/docker,etc/systemd/system,run/systemd/system,run,nix/var/nix/profiles/default/bin,bin}
cp "$repo_root/nix/host-activate.sh" "$root/payload/bin/sub2api-host-activate"
mkdir -p "$root/payload/etc/sub2api-nix-host/files" "$root/payload/share/sub2api-runtime"
for file in sub2api-nix-docker.service sub2api-nix-docker.socket daemon.json; do
  cp "$repo_root/nix/tests/fixtures/$file" "$root/payload/etc/sub2api-nix-host/files/$file"
done
{
  for entry in \
    'sub2api-nix-docker.service /etc/systemd/system/sub2api-nix-docker.service' \
    'sub2api-nix-docker.socket /etc/systemd/system/sub2api-nix-docker.socket' \
    'daemon.json /etc/docker/daemon.json'; do
    set -- $entry
    printf '0644 %s etc/sub2api-nix-host/files/%s %s\n' "$(sha256sum "$root/payload/etc/sub2api-nix-host/files/$1" | cut -d ' ' -f 1)" "$1" "$2"
  done
} >"$root/payload/share/sub2api-runtime/activation-manifest"
script_contents="$(<"$root/payload/bin/sub2api-host-activate")"
script_contents="${script_contents/activation_root=\"\"/activation_root=\"$root\"}"
script_contents="${script_contents/'[[ "${EUID}" -eq 0 ]] || fail "must be run as root"'/':'}"
script_contents="${script_contents/expected_owner=0/expected_owner=$(id -u)}"
script_contents="${script_contents/'while [[ "$path" != / ]]; do'/'while [[ "$path" != "'$root'" ]]; do'}"
printf '%s' "$script_contents" >"$root/payload/bin/sub2api-host-activate"
chmod 0755 "$root/payload/bin/sub2api-host-activate"
printf 'ID=ubuntu\nVERSION_ID="24.04"\n' >"$root/etc/os-release"
for binary in docker dockerd containerd runc nft ssh; do printf '#!/usr/bin/env bash\nexit 0\n' >"$root/payload/bin/$binary"; chmod 0755 "$root/payload/bin/$binary"; done
cat >"$root/nix/var/nix/profiles/default/bin/nix-env" <<'SH'
#!/usr/bin/env bash
printf 'nix-env %s\n' "$*" >>"ROOT/commands.log"
if [[ "${FAIL_NIX_ENV:-}" == 1 ]]; then exit 1; fi
ln -sfn "${@: -1}" "${@: -3:1}"
SH
cat >"$root/bin/systemctl" <<'SH'
#!/usr/bin/env bash
printf 'systemctl %s\n' "$*" >>"ROOT/commands.log"
case "$*" in
  'is-active --quiet sub2api-nix-docker.socket') exit "${SOCKET_ACTIVE:-3}" ;;
  'show --value -p FragmentPath sub2api-nix-docker.socket') printf '%s\n' 'ROOT/etc/systemd/system/sub2api-nix-docker.socket' ;;
  'show --value -p Listen sub2api-nix-docker.socket') printf '/run/docker.sock (Stream)\n' ;;
  'show --value -p FragmentPath docker.service'|'show --value -p FragmentPath docker.socket'|'show --value -p FragmentPath docker') printf 'n/a\n' ;;
esac
SH
for path in "$root/nix/var/nix/profiles/default/bin/nix-env" "$root/bin/systemctl"; do
  contents="$(<"$path")"
  printf '%s' "${contents//ROOT/$root}" >"$path"
done
chmod 0755 "$root/nix/var/nix/profiles/default/bin/nix-env" "$root/bin/systemctl"
activate() { "$root/payload/bin/sub2api-host-activate" "$@"; }

printf '{}' >"$root/etc/docker/foreign.json"
expect_failure 'refusing to adopt existing Docker configuration' activate
rm -rf "$root/etc/docker"
mkdir -p "$root/usr/lib/systemd/system"; printf '[Unit]\n' >"$root/usr/lib/systemd/system/docker.service"
expect_failure 'refusing to compete with existing Docker unit/socket' activate
rm -rf "$root/usr"

# The actual safe_ancestor body rejects a writable managed directory before writes.
mkdir -p "$root/etc/docker"
chmod 0777 "$root/etc/docker"
expect_failure 'writable ancestor' activate
chmod 0755 "$root/etc/docker"

(umask 000; activate)
[[ "$(stat -c %a "$root/etc/sub2api-nix-host")" == 700 ]] || fail 'activation record directory inherited an unsafe umask'
jq -e . "$root/etc/docker/daemon.json" >/dev/null || fail 'invalid JSON'
calls_before="$(grep -c '^nix-env ' "$log")"
reloads_before="$(grep -c '^systemctl daemon-reload$' "$log")"
inode_before="$(stat -c %i "$root/etc/sub2api-nix-host/ownership.sha256")"

# Matching bytes do not establish ownership of a symlink at a managed path.
for managed in "$root/etc/systemd/system/sub2api-nix-docker.service" "$root/etc/docker/daemon.json"; do
  mv "$managed" "$managed.saved"
  ln -s "$managed.saved" "$managed"
  expect_failure 'refusing to adopt existing Docker' activate
  rm "$managed"
  mv "$managed.saved" "$managed"
done

# A genuine UNIX socket is accepted only with owned active systemd evidence.
python3 - "$root/run/docker.sock" <<'PY'
import socket, sys
s=socket.socket(socket.AF_UNIX); s.bind(sys.argv[1]); s.listen(1)
open(sys.argv[1]+'.pid', 'w').write('fixture')
PY
SOCKET_ACTIVE=0 activate
[[ "$(stat -c %F "$root/run/docker.sock")" == socket ]] || fail 'fixture socket is not UNIX'
[[ "$calls_before" == "$(grep -c '^nix-env ' "$log")" ]] || fail 'idempotent activation changed profile'
[[ "$reloads_before" == "$(grep -c '^systemctl daemon-reload$' "$log")" ]] || fail 'idempotent activation reloaded systemd'
[[ "$inode_before" == "$(stat -c %i "$root/etc/sub2api-nix-host/ownership.sha256")" ]] || fail 'idempotent activation rewrote manifest'

SOCKET_ACTIVE=0 activate --start; SOCKET_ACTIVE=0 activate --restart
grep -Fqx 'systemctl start sub2api-nix-docker.socket' "$log" || fail '--start missing'
grep -Fqx 'systemctl restart sub2api-nix-docker.service' "$log" || fail '--restart missing'

# A regular file at the socket location must not be treated as an owned socket.
rm -f "$root/run/docker.sock"
: >"$root/run/docker.sock"
SOCKET_ACTIVE=0 expect_failure 'refusing to adopt existing Docker socket' activate
rm -f "$root/run/docker.sock"

# A failed profile switch leaves only the private pending marker and re-enters safely.
rm -f "$root/run/docker.sock" "$root/nix/var/nix/profiles/sub2api-host"
FAIL_NIX_ENV=1 expect_failure '' activate
[[ -f "$root/etc/sub2api-nix-host/activation.pending" ]] || fail 'missing pending marker'
activate
[[ ! -e "$root/etc/sub2api-nix-host/activation.pending" ]] || fail 'pending marker not cleared'

# Simulate interruption after only the first target has been committed. Missing
# later targets are recoverable, while a corrupt pending hash is not ownership.
rm -f "$root/nix/var/nix/profiles/sub2api-host"
FAIL_NIX_ENV=1 expect_failure '' activate
rm -f "$root/etc/sub2api-nix-host/ownership.sha256" \
  "$root/etc/systemd/system/sub2api-nix-docker.socket" "$root/etc/docker/daemon.json"
activate
[[ -f "$root/etc/systemd/system/sub2api-nix-docker.socket" ]] || fail 'partial transaction was not recovered'
[[ ! -e "$root/etc/sub2api-nix-host/activation.pending" ]] || fail 'partial pending marker not cleared'
printf 'host activation fixture tests passed\n'
