#!/usr/bin/env bash
set -euo pipefail

IMAGE='gmwallet/epusdt:v2.0.0'
REPOSITORY='gmwallet/epusdt'
EVIDENCE_SELECTOR='gmpay-image-contract-v1'
EXPECTED_ARCH="${EXPECTED_ARCH:?EXPECTED_ARCH must be amd64 or arm64}"
EVIDENCE_FILE="${EVIDENCE_FILE:-}"
READINESS_TIMEOUT_SECONDS=90
READINESS_POLL_SECONDS=2
PROBE_TIMEOUT_SECONDS=5
phase=initialization
root="$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)"
manifest_helper="$root/scripts/tests/gmpay-image-manifest.py"

case "$EXPECTED_ARCH" in
  amd64|arm64) ;;
  *) printf '%s\n' 'unsupported expected architecture' >&2; exit 2 ;;
esac

case "$(uname -m)" in
  x86_64) runner_arch=amd64 ;;
  aarch64|arm64) runner_arch=arm64 ;;
  *) printf '%s\n' 'unsupported runner architecture' >&2; exit 2 ;;
esac
test "$runner_arch" = "$EXPECTED_ARCH"

temporary_root="${RUNNER_TEMP:-${TMPDIR:-/tmp}}"
data_dir="$(mktemp -d "$temporary_root/gmpay-image-contract.XXXXXX")"
evidence_dir="$(mktemp -d "$temporary_root/gmpay-image-evidence.XXXXXX")"
container="gmpay-image-contract-${EXPECTED_ARCH}-$$-${RANDOM}"
TEST_LABEL='gmpay-image-contract=true'
failure_log="$evidence_dir/failure.log"
if test -n "$EVIDENCE_FILE"; then
  evidence_parent="$(dirname "$EVIDENCE_FILE")"
  test -d "$evidence_parent" || mkdir -p "$evidence_parent"
  failure_log="${EVIDENCE_FILE}.failure.log"
fi
: > "$failure_log"
chmod 0600 "$failure_log"

capture_failure_logs() {
  if docker container inspect "$container" >/dev/null 2>&1; then
    docker logs --tail 200 "$container" 2>&1 \
      | sed -E \
          -e "s/((authorization|token|password|secret|api[_-]?key|private[_-]?key)[[:space:]\"']*[=:][[:space:]\"']*).*/\\1REDACTED/Ig" \
          -e "s/(Bearer[[:space:]]+).*/\\1REDACTED/Ig" \
      > "$evidence_dir/container.log" || true
    chmod 0600 "$evidence_dir/container.log" || true
  fi
}

