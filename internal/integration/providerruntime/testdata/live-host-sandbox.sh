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
  elif grep -Eqi 'failed to start daemon:.*(cgroup|cgroups)|devices cgroup (isn.t|is not) mounted' "$log"; then
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
docker_cli() {
  docker -H unix:///var/run/docker.sock "$@"
}
if [ "${1:-}" = --classify-docker-log ]; then
  docker_failure_reason "${2:?}" "${3:?}"
  exit 0
fi
name=${1:?}
root=${LIVE_ROOT:?}
host=${LIVE_HOST_BINARY:?}
images=${LIVE_IMAGE_ARCHIVE:?}
log="$root/$name.private.log"
stage=mount-setup
dockerd=
sshd=
cleanup_failed=0
shutdown_requested=0
host_mount_namespace=$(readlink "/proc/$$/ns/mnt")
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
host_runtime_identity_matches() {
  pid=$1
  mount_namespace=$(readlink "/proc/$pid/ns/mnt" 2>/dev/null) || return 1
  [ "$mount_namespace" = "$host_mount_namespace" ] || return 1
  comm=$(cat "/proc/$pid/comm" 2>/dev/null) || return 1
  case "$comm" in
    containerd|containerd-shim*) ;;
    *) return 1 ;;
  esac
}
host_runtime_pid() {
  pid=$1
  case "$pid" in
    *[!0-9]*|'') return 1 ;;
  esac
  [ -r "/proc/$pid/comm" ] || return 1
  host_runtime_identity_matches "$pid" || return 1
  # Revalidate before callers act in case the process exited and its PID was reused.
  host_runtime_identity_matches "$pid"
}
host_runtime_alive() {
  for path in /proc/[0-9]*; do
    pid=${path#/proc/}
    host_runtime_pid "$pid" && return 0
  done
  return 1
}
wait_for_host_runtime_exit() {
  i=0
  while host_runtime_alive; do
    [ "$i" -lt 4 ] || return 1
    i=$((i + 1))
    sleep 1
  done
}
signal_host_runtime() {
  signal=$1
  for path in /proc/[0-9]*; do
    pid=${path#/proc/}
    host_runtime_pid "$pid" || continue
    kill "-$signal" "$pid" 2>/dev/null || true
  done
}
cleanup_host_runtime() {
  wait_for_host_runtime_exit && return 0
  signal_host_runtime TERM
  wait_for_host_runtime_exit && return 0
  signal_host_runtime KILL
  wait_for_host_runtime_exit
}
cleanup() {
  status=$?
  trap - EXIT INT TERM
  [ "$shutdown_requested" -eq 0 ] || status=0
  if [ "$status" -ne 0 ] && [ "$stage" != running ]; then
    printf '%s\n' "SUB2API_LIVE_STAGE=$name-$stage" >&2
  fi
  remove_all_docker_containers || cleanup_failed=1
  stop_group "$dockerd" || cleanup_failed=1
  cleanup_host_runtime || cleanup_failed=1
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
mkdir -p "$root/$name" "$root/$name/docker" "$root/$name/mount"
printf '%s\n' 'root:x:20000:0:99999:7:::' >"$root/$name.shadow"
chmod 0600 "$root/$name.shadow"
mount --bind "$root/$name.machine-id" /etc/machine-id
mount --bind "$root/$name.shadow" /etc/shadow
mount --bind "$root/cgroup-host" /sys/fs/cgroup
mount -t tmpfs -o mode=0755,size=32m tmpfs /usr/local
mount -t tmpfs -o mode=0700,size=256m tmpfs /var/lib
mount -t tmpfs -o mode=0755,size=32m tmpfs /var/run
mkdir -p /usr/local/libexec /var/run/sshd /var/run/sub2api-runtime
chmod 0700 /var/run/sub2api-runtime
printf '%s %s\n' "$$" "$(awk '{print $22}' /proc/$$/stat)" >"$root/$name/supervisor"
printf '%s\n' '{}' >"$root/$name/daemon.json"
stage=docker-start
XDG_RUNTIME_DIR=/var/run/sub2api-runtime setsid dockerd --config-file "$root/$name/daemon.json" --storage-driver vfs --data-root "$root/$name/docker" --exec-root /var/run/sub2api-docker --pidfile "$root/$name/dockerd.pid" --host unix:///var/run/docker.sock --iptables=true --ip-forward=true --ip-masq=true --icc=false >"$log" 2>&1 &
dockerd=$!
i=0
until docker_cli info >/dev/null 2>&1; do
  if ! kill -0 "$dockerd" 2>/dev/null; then
    reason=$(docker_failure_reason "$log" unknown)
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
