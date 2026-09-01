#!/bin/sh
set -eu

# CI-only OpenSSH endpoint. It never acts as or installs a Host binary.
trace=${PROVIDER_RUNTIME_TRACE:?}
mkdir -p "$trace"
pgid=$(awk '{print $5}' "/proc/$$/stat")
start=$(awk '{print $22}' "/proc/$$/stat")
identity_tmp="$trace/ssh.identity.$$"
printf '%s %s %s\n' "$$" "$start" "$pgid" > "$identity_tmp"
mv -f "$identity_tmp" "$trace/ssh.identity"

expected_log_dir=${PROVIDER_RUNTIME_CLIENT_LOG_DIR:?}
[ "$#" -eq 48 ] || { printf 'fixture argv count mismatch\n' >&2; exit 64; }
set -- "$@"
expected='-T -a -x -o BatchMode=yes -o NumberOfPasswordPrompts=0 -o RequestTTY=no -o ForwardAgent=no -o ForwardX11=no -o ForwardX11Trusted=no -o ClearAllForwardings=yes -o Tunnel=no -o ExitOnForwardFailure=yes -o StrictHostKeyChecking=yes -o UpdateHostKeys=no -o PermitLocalCommand=no -o ForkAfterAuthentication=no -o ControlMaster=no -o ControlPath=none -o RemoteCommand=none -o SessionType=default -o StdinNull=no -o ConnectTimeout=10 -o LogLevel=ERROR'
index=1
for want in $expected; do
  eval "got=\${$index}"
  [ "$got" = "$want" ] || { printf 'fixture argv mismatch\n' >&2; exit 64; }
  index=$((index + 1))
done
eval "flag=\${$index}"; [ "$flag" = -E ] || { printf 'fixture client log flag mismatch\n' >&2; exit 64; }; index=$((index + 1))
eval "client_log=\${$index}"
case "$client_log" in "$expected_log_dir"/sub2api-ssh-*) ;; *) printf 'fixture client log path mismatch\n' >&2; exit 64 ;; esac
[ -f "$client_log" ] || { printf 'fixture client log missing\n' >&2; exit 64; }
index=$((index + 1)); eval "terminator=\${$index}"; [ "$terminator" = -- ] || { printf 'fixture argv terminator mismatch\n' >&2; exit 64; }
index=$((index + 1)); eval "alias=\${$index}"; [ "$alias" = edge ] || { printf 'fixture alias mismatch\n' >&2; exit 64; }
index=$((index + 1)); eval "remote=\${$index}"

record_args() {
  n=1
  for value do
    sum=$(printf %s "$value" | sha256sum | awk '{print $1}')
    printf '%s %s\n' "$n" "$sum"
    n=$((n + 1))
  done
}

if printf %s "$remote" | cmp -s "$PROVIDER_RUNTIME_PROBE_COMMAND" -; then
    record_args "$@" > "$trace/ssh.probe.args"
    printf 's2p1:Linux\namd64\n0911601b3b0a5f6fdc51f3661518ee20e26ea0cbadfb4f7283e5b1f288941f54\nmissing\n'
    exit 0
fi
if ! printf %s "$remote" | cmp -s "$PROVIDER_RUNTIME_BOOTSTRAP_COMMAND" -; then
  printf 'fixture remote command mismatch\n' >&2
  exit 64
fi
record_args "$@" > "$trace/ssh.bootstrap.args"

IFS= read -r header
case "$header" in s2a1:*:*) ;; *) printf 'fixture artifact magic mismatch\n' >&2; exit 64 ;; esac
body=${header#s2a1:}
size=${body%%:*}
digest=${body#*:}
case "$size" in ''|*[!0-9]*) printf 'fixture artifact size mismatch\n' >&2; exit 64 ;; esac
[ ${#digest} -eq 64 ] || { printf 'fixture artifact digest mismatch\n' >&2; exit 64; }
case "$digest" in *[!0123456789abcdef]*) printf 'fixture artifact digest mismatch\n' >&2; exit 64 ;; esac
artifact=${PROVIDER_RUNTIME_ARTIFACT:?}
[ "$(wc -c < "$artifact")" = "$size" ] || { printf 'fixture artifact length mismatch\n' >&2; exit 64; }
[ "$(sha256sum "$artifact" | awk '{print $1}')" = "$digest" ] || { printf 'fixture artifact hash mismatch\n' >&2; exit 64; }
dd bs=1 count="$size" status=none | cmp -s - "$artifact" || { printf 'fixture artifact body mismatch\n' >&2; exit 64; }
printf 'size=%s\ndigest=%s\n' "$size" "$digest" > "$trace/bootstrap.meta"

exec env \
SUB2API_PROVIDER_RUNTIME_CI_HELPER=1 \
SUB2API_PROVIDER_RUNTIME_ROOT="$PROVIDER_RUNTIME_ROOT" \
SUB2API_PROVIDER_RUNTIME_MACHINE_ID="$PROVIDER_RUNTIME_MACHINE_ID" \
SUB2API_PROVIDER_RUNTIME_MODE=bootstrap \
PROVIDER_RUNTIME_REQUEST_DIGEST="$PROVIDER_RUNTIME_REQUEST_DIGEST" \
PROVIDER_RUNTIME_TRACE="$PROVIDER_RUNTIME_TRACE" \
PATH="$PATH" \
"$PROVIDER_RUNTIME_TEST_BINARY" -test.run '^TestProviderRuntimeCIHelper$'
