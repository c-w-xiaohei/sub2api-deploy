#!/usr/bin/env bash
set -euo pipefail

output_path="${1:?usage: build-pulumi-release.sh OUTPUT_PATH}"
mkdir -p "$(dirname "$output_path")"

CGO_ENABLED=0 GOOS="${GOOS:-linux}" GOARCH="${GOARCH:-amd64}" \
  go build -trimpath -o "$output_path" ./infra

chmod 0755 "$output_path"
