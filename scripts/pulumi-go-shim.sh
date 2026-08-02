#!/usr/bin/env bash
set -euo pipefail

# Pulumi's Go language host uses these read-only commands even when the program
# itself is prebuilt. This shim supplies the metadata from the release bundle;
# it is not a Go compiler or a general-purpose Go replacement.
bundle_root="${PULUMI_BUNDLE_ROOT:-$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)}"
go_version="go1.25.11"

json_module() {
  local module_path="$1"
  local module_dir="$bundle_root"
  local module_version="v0.0.0"

  case "$module_path" in
    github.com/pulumi/pulumi-cloudflare/sdk/v6)
      module_dir="$bundle_root/scripts/pulumi-plugins/cloudflare"
      module_version="v6.18.0"
      ;;
    github.com/pulumi/pulumi-command/sdk)
      module_dir="$bundle_root/scripts/pulumi-plugins/command"
      module_version="v1.2.1"
      ;;
    github.com/upstash/pulumi-upstash/sdk)
      module_dir="$bundle_root/scripts/pulumi-plugins/upstash"
      module_version="v0.5.0"
      ;;
    github.com/kislerdm/pulumi-sdk-neon)
      module_dir="$bundle_root/scripts/pulumi-plugins/neon"
      module_version="v0.0.0-20241217015548-601a1132b220"
      ;;
  esac

  printf '{"Path":"%s","Version":"%s","Dir":"%s"}\n' \
    "$module_path" "$module_version" "$module_dir"
}

module_list() {
  local module_path
  for module_path in "$@"; do
    case "$module_path" in
      -*|'') continue ;;
    esac
    json_module "$module_path"
  done
}

command_name="${1:-}"
shift || true

case "$command_name" in
  version)
    architecture="$(uname -m)"
    case "$architecture" in
      x86_64) architecture="amd64" ;;
      aarch64) architecture="arm64" ;;
    esac
    printf 'go version %s linux/%s\n' "$go_version" "$architecture"
    ;;
  env)
    case "${1:-}" in
      GOMOD) printf '%s\n' "$bundle_root/go.mod" ;;
      GOVERSION) printf '%s\n' "$go_version" ;;
      *) printf 'unsupported bundled go env query: %s\n' "${1:-}" >&2; exit 2 ;;
    esac
    ;;
  list)
    if [[ " $* " == *" -f "* ]]; then
      printf '%s\n' "$bundle_root/go.mod"
    elif [[ " $* " == *" -json "* ]]; then
      module_list "$@"
    else
      printf 'unsupported bundled go list query\n' >&2
      exit 2
    fi
    ;;
  mod)
    if [[ "${1:-}" == "download" && " $* " == *" -json "* ]]; then
      shift
      module_list "$@"
    else
      printf 'unsupported bundled go mod query\n' >&2
      exit 2
    fi
    ;;
  *)
    printf 'unsupported bundled go command: %s\n' "$command_name" >&2
    exit 2
    ;;
esac
