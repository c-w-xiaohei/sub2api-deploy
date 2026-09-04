import { readFileSync } from "node:fs";
import { describe, expect, it } from "vitest";

const workflow = readFileSync(new URL("../.github/workflows/release.yml", import.meta.url), "utf8");
const gateSymbols = ["TestRegisterFoundationGraph", "TestRegisterPreservesComputedUpstashOutputs", "TestConfigAndHostCheckPreservePropertyClassesWithoutEffects", "TestRunUsesFixedSSHArgvAndFramedStdin", "TestRunOperationHoldsLockAcrossEffectAndResponseLossRetry", "TestStdioProcessExitsAfterOneFrameAndRejectsTwo", "TestRunPulumiPlanStagesPrivateStackAndKeepsPassphraseOutOfPulumi", "TestEngineGraphFailureStopsPublication", "TestEngineGraphReadyPublishesAfterOrderedHosts", "TestEngineGraphMaintenanceUpdateKeepsHostsAndRemovesPublication", "TestEngineConfiguredServerCountZero", "TestEngineConfiguredServerCountOneTwo", "TestEngineAppPlacementOneReadyFailure", "TestEngineManagedUpstashPreviewPreservesComputedSecretProjection", "TestEngineGraphPartialCheckpointKeepsSuccessfulPredecessor", "TestEngineManagedUpstashStateIsProtectedAndRetained", "TestEngineGraphCrossHostDataAdmissionOrderingAndFailureStop", "TestEngineGraphCrossHostDataRemovalIsReverseStaged", "TestEngineGraphTraceArtifactIsSanitizedJSONL", "TestProviderProcessUsesScriptedSSHTransport", "TestProtocolFrameBoundaries", "TestLoopbackStrictKnownHostAndOptionTerminator", "TestProviderProcessReachesSharedTemporaryRuntimeServe", "TestProviderLifecycleWithHostProcessTempRuntime", "TestProviderRuntimeCrossHostDataAdmissionLive", "TestEngineImportPreviewIsNoOpOrAcceptedDiff"];

describe("release promotion contract", () => {
  it("requires exactly eight successful CI gates and the four final candidate files", () => {
    expect(workflow).toContain('"Verify", "Host Controller (amd64)", "Host Controller (arm64)", "Engine Graph", "Provider SSH", "Provider Runtime", "Provider Import", "Target Release"');
    expect(workflow).toContain('expected = {f"sub2api-controller-{sha}.tar.gz", f"sub2api-controller-{sha}.tar.gz.sha256", "metadata.json", "consumer-trace.json"}');
    expect(workflow).toContain("candidate artifact missing or duplicate");
    expect(workflow).toContain("sha256sum -c");
  });

  it("validates the exact-SHA App live trace, hashes, and credential-only scans before promotion", () => {
    for (const symbol of gateSymbols) expect(workflow).toContain(symbol);
    expect((workflow.match(/const symbols = \{[^\n]+/g) ?? [""])[0]).toContain("TestEngineGraphFailureStopsPublication");
    expect((workflow.match(/const symbols = \{[^\n]+/g) ?? [""])[0]).toContain("TestLoopbackStrictKnownHostAndOptionTerminator");
    expect(workflow).toContain("TestEngineGraphCrossHostDataAdmissionOrderingAndFailureStop");
    expect(workflow).toContain("TestEngineGraphCrossHostDataRemovalIsReverseStaged");
    expect(workflow).toContain("TestEngineGraphTraceArtifactIsSanitizedJSONL");
    expect(workflow).toContain("TestProviderRuntimeCrossHostDataAdmissionLive");
    expect(workflow).toContain("providerRuntimeCrossHostAppLive");
    expect(workflow).toContain('!== "data-then-app-pass"');
    expect(workflow).not.toContain("providerRuntimeCrossHostLive");
    expect(workflow).toContain("providerSHA256");
    expect(workflow).toContain("hostAMD64SHA256");
    expect(workflow).toContain("candidate metadata mismatch");
    expect(workflow).toContain('"runId", "runUrl"');
    expect(workflow).toContain("metadata.runId !== runId");
    expect(workflow).toContain("metadata.runUrl !== runUrl");
    expect(workflow).toContain("process.env.GITHUB_SERVER_URL");
    expect(workflow).toContain("CrossHost.*(?:Password|Secret)");
    expect(workflow).toContain('generated_ip=\'(?:[0-9]{1,3}\\.){3}[0-9]{1,3}\'');
    expect(workflow).toContain('for evidence_file in "$private/artifact/metadata.json" "$private/artifact/consumer-trace.json"');
    expect(workflow).not.toContain("CrossHost.*(?:Password|Secret)|(?:[0-9]{1,3}");
    expect(workflow).not.toContain("argv|frame|sql|acl|nft|state");
    expect(workflow).toContain("gh release create");
  });

  it("allows documented example addresses in the bundle while keeping generated evidence IP-clean", () => {
    const credentialScan = workflow.slice(workflow.indexOf("forbidden="), workflow.indexOf("generated_ip="));
    const bundleScan = workflow.slice(workflow.indexOf("for artifact_file"), workflow.indexOf("generated_ip="));
    const evidenceScan = workflow.slice(workflow.indexOf("generated_ip="), workflow.indexOf("- name: Publish validated candidate"));
    expect(bundleScan).toContain('"$consumer/bundle/README.md"');
    expect(bundleScan).toContain('"$consumer/bundle/Pulumi.production.example.yaml"');
    expect(bundleScan).not.toContain("[0-9]{1,3}");
    expect(evidenceScan).toContain('"$private/artifact/metadata.json"');
    expect(evidenceScan).toContain('"$private/artifact/consumer-trace.json"');
    expect(evidenceScan).toContain("[0-9]{1,3}");
    expect(credentialScan).toContain("AKIA|-----BEGIN [A-Z ]*PRIVATE KEY");
  });
});
