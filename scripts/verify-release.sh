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
