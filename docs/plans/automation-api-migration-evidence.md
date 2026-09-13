# Automation API Migration Evidence

## Acceptance Status

The Automation API migration is functionally verified at implementation
revision `be41976a6841b3f2db73d5395a9452a053b3af76`. The resource-reduction
objective is not met: Engine and Import cold compilation became faster, but
their cgroup memory peaks were effectively unchanged, while CLI compilation
regressed in both time and memory. Do not describe this migration as an overall
build-resource optimization.

| Requirement | Evidence required | Status |
| --- | --- | --- |
| SDK lifecycle rather than a passthrough wrapper | CLI behavior tests exercise Automation Stack operations while retaining approval, source-config isolation, and program-first import | Verified |
| External Engine for graph and import tests | Real official CLI operations plus dependency closure excluding Engine/backend/deploy packages | Verified |
| Preserve graph semantics | Existing required symbols, checkpoints, failure stop, reverse removal, secrets, and import assertions | Verified |
| Preserve checkpoint integrity checks | Every Engine Graph and Provider Import export is imported unmodified into a disposable same-name file-backend stack through public Automation API | Verified |
| Preserve runtime and release | All eight required jobs and same-byte candidate consumption | Verified |
| Lower build memory with comparable duration | Paired same-runner cold-cache compile/link measurements, fixed Go/concurrency, successful commands | Not achieved |
| Local safety | No local build/test/list/vet/formatter, package-manager, Pulumi, Docker, benchmark, or measurement execution | Verified |

## Final Functional Evidence

- Revision: `be41976a6841b3f2db73d5395a9452a053b3af76`.
- Run: https://github.com/c-w-xiaohei/sub2api-deploy/actions/runs/34754034422.
- All eight required jobs passed: Verify, Engine Graph, Provider SSH, Provider
  Runtime, Provider Import, Host Controller (amd64), Host Controller (arm64),
  and Target Release.
- Verify passed `go mod tidy` equality and the dependency-closure guard that
  rejects `pkg/v3/engine`, `pkg/v3/backend`, and `pkg/v3/resource/deploy` from
  command and integration-test dependencies.
- Engine Graph passed all 12 required external-Engine tests, including partial
  checkpoints, cross-Host admission ordering, reverse removal, failure stop,
  retained/protected managed data, and sanitized trace artifacts.
- Provider Import passed with the exact observed lifecycle
  `Configure -> Read -> Check -> Diff` and no Create, Update, or Delete call.
- Target Release passed after consuming the exact candidate artifacts produced
  by the two Host Controller jobs and Provider Runtime.

### Checkpoint Integrity

`automationtest.ValidatedExport` calls public `Stack.Export`, then imports the
unmodified payload through public `Stack.Import` into an isolated disposable
file backend with the same project and stack name. The official CLI performs a
non-forced `stack import`; Pulumi's `SaveSnapshot` therefore runs
`snapshot.VerifyIntegrity()` before saving. The raw export remains private and
is neither logged nor uploaded. Engine Graph and Provider Import both passed
this validation for their full and partial checkpoints in the final functional
run.

## Historical Functional Baseline

- Revision: `46be3e20299b1c2e43251a52e48a942248d6199b`.
- Run: https://github.com/c-w-xiaohei/sub2api-deploy/actions/runs/34182012748.
- All eight required Task 4 jobs passed.
- This is before the Automation API migration. It is not evidence for later revisions.

## Initial Resource Observation

- Revision: `596c2b4a5203b83a9bf8b2da1668d7675d2fd214`.
- Run: https://github.com/c-w-xiaohei/sub2api-deploy/actions/runs/34742922548.
- Engine Graph job passed; the whole run did not pass.
- Artifact: `engine-graph-evidence-596c2b4a5203b83a9bf8b2da1668d7675d2fd214`.
- Record: `engine-graph-resource.json`, schema `engine-graph-resource-v1`.
- Implementation: `embedded-engine`.
- Elapsed: 19,200 milliseconds.
- GNU time maximum single-process RSS: 2,879,460 KiB.

The cache was restored and vet ran before this command. GNU time's maximum RSS
does not establish concurrent process-tree aggregate memory. These numbers
cannot authorize local compilation or prove the effect of the migration.

## First Integrated CI Attempt

- Revision: `919e3394100921c76de6dba8de27c636af245846`.
- Functional run: https://github.com/c-w-xiaohei/sub2api-deploy/actions/runs/34745867920.
- Verify stopped at the committed module graph check; the migration requires
  adopting the new CI-generated tidy artifact.
- Engine Graph compilation found a value-to-pointer assignment regression.
  Provider Import also has an undefined fixture variable. Neither suite has
  established migrated runtime behavior at this revision.
- Resource run: https://github.com/c-w-xiaohei/sub2api-deploy/actions/runs/34745867951.
  Engine and Import compilation failed, so the overall comparison failed closed.

The successful CLI pair from this run is a regression, not a reduction:

| Cold CLI compilation | Baseline `46be3e2` | Candidate `919e339` |
| --- | --- | --- |
| Wall seconds | 30.428839374 | 40.787013561 |
| Cgroup peak bytes, including accounted file cache | 1,216,000,000 | 2,206,711,808 |

Both commands exited zero on the same runner with Go 1.25.11, linux/amd64,
CGO disabled, GOMAXPROCS 2, `-p=2`, and separate cold build caches. This pair
does not measure runtime Engine memory, total CI duration, or test compilation.
Later fixes require a new exact-SHA functional and resource comparison.

## Final Resource Evidence

- Candidate revision: `be41976a6841b3f2db73d5395a9452a053b3af76`.
- Baseline revision: `46be3e20299b1c2e43251a52e48a942248d6199b`.
- Run: https://github.com/c-w-xiaohei/sub2api-deploy/actions/runs/34754034415.
- All three measurement jobs and the comparison job passed.

| Cold compilation target | Baseline wall seconds | Candidate wall seconds | Wall ratio | Baseline cgroup peak bytes | Candidate cgroup peak bytes | Memory ratio |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| CLI | 30.871905325 | 40.593820031 | 1.315 | 1,215,561,728 | 2,209,357,824 | 1.818 |
| Engine | 181.041036258 | 155.864516118 | 0.861 | 15,681,097,728 | 15,721,762,816 | 1.003 |
| Import | 219.811342972 | 193.510525658 | 0.880 | 15,710,162,944 | 15,716,593,664 | 1.000 |

Engine and Import compile faster under the constrained measurement, but neither
shows a meaningful memory reduction. CLI is materially slower and uses more
memory. The resource objective therefore fails even though all commands exited
zero and the migration's functional and dependency-isolation requirements pass.

## Resource Decision

The separate comparison workflow measures baseline and candidate compilation on
the same runner with separate cold Go build caches and fixed concurrency. Its
cgroup measurement includes accounted file cache and must not be labeled RSS.
Both compilation success and measurement availability are required before any
reduction claim. The final measurements do not support such a claim. A result
for constrained CI compilation does not automatically characterize
unconstrained local builds, race tests, or production operations.

`pulumi-go-provider` may retain `pkg/v3/codegen/schema` and related dependencies.
The promised isolation concerns Engine/backend/deploy execution and build
dependencies, not deletion of the entire `pkg/v3` module from the module graph.
