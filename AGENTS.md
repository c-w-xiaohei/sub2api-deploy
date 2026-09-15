# Sub2API Deployment Repository

## Pulumi Engine Boundary

- Never build Pulumi CLI or Pulumi Engine from source locally. Do not run
  `go build`, `go test`, `go install`, `go run`, `go list`, or `go vet` against
  a Pulumi source checkout, the Pulumi module cache, or
  `github.com/pulumi/pulumi/pkg/v3/...` packages.
- Never add `github.com/pulumi/pulumi/pkg/v3/engine`,
  `github.com/pulumi/pulumi/pkg/v3/backend`, or
  `github.com/pulumi/pulumi/pkg/v3/resource/deploy` to a local build or test
  dependency closure. Pulumi Engine must remain inside the prebuilt official
  `pulumi` executable.
- External Engine tests may run locally only with an already-built official
  Pulumi CLI `v3.256.0`. Set `ENGINE_GRAPH_PULUMI_CLI` and
  `EXTERNAL_PULUMI_CLI` to its absolute executable path. Do not substitute a
  source build, wrapper, installer shim, or different version.
- `PROVIDER_IMPORT_BINARY` may reference an already-built project Provider;
  building the project Provider does not authorize building Pulumi Engine.

## Local Build And Test

- Cloudflare's full generated Go SDK is another heavyweight compile boundary,
  independent of Pulumi Engine. Until CI proves its removal from the dependency
  closure, do not locally compile/test/vet the Environment Program, `infra`,
  `internal/program`, or their dependent integration packages. `-p=1` does not
  bound the memory of one large generated package. Keep those checks in CI.

- Run local builds and tests serially: one command at a time, no background
  jobs, parallel shells, matrix execution, or concurrent package commands.
- Use `GOMAXPROCS=2` and Go package parallelism `-p=1`. Add `-parallel=1` to
  `go test` commands.
- Build and test only explicitly named repository packages. Do not run broad
  commands such as `go build ./...`, `go test ./...`, or `go vet ./...`.
- CLI-only local verification is limited to `./cmd/sub2api-deploy`. Its tests
  may use repository-owned test shims, but must not build or start Pulumi
  Engine.
- Engine Graph and Provider Import tests must not run unless the required
  environment variables point to verified, prebuilt CLI and Provider
  executables. Run those suites separately and serially.
- Never run local Pulumi updates, refreshes, destroys, production backends,
  Docker operations, release assembly, race tests, or resource benchmarks as
  part of routine local verification. Keep those gates in CI unless the user
  explicitly authorizes the exact operation.
