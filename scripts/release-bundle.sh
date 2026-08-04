#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(CDPATH= cd -- "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
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
  local path
  declare -A seen=()
  while IFS= read -r path; do
    [[ "$path" != /* && "$path" != ../* && "$path" != */../* && "$path" != */.. ]] || die "unsafe manifest path: $path"
    [[ -z "${seen[$path]+present}" ]] || die "duplicate manifest path: $path"
    seen["$path"]=1
    [[ -f "$ROOT_DIR/$path" ]] || die "manifest file is missing: $path"
  done < <(manifest_paths)
}

assemble() {
  [[ $# -eq 2 ]] || die 'usage: release-bundle.sh assemble BUNDLE_DIR PROGRAM_PATH'
  local bundle_dir="$1"
  local program_path="$2"
  local path destination

  validate_manifest
  [[ -f "$program_path" ]] || die "Pulumi program is missing: $program_path"
  [[ ! -e "$bundle_dir" ]] || die "bundle directory already exists: $bundle_dir"
  mkdir -p "$bundle_dir/bin"
  install -m 0755 "$program_path" "$bundle_dir/bin/pulumi-program"
  install -m 0755 "$ROOT_DIR/scripts/pulumi-go-shim.sh" "$bundle_dir/bin/go"
  install -m 0755 "$ROOT_DIR/scripts/pulumi-cli-wrapper.sh" "$bundle_dir/bin/pulumi"

  while IFS= read -r path; do
    destination="$bundle_dir/$path"
    mkdir -p "$(dirname "$destination")"
    cp -p -- "$ROOT_DIR/$path" "$destination"
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
  local member relative top="" path has_child=false
  local -a members expected
  declare -A seen=()

  mapfile -t members < <(archive_members "$archive")
  ((${#members[@]} > 0)) || die 'archive is empty'
  for member in "${members[@]}"; do
    [[ "$member" != /* && "$member" != ../* && "$member" != */../* && "$member" != */.. ]] || die "unsafe archive member: $member"
    [[ "$member" != *$'\n'* ]] || die 'archive member contains a newline'
    if [[ -z "$top" ]]; then
      top="${member%%/*}"
    fi
    [[ "$member" == "$top" || "$member" == "$top/"* ]] || die 'archive contains more than one top-level bundle'
    [[ "$member" == "$top/"* ]] && has_child=true
    [[ -z "${seen[$member]+present}" ]] || die "archive contains duplicate member: $member"
    seen["$member"]=1
  done
  [[ "$has_child" == true ]] || die 'archive has no top-level bundle directory'

  expected=(bin/go bin/pulumi bin/pulumi-program)
  while IFS= read -r path; do expected+=("$path"); done < <(manifest_paths)
  for path in "${expected[@]}"; do
    [[ -n "${seen[$top/$path]+present}" ]] || die "required archive member is missing: $path"
  done
  for member in "${members[@]}"; do
    [[ "$member" == "$top" ]] && continue
    relative="${member#"$top/"}"
    case "$relative" in
      active.yml|*/active.yml|scripts/compose-common.sh|scripts/deploy-compose.sh|scripts/infra-reconcile.sh|scripts/render-route.ts|compose/override.yml)
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

require_text() {
  local file="$1"
  local text="$2"
  local description="$3"
  grep -Fq -- "$text" "$file" || die "$description"
}

require_count() {
  local file="$1"
  local text="$2"
  local expected="$3"
  local actual
  actual="$(grep -Fc -- "$text" "$file")"
  [[ "$actual" == "$expected" ]] || die "expected $expected occurrences of $text in $file, found $actual"
}

check_provider_metadata() {
  local provider="$1"
  local expected_name="$2"
  local expected_version="$3"
  local expected_server="$4"
  local metadata="$bundle_root/scripts/pulumi-plugins/$provider/pulumi-plugin.json"
  if ! jq -e \
    --arg name "$expected_name" \
    --arg version "$expected_version" \
    --arg server "$expected_server" \
    '.resource == true and .name == $name
      and (if $version == "" then (has("version") | not) else .version == $version end)
      and (if $server == "" then (has("server") | not) else .server == $server end)' \
    "$metadata" >/dev/null; then
    die "provider metadata contract failed: $provider"
  fi
}

check_module_metadata() {
  local module="$1"
  local version="$2"
  local provider="$3"
  local output expected_dir
  expected_dir="$bundle_root/scripts/pulumi-plugins/$provider"
  output="$(PULUMI_BUNDLE_ROOT="$bundle_root" "$bundle_root/bin/go" list -m -json "$module")" \
    || die "bundled Go metadata query failed: $module"
  if ! jq -e \
    --arg path "$module" \
    --arg expected_version "$version" \
    --arg expected_dir "$expected_dir" \
    'type == "object" and .Path == $path and .Version == $expected_version and .Dir == $expected_dir' \
    <<<"$output" >/dev/null; then
    die "bundled Go metadata contract failed: $module"
  fi
}

check_sing_box_template() {
  awk '
    function fail(message) { print message > "/dev/stderr"; exit 1 }
    $0 == "  routers:" { section = "routers"; next }
    $0 == "  services:" { section = "services"; next }
    section == "routers" && $0 == "    sing-box-reality:" {
      if (router++) fail("duplicate sing-box router")
      section = "router"
      next
    }
    section == "router" {
      if ($0 == "      rule: \"HostSNI(`${SING_BOX_SERVER_NAME}`)\"") rule = 1
      else if ($0 == "      entryPoints:") entry_points = 1
      else if ($0 == "        - websecure") websecure = 1
      else if ($0 == "      service: sing-box-reality") router_service = 1
      else if ($0 == "      tls:") tls = 1
      else if ($0 == "        passthrough: true") passthrough = 1
    }
    section == "services" && $0 == "    sing-box-reality:" {
      if (service++) fail("duplicate sing-box service")
      section = "service"
      next
    }
    section == "service" {
      if ($0 == "      loadBalancer:") load_balancer = 1
      else if ($0 == "        servers:") servers = 1
      else if ($0 == "          - address: \"${SING_BOX_TARGET}\"") address = 1
    }
    END {
      if (router != 1 || service != 1 || !rule || !entry_points || !websecure
        || !router_service || !tls || !passthrough || !load_balancer || !servers || !address) exit 1
    }
  ' "$1" || die 'sing-box router/service template relationship is invalid'
}

check_site_route_template() {
  awk '
    function fail(message) { print message > "/dev/stderr"; exit 1 }
    $0 == "  routers:" { section = "routers"; next }
    $0 == "  services:" { section = "services"; next }
    section == "routers" && $0 == "    site-${SITE_ID}-${SLOT}:" {
      if (router++) fail("duplicate Site router")
      section = "router"
      next
    }
    section == "router" {
      if ($0 == "      rule: \"Host(`${DOMAIN}`)\"") rule = 1
      else if ($0 == "      entryPoints:") entry_points = 1
      else if ($0 == "        - websecure") websecure = 1
      else if ($0 == "      tls:") tls = 1
      else if ($0 == "        certResolver: cloudflare") cert_resolver = 1
      else if ($0 == "      service: site-${SITE_ID}-${SLOT}") router_service = 1
    }
    section == "services" && $0 == "    site-${SITE_ID}-${SLOT}:" {
      if (service++) fail("duplicate Site service")
      section = "service"
      next
    }
    section == "service" {
      if ($0 == "      loadBalancer:") load_balancer = 1
      else if ($0 == "        servers:") servers = 1
      else if ($0 == "          - url: \"http://${ACTIVE_EDGE_ALIAS}:8080\"") address = 1
    }
    END {
      if (router != 1 || service != 1 || !rule || !entry_points || !websecure || !tls
        || !cert_resolver || !router_service || !load_balancer || !servers || !address) exit 1
    }
  ' "$1" || die 'Site router/service template relationship is invalid'
}

verify_content() {
  local bundle_root="$1"
  local verification_dir edge_env edge_json site_env site_json site_id

  for path in bin/go bin/pulumi bin/pulumi-program; do
    [[ -x "$bundle_root/$path" ]] || die "required executable is missing: $path"
  done
  for path in compose/edge.yml compose/site.yml traefik/dynamic/sing-box.yml traefik/dynamic/site.yml scripts/host-preflight.ts scripts/bootstrap-site.sh scripts/application-release.sh scripts/switch-slot.sh scripts/rollback-slot.sh scripts/reconcile-site.sh scripts/reconcile-edge.sh docs/migrations/single-site-to-multi-site.md; do
    [[ -f "$bundle_root/$path" ]] || die "required active path is missing: $path"
  done

  verification_dir="$bundle_root/.release-verification"
  mkdir -p "$verification_dir/edge" "$verification_dir/sites"
  edge_env="$verification_dir/edge.env"
  edge_json="$verification_dir/edge.json"
  printf '%s\n' \
    'TRAEFIK_IMAGE=traefik:v3.3.3' \
    'CLOUDFLARE_DNS_API_TOKEN=not-used-in-verification' \
    'ACME_EMAIL=ops@example.com' \
    "EDGE_RUNTIME_ROOT=$verification_dir/edge" > "$edge_env"
  docker compose \
    --project-name sub2api-edge \
    --env-file "$edge_env" \
    -f "$bundle_root/compose/edge.yml" \
    config --format json > "$edge_json"
  jq -e '
    (.services | keys) == ["traefik"] and
    ([.services.traefik.ports[]? |
      {target: (.target | tostring), published: (.published | tostring)}] |
      sort_by(.target)) ==
      ([{target: "443", published: "443"}, {target: "80", published: "80"}] |
        sort_by(.target))
  ' "$edge_json" >/dev/null || die 'Edge Compose must render only Traefik with exactly public ports 80 and 443'

  require_text "$bundle_root/compose/edge.yml" 'host.docker.internal:host-gateway' 'host gateway mapping is missing from the assembled Edge Compose file'
  require_text "$bundle_root/compose/edge.yml" 'name: sub2api-edge' 'Edge network name is missing from the assembled Edge Compose file'

  for site_id in code2 code3; do
    site_env="$verification_dir/sites/$site_id.env"
    site_json="$verification_dir/sites/$site_id.json"
    printf '%s\n' \
      'SUB2API_IMAGE=weishaw/sub2api@sha256:abcdef1234567890' \
      "SITE_RUNTIME_ROOT=$verification_dir/sites/$site_id" \
      "BLUE_EDGE_ALIAS=sub2api-$site_id-blue" \
      "GREEN_EDGE_ALIAS=sub2api-$site_id-green" \
      'SLOT=blue' \
      'SLOT_DATA_DIR=blue' \
      'AUTO_SETUP=false' \
      "DOMAIN=$site_id.example.com" \
      'DATABASE_HOST=postgres' \
      'DATABASE_PORT=5432' \
      'DATABASE_USER=sub2api' \
      'DATABASE_PASSWORD=not-used-in-verification' \
      'DATABASE_DBNAME=sub2api' \
      'DATABASE_SSLMODE=disable' \
      'POSTGRES_USER=sub2api' \
      'POSTGRES_PASSWORD=not-used-in-verification' \
      'POSTGRES_DB=sub2api' \
      'REDIS_HOST=redis' \
      'REDIS_PORT=6379' \
      'REDIS_PASSWORD=not-used-in-verification' \
      'REDIS_ENABLE_TLS=false' \
      'ADMIN_EMAIL=admin@example.com' > "$site_env"
    docker compose \
      --project-name "sub2api-$site_id" \
      --env-file "$site_env" \
      --profile app \
      --profile postgres \
      --profile redis \
      -f "$bundle_root/compose/upstream.yml" \
      -f "$bundle_root/compose/site.yml" \
      config --format json > "$site_json"
    jq -e '
      ([.services[] | (.ports // [])[]? |
        select((.published // null) != null)] | length) == 0 and
      ((.services | has("sub2api-blue")) and
        (.services | has("sub2api-green"))) and
      (.services["sub2api-blue"].networks | has("sub2api-edge")) and
      (.services["sub2api-green"].networks | has("sub2api-edge")) and
      (.networks["sub2api-edge"].external == true) and
      (.networks["sub2api-edge"].name == "sub2api-edge")
    ' "$site_json" >/dev/null || die "Site Compose contract failed for $site_id"
  done

  check_sing_box_template "$bundle_root/traefik/dynamic/sing-box.yml"
  check_site_route_template "$bundle_root/traefik/dynamic/site.yml"
  require_text "$bundle_root/scripts/host-preflight.ts" 'checkHostPreflight' 'host preflight implementation is missing'

  check_provider_metadata cloudflare cloudflare 6.18.0 ''
  check_provider_metadata command command 1.2.1 ''
  check_provider_metadata neon neon 0.0.1-alpha.1 'https://github.com/kislerdm/pulumi-neon/releases/download/v${VERSION}'
  check_provider_metadata upstash upstash '' 'github://api.github.com/upstash/pulumi-upstash'
  check_module_metadata github.com/pulumi/pulumi-cloudflare/sdk/v6 v6.18.0 cloudflare
  check_module_metadata github.com/pulumi/pulumi-command/sdk v1.2.1 command
  check_module_metadata github.com/kislerdm/pulumi-sdk-neon v0.0.0-20241217015548-601a1132b220 neon
  check_module_metadata github.com/upstash/pulumi-upstash/sdk v0.5.0 upstash

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
  if grep -R -Fq -- 'active.yml' "$bundle_root"; then
    die 'deleted traefik/dynamic/active.yml is referenced in the assembled bundle'
  fi
}

operation="${1:-}"
shift || true
case "$operation" in
  assemble) assemble "$@" ;;
  verify) verify "$@" ;;
  *) die 'usage: release-bundle.sh {assemble BUNDLE_DIR PROGRAM_PATH|verify ARCHIVE_PATH}' ;;
esac
