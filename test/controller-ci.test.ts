import { readFileSync } from "node:fs";
import { describe, expect, it } from "vitest";

const workflow = readFileSync(new URL("../.github/workflows/ci.yml", import.meta.url), "utf8");

function normalized(value: string) {
  return value.replace(/[ \t]+$/gm, "").replace(/\n*$/, "\n");
}

function workflowJobs(source: string) {
  const lines = source.split("\n");
  const start = lines.findIndex((line) => line === "jobs:");
  if (start < 0) throw new Error("workflow has no top-level jobs block");
  const end = lines.slice(start + 1).findIndex((line) => line !== "" && !line.startsWith(" "));
  const jobsBlock = lines.slice(start + 1, end < 0 ? undefined : start + 1 + end).join("\n");
  const jobs = new Map<string, string>();
  for (const match of jobsBlock.matchAll(/^  ([a-z][a-z0-9-]*):\s*\n([\s\S]*?)(?=^  [a-z][a-z0-9-]*:\s*\n|(?![\s\S]))/gm)) {
    if (jobs.has(match[1])) throw new Error(`duplicate workflow job: ${match[1]}`);
    jobs.set(match[1], `  ${match[1]}:\n${match[2]}`);
  }
  return jobs;
}

function jobSteps(job: string) {
  return [...job.matchAll(/^      - name:\s*["']?([^"'\n]+?)["']?\s*\n([\s\S]*?)(?=^      - name:|(?![\s\S]))/gm)]
    .map((match) => ({ name: match[1], source: match[0] }));
}

const targetInventory = [
  "Pulumi.production.example.yaml", "Pulumi.yaml", "README.md", "artifacts/sub2api-host/manifest.json",
  "artifacts/sub2api-host/sub2api-host-linux-amd64", "artifacts/sub2api-host/sub2api-host-linux-arm64", "bin/go",
  "bin/pulumi", "bin/pulumi-program", "bin/pulumi-resource-sub2api-host", "bin/sub2api-deploy", "go.mod",
  "scripts/pulumi-plugins/cloudflare/pulumi-plugin.json", "scripts/pulumi-plugins/upstash/pulumi-plugin.json",
];

