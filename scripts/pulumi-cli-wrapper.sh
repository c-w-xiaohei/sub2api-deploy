#!/usr/bin/env bash
set -euo pipefail

bundle_bin="${PULUMI_BUNDLE_BIN:-$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../bin" && pwd)}"

# The wrapper itself is named pulumi, so remove its directory before resolving
# the real CLI. The bundled go shim remains available to the Go language host.
if [[ "$PATH" == "$bundle_bin:"* ]]; then
  PATH="${PATH#"$bundle_bin:"}"
elif [[ "$PATH" == "$bundle_bin" ]]; then
  PATH=""
fi
export PATH="$bundle_bin${PATH:+:$PATH}"

pulumi_cli=""
old_ifs="$IFS"
IFS=:
for directory in $PATH; do
  [[ -z "$directory" || "$directory" == "$bundle_bin" ]] && continue
  if [[ -x "$directory/pulumi" ]]; then
    pulumi_cli="$directory/pulumi"
    break
  fi
done
IFS="$old_ifs"

if [[ -z "$pulumi_cli" ]]; then
  printf 'Pulumi CLI not found outside the release bundle; install pulumi first\n' >&2
  exit 127
fi

exec "$pulumi_cli" "$@"
