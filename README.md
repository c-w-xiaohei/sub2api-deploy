# Sub2API VPS Deployment

This is a direct Pulumi project for one VPS. `pulumi up` runs on the VPS itself,
uses `command.local.Command`, and invokes the local Docker Compose CLI. It does
not use SST, a remote Docker daemon, the Pulumi Docker provider, a Tunnel,
Kubernetes, weighted canaries, or a hosted deployment bridge.

## Prerequisites

- Docker Engine
- Docker Compose v2
- Pulumi CLI
- Node.js or Bun
- An active Cloudflare zone and a scoped Cloudflare API token
- Credentials for the selected PostgreSQL and Redis services

The pinned Sub2API Compose baseline is kept in `compose/upstream.yml`. The
deployment adds only the slot, edge, data-mode, and local runtime overrides.
The default edge path is proxied Cloudflare DNS to the public origin, Traefik
DNS-01 certificates, and Cloudflare Full (strict).

## Basic setup

Install dependencies and initialize the Pulumi stack on the VPS:

```bash
npm install
cp Pulumi.production.example.yaml Pulumi.production.yaml
${EDITOR:-vi} Pulumi.production.yaml
pulumi stack init production
```

Edit the ordinary values directly in `Pulumi.production.yaml`: the domain,
origin IP, explicit Cloudflare zone ID, ACME email, immutable application image,
application probe path, and PostgreSQL/Redis modes. The example file defaults
to local Docker PostgreSQL and Redis. `Pulumi.production.yaml` is ignored by
Git; only `Pulumi.production.example.yaml` is committed.

Do not type plaintext credentials into the YAML file. Let Pulumi encrypt and
write each secret required by the selected modes. For the default
`docker/docker` configuration:

```bash
pulumi config set --secret cloudflareApiToken '...'
pulumi config set --secret postgresPassword '...'
pulumi config set --secret redisPassword '...'
```

Optional Sub2API bootstrap credentials are set the same way:

```bash
pulumi config set --secret adminPassword '...'
pulumi config set --secret jwtSecret '...'
pulumi config set --secret totpEncryptionKey '...'
```

Review the deployment plan before applying it:

```bash
pulumi preview
pulumi up
```

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

### Sub2API native configuration

This project intentionally does not mirror every Sub2API environment variable
into Pulumi. The pinned upstream Compose file remains the source of truth for
Sub2API configuration semantics and defaults. If a native setting is not
explicitly exposed by this project, Sub2API receives the upstream default.

Do not edit `runtime/runtime.env` by hand. Pulumi generates that file during
infrastructure reconciliation and may replace it. When a deployment needs a
native setting that is not exposed yet, add it explicitly rather than creating
a second generic configuration system:

1. Add an optional input and default to `src/config.ts`.
2. Add the value to `runtimePayload` in `src/index.ts` using the upstream
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
| Neon `existing` | `postgresMode: neon`, `neonResourceMode: existing`, `neonHost` and connection fields | `neonPassword` |
| Neon `create` | `postgresMode: neon`, `neonResourceMode: create`, `neonProjectId`, `neonBranchId` | `neonApiToken` |
| Redis `docker` | `redisMode: docker` | `redisPassword` |
| Upstash `existing` | `redisMode: upstash`, `upstashResourceMode: existing`, `upstashHost`, `upstashPort` and connection fields | `upstashPassword` |
| Upstash `create` | `redisMode: upstash`, `upstashResourceMode: create`, `upstashEmail`, `upstashDatabaseName`, `upstashRegion` | `upstashApiKey` |

For example, when connecting to an existing Neon database, edit the PostgreSQL
fields in the YAML file and run only:

```bash
pulumi config set --secret neonPassword '...'
```

`existing` connects to a data service without managing its lifecycle. `create`
allows Pulumi to manage the selected Neon or Upstash resource and therefore
requires its provider API credential. The application receives the upstream
split variables (`DATABASE_*` and `REDIS_*`), not a URL. Neon uses
`DATABASE_SSLMODE=require`; Upstash uses Redis TLS.

The first deployment starts blue with `AUTO_SETUP=true`, probes it, then keeps
the runtime configuration at `AUTO_SETUP=false`. The upstream setup may need a
PostgreSQL maintenance connection and `CREATE DATABASE`; a Neon role without
those permissions must be prepared separately or setup must be completed by
the upstream supported path.

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
npm test
npm run build
bash -n scripts/*.sh
bash scripts/validate-compose.sh
```

Do not run `pulumi up` against a real stack as part of offline verification.
