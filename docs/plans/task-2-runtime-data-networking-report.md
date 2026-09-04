# Task 2 Runtime Data Networking Report

**Status: DONE (all Task 2 source and focused test scope is complete; privileged/live nft and PostgreSQL evidence remains Task 4 CI-only)**

## Final Evidence Gap Closure

- Inventory invariant tests now construct otherwise-valid PostgreSQL and Redis
  local-data objects, including `AppliedRevision`, before independently making
  Config/HBA/ident malformed or missing. Each case therefore fails only for its
  intended security-artifact invariant.
- A full stateful-catalog Reconcile lifecycle now creates a PostgreSQL service
  with an old client, writes a persistent-data sentinel, then changes only the
  client username/database/set while retaining the binding. It proves catalog
  convergence to the target operation, exactly one shell replacement, an HBA
  with the exact new user/database and no old user/source/trust rule, preserved
  data/path tokens and sentinel, removal of old Env/Config/HBA/ident artifacts,
  presence of replacement artifacts, and an immediate ready, non-drifted,
  read-only Inspect.
- PostgreSQL service-removal and retirement fixtures now assert cleanup of the
  old Env, Config, HBA, and ident artifacts while preserving persistent data.

Final evidence-gap RED command:

```text
GOMAXPROCS=2 go test -p=1 ./internal/hostruntime -run '^TestReconcilePostgresClientChangeReplacesShellAndPreservesData$'
```

Result: initially failed `recovery-required` because the new binding fixture had
not supplied an exact owned nft-table observation. After modeling that required
external state in the test fixture, it exposed no production behavior defect.

Final evidence-gap GREEN command:

```text
GOMAXPROCS=2 go test -p=1 ./internal/hostruntime -run '^(TestPostgresHasExplicitScramConfigAndHBAArtifacts|TestReconcilePostgresClientChangeReplacesShellAndPreservesData|TestLocalDataRemovalRestoresMetadataAndRejectsDifferentTypeBeforeMutation|TestRetireEnumeratesInventoryAndPreservesMetadata)$'
```

Result: `ok github.com/c-w-xiaohei/sub2api-deploy/internal/hostruntime`.
`gofmt` and scoped `git diff --check` passed. No broad package, build, Pulumi,
provider, engine, race, release, Git staging, or real-service command was run.

Latest requested scoped verification:

```text
GOMAXPROCS=2 go test -count=1 -p=1 ./internal/hostruntime -run '^TestReconcilePostgresClientChangeReplacesShellAndPreservesData$'
```

Result: `ok github.com/c-w-xiaohei/sub2api-deploy/internal/hostruntime`.
`gofmt` and scoped `git diff --check` passed.

## Final Admission Closure

- Reconcile admission is split into a state-free `validateReconcileRequest` and
  the existing live ownership/runtime admission. Bootstrap invokes the pure
  validator after its protocol/revision checks and before machine identity,
  root inspection/creation, discovery, locking, state, journal, artifacts, or
  Docker/nft activity.
- The pure boundary validates configured service-ID grammar, Redis client
  principal/database (`0` through `15`, canonical decimal) and `default`
  reservation, exact local client password maps, deterministic renderer sizes,
  App environment/link credentials, and deployment-owned environment keys.
- Redis readiness is now fixed direct Docker argv:
  `exec <container> redis-cli --raw -h 127.0.0.1 -p <port> ping`, with empty
  stdin and exact `PONG\n` output. `REDISCLI_AUTH` remains supplied only by the
  Redis container's `--env-file`; no shell, password option, URI, or
  password-bearing `-e` is used.
- Focused coverage adds fresh Bootstrap and ordinary Reconcile zero-effect
  matrices, Redis client password rotation with a metacharacter password and
  persisted/argv secrecy assertions, Config/HBA/Ident Inspect drift checks,
  and complete public/secret reserved-key table coverage. Existing Task 2
  PostgreSQL catalog, shell replacement, source-only, artifact cleanup, nft,
  retirement, and recovery coverage is preserved.

## Accepted Review Follow-Up

