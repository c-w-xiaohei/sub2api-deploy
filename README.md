# Sub2API Environment Controller

`sub2api-deploy` is a controller-side thin wrapper around standard Pulumi
operations. Pulumi remains the only graph, checkpoint, retry, and state engine:
the CLI does not store a plan or run a removal workflow for you.

## Capability Boundary

Cross-Host Docker PostgreSQL and Redis are implementation-present and
CI-evidence-pending: a data Host derives its admission policy from App
placement, and the Program orders data admission, App readiness, then
Cloudflare publication. Use the workflow below, but do not treat this as
production-proven until the Task 4 exact-SHA gates pass. Local data remains
supported too. Neon, MicroSocks, Tunnel Connector, production migration/cutover,
and a published production release remain separate gaps. This does not claim
that the original 001 product is complete.

## Environment Input

Keep plaintext desired configuration in `environments/production/config.yaml`.
Keep `environments/production/secrets.yaml` encrypted with SOPS; it contains
the Pulumi passphrase, revision key, service admin passwords, and only each
App's scoped credentials. Do not put a firewall or allowlist field in either
file: the data Host derives source admission from data ownership and App
placement.

```yaml
# environments/production/config.yaml
version: 1
reverseProxy:
  image: traefik@sha256:1111111111111111111111111111111111111111111111111111111111111111
  acmeEmail: ops@example.com
servers:
  data-one:
    sshAlias: production-data-one
    addresses:
      internal: {ipv4: 10.42.0.10}
  api-one:
    sshAlias: production-api-one
    addresses:
      public: {ipv4: 203.0.113.21}
      internal: {ipv4: 10.42.0.21}
  api-two:
    sshAlias: production-api-two
    addresses:
      public: {ipv4: 203.0.113.22}
      internal: {ipv4: 10.42.0.22}
postgres:
  primary: {type: docker, server: data-one}
redis:
  primary: {type: docker, server: data-one}
apps:
  api:
    hostname: api.example.com
    image: ghcr.io/example/sub2api@sha256:2222222222222222222222222222222222222222222222222222222222
    initialAdminEmail: admin@example.com
    readinessPath: /healthz
    drainTimeout: 30s
    servers: [api-one, api-two]
    postgres: {name: primary, database: sub2api}
    redis: {name: primary, database: 0}
    publicAccess:
      type: cloudflare
      servers: [api-one, api-two]
      cloudflare: {mode: dns, connectBy: publicAddress}
cloudflare:
  zoneId: replace-with-zone-id
```

The App refers to the ordinary `primary` service names, not IP addresses. The
derived runtime uses the data Host's internal address. A corresponding SOPS
plaintext editing shape is:

```yaml
# environments/production/secrets.yaml, encrypt this file with SOPS
pulumiPassphrase: replace-before-encrypting
revisionKey: MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=
apps:
  api:
    initialAdminPassword: replace-before-encrypting
    jwtSecret: replace-before-encrypting
    totpEncryptionKey: replace-before-encrypting
    postgres: {username: sub2api, password: replace-before-encrypting}
    redis: {username: default, password: replace-before-encrypting}
postgres:
  primary: {adminPassword: replace-before-encrypting}
redis:
  primary: {adminPassword: replace-before-encrypting}
reverseProxy: {dnsChallengeToken: replace-before-encrypting}
cloudflare: {apiToken: replace-before-encrypting}
```

Encrypt that file with the repository's approved SOPS recipient policy, then
run `sub2api-deploy validate production`. The CLI decrypts it only for the
short-lived staged Pulumi stack; do not commit plaintext secrets.

## Addition And Removal

For an addition, change `config.yaml` to its final additive placement and run:

```bash
sub2api-deploy pulumi production preview
sub2api-deploy pulumi production up --yes
```

This is one normal Pulumi update: it adds data-source admission before App
readiness and publication. A mixed move is also additive first: write an
intermediate configuration with the old and destination App Hosts, run the
full `preview` and `up`, then write the final configuration and perform the
removal checkpoints below.

For a pure removal, first write the final canonical config. Complete each
command before starting the next; every command is safely rerunnable through
Pulumi checkpoints and the existing Host journal.

```bash
# 1. Detach each affected publication record.
sub2api-deploy pulumi production up --target=urn:pulumi:production::sub2api-environment::cloudflare:index/dnsRecord:DnsRecord::dns-api-api-one-A --yes
sub2api-deploy pulumi production up --target=urn:pulumi:production::sub2api-environment::cloudflare:index/dnsRecord:DnsRecord::dns-api-api-two-A --yes

# 2. Stop each old consumer in reverse data/App DAG order.
sub2api-deploy pulumi production up --target=urn:pulumi:production::sub2api-environment::sub2api-host:index:Host::host-api-two --yes
sub2api-deploy pulumi production up --target=urn:pulumi:production::sub2api-environment::sub2api-host:index:Host::host-api-one --yes

# 3. Reconcile the remaining graph; the data Host can now revoke old sources.
sub2api-deploy pulumi production up --yes
```

The wrapper preserves generic safe `--target` support, but each target must be
an exact current-stack/current-project URN:
`urn:pulumi:production::sub2api-environment::<package>:<module>:<member>::<logical-name>`.
Malformed, empty, control-character, other-stack, and other-project targets
fail closed. This removal path documents only the actual Program resources:
Cloudflare DNS `A`/`AAAA` records named `dns-<app>-<server>-A|AAAA` and Host
resources named `host-<server-key>`. Do not use `--target` as a substitute for
the full final checkpoint. These stages are not atomic cross-Host transactions;
inspect the checkpoint and retry the same normal command after a failure.
Do not replace the targeted stages with a direct full `up` from the
pre-removal checkpoint: that can revoke the data allowlist before its consumer
Host has been detached.

To retire a server, keep it configured while detaching its publication and
removing its Apps by the sequence above. Only after its target is drained,
remove the server from `config.yaml`, run a full `up`, and complete the
existing preserve-data retirement approval. Do not remove a data Host while
any consumer still refers to its service.

## Production Prerequisites

Production operators must provide root or non-interactive `sudo` access needed
by the Host lifecycle, rootful Docker, and nftables support. Internal Host
addresses must be genuine routed private connectivity between the Hosts.
The implementation installs an owned nftables policy before Docker DNAT and
uses internal-network anti-spoofing assumptions to admit only derived consumer
sources. These are deployment prerequisites, not inferred guarantees: verify
routing, source-address integrity, Docker behavior, nftables availability, and
least-privilege sudo policy in the production environment before cutover.

## Managed Import

```bash
sub2api-deploy pulumi production import sub2api-host:index:Host host-edge edge
```

The release inventory is `bin/sub2api-deploy`, `bin/pulumi-program`,
`bin/pulumi-resource-sub2api-host`, Pulumi wrappers, Host Runtime artifacts for
Linux amd64 and arm64, and public Pulumi and Go metadata files.
