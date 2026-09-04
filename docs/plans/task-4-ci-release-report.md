# Task 4 CI And Release Report

## Status

CI-ready / exact-SHA evidence pending. No local npm, Go, build, release, Docker,
or privileged command was run for this wiring.

## Delivered Wiring

- The complete pre-existing 001 evidence contract is restored: Host Controller
  requires all seven Program/Controller symbols, Provider SSH requires all
  three transport/protocol symbols, Provider Import requires its preview
  symbol, and Provider Runtime retains both normal and race evidence for its
  two existing symbols. Every selector captures its Go exit status before Go
  JSON terminal-pass enforcement reports each fixed test name and status,
  rejects skips/absence and stderr, then asserts the recorded status. It uploads
  only sanitized evidence.
  Host Controller retains its full package test and vet coverage and excludes
  Provider Runtime from its historical broad race pass; Provider Runtime owns
  that dedicated race pass. The other existing full package and vet checks are
  retained alongside their selector evidence.
- `Engine Graph` requires all nine original Engine symbols plus the three
  `MX-ALLOWLIST-01` symbols as no-skip requirements. It derives the complete
  deterministic persisted JSONL filename
  set from the selected top-level/subtest harness names, rejects extras,
   directories, symlinks, wrong modes, malformed records, and fixture canaries.
   Each expected trace file must contain at least one strict JSONL event record;
   multiple valid harness events are retained and validated. It uploads only
   reduced terminal test events.
- `Provider Runtime` retains required normal and race gates, then assembles the
  exact-SHA bundle before the live gate. It installs the live fixture tools,
  requires noninteractive sudo and root mount/network unshare, builds the
  `sub2api-live-app:mx-allowlist` fixture without build arguments, pulls and
  saves it with `postgres:18-alpine` and `redis:8-alpine`, verifies every live
  tool including `nsenter`, and runs the live test without `-race` as root in
  its isolated namespace.
- A private failure-safe directory exists before any Provider Runtime gate. Raw
  Go JSON, stderr, private traces, consumer extraction, image archive, and any
  workflow-started daemon log/process are removed by an always finalizer. The
  successful upload is limited to a sanitized terminal event and mode/schema
  validated `mx-allowlist-live.json`. Its exact schema requires true
  `appDataEnvironmentAuthenticated` and `appReadyAfterData` booleans before
  the summarized consumer trace is generated.
- The live result is recorded in `consumer-trace.json` with exact SHA plus
  Provider and Host hashes and `providerRuntimeCrossHostAppLive` proving the
  released data Host then released App Host path. DNS publication remains
  Engine Graph ordering evidence, not a live-fixture claim. After all Provider
  Runtime gates pass, only its archive, checksum, and trace become the
  intermediate candidate artifact.
- `Target Release` depends on all existing jobs, downloads that intermediate
  exact-SHA artifact, verifies its checksum, release bundle, hashes, and live
  trace, and writes final gate metadata without rebuilding. Its final artifact
  remains the existing four files.
- Target metadata and promotion validation use the same full gate-symbol lists,
  preserve all eight successful job names, and bind the exact SHA, CI run ID,
  CI run URL, archive name, and archive SHA-256. Credential canaries are
  scanned across textual bundle files; IPv4 confidentiality applies only to
  generated `metadata.json` and `consumer-trace.json`, so documented example
  and internal addresses remain valid bundle content.

## Production Prerequisite

The CI fixture deliberately uses the source-required mutable tags
`postgres:18-alpine` and `redis:8-alpine`. This is not an immutable production
image approval; approved immutable production image digests remain external.

## Verification Limit

YAML parsing, Node syntax checks for the static contract tests, and scoped diff
checks passed locally. Exact-SHA execution, privileged namespace behavior,
image availability, and promotion evidence remain pending CI.