- The nft JSON classifier now accepts libnftables terminal verdict statements
  (`{"accept":null}` and `{"drop":null}`) only. Test fixtures independently
  construct that realistic JSON and no longer consume the production rule
  renderer or the former private `verdict.jump` representation.
- Direct runtime admission rejects unsafe app environment values, unsupported
  data-link kinds, duplicate per-kind links, missing credentials, extra
  credentials, and missing/extra local client passwords before `Begin` or any
  filesystem, Docker, nft, or SQL effect.
- PostgreSQL HBA is source-independent: nft remains the sole source-IP gate;
  HBA emits exact declared database/user SCRAM records using PostgreSQL's `all`
  address family followed by explicit rejects. Client changes replace the
  mounted PostgreSQL shell while source-only changes retain it. Config, HBA,
  and ident artifacts are validated as a security unit during reuse and Inspect;
  stale ident artifacts are removed on replacement, removal, and retirement.
- `sub2api` is accepted as a normal App PostgreSQL client. Inventory service IDs
  now use the configured App/config-ID grammar, including hyphens, rather than
  SQL principal grammar.
- Redis launches with a protected `REDISCLI_AUTH` env artifact and mounts it for
  a fixed readiness command. Readiness accepts exactly bounded `PONG\n`; an
  error or any other stdout fails the operation without exposing secrets in argv.
- Retirement classifies nft ownership before opening its operation or mutating
  routes, containers, artifacts, or the network. Foreign/malformed tables fail
  closed; only owned/absent state continues to the existing removal sequence.

## Final Recovery Completion

The remaining PostgreSQL recovery and Inspect matrix is complete.

- The pure catalog classifier now accepts an ordered suffix containing any
  number of prior/absent databases after target roles have advanced to the old
  suffix. It still rejects an exact or create-only database after that suffix,
  and still permits only one create-only phase.
- The legacy two-field catalog fake was replaced by a strict stateful fake that
  emits only approved role, database-reference, and conditional
  database-detail records in sorted order. Its catalog facts outlive a new
  Runtime/runner wrapper, modeling a process boundary. It separately counts
  role/password, guarded create, and finalization semantic mutations and
  supports both before-commit and after-commit response loss.
- Ambiguous role, guarded-create, and finalization responses now re-observe the
  catalog but stop the current attempt. The existing pending journal therefore
  requires a retry, preventing a second mutation in the same attempt and
  allowing a new Runtime to continue only after exact observation.
- Focused tests cover role before-commit and after-commit behavior, process
  restart after role/create/finalization response loss, no duplicate semantic
  mutation, one-client/shared-username and two-database paths, and no DROP or
  secret argv/ordinary persisted output. Admin artifacts remain unchanged until
  the required catalog observation and journal completion.
- Malformed, oversized, unavailable, foreign, and mixed catalog observations
  fail closed without semantic SQL mutation. Read-only Inspect covers exact
  readiness, owned create-only drift, and foreign/malformed/unavailable
  recovery; catalog, Docker, and nft mutation counters remain zero.
- State, inventory, and journal assertions exclude plaintext passwords and
  catalog marker/verifier terms. Existing marker tests assert markers are
  non-secret and bound only to host/service/operation identity.

Final recovery RED command:

```text
GOMAXPROCS=2 go test -p=1 ./internal/hostruntime -run '^(TestPostgresCatalogProtocolClassifierRequiresOrderedKeyedPrefix|TestPostgresCatalogRoleResponseLossResumesAfterExactObservation|TestPostgresCatalogCreateAndFinalizeResponseLossResumeWithoutDuplicateMutation)$'
```

Result: failed as expected before the classifier correction because a second
prior/absent database in the old suffix classified as mixed/recovery-required,
blocking guarded create/finalization recovery.

Final recovery GREEN command:

```text
GOMAXPROCS=2 go test -p=1 ./internal/hostruntime -run '^(TestPostgresCatalogProtocolClassifierRequiresOrderedKeyedPrefix|TestPostgresCatalogRoleResponseLossResumesAfterExactObservation|TestPostgresCatalogRoleBeforeCommitPreservesArtifactAndRetriesOnce|TestPostgresCatalogCreateAndFinalizeResponseLossResumeWithoutDuplicateMutation|TestPostgresCatalogObservationFailuresDoNotMutateOrAdvanceArtifacts|TestInspectPostgresCatalogIsReadOnlyAndClassifiesDrift|TestNftExactDesiredIsNoopAndResponseLossRelists|TestNftApplyResponseLossAcceptsOnlyExactDesiredState)$'
```

