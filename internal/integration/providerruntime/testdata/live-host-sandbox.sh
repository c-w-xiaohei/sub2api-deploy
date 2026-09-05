#!/bin/sh
# Runs inside one test-owned Host network namespace and creates that Host's
# private mount namespace. No path below /usr/local or /var/lib is touched on
# the runner mount namespace.
set -eu
docker_failure_reason() {
  log=${1:?}
  fallback=${2:?}
  if [ "$fallback" = timeout ] || [ ! -s "$log" ]; then
    printf '%s' "$fallback"
  elif grep -Eqi 'permission denied|operation not permitted' "$log"; then
    printf '%s' permission
  elif grep -Eqi 'no space left|cannot allocate memory|out of memory|resource temporarily unavailable|too many open files' "$log"; then
    printf '%s' resource
  elif grep -Eqi 'already running|address already in use|resource busy|pid file|specified both as a flag and in the configuration file' "$log"; then
    printf '%s' conflict
  elif grep -Eqi 'network controller|iptables|ip6tables|bridge driver' "$log"; then
    printf '%s' network
  elif grep -Eqi 'failed.*(storage driver|graphdriver|overlay|volume store|filesystem)|error (initializing|mounting).*(graphdriver|overlay|filesystem)' "$log"; then
    printf '%s' storage
  elif grep -Eqi '(failed|error|unable|invalid|not found|not mounted).*(cgroup|cgroups)|(cgroup|cgroups).*(failed|error|unable|invalid|not found|not mounted)' "$log"; then
    printf '%s' cgroup
  elif grep -Eqi 'docker-proxy|docker-init|executable file not found|not found in.*PATH' "$log"; then
    printf '%s' helper
  elif grep -Eqi 'invalid configuration|configuration.*(failed|error|invalid)|failed to decode.*config' "$log"; then
    printf '%s' config
  elif grep -Eqi 'daemon root|data root|exec root|state directory|mkdir|read-only file system' "$log"; then
    printf '%s' filesystem
  elif grep -Eqi '(containerd|unix|socket).*(file name too long|path too long|invalid argument)|(file name too long|path too long).*(containerd|unix|socket)' "$log"; then
    printf '%s' containerd-path
  elif grep -Eqi '(failed|error).*(containerd).*(timeout|timed out)|(failed|error).*(timeout|timed out).*(containerd)|(containerd).*(timeout|timed out).*(failed|error)' "$log"; then
    printf '%s' containerd-timeout
  elif grep -Eqi '(containerd).*(connection refused|no such file|unavailable)|(connection refused|no such file|unavailable).*(containerd)' "$log"; then
    printf '%s' containerd-socket
  elif grep -Eqi '(containerd).*(exited|exit status|killed|terminated)|(exited|exit status|killed|terminated).*(containerd)' "$log"; then
    printf '%s' containerd-exit
  elif grep -Eqi '(failed|error).*(containerd)|(containerd).*(failed|error)' "$log"; then
    printf '%s' containerd
  elif grep -Eqi 'failed to start daemon|error initializing' "$log"; then
    printf '%s' initialization
  else
    printf '%s' "$fallback"
  fi
}
containerd_startup_reason() {
  log=${1:?}
  if grep -Eqi 'successfully booted' "$log"; then
    printf '%s' containerd-booted
  elif grep -Eqi 'permission denied|operation not permitted' "$log"; then
    printf '%s' containerd-permission
  elif grep -Eqi 'no space left|cannot allocate memory|out of memory|resource temporarily unavailable|too many open files' "$log"; then
    printf '%s' containerd-resource
  elif grep -Eqi 'failed to get listener|failed to serve|listen unix|address already in use' "$log"; then
    printf '%s' containerd-listener
  elif grep -Eqi '(failed to load|loading) plugin' "$log"; then
    printf '%s' containerd-plugin
  elif grep -Eqi 'starting containerd' "$log"; then
    printf '%s' containerd-startup
  else
    printf '%s' containerd-timeout
  fi
}
docker_cli() {
  docker -H unix:///var/run/docker.sock "$@"
}
ctr_cli() {
  timeout --signal=TERM --kill-after=1s 6s ctr --address /var/run/sub2api-containerd/containerd.sock --namespace "sub2api-$name" --timeout 5s --connect-timeout 2s "$@"
}
ctr_cleanup() {
  timeout --signal=TERM --kill-after=1s 3s ctr --address /var/run/sub2api-containerd/containerd.sock --namespace "sub2api-$name" --timeout 2s --connect-timeout 1s "$@"
}
if [ "${1:-}" = --classify-docker-log ]; then
  docker_failure_reason "${2:?}" "${3:?}"
  exit 0
