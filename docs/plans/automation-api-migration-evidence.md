# Automation API Migration Evidence

## Acceptance Status

This migration is not yet verified. Do not permit local builds or related tests.

| Requirement | Evidence required | Status |
| --- | --- | --- |
| SDK lifecycle rather than a passthrough wrapper | CLI behavior tests exercise Automation Stack operations while retaining approval, source-config isolation, and program-first import | Pending exact-SHA CI |
| External Engine for graph and import tests | Real official CLI operations plus dependency closure excluding Engine/backend/deploy packages | Pending exact-SHA CI |
| Preserve graph semantics | Existing required symbols, checkpoints, failure stop, reverse removal, secrets, and import assertions | Pending exact-SHA CI |
| Preserve runtime and release | All eight required jobs and same-byte candidate consumption | Pending exact-SHA CI |
| Lower build memory with comparable duration | Paired same-runner cold-cache compile/link measurements, fixed Go/concurrency, successful commands | Pending comparison |
| Local safety | No local build/test/list/vet/formatter or package-manager execution | Prohibition remains in force |

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

## Resource Decision

The separate comparison workflow measures baseline and candidate compilation on
the same runner with separate cold Go build caches and fixed concurrency. Its
cgroup measurement includes accounted file cache and must not be labeled RSS.
Both compilation success and measurement availability are required before any
reduction claim. A result for constrained CI compilation does not automatically
authorize unconstrained local builds, race tests, or production operations.

`pulumi-go-provider` may retain `pkg/v3/codegen/schema` and related dependencies.
The promised isolation concerns Engine/backend/deploy execution and build
dependencies, not deletion of the entire `pkg/v3` module from the module graph.
