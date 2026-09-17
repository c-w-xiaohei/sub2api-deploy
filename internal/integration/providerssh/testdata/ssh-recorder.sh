#!/bin/sh
set -eu

trace_dir=$SSH_TRACE_DIR
counter=$trace_dir/count
mkdir -p "$trace_dir"
call=0
if [ -f "$counter" ]; then
  call=$(cat "$counter")
fi
call=$((call + 1))
printf '%s\n' "$call" > "$counter"

identity_tmp=$trace_dir/call-$call.identity.tmp.$$
mkdir "$identity_tmp"
printf '%s\n' "$$" > "$identity_tmp/pid"
awk '{print $22}' /proc/$$/stat > "$identity_tmp/start"
awk '{print $5}' /proc/$$/stat > "$identity_tmp/pgid"
mv "$identity_tmp/start" "$trace_dir/call-$call.start"
mv "$identity_tmp/pgid" "$trace_dir/call-$call.pgid"
mv "$identity_tmp/pid" "$trace_dir/call-$call.pid"
rmdir "$identity_tmp"

args_file=$trace_dir/call-$call.args
stdin_file=$trace_dir/call-$call.stdin
started_file=$trace_dir/call-$call.started
: > "$args_file"
for arg do
  printf '%s\n' "$arg" >> "$args_file"
done
printf started > "$started_file"
cat > "$stdin_file"

after_terminator=0
remote=
for arg do
  if [ "$after_terminator" -eq 1 ]; then
    after_terminator=2
  elif [ "$after_terminator" -eq 2 ]; then
    remote=$arg
    break
  elif [ "$arg" = -- ]; then
    after_terminator=1
  fi
done
[ -n "$remote" ] || { printf 'missing fixed remote command\n' >&2; exit 64; }

mode=normal
if [ -f "$SSH_MODE_FILE" ]; then
  mode=$(cat "$SSH_MODE_FILE")
fi
if [ "$mode" = cancel ]; then
  # Keep a descendant alive so cancellation evidence covers the whole SSH group.
  sleep 30 &
	child=$!
	child_identity_tmp=$trace_dir/call-$call.child.identity.tmp.$$
	mkdir "$child_identity_tmp"
	printf '%s\n' "$child" > "$child_identity_tmp/pid"
	awk '{print $22}' "/proc/$child/stat" > "$child_identity_tmp/start"
	awk '{print $5}' "/proc/$child/stat" > "$child_identity_tmp/pgid"
	mv "$child_identity_tmp/start" "$trace_dir/call-$call.child.start"
	mv "$child_identity_tmp/pgid" "$trace_dir/call-$call.child.pgid"
	mv "$child_identity_tmp/pid" "$trace_dir/call-$call.child.pid"
	rmdir "$child_identity_tmp"
	ready_tmp=$trace_dir/call-$call.ready.tmp.$$
	: > "$ready_tmp"
	mv "$ready_tmp" "$trace_dir/call-$call.ready"
	while :; do
    sleep 1
  done
fi
if [ "$mode" = host-key ]; then
  diagnostic=
  previous=
  for arg do
    if [ "$previous" = -E ]; then
      diagnostic=$arg
      break
    fi
    previous=$arg
  done
  [ -n "$diagnostic" ]
  printf 'Host key verification failed\n' > "$diagnostic"
  exit 255
fi

if [ "$remote" = "sudo -n -- /nix/var/nix/profiles/sub2api-host/bin/sub2api-host probe" ]; then
  printf 's2p2:Linux\namd64\nmid1:0911601b3b0a5f6fdc51f3661518ee20e26ea0cbadfb4f7283e5b1f288941f54\n%s\n%s\n' \
    'bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb' \
    'release@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'
  exit 0
fi
[ "$remote" = "sudo -n -- /nix/var/nix/profiles/sub2api-host/bin/sub2api-host stdio" ] || {
  printf 'unexpected fixed remote command\n' >&2
  exit 64
}

