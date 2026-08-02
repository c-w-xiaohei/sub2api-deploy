# Sub2API VPS Deployment

This is a direct Pulumi project for one VPS. The Pulumi program is Go and is
loaded from the prebuilt `bin/pulumi-program`; `pulumi up` runs on the VPS
itself, uses `command.local.Command`, and invokes the local Docker Compose CLI. It does
not use SST, a remote Docker daemon, the Pulumi Docker provider, a Tunnel,
Kubernetes, weighted canaries, or a hosted deployment bridge.

## Prerequisites

- Docker Engine
- Docker Compose v2
- Pulumi CLI
- A release bundle containing the matching Linux `pulumi-program` binary and
  bundled Pulumi runtime helpers
- Node.js or Bun for the shell-invoked `tsx` runtime helpers
- An active Cloudflare zone and a scoped Cloudflare API token
- Credentials for the selected PostgreSQL and Redis services

The pinned Sub2API Compose baseline is kept in `compose/upstream.yml`. The
deployment adds only the slot, edge, data-mode, and local runtime overrides.
The default edge path is proxied Cloudflare DNS to the public origin, Traefik
DNS-01 certificates, and Cloudflare Full (strict).

## Basic setup

Download the release bundle for the VPS architecture, then install the
TypeScript runtime helpers and initialize the Pulumi stack:

