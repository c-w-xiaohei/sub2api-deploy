#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(CDPATH= cd -- "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CANONICAL_ROOT="$(readlink -f -- "$ROOT_DIR")"
MANIFEST="$ROOT_DIR/scripts/release-bundle-files.txt"

die() {
  printf 'release bundle verification failed: %s\n' "$1" >&2
  exit 1
}

manifest_paths() {
  local path
  while IFS= read -r path || [[ -n "$path" ]]; do
    [[ -z "$path" || "$path" == \#* ]] && continue
    printf '%s\n' "$path"
  done < "$MANIFEST"
}

validate_manifest() {
  local path source ancestor resolved
  declare -A seen=()
  while IFS= read -r path; do
    [[ "$path" != /* && "$path" != ../* && "$path" != */../* && "$path" != */.. ]] || die "unsafe manifest path: $path"
    [[ -z "${seen[$path]+present}" ]] || die "duplicate manifest path: $path"
    seen["$path"]=1
    source="$ROOT_DIR/$path"
    ancestor="$ROOT_DIR"
    IFS=/ read -r -a segments <<<"$path"
    for segment in "${segments[@]}"; do
      ancestor="$ancestor/$segment"
      [[ ! -L "$ancestor" ]] || die "manifest source has a symlink ancestor: $path"
    done
    [[ -f "$source" ]] || die "manifest file is missing or not regular: $path"
    resolved="$(readlink -f -- "$source")"
    [[ "$resolved" == "$CANONICAL_ROOT/"* ]] || die "manifest source escapes repository: $path"
  done < <(manifest_paths)
}

# CI supplies immutable checkout and component trees. These checks and no-deref
# copying minimize shell-level TOCTOU exposure without claiming atomic openat safety.
copy_regular() {
  local source="$1"
  local destination="$2"
  local mode="$3"
  [[ -f "$source" && ! -L "$source" ]] || die "source is missing, not regular, or a symlink: $source"
  cp -p --no-dereference -- "$source" "$destination"
  [[ -f "$destination" && ! -L "$destination" ]] || die "copied file is not regular: $destination"
  chmod "$mode" "$destination"
  [[ "$(stat -c %a "$destination")" == "${mode#0}" ]] || die "copied file mode is incorrect: $destination"
}

assemble() {
  [[ $# -eq 3 ]] || die 'usage: release-bundle.sh assemble BUNDLE_DIR COMPONENT_DIR RELEASE_ID'
  local bundle_dir="$1"
  local component_dir="$2"
  local release_id="$3"
  local path destination host_dir amd64_hash arm64_hash amd64_size arm64_size canonical_component_dir source resolved_source

  validate_manifest
  [[ -d "$component_dir" && ! -L "$component_dir" ]] || die "component directory is missing or a symlink: $component_dir"
  canonical_component_dir="$(readlink -f -- "$component_dir")"
  for path in sub2api-deploy pulumi-program pulumi-resource-sub2api-host sub2api-host-linux-amd64 sub2api-host-linux-arm64; do
    source="$component_dir/$path"
    [[ -f "$source" && ! -L "$source" && -x "$source" ]] || die "required component is missing, not regular, executable, or is a symlink: $source"
    resolved_source="$(readlink -f -- "$source")"
    [[ "$(dirname -- "$resolved_source")" == "$canonical_component_dir" ]] || die "component source escapes component directory: $source"
  done
  [[ ! -e "$bundle_dir" ]] || die "bundle directory already exists: $bundle_dir"
  mkdir -p "$bundle_dir/bin" "$bundle_dir/artifacts/sub2api-host"
  copy_regular "$canonical_component_dir/sub2api-deploy" "$bundle_dir/bin/sub2api-deploy" 0755
  copy_regular "$canonical_component_dir/pulumi-program" "$bundle_dir/bin/pulumi-program" 0755
  copy_regular "$canonical_component_dir/pulumi-resource-sub2api-host" "$bundle_dir/bin/pulumi-resource-sub2api-host" 0755
  copy_regular "$canonical_component_dir/sub2api-host-linux-amd64" "$bundle_dir/artifacts/sub2api-host/sub2api-host-linux-amd64" 0755
  copy_regular "$canonical_component_dir/sub2api-host-linux-arm64" "$bundle_dir/artifacts/sub2api-host/sub2api-host-linux-arm64" 0755
  copy_regular "$ROOT_DIR/scripts/pulumi-go-shim.sh" "$bundle_dir/bin/go" 0755
  copy_regular "$ROOT_DIR/scripts/pulumi-cli-wrapper.sh" "$bundle_dir/bin/pulumi" 0755

  host_dir="$bundle_dir/artifacts/sub2api-host"
  amd64_hash="$(sha256sum "$host_dir/sub2api-host-linux-amd64" | cut -d' ' -f1)"
  arm64_hash="$(sha256sum "$host_dir/sub2api-host-linux-arm64" | cut -d' ' -f1)"
  amd64_size="$(stat -c %s "$host_dir/sub2api-host-linux-amd64")"
  arm64_size="$(stat -c %s "$host_dir/sub2api-host-linux-arm64")"
  jq -n \
    --arg release "$release_id" \
    --arg amd64_hash "$amd64_hash" \
    --arg arm64_hash "$arm64_hash" \
    --argjson amd64_size "$amd64_size" \
    --argjson arm64_size "$arm64_size" \
    '{schemaVersion: 1, release: $release,
      "linux-amd64": {path: "sub2api-host-linux-amd64", sha256: $amd64_hash, size: $amd64_size},
      "linux-arm64": {path: "sub2api-host-linux-arm64", sha256: $arm64_hash, size: $arm64_size}}' \
    > "$host_dir/manifest.json"

  while IFS= read -r path; do
    destination="$bundle_dir/$path"
    mkdir -p "$(dirname "$destination")"
    copy_regular "$(readlink -f -- "$ROOT_DIR/$path")" "$destination" 0644
  done < <(manifest_paths)
}

archive_members() {
  tar -tzf "$1" | while IFS= read -r path; do
    path="${path%/}"
    [[ -n "$path" ]] && printf '%s\n' "$path"
  done
}

verify_archive_shape() {
  local archive="$1"
  local member relative top="" path has_child=false type
  local -a members metadata expected
  declare -A seen=() member_types=()

  mapfile -t members < <(archive_members "$archive")
  # GNU tar emits the entry type as the first character of verbose metadata.
  # Pair it with the plain listing so quoted or otherwise unusual names fail closed.
  mapfile -t metadata < <(tar -tvzf "$archive")
  ((${#members[@]} > 0)) || die 'archive is empty'
  ((${#members[@]} == ${#metadata[@]})) || die 'archive metadata cannot be safely inspected'
  for member in "${members[@]}"; do
    type="${metadata[${#member_types[@]}]:0:1}"
    [[ "$member" != /* && "$member" != ../* && "$member" != */../* && "$member" != */.. ]] || die "unsafe archive member: $member"
    [[ "$member" != *$'\n'* ]] || die 'archive member contains a newline'
    if [[ -z "$top" ]]; then
      top="${member%%/*}"
    fi
    [[ "$member" == "$top" || "$member" == "$top/"* ]] || die 'archive contains more than one top-level bundle'
    [[ "$member" == "$top/"* ]] && has_child=true
    [[ -z "${seen[$member]+present}" ]] || die "archive contains duplicate member: $member"
    [[ "$type" == '-' || "$type" == 'd' ]] || die "archive member is not a regular file or directory: $member"
    seen["$member"]=1
    member_types["$member"]="$type"
  done
  [[ "$has_child" == true ]] || die 'archive has no top-level bundle directory'
  [[ "${member_types[$top]:-}" == d ]] || die 'archive top-level bundle is not a directory'

  expected=(bin/go bin/pulumi bin/sub2api-deploy bin/pulumi-program bin/pulumi-resource-sub2api-host artifacts/sub2api-host/manifest.json artifacts/sub2api-host/sub2api-host-linux-amd64 artifacts/sub2api-host/sub2api-host-linux-arm64)
  while IFS= read -r path; do expected+=("$path"); done < <(manifest_paths)
  for path in "${expected[@]}"; do
    [[ -n "${seen[$top/$path]+present}" ]] || die "required archive member is missing: $path"
    [[ "${member_types[$top/$path]}" == '-' ]] || die "required archive member is not a regular file: $path"
  done
  for member in "${members[@]}"; do
    [[ "$member" == "$top" ]] && continue
    relative="${member#"$top/"}"
    case "$relative" in
      active.yml|*/active.yml|compose/override.yml)
        die "stale single-Site path is shipped: $relative"
        ;;
    esac
    for path in "${expected[@]}"; do
      [[ "$relative" == "$path" || "$path" == "$relative/"* ]] && break
    done
    [[ "$relative" == "$path" || "$path" == "$relative/"* ]] || die "unlisted archive member: $relative"
  done

  BUNDLE_TOP="$top"
}

verify_host_artifacts() {
  [[ $# -eq 1 ]] || die 'usage: release-bundle.sh verify-host-artifacts BUNDLE_DIR'
  local bundle_root="$1"
  local host_dir="$bundle_root/artifacts/sub2api-host"
  local manifest="$host_dir/manifest.json"
  local architecture name expected_machine declared_path declared_hash declared_size actual_hash actual_size elf_header

  [[ -f "$manifest" && ! -L "$manifest" ]] || die 'Host artifact manifest is missing or not a regular file'
  jq -e '
    type == "object" and
    (keys | sort) == ["linux-amd64", "linux-arm64", "release", "schemaVersion"] and
    (.schemaVersion | type == "number" and . == 1) and
    (.release | type == "string" and length > 0) and
    ([."linux-amd64", ."linux-arm64"] | all(
      type == "object" and
      (keys | sort) == ["path", "sha256", "size"] and
      (.path | type == "string") and
      (.sha256 | type == "string" and test("^[0-9a-f]{64}$")) and
      (.size | type == "number" and floor == . and . >= 0 and . <= 67108864)
    ))
  ' "$manifest" >/dev/null || die 'Host artifact manifest has an invalid schema'

  for architecture in amd64 arm64; do
    name="sub2api-host-linux-$architecture"
    expected_machine=62
    [[ "$architecture" == arm64 ]] && expected_machine=183
    declared_path="$(jq -r --arg key "linux-$architecture" '.[$key].path' "$manifest")"
    declared_hash="$(jq -r --arg key "linux-$architecture" '.[$key].sha256' "$manifest")"
    declared_size="$(jq -r --arg key "linux-$architecture" '.[$key].size' "$manifest")"
    [[ "$declared_path" == "$name" ]] || die "Host artifact path does not bind to linux-$architecture binary"
    [[ -f "$host_dir/$name" && ! -L "$host_dir/$name" && -x "$host_dir/$name" ]] || die "Host artifact is missing, not regular, or not executable: $name"
    actual_hash="$(sha256sum "$host_dir/$name" | cut -d' ' -f1)"
    actual_size="$(stat -c %s "$host_dir/$name")"
    [[ "$declared_hash" == "$actual_hash" ]] || die "Host artifact checksum does not match: $name"
    [[ "$declared_size" == "$actual_size" ]] || die "Host artifact size does not match: $name"
    elf_header="$(od -An -N20 -tu1 "$host_dir/$name" | tr -s ' ' | tr '\n' ' ')"
    read -r -a elf_bytes <<<"$elf_header"
    ((${#elf_bytes[@]} >= 20)) || die "Host artifact is not a complete ELF header: $name"
    [[ "${elf_bytes[0]} ${elf_bytes[1]} ${elf_bytes[2]} ${elf_bytes[3]} ${elf_bytes[4]} ${elf_bytes[5]}" == '127 69 76 70 2 1' ]] || die "Host artifact is not a 64-bit little-endian ELF binary: $name"
    [[ "$((elf_bytes[18] + 256 * elf_bytes[19]))" == "$expected_machine" ]] || die "Host artifact ELF machine does not match linux-$architecture: $name"
  done
}

verify_content() {
  local bundle_root="$1"

  for path in bin/go bin/pulumi bin/sub2api-deploy bin/pulumi-program bin/pulumi-resource-sub2api-host; do
    [[ -f "$bundle_root/$path" && ! -L "$bundle_root/$path" && -x "$bundle_root/$path" ]] || die "required executable is missing, not regular, or not executable: $path"
  done
  verify_host_artifacts "$bundle_root"
  while IFS= read -r path; do
    [[ -f "$bundle_root/$path" && ! -L "$bundle_root/$path" ]] || die "required bundle file is missing or not regular: $path"
  done < <(manifest_paths)
  check_plugin_metadata "$bundle_root/scripts/pulumi-plugins/cloudflare/pulumi-plugin.json" cloudflare 6.18.0 ''
  check_plugin_metadata "$bundle_root/scripts/pulumi-plugins/upstash/pulumi-plugin.json" upstash '' github://api.github.com/upstash/pulumi-upstash
  local fake_cli
  fake_cli="$(mktemp -d "${TMPDIR:-/tmp}/sub2api-fake-pulumi.XXXXXX")/pulumi"
  cat > "$fake_cli" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
[[ "${PATH%%:*}" == "$EXPECTED_BUNDLE_BIN" ]] || exit 41
[[ "${1:-}" == --bundle-contract ]] || exit 42
printf 'fake pulumi delegate verified\n'
EOF
  chmod 0755 "$fake_cli"
  EXPECTED_BUNDLE_BIN="$bundle_root/bin" PATH="$(dirname "$fake_cli"):$PATH" "$bundle_root/bin/pulumi" --bundle-contract >/dev/null
  rm -rf "$(dirname "$fake_cli")"
}

check_plugin_metadata() {
  local metadata="$1"
  local name="$2"
  local version="$3"
  local server="$4"
  jq -e --arg name "$name" --arg version "$version" --arg server "$server" '
    type == "object" and
    (if $version == "" then . == {resource: true, name: $name, server: $server}
     elif $server == "" then . == {resource: true, name: $name, version: $version}
     else false end)
  ' "$metadata" >/dev/null || die "provider metadata contract failed: $name"
}

verify() {
  [[ $# -eq 1 ]] || die 'usage: release-bundle.sh verify ARCHIVE_PATH'
  local archive="$1"
  local extraction bundle_root
  [[ -f "$archive" ]] || die "archive is missing: $archive"
  extraction="$(mktemp -d "${TMPDIR:-/tmp}/sub2api-release-verify.XXXXXX")"
  trap 'rm -rf "$extraction"' RETURN
  verify_archive_shape "$archive"
  tar -xzf "$archive" -C "$extraction"
  bundle_root="$extraction/$BUNDLE_TOP"
  [[ -d "$bundle_root" ]] || die 'archive top-level bundle directory did not extract'
  verify_content "$bundle_root"
}

operation="${1:-}"
shift || true
case "$operation" in
  assemble) assemble "$@" ;;
  verify) verify "$@" ;;
  verify-host-artifacts) verify_host_artifacts "$@" ;;
  *) die 'usage: release-bundle.sh {assemble BUNDLE_DIR COMPONENT_DIR RELEASE_ID|verify ARCHIVE_PATH|verify-host-artifacts BUNDLE_DIR}' ;;
esac
