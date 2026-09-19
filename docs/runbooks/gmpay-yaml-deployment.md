# GM Pay YAML Deployment

This runbook covers the YAML-driven GM Pay deployment supported by the Host
controller. It does not cover Sub2API payment-provider integration or GM Pay
business configuration.

## Configuration

Add an optional top-level `paymentGateways` entry to the plaintext environment
configuration:

```yaml
paymentGateways:
  gmpay-primary:
    type: gmpay
    server: api-one
    hostname: pay.example.com
    image: gmwallet/epusdt:v2.0.0
    publicAccess:
      type: cloudflare
      cloudflare:
        mode: dns
        connectBy: publicAddress
```

The gateway ID follows the lowercase resource-ID rules. `type: gmpay` is the
only supported gateway type. Each gateway selects exactly one configured
server, and its hostname must be unique across Apps and payment gateways.

Cloudflare publication requires the top-level `cloudflare.zoneId`, an
encrypted `cloudflare.apiToken` in `secrets.yaml`, and a public address on the
selected server. The controller creates DNS records for the selected server's
public addresses. `publicAccess.cloudflare.mode: dns` and
`connectBy: publicAddress` are required for this gateway shape.

Accepted image references are:

- An explicit `vMAJOR.MINOR.PATCH` tag, such as `gmwallet/epusdt:v2.0.0`.
- That version tag followed by an `@sha256:<64 lowercase hex digits>` digest.
- An immutable `repository@sha256:<64 lowercase hex digits>` digest.

Untagged images, `latest`, prerelease tags, malformed version tags, malformed
digests, and other floating references are rejected. The image string is the
upgrade intent: reusing the same tag does not pull, refresh, or restart the
gateway, even if an upstream registry tag has changed. Change the YAML image
reference explicitly to request an upgrade.

## Validate And Deploy

From the Nix-managed Controller workspace, validate and preview the complete
Pulumi graph before applying an approved change:

```bash
sub2api-deploy validate production
sub2api-deploy pulumi production preview --diff
sub2api-deploy pulumi production up --yes
```

These are operator commands, not a claim that a production operation was run
as part of this documentation change. Review that the preview places the
gateway only on the selected server, creates the expected Cloudflare records,
and contains no unexpected replacement or deletion. The normal read-only
refresh command is:

```bash
sub2api-deploy pulumi production refresh
```

Do not bypass the wrapper with ad hoc production Docker or Pulumi commands.

## Runtime And First Setup

The selected Host runs exactly one active GM Pay container. The container joins
the Host's internal managed Docker network, has a stable derived network alias,
mounts its persistent directory at `/data`, and receives
`EPUSDT_CONFIG=/data/.env`. No host port is published. Traefik routes the
gateway hostname to container port `8000` on the managed network.

The Host derives the gateway data directory beneath its state root:

```text
/var/lib/sub2api-host/runtime/data/<derived-payment-gateway-data-token>
```

The exact token derivation is:

```text
gatewayToken = first 24 lowercase hex characters of
  SHA256("payment-gateway" + NUL + gateway ID + NUL)
dataToken = first 24 lowercase hex characters of
  SHA256("payment-gateway-data" + NUL + gatewayToken + NUL)
```

Environment and server are not inputs to either token. The Host state root and
its ownership metadata separate machines and environments; the token itself is
not a YAML field and must not be replaced with a hand-chosen value. For a
read-only path calculation, using the gateway ID from the YAML:

```bash
python3 - 'gmpay-primary' <<'PY'
import hashlib
import sys

gateway_id = sys.argv[1]
gateway_token = hashlib.sha256(
    b"payment-gateway\0" + gateway_id.encode() + b"\0"
).hexdigest()[:24]
data_token = hashlib.sha256(
    b"payment-gateway-data\0" + gateway_token.encode() + b"\0"
).hexdigest()[:24]
print(f"/var/lib/sub2api-host/runtime/data/{data_token}")
PY
```

The Host's ownership and recovery metadata remains under
`/var/lib/sub2api-host`; preserve that state together with the data directory.

Deployment readiness is an HTTP response from the container's root/install
surface. The first deployment can therefore complete while the first-run
`.env` file and setup wizard are still pending; deployment does not synthesize
`.env`. After the Pulumi update publishes the hostname, open the GM Pay URL in
an approved browser path and complete the Web setup there.

