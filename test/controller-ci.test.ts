import { readFileSync } from "node:fs";
import { describe, expect, it } from "vitest";

const workflow = readFileSync(new URL("../.github/workflows/ci.yml", import.meta.url), "utf8");
const liveHostSandbox = readFileSync(new URL("../internal/integration/providerruntime/testdata/live-host-sandbox.sh", import.meta.url), "utf8");
const liveRuntime = readFileSync(new URL("../internal/integration/providerruntime/testdata/live-runtime.sh", import.meta.url), "utf8");
const liveRuntimeTest = readFileSync(new URL("../internal/integration/providerruntime/live_mx_allowlist_linux_test.go", import.meta.url), "utf8");
const jobs = workflow.slice(workflow.indexOf("\njobs:\n"));
const gateSymbols = {
  verify: ["test/environment-program-target.test.ts::targets the environment program without an infra fallback"],
  "host-controller": ["TestRegisterFoundationGraph", "TestRegisterPreservesComputedUpstashOutputs", "TestConfigAndHostCheckPreservePropertyClassesWithoutEffects", "TestRunUsesFixedSSHArgvAndFramedStdin", "TestRunOperationHoldsLockAcrossEffectAndResponseLossRetry", "TestStdioProcessExitsAfterOneFrameAndRejectsTwo", "TestRunPulumiPlanStagesPrivateStackAndKeepsPassphraseOutOfPulumi"],
  "engine-graph": ["TestEngineGraphFailureStopsPublication", "TestEngineGraphReadyPublishesAfterOrderedHosts", "TestEngineGraphMaintenanceUpdateKeepsHostsAndRemovesPublication", "TestEngineConfiguredServerCountZero", "TestEngineConfiguredServerCountOneTwo", "TestEngineAppPlacementOneReadyFailure", "TestEngineManagedUpstashPreviewPreservesComputedSecretProjection", "TestEngineGraphPartialCheckpointKeepsSuccessfulPredecessor", "TestEngineManagedUpstashStateIsProtectedAndRetained", "TestEngineGraphCrossHostDataAdmissionOrderingAndFailureStop", "TestEngineGraphCrossHostDataRemovalIsReverseStaged", "TestEngineGraphTraceArtifactIsSanitizedJSONL"],
  "provider-ssh": ["TestProviderProcessUsesScriptedSSHTransport", "TestProtocolFrameBoundaries", "TestLoopbackStrictKnownHostAndOptionTerminator"],
  "provider-runtime": ["TestProviderProcessReachesSharedTemporaryRuntimeServe", "TestProviderLifecycleWithHostProcessTempRuntime", "TestProviderRuntimeCrossHostDataAdmissionLive"],
  "provider-import": ["TestEngineImportPreviewIsNoOpOrAcceptedDiff"],
};

function selectorFor(job: string): string[] {
  const matches = [...jobs.matchAll(/^  ([a-z-]+):$/gm)];
  const index = matches.findIndex((match) => match[1] === job);
  const start = matches[index]?.index;
  const next = matches[index + 1]?.index;
  if (start === undefined) return [];
  const section = jobs.slice(start, next);
  const match = section.match(/tests='\^\(([^']+)\)\$'/);
  return match?.[1].split("|") ?? [];
}

