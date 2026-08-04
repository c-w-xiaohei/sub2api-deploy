# Sub2API VPS Deployment

This deployment manages one Host per Pulumi Stack. A Host has one shared Edge
and an arbitrary map of isolated Sites. The Pulumi program is Go and is loaded
from the prebuilt `bin/pulumi-program`; `pulumi preview` and `pulumi up` run on
the VPS itself, use `command.local.Command`, and invoke the local Docker Compose
CLI. This project does not use SST, a remote Docker daemon, the Pulumi Docker
provider, a Tunnel, Kubernetes, weighted canaries, or a hosted deployment
bridge.

## Release Bundle

Download the release bundle for the VPS architecture. Each release contains
the matching Linux `pulumi-program` binary, the bundled Pulumi runtime helpers,
and the complete Edge/Site/host lifecycle files:

```bash
VERSION=v0.1.7
ARCH=amd64 # use arm64 for an ARM VPS
gh release download "$VERSION" \
  --pattern "sub2api-vps-deploy-${VERSION}-linux-${ARCH}.tar.gz" \
  --dir /tmp
tar -xzf "/tmp/sub2api-vps-deploy-${VERSION}-linux-${ARCH}.tar.gz"
cd "sub2api-vps-deploy-${VERSION}-linux-${ARCH}"
npm ci
cp Pulumi.production.example.yaml Pulumi.production.yaml
${EDITOR:-vi} Pulumi.production.yaml
./bin/pulumi stack init production
```

The released archive names are
`sub2api-vps-deploy-${VERSION}-linux-amd64.tar.gz` and
`sub2api-vps-deploy-${VERSION}-linux-arm64.tar.gz`. Their checksum files are
the corresponding `.tar.gz.sha256` files. The bundle includes `bin/go`, a
metadata-only compatibility shim used by Pulumi's Go language host, and
`bin/pulumi`, which invokes the installed Pulumi CLI with the bundle on
`PATH`. Go itself is not required on the VPS.

Before using a real Stack, run the offline checks on a build machine. They are
not production operations:

```bash
go test ./...
go vet ./...
go build -o /tmp/sub2api-pulumi-go ./infra
npm test
npm run build
```

## Host And Sites

The public Pulumi configuration exposes exactly four structured objects:

| Object | Purpose |
| --- | --- |
| `edge` | The shared public edge for this Host |
| `sites` | The map of independent Site declarations |
| `edgeSecrets` | Encrypted credentials used only by the shared Edge |
| `siteSecrets` | Encrypted credentials keyed by Site ID |

Do not add flat top-level keys. Runtime directories, Compose project names,
network names, route paths, blue/green slots, and network aliases are derived
from the Host/Site declarations and are not configuration inputs. `resourcePrefix`
is also omitted from the ordinary example so the Site ID remains the naming
boundary by default.

One Stack is one Host. The shared Edge owns public ports `80` and `443`, ACME,
the `sub2api-edge` Compose project, Cloudflare Edge resources, and sing-box TCP
passthrough. Each Site owns its application, data-mode wiring, runtime, and
route in an isolated layout. A Site's domain must be unique on the Host.

Traefik is the sole public listener on ports `80` and `443`. HTTPS requests for
each configured Site domain terminate at Traefik and route to that Site's
active blue/green slot. TLS with SNI `www.cloudflare.com` passes through
unchanged to sing-box at `host.docker.internal:8443`.

Before the first `pulumi up`, migrate the existing sing-box listener from
`0.0.0.0:443` to host port `8443` and verify it is listening there. This is an
explicit host operation outside Pulumi; the deployment does not edit, stop, or
restart sing-box. Do not start the Edge while sing-box still owns public `443`.

## Structured Configuration

Copy `Pulumi.production.example.yaml` and edit ordinary values in its `edge`
and `sites` objects. Every Site requires `domain`, an immutable `image` in the
form `name@sha256:<64 lowercase hexadecimal characters>`, `adminEmail`, and an
application `appProbePath` other than `/health`. Site images must remain
immutable digests. The shared Edge Traefik image is the intentional exception:
`edge.traefikImage` may use a stable version tag such as `traefik:v3.3.3`.
Database and Redis modes are selected inside each Site. The current host
remains resource-constrained even for two low-traffic Sites.

The `edge` object contains `originIp`, `cloudflareZoneId`, `acmeEmail`,
`traefikImage`, and `singBox.serverName`/`target`. A Site may contain
`database.mode` (`docker` or `neon`), `database.resourceMode`, `redis.mode`
(`docker` or `upstash`), `redis.resourceMode`, and the ordinary connection
fields required by the selected external mode. Do not configure generated
runtime, project, network, route, slot, or alias values.

