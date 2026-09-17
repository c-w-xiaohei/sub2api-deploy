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

# A flock-protected ordinal record rejects concurrent, skipped, and reordered
# fixed remote commands without retaining their contents.
publish() {
  record=$1
  shift
  exec 8>"$trace/ssh.ordinal.lock"
  flock -x 8
  ordinal_file="$trace/ssh.ordinal"
  ordinal=0
  [ -f "$ordinal_file" ] && ordinal=$(cat "$ordinal_file")
  want=$((ordinal + 1))
  tmp="$ordinal_file.$$"
  printf '%s\n' "$want" > "$tmp"
  mv -f "$tmp" "$ordinal_file"
  record_args "$@" > "$trace/$record"
  flock -u 8
  exec 8>&-
}

if printf %s "$remote" | cmp -s "$PROVIDER_RUNTIME_PROBE_COMMAND" -; then
  probe_count=1
  if [ -f "$trace/ssh.probe.count" ]; then
    [ -f "$trace/ssh.ordinal" ] && [ "$(cat "$trace/ssh.ordinal")" -gt 0 ] || { printf 'fixture repeated probe before first transition\n' >&2; exit 64; }
    probe_count=$(( $(cat "$trace/ssh.probe.count") + 1 ))
  else
    [ ! -f "$trace/ssh.ordinal" ] || [ "$(cat "$trace/ssh.ordinal")" -eq 0 ] || { printf 'fixture probe must be first\n' >&2; exit 64; }
  fi
  printf '%s\n' "$probe_count" > "$trace/ssh.probe.count"
  publish "ssh.probe.$probe_count.args" "$@"
  arch=${PROVIDER_RUNTIME_ARCH:-amd64}
  case "$arch" in amd64|arm64) ;; *) printf 'fixture unsupported probe architecture\n' >&2; exit 64 ;; esac
  profile_digest=${PROVIDER_RUNTIME_PROFILE_DIGEST:?}
  [ ${#profile_digest} -eq 64 ] || { printf 'fixture invalid profile digest\n' >&2; exit 64; }
  case "$profile_digest" in *[!0123456789abcdef]*) printf 'fixture invalid profile digest\n' >&2; exit 64 ;; esac
  release=${PROVIDER_RUNTIME_RELEASE:?}
  [ -n "$release" ] || { printf 'fixture invalid release identity\n' >&2; exit 64; }
  printf 's2p2:Linux\n%s\nmid1:0911601b3b0a5f6fdc51f3661518ee20e26ea0cbadfb4f7283e5b1f288941f54\n%s\n%s\n' "$arch" "$profile_digest" "$release"
  exit 0
fi
if printf %s "$remote" | cmp -s "$PROVIDER_RUNTIME_HOST_COMMAND" -; then
  [ -f "$trace/ssh.ordinal" ] && [ "$(cat "$trace/ssh.ordinal")" -ge 1 ] || { printf 'fixture Host before probe\n' >&2; exit 64; }
  host_count=1
  [ -f "$trace/ssh.host.count" ] && host_count=$(( $(cat "$trace/ssh.host.count") + 1 ))
  printf '%s\n' "$host_count" > "$trace/ssh.host.count"
  publish "ssh.host.$host_count.args" "$@"
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
printf 'fixture remote command mismatch\n' >&2
exit 64
