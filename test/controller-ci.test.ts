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

  it("assigns provider-runtime race coverage exclusively to its dedicated job", () => {
    const hostStart = workflow.indexOf("  host-controller:");
    const hostEnd = workflow.indexOf("  engine-graph:", hostStart);
    const hostJob = workflow.slice(hostStart, hostEnd);
    const targetPackage = "github.com/c-w-xiaohei/sub2api-deploy/internal/integration/providerruntime";

    expect(hostJob).toMatch(
      /^\s*package_file="\$RUNNER_TEMP\/host-controller-packages\.txt"\s*$/m,
    );
    expect(hostJob).toContain('if ! go list ./internal/... ./cmd/... > "$package_file"; then');
    expect(hostJob).toContain("exit 1");
    expect(hostJob).toContain('mapfile -t packages < "$package_file"');
    expect(hostJob).toContain('rm -f "$package_file"');
    expect(hostJob).not.toContain("mapfile -t packages < <(go list ./internal/... ./cmd/...)");
    expect(hostJob.indexOf('if ! go list ./internal/... ./cmd/... > "$package_file"; then')).toBeLessThan(
      hostJob.indexOf('mapfile -t packages < "$package_file"'),
    );
    expect(hostJob.indexOf('mapfile -t packages < "$package_file"')).toBeLessThan(
      hostJob.indexOf('rm -f "$package_file"'),
    );
    expect(hostJob.indexOf('rm -f "$package_file"')).toBeLessThan(
      hostJob.indexOf('go test -race -count=1 "${filtered[@]}"'),
    );
    expect(hostJob).toContain(`target_package=\"${targetPackage}\"`);
    expect(hostJob).toContain('if [[ "$package" == "$target_package" ]]; then');
    expect(hostJob).toContain("found=true");
    expect(hostJob).toContain("filtered=()");
    expect(hostJob).toContain('filtered+=("$package")');
    expect(hostJob).toContain('test "$found" = true');
    expect(hostJob).toContain('test "${#filtered[@]}" -gt 0');
    expect(hostJob).toContain('go test -race -count=1 "${filtered[@]}"');
    expect(hostJob).not.toContain("go test -race -count=1 ./internal/... ./cmd/...");

    const runtimeStart = workflow.indexOf("  provider-runtime:");
    expect(runtimeStart).toBeGreaterThanOrEqual(0);
    const runtimeJob = workflow.slice(runtimeStart);
    expect(runtimeJob).toContain("go test -race -json -count=1 -timeout=15m -run \"$tests\" ./internal/integration/providerruntime");
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
    expect(job).toMatch(/^\s*timeout-minutes:\s*30\s*$/m);
    expect(job).toMatch(
      /^\s*normal_events="\$RUNNER_TEMP\/provider-runtime-\$\{TARGET_SHA\}-normal-events\.jsonl"\s*$/m,
    );
    expect(job).toMatch(
      /^\s*normal_stderr="\$RUNNER_TEMP\/provider-runtime-\$\{TARGET_SHA\}-normal-stderr\.log"\s*$/m,
    );
    expect(job).toMatch(
      /^\s*race_events="\$RUNNER_TEMP\/provider-runtime-\$\{TARGET_SHA\}-race-events\.jsonl"\s*$/m,
    );
    expect(job).toMatch(
      /^\s*race_stderr="\$RUNNER_TEMP\/provider-runtime-\$\{TARGET_SHA\}-race-stderr\.log"\s*$/m,
    );
    expect(job).toMatch(
      /go test -json -count=1 -timeout=8m -run "\$tests" \.\/internal\/integration\/providerruntime > "\$normal_events" 2> "\$normal_stderr"\n\s*normal_status=\$\?/,
    );
    expect(job).toMatch(
      /go test -race -json -count=1 -timeout=15m -run "\$tests" \.\/internal\/integration\/providerruntime > "\$race_events" 2> "\$race_stderr"\n\s*race_status=\$\?/,
    );
    expect(job).toMatch(
      /extract_sanitized_trace "\$normal_events" "\$evidence\/trace\/normal\.jsonl"\n\s*normal_trace_status=\$\?/,
    );
    expect(job).toMatch(
      /extract_sanitized_trace "\$race_events" "\$evidence\/trace\/race\.jsonl"\n\s*race_trace_status=\$\?/,
    );
    expect(job).toContain('rm -f "$normal_events" "$normal_stderr" "$race_events" "$race_stderr"');
    expect(job).not.toMatch(/go test[^\n]*\.\/\.\./);

    for (const [parser, events, status] of [
      ["verify_normal_required_tests", "normal_events", "normal_status"],
      ["verify_race_required_tests", "race_events", "race_status"],
    ]) {
      const parserCall = `${parser} "$${events}" "$${status}"`;
      const statusGate = `test "$${status}" -eq 0`;
      const parserStatus = status.replace("_status", "_parser_status");
      const marker = parser === "verify_normal_required_tests" ? "NODE_NORMAL" : "NODE_RACE";
      expect(job).toContain(`${parser}()`);
      expect(job).toContain(`node - "$1" <<'${marker}'`);
      expect(job).toContain(marker);
      expect(job).toMatch(new RegExp(String.raw`${parser}\(\)\s*\{\s*node - "\$1" <<'${marker}'`));
      const nodeBodyMatch = job.match(
        new RegExp(String.raw`^\s*node - "\$1" <<'${marker}'\s*$\n([\s\S]*?)^\s*${marker}\s*$`, "m"),
      );
      expect(nodeBodyMatch).not.toBeNull();
      const body = nodeBodyMatch?.[1] ?? "";
      expect(body).toContain("JSON.parse(line)");
      expect(body).toContain('if (!line) continue;');
      expect(body).toContain("process.argv[2]");
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
      expect(job).toMatch(
        new RegExp(
          String.raw`${marker}\s*\n\s*parser_status=\$\?\s*\n\s*if test "\$parser_status" -ne 0; then\s*\n\s*return "\$parser_status"\s*\n\s*fi\s*\n\s*test "\$2" -eq 0`,
        ),
      );
      expect(job.indexOf(parserCall)).toBeGreaterThan(job.indexOf(`${status}=$?`));
      expect(job.indexOf(statusGate)).toBeGreaterThan(job.indexOf(parserCall));
      expect(job).toContain(`${parserCall}\n          ${parserStatus}=$?`);
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
    for (const [events, sanitized] of [
      ["normal_events", "normal.jsonl"],
      ["race_events", "race.jsonl"],
    ]) {
      const extract = `extract_sanitized_trace "$${events}" "$evidence/trace/${sanitized}"`;
      expect(job).toContain("extract_sanitized_trace()");
      expect(job).toContain(extract);
      expect(job.indexOf(extract)).toBeGreaterThan(job.indexOf("race_status=$?"));
      expect(job.indexOf(extract)).toBeLessThan(
        job.indexOf('verify_normal_required_tests "$normal_events" "$normal_status"'),
      );
    }
    expect(job).toContain("extract_sanitized_trace()");
    expect(job).toContain('node - "$1" "$2" <<\'NODE_TRACE\'');
    const traceBodyMatch = job.match(
      /^\s*node - "\$1" "\$2" <<'NODE_TRACE'\s*$\n([\s\S]*?)^\s*NODE_TRACE\s*$/m,
    );
    expect(traceBodyMatch).not.toBeNull();
    const traceBody = traceBodyMatch?.[1] ?? "";
    expect(traceBody).toContain("process.argv[2]");
    expect(traceBody).toContain("process.argv[3]");
    expect(traceBody).toContain("JSON.parse(line)");
    expect(traceBody).toContain('if (!line) continue;');
    expect(traceBody).toContain("if (!event.Test || !required.has(event.Test)) continue;");
    expect(traceBody).toContain("{ Action: event.Action, Test: event.Test, Elapsed: event.Elapsed }");
    expect(traceBody).not.toContain("Output");
    expect(traceBody).not.toContain("...");
    expect(job).toContain(
      'expected_files=$(printf \'%s\\n\' "$evidence/metadata.json" "$evidence/trace/README.txt" "$evidence/trace/normal.jsonl" "$evidence/trace/race.jsonl")',
    );
    expect(job).toContain('actual_files=$(find "$evidence" -type f -print | sort)');
    expect(job).toContain('test "$actual_files" = "$expected_files" || exit 1');
    expect(job).toContain('for trace_file in "$evidence/trace/normal.jsonl" "$evidence/trace/race.jsonl"; do');
    expect(job).toContain('test -s "$trace_file" || exit 1');
    expect(job).toContain(
      "forbidden='AKIA|-----BEGIN [A-Z ]*PRIVATE KEY-----|\\\"authorization\\\"\\s*:|sub2api-host-v1 |\\\"secrets\\\"\\s*:'",
    );
    expect(job).toMatch(/if grep -Eq "\$forbidden" "\$trace_file"; then\s*exit 1\s*fi/);
    expect(job.indexOf('rm -f "$normal_events" "$normal_stderr" "$race_events" "$race_stderr"')).toBeGreaterThan(
      job.indexOf('verify_race_required_tests "$race_events" "$race_status"'),
    );
    expect(job.indexOf('rm -f "$normal_events" "$normal_stderr" "$race_events" "$race_stderr"')).toBeLessThan(
      job.indexOf('test "$normal_status" -eq 0'),
    );
    for (const status of [
      "normal_trace_status",
      "race_trace_status",
      "trace_validation_status",
      "normal_parser_status",
      "normal_status",
      "race_parser_status",
      "race_status",
    ]) {
      expect(job.indexOf(`test "$${status}" -eq 0`)).toBeGreaterThan(
        job.indexOf('rm -f "$normal_events" "$normal_stderr" "$race_events" "$race_stderr"'),
      );
    }

    const upload = job.match(
      /- name: Upload provider-runtime evidence[\s\S]*?if: always\(\)[\s\S]*?uses: actions\/upload-artifact@v4[\s\S]*?name: provider-runtime-evidence-\$\{\{ env\.TARGET_SHA \}\}\s*$[\s\S]*?path: evidence\/\$\{\{ env\.TARGET_SHA \}\}\/provider-runtime\s*$[\s\S]*?if-no-files-found: error/m,
    );
    expect(upload).not.toBeNull();
    expect(job).not.toContain("continue-on-error");
  });
});