Do not put credentials, tokens, DSNs, or passwords in YAML. Set the encrypted
secret objects as JSON supplied by a protected shell environment or secure
input. Do not print those JSON values in shell history or logs:

```bash
./bin/pulumi config set --secret edgeSecrets "$EDGE_SECRETS_JSON"
./bin/pulumi config set --secret siteSecrets "$SITE_SECRETS_JSON"
```

`edgeSecrets` requires `cloudflareApiToken`. Every `siteSecrets.<siteId>`
requires `adminPassword`, `jwtSecret`, and `totpEncryptionKey`. Add only the
mode-specific fields below:

| Site mode | Ordinary Site fields | Encrypted `siteSecrets.<siteId>` field |
| --- | --- | --- |
| PostgreSQL `docker` | `database.mode: docker` | `database.password` |
| Neon `existing` | `database.mode: neon`, `database.resourceMode: existing` | `database.dsn` |
| Neon `create` | `database.mode: neon`, `database.resourceMode: create` | `database.apiToken` |
| Redis `docker` | `redis.mode: docker` | `redis.password` |
| Upstash `existing` | `redis.mode: upstash`, `redis.resourceMode: existing`, `redis.endpoint` | `redis.password` |
| Upstash `create` | `redis.mode: upstash`, `redis.resourceMode: create`, optional `redis.region` | `redis.apiKey` |

For `neon` and `upstash`, `resourceMode: existing` does not manage the
provider resource lifecycle. `create` lets Pulumi manage the selected
namespaced resource. Managed Neon and Upstash resources are protected and
retained when a Site is removed or renamed. Such changes fail closed; do not
unprotect or manually delete provider resources as a workaround. Use a
separately approved retirement workflow if data retirement is actually needed.

Inspect state without `--show-secrets`, then review the complete change before
applying it:

```bash
./bin/pulumi stack export > /tmp/sub2api-stack.json
./bin/pulumi preview
./bin/pulumi up
```

## Adding A Site

Adding a Site is one Stack configuration change. Add the new Site entry to the
existing `sites` object, add its matching `siteSecrets` entry with an encrypted
object command, and review one preview before applying one update:

```bash
${EDITOR:-vi} Pulumi.production.yaml
./bin/pulumi config set --secret siteSecrets "$SITE_SECRETS_JSON"
./bin/pulumi preview
./bin/pulumi up
```

Do not duplicate or edit the release bundle for each Site. The shared Edge and
Site templates/scripts are assembled once and the Stack derives the additional
Site runtime, project, route, slot, and alias values. Site bootstrap and release
mutations are serialized: do not initiate bootstrap or release for multiple
Sites concurrently. Sites with the same image digest share Docker layers, but
two low-traffic Sites on the current Host are still resource-constrained.

## Code2 Adoption Boundary

Adopting the existing single-Site layout into the Host/Site architecture is a
distinct, separately approved maintenance-window operation. Follow
`docs/migrations/single-site-to-multi-site.md`; it is not ordinary Pulumi
configuration and does not migrate Cloudflare, Neon, Upstash, PostgreSQL,
Redis, volumes, or runtime data.

Do not add `code3` until code2 adoption has completed and its state preview,
reviewed update, direct-origin/public health, sing-box passthrough evidence,
and journal retirement are complete. Keep the code2 application image digest
unchanged throughout architecture adoption. Application image updates are a
later, separately reviewed operation after adoption evidence is complete.

## Image Updates And Rollback

Change the relevant Site's `image` to another immutable `@sha256:` digest and
run a reviewed `pulumi preview` followed by `pulumi up`. Pulumi starts the
inactive blue/green slot, waits for container health and the configured
application probe, atomically updates that Site's route, validates public
HTTPS, drains briefly, and stops the old slot. This is simple slot switching,
not weighted canary deployment. `scripts/rollback-slot.sh` uses the recorded
prior digest and slot; it does not roll back database migrations or Redis
state.

Changing a database or Redis mode after first deployment does not migrate data.
It fails before runtime rewrite, local service mutation, slot start, or route
change. Perform an explicit, approved data migration outside ordinary `pulumi
up`.

`pulumi destroy` is not a production data recovery operation. Do not use it
against retained database resources, volumes, or a production Stack without an
explicit backup and recovery plan. This documentation provides no manual
deletion procedure for Host state, Docker objects, volumes, databases, Redis,
or provider resources.

## Offline Verification

The project can be checked without contacting Cloudflare, Neon, Upstash, a VPS,
or creating cloud resources:

```bash
go test ./...
go vet ./...
go build -o /tmp/sub2api-pulumi-go ./infra
npm test
npm run build
bash -n scripts/*.sh
bash scripts/validate-compose.sh
```

Do not run `pulumi up` against a real Stack as part of offline verification.