// The exact candidate job makes every promotion input explicit and forbids an unreviewed execution surface.
const expectedTargetReleaseJob = `  target-release:
    name: Target Release
    needs: [verify, host-controller, engine-graph, provider-ssh, provider-runtime, provider-import]
    runs-on: ubuntu-latest
    timeout-minutes: 30
    steps:
      - name: Check out repository
        uses: actions/checkout@v4
        with:
          ref: \${{ env.TARGET_SHA }}
      - name: Verify exact target SHA
        run: |
          set -euo pipefail
          [[ "$TARGET_SHA" =~ ^[0-9a-f]{40}$ ]]
          test "$(git rev-parse HEAD)" = "$TARGET_SHA"
      - name: Set up Go
        uses: actions/setup-go@v5
        with:
          go-version: "1.25.11"
          cache: true
      - name: Set up Node.js
        uses: actions/setup-node@v4
        with:
          node-version: 22
          cache: npm
      - name: Build and verify target candidate
        run: |
          set -euo pipefail
          candidate_dir="$RUNNER_TEMP/target-release-$TARGET_SHA"
          components="$candidate_dir/components"
          archive_name="sub2api-controller-$TARGET_SHA.tar.gz"
          archive="$candidate_dir/$archive_name"
          bundle="$candidate_dir/bundle"
          mkdir -p "$components"
          go build -trimpath -o "$components/sub2api-deploy" ./cmd/sub2api-deploy
          scripts/build-pulumi-release.sh "$components/pulumi-program"
          go build -trimpath -o "$components/pulumi-resource-sub2api-host" ./cmd/pulumi-resource-sub2api-host
          CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o "$components/sub2api-host-linux-amd64" ./cmd/sub2api-host
          CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -o "$components/sub2api-host-linux-arm64" ./cmd/sub2api-host
          bash scripts/release-bundle.sh assemble "$bundle" "$components" "$TARGET_SHA"
          tar -C "$candidate_dir" -czf "$archive" bundle
          bash scripts/release-bundle.sh verify "$archive"
          (cd "$candidate_dir" && sha256sum "$archive_name" > "$archive_name.sha256")
          (cd "$candidate_dir" && sha256sum -c "$archive_name.sha256")
      - name: Consume isolated target candidate
        run: |
          set -euo pipefail
          candidate_dir="$RUNNER_TEMP/target-release-$TARGET_SHA"
          archive="$candidate_dir/sub2api-controller-$TARGET_SHA.tar.gz"
          consumer="$RUNNER_TEMP/target-release-consumer-$TARGET_SHA"
          rm -rf "$consumer"
          mkdir -p "$consumer"
          tar -xzf "$archive" -C "$consumer"
          expected_files="$(printf '%s\\n'${targetInventory.map((path) => ` "$consumer/bundle/${path}"`).join("")} | sort)"
          actual_files="$(find "$consumer" -type f -print | sort)"
          test "$actual_files" = "$expected_files"
          raw_output="$RUNNER_TEMP/target-release-$TARGET_SHA-provider-create.raw"
          SUB2API_TEST_PROVIDER_BINARY="$consumer/bundle/bin/pulumi-resource-sub2api-host" SUB2API_TEST_RELEASE_ROOT="$consumer/bundle" GOMAXPROCS=2 go test -count=1 -p=1 -run '^TestProviderProcessReachesSharedTemporaryRuntimeServe$' ./internal/integration/providerruntime > "$raw_output" 2>&1
          digest="$(sha256sum "$consumer/bundle/artifacts/sub2api-host/sub2api-host-linux-amd64" | cut -d ' ' -f1)"
          node - "$candidate_dir/consumer-trace.json" "$TARGET_SHA" "$digest" <<'NODE'
          const fs = require("node:fs");
          fs.writeFileSync(process.argv[2], JSON.stringify({ sha: process.argv[3], providerCreate: "pass", hostAMD64SHA256: process.argv[4] }) + "\\n");
          NODE
          rm -f "$raw_output"
          test "$(readelf -h "$consumer/bundle/artifacts/sub2api-host/sub2api-host-linux-amd64" | grep 'Machine:.*Advanced Micro Devices')"
          test "$(readelf -h "$consumer/bundle/artifacts/sub2api-host/sub2api-host-linux-arm64" | grep 'Machine:.*AArch64')"
          test -x "$consumer/bundle/bin/go"
          test -x "$consumer/bundle/bin/pulumi"
          test -x "$consumer/bundle/bin/pulumi-program"
          test -x "$consumer/bundle/bin/pulumi-resource-sub2api-host"
          node - "$consumer/bundle/artifacts/sub2api-host/manifest.json" "$consumer/bundle/scripts/pulumi-plugins/cloudflare/pulumi-plugin.json" "$consumer/bundle/scripts/pulumi-plugins/upstash/pulumi-plugin.json" <<'NODE'
          const fs = require("node:fs");
          const [manifest, cloudflare, upstash] = process.argv.slice(2).map((path) => JSON.parse(fs.readFileSync(path, "utf8")));
          if (manifest.schemaVersion !== 1 || cloudflare.name !== "cloudflare" || upstash.name !== "upstash") throw new Error("candidate consumer metadata mismatch");
          NODE
          forbidden='AKIA|-----BEGIN [A-Z ]*PRIVATE KEY-----|"authorization"\\s*:|sub2api-host-v1 |"secrets"\\s*:'
          for text_file in "$consumer/bundle/Pulumi.production.example.yaml" "$consumer/bundle/Pulumi.yaml" "$consumer/bundle/README.md" "$consumer/bundle/go.mod" "$consumer/bundle/artifacts/sub2api-host/manifest.json" "$consumer/bundle/scripts/pulumi-plugins/cloudflare/pulumi-plugin.json" "$consumer/bundle/scripts/pulumi-plugins/upstash/pulumi-plugin.json" "$candidate_dir/consumer-trace.json"; do
            if grep -qE "$forbidden" "$text_file"; then exit 1; else s=$?; test "$s" -eq 1 || exit 1; fi
          done
      - name: Write target candidate metadata
        run: |
          set -euo pipefail
          candidate_dir="$RUNNER_TEMP/target-release-$TARGET_SHA"
          archive="sub2api-controller-$TARGET_SHA.tar.gz"
          sha256="$(sha256sum "$candidate_dir/$archive" | cut -d ' ' -f1)"
          TARGET_SHA="$TARGET_SHA" GITHUB_RUN_ID="$GITHUB_RUN_ID" GITHUB_RUN_URL="$GITHUB_SERVER_URL/$GITHUB_REPOSITORY/actions/runs/$GITHUB_RUN_ID" ARCHIVE="$archive" SHA256="$sha256" node <<'NODE'
          const fs = require("node:fs");
          const gateSymbols = { verify: ["test/environment-program-target.test.ts::targets the environment program without an infra fallback"], "host-controller": ["TestRegisterFoundationGraph", "TestRegisterPreservesComputedUpstashOutputs", "TestConfigAndHostCheckPreservePropertyClassesWithoutEffects", "TestRunUsesFixedSSHArgvAndFramedStdin", "TestRunOperationHoldsLockAcrossEffectAndResponseLossRetry", "TestStdioProcessExitsAfterOneFrameAndRejectsTwo", "TestRunPulumiPlanStagesPrivateStackAndKeepsPassphraseOutOfPulumi"], "engine-graph": ["TestEngineGraphFailureStopsPublication", "TestEngineGraphReadyPublishesAfterOrderedHosts", "TestEngineGraphMaintenanceUpdateKeepsHostsAndRemovesPublication", "TestEngineConfiguredServerCountZero", "TestEngineConfiguredServerCountOneTwo", "TestEngineAppPlacementOneReadyFailure", "TestEngineManagedUpstashPreviewPreservesComputedSecretProjection", "TestEngineGraphPartialCheckpointKeepsSuccessfulPredecessor", "TestEngineManagedUpstashStateIsProtectedAndRetained"], "provider-ssh": ["TestProviderProcessUsesScriptedSSHTransport", "TestProtocolFrameBoundaries", "TestLoopbackStrictKnownHostAndOptionTerminator"], "provider-runtime": ["TestProviderProcessReachesSharedTemporaryRuntimeServe", "TestProviderLifecycleWithHostProcessTempRuntime"], "provider-import": ["TestEngineImportPreviewIsNoOpOrAcceptedDiff"] };
          fs.writeFileSync(process.env.RUNNER_TEMP + "/target-release-" + process.env.TARGET_SHA + "/metadata.json", JSON.stringify({ sha: process.env.TARGET_SHA, runId: process.env.GITHUB_RUN_ID, runUrl: process.env.GITHUB_RUN_URL, gate: "target-release", archive: process.env.ARCHIVE, sha256: process.env.SHA256, requiredGateIds: ["verify", "host-controller", "engine-graph", "provider-ssh", "provider-runtime", "provider-import"], gateSymbols }) + "\\n");
          NODE
          expected_artifact="$(printf '%s\\n' "$candidate_dir/$archive" "$candidate_dir/$archive.sha256" "$candidate_dir/metadata.json" "$candidate_dir/consumer-trace.json" | sort)"
          actual_artifact="$(find "$candidate_dir" -maxdepth 1 -type f -print | sort)"
          test "$actual_artifact" = "$expected_artifact"
          forbidden='AKIA|-----BEGIN [A-Z ]*PRIVATE KEY-----|"authorization"\\s*:|sub2api-host-v1 |"secrets"\\s*:'
          for text_file in "$candidate_dir/metadata.json" "$candidate_dir/consumer-trace.json"; do
            if grep -qE "$forbidden" "$text_file"; then exit 1; else s=$?; test "$s" -eq 1 || exit 1; fi
          done
      - name: Upload target release candidate
        if: success()
        uses: actions/upload-artifact@v4
        with:
          name: target-release-\${{ env.TARGET_SHA }}
          path: |
            \${{ runner.temp }}/target-release-\${{ env.TARGET_SHA }}/sub2api-controller-\${{ env.TARGET_SHA }}.tar.gz
            \${{ runner.temp }}/target-release-\${{ env.TARGET_SHA }}/sub2api-controller-\${{ env.TARGET_SHA }}.tar.gz.sha256
            \${{ runner.temp }}/target-release-\${{ env.TARGET_SHA }}/metadata.json
            \${{ runner.temp }}/target-release-\${{ env.TARGET_SHA }}/consumer-trace.json
          if-no-files-found: error
`;

