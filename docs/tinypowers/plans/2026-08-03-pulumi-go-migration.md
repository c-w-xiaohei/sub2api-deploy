# Pulumi Go Runtime Migration Plan

**Source:** User request to replace the entire Pulumi IaC program with Go after the prior language-host program stalled during preview on a 720 MiB VPS.

**Goal:** Replace the Pulumi language host and all deployment helper scripts with Go while preserving resource URNs, configuration keys, command behavior, secret handling, and the existing shell deployment runtime.

**Approach:** Keep the Pulumi resource graph in `infra/` and put the runtime/state/Neon helper behind `sub2api-deploy runtime`; keep `scripts/`, `compose/`, and `traefik/` as shell/configuration surfaces. Pin Go provider modules and pair Neon alpha Go SDK with the alpha.1 provider binary.

## Global Constraints
- Keep project name `sub2api-vps-deploy` and all existing Pulumi configuration keys.
- Keep provider logical names, resource logical names, type tokens, trigger order, `dependsOn`, `ignoreChanges`, and secret boundaries unchanged.
- Do not run `pulumi up` or change real cloud/VPS resources during local verification.
- Do not use Neon beta SDK with the alpha.1 provider binary.
- Do not rewrite shell/Compose/Traefik runtime helpers as part of this migration.

## Requirements and Scope
- Change `Pulumi.yaml` from `runtime: nodejs` to Go with `main: ./infra` or a documented prebuilt binary path.
- Add a Go Pulumi program covering config validation, Cloudflare DNS/SSL resources, optional Neon Project, optional Upstash Redis database, three local commands, outputs, and all existing runtime payload fields.
- Migrate the prior deployment preflight and all runtime helpers to `internal/runtime`; preserve their state formats, atomicity, and safety checks.
- Preserve runtime behavior tests in Go; migrate CI/release contract checks to standard-library Python and Go tests.
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

### Task 4: Remove script language runtime dependencies and update operations docs

**Depends on:** Task 3

**Files:**
- Delete: `src/index.ts`, `src/config.ts`, `src/database.ts`, `src/redis.ts`, `src/cloudflare.ts`, `src/command-triggers.ts`
- Delete: their former Pulumi-host tests after equivalent Go coverage exists
- Modify: `README.md`, `Pulumi.production.example.yaml`, `.gitignore`, CI/release workflows
- Create: `internal/runtime`, `scripts/ci-evidence.py`

**Requirements:**
- Remove the former JavaScript dependency graph and all helper sources from the active deployment path.
- Document whether the VPS uses `main: ./infra` or a prebuilt Linux binary; prefer prebuilt binary if local Go compilation is too costly.
- Document the mandatory preflight state export and preview-only verification before production `up`.

**Acceptance:** No production, legacy, test, or CI path invokes a Node runtime; shell runtime checks still pass; the Go helper and Python evidence parser are reproducible from the repository toolchain.

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
- No claim that Go changes the Pulumi Engine boundary; the official Pulumi executable remains external.