fi
if [ "${1:-}" = --classify-containerd-log ]; then
  containerd_startup_reason "${2:?}"
  exit 0
fi
name=${1:?}
root=${LIVE_ROOT:?}
host=${LIVE_HOST_BINARY:?}
images=${LIVE_IMAGE_ARCHIVE:?}
log="$root/$name.private.log"
containerd_log="$root/$name.containerd.private.log"
stage=mount-setup
containerd=
dockerd=
sshd=
cleanup_failed=0
shutdown_requested=0
process_alive() {
  pid=$1
  kill -0 "$pid" 2>/dev/null || return 1
  awk '{ exit ($3 == "Z") ? 1 : 0 }' "/proc/$pid/stat" 2>/dev/null
}
stop_group() {
  pid=$1
  [ -n "$pid" ] || return 0
  kill -TERM "-$pid" 2>/dev/null || true
  i=0
  while process_alive "$pid" && [ "$i" -lt 8 ]; do
    sleep 1
    i=$((i + 1))
  done
  if process_alive "$pid"; then
    kill -KILL "-$pid" 2>/dev/null || true
    sleep 1
  fi
  process_alive "$pid" && return 1
  wait "$pid" 2>/dev/null || true
}
remove_all_docker_containers() {
  kill -0 "$dockerd" 2>/dev/null || return 0
  ids=$(timeout --signal=TERM --kill-after=1s 4s docker -H unix:///var/run/docker.sock ps -aq) || return 1
  if [ -n "$ids" ]; then
    timeout --signal=TERM --kill-after=1s 12s docker -H unix:///var/run/docker.sock rm -f $ids >/dev/null 2>&1 || return 1
  fi
  [ -z "$(timeout --signal=TERM --kill-after=1s 4s docker -H unix:///var/run/docker.sock ps -aq)" ]
}
wait_for_no_containerd_tasks() {
  [ -n "$containerd" ] || return 0
  i=0
  while [ "$i" -lt 5 ]; do
    tasks=$(ctr_cleanup tasks ls -q 2>/dev/null) || return 1
    [ -z "$tasks" ] && return 0
    i=$((i + 1))
    sleep 1
  done
  return 1
}
host_shims_alive() {
  ps -eo args= | awk -v ns="sub2api-$name" -v socket=/var/run/sub2api-containerd/containerd.sock '
    /containerd-shim/ && index($0, "-namespace " ns) && index($0, "-address " socket) { found = 1 }
    END { exit found ? 0 : 1 }
  '
}
wait_for_no_host_shims() {
  [ -n "$containerd" ] || return 0
  i=0
  while host_shims_alive; do
    i=$((i + 1))
    [ "$i" -lt 5 ] || return 1
    sleep 1
  done
}
cleanup() {
  status=$?
  trap - EXIT INT TERM
  [ "$shutdown_requested" -eq 0 ] || status=0
  if [ "$status" -ne 0 ] && [ "$stage" != running ]; then
    printf '%s\n' "SUB2API_LIVE_STAGE=$name-$stage" >&2
  fi
  remove_all_docker_containers || cleanup_failed=1
  wait_for_no_containerd_tasks || cleanup_failed=1
  wait_for_no_host_shims || cleanup_failed=1
  stop_group "$dockerd" || cleanup_failed=1
  stop_group "$containerd" || cleanup_failed=1
  stop_group "$sshd" || cleanup_failed=1
  [ "$cleanup_failed" -eq 0 ] || status=1
  exit "$status"
}
on_signal() {
  shutdown_requested=1
  exit 0
}
trap cleanup EXIT
trap on_signal INT TERM
mkdir -p "$root/$name" "$root/$name/containerd" "$root/$name/docker" "$root/$name/etc-containerd/conf.d" "$root/$name/mount"
test -d /etc/containerd
mount --bind "$root/$name.machine-id" /etc/machine-id
mount --bind "$root/$name/etc-containerd" /etc/containerd
mount -t tmpfs -o mode=0755,size=32m tmpfs /usr/local
mount -t tmpfs -o mode=0700,size=256m tmpfs /var/lib
mount -t tmpfs -o mode=0755,size=32m tmpfs /var/run
mkdir -p /usr/local/libexec /var/lib/sub2api-host /var/run/sshd
printf '%s %s\n' "$$" "$(awk '{print $22}' /proc/$$/stat)" >"$root/$name/supervisor"
printf '%s\n' '{}' >"$root/$name/daemon.json"
cat >"$root/$name/containerd.toml" <<'EOF'
version = 3
imports = []
disabled_plugins = ["io.containerd.grpc.v1.cri", "io.containerd.cri.v1", "io.containerd.cri.v1.images", "io.containerd.cri.v1.runtime", "io.containerd.podsandbox.controller.v1.podsandbox"]
EOF
stage=docker-containerd
setsid containerd --config "$root/$name/containerd.toml" --root "$root/$name/containerd" --state /var/run/sub2api-containerd --address /var/run/sub2api-containerd/containerd.sock >"$containerd_log" 2>&1 &
containerd=$!
i=0
until [ -S /var/run/sub2api-containerd/containerd.sock ]; do
  if ! process_alive "$containerd"; then
    stage=docker-containerd-exit
    exit 1
  fi
  i=$((i + 1))
  if [ "$i" -ge 45 ]; then
    stage=docker-containerd-socket
    exit 1
  fi
  sleep 1
done
stage=docker-start
setsid dockerd --config-file "$root/$name/daemon.json" --storage-driver vfs --data-root "$root/$name/docker" --exec-root /var/run/sub2api-docker --pidfile "$root/$name/dockerd.pid" --host unix:///var/run/docker.sock --containerd /var/run/sub2api-containerd/containerd.sock --containerd-namespace "sub2api-$name" --containerd-plugins-namespace "plugins.sub2api-$name" --iptables=true --ip-forward=true --ip-masq=true --icc=false >"$log" 2>&1 &
dockerd=$!
i=0
until docker_cli info >/dev/null 2>&1; do
  if ! kill -0 "$dockerd" 2>/dev/null; then
    reason=$(docker_failure_reason "$log" unknown)
    if [ "$reason" = containerd-timeout ]; then
      reason=$(containerd_startup_reason "$containerd_log")
    fi
    stage="docker-$reason"
    exit 1
  fi
  i=$((i + 1))
  if [ "$i" -ge 45 ]; then
    stage=docker-timeout
    exit 1
  fi
  sleep 1
done
stage=image-load
docker_cli load --input "$images" >/dev/null 2>&1
docker_cli image inspect postgres:18-alpine redis:8-alpine sub2api-live-app:mx-allowlist >/dev/null 2>&1
stage=sshd-start
setsid /usr/sbin/sshd -D -e -f "$root/$name/sshd_config" >>"$log" 2>&1 &
sshd=$!
i=0
until kill -0 "$sshd" 2>/dev/null; do
  i=$((i + 1))
  [ "$i" -lt 15 ] || exit 1
  sleep 1
done
sleep 1
kill -0 "$sshd" 2>/dev/null
stage=running
touch "$root/$name.ready"
wait "$sshd"
