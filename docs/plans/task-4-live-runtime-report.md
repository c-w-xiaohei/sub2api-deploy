# Task 4 Live Provider Runtime Report

## Status

Implementation-present / exact-SHA evidence pending. Source and required CI
workflow wiring are present but unexecuted. No privileged/live command was run
locally.

## Boundary And Evidence

- Required CI contract: `SUB2API_PROVIDER_RUNTIME_LIVE=1`, `SUB2API_TEST_PROVIDER_BINARY`, `SUB2API_TEST_RELEASE_ROOT`, `SUB2API_TEST_PROVIDER_SHA256`, `SUB2API_PROVIDER_RUNTIME_LIVE_IMAGE_ARCHIVE`, and private `PROVIDER_RUNTIME_LIVE_TRACE_DIR`. The test checks the explicit Provider provenance hash and exact released `linux-amd64` Host manifest size/hash/release before effects.
- The outer symbol re-execs its live body through `unshare --mount --net`. It creates a test-owned bridge plus data, App, and unauthorized namespaces. Data and App each run distinct mount sandboxes, machine identities, SSH aliases/endpoints, Host state/artifact paths, Docker daemons, and released Host binaries. The production OpenSSH/sudo bootstrap receiver executes normally for each candidate Host; no DNS or Cloudflare publication claim is made here.
- The image archive is loaded into the private daemon before Provider execution. The daemon has no bridge and the isolated network namespace has no external route, so it cannot pull public images. The mutable `postgres:18-alpine` and `redis:8-alpine` tags still require approved immutable production digests outside this source test.
- Assertions are bounded and sanitized: data Host reconciliation, App Host reconciliation, released App-container projected-data environment authentication and `/ready`, direct App-namespace PostgreSQL SCRAM and Redis ACL checks, wrong-password/default-user denial, PostgreSQL catalog restrictions, Redis user restrictions, pre-DNAT source admission/drop, and exact foreign nft sentinel JSON digest preservation. `mx-allowlist-live.json` is mode `0600` and contains `dataHostPass`, `appHostPass`, app booleans, and verified artifact/foreign-digest hashes only.
- This live boundary deliberately covers initial data admission followed by App reconciliation. It does not add a DataLink Update approval exercise: the production initial-Create contract does not require that approval, and the existing Provider Runtime matrix covers Update approval behavior.

## Production Prerequisite

The runtime currently uses mutable `postgres:18-alpine` and `redis:8-alpine` tags. This test deliberately does not invent immutable production image digests; approved immutable digests remain a production prerequisite.