write_failure_artifact() {
  {
    printf 'phase=%s\nstatus=%s\n' "$phase" "$1"
    shopt -s nullglob
    for stderr in "$evidence_dir"/*.stderr; do
      printf 'stderr_file=%s\n' "$(basename "$stderr")"
      python3 "$manifest_helper" redact < "$stderr"
    done
    if test -s "$evidence_dir/container.log"; then
      printf 'container_log=present\n'
      python3 "$manifest_helper" redact < "$evidence_dir/container.log"
    fi
  } > "$failure_log"
  chmod 0600 "$failure_log"
  test -s "$failure_log"
  printf 'sanitized failure diagnostics: %s\n' "$failure_log" >&2
}

run_capture() {
  phase="$1"
  output="$2"
  shift 2
  "$@" > "$output" 2> "$evidence_dir/$phase.stderr"
}

cleanup() {
  status=$?
  trap - EXIT
  if (( status != 0 )); then
    capture_failure_logs
    write_failure_artifact "$status"
  fi
  docker rm -f "$container" >/dev/null 2>&1 || true
  rm -rf "$data_dir"
  if test -z "$EVIDENCE_FILE"; then
    rm -rf "$evidence_dir"
  fi
  exit "$status"
}
trap cleanup EXIT

phase=tag-manifest-inspect
tag_manifest_raw="$evidence_dir/tag-manifest.json"
run_capture "$phase" "$tag_manifest_raw" timeout 20s docker buildx imagetools inspect --raw "$IMAGE"
phase=tag-manifest-parse
run_capture "$phase" "$evidence_dir/tag-resolution.json" timeout 5s python3 "$manifest_helper" resolve "$tag_manifest_raw" "$EXPECTED_ARCH"
platform_digest="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["platformDigest"])' "$evidence_dir/tag-resolution.json")"
IMMUTABLE_IMAGE="$REPOSITORY@$platform_digest"

phase=immutable-manifest-inspect
immutable_manifest_raw="$evidence_dir/immutable-manifest.json"
run_capture "$phase" "$immutable_manifest_raw" timeout 20s docker buildx imagetools inspect --raw "$IMMUTABLE_IMAGE"
phase=immutable-manifest-parse
run_capture "$phase" "$evidence_dir/immutable-resolution.json" timeout 5s python3 "$manifest_helper" config "$immutable_manifest_raw"
config_digest="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["configDigest"])' "$evidence_dir/immutable-resolution.json")"
[[ "$config_digest" =~ ^sha256:[0-9a-f]{64}$ ]]

phase=pull-immutable-image
run_capture "$phase" "$evidence_dir/pull.stdout" timeout 30s docker pull "$IMMUTABLE_IMAGE"
phase=docker-architecture
run_capture "$phase" "$evidence_dir/docker-info.stdout" docker info --format '{{.Architecture}}'
docker_info_arch="$(<"$evidence_dir/docker-info.stdout")"
case "$docker_info_arch" in
  x86_64|amd64) docker_arch=amd64 ;;
  aarch64|arm64) docker_arch=arm64 ;;
  *) printf '%s\n' 'unsupported Docker architecture' >&2; exit 2 ;;
esac
test "$docker_arch" = "$EXPECTED_ARCH"
phase=image-platform-inspect
run_capture "$phase" "$evidence_dir/image-platform.stdout" docker image inspect --format '{{.Os}}/{{.Architecture}}' "$IMMUTABLE_IMAGE"
image_platform="$(<"$evidence_dir/image-platform.stdout")"
test "$image_platform" = "linux/$EXPECTED_ARCH"
phase=image-id-inspect
run_capture "$phase" "$evidence_dir/image-id.stdout" docker image inspect --format '{{.Id}}' "$IMMUTABLE_IMAGE"
image_id="$(<"$evidence_dir/image-id.stdout")"
phase=repo-digest-inspect
run_capture "$phase" "$evidence_dir/repo-digest.stdout" docker image inspect --format '{{index .RepoDigests 0}}' "$IMMUTABLE_IMAGE"
repo_digest="$(<"$evidence_dir/repo-digest.stdout")"
[[ "$image_id" =~ ^sha256:[0-9a-f]{64}$ ]]
[[ "$repo_digest" =~ ^gmwallet/epusdt@sha256:[0-9a-f]{64}$ ]]
[[ "$image_id" = "$config_digest" ]]

assert_no_running_test_container() {
  test -z "$(docker ps --filter "label=$TEST_LABEL" --format '{{.ID}}')"
}

assert_one_running_test_container() {
  running_ids=()
  mapfile -t running_ids < <(docker ps --filter "label=$TEST_LABEL" --format '{{.ID}}')
  test "${#running_ids[@]}" -eq 1
}

start_container() {
  phase=container-start
  docker run --detach \
    --name "$container" \
    --label "$TEST_LABEL" \
    --restart unless-stopped \
    --network none \
    --env EPUSDT_CONFIG=/data/.env \
    --volume "$data_dir:/data" \
    "$IMMUTABLE_IMAGE" >/dev/null
  phase=container-image-inspect
  test "$(docker inspect --format '{{.Config.Image}}' "$container")" = "$IMMUTABLE_IMAGE"
  assert_one_process
  assert_one_running_test_container
}

assert_one_process() {
  phase=process-count
  process_lines="$(docker top "$container" | wc -l)"
  test "$process_lines" -ge 1
  process_count=$((process_lines - 1))
  test "$process_count" -eq 1
}

assert_container_contract() {
  phase=container-contract
  port_bindings="$(docker inspect --format '{{json .HostConfig.PortBindings}}' "$container")"
  test "$port_bindings" = null
  restart_policy="$(docker inspect --format '{{.HostConfig.RestartPolicy.Name}}' "$container")"
  test "$restart_policy" = unless-stopped
  test "$(docker inspect --format '{{.HostConfig.NetworkMode}}' "$container")" = none
  test "$(docker inspect --format '{{len .Mounts}}' "$container")" -eq 1
  test "$(docker inspect --format '{{(index .Mounts 0).Type}}' "$container")" = bind
  test "$(docker inspect --format '{{(index .Mounts 0).Destination}}' "$container")" = /data
  expected_source="$(realpath -e -- "$data_dir")"
  inspected_source="$(docker inspect --format '{{(index .Mounts 0).Source}}' "$container")"
  inspected_source="$(realpath -e -- "$inspected_source")"
  test "$inspected_source" = "$expected_source"
  env_matches="$(docker inspect --format '{{range .Config.Env}}{{println .}}{{end}}' "$container" | grep -Fx 'EPUSDT_CONFIG=/data/.env' | wc -l)"
  test "$env_matches" -eq 1
}

wait_until_ready() {
  phase=readiness
  READINESS_DEADLINE_SECONDS=$((SECONDS + READINESS_TIMEOUT_SECONDS))
  while (( SECONDS < READINESS_DEADLINE_SECONDS )); do
    if timeout --kill-after=1s "${PROBE_TIMEOUT_SECONDS}s" docker exec "$container" wget -q -O /dev/null http://localhost:8000/; then
      return 0
    fi
    sleep "$READINESS_POLL_SECONDS"
  done
  return 1
}

assert_wget_and_readiness() {
  phase=wget-check
  timeout --kill-after=1s "${PROBE_TIMEOUT_SECONDS}s" docker exec "$container" sh -ceu 'command -v wget >/dev/null'
  wait_until_ready
}

assert_no_running_test_container
start_container
assert_container_contract
assert_wget_and_readiness
assert_one_process

sentinel='compatibility-sentinel'
sentinel_value="gmpay-image-contract-$EXPECTED_ARCH"
phase=sentinel-container-write
docker exec "$container" sh -ceu 'printf "%s\\n" "$1" > "/data/$2"' container-shell "$sentinel_value" "$sentinel"
phase=sentinel-host-read
test "$(cat "$data_dir/$sentinel")" = "$sentinel_value"
phase=sentinel-container-read
test "$(docker exec "$container" cat "/data/$sentinel")" = "$sentinel_value"

phase=container-replace
docker rm -f "$container" >/dev/null
assert_no_running_test_container
start_container
assert_container_contract
assert_wget_and_readiness
assert_one_process
phase=sentinel-host-read-after-replace
test "$(cat "$data_dir/$sentinel")" = "$sentinel_value"
phase=sentinel-container-read-after-replace
test "$(docker exec "$container" cat "/data/$sentinel")" = "$sentinel_value"

if test -n "$EVIDENCE_FILE"; then
  umask 077
  printf '{"selector":"%s","declaredImage":"%s","immutableImage":"%s","expectedArchitecture":"%s","runnerArchitecture":"%s","dockerArchitecture":"%s","imagePlatform":"%s","platformDigest":"%s","configDigest":"%s","imageId":"%s","repoDigest":"%s","readiness":"passed","sentinelPersistence":"passed"}\n' \
    "$EVIDENCE_SELECTOR" "$IMAGE" "$IMMUTABLE_IMAGE" "$EXPECTED_ARCH" "$runner_arch" "$docker_arch" "$image_platform" "$platform_digest" "$config_digest" "$image_id" "$repo_digest" \
    > "$EVIDENCE_FILE"
  chmod 0600 "$EVIDENCE_FILE"
else
  printf '%s\n' "$EVIDENCE_SELECTOR"
fi
