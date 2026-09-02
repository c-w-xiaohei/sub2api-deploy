#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."

module="$(go list -m)"
[[ -n "$module" ]] || { printf 'release verification failed: Go module is empty\n' >&2; exit 1; }
package_file="$(mktemp)"
trap 'rm -f "$package_file"' EXIT
go list ./... > "$package_file"
mapfile -t packages < "$package_file"
target_packages=()
infra_found=false
for package in "${packages[@]}"; do
  if [[ "$package" == "$module/infra" ]]; then
    infra_found=true
    continue
  fi
  target_packages+=("$package")
done
[[ "$infra_found" == true ]] || { printf 'release verification failed: legacy infra package was not enumerated\n' >&2; exit 1; }
((${#target_packages[@]} > 0)) || { printf 'release verification failed: no target Go packages were enumerated\n' >&2; exit 1; }
# Legacy infra source remains in the repository but is not target release verification surface.
go test -count=1 "${target_packages[@]}"
go vet "${target_packages[@]}"
go mod verify

bash -n scripts/*.sh

component_dir=/tmp/sub2api-release-components
bundle_dir=/tmp/sub2api-release-bundle
archive=/tmp/sub2api-release-bundle.tar.gz
release_id=ci-verification

rm -rf "$component_dir" "$bundle_dir" "$archive"
mkdir -p "$component_dir"
go build -trimpath -o "$component_dir/sub2api-deploy" ./cmd/sub2api-deploy
bash scripts/build-pulumi-release.sh "$component_dir/pulumi-program"
go build -trimpath -o "$component_dir/pulumi-resource-sub2api-host" ./cmd/pulumi-resource-sub2api-host
for goarch in amd64 arm64; do
  CGO_ENABLED=0 GOOS=linux GOARCH="$goarch" go build -trimpath -o "$component_dir/sub2api-host-linux-$goarch" ./cmd/sub2api-host
done

bash scripts/release-bundle.sh assemble "$bundle_dir" "$component_dir" "$release_id"
tar -C /tmp -czf /tmp/sub2api-release-bundle.tar.gz sub2api-release-bundle
bash scripts/release-bundle.sh verify /tmp/sub2api-release-bundle.tar.gz
