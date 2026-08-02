# Pulumi Go Runtime Migration Plan

**Source:** User request to replace the entire Pulumi IaC program with Go after the TypeScript program stalled during preview on a 720 MiB VPS.

**Goal:** Replace the Pulumi Node.js/TypeScript language host with a Go program while preserving resource URNs, configuration keys, command behavior, secret handling, and the existing shell deployment runtime.

**Approach:** Move only the Pulumi resource graph into `infra/`; keep `scripts/`, `compose/`, `traefik/`, and the small TypeScript compatibility helper used directly by shell scripts. Pin Go provider modules and pair Neon alpha Go SDK with the alpha.1 provider binary.

## Global Constraints
- Keep project name `sub2api-vps-deploy` and all existing Pulumi configuration keys.
- Keep provider logical names, resource logical names, type tokens, trigger order, `dependsOn`, `ignoreChanges`, and secret boundaries unchanged.
- Do not run `pulumi up` or change real cloud/VPS resources during local verification.
- Do not use Neon beta SDK with the alpha.1 provider binary.
- Do not rewrite shell/Compose/Traefik runtime helpers as part of this migration.

## Requirements and Scope
- Change `Pulumi.yaml` from `runtime: nodejs` to Go with `main: ./infra` or a documented prebuilt binary path.
- Add a Go Pulumi program covering config validation, Cloudflare DNS/SSL resources, optional Neon Project, optional Upstash Redis database, three local commands, outputs, and all existing runtime payload fields.
- Preserve `src/deployment-preflight.ts` because `scripts/infra-reconcile.sh` invokes it directly.
- Preserve runtime Vitest tests; migrate Pulumi-host pure behavior tests to Go tests.
- Pin Cloudflare `v6.18.0`, Command `v1.2.1`, Upstash `v0.5.0`, and Neon alpha SDK commit `601a1132b2200425bad604f1c8bd434f24e9178d`.

## Tasks

### Task 1: Establish Go project and provider dependencies

**Depends on:** none

**Files:**
- Create: `go.mod`, `go.sum`
- Create: `infra/main.go`
- Modify: `Pulumi.yaml`

**Requirements:**
- Register the Go Pulumi runtime without changing the project name.
- Import the generated Go SDKs for Cloudflare, Command, Upstash, and the pinned Neon alpha SDK.
- Ensure provider package registration and plugin metadata resolve to the intended provider versions.

**Acceptance:** `go test ./...`, `go vet ./...`, and `go build ./infra` pass.

### Task 2: Migrate pure config and connection behavior

**Depends on:** Task 1

**Files:**
- Create: `infra/config.go`, `infra/config_test.go`
- Create: `infra/database.go`, `infra/database_test.go`
- Create: `infra/redis.go`, `infra/redis_test.go`
- Create: `infra/triggers.go`, `infra/triggers_test.go`

**Requirements:**
- Preserve defaults and validation errors for modes, namespace, image digest, credentials, probe path, and provider settings.
- Preserve DSN parsing, split database/Redis connection fields, managed names, secret outputs, and trigger ordering.
- Write tests before implementation for each migrated behavior and observe the expected RED result.

**Acceptance:** Go tests cover the existing TypeScript config/database/connection/trigger contracts and pass.

### Task 3: Recreate the Pulumi resource graph in Go

**Depends on:** Task 2

**Files:**
- Modify: `infra/main.go`
- Create or modify: `infra/cloudflare.go`, `infra/commands.go`, `infra/resources_test.go`

**Requirements:**
- Keep Cloudflare DNS record and strict SSL settings identical.
- Keep optional Neon Project and Upstash Redis resource type tokens and logical names identical.
- Preserve all three Command resources, environments, `LoggingNone`, triggers, `dependsOn`, `ignoreChanges`, and `AdditionalSecretOutputs`.
- Export only `domainName`, `dnsRecordId`, `strictReadinessId`, and `deploymentId`.
- Keep `runtimePayload` secret and preserve `AUTO_SETUP` handling.

**Acceptance:** Pulumi Go mocks or an equivalent offline resource graph test confirms type tokens, names, critical inputs, options, and exports.

### Task 4: Remove Pulumi TypeScript host dependencies and update operations docs

**Depends on:** Task 3

**Files:**
- Delete: `src/index.ts`, `src/config.ts`, `src/database.ts`, `src/redis.ts`, `src/cloudflare.ts`, `src/command-triggers.ts`
- Delete: their Pulumi-host Vitest tests after equivalent Go coverage exists
- Modify: `package.json`, `tsconfig.json`, `README.md`, `Pulumi.production.example.yaml`, `.gitignore`
- Create: `scripts/build-pulumi.sh` if prebuilt binary mode is selected

**Requirements:**
- Keep Node dependencies only for runtime TS helpers and their tests.
- Document whether the VPS uses `main: ./infra` or a prebuilt Linux binary; prefer prebuilt binary if local Go compilation is too costly.
- Document the mandatory preflight state export and preview-only verification before production `up`.

**Acceptance:** No Pulumi entrypoint imports TypeScript; shell runtime checks still pass; clean dependency installation remains reproducible.

### Task 5: Verify offline and migration safety

**Depends on:** Task 4

**Files:**
- No production file changes expected.

**Requirements:**
- Run Go tests, vet, build, existing runtime tests, shell syntax checks, compose validation, and `git diff --check`.
- On the VPS, separately run `pulumi stack export` without `--show-secrets` and inspect whether resources are empty before any `up`.
- Compare a Go `pulumi preview --diff --show-urns --suppress-outputs` against the existing stack; stop on provider version, type token, delete/create, or replacement changes.

**Acceptance:** Local checks pass and the production stack is not changed until the state export and preview are manually reviewed.

## Explicit Non-Goals
- No Neon data migration or resource creation in this code change.
- No shell, Compose, Traefik, or Sub2API application behavior rewrite.
- No forced state rename/remove/replace operations.
- No claim that Go eliminates all Node memory use; shell runtime helpers still use Node during infrastructure reconciliation.
