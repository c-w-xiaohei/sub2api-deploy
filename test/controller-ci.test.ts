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
    expect(workflow).toContain("required test did not pass");
  });

  it("uploads SHA-bound JSON events and a common trace intake", () => {
    expect(workflow).toContain("evidence/${TARGET_SHA}");
    expect(workflow).toContain("trace/");
    expect(workflow).toMatch(/name:\s*controller-evidence-\$\{\{\s*env\.TARGET_SHA\s*\}\}/);
    expect(workflow).toContain("events.jsonl");
    expect(workflow).toContain("metadata.json");
  });
});