Result: `ok github.com/c-w-xiaohei/sub2api-deploy/internal/hostruntime`.
`gofmt` and scoped `git diff --check` passed. No broad package, build,
Docker, PostgreSQL, nft, privileged, remote, or release command was run.

## Follow-up Review Record

The initial implementation was reviewed while the coupled nft/Docker/inventory
cluster was incomplete. Subsequent follow-ups completed the strict nft JSON parser
and idempotent mutation subset: deterministic socket-group ordering (every
socket's accepts immediately precede its drop), exact owned-table decoding, and
response-loss relists without duplicate applies.

Historical follow-up record:

- PostgreSQL catalog-marker reconcile integration is now routed through the
  approved pure protocol. Runtime admission builds its expected projection from
  the Host resource/ownership, target revision, applied inventory revision and
  prior clients, and desired clients. It rejects malformed PostgreSQL clients
  and missing/invalid scoped passwords before `Begin`. Runtime observation uses
  the approved roles, database-reference, and conditional database-detail SQL
  records, synthesizing detail only after an absent reference; it does not use
  the former private `postgresCatalogState` classifier. Initial/prior/partial,
  foreign, and unavailable classifications select the catalog-only mutation
  path or fail closed. The current writer uses stdin-only `COPY` plus `%I/%L`
  `\gexec` SQL and target markers at transaction end.
- The PostgreSQL catalog reconciliation is complete against the approved Task 2
  handoff, including scoped obsolete managed CONNECT/schema ACL revocation and
  prior creator/comment/settings cleanup in the role and finalization writers.
  The pure v2 observer is isolated in `postgres_catalog.go`: it generates non-secret
   `s2hpg2` comments via exact `pg_shdescription` object/class OID bindings,
   without a marker table; produces bounded role, database reference, and
   database-detail catalog projections; strictly parses their fixed records;
   and classifies initial/prior/exact/pending-partial/foreign/mixed state.
   It is wired into `reconcile.go` in the completed Task 2 slice.
   The pure protocol now binds each scoped role description to that principal's
   own target/prior marker set, rejecting marker swaps between owners, creators,
   and clients. Its data-derived, non-secret writer handoff enumerates the
   complete ordered role transaction followed by sorted, interleaved guarded
   database-create and per-database finalization phases. Each database marker
   immediately precedes its finalization commit, so a later database is never
   created while an earlier one remains create-only. Stable owners alone receive
   `public` schema `USAGE, CREATE`; clients receive database `CONNECT`, owner
   membership with `SET TRUE`, and per-database `SET ROLE` defaults. The observer
   rejects client schema ACLs as scoped extras. Reconcile integration is
   complete.
- Stateful response-loss/process-restart coverage was completed in the final
  recovery slice above.
- A safe bounded upgrade of an existing unshipped private runtime has not been
  implemented. Existing inventory version mismatches fail closed and preserve
  the persistent data path; no migration behavior was broadened.

## Scope

Implemented only `internal/hostruntime/**` plus this report. Task 1's accepted
contract was consumed as-is: `LocalDataServiceTarget.Bindings`,
`LocalDataServiceTarget.Clients`, scoped `ClientPasswords`, and TLS-aware app
data links provide all required runtime inputs.

## RED / GREEN Evidence

Catalog runtime integration RED command:

```text
GOMAXPROCS=2 go test -p=1 ./internal/hostruntime -run '^(TestDataClientLifecycleUsesStdinAndNeverDropsPostgresData|TestPostgresCatalogProtocol.*|TestNftExactDesiredIsNoopAndResponseLossRelists)$'
```

Result: failed closed with `recovery-required` because the direct lifecycle
fake emitted the former two-field private observer response rather than the
approved bounded roles/reference/detail records.

Catalog runtime integration GREEN command:

```text
GOMAXPROCS=2 go test -p=1 ./internal/hostruntime -run '^(TestPostgresCatalogProtocol.*|TestDataClientLifecycleUsesStdinAndNeverDropsPostgresData|TestPostgresHasExplicitScramConfigAndHBAArtifacts|TestPostgresUsesFixedPeerAdminAndPasswordFile|TestPostgresClientSQLUsesStableOwnersAndStdinContainment|TestPostgresPasswordRotationChangesCredentialBeforeShellReplacementWithoutLeaks|TestPostgresRotationDoesNotDependOnPasswordAuthentication|TestPostgresPeerAdminIsIndependentOfReplacementPassword|TestNftExactDesiredIsNoopAndResponseLossRelists|TestNftApplyResponseLossAcceptsOnlyExactDesiredState|TestNftDeleteResponseLossAndUnknownStateFailClosed)$'
```

Result: `ok github.com/c-w-xiaohei/sub2api-deploy/internal/hostruntime`.
`gofmt` and scoped `git diff --check` also passed. No broad package, build,
Docker, PostgreSQL, nft, privileged, or remote command was run.

Final PostgreSQL pure catalog protocol correction RED command:

```text
GOMAXPROCS=2 go test -p=1 ./internal/hostruntime -run '^TestPostgresCatalogProtocol'
```

Result: failed as expected while the new pure tests required the actual fresh
`POSTGRES_USER=s2h_admin` attribute set, exact scoped membership directionality,
zero-client constant predicates, and compact create-only detection without
assuming final schema ACL/ownership state.

Final PostgreSQL pure catalog protocol correction GREEN command:

```text
GOMAXPROCS=2 go test -p=1 ./internal/hostruntime -run '^TestPostgresCatalogProtocol'
```

Result: `ok github.com/c-w-xiaohei/sub2api-deploy/internal/hostruntime`.

The pure observer now requires `s2h_admin` to be the local-peer management
superuser (`SUPERUSER CREATEDB CREATEROLE LOGIN INHERIT NOREPLICATION
NOBYPASSRLS`) and rejects membership in either direction. Stable owners and
clients are checked as `NOINHERIT` nonprivileged principals. Desired/prior
membership, database settings, and CONNECT predicates use their respective
client topologies; retained moved clients must have obsolete membership and
per-database settings removed. Scoped ACL/settings checks reject old or
undesired entries and require owner/client CONNECT, including explicit absence
of an owner per-database setting. Empty topologies produce `TRUE`/`FALSE`
predicates rather than `IN ()`.

The final pure-protocol correction uses a nonempty valid `PreviousRevision`, not
client count, as the sole prior-existence signal. Prior admin, owner, client,
and database markers therefore remain recognizable for a zero-client revision.
Nonempty previous clients without that revision are rejected. Scoped role
collision and membership checks cover the union of desired and previous stable
owners, operation creators, and clients, plus the admin role. A previous-only
owner with a wrong or missing marker is foreign/mixed, and every unexpected
membership to or from a scoped principal fails exactness. False structural bits
for either target or prior are mixed.

Create-only provenance is target-operation-specific. The pure contract derives
an exact managed `s2h_create_<24hex>` NOLOGIN/NOINHERIT/nonprivileged creator
role from the Host binding, service, revision, and database operation, with an
exact role comment. A pending create-only database must have that exact creator
as owner, no database comment, and `public`; a stable-owner or other-owner
unmarked database is foreign/mixed. Finalization transfers the database and
`public` schema to the stable owner and writes the target database marker; the
creator remains managed. The writer integration requirements now explicitly
cover ordered role and database transactions: exact admin/owner/client/creator
flags, canonical shared-user comments, membership grants, moved/removed
revokes, per-database `RESET ROLE`, removed-client `NOLOGIN`, desired-client
`SET ROLE TO` stable-owner settings, intended owner/client CONNECT grants,
owner/schema/ACL finalization, and object/class-bound markers immediately
before their respective commits. Database reference records separately project
stable-owner and creator-owner bits. Target/prior requires only the stable
owner; create-only requires only the creator, so a creator-owned marked target
or prior classifies as mixed. Reconcile remains deliberately untouched.

PostgreSQL pure catalog protocol correction RED command:

```text
GOMAXPROCS=2 go test -p=1 ./internal/hostruntime -run '^TestPostgresCatalogProtocol'
```

Result: failed as expected at compilation after the strengthened tests introduced
the required keyed database records, explicit prior-operation input, and the
create-only detail shape before those protocol surfaces existed.

PostgreSQL pure catalog protocol correction GREEN command:

```text
GOMAXPROCS=2 go test -p=1 ./internal/hostruntime -run '^TestPostgresCatalogProtocol'
```

Result: `ok github.com/c-w-xiaohei/sub2api-deploy/internal/hostruntime`.

The focused tests cover independent desired/previous App-ID uniqueness with
normal overlap, Host-contract hyphenated binding values, shared PostgreSQL
usernames with stable reordered App-ID sets and prior changed sets, exact
catalog-class/OID marker binding, explicit 0/1 SQL outputs, strict keyed record
framing/bounds/token validation and impossible operation-pair rejection, full
detail parsing, absent-detail synthesis after an absent reference, and
initial/prior/exact/create-only prefix/foreign/unavailable/mixed classification.
Database keys are deterministic tokens and ordering is required. The observer
uses only explicit current/previous protocol inputs, with no verifier reads or
new durable state. `gofmt` and scoped `git diff --check` also passed for this
   slice. No real Docker, PostgreSQL, SSH, privileged, build, or
broad test command was run.

PostgreSQL v2 marker RED command:

```text
GOMAXPROCS=2 go test -p=1 ./internal/hostruntime -run '^(TestPostgresRecoveryMarkersBindHostServiceAndOperation)$'
```

Result: failed as expected because the v2 marker helpers did not exist.

PostgreSQL v2 marker GREEN command:

```text
GOMAXPROCS=2 go test -p=1 ./internal/hostruntime -run '^(TestPostgresRecoveryMarkersBindHostServiceAndOperation|TestPostgresClientSQLUsesStableOwnersAndStdinContainment|TestDataClientLifecycleUsesStdinAndNeverDropsPostgresData|TestPostgresPasswordRotationChangesCredentialBeforeShellReplacementWithoutLeaks)$'
```

Result: `ok github.com/c-w-xiaohei/sub2api-deploy/internal/hostruntime`.

Existing PostgreSQL scoped regression command:

```text
GOMAXPROCS=2 go test -p=1 ./internal/hostruntime -run '^(TestDataClientLifecycleUsesStdinAndNeverDropsPostgresData|TestPostgresHasExplicitScramConfigAndHBAArtifacts|TestPostgresUsesFixedPeerAdminAndPasswordFile|TestPostgresClientSQLUsesStableOwnersAndStdinContainment|TestPostgresPasswordRotationChangesCredentialBeforeShellReplacementWithoutLeaks|TestPostgresRotationDoesNotDependOnPasswordAuthentication|TestPostgresPeerAdminIsIndependentOfReplacementPassword)$'
```

Result: `ok github.com/c-w-xiaohei/sub2api-deploy/internal/hostruntime`.

Parser-subset RED command:

```text
GOMAXPROCS=2 go test -p=1 ./internal/hostruntime -run '^(TestNftJSONClassifiesExactForeignOldAndMalformedStates|TestNftJSONRejectsExtraAndOutOfOrderRules|TestNftExactDesiredIsNoopAndResponseLossRelists|TestNftTransactionOwnsDeterministicPreDNATAllowlistWithoutSecrets)$'
```

Result: compilation failed as expected from the incomplete handoff: the rule
expression refactor passed a byte where JSON was required, `nftRender` had the
wrong return signature, and the realistic nft JSON fixture was missing.

Parser-subset GREEN command:

```text
GOMAXPROCS=2 go test -p=1 ./internal/hostruntime -run '^(TestNftJSONClassifiesExactForeignOldAndMalformedStates|TestNftJSONRejectsExtraAndOutOfOrderRules|TestNftExactDesiredIsNoopAndResponseLossRelists|TestNftTransactionOwnsDeterministicPreDNATAllowlistWithoutSecrets)$'
```

Result: `ok github.com/c-w-xiaohei/sub2api-deploy/internal/hostruntime`.

PostgreSQL peer/HBA and data-shell subset GREEN command:

```text
GOMAXPROCS=2 go test -p=1 ./internal/hostruntime -run '^(TestAppEnvironmentUsesCanonicalDataLinksAndReservesDeploymentKeys|TestAppRunUsesStableNetworkAlias|TestLocalDataRunBindsExactlyAndUsesStableAlias|TestPostgresHasExplicitScramConfigAndHBAArtifacts|TestPostgresUsesFixedPeerAdminAndPasswordFile|TestPostgresClientSQLUsesStableOwnersAndStdinContainment|TestDataClientLifecycleUsesStdinAndNeverDropsPostgresData|TestDockerPortBindingsRequireExactCanonicalTCPPublications|TestPostgresPasswordRotationChangesCredentialBeforeShellReplacementWithoutLeaks|TestPostgresRotationDoesNotDependOnPasswordAuthentication|TestPostgresPeerAdminIsIndependentOfReplacementPassword|TestRedisACLRemovalReplacesShellButPreservesDataPath|TestRedisAndProxyRotationRemovesOldArtifactsAndPreservesData)$'
```

Result: `ok github.com/c-w-xiaohei/sub2api-deploy/internal/hostruntime`.

Docker/O-U-N subset RED command:

```text
GOMAXPROCS=2 go test -p=1 ./internal/hostruntime -run '^(TestNftUnionDeduplicatesOldAndNewSourcesWithoutLooseningPolicyValidation|TestDockerPortBindingsRequireExactCanonicalTCPPublications|TestAllowlistOnlyChangeDoesNotReplaceDataContainerAndObserveMarksNftDrift)$'
```

Initial result: source-only reconciliation failed before an nft command because
the union builder re-appended an existing source (`.9,.9,.10`), and the strict
policy builder correctly rejected that duplicate.

Docker/O-U-N subset GREEN command:

```text
GOMAXPROCS=2 go test -p=1 ./internal/hostruntime -run '^(TestNftUnionDeduplicatesOldAndNewSourcesWithoutLooseningPolicyValidation|TestDockerPortBindingsRequireExactCanonicalTCPPublications|TestAllowlistOnlyChangeDoesNotReplaceDataContainerAndObserveMarksNftDrift|TestNftJSONClassifiesExactForeignOldAndMalformedStates|TestNftJSONRejectsExtraAndOutOfOrderRules|TestNftExactDesiredIsNoopAndResponseLossRelists|TestNftTransactionOwnsDeterministicPreDNATAllowlistWithoutSecrets)$'
```

Result: `ok github.com/c-w-xiaohei/sub2api-deploy/internal/hostruntime`.

RED command:

```text
GOMAXPROCS=2 go test -p=1 ./internal/hostruntime -run '^(TestAppEnvironmentUsesCanonicalDataLinksAndReservesDeploymentKeys|TestLocalDataRunBindsExactlyAndUsesStableAlias)$'
```

Initial result: the focused tests failed because `runLocal` had no target/binding
input, `DATABASE_HOST` was not reserved, PostgreSQL had no explicit config/HBA,
and nft list failures were indistinguishable from absence. These were expected
missing behaviors.

GREEN command:

```text
GOMAXPROCS=2 go test -p=1 ./internal/hostruntime -run '^(TestAppEnvironmentUsesCanonicalDataLinksAndReservesDeploymentKeys|TestAppRunUsesStableNetworkAlias|TestLocalDataRunBindsExactlyAndUsesStableAlias|TestPostgresHasExplicitScramConfigAndHBAArtifacts|TestDataClientLifecycleUsesStdinAndNeverDropsPostgresData|TestRedisACLRemovalReplacesShellButPreservesDataPath|TestNftTransactionOwnsDeterministicPreDNATAllowlistWithoutSecrets|TestNftApplyResponseLossAcceptsOnlyExactDesiredState|TestNftDeleteResponseLossAndUnknownStateFailClosed|TestNftForeignTableIsNeverDeleted|TestAllowlistOnlyChangeDoesNotReplaceDataContainerAndObserveMarksNftDrift|TestReconcileProjectsLocalDataBeforeAppsAndTraefikRoute|TestReconcileCreatesOwnedSharedNetworkAndUsesItForEveryContainer|TestPostgresPasswordRotationChangesCredentialBeforeShellReplacementWithoutLeaks|TestRedisAndProxyRotationRemovesOldArtifactsAndPreservesData)$'
```

Result: `ok github.com/c-w-xiaohei/sub2api-deploy/internal/hostruntime`.

## Implemented Behavior

- App environment emits exact `DATABASE_*` and `REDIS_*` values from canonical
  data links and scoped credentials. All deployment-owned data keys are rejected
  from settings and runtime secrets.
- App and same-host data containers use stable Docker aliases equal to their
  service IDs. Unbound local data services publish no host port. Bound services
  publish only exact configured IPv4 or bracketed IPv6 `host:port:port/tcp`
  endpoints.
- PostgreSQL generates and mounts explicit `listen_addresses = '*'`, SCRAM,
  HBA, and ident artifacts. Fresh containers use `POSTGRES_USER=s2h_admin`,
  `POSTGRES_DB=postgres`, a mounted `POSTGRES_PASSWORD_FILE`, and initdb SCRAM
  host/peer local options. HBA permits only local peer mapping of `root` and
  `postgres` to `s2h_admin`, exact owned Docker-network CIDRs and configured
  remote sources for declared client/database pairs, then local/host rejects;
  it has no trust rule. Runtime `psql -X` administration uses the peer role,
  so admin-password rotation does not require the rotated password. Client SQL
  remains stdin-only, uses strict identifiers and server-side `format` plus
  `\gexec`, creates stable `s2h_owner_<24hex>` no-login owners, grants
  `SET ROLE` membership/default role for first-party migrations, and changes
  removed logins to `NOLOGIN` without dropping business roles, databases,
  schema, tables, or data.
- Redis ACL configuration contains the admin and desired client users. Client
  target changes intentionally replace the controlled Redis container so its
  generated ACL is loaded; the persistent data path is preserved and never
  removed or reinitialized.
- Added a fixed-purpose nft runner: only `nft -j list table inet <name>`,
  `nft -c -f -`, and `nft -f -` are accepted. Transactions use stdin, own a
  deterministic per-Host `inet` table/comment, install `prerouting` at `-110`,
  deterministically sort source rules, and emit exact source/destination/TCP
  port accepts immediately before each destination drop. The parser accepts
  only the bounded ordered `metainfo`/table/chain/rule list form, optional
  numeric handles, the exact owned table and base-chain shape, and exact
  canonical source/destination/port/verdict expressions; extra, reordered, or
  unsupported semantics fail closed.
- Reconciliation performs classified inspect, syntax check, atomic owned-table
  replacement, then exact fingerprint verification. Only an explicit not-found
  result permits creation; all other list failures fail closed. Ambiguous apply
  and delete responses re-inspect exact desired/absent state. Observe marks
  missing/mutated policy as drift. Retirement removes owned containers, then
  owned nft policy, then the Docker network; foreign tables are never deleted.
- Allowlist-only updates replace the owned nft policy without replacing the data
  container when the Docker binding is unchanged. The private inventory version
  records its Host applied revision separately from immutable local shell
  generations, so reused shell labels/artifacts remain valid across a
  source-only Host revision. The transition first installs the old/new socket
  union, verifies exact fixed-format Docker TCP publications, mutates local
  shells only when publication or shell inputs change, then installs final N.
  Local-data removal and retirement re-list the owned container to confirm
  absence before the final owned-table deletion; foreign nft ownership remains
  a conflict and is untouched.

## CI-Only / Unverified

- Privileged nft network-namespace verification of pre-DNAT authorized traffic
  passing and unauthorized traffic dropping remains Task 4 CI-only.
- CI must exercise real nft JSON shape/error handling and privileged
  network-namespace behavior against the real binary. No real nft, Docker, SSH, sudo, provider,
  engine, build, race, or release command was run locally.
- Full-package and cross-package runtime regressions were intentionally not run;
  verification used only the fixed narrow Host Runtime symbol selection.
- Executable PostgreSQL syntax and live catalog output require Task 4 CI-only
  integration; no local PostgreSQL command was run for this pure protocol slice.
