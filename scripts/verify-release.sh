#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."

go test -count=1 ./...
go vet ./...
go build -o /tmp/sub2api-pulumi-go ./infra
go mod verify

npm ci
npm test
npm run build
bash -n scripts/*.sh
bash scripts/validate-compose.sh

rm -rf /tmp/sub2api-release-bundle /tmp/sub2api-release-bundle.tar.gz
bash scripts/release-bundle.sh assemble /tmp/sub2api-release-bundle /tmp/sub2api-pulumi-go
tar -C /tmp -czf /tmp/sub2api-release-bundle.tar.gz sub2api-release-bundle
bash scripts/release-bundle.sh verify /tmp/sub2api-release-bundle.tar.gz
