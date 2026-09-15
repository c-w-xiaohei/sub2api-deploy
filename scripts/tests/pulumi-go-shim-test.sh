#!/usr/bin/env bash
# Exercise the prebuilt Go language-host metadata shim, without a Go compiler.
set -euo pipefail
root="$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)"
export PULUMI_BUNDLE_ROOT="$root"
all="$(bash "$root/scripts/pulumi-go-shim.sh" list -m -json all)"
jq -es --arg root "$root" '
  length == 4 and
  (map(.Path) | unique | length == 4) and
  (map(select(.Path == "github.com/pulumi/pulumi-cloudflare/sdk/v6")) ==
    [{Path:"github.com/pulumi/pulumi-cloudflare/sdk/v6", Version:"v6.18.0",
      Dir:($root + "/scripts/pulumi-plugins/cloudflare")}]) and
  (map(select(.Path == "github.com/upstash/pulumi-upstash/sdk")) ==
    [{Path:"github.com/upstash/pulumi-upstash/sdk", Version:"v0.5.0",
      Dir:($root + "/scripts/pulumi-plugins/upstash")}])
' <<< "$all" >/dev/null
while IFS= read -r directory; do
  jq -e '.resource == true and (.name == "cloudflare" or .name == "upstash")' \
    "$directory/pulumi-plugin.json" >/dev/null
done < <(jq -rs '.[] | select(.Path == "github.com/pulumi/pulumi-cloudflare/sdk/v6" or .Path == "github.com/upstash/pulumi-upstash/sdk") | .Dir' <<< "$all")
specific="$(bash "$root/scripts/pulumi-go-shim.sh" list -m -json github.com/pulumi/pulumi-cloudflare/sdk/v6)"
jq -es 'length == 1 and .[0].Version == "v6.18.0"' <<< "$specific" >/dev/null
if bash "$root/scripts/pulumi-go-shim.sh" build ./... >/dev/null 2>&1; then
  printf 'shim must reject compilation\n' >&2
  exit 1
fi
printf 'Pulumi Go shim plugin metadata fixture passed\n'
