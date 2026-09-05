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
  elif grep -Eqi '(containerd|unix|socket).*(file name too long|path too long|invalid argument)|(file name too long|path too long).*(containerd|unix|socket)' "$log"; then
    printf '%s' containerd-path
  elif grep -Eqi '(containerd).*(timeout|timed out)|(timeout|timed out).*(containerd)' "$log"; then
    printf '%s' containerd-timeout
  elif grep -Eqi '(containerd).*(connection refused|no such file|unavailable)|(connection refused|no such file|unavailable).*(containerd)' "$log"; then
    printf '%s' containerd-socket
  elif grep -Eqi '(containerd).*(exited|exit status|killed|terminated)|(exited|exit status|killed|terminated).*(containerd)' "$log"; then
    printf '%s' containerd-exit
  elif grep -Eqi '(failed|error|timeout|timed out).*(containerd)|(containerd).*(failed|error|timeout|timed out)' "$log"; then
    printf '%s' containerd
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
cleanup() {
  status=$?
  if [ "$status" -ne 0 ] && [ "$stage" != running ]; then
    printf '%s\n' "SUB2API_LIVE_STAGE=$name-$stage" >&2
  fi
  for pid in "$sshd" "$dockerd"; do
    [ -n "$pid" ] || continue
    kill -TERM "-$pid" 2>/dev/null || true
    i=0
    while kill -0 "$pid" 2>/dev/null && [ "$i" -lt 15 ]; do
      sleep 1
      i=$((i + 1))
    done
    kill -KILL "-$pid" 2>/dev/null || true
    wait "$pid" 2>/dev/null || true
  done
  trap - EXIT INT TERM
  exit "$status"
}
trap cleanup EXIT INT TERM
mkdir -p "$root/$name" "$root/$name/docker" "$root/$name/mount"
mount --bind "$root/$name.machine-id" /etc/machine-id
mount -t tmpfs -o mode=0755,size=32m tmpfs /usr/local
mount -t tmpfs -o mode=0700,size=256m tmpfs /var/lib
mount -t tmpfs -o mode=0755,size=32m tmpfs /var/run
mkdir -p /usr/local/libexec /var/lib/sub2api-host /var/run/sshd
printf '%s %s\n' "$$" "$(awk '{print $22}' /proc/$$/stat)" >"$root/$name/supervisor"
printf '%s\n' '{}' >"$root/$name/daemon.json"
stage=docker-start
setsid dockerd --config-file "$root/$name/daemon.json" --storage-driver vfs --data-root "$root/$name/docker" --exec-root /var/run/sub2api-docker --pidfile "$root/$name/dockerd.pid" --host unix:///var/run/docker.sock --iptables=true --ip-forward=true --ip-masq=true --icc=false >"$log" 2>&1 &
dockerd=$!
i=0
until docker_cli info >/dev/null 2>&1; do
  if ! kill -0 "$dockerd" 2>/dev/null; then
    stage="docker-$(docker_failure_reason "$log" unknown)"
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
