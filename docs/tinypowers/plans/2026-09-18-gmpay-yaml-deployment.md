# GM Pay YAML Deployment Implementation Plan

**Source:** User-confirmed scope in this session; current configuration and runtime evidence in `internal/environment/environment.go`, `internal/program/program.go`, `internal/hostcontract/contract.go`, `internal/hostprovider/provider.go`, and `internal/hostruntime/reconcile.go`; upstream GM Pay `v2.0.0` Docker documentation and image.
**Goal:** Let operators declare one GM Pay instance in `environments/<name>/config.yaml`, choose its server and hostname, and have the existing Host/Traefik/Cloudflare machinery deploy, update, observe, and remove the instance without managing its persistent data as disposable state.

## Current State And Gaps

- `servers` already defines SSH placement and public/internal addresses. `apps.<id>.servers` maps stateless Sub2API applications onto Host resources, while Docker PostgreSQL and Redis use one explicit `server`.
- `apps` is not a generic container interface: it requires PostgreSQL/Redis and uses blue/green slots with `/app/data`. GM Pay instead needs one active scanner/worker instance, a `/data` mount, and no concurrent old/new processes.
- Host targets currently contain Apps, local data services, the reverse proxy, SOCKS, and tunnel connectors. They have no stateful payment-gateway target or observation.
- Cloudflare DNS registration currently iterates only App public access.
- App images require immutable digests. GM Pay deployment will intentionally accept an explicit release tag such as `gmwallet/epusdt:v2.0.0`; untagged and floating `latest` references remain invalid.
- Scope excludes Sub2API payment-provider configuration, GM Pay wallet/RPC/API-key setup, automatic cross-server data migration, and automatic deletion of GM Pay data.

## YAML Contract

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

- `paymentGateways` is optional; existing environments remain valid without it.
- Gateway IDs use the existing lowercase resource ID rules.
- Initial implementation supports only `type: gmpay`, one `server`, and Cloudflare DNS through that server's public addresses.
- `hostname` must be globally unique across Apps and payment gateways.
- Accepted images are an explicit `vMAJOR.MINOR.PATCH` tag, the same tag pinned with `@sha256:...`, or an immutable digest reference. Untagged references and `latest` are rejected.
- A changed image string is an explicit upgrade. Reuse of the same tag does not refresh or redeploy an upstream-mutated tag.
- Runtime paths, Docker names, network aliases, ports, and volume paths are derived from environment/server/gateway identity and are not YAML fields.

## Shared Constraints

- Preserve all existing YAML, App, data-service, Host import, Provider, and release behavior when `paymentGateways` is absent.
- Preserve the fixed system-OpenSSH Provider transport and Nix-owned Host runtime; this work adds target data and Host reconciliation only.
- Exactly one GM Pay container may run for a gateway. An upgrade may keep a stopped rollback container, but old and new versions must never scan or process callbacks concurrently.
- GM Pay data is outside the Nix store and survives container replacement, gateway removal, failed upgrades, and Host runtime upgrades.
- Deployment creates no GM Pay administrator, wallet, RPC node, merchant key, or Sub2API provider.
- Local Go checks follow `AGENTS.md`: explicit packages, `GOMAXPROCS=2`, `-p=1`, `-parallel=1`; Environment Program and heavyweight integration remain CI-only where required.

## Tasks

### Task 1: Accept And Validate The YAML Interface

**Owns:** `internal/environment/environment.go`, `internal/environment/environment_test.go`, and focused CLI validation fixtures.
**Depends on:** none.
**Produces:** A strict, normalized `PaymentGateways map[string]PaymentGateway` configuration interface.
**Preserve:** Existing fixture YAML must parse and validate unchanged; capture that baseline before adding the optional map.

**Requirements:**

- Add strict YAML types for `PaymentGateway` and its singleton public-access settings without reusing App-only `PublicAccess.Servers` semantics.
- Validate required `type`, `server`, `hostname`, `image`, and `publicAccess` fields and reject unknown/null fields.
- Require `type: gmpay`, an existing server, a server public address for Cloudflare `connectBy: publicAddress`, and unique hostnames across Apps and gateways.
- Accept `gmwallet/epusdt:v2.0.0`, version-tag-plus-digest, and digest-only references; reject `latest`, untagged images, malformed tags, uppercase/noncanonical hostnames, duplicate IDs, and missing servers.
- Count gateway Cloudflare use when validating `cloudflare.zoneId` and encrypted `cloudflare.apiToken`.
- Do not add gateway secrets in this phase.