function expectedFinalization(gate: string, recordID: string, eventNames: string[], stderrNames: string[], traces: string[], symbols: string[], guard = "always()") {
  return `      - name: Finalize ${gate} evidence
        id: finalize-${gate}
        if: ${guard}
        env:
          RECORD_OUTCOME: \${{ steps.${recordID}.outcome }}
          TARGET_SHA: \${{ env.TARGET_SHA }}
        run: |
          safe="$RUNNER_TEMP/${gate}-safe-\${TARGET_SHA}"
          mkdir -p "$safe/trace"
          trap 'rc=$?; rm -f${[...eventNames, ...stderrNames].map((name) => ` "$RUNNER_TEMP/${gate}-\${TARGET_SHA}-${name}"`).join("")}; exit "$rc"' EXIT
          TARGET_SHA="$TARGET_SHA" RECORD_OUTCOME="$RECORD_OUTCOME" node - "$safe"${eventNames.map((name) => ` "$RUNNER_TEMP/${gate}-\${TARGET_SHA}-${name}"`).join("")} <<'NODE'
          const fs = require("node:fs");
          const [safe, ...events] = process.argv.slice(2);
          const required = new Set(${JSON.stringify(symbols)});
          const trace = [];
          const recordOutcome = process.env.RECORD_OUTCOME;
          if (recordOutcome === "success") {
            for (const path of events) for (const line of fs.readFileSync(path, "utf8").split("\\n")) {
              if (!line) continue;
              const event = JSON.parse(line);
              if (event.Test && required.has(event.Test)) trace.push({ Action: event.Action, Test: event.Test, Elapsed: event.Elapsed });
            }
            for (const name of required) if (!trace.some((event) => event.Test === name && event.Action === "pass")) throw new Error("required sanitized test missing or nonpass");
          }
          fs.writeFileSync(safe + "/metadata.json", JSON.stringify({ sha: process.env.TARGET_SHA, runUrl: process.env.GITHUB_SERVER_URL + "/" + process.env.GITHUB_REPOSITORY + "/actions/runs/" + process.env.GITHUB_RUN_ID, gate: "${gate}", recordOutcome }) + "\\n");
          fs.writeFileSync(safe + "/trace/README.txt", "Sanitized test events only.\\n");
          for (const name of ${JSON.stringify(traces)}) fs.writeFileSync(safe + "/trace/" + name, recordOutcome === "success" ? trace.map((event) => JSON.stringify(event)).join("\\n") + "\\n" : JSON.stringify({ Action: "error", Test: "record", Elapsed: 0, Status: "record-unavailable" }) + "\\n");
          NODE
          if test "$RECORD_OUTCOME" = success; then${stderrNames.map((name) => ` test ! -s "$RUNNER_TEMP/${gate}-\${TARGET_SHA}-${name}";`).join("")} fi
          expected_files=$(printf '%s\\n' "$safe/metadata.json" "$safe/trace/README.txt"${traces.map((trace) => ` "$safe/trace/${trace}"`).join("")})
          actual_files=$(find "$safe" -type f -print | sort)
          test "$actual_files" = "$expected_files"
          forbidden='AKIA|-----BEGIN [A-Z ]*PRIVATE KEY-----|"authorization"\\s*:|sub2api-host-v1 |"secrets"\\s*:'
          for evidence_file in "$safe/metadata.json" "$safe/trace/README.txt"${traces.map((trace) => ` "$safe/trace/${trace}"`).join("")}; do
            if grep -qE "$forbidden" "$evidence_file"; then exit 1; else s=$?; test "$s" -eq 1 || exit 1; fi
          done
          test "$RECORD_OUTCOME" = success || test "$RECORD_OUTCOME" = failure || test "$RECORD_OUTCOME" = cancelled || test "$RECORD_OUTCOME" = skipped`;
}

