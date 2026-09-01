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

# A flock-protected ordinal record rejects concurrent, retried, skipped, and
# reordered fixed remote commands without retaining their contents.
publish() {
  want=$1
  record=$2
  shift 2
  exec 8>"$trace/ssh.ordinal.lock"
  flock -x 8
  ordinal_file="$trace/ssh.ordinal"
  ordinal=0
  [ -f "$ordinal_file" ] && ordinal=$(cat "$ordinal_file")
  [ "$want" = "$((ordinal + 1))" ] || { printf 'fixture SSH transition mismatch\n' >&2; exit 64; }
  [ "$want" != 4 ] || [ -f "$trace/bootstrap.complete" ] || { printf 'fixture Host before bootstrap\n' >&2; exit 64; }
  tmp="$ordinal_file.$$"
  printf '%s\n' "$want" > "$tmp"
  mv -f "$tmp" "$ordinal_file"
  record_args "$@" > "$trace/$record"
  flock -u 8
  exec 8>&-
}

publish_probe() {
  exec 8>"$trace/ssh.ordinal.lock"
  flock -x 8
  ordinal_file="$trace/ssh.ordinal"
  ordinal=0
  [ -f "$ordinal_file" ] && ordinal=$(cat "$ordinal_file")
  case "$ordinal" in
    0) next=1; count=1 ;;
    2) [ -f "$trace/bootstrap.complete" ] || { printf 'fixture probe transition mismatch\n' >&2; exit 64; }; next=3; count=2 ;;
    *) printf 'fixture probe transition mismatch\n' >&2; exit 64 ;;
  esac
  tmp="$ordinal_file.$$"
  printf '%s\n' "$next" > "$tmp"
  mv -f "$tmp" "$ordinal_file"
  record_args "$@" > "$trace/ssh.probe.$count.args"
  flock -u 8
  exec 8>&-
  printf '%s\n' "$count"
}

if printf %s "$remote" | cmp -s "$PROVIDER_RUNTIME_PROBE_COMMAND" -; then
    count=$(publish_probe "$@")
    digest=missing
    if [ -f "$trace/bootstrap.complete" ]; then
      digest=$(sha256sum "$PROVIDER_RUNTIME_ARTIFACT" | awk '{print $1}')
    fi
    printf 's2p1:Linux\namd64\n0911601b3b0a5f6fdc51f3661518ee20e26ea0cbadfb4f7283e5b1f288941f54\n%s\n' "$digest"
    exit 0