**Acceptance:** Table tests prove valid default/example shapes and every rejection above; existing environment tests stay green.

### Task 2: Extend The Host Contract And Provider Schema

**Owns:** `internal/hostcontract/contract.go`, `internal/hostcontract/contract_test.go`, `internal/hostprovider/provider.go`, and focused Provider schema/check tests.
**Depends on:** Task 1 because the Host contract consumes its accepted gateway fields.
**Produces:** Stable `PaymentGatewayTarget` and `PaymentGatewayObservation` interfaces carried by Host requests, state inspection, Provider checks, and schema output.

**Requirements:**

- Add `PaymentGateways []PaymentGatewayTarget` to `hostcontract.Target` with only `id`, `type`, `image`, and `hostname`; port, mount, route, and readiness details remain hidden in the GM Pay runtime adapter.
- Add gateway observations containing ID, active image reference, and readiness.
- Apply independent contract validation for IDs, `gmpay`, hostnames, and accepted image-reference forms.
- Update Provider shape validation and Pulumi schema consistently; unknown fields and invalid gateway targets fail during Check rather than at mutation time.
- Include gateways in canonical target revision hashing, equality, redaction, state observation, and import/replay tests without adding secrets.

**Acceptance:** Contract round-trip and Provider Check tests show exact gateway preservation, changed-tag revision changes, deterministic ordering, malformed-input rejection, and no regression to existing schemas.

### Task 3: Map Placement And Public DNS Into The Pulumi Graph

**Owns:** `internal/program/program.go`, `internal/program/program_test.go`, `cmd/sub2api-environment/main_test.go`, and Cloudflare graph assertions that consume existing resource adapters.
**Depends on:** Tasks 1 and 2 because it maps validated YAML into the accepted Host contract.
**Produces:** Each gateway appears in exactly one Host target, and its hostname resolves to that Host's declared public addresses.

**Requirements:**

- Sort gateway IDs before graph construction.
- Add a gateway only to `paymentGateways.<id>.server`; unrelated Hosts receive no gateway target.
- Keep the reverse proxy on the selected Host and create A/AAAA records for the gateway hostname after that Host reconciles successfully.
- Include gateway hostnames in global duplicate detection and deterministic Pulumi resource naming.
- A tag or hostname change updates only the selected Host and associated DNS resource; adding a gateway must not change App placement or data-service ownership.
- Removing or moving a gateway does not claim to migrate data. A server move creates a fresh destination data root while the source Host preserves its old root; document backup/restore as an operator prerequisite.

**Acceptance:** Pulumi mock graph tests prove singleton placement, exact DNS addresses/dependencies, deterministic ordering, changed-tag inputs, absent-gateway compatibility, and no gateway leakage to other Hosts.

### Task 4: Implement The Single-Instance GM Pay Runtime Adapter

**Owns:** `internal/hostruntime/reconcile.go`, related Host runtime state/path helpers, and focused runtime tests in `internal/hostruntime/*_test.go`.
**Depends on:** Task 2 because it consumes the final Host target and observation interfaces.
**Produces:** Idempotent create, inspect, update, rollback, remove-preserve-data, and replay behavior for one GM Pay container.
**Preserve:** Before changing inventory validation, prove existing App/local-data/proxy inventories still round-trip and reconcile.

**Requirements:**

- Derive a collision-resistant gateway token, stable Docker name, shared-network alias, route artifact, and persistent path beneath the existing Host state root.
- Run the official image with `EPUSDT_CONFIG=/data/.env`, mount the derived data root at `/data`, use `--restart unless-stopped`, join the managed Host network, expose no host port, and route Traefik to container port `8000`.
- Treat a successful HTTP response from the GM Pay root/install surface as runtime readiness; deployment must complete while the first-run wizard is still pending and must not synthesize `.env`.
- Explicitly pull only for first creation or when the declared image string changes. Unrelated Host revisions and repeated reconciliation of the same tag must not pull or restart the gateway.
- On upgrade, pull before interruption; stop the old container before starting the new one; retain the stopped old container/image as rollback material until the new container is ready. Never run both versions concurrently against `/data`.
- If new startup/readiness fails, remove the failed container, restore/start the old container, restore its route and inventory, and return a failed operation. Journal/replay must recover each interruption point without starting duplicate workers.
- On removal, remove the route before stopping/removing the container, preserve the data directory, and retain enough owned metadata to reject foreign takeover and safely re-add the same gateway.
- Reject unknown pre-existing containers/routes/paths and writable/symlink escapes under the managed state root using existing ownership rules.