Wallet configuration, RPC configuration, GM Pay administrator setup, merchant
keys, API keys, and Sub2API provider linkage are deliberately outside this
deployment feature. Configure them through the product's approved Web or
secret-management procedures after the container is reachable.

## Updates And Retirement

For an explicit image-reference change, the Host pulls the requested target
before interrupting the current worker. It stops the old container before
starting the new one, so old and new workers never process the same `/data`
directory concurrently. The old container is retained as rollback material
until the new container is ready. A pull, startup, or root/install readiness
failure removes the failed candidate and restores the old worker and route.

Changing only the hostname rewrites the route without restarting the worker.
When the owned container is present and healthy, repeated reconciliation of the
same image string does not refresh, pull, or restart it. If that owned
container is missing, the Host attempts repair/start with `--pull never` from
the locally available image; if that image is absent, repair fails rather than
fetching it. A same-tag declaration never fetches upstream-mutated bytes. The
Host journal makes interrupted operations replayable without creating
concurrent workers.

Removing a gateway from YAML is a gateway configuration removal, not whole
Host retirement. Applying that removal removes the gateway route and container
but preserves its data directory and retained gateway metadata. Re-adding the
same gateway ID on the same Host reuses that data and metadata.

## Server-Move Migration

A direct change to `server` is rejected because it could run source and
destination workers concurrently and does not migrate data. Use this explicit
downtime sequence; the completed removal apply clears the placement guard so a
later re-add on the destination can proceed without starting against an empty
directory.
Verify the backup before proceeding; the hostname has no DNS publication while
the gateway is removed. Before step 1, check whether any App or other gateway
still consumes Cloudflare. If this gateway is the only Cloudflare consumer,
the temporary removal configuration must also remove the top-level
`cloudflare.zoneId` and the corresponding encrypted `cloudflare.apiToken` from
the SOPS-managed environment secrets in the same change. If any App or other
gateway still consumes Cloudflare, keep those settings. Never expose the token
while parking or restoring it; keep it encrypted under the approved SOPS
policy.

1. Record the gateway ID and compute its exact source data path with the
   read-only snippet above. Remove the gateway entry from YAML. If it was the
   only Cloudflare consumer, remove or securely park the top-level Cloudflare
   settings as described above; do not leave an orphaned Cloudflare setting in
   the plaintext config or decrypted secrets. Run `validate`, review
   `preview`, and apply the change. This stops and removes the source gateway
   while preserving its data and metadata.
2. On the source Host, back up the exact
   `/var/lib/sub2api-host/runtime/data/<dataToken>` directory. Verify that the
   archive can be listed and restored before relying on it. Do not use an ad
   hoc Docker command to copy or migrate the data.
3. On the destination Host, pre-stage the backup at the same derived
   `/var/lib/sub2api-host/runtime/data/<dataToken>` path before adding the
   gateway target. Use the approved root-only transfer procedure, and ensure
   the destination path and its state-root parents are real directories owned
   by root with mode `0700`; do not replace any component with a symlink. The
   Host runtime's path hardening rejects unsafe ownership, permissions, links,
   or path components.
4. Verify the restored bytes and path ownership. Add the same gateway ID back
   to YAML with the new `server`. If it was the only Cloudflare consumer,
   restore `cloudflare.zoneId` and the encrypted SOPS-managed
   `cloudflare.apiToken`; if other consumers remain, retain their existing
   settings. Keep the secret encrypted and do not print or expose it. Run
   `validate`, review `preview`, and apply. The deterministic data token then
   points the new container at the restored directory and DNS publication is
   recreated.

This is an operator backup/restore procedure, not automatic cross-server
migration. Expect downtime and absent DNS publication between steps 1 and 4.

Whole Host retirement is a separate lifecycle. It preserves Host data but marks
the Host retired; a retired Host cannot reconcile or re-add a gateway through
normal YAML changes. Reuse requires a separate approved adoption/recovery
workflow, not merely re-adding the gateway ID.

The configured `gmwallet/epusdt:v2.0.0` release is covered by native Linux
amd64 and arm64 CI compatibility checks for the deployment contract. Those
checks cover image/platform and runtime assumptions; they do not perform
wallet, chain, payment, callback, or production-network transactions.