describe("Task4 CI contracts", () => {
  it("keeps exactly the eight required gates and exact checkout binding", () => {
    expect([...jobs.matchAll(/^  ([a-z-]+):$/gm)].map((match) => match[1])).toEqual(["verify", "host-controller", "engine-graph", "provider-ssh", "provider-runtime", "provider-import", "target-release"]);
    expect(workflow).toContain('name: Host Controller (${{ matrix.arch }})');
    expect(workflow.match(/ref: \$\{\{ env\.TARGET_SHA \}\}/g)).toHaveLength(7);
    expect(workflow).toContain("needs: [verify, host-controller, engine-graph, provider-ssh, provider-runtime, provider-import]");
  });

  it("keeps every required gate symbol in exact selectors and promotion metadata", () => {
    expect(selectorFor("host-controller")).toEqual(gateSymbols["host-controller"]);
    expect(selectorFor("engine-graph")).toEqual(gateSymbols["engine-graph"]);
    expect(selectorFor("provider-ssh")).toEqual(gateSymbols["provider-ssh"]);
    expect(selectorFor("provider-runtime")).toEqual(gateSymbols["provider-runtime"].slice(0, 2));
    expect(selectorFor("provider-import")).toEqual(gateSymbols["provider-import"]);
    for (const symbols of Object.values(gateSymbols)) for (const symbol of symbols) expect(workflow).toContain(symbol);
    expect(workflow).toContain("runId:process.env.RUN_ID");
    expect(workflow).toContain("runUrl:process.env.RUN_URL");
  });

  it("requires all Engine symbols without skip and preserves sanitized persisted evidence", () => {
    expect(workflow).toContain("required test did not pass");
    expect(workflow).toContain("ENGINE_GRAPH_TRACE_DIR");
    expect(workflow).toContain("unexpected persisted trace inventory");
    expect(workflow).toContain("const traceTests=[");
    for (const traceTest of [
      "TestEngineAppPlacementOneReadyFailure/alpha_failure_blocks_publication",
      "TestEngineAppPlacementOneReadyFailure/alpha_ready_publishes_once",
      "TestEngineGraphCrossHostDataAdmissionOrderingAndFailureStop/admission_then_App_then_publication",
      "TestEngineGraphCrossHostDataAdmissionOrderingAndFailureStop/data_failure_stops_App_and_publication",
      "TestEngineGraphCrossHostDataAdmissionOrderingAndFailureStop/App_failure_stops_publication",
    ]) expect(workflow).toContain(`'${traceTest}'`);
    expect(workflow).not.toContain("'TestEngineAppPlacementOneReadyFailure_alpha_failure_blocks_publication'");
    expect(workflow).toContain("crypto.createHash('sha256').update(t).digest('hex')");
    expect(workflow).toContain("invalid persisted trace mode");
    expect(workflow).toContain("invalid persisted trace directory mode");
    expect(workflow).toContain("persisted trace must contain at least one event");
    expect(workflow).toContain("text.split('\\n').filter(Boolean)");
    expect(workflow).not.toContain("lines.length!==1");
    expect(workflow).toContain("stat.isSymbolicLink()");
    expect(workflow).toContain("(stat.mode&0o777)!==0o600");
    expect(workflow).toContain('mkdir -m 0700 "$trace"');
    expect(workflow).toContain("invalid persisted trace schema");
    expect(workflow).toContain("persisted trace is not sanitized");
    expect(workflow).not.toContain("files.length!==3");
    expect(workflow).toContain("engine-graph-evidence-${{ env.TARGET_SHA }}");
  });

  it("keeps scoped JSON no-skip evidence and baseline full checks for every required Go gate", () => {
    expect(workflow).toContain("host-controller-${TARGET_SHA}-${{ matrix.arch }}.jsonl");
    expect(workflow).toContain("host-controller-safe-${TARGET_SHA}-${{ matrix.arch }}");
    expect(workflow).toContain("provider-ssh-safe-${TARGET_SHA}");
    expect(workflow).toContain("provider-runtime-safe-$TARGET_SHA");
    expect(workflow).toContain("provider-import-safe-${TARGET_SHA}");
    expect(workflow).toContain("console.log(`${test}: ${action}`)");
    expect(workflow).toContain("console.log(`${t}: ${action}`)");
    expect(workflow).toContain('test "$status" -eq 0');
    expect(workflow).toContain("go vet ./internal/... ./cmd/...");
    expect(workflow).toContain("go test -race -count=1 $(go list ./internal/... ./cmd/... | grep -v '/internal/integration/providerruntime$')");
    expect(workflow).toContain("go test -race -json -count=1 -timeout=15m");
    expect(workflow).toContain("provider-runtime-safe-$TARGET_SHA");
    expect(workflow).toContain("${safe}/${process.env.MODE}.jsonl");
    expect(workflow).toContain("host-controller-evidence-${{ env.TARGET_SHA }}-${{ matrix.arch }}");
    expect(workflow).toContain("provider-ssh-evidence-${{ env.TARGET_SHA }}");
    expect(workflow).toContain("provider-import-evidence-${{ env.TARGET_SHA }}");
  });

  it("runs the root-preserved App live fixture only from the offline exact candidate", () => {
    expect(workflow).toContain("TestProviderRuntimeCrossHostDataAdmissionLive");
    expect(workflow).toContain("SUB2API_PROVIDER_RUNTIME_LIVE=1");
    expect(workflow).toContain("sudo unshare --mount --net --propagation private true");
    expect(workflow).toContain("openssh-server nftables iproute2 util-linux postgresql-client redis-tools sudo openssl");
    expect(workflow).not.toMatch(/apt-get install[^\n]*docker\.io/);
    expect(workflow).toContain("unshare nsenter psql redis-cli openssl");
    expect(workflow).toContain('command -v "$tool"');
    expect(workflow).toContain("sudo docker pull postgres:18-alpine");
    expect(workflow).toContain("sudo docker pull redis:8-alpine");
    expect(workflow).toContain("sub2api-live-app:mx-allowlist");
    expect(workflow).toContain("internal/integration/providerruntime/testdata/live-app.Dockerfile");
    expect(workflow).toContain("sudo docker save postgres:18-alpine redis:8-alpine sub2api-live-app:mx-allowlist");
    expect(workflow).toContain("sudo --preserve-env=CI,TARGET_SHA,GOCACHE,GOMODCACHE,GOPATH,GOENV env");
    expect(workflow).toContain('mkdir -m 0700 "$trace"');
    expect(workflow).toContain("(stat.mode&0o777)!==0o600");
    expect(workflow).toContain("dataHostPass");
    expect(workflow).toContain("appHostPass");
    expect(workflow).toContain("appDataEnvironmentAuthenticated");
    expect(workflow).toContain("appReadyAfterData");
    expect(workflow).toContain("Object.values(e).every(v=>typeof v!=='boolean'||v)");
    expect(workflow).toContain("providerRuntimeCrossHostAppLive:'data-then-app-pass'");
    expect(workflow).toContain("for _ in $(seq 1 45)");
    expect(workflow).toContain("provider-runtime-dockerd-${TARGET_SHA}.pid");
    expect(workflow).toContain("provider-runtime-dockerd-${TARGET_SHA}.log");
    expect(liveHostSandbox).toContain('printf \'%s\\n\' \'{}\' >"$root/$name/daemon.json"');
    expect(liveHostSandbox).toContain('dockerd --config-file "$root/$name/daemon.json" --storage-driver vfs --data-root "$root/$name/docker" --exec-root /var/run/sub2api-docker');
    expect(liveHostSandbox).not.toContain('--exec-root "$root/$name/run"');
    expect(liveHostSandbox).not.toContain('"$root/$name/run"');
    expect(liveHostSandbox).not.toContain("--storage-driver overlay");
    expect(liveHostSandbox).toContain('docker -H unix:///var/run/docker.sock "$@"');
    expect(liveHostSandbox).toContain("until docker_cli info >/dev/null 2>&1; do");
    expect(liveHostSandbox).toContain('docker_cli load --input "$images" >/dev/null 2>&1');
    expect(liveHostSandbox).toContain("docker_cli image inspect postgres:18-alpine redis:8-alpine sub2api-live-app:mx-allowlist >/dev/null 2>&1");
    expect(liveHostSandbox).toContain("stage=docker-timeout\n    exit 1");
    const dataWait = 'wait_ready data "$data_pid"';
    const appStart = '"$root/host-sandbox.sh" app &';
    const appWait = 'wait_ready app "$app_pid" "$data_pid"';
    for (const command of [dataWait, appStart, appWait]) expect(liveRuntime).toContain(command);
    expect(liveRuntime.indexOf(dataWait)).toBeLessThan(liveRuntime.indexOf(appStart));
    expect(liveRuntime.indexOf(appStart)).toBeLessThan(liveRuntime.indexOf(appWait));
    expect(liveRuntime).toContain('[ -z "$peer_pid" ] || kill -0 "$peer_pid"');
    expect(liveRuntime).toContain('[ "$i" -lt 120 ] || exit 1');
    const bothHostsLive = 'kill -0 "$data_pid" 2>/dev/null\nkill -0 "$app_pid" 2>/dev/null';
    const testInvocation = '"$test_binary" "$@"';
    for (const command of [bothHostsLive, testInvocation]) expect(liveRuntime).toContain(command);
    expect(liveRuntime.indexOf(bothHostsLive)).toBeLessThan(liveRuntime.indexOf(testInvocation));
    expect(liveRuntimeTest).toContain("cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}");
    expect(liveRuntimeTest).toContain("syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)");
    expect(workflow).toContain("Finalize Provider Runtime private files");
    expect(workflow).toContain("if: always()\n        uses: actions/upload-artifact@v4");
    expect(workflow).toContain("- name: Upload exact-SHA intermediate candidate\n        if: success()");
    expect(workflow).toContain("invalid live evidence");
    expect(workflow).toContain("const allowedLiveStages = new Set([");
    expect(workflow).toContain("'app-docker-network'");
    expect(workflow).toContain("'data-docker-timeout'");
    expect(workflow).toContain("event.Test === test && typeof event.Output === 'string'");
    expect(workflow).toContain("live namespace fixture failed: ([a-z-]+)");
    expect(workflow).toContain("console.log(`${test} stage: ${stage}`)");
    expect(workflow).not.toContain("console.log(event.Output)");
    expect(workflow).not.toContain("argv|frame|sql|acl|nft|state");
    expect(workflow).not.toContain("-race -json -count=1 -timeout=15m -run '^TestProviderRuntimeCrossHostDataAdmissionLive$");
    expect(workflow).toContain("test -json -count=1 -timeout=11m -run '^TestProviderRuntimeCrossHostDataAdmissionLive$'");
  });

  it("hands the tested candidate to Target Release without rebuilding it", () => {
    expect(workflow).toContain("name: provider-runtime-candidate-${{ env.TARGET_SHA }}");
    expect(workflow).toContain("uses: actions/download-artifact@v4");
    expect(workflow).toContain("name: provider-runtime-candidate-${{ env.TARGET_SHA }}");
    const target = workflow.slice(workflow.indexOf("  target-release:"));
    expect(target).not.toMatch(/go build|build-pulumi-release|release-bundle\.sh assemble/);
    expect(target).toContain("sha256sum -c");
    expect(target).toContain("invalid consumed live trace");
    expect(target).toContain("providerRuntimeCrossHostAppLive");
    expect(target).toContain("name: target-release-${{ env.TARGET_SHA }}");
  });
});
