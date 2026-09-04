# Task 1 Cross-Host Contract Report

## Status

DONE. Task 1 source, local-safe contract verification, and CI-only Program/Provider test definitions are complete.

## Behavior

- Environment validation canonicalizes configured IP literals and accepts remote Docker relations only when owner and consumer share safe internal IPv4 (preferred) or IPv6; mapped, wildcard, loopback, multicast, and link-local literals are rejected for these relations.
- Host contracts include sorted local data bindings, exact sources, data clients, and client passwords scoped to the owning data Host.
- Docker data identities are `docker:<owner-server>:<service-id>`; same-Host links use the stable service ID and remote links use the selected owner internal address. `DataIdentity` carries canonical PostgreSQL/Redis TLS semantics for Task 2 rendering.
- Provider compatibility accepts legacy data identities that omit `tlsMode`; `hostcontract.NormalizeTargetSecrets` is the sole defaulting point, while Program always projects explicit current modes. Invalid explicit values remain rejected during Check.
- Reopened F6/F8/F9/F10/F13: Docker PostgreSQL reserves every `s2h_` role, validates provisioned usernames/databases as PostgreSQL identifiers, excludes managed system databases, and rejects unsafe upstream keyword-DSN passwords. The DSN lexical rule applies to all present App PostgreSQL credentials before registration; Docker-only identifier rules remain limited to local provisioning clients.
- Program registration orders Host resources from a deterministic acyclic data-to-App graph, then derives rolling App placement order from that graph, derives local bindings/clients, scopes client passwords to the owner, and rejects topology cycles, conflicting local principals, and remote socket collisions before Host registration.

## Changed Paths

- `internal/environment/environment.go`
- `internal/environment/environment_test.go`
- `internal/hostcontract/contract.go`
- `internal/hostcontract/contract_test.go`
- `internal/hostprovider/provider.go`
- `internal/program/program.go`
- `internal/program/program_test.go`
- `docs/plans/task-1-cross-host-contract-report.md`

## TDD And Tests

- RED: `GOMAXPROCS=2 go test -p=1 ./internal/environment` failed because `CommonInternalAddress` did not exist. `GOMAXPROCS=2 go test -p=1 ./internal/hostcontract` initially failed on the added contract test before the contract types existed.
- GREEN: `GOMAXPROCS=2 go test -p=1 ./internal/environment` passes.
- GREEN: `GOMAXPROCS=2 go test -p=1 ./internal/hostcontract` passes.
- Formatting: `gofmt` completed for every changed Go file. `git diff --check` reported no whitespace errors for the scoped code paths.
- Latest local-safe result: `GOMAXPROCS=2 go test -p=1 ./internal/hostcontract` passes. `gofmt` and scoped `git diff --check` pass.
- CI-only by brief: `TestValidateDockerDataClientsAggregatesMultipleRemoteConsumers`, `TestValidateDockerDataClientsAcceptsIdenticalDuplicatePrincipal`, `TestValidateDockerDataClientsPostgresProvisioningContract`, `TestRegisterCrossHostDockerProjectsRemotePostgresAndRedis`, `TestRegisterCrossHostDockerIPv4PreferenceIPv6FallbackAndTLS`, `TestRegisterCrossHostDockerAggregatesSourcesAndUsesDataDAGPlacement`, `TestRegisterExternalDataProjectsCanonicalTLSModes`, `TestRegisterCrossHostDockerPreflightRejectsCyclesSocketsAndPrincipals`, `TestProviderSchemaHasOnlyHostAndSecretRevisionKey`, `TestCheckAcceptsCompleteCrossHostDataContract`, `TestCheckAcceptsLegacyOmittedTLSModeAndRejectsInvalidExplicitMode`, and `TestCheckRejectsUnsafePostgresClientContract` are defined but intentionally unexecuted locally. No Pulumi, Provider-linked, build, broad, race, Engine, release, or runtime test was run locally.
- Review-fix RED: the added unsafe internal-address test failed before the environment hardening; the added Host binding/admin-secret test failed before contract enforcement.
- Review-fix GREEN: the same constrained Environment and Host contract commands pass after the fixes.
- Reopened-finding RED/GREEN: `TestValidateRejectsUnsafePostgresClientCredentials` and `TestLocalPostgresClientContractRejectsReservedIdentifiersAndUnsafeDSNPasswords` failed before validation and pass after it. CI-only `TestValidateDockerDataClientsPostgresProvisioningContract` independently covers reserved username, invalid identifier, system database, unsafe DSN password, and a valid hyphenated App ID with permitted DSN punctuation. `TestCheckRejectsUnsafePostgresClientContract` invokes Provider Check for the semantic Host-contract rejection; both are intentionally unexecuted locally.

## Self-Review

- Reviewed authority constraints: no environment allowlist/firewall fields were added; all policy is derived from service ownership and App placement.
- Reviewed secret scope: data-owner targets receive service admin and relevant client passwords; non-owner Hosts receive neither through `localDataServices`.
- Reviewed ordering: all data-owner and rolling-placement edges are topologically sorted before any Host registration; cycle detection occurs in preflight.
- Review follow-up: source maps each Program socket to service identity, Provider `tlsMode` accepts the contract union as an optional compatibility field, and contract duplicate-principal comparison includes App-ID-resolved passwords. The named CI-only tests cover remote projection, binding aggregation, DAG/bootstrap ordering, cycle/socket/principal preflight, address-family selection, TLS, owner secret scope, parsed schema fields, explicit/legacy/invalid Provider Check inputs, and same-Host PostgreSQL `disable` mode with no TLS server name.
- The credential grammar is enforced independently at Environment, Program preflight, and Host contract boundaries without adding business-data target fields or Runtime changes.
- Preserved existing dirty checkout changes and did not stage, commit, push, reset, clean, or revert worktree state.

## Concerns

- Program and Provider schema behavior requires CI-only execution under the task constraints. Runtime enforcement of bindings, TLS transport flags, client provisioning, and nftables policy remains Task 2 scope.
- Existing unrelated dirty changes dominate the aggregate file diffs, especially the Program test file; this task did not reset or rewrite them.
