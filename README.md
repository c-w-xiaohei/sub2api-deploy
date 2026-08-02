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

Install dependencies and select the Pulumi stack on the VPS. The basic operator
decisions are the domain, origin IP, explicit zone ID, ACME email, immutable
application image, the two data modes, and credentials for the selected modes:

```bash
npm install
pulumi stack init production
pulumi config set domain sub2api.example.com
pulumi config set originIp 203.0.113.10
pulumi config set cloudflareZoneId replace-with-zone-id
pulumi config set acmeEmail ops@example.com
pulumi config set appProbePath /api/ready
pulumi config set sub2apiImage 'weishaw/sub2api@sha256:IMMUTABLE_DIGEST'
pulumi config set postgresMode docker
pulumi config set redisMode docker
pulumi config set --secret cloudflareApiToken '...'
pulumi config set --secret postgresPassword '...'
pulumi config set --secret redisPassword '...'
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

Every password or token represented by `...` must be set with `pulumi config
set --secret`. The Pulumi program passes secrets only to the local command,
writes `runtime/runtime.env` atomically with mode `0600`, sets Command logging
to `none`, and exports no credentials as stack outputs.

`cloudflareZoneId` is explicit and is never inferred from hostname labels.
`acmeEmail` is a non-secret Pulumi config value. `appProbePath` must be a real
application path and cannot be `/health`, which is only the upstream liveness
check. The direct-origin probe temporarily resolves the origin IP; the normal
post-switch probe follows public DNS without `--resolve`.

The four independent data combinations (`docker/docker`, `neon/docker`,
`docker/upstash`, and `neon/upstash`) use the same `pulumi up` command:

```bash
pulumi config set postgresMode docker && pulumi config set redisMode docker && pulumi up
pulumi config set postgresMode neon && pulumi config set neonHost ep.example.neon.tech && pulumi config set --secret neonPassword '...' && pulumi config set redisMode docker && pulumi up
pulumi config set postgresMode docker && pulumi config set redisMode upstash && pulumi config set upstashHost example.upstash.io && pulumi config set upstashPort 6380 && pulumi config set --secret upstashPassword '...' && pulumi up
pulumi config set postgresMode neon && pulumi config set neonHost ep.example.neon.tech && pulumi config set --secret neonPassword '...' && pulumi config set redisMode upstash && pulumi config set upstashHost example.upstash.io && pulumi config set upstashPort 6380 && pulumi config set --secret upstashPassword '...' && pulumi up
```

Neon and Upstash are existing-resource connection modes. This project does not
silently create or destroy production data resources. The application receives
the upstream split variables (`DATABASE_*` and `REDIS_*`), not a URL. Neon uses
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
