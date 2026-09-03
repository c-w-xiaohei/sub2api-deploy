import { readFileSync } from "node:fs";
import { describe, expect, it } from "vitest";

const workflow = readFileSync(new URL("../.github/workflows/release.yml", import.meta.url), "utf8");

function normalized(value: string) {
  return value.replace(/[ \t]+$/gm, "").replace(/\n*$/, "\n");
}

// This is intentionally a complete promotion workflow: no build capability can be hidden outside a checked step.
const expectedReleaseWorkflow = `name: Promote Release

on:
  push:
    tags:
      - "v*"

permissions:
  actions: read
  contents: write

concurrency:
  group: release-\${{ github.ref }}
  cancel-in-progress: false

jobs:
  promote:
    name: Promote exact CI candidate
    runs-on: ubuntu-latest
    timeout-minutes: 20
    env:
      TAG_SHA: \${{ github.sha }}
      TAG_NAME: \${{ github.ref_name }}
    steps:
      - name: Check out exact tag SHA
        uses: actions/checkout@v4
        with:
          ref: \${{ env.TAG_SHA }}
          fetch-depth: 0

      - name: Select and validate exact CI candidate
        env:
          GH_TOKEN: \${{ github.token }}
        run: |
          set -euo pipefail
          [[ "$TAG_SHA" =~ ^[0-9a-f]{40}$ ]]
          [[ "$TAG_NAME" =~ ^v[0-9A-Za-z._-]+$ ]]
          test "$(git rev-parse HEAD)" = "$TAG_SHA"
          test "$(git rev-parse "refs/tags/\${TAG_NAME}^{commit}")" = "$TAG_SHA"
          # A successful branch-push CI run at this exact commit is the promotion source.
          runs="$RUNNER_TEMP/ci-runs.json"
          gh api --paginate --slurp "repos/$GITHUB_REPOSITORY/actions/workflows/ci.yml/runs?event=push&status=completed&per_page=100" > "$runs"
          run_id="$(node - "$runs" "$TAG_SHA" <<'NODE'
          const fs = require("node:fs");
          const [path, sha] = process.argv.slice(2);
          const runs = JSON.parse(fs.readFileSync(path, "utf8")).flatMap((page) => page.workflow_runs);
          const selected = runs.filter((run) => run.head_sha === sha && run.event === "push" && run.status === "completed" && run.conclusion === "success" && run.path === ".github/workflows/ci.yml" && run.name === "Pull-Request CI" && /^agent\\x2f/.test(run.head_branch || "") && !run.head_branch.startsWith("refs/tags/")).sort((a, b) => b.run_attempt - a.run_attempt || b.id - a.id)[0];
          if (!selected) throw new Error("missing successful ci.yml run");
          process.stdout.write(String(selected.id));
          NODE
          )"
          [[ "$run_id" =~ ^[0-9]+$ ]]
          jobs="$RUNNER_TEMP/ci-jobs.json"
          gh api --paginate --slurp "repos/$GITHUB_REPOSITORY/actions/runs/$run_id/jobs?per_page=100" > "$jobs"
          node - "$jobs" <<'NODE'
          const fs = require("node:fs");
          const expected = ["Verify", "Host Controller (amd64)", "Host Controller (arm64)", "Engine Graph", "Provider SSH", "Provider Runtime", "Provider Import", "Target Release"];
          const jobs = JSON.parse(fs.readFileSync(process.argv[2], "utf8")).flatMap((page) => page.jobs);
          for (const name of expected) {
            const found = jobs.filter((job) => job.name === name);
          if (found.length !== 1 || found[0].status !== "completed" || found[0].conclusion !== "success") throw new Error("invalid required gate: " + name);
          }
          NODE
          artifacts="$RUNNER_TEMP/ci-artifacts.json"
          gh api --paginate --slurp "repos/$GITHUB_REPOSITORY/actions/runs/$run_id/artifacts?per_page=100" > "$artifacts"
          artifact_id="$(node - "$artifacts" "$TAG_SHA" <<'NODE'
          const fs = require("node:fs");
          const [path, sha] = process.argv.slice(2);
          const found = JSON.parse(fs.readFileSync(path, "utf8")).flatMap((page) => page.artifacts).filter((artifact) => artifact.name === "target-release-" + sha && artifact.expired === false);
          if (found.length !== 1) throw new Error("candidate artifact missing or duplicate");
          process.stdout.write(String(found[0].id));
          NODE
          )"
          [[ "$artifact_id" =~ ^[0-9]+$ ]]
          private="$RUNNER_TEMP/target-release-$TAG_SHA"
          mkdir -p "$private"
          gh api "repos/$GITHUB_REPOSITORY/actions/artifacts/$artifact_id/zip" > "$private/candidate.zip"
          python3 - "$private/candidate.zip" "$private/artifact" "$TAG_SHA" <<'PYTHON'
          import os, stat, sys, zipfile
          archive, destination, sha = sys.argv[1:]
          expected = {f"sub2api-controller-{sha}.tar.gz", f"sub2api-controller-{sha}.tar.gz.sha256", "metadata.json", "consumer-trace.json"}
          with zipfile.ZipFile(archive) as bundle:
              names = [entry.filename for entry in bundle.infolist()]
              if len(names) != len(set(names)) or set(names) != expected:
                  raise SystemExit("invalid candidate ZIP inventory")
              for entry in bundle.infolist():
                  mode = entry.external_attr >> 16
                  if entry.is_dir() or "/" in entry.filename or "\\\\" in entry.filename or entry.filename.startswith("/") or ".." in entry.filename.split("/") or not stat.S_ISREG(mode):
                      raise SystemExit("unsafe candidate ZIP entry")
              bundle.extractall(destination)
          PYTHON
          expected_artifact="$(printf '%s\\n' "$private/artifact/sub2api-controller-$TAG_SHA.tar.gz" "$private/artifact/sub2api-controller-$TAG_SHA.tar.gz.sha256" "$private/artifact/metadata.json" "$private/artifact/consumer-trace.json" | sort)"
          actual_artifact="$(find "$private/artifact" -type f -print | sort)"
          test "$actual_artifact" = "$expected_artifact"
          archive="sub2api-controller-$TAG_SHA.tar.gz"
          checksum="$archive.sha256"
          test "$(basename "$archive")" = "$archive"
          test "$(basename "$checksum")" = "$checksum"
          [[ "$archive" =~ ^sub2api-controller-[0-9a-f]{40}\.tar\.gz$ ]]
          [[ "$checksum" =~ ^sub2api-controller-[0-9a-f]{40}\.tar\.gz\.sha256$ ]]
          (cd "$private/artifact" && sha256sum -c "$checksum")
          actual_sha="$(sha256sum "$private/artifact/$archive" | cut -d ' ' -f1)"
          test "$(cat "$private/artifact/$checksum")" = "$actual_sha  $archive"
          node - "$private/artifact/metadata.json" "$TAG_SHA" "$run_id" "$actual_sha" "$archive" <<'NODE'
          const fs = require("node:fs");
          const [path, sha, runId, hash, archive] = process.argv.slice(2);
          const metadata = JSON.parse(fs.readFileSync(path, "utf8"));
          const ids = ["verify", "host-controller", "engine-graph", "provider-ssh", "provider-runtime", "provider-import"];
          const symbols = { verify: ["test/environment-program-target.test.ts::targets the environment program without an infra fallback"], "host-controller": ["TestRegisterFoundationGraph", "TestRegisterPreservesComputedUpstashOutputs", "TestConfigAndHostCheckPreservePropertyClassesWithoutEffects", "TestRunUsesFixedSSHArgvAndFramedStdin", "TestRunOperationHoldsLockAcrossEffectAndResponseLossRetry", "TestStdioProcessExitsAfterOneFrameAndRejectsTwo", "TestRunPulumiPlanStagesPrivateStackAndKeepsPassphraseOutOfPulumi"], "engine-graph": ["TestEngineGraphFailureStopsPublication", "TestEngineGraphReadyPublishesAfterOrderedHosts", "TestEngineGraphMaintenanceUpdateKeepsHostsAndRemovesPublication", "TestEngineConfiguredServerCountZero", "TestEngineConfiguredServerCountOneTwo", "TestEngineAppPlacementOneReadyFailure", "TestEngineManagedUpstashPreviewPreservesComputedSecretProjection", "TestEngineGraphPartialCheckpointKeepsSuccessfulPredecessor", "TestEngineManagedUpstashStateIsProtectedAndRetained"], "provider-ssh": ["TestProviderProcessUsesScriptedSSHTransport", "TestProtocolFrameBoundaries", "TestLoopbackStrictKnownHostAndOptionTerminator"], "provider-runtime": ["TestProviderProcessReachesSharedTemporaryRuntimeServe", "TestProviderLifecycleWithHostProcessTempRuntime"], "provider-import": ["TestEngineImportPreviewIsNoOpOrAcceptedDiff"] };
          if (JSON.stringify(Object.keys(metadata).sort()) !== JSON.stringify(["archive", "gate", "gateSymbols", "requiredGateIds", "runId", "runUrl", "sha", "sha256"].sort()) || JSON.stringify(Object.keys(metadata.gateSymbols || {}).sort()) !== JSON.stringify([...ids].sort()) || metadata.sha !== sha || String(metadata.runId) !== runId || metadata.runUrl !== "https://github.com/" + process.env.GITHUB_REPOSITORY + "/actions/runs/" + runId || metadata.gate !== "target-release" || metadata.archive !== archive || metadata.sha256 !== hash || JSON.stringify(metadata.requiredGateIds) !== JSON.stringify(ids) || JSON.stringify(metadata.gateSymbols) !== JSON.stringify(symbols)) throw new Error("candidate metadata mismatch");
          NODE
          bash scripts/release-bundle.sh verify "$private/artifact/$archive"
          consumer="$RUNNER_TEMP/target-release-consumer-$TAG_SHA"
          mkdir -p "$consumer"
          tar -xzf "$private/artifact/$archive" -C "$consumer"
          expected_files="$(printf '%s\\n' "$consumer/bundle/Pulumi.production.example.yaml" "$consumer/bundle/Pulumi.yaml" "$consumer/bundle/README.md" "$consumer/bundle/artifacts/sub2api-host/manifest.json" "$consumer/bundle/artifacts/sub2api-host/sub2api-host-linux-amd64" "$consumer/bundle/artifacts/sub2api-host/sub2api-host-linux-arm64" "$consumer/bundle/bin/go" "$consumer/bundle/bin/pulumi" "$consumer/bundle/bin/pulumi-program" "$consumer/bundle/bin/pulumi-resource-sub2api-host" "$consumer/bundle/bin/sub2api-deploy" "$consumer/bundle/go.mod" "$consumer/bundle/scripts/pulumi-plugins/cloudflare/pulumi-plugin.json" "$consumer/bundle/scripts/pulumi-plugins/upstash/pulumi-plugin.json")"
          actual_files="$(find "$consumer" -type f -print | sort)"
          test "$actual_files" = "$expected_files"
          host_amd64_sha="$(sha256sum "$consumer/bundle/artifacts/sub2api-host/sub2api-host-linux-amd64" | cut -d ' ' -f1)"
          node - "$private/artifact/consumer-trace.json" "$TAG_SHA" "$host_amd64_sha" <<'NODE'
          const fs = require("node:fs");
          const [path, sha, hash] = process.argv.slice(2);
          const trace = JSON.parse(fs.readFileSync(path, "utf8"));
          if (JSON.stringify(Object.keys(trace).sort()) !== JSON.stringify(["hostAMD64SHA256", "providerCreate", "sha"].sort()) || trace.sha !== sha || trace.providerCreate !== "pass" || trace.hostAMD64SHA256 !== hash) throw new Error("candidate Provider Create trace mismatch");
          NODE
          forbidden='AKIA|-----BEGIN [A-Z ]*PRIVATE KEY-----|"authorization"\\s*:|sub2api-host-v1 |"secrets"\\s*:'
          for artifact_file in "$private/artifact/metadata.json" "$private/artifact/consumer-trace.json" "$consumer/bundle/Pulumi.production.example.yaml" "$consumer/bundle/Pulumi.yaml" "$consumer/bundle/README.md" "$consumer/bundle/go.mod" "$consumer/bundle/artifacts/sub2api-host/manifest.json" "$consumer/bundle/scripts/pulumi-plugins/cloudflare/pulumi-plugin.json" "$consumer/bundle/scripts/pulumi-plugins/upstash/pulumi-plugin.json"; do
            if grep -qE "$forbidden" "$artifact_file"; then exit 1; else s=$?; test "$s" -eq 1 || exit 1; fi
          done

      - name: Publish validated candidate
        env:
          GH_TOKEN: \${{ github.token }}
        run: |
          set -euo pipefail
          private="$RUNNER_TEMP/target-release-$TAG_SHA/artifact"
          [[ "$TAG_SHA" =~ ^[0-9a-f]{40}$ ]]
          [[ "$TAG_NAME" =~ ^v[0-9A-Za-z._-]+$ ]]
          test "$(git rev-parse "refs/tags/\${TAG_NAME}^{commit}")" = "$TAG_SHA"
          tag_ref="$(gh api "repos/$GITHUB_REPOSITORY/git/ref/tags/$TAG_NAME")"
          TAG_REF="$tag_ref" TAG_NAME="$TAG_NAME" node <<'NODE' > "$RUNNER_TEMP/tag-object.txt"
          const ref = JSON.parse(process.env.TAG_REF);
          if (ref.ref !== "refs/tags/" + process.env.TAG_NAME || !ref.object || !/^(commit|tag)$/.test(ref.object.type) || !/^[0-9a-f]{40}$/.test(ref.object.sha)) throw new Error("invalid remote tag ref");
          process.stdout.write(ref.object.type + " " + ref.object.sha + "\\n");
          NODE
          read -r tag_type tag_sha < "$RUNNER_TEMP/tag-object.txt"
          seen_tag_shas=" $tag_sha "
          for _ in 1 2 3 4 5; do
            if test "$tag_type" = commit; then break; fi
            tag_object="$(gh api "repos/$GITHUB_REPOSITORY/git/tags/$tag_sha")"
            TAG_OBJECT="$tag_object" node <<'NODE' > "$RUNNER_TEMP/tag-object.txt"
            const tag = JSON.parse(process.env.TAG_OBJECT);
            if (!tag.object || !/^(commit|tag)$/.test(tag.object.type) || !/^[0-9a-f]{40}$/.test(tag.object.sha)) throw new Error("invalid annotated tag object");
            process.stdout.write(tag.object.type + " " + tag.object.sha + "\\n");
            NODE
            read -r next_type next_sha < "$RUNNER_TEMP/tag-object.txt"
            case "$seen_tag_shas" in *" $next_sha "*) exit 1 ;; esac
            seen_tag_shas+="$next_sha "
            tag_type="$next_type"
            tag_sha="$next_sha"
          done
          test "$tag_type" = commit
          test "$tag_sha" = "$TAG_SHA"
          gh release create "$TAG_NAME" "$private/sub2api-controller-$TAG_SHA.tar.gz" "$private/sub2api-controller-$TAG_SHA.tar.gz.sha256" "$private/metadata.json" --repo "$GITHUB_REPOSITORY" --verify-tag --generate-notes --title "$TAG_NAME"
`;

describe("release promotion contract", () => {
  it("matches the complete exact-SHA candidate-promotion workflow", () => {
    expect(normalized(workflow)).toBe(normalized(expectedReleaseWorkflow));
  });
});
