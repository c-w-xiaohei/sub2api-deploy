# Pulumi Resource Comparison Report Schema

The `pulumi-resource-comparison` workflow records one sanitized JSON object for
each fixed label (`cli`, `engine`, and `import`) and revision (`baseline` or
`candidate`). The candidate is `github.sha`; the baseline is the immutable
`46be3e20299b1c2e43251a52e48a942248d6199b` commit.

## Measurement Contract

- Runs on `ubuntu-24.04` with Go `1.25.11`, `CGO_ENABLED=0`, `GOMAXPROCS=2`,
  and Go build parallelism `-p=2`.
- Baseline and candidate are checked out into separate directories on the same
  runner for each matrix target.
- `GOMODCACHE` is shared and predownloaded with `go mod download` before timing.
  Every measured command receives a new private, empty `GOCACHE`.
- On the disposable runner, both revisions sync and drop the host page cache
  before measurement, preventing the baseline from warming candidate inputs.
  Each compile has an internal 1,100-second deadline with process-group cleanup.
- The measured CLI command is `go build -o <private> ./cmd/sub2api-deploy`.
  The measured Engine and Import commands are `go test -c -o <private>` for
  `./internal/integration/enginegraph` and
  `./internal/integration/providerimport`, respectively. The resulting
  binaries are never executed.
- A cgroup-v2 child is mandatory. The peak is read from that child’s
  `memory.peak` after the command exits. If the cgroup cannot be created,
  populated, or read, the record is `measurement_unavailable`; no per-process
  RSS or GNU `time` fallback is permitted.
- Command stderr/stdout and cgroup setup diagnostics remain in a private,
  deleted finalizer directory. They are not uploaded or represented in the
  report.

## Schema

Every report has exactly these keys:

```json
{
  "architecture": "amd64",
  "cgo_enabled": false,
  "command_exit": 0,
  "gomaxprocs": 2,
  "gocache": "private-cold",
  "gomodcache": "shared-predownloaded",
  "goos": "linux",
  "label": "cli",
  "memory_peak_source": "cgroup-v2-memory.peak",
  "parallelism": 2,
  "revision": "candidate",
  "schema": "pulumi-resource-comparison-v1",
  "source_sha": "<40 lowercase hexadecimal characters>",
  "status": "measured",
  "toolchain": "go1.25.11",
  "wall_seconds": 1.234567,
  "cgroup_memory_peak_bytes": 123456789
}
```

`status` is one of `measured`, `compile_failed`, or
`measurement_unavailable`. A measured record has exit code `0`, a non-negative
wall-clock value, and a non-negative cgroup peak. A compile failure records its
nonzero command exit and measured resource values. An unavailable measurement
uses `null` for `command_exit`, `wall_seconds`, and
`cgroup_memory_peak_bytes`.

The workflow comparison uses candidate / baseline ratios for wall time and
cgroup peak. A ratio below `1.0` is an observed reduction; no numeric pass
threshold is invented. Compilation failures and unavailable measurements fail
the comparison job, while successful measurements are reported objectively.

## Interpretation Limits

`memory.peak` is an aggregate cgroup charge and can include page cache. It must
not be described as RSS. The experiment controls Go's parallelism at `-p=2`
and cold-starts `GOCACHE` with symmetric host page-cache preparation; it does not
reproduce historic unconstrained WSL behavior. Passing this CI
comparison therefore does not authorize local Go builds/tests, and it does not
establish production Pulumi or Docker behavior.
