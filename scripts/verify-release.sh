#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."

go test -count=1 ./...
go vet ./...
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