describe("Task10 CI release candidate contracts", () => {
  it("has unique bounded workflow job slices and no CI-side release authority", () => {
    expect(() => workflowJobs(workflow)).not.toThrow();
    // Static defense-in-depth; top-level contents: read and no actions: write remain authoritative.
    expect(workflow).toMatch(/^permissions:\n  contents: read$/m);
    expect(workflow).not.toMatch(/^  actions:\s*write$/m);
    expect(workflow).not.toMatch(/softprops\/action-gh-release|ncipollo\/release-action|marvinpinto\/action-automatic-releases|actions\/create-release|upload-release-asset|gh\s+release|gh\s+api[^\n]*(POST|PATCH|DELETE)[^\n]*\/(releases|uploads)|(?:POST|PATCH|DELETE)[^\n]*\/(releases|uploads)|contents:\s*write/);
  });

  it("matches the complete exact target-release candidate job", () => {
    expect(normalized(workflowJobs(workflow).get("target-release") ?? "")).toBe(normalized(expectedTargetReleaseJob));
  });

  it("requires every diagnostic evidence upload to follow its bounded sanitized finalization", () => {
    const jobs = workflowJobs(workflow);
    for (const [id, uploadName, artifactName, gate, recordID, eventNames, stderrNames, traces, symbols, guard] of [
      ["host-controller", "Upload controller evidence", "controller-evidence", "controller", "record-controller", ["events.jsonl"], ["stderr.log"], ["controller.jsonl"], ["TestRegisterFoundationGraph", "TestRegisterPreservesComputedUpstashOutputs", "TestConfigAndHostCheckPreservePropertyClassesWithoutEffects", "TestRunUsesFixedSSHArgvAndFramedStdin", "TestRunOperationHoldsLockAcrossEffectAndResponseLossRetry", "TestStdioProcessExitsAfterOneFrameAndRejectsTwo", "TestRunPulumiPlanStagesPrivateStackAndKeepsPassphraseOutOfPulumi"], "always() && matrix.arch == 'amd64'"],
      ["engine-graph", "Upload engine-graph evidence", "engine-graph-evidence", "engine-graph", "record-engine-graph", ["events.jsonl"], ["stderr.log"], ["engine-graph.jsonl"], ["TestEngineGraphFailureStopsPublication", "TestEngineGraphReadyPublishesAfterOrderedHosts", "TestEngineGraphMaintenanceUpdateKeepsHostsAndRemovesPublication", "TestEngineConfiguredServerCountZero", "TestEngineConfiguredServerCountOneTwo", "TestEngineAppPlacementOneReadyFailure", "TestEngineManagedUpstashPreviewPreservesComputedSecretProjection", "TestEngineGraphPartialCheckpointKeepsSuccessfulPredecessor", "TestEngineManagedUpstashStateIsProtectedAndRetained"], "always()"],
      ["provider-ssh", "Upload provider-ssh evidence", "provider-ssh-evidence", "provider-ssh", "record-provider-ssh", ["events.jsonl"], ["stderr.log"], ["provider-ssh.jsonl"], ["TestProviderProcessUsesScriptedSSHTransport", "TestProtocolFrameBoundaries", "TestLoopbackStrictKnownHostAndOptionTerminator"], "always()"],
      ["provider-runtime", "Upload provider-runtime evidence", "provider-runtime-evidence", "provider-runtime", "record-provider-runtime", ["normal-events.jsonl", "race-events.jsonl"], ["stderr.log"], ["normal.jsonl", "race.jsonl"], ["TestProviderProcessReachesSharedTemporaryRuntimeServe", "TestProviderLifecycleWithHostProcessTempRuntime"], "always()"],
      ["provider-import", "Upload provider-import evidence", "provider-import-evidence", "provider-import", "record-provider-import", ["events.jsonl"], ["stderr.log"], ["test.jsonl"], ["TestEngineImportPreviewIsNoOpOrAcceptedDiff"], "always()"],
    ]) {
      const job = jobs.get(id);
      expect(job, `${id} job`).toBeDefined();
      expect(job).toContain(`id: ${recordID}`);
      const record = jobSteps(job ?? "").find((step) => step.source.includes(`id: ${recordID}`));
      expect(record, `${id} record step`).toBeDefined();
      if (id === "host-controller") expect(record?.source).toContain('mkdir -p "evidence/${TARGET_SHA}/trace"');
      for (const rawName of [...eventNames, ...stderrNames]) {
        const raw = `$RUNNER_TEMP/${gate}-\${TARGET_SHA}-${rawName}`;
        expect(record?.source).toContain(raw);
        expect(record?.source).toMatch(new RegExp(`(?:[A-Za-z_][A-Za-z0-9_]*=\")?${raw.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")}\"?`));
        expect(record?.source).toContain(`> "$${rawName.endsWith("stderr.log") ? "stderr" : "events"}"`);
      }
      expect(job).toContain(expectedFinalization(gate, recordID, eventNames, stderrNames, traces, symbols, guard));
      const finalization = job?.indexOf(`id: finalize-${gate}`) ?? -1;
      const uploads = jobSteps(job ?? "").filter((step) => step.source.includes("uses: actions/upload-artifact@v4"));
      const evidenceUploads = uploads.filter((step) => step.name === uploadName);
      expect(evidenceUploads, `${id} named evidence upload`).toHaveLength(1);
      const evidenceUpload = evidenceUploads[0];
      expect(evidenceUpload?.source).toContain(`if: ${guard} && steps.finalize-${gate}.outcome == 'success'`);
      expect(evidenceUpload?.source).toContain(`name: ${artifactName}-\${{ env.TARGET_SHA }}`);
      expect(evidenceUpload?.source).toContain(`path: \${{ runner.temp }}/${gate}-safe-\${{ env.TARGET_SHA }}`);
      expect(evidenceUpload?.source).toContain("if-no-files-found: error");
      for (const upload of uploads) {
        if (upload.name !== "Upload Host controller binaries") {
          expect(upload.source).toContain(`path: \${{ runner.temp }}/${gate}-safe-\${{ env.TARGET_SHA }}`);
        }
      }
      const uploadNameIndex = job?.indexOf(`- name: ${uploadName}`) ?? -1;
      const upload = uploadNameIndex;
      expect(finalization).toBeGreaterThanOrEqual(0);
      expect(uploadNameIndex).toBeGreaterThan(finalization);
      expect(upload).toBeGreaterThan(finalization);
    }
  });
});
