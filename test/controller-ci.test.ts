import { readFileSync } from "node:fs";
import { describe, expect, it } from "vitest";

const workflow = readFileSync(new URL("../.github/workflows/ci.yml", import.meta.url), "utf8");

describe("Host controller CI evidence contract", () => {
  it("binds evidence to an explicit exact target SHA", () => {
    expect(workflow).toContain("target_sha:");
    expect(workflow).toContain("github.event.pull_request.head.sha");
    expect(workflow).toContain("github.sha");
    expect(workflow).toMatch(/ref:\s*\$\{\{[^\n]*TARGET_SHA[^\n]*\}\}/);
    expect(workflow).toContain('test "$(git rev-parse HEAD)" = "$TARGET_SHA"');
    expect(workflow).toMatch(/\[\[\s+"\$TARGET_SHA"\s+=~\s+\^\[0-9a-f\]\{40\}\$\s+\]\]/);
  });

  it("prepares the required offline OpenSSH environment without contacting a VPS", () => {
    expect(workflow).toMatch(/apt-get install[^\n]*openssh-server/);
    expect(workflow).toMatch(/command -v sshd/);
    const prerequisite = workflow.indexOf("Prepare offline OpenSSH prerequisites");
    expect(prerequisite).toBeGreaterThan(workflow.indexOf("Test Host controller"));
    expect(prerequisite).toBeGreaterThan(workflow.indexOf("Race-test Host controller"));
  });

  it("records fixed candidate tests and fails skipped or absent evidence", () => {
    for (const symbol of [
      "TestRegisterFoundationGraph",
      "TestRegisterPreservesComputedUpstashOutputs",
      "TestConfigAndHostCheckPreservePropertyClassesWithoutEffects",
      "TestRunUsesFixedSSHArgvAndFramedStdin",
      "TestRunOperationHoldsLockAcrossEffectAndResponseLossRetry",
      "TestStdioProcessExitsAfterOneFrameAndRejectsTwo",
      "TestRunPulumiPlanStagesPrivateStackAndKeepsPassphraseOutOfPulumi",
    ]) {
      expect(workflow).toContain(symbol);
    }
    expect(workflow).toContain("go test -json");
    expect(workflow).toContain('event.Action === "skip"');
    expect(workflow).toContain('event.Action === "pass"');
    expect(workflow).toContain('event.Action === "fail"');
    expect(workflow).toContain("required test did not pass");
    expect(workflow).toContain("test_status=$?");
    expect(workflow).toContain('test "$test_status" -eq 0');
  });

  it("uploads SHA-bound JSON events and a common trace intake", () => {
    expect(workflow).toContain("evidence/${TARGET_SHA}");
    expect(workflow).toContain("trace/");
    expect(workflow).toMatch(/name:\s*controller-evidence-\$\{\{\s*env\.TARGET_SHA\s*\}\}/);
    expect(workflow).toContain("events.jsonl");
    expect(workflow).toContain("metadata.json");
    expect(workflow).toContain("if: always() && matrix.arch == 'amd64'");
    const metadata = workflow.indexOf("metadata.json");
    const events = workflow.indexOf("go test -json");
    const parser = workflow.indexOf("required test did not pass");
    expect(metadata).toBeLessThan(events);
    expect(workflow.indexOf("trace/README.txt")).toBeLessThan(events);
    expect(parser).toBeGreaterThan(events);
  });

  it("runs the exact-SHA engine-graph gate with every implemented engine test", () => {
    for (const symbol of [
      "TestEngineGraphFailureStopsPublication",
      "TestEngineGraphPartialCheckpointKeepsSuccessfulPredecessor",
      "TestEngineManagedUpstashStateIsProtectedAndRetained",
      "TestEngineGraphReadyPublishesAfterOrderedHosts",
      "TestEngineGraphMaintenanceUpdateKeepsHostsAndRemovesPublication",
      "TestEngineConfiguredServerCountZero",
      "TestEngineConfiguredServerCountOneTwo",
      "TestEngineAppPlacementOneReadyFailure",
      "TestEngineManagedUpstashPreviewPreservesComputedSecretProjection",
    ]) {
      expect(workflow).toContain(symbol);
    }

    expect(workflow).toContain("engine-graph");
    expect(workflow).toContain("go test -json");
    expect(workflow).toContain('event.Action === "skip"');
    expect(workflow).toContain('event.Action === "pass"');
    expect(workflow).toContain('event.Action === "fail"');
    expect(workflow).toContain("required test did not pass");
    expect(workflow).toContain("test_status=$?");
    expect(workflow).toContain('test "$test_status" -eq 0');
  });

  it("publishes engine-graph JSON evidence and trace under its exact target SHA", () => {
    const gate = workflow.indexOf("engine-graph");
    expect(gate).toBeGreaterThanOrEqual(0);

    expect(workflow).toContain('sha: process.env.TARGET_SHA');
    expect(workflow).toContain('gate: "engine-graph"');
    expect(workflow).toContain("evidence/${TARGET_SHA}");
    expect(workflow).toMatch(
      /^\s*(?:export\s+)?ENGINE_GRAPH_TRACE_DIR\s*=\s*(?:\$\{GITHUB_WORKSPACE\}\/evidence\/\$\{TARGET_SHA\}\/trace|"\$\{GITHUB_WORKSPACE\}\/evidence\/\$\{TARGET_SHA\}\/trace"|'\$\{GITHUB_WORKSPACE\}\/evidence\/\$\{TARGET_SHA\}\/trace')\s*$/m,
    );
    expect(workflow).toMatch(
      /(?:export\s+ENGINE_GRAPH_TRACE_DIR|ENGINE_GRAPH_TRACE_DIR\s*=\s*[^\n]*go test -json|env:[\s\S]{0,160}ENGINE_GRAPH_TRACE_DIR)[\s\S]*go test -json/,
    );
    expect(workflow).toContain("events.jsonl");
    expect(workflow).toContain("metadata.json");
    const traceFindLine = workflow.split("\n").find((line) => line.includes('find "$ENGINE_GRAPH_TRACE_DIR"'));
    expect(traceFindLine).toBeDefined();
    expect(traceFindLine).toContain("-name '*.jsonl'");
    expect(workflow).toContain('test -n "$trace_files"');
    expect(workflow).toContain('test -s "$trace_file"');
    expect(workflow).toMatch(/name:\s*engine-graph-evidence-\$\{\{\s*env\.TARGET_SHA\s*\}\}/);
    expect(workflow).toMatch(/path:\s*evidence\/\$\{\{\s*env\.TARGET_SHA\s*\}\}\//);
  });

  it("requires a distinct exact-SHA provider-ssh evidence job", () => {
    const start = workflow.indexOf("  provider-ssh:");
    expect(start).toBeGreaterThanOrEqual(0);
    const nextJobMatch = /\n  [a-z][a-z0-9-]*:\s*\n/g;
    nextJobMatch.lastIndex = start + 3;
    const nextJobResult = nextJobMatch.exec(workflow);
    const nextJob = nextJobResult?.index ?? -1;
    const job = workflow.slice(start, nextJob < 0 ? workflow.length : nextJob);

    expect(job).toContain("TestProviderProcessUsesScriptedSSHTransport");
    expect(job).toContain("TestProtocolFrameBoundaries");
    expect(job).toContain("TestLoopbackStrictKnownHostAndOptionTerminator");
    expect(job).toContain("go test -json");
    expect(job).toContain('event.Action === "skip"');
    expect(job).toContain('event.Action === "fail"');
    expect(job).toContain('event.Action === "pass"');
    expect(job).toContain('result ?? "absent"');
    expect(job).toContain("required test did not pass");
    expect(job).toContain("test_status=$?");
    expect(job).toContain('test "$test_status" -eq 0');

    expect(job).toContain("openssh-server");
    expect(job).toContain("command -v sshd");
    expect(job).toMatch(/\[\[\s+"\$TARGET_SHA"\s+=~\s+\^\[0-9a-f\]\{40\}\$\s+\]\]/);
    expect(job).toMatch(/ref:\s*\$\{\{\s*env\.TARGET_SHA\s*\}\}/);
    expect(job).toContain('test "$(git rev-parse HEAD)" = "$TARGET_SHA"');

    expect(job).toContain('gate: "provider-ssh"');
    expect(job).toContain("provider-ssh-evidence-${{ env.TARGET_SHA }}");
    expect(job).toContain("evidence/${{ env.TARGET_SHA }}/");
    expect(job).toContain("events.jsonl");
    expect(job).toContain("metadata.json");
    expect(job).toContain("trace/");
  });

  it("requires a distinct exact-SHA provider-runtime evidence job", () => {
    const start = workflow.indexOf("  provider-runtime:");
    expect(start).toBeGreaterThanOrEqual(0);
    const nextJobMatch = /\n  [a-z][a-z0-9-]*:\s*\n/g;
    nextJobMatch.lastIndex = start + 3;
    const nextJobResult = nextJobMatch.exec(workflow);
    const nextJob = nextJobResult?.index ?? -1;
    const job = workflow.slice(start, nextJob < 0 ? workflow.length : nextJob);

    expect(job).toContain('go-version: "1.25.11"');
    expect(job).toContain("go mod verify");
    expect(job).toContain("openssh-server");
    expect(job).toContain("command -v sshd");
    expect(job).toMatch(/\[\[\s+"\$TARGET_SHA"\s+=~\s+\^\[0-9a-f\]\{40\}\$\s+\]\]/);
    expect(job).toMatch(/ref:\s*\$\{\{\s*env\.TARGET_SHA\s*\}\}/);
    expect(job).toContain('test "$(git rev-parse HEAD)" = "$TARGET_SHA"');

    for (const symbol of [
      "TestProviderProcessReachesSharedTemporaryRuntimeServe",
      "TestProviderLifecycleWithHostProcessTempRuntime",
    ]) {
      expect(job).toContain(symbol);
    }
    expect(job).toContain(
      "tests='^(TestProviderProcessReachesSharedTemporaryRuntimeServe|TestProviderLifecycleWithHostProcessTempRuntime)$'",
    );
    expect(job).toMatch(/^\s*timeout-minutes:\s*25\s*$/m);
    expect(job).toMatch(
      /go test -json -count=1 -timeout=8m -run "\$tests" \.\/internal\/integration\/providerruntime > "\$evidence\/normal-events\.jsonl"\n\s*normal_status=\?/,
    );
    expect(job).toMatch(
      /go test -race -json -count=1 -timeout=10m -run "\$tests" \.\/internal\/integration\/providerruntime > "\$evidence\/race-events\.jsonl"\n\s*race_status=\?/,
    );
    expect(job).not.toMatch(/go test[^\n]*\.\/\.\./);

    for (const [parser, events, status] of [
      ["verify_normal_required_tests", "normal-events.jsonl", "normal_status"],
      ["verify_race_required_tests", "race-events.jsonl", "race_status"],
    ]) {
      const parserCall = `${parser} "$evidence/${events}" "$${status}"`;
      const statusGate = `test "$${status}" -eq 0`;
      const marker = parser === "verify_normal_required_tests" ? "NODE_NORMAL" : "NODE_RACE";
      expect(job).toContain(`${parser}()`);
      expect(job).toContain(`node <<'${marker}'`);
      expect(job).toContain(marker);
      expect(job).toMatch(new RegExp(String.raw`${parser}\(\)\s*\{\s*node <<'${marker}'`));
      const nodeBodyMatch = job.match(
        new RegExp(String.raw`^\s*node <<'${marker}'\s*$\n([\s\S]*?)^\s*${marker}\s*$`, "m"),
      );
      expect(nodeBodyMatch).not.toBeNull();
      const body = nodeBodyMatch?.[1] ?? "";
      expect(body).toContain("JSON.parse(line)");
      expect(body).toContain('if (!line) continue;');
      expect(body).toContain(
        'const terminal = new Map(required.map((name) => [name, "absent"]));',
      );
      expect(body).toContain("for (const name of required)");
      expect(body).toMatch(
        /if \(event\.Action === "skip" \|\| event\.Action === "fail"\) \{[\s\S]*?throw new Error/,
      );
      expect(body).toMatch(/if \(event\.Action === "pass"\) terminal\.set\(event\.Test, "pass"\);/);
      expect(body).toContain('if (result !== "pass")');
      expect(body).toContain("required test did not pass");
      expect(body).toContain("if (!event.Test || !required.includes(event.Test)) continue;");
      expect(job.indexOf(parserCall)).toBeGreaterThan(job.indexOf(`${status}=$?`));
      expect(job.indexOf(statusGate)).toBeGreaterThan(job.indexOf(parserCall));
    }

    expect(job).toContain('evidence="evidence/${TARGET_SHA}/provider-runtime"');
    expect(job).toContain('const root = `evidence/${process.env.TARGET_SHA}/provider-runtime`');
    expect(job).toContain('fs.writeFileSync(`${root}/metadata.json`');
    expect(job).toContain('sha: process.env.TARGET_SHA');
    expect(job).toContain("runUrl: process.env.GITHUB_RUN_URL");
    expect(job).toContain('gate: "provider-runtime"');
    expect(job).toContain("trace/README.txt");
    expect(job).toContain("sanitized fixture traces");
    expect(job).toContain("no secrets or raw request frames");
    expect(job).toContain(
      'trace_files=$(find "$evidence/trace" -type f ! -name README.txt -print)',
    );
    expect(job).toContain('test -n "$trace_files"');
    const traceLoop = job.match(
      /while IFS= read -r trace_file; do([\s\S]*?)done <<< "\$trace_files"/,
    );
    expect(traceLoop).not.toBeNull();
    const traceBody = traceLoop?.[1] ?? "";
    expect(traceBody).toContain('test -s "$trace_file"');
    expect(traceBody).toContain(
      "forbidden='AKIA|-----BEGIN [A-Z ]*PRIVATE KEY-----|\\\"authorization\\\"\\s*:|sub2api-host-v1 |\\\"secrets\\\"\\s*:'",
    );
    expect(traceBody).toMatch(/if grep -E "\$forbidden" "\$trace_file"; then\s*exit 1\s*fi/);

    const upload = job.match(
      /- name: Upload provider-runtime evidence[\s\S]*?if: always\(\)[\s\S]*?uses: actions\/upload-artifact@v4[\s\S]*?name: provider-runtime-evidence-\$\{\{ env\.TARGET_SHA \}\}\s*$[\s\S]*?path: evidence\/\$\{\{ env\.TARGET_SHA \}\}\/provider-runtime\s*$[\s\S]*?if-no-files-found: error/m,
    );
    expect(upload).not.toBeNull();
    expect(job).not.toContain("continue-on-error");
  });
});
