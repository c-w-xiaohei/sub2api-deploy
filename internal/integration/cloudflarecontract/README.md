# Cloudflare Provider Contract Gate

This CI-only integration gate starts the official prebuilt
`pulumi-resource-cloudflare` v6.18.0 binary named by `CF_PROVIDER_BINARY`. It
uses schema and Check RPCs only. It does not Configure the provider or invoke
resource lifecycle RPCs.

Run the required CI mode inside a fresh Linux network namespace containing
only loopback. This prevents a provider implementation from reaching a cloud
endpoint during its startup or Check implementation; the test itself does not
claim that omitting credentials alone provides network isolation.

```sh
GOMAXPROCS=2 go test -c -p=1 -o "$RUNNER_TEMP/cloudflare-contract.test" ./internal/integration/cloudflarecontract
sudo env GOMAXPROCS=2 CF_PROVIDER_CONTRACT_REQUIRED=1 \
CF_PROVIDER_BINARY="$PULUMI_HOME/plugins/resource-cloudflare-v6.18.0/pulumi-resource-cloudflare" \
unshare --net --fork sh -ceu '
  ip link set lo up
  exec "$1" -test.v -test.parallel=1 -test.count=1 -test.timeout=2m
' sh "$RUNNER_TEMP/cloudflare-contract.test"
```

Compile the test binary while network access is available; run only the
precompiled test in the isolated namespace. The CI job must install the
official release before these commands. The test
fails closed if the required binary is absent, not absolute, not a regular
executable, if the namespace has an active non-loopback interface, or if Check
requires Configure. Outside required mode an absent binary skips the tests;
an unconfigured-Check limitation skips only the Check test after the schema
test has run.