ready_tmp=$trace_dir/call-$call.ready.tmp.$$
: > "$ready_tmp"
mv "$ready_tmp" "$trace_dir/call-$call.ready"

atomic_copy() {
  source=$1
  destination=$2
  temporary=$destination.tmp.$$
  cp "$source" "$temporary"
  mv "$temporary" "$destination"
}

create_exclusive() {
  destination=$1
  value=$2
  (set -C; printf '%s\n' "$value" > "$destination") 2>/dev/null
}

create_exclusive_empty() {
  destination=$1
  (set -C; : > "$destination") 2>/dev/null
}

write_pending_state() {
  temporary=$trace_dir/state.pending.tmp.$$
  if [ -e "$trace_dir/state.pending" ] || ! mkdir "$temporary" 2>/dev/null; then
    return 1
  fi
  cp "$SSH_EXPECTED_OPERATION_KEY" "$temporary/key"
  cp "$SSH_EXPECTED_PENDING_EVIDENCE" "$temporary/evidence"
  mv "$temporary" "$trace_dir/state.pending"
}

write_complete_state() {
  temporary=$trace_dir/state.complete.tmp.$$
  if [ -e "$trace_dir/state.complete" ] || ! mkdir "$temporary" 2>/dev/null; then
    return 1
  fi
  cp "$SSH_EXPECTED_OPERATION_KEY" "$temporary/key"
  cp "$SSH_EXPECTED_COMPLETE_EVIDENCE" "$temporary/evidence"
  mv "$temporary" "$trace_dir/state.complete"
}

conflict_response() {
  cat "$SSH_RESPONSE_DIR/response-conflict"
  exit 0
}

if cmp -s "$stdin_file" "$SSH_EXPECTED_RECONCILE_FRAME"; then
  if [ -e "$trace_dir/state.complete" ]; then
    cmp -s "$trace_dir/state.complete/key" "$SSH_EXPECTED_OPERATION_KEY" || conflict_response
	cmp -s "$stdin_file" "$SSH_EXPECTED_RECONCILE_FRAME" || conflict_response
    cat "$SSH_RESPONSE_DIR/response-applied"
    exit 0
  fi
  if [ -e "$trace_dir/state.pending" ]; then
    cmp -s "$trace_dir/state.pending/key" "$SSH_EXPECTED_OPERATION_KEY" || conflict_response
    write_complete_state || conflict_response
    rm -rf "$trace_dir/state.pending"
    cat "$SSH_RESPONSE_DIR/response-applied"
    exit 0
  fi
  write_pending_state || conflict_response
  atomic_copy "$SSH_EXPECTED_OPERATION_KEY" "$trace_dir/operation-key-evidence"
  create_exclusive "$trace_dir/effect-marker" "reconcile-effect" || conflict_response
  create_exclusive_empty "$trace_dir/effect.log" || conflict_response
  cat "$SSH_EXPECTED_OPERATION_KEY" >> "$trace_dir/effect.log"
  printf '\n' >> "$trace_dir/effect.log"
  if [ "${SSH_DROP_RESPONSE_CALL:-}" = "$call" ]; then
    exit 0
  fi
  exit 0
fi

if cmp -s "$stdin_file" "$SSH_EXPECTED_INSPECT_FRAME"; then
  if [ -e "$trace_dir/state.complete" ]; then
    cmp -s "$trace_dir/state.complete/key" "$SSH_EXPECTED_OPERATION_KEY" || conflict_response
    cat "$SSH_RESPONSE_DIR/response-complete"
    exit 0
  fi
  if [ -e "$trace_dir/state.pending" ]; then
    cmp -s "$trace_dir/state.pending/key" "$SSH_EXPECTED_OPERATION_KEY" || conflict_response
    cat "$SSH_RESPONSE_DIR/response-pending"
    exit 0
  fi
  cat "$SSH_RESPONSE_DIR/response-1"
  exit 0
fi

conflict_response