**Acceptance:** Fake-runner tests prove exact Docker argv, no published port, fixed mount/env/network, create idempotency, tag upgrade, same-tag no-op, pull failure, startup failure rollback, readiness failure rollback, response-loss replay at every mutating step, route-before-publication/removal ordering, observation accuracy, drift/conflict rejection, and preserved data on removal.

### Task 5: Add Deterministic End-To-End Runtime Evidence

**Owns:** `internal/integration/providerruntime/` fixtures/tests and only the CI fixture wiring needed to exercise the new Host contract.
**Depends on:** Tasks 3 and 4 because it verifies their combined target and runtime behavior.
**Produces:** A repeatable non-production integration scenario covering YAML-to-Provider-to-Host reconciliation.

**Requirements:**

- Extend the scripted Provider Runtime fixture with one gateway assigned to one of at least two Hosts and assert the other Host never receives or mutates it.
- Use a repository-owned fixture image/service that implements the GM Pay runtime contract (`/data`, port 8000, install-ready HTTP root) so lifecycle tests do not depend on Docker Hub availability.
- Exercise first create, no-op replay, `v2.0.0` to `v2.0.1` tag change, forced failed readiness with old-container restoration, final successful update, and removal with data preservation.
- Keep Provider SSH transport assertions unchanged: only fixed Nix-profile `probe` and `stdio` commands cross SSH.

**Acceptance:** The focused Provider Runtime package and exact CI evidence selector pass with trace assertions for placement, mutation order, rollback, and persisted bytes.

### Task 6: Verify The Official GM Pay Image Contract On Both Host Architectures

**Owns:** a focused script under `scripts/tests/`, its unit/static tests, and a bounded CI job or matrix in `.github/workflows/ci.yml`.
**Depends on:** Task 4 because it verifies the runtime assumptions encoded by the adapter.
**Produces:** External compatibility evidence for the exact configured GM Pay release tag, separate from deterministic runtime tests.

**Requirements:**

- On native x86_64 and arm64 Linux runners, pull `gmwallet/epusdt:v2.0.0`, record the resolved platform digest, mount a temporary `/data`, set `EPUSDT_CONFIG=/data/.env`, and start without publishing a public host port.
- Verify only deployment assumptions: one process starts, HTTP install/root surface becomes reachable, writes under `/data` survive container replacement, and the image does not require a second concurrent worker.
- Do not perform wallet, chain, payment, callback, or production-network transactions.
- Fail closed if the tag loses either platform or no longer satisfies the declared volume/listen/readiness contract.

**Acceptance:** Both native architecture jobs pass for `v2.0.0`; deterministic repository fixtures remain the source of rollback/replay evidence when Docker Hub is unavailable.

## Verification And Closure

- Run serial focused tests for `internal/environment`, `internal/hostcontract`, `internal/hostprovider`, `internal/hostruntime`, and affected CLI packages under repository limits.
- Keep Environment Program, `internal/program`, Engine Graph, Provider Import, and real Docker/network integration in their existing CI gates unless the required prebuilt binaries and explicit authorization are present.
- Parse all workflow YAML, run `bash -n` over embedded shell blocks and new scripts, compile Python helpers where applicable, and run `git diff --check`.
- Update `README.md` and `Pulumi.production.example.yaml` with the accepted YAML. Add an operator section covering first-run browser setup, the derived data path, version-tag upgrades, same-tag non-refresh semantics, backup/restore before server moves, and data preservation on removal.
- Final CI acceptance requires exact-commit Pull-Request CI, Provider Runtime/SSH, Engine Graph, Provider Import, Resource Comparison, and native x86_64/arm64 GM Pay compatibility evidence. No production Pulumi or Docker operation is part of implementation verification.

## Acceptance Summary

The feature is complete when this declaration alone is sufficient to place and publish GM Pay:

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

and evidence proves that it creates one container on `api-one`, publishes only `pay.example.com`, preserves `/data`, upgrades only when the YAML image reference changes, rolls back a failed upgrade without concurrent workers, and leaves all existing environments unchanged when `paymentGateways` is absent.