fi
if printf %s "$remote" | cmp -s "$PROVIDER_RUNTIME_HOST_COMMAND" -; then
  if [ "${PROVIDER_RUNTIME_LIFECYCLE_SCENARIO:-false}" = true ]; then
    exec 8>"$trace/ssh.ordinal.lock"
    flock -x 8
    ordinal_file="$trace/ssh.ordinal"
    ordinal=0
    [ -f "$ordinal_file" ] && ordinal=$(cat "$ordinal_file")
    [ "$ordinal" -ge 3 ] || { printf 'fixture Host before bootstrap\n' >&2; exit 64; }
    ordinal=$((ordinal + 1))
    tmp="$ordinal_file.$$"
    printf '%s\n' "$ordinal" > "$tmp"
    mv -f "$tmp" "$ordinal_file"
    host_count=0
    [ -f "$trace/ssh.host.count" ] && host_count=$(cat "$trace/ssh.host.count")
    host_count=$((host_count + 1))
    printf '%s\n' "$host_count" > "$trace/ssh.host.count"
    record_args "$@" > "$trace/ssh.host.$host_count.args"
    flock -u 8
    exec 8>&-
  else
    publish 4 ssh.host.args "$@"
  fi
  response="$trace/ssh.host.response.$$"
  if env \
    SUB2API_PROVIDER_RUNTIME_CI_HELPER=1 \
    SUB2API_PROVIDER_RUNTIME_ROOT="$PROVIDER_RUNTIME_ROOT" \
    SUB2API_PROVIDER_RUNTIME_MACHINE_ID="$PROVIDER_RUNTIME_MACHINE_ID" \
    SUB2API_PROVIDER_RUNTIME_MODE=serve \
    PROVIDER_RUNTIME_REQUEST_DIGEST="$trace/ssh.host.request.sha256" \
    PROVIDER_RUNTIME_TRACE="$PROVIDER_RUNTIME_TRACE" \
    PATH="$PATH" \
    "$PROVIDER_RUNTIME_TEST_BINARY" -test.run '^TestProviderRuntimeCIHelper$' >"$response"; then
      :
  else
      rm -f "$response"
      exit 1
  fi
  metadata="$trace/ssh.host.request.sha256"
  [ -f "$metadata" ] || { rm -f "$response"; printf 'fixture missing Host metadata\n' >&2; exit 64; }
  [ "$(wc -l < "$metadata")" -eq 2 ] || { rm -f "$response"; printf 'fixture invalid Host metadata\n' >&2; exit 64; }
  action=$(awk -F= '$1 == "action" { count++; value=$2 } END { if (count == 1) print value }' "$metadata")
  digest=$(awk -F= '$1 == "operationDigest" { count++; value=$2 } END { if (count == 1) print value }' "$metadata")
  case "$action" in inspect|reconcile|retire-preserve-data) ;; *) rm -f "$response"; printf 'fixture invalid Host action\n' >&2; exit 64 ;; esac
  [ ${#digest} -eq 64 ] && case "$digest" in *[!0123456789abcdef]*) false ;; *) true ;; esac || { rm -f "$response"; printf 'fixture invalid Host digest\n' >&2; exit 64; }
  exec 8>"$trace/ssh.ordinal.lock"
  flock -x 8
  queue="$trace/host-action.queue"
  [ -f "$queue" ] || { flock -u 8; exec 8>&-; rm -f "$response"; printf 'fixture Host action queue absent\n' >&2; exit 64; }
  expected_action=$(awk 'NF { print; exit }' "$queue")
  [ "$expected_action" = "$action" ] || { flock -u 8; exec 8>&-; rm -f "$response"; printf 'fixture Host action queue mismatch\n' >&2; exit 64; }
  queue_tmp="$queue.$$"
  awk 'seen { print; next } NF { seen=1 }' "$queue" > "$queue_tmp"
  mv -f "$queue_tmp" "$queue"
  marker="$trace/drop-host-response.$action"
  if [ -f "$marker" ]; then
    rm -f "$marker" "$response"
    drop_tmp="$trace/ssh.host.response-loss.$$"
    printf 'action=%s\noperationDigest=%s\ndropped-after-complete\n' "$action" "$digest" > "$drop_tmp"
    mv -f "$drop_tmp" "$trace/ssh.host.response-loss"
    flock -u 8
    exec 8>&-
    exit 1
  fi
  flock -u 8
  exec 8>&-
  cat "$response"
  rm -f "$response"
  exit 0
fi
if ! printf %s "$remote" | cmp -s "$PROVIDER_RUNTIME_BOOTSTRAP_COMMAND" -; then
  printf 'fixture remote command mismatch\n' >&2
  exit 64
fi
publish 2 ssh.bootstrap.args "$@"

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
exec 8>"$trace/ssh.ordinal.lock"
flock -x 8
bootstrap_tmp="$trace/bootstrap.meta.$$"
printf 'size=%s\ndigest=%s\n' "$size" "$digest" > "$bootstrap_tmp"
mv -f "$bootstrap_tmp" "$trace/bootstrap.meta"
flock -u 8
exec 8>&-
touch "$trace/bootstrap.complete"

exec env \
SUB2API_PROVIDER_RUNTIME_CI_HELPER=1 \
SUB2API_PROVIDER_RUNTIME_ROOT="$PROVIDER_RUNTIME_ROOT" \
SUB2API_PROVIDER_RUNTIME_MACHINE_ID="$PROVIDER_RUNTIME_MACHINE_ID" \
SUB2API_PROVIDER_RUNTIME_MODE=bootstrap \
PROVIDER_RUNTIME_REQUEST_DIGEST="$PROVIDER_RUNTIME_REQUEST_DIGEST" \
PROVIDER_RUNTIME_TRACE="$PROVIDER_RUNTIME_TRACE" \
PATH="$PATH" \
"$PROVIDER_RUNTIME_TEST_BINARY" -test.run '^TestProviderRuntimeCIHelper$'
