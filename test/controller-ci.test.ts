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
});
