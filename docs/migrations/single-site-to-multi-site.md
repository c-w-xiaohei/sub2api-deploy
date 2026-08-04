# Single-Site Code2 Adoption

This procedure is a separately approved production maintenance-window operation
for adopting the existing single-Site layout as `code2` on the one-Host
architecture. It is not ordinary Pulumi configuration and it does not migrate
Cloudflare, Neon, Upstash, PostgreSQL, Redis, volumes, or `runtime/data`.

The code2 application image digest must remain unchanged for the entire
architecture adoption. Application updates come later, after adoption evidence
and journal retirement, as a separate reviewed change.

If `runtime/oidc.env` exists, the first normal code2 reconcile is fail-closed:
an explicitly supplied encrypted `siteSecrets.code2.appEnv` must contain every
legacy key with the identical value before managed `runtime/app.env` is written.
The legacy dotenv is parsed as data, never sourced as shell. Adoption preserves
the legacy file and journals a mode-state backup only when it adds the verified
`postgresMode` and `redisMode` metadata needed by the new lifecycle; rollback
restores that original state.

Before the window, take encrypted backups and a sanitized Pulumi stack export.
Remove credentials, DSNs, tokens, and IDs that are not needed to inspect URN
shape. Review `pulumi preview` manually: existing code2 Cloudflare, Neon, and
Upstash resources must be updates/adoptions, never replacements or deletes.
Managed Neon and Upstash data is protected and retained. Do not unprotect it or
use manual deletion as an adoption workaround. If the preview is rejected or
shows destructive cloud/data changes, do not proceed; escalate through the
approved maintenance/change process.

Run host preflight first with only `code2` configured. It must identify
`runtime/deploy-state.json` and no normalized host state. Confirm the legacy
containers by immutable Compose labels
(`com.docker.compose.project=sub2api` plus the expected service label),
direct-origin health, public application health, and sing-box reachability
before starting.

The command is dry-run by default. It writes no state and performs no Docker
action:

```bash
TRAEFIK_IMAGE=... CLOUDFLARE_API_TOKEN=... ACME_EMAIL=... \
SING_BOX_SERVER_NAME=... SING_BOX_TARGET=... DOMAIN=... ORIGIN_IP=... APP_PROBE_PATH=... \
POSTGRES_MODE=neon REDIS_MODE=docker SING_BOX_VERIFY_COMMAND='approved-sing-box-verification-command' \
bash scripts/adopt-single-site-layout.sh --environment production --site code2
```

During the approved window, run the same command with `--apply`:

```bash
TRAEFIK_IMAGE=... CLOUDFLARE_API_TOKEN=... ACME_EMAIL=... \
SING_BOX_SERVER_NAME=... SING_BOX_TARGET=... DOMAIN=... ORIGIN_IP=... APP_PROBE_PATH=... \
POSTGRES_MODE=neon REDIS_MODE=docker SING_BOX_VERIFY_COMMAND='approved-sing-box-verification-command' \
bash scripts/adopt-single-site-layout.sh --environment production --site code2 --apply
```

`POSTGRES_MODE=neon` and `REDIS_MODE=docker` are verified lifecycle metadata
inputs for the existing code2 placement. They let adoption validate or record
the deployment state without moving or migrating any PostgreSQL or Redis data.

Before the window, prepare the non-mutating preview marker. This records only
the strictly validated non-secret legacy mapping and does not touch Docker,
routes, Edge files, ACME, or runtime data:

```bash
bash scripts/adopt-single-site-layout.sh --environment production --site code2 --prepare-preview
ALLOW_PENDING_LEGACY_PREVIEW=1 ./bin/pulumi preview
```

The environment gate is for this reviewed preview command only. It is not
supplied to lifecycle commands; an ordinary update remains blocked while the
marker is pending. Production preview review remains mandatory and must show no
cloud/data replacement. If that review is not accepted, stop and escalate via
the approved change process. Do not attempt to repair state manually.

After validating all labels, inputs, absent generated Edge files, absent Edge
network/container, and the active legacy slot, apply atomically writes
`runtime/adopt-single-site-layout.journal` at mode `0600` before any persistent
or Docker mutation. Before each writer or Docker action it records a durable
intent, then records observed completion after it succeeds. It stages only
`runtime/edge/edge.env`, static/sing-box files, and `runtime/edge/acme.json`.
Those generated files are removed on rollback only when absent before adoption.
Legacy ACME is copied only to an absent destination and never modified. The
journal state progresses `prepared`, `pending`, `completing`, then `complete`,
recording IDs, route backup, network/container ownership intent, attachment, and
ACME ownership. It contains no secret. It connects only the active code2
application container, writes only `site-code2.yml`, and never copies or mutates
`runtime/data`, volumes, or data services.

Keep the journal while verifying the new Edge, the code2 route, direct-origin
health, public health, and sing-box passthrough. If any check fails, use the
exact supported rollback path:

```bash
bash scripts/adopt-single-site-layout.sh --environment production --site code2 --rollback
```

Rollback needs no credentials or sing-box command. It accepts only a validated
`prepared`, `pending`, `completing`, or `complete` journal with its exact
recoverable host-state pair. It validates IDs and Docker labels before
mutation, stops/removes only journal-owned Edge resources, restores the prior
route/network/ACME destination where applicable, starts and verifies the
journaled legacy Traefik container, and only then removes the code2-only
host-state mapping. If restoration fails, the journal and registry remain for
investigation. A completed journal can be rolled back only while the host
registry is exactly code2.

Do not add `code3` until all of these are complete: code2 adoption, the reviewed
state preview, the reviewed update, direct-origin and public health evidence,
sing-box passthrough evidence, and retirement of the completed adoption journal.
Site bootstrap and release mutations are serialized; do not initiate them
concurrently during or after this boundary.

After archiving direct-origin, public-route, and approved sing-box evidence,
retire the completed journal before adding any Site:

```bash
bash scripts/adopt-single-site-layout.sh --environment production --site code2 --retire-journal
```

Retirement accepts only a completed journal and a completed host mapping whose
Site registry is exactly `code2`; it refuses pending, inconsistent, or
multi-Site state and is never run by Pulumi. There is no cancellation command.

The repository contains a sanitized exported-stack-shape fixture and offline
mock-graph assertions for alias and input-shape regression evidence. They are
not a Pulumi preview or production adoption evidence. The completed mapping is
consumed internally only for code2: its application project and data/deploy-state
paths remain `sub2api` and `runtime`, while later Sites retain normalized
`runtime/sites/<id>` layouts. The mapping is not public configuration. Future
Site removal or rename is fail-closed while managed provider resources remain
protected; use a separately approved retirement workflow rather than
unprotecting or manually deleting data.
