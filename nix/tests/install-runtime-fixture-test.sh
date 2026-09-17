#!/usr/bin/env bash
set -euo pipefail

repo_root="$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)"
root="$(mktemp -d)"
trap 'rm -rf "$root"' EXIT
log="$root/nix.log"
mkdir "$root/bin"

cat > "$root/bin/nix" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >> "$NIX_FIXTURE_LOG"
[[ "$1 $2" == "--extra-experimental-features nix-command flakes fetch-tree" ]] || exit 69
shift 2
case "$1 $2" in
  'eval --impure') printf 'x86_64-linux' ;;
  'eval --raw') printf '/nix/store/first\n/nix/store/second\n' ;;
  'eval --json') printf '["/nix/store/candidate"]' ;;
  'copy --from') [[ "${FAIL_SECOND_COPY:-0}" != 1 || "${@: -1}" != /nix/store/second ]] ;;
  'profile add') exit 0 ;;
  *) exit 70 ;;
esac
SH
chmod 0755 "$root/bin/nix"

if FAIL_SECOND_COPY=1 NIX_FIXTURE_LOG="$log" PATH="$root/bin:/usr/bin:/bin" "$repo_root/nix/install-runtime.sh" host-environment; then
  printf 'FAIL: installer ignored a binary-cache copy failure\n' >&2
  exit 1
fi
if grep -Fq 'profile add' "$log"; then
  printf 'FAIL: installer continued after a binary-cache copy failure\n' >&2
  exit 1
fi

: > "$log"
NIX_FIXTURE_LOG="$log" PATH="$root/bin:/usr/bin:/bin" "$repo_root/nix/install-runtime.sh" host-environment
grep -Fq -- "--extra-experimental-features nix-command flakes fetch-tree profile add --profile /nix/var/nix/profiles/sub2api-host --offline --option substituters  path:$repo_root#host-environment" "$log" || {
  printf 'FAIL: installer did not disable substituters for profile installation\n' >&2
  exit 1
}

printf 'runtime installer fixture tests passed\n'