```bash
VERSION=v0.1.3
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

`Pulumi.yaml` points to `./bin/pulumi-program`, so the VPS does not compile the
Go program during `pulumi preview` or `pulumi up`. The bundle also includes
`bin/go`, a metadata-only compatibility shim used by Pulumi's Go language host,
and `bin/pulumi`, which invokes the installed Pulumi CLI with the bundle on
`PATH`. Go itself is not required on the VPS. Before using a real stack, run
the offline checks on a build machine:

```bash
go test ./...
go vet ./...
go build -o /tmp/sub2api-pulumi-go ./infra
npm test
npm run build
```

Edit the ordinary values directly in `Pulumi.production.yaml`: the resource
namespace, domain,
origin IP, explicit Cloudflare zone ID, ACME email, immutable application image,
application probe path, and PostgreSQL/Redis modes. The example file defaults
to local Docker PostgreSQL and Redis. `Pulumi.production.yaml` is ignored by
Git; only `Pulumi.production.example.yaml` is committed.

Do not type plaintext credentials into the YAML file. Let Pulumi encrypt and
write each secret required by the selected modes. For the default
`docker/docker` configuration:

```bash
./bin/pulumi config set --secret cloudflareApiToken '...'
./bin/pulumi config set --secret postgresPassword '...'
./bin/pulumi config set --secret redisPassword '...'
```

Optional Sub2API bootstrap credentials are set the same way:

```bash
./bin/pulumi config set --secret adminPassword '...'
./bin/pulumi config set --secret jwtSecret '...'
./bin/pulumi config set --secret totpEncryptionKey '...'
```

Export and inspect the existing state before any production operation. Do not
use `--show-secrets`:

```bash
./bin/pulumi stack export > /tmp/sub2api-stack.json
```

Review a preview against the existing stack before applying it:

```bash
./bin/pulumi preview
./bin/pulumi up
```

## Release Artifacts

Pushing a version tag triggers `.github/workflows/release.yml`. CI verifies the
Go program and runtime helpers, builds Linux `amd64` and `arm64` binaries, and
publishes architecture-specific deployment bundles plus SHA-256 files:

```bash
git tag v0.1.3
git push origin v0.1.3
```

The release workflow never runs `pulumi preview` or `pulumi up`; it only builds
and packages the deployment program.

`pulumi up` runs directly on the target VPS and uses its local Docker Compose
CLI. It does not use a remote deployment bridge.

Changing `postgresMode` or `redisMode` after the first successful deployment
does not migrate data. It fails before runtime.env rewrite, local service
stop/start, slot start, or route change. Perform an explicit data migration
outside ordinary `pulumi up`. For a legacy state without persisted modes, first
verify the existing placement and adopt it once:

```bash
npx --no-install tsx scripts/deployment-mode.ts adopt runtime/deploy-state.json docker docker
```

The adoption command records existing placement; it does not migrate or move
data. States with persisted modes require a migration rather than adoption.

## Advanced configuration

`appProbePath` remains required because `/health` is only the upstream liveness
check. `neonResourceMode` and `upstashResourceMode` retain their `existing` or
`create` behavior. These advanced inputs do not change the basic `pulumi up`
workflow.

Every password, token, or API key must be written with `pulumi config set
--secret`; ordinary values are edited directly in `Pulumi.production.yaml`.
The Pulumi program passes secrets only to the local command, writes
`runtime/runtime.env` atomically with mode `0600`, sets Command logging to
`none`, and exports no credentials as stack outputs.

`cloudflareZoneId` is explicit and is never inferred from hostname labels.
`acmeEmail` is a non-secret Pulumi config value. `appProbePath` must be a real
application path and cannot be `/health`, which is only the upstream liveness
check. The direct-origin probe temporarily resolves the origin IP; the normal
post-switch probe follows public DNS without `--resolve`.

`resourceNamespace` is the naming boundary for one managed Neon/Upstash
resource set. Use a different lowercase namespace, such as `customer-a`, for a
separate stack or environment. It changes cloud resource names; it is not a
Sub2API tenant, schema, or runtime database setting. Treat it as immutable after
managed resources are created; changing it creates a different resource set,
not a data rename.

Managed Neon uses the pinned native Go alpha SDK pseudo-version
`v0.0.0-20241217015548-601a1132b220`, paired with the alpha.1 provider binary.
The Go SDK uses its root package for the provider and its `/resource` package
for `Project`; generated field names such as `Connection_uri` are intentional.
Keep the SDK and provider versions paired and do not use the Neon beta SDK.

### Sub2API native configuration

This project intentionally does not mirror every Sub2API environment variable
into Pulumi. The pinned upstream Compose file remains the source of truth for
Sub2API configuration semantics and defaults. If a native setting is not
explicitly exposed by this project, Sub2API receives the upstream default.

Do not edit `runtime/runtime.env` by hand. Pulumi generates that file during
infrastructure reconciliation and may replace it. When a deployment needs a
native setting that is not exposed yet, add it explicitly rather than creating
a second generic configuration system:

1. Add an optional input and default to `infra/config.go`.
2. Add the value to `runtimePayload` in `infra/main.go` using the upstream
   environment variable name, such as `TZ` or `GATEWAY_*`.
3. Add the ordinary value or `pulumi config set --secret` instruction to
   `Pulumi.production.example.yaml` and this README.
4. Add a focused behavior test, then run the offline verification commands.

Keep deployment-owned values such as database and Redis connection settings,
`AUTO_SETUP`, slot variables, and image selection under this project. Do not
edit `compose/upstream.yml` for a per-deployment setting; it is pinned to an
upstream commit and should only change when the upstream baseline is upgraded.

The four independent data combinations (`docker/docker`, `neon/docker`,
`docker/upstash`, and `neon/upstash`) all use the same `pulumi preview` and
`pulumi up` workflow. Select them by editing ordinary values in
`Pulumi.production.yaml`, then set only the corresponding credentials with
`pulumi config set --secret`:

| Service mode | Ordinary YAML values | Secret config |
| --- | --- | --- |
| PostgreSQL `docker` | `postgresMode: docker` | `postgresPassword` |
| Neon `existing` | `postgresMode: neon`, `neonResourceMode: existing`, optional split connection fields | `neonDsn` or `neonPassword` |
| Neon `create` | `postgresMode: neon`, `neonResourceMode: create`, optional `neonOrgId` | `neonApiToken` |
| Redis `docker` | `redisMode: docker` | `redisPassword` |
| Upstash `existing` | `redisMode: upstash`, `upstashResourceMode: existing`, `upstashHost`, `upstashPort` and connection fields | `upstashPassword` |
| Upstash `create` | `redisMode: upstash`, `upstashResourceMode: create`, `upstashEmail` and optional `upstashDatabaseName`/`upstashRegion` | `upstashApiKey` |

For example, when connecting to an existing Neon database, keep the mode in
the YAML file and provide the DSN as a Pulumi secret:

```bash
./bin/pulumi config set --secret neonDsn 'postgresql://user:password@host:5432/database?sslmode=require'
```

For a fully managed Neon plus Upstash deployment, no Project, Branch, Redis
database, or DSN needs to be created beforehand. Set the modes and provider
credentials; the namespace and default regions supply the resource names:

```bash
./bin/pulumi config set resourceNamespace tenant-a
./bin/pulumi config set postgresMode neon
./bin/pulumi config set neonResourceMode create
./bin/pulumi config set --secret neonApiToken '...'
./bin/pulumi config set redisMode upstash
./bin/pulumi config set upstashResourceMode create
./bin/pulumi config set upstashEmail ops@example.com
./bin/pulumi config set --secret upstashApiKey '...'
./bin/pulumi up
```

The native Neon provider lets Neon choose the project region; set `neonOrgId`
when the API key can access multiple organizations. Set `upstashRegion` when
the default `us-east-1` is not appropriate.

`existing` connects to a data service without managing its lifecycle. `create`
allows Pulumi to manage the selected Neon or Upstash resource. Managed Neon
creates one namespaced Project from `neonApiToken`; Neon provisions its default
Branch, Database, Role, and Endpoint and returns the connection URI. It does
not require manually created Project or Branch IDs.
Managed Upstash creates a namespaced Redis database from `upstashApiKey` and
`upstashEmail`. The application receives the upstream split variables
(`DATABASE_*` and `REDIS_*`), not a URL. Neon uses
`DATABASE_SSLMODE=require`; Upstash uses Redis TLS.

The first deployment starts blue with `AUTO_SETUP=true`, probes it, then keeps
the runtime configuration at `AUTO_SETUP=false`. An external PostgreSQL
connection may need a maintenance connection and `CREATE DATABASE`; a role
without those permissions must be prepared separately or setup must be
completed by the upstream supported path. Managed Neon creates its database
and role before the application starts.

## Image Updates And Rollback

Change `sub2apiImage` to another `@sha256:` digest and run `pulumi up`. Pulumi
starts the inactive blue/green slot, waits for container health and the
configured application probe, atomically updates the Traefik file-provider
route, validates public HTTPS, drains briefly, and stops the old slot. This is
simple slot switching, not weighted canary. `scripts/rollback-slot.sh` uses the
recorded prior digest and slot; it does not roll back database migrations or
Redis state.

Before production use, validate that worker overlap is safe for this Sub2API
version. Database migrations must be backward compatible with the prior image.
SSE and WebSocket connections can be interrupted during the short drain; this
deployment does not promise lossless stream handoff.

`pulumi destroy` is not a production data recovery operation. Do not use it
against retained database resources, volumes, or a production stack without an
explicit backup and recovery plan.

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

Do not run `pulumi up` against a real stack as part of offline verification.
