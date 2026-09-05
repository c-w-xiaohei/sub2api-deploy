#!/bin/sh
# CI-only outer launcher. It owns a bridge and distinct data/App/unauthorized
# namespaces. Each Host starts a separate private mount namespace and Docker.
set -eu

root=${LIVE_ROOT:?}
images=${LIVE_IMAGE_ARCHIVE:?}
test_binary=${1:?}
shift
data_pid=
app_pid=
cleanup_failed=0
process_alive() {
  pid=$1
  kill -0 "$pid" 2>/dev/null || return 1
  awk '{ exit ($3 == "Z") ? 1 : 0 }' "/proc/$pid/stat" 2>/dev/null
}

signal_group() {
  pid=$1
  [ -n "$pid" ] || return 0
  kill -TERM "-$pid" 2>/dev/null || true
}
wait_group() {
  pid=$1
  [ -n "$pid" ] || return 0
  i=0
  while process_alive "$pid" && [ "$i" -lt 90 ]; do
    sleep 1
    i=$((i + 1))
  done
  if process_alive "$pid"; then
    kill -KILL "-$pid" 2>/dev/null || true
    sleep 1
  fi
  if process_alive "$pid"; then
    cleanup_failed=1
  fi
  if ! process_alive "$pid"; then
    wait "$pid" 2>/dev/null || cleanup_failed=1
  fi
}
wait_ready() {
  name=$1
  pid=$2
  peer_pid=${3:-}
  i=0
  until [ -f "$root/$name.ready" ]; do
    kill -0 "$pid" 2>/dev/null || exit 1
    [ -z "$peer_pid" ] || kill -0 "$peer_pid" 2>/dev/null || exit 1
    i=$((i + 1))
    [ "$i" -lt 120 ] || exit 1
    sleep 1
  done
}
cleanup() {
  status=$?
  signal_group "$data_pid"
  signal_group "$app_pid"
  wait_group "$data_pid"
  wait_group "$app_pid"
  umount "$root/cgroup-host" 2>/dev/null || cleanup_failed=1
  ip netns del "${LIVE_DATA_NS:?}" 2>/dev/null || true
  ip netns del "${LIVE_APP_NS:?}" 2>/dev/null || true
  ip netns del "${LIVE_UNAUTHORIZED_NS:?}" 2>/dev/null || true
  ip link del "${LIVE_BRIDGE:?}" 2>/dev/null || true
  trap - EXIT INT TERM
  [ "$cleanup_failed" -eq 0 ] || exit 1
  exit "$status"
}
trap cleanup EXIT INT TERM

for binary in bash dockerd docker sshd ssh sudo nft ip psql redis-cli openssl unshare setsid; do
  command -v "$binary" >/dev/null 2>&1 || exit 1
done

mkdir -p "$root/cgroup-host"
mount --bind /sys/fs/cgroup "$root/cgroup-host"
printf '%s\n' 'SUB2API_LIVE_STAGE=network-setup' >&2
ip link set lo up
ip link add "${LIVE_BRIDGE:?}" type bridge
ip addr add 10.252.0.1/24 dev "$LIVE_BRIDGE"
ip link set "$LIVE_BRIDGE" up
ip netns add "${LIVE_DATA_NS:?}"
ip netns add "${LIVE_APP_NS:?}"
ip netns add "${LIVE_UNAUTHORIZED_NS:?}"
ip link add "${LIVE_DATA_VETH_OUT:?}" type veth peer name "${LIVE_DATA_VETH_IN:?}"
ip link set "$LIVE_DATA_VETH_IN" netns "$LIVE_DATA_NS"
ip link set "$LIVE_DATA_VETH_OUT" master "$LIVE_BRIDGE"
ip link set "$LIVE_DATA_VETH_OUT" up
ip -n "$LIVE_DATA_NS" addr add "$LIVE_DATA_IP/24" dev "$LIVE_DATA_VETH_IN"
ip -n "$LIVE_DATA_NS" link set lo up
ip -n "$LIVE_DATA_NS" link set "$LIVE_DATA_VETH_IN" up
ip link add "${LIVE_APP_VETH_OUT:?}" type veth peer name "${LIVE_APP_VETH_IN:?}"
ip link set "$LIVE_APP_VETH_IN" netns "$LIVE_APP_NS"
ip link set "$LIVE_APP_VETH_OUT" master "$LIVE_BRIDGE"
ip link set "$LIVE_APP_VETH_OUT" up
ip -n "$LIVE_APP_NS" addr add "$LIVE_APP_IP/24" dev "$LIVE_APP_VETH_IN"
ip -n "$LIVE_APP_NS" link set lo up
ip -n "$LIVE_APP_NS" link set "$LIVE_APP_VETH_IN" up
ip link add "${LIVE_BAD_VETH_OUT:?}" type veth peer name "${LIVE_BAD_VETH_IN:?}"
ip link set "$LIVE_BAD_VETH_IN" netns "$LIVE_UNAUTHORIZED_NS"
ip link set "$LIVE_BAD_VETH_OUT" master "$LIVE_BRIDGE"
ip link set "$LIVE_BAD_VETH_OUT" up
ip -n "$LIVE_UNAUTHORIZED_NS" addr add "$LIVE_BAD_IP/24" dev "$LIVE_BAD_VETH_IN"
ip -n "$LIVE_UNAUTHORIZED_NS" link set lo up
ip -n "$LIVE_UNAUTHORIZED_NS" link set "$LIVE_BAD_VETH_IN" up
printf '%s\n' 'SUB2API_LIVE_STAGE=sandbox-start' >&2
setsid ip netns exec "$LIVE_DATA_NS" unshare --mount --propagation private "$root/host-sandbox.sh" data &
data_pid=$!
wait_ready data "$data_pid"
setsid ip netns exec "$LIVE_APP_NS" unshare --mount --propagation private "$root/host-sandbox.sh" app &
app_pid=$!
wait_ready app "$app_pid" "$data_pid"
kill -0 "$data_pid" 2>/dev/null
kill -0 "$app_pid" 2>/dev/null

printf '%s\n' 'SUB2API_LIVE_STAGE=sandboxes-ready' >&2
"$test_binary" "$@"
status=$?
exit "$status"
