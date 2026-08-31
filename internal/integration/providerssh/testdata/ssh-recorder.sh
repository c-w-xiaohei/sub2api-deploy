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
printf '%s\n' "$$" > "$trace_dir/call-$call.pid"
awk '{print $22}' /proc/$$/stat > "$trace_dir/call-$call.start"
awk '{print $5}' /proc/$$/stat > "$trace_dir/call-$call.pgid"

args_file=$trace_dir/call-$call.args
stdin_file=$trace_dir/call-$call.stdin
started_file=$trace_dir/call-$call.started
: > "$args_file"
for arg do
  printf '%s\n' "$arg" >> "$args_file"
done
printf started > "$started_file"
cat > "$stdin_file"

mode=normal
if [ -f "$SSH_MODE_FILE" ]; then
  mode=$(cat "$SSH_MODE_FILE")
fi
if [ "$mode" = cancel ]; then
	# Keep a descendant alive so cancellation evidence covers the whole SSH group.
	sleep 30 &
	child=$!
	printf '%s\n' "$child" > "$trace_dir/call-$call.child.pid"
	awk '{print $22}' "/proc/$child/stat" > "$trace_dir/call-$call.child.start"
	awk '{print $5}' "/proc/$child/stat" > "$trace_dir/call-$call.child.pgid"
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
if [ "${SSH_DROP_RESPONSE_CALL:-}" = "$call" ]; then
  exit 0
fi
cat "$SSH_RESPONSE_DIR/response-$call"
