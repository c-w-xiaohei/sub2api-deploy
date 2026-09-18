#!/usr/bin/env python3
"""Small, dependency-free validators for the CI evidence contract."""
import hashlib
import json
import os
import re
import stat
import sys
from pathlib import Path

GATE_SYMBOLS = {
    "verify": [
        "TestProgramTargetUsesDedicatedGoEntrypoint",
        "TestCloudflareRegistrationCompatibility",
        "TestCloudflareExplicitVersionOverridesPackageDefault",
        "TestCloudflareInputsPreserveUnknownAndSecretRPCValues",
        "TestLegacyCloudflareCallersPreservePersistedIdentities",
        "TestOfficialCloudflareProviderSchemaContract",
        "TestOfficialCloudflareProviderCheckContract",
    ],
    "host-controller": [
        "TestRegisterFoundationGraph",
        "TestRegisterPreservesComputedUpstashOutputs",
        "TestConfigAndHostCheckPreservePropertyClassesWithoutEffects",
        "TestRunUsesOnlyTheFixedHostCommand",
        "TestProbeUsesTheFixedProbeCommand",
        "TestFixedRemoteCommandInventoryHasOnlyProbeAndStdio",
        "TestRunOperationHoldsLockAcrossEffectAndResponseLossRetry",
        "TestStdioProcessExitsAfterOneFrameAndRejectsTwo",
        "TestRunPulumiPlanStagesPrivateStackAndKeepsPassphraseOutOfPulumi",
    ],
    "engine-graph": [
        "TestEngineGraphFailureStopsPublication",
        "TestEngineGraphReadyPublishesAfterOrderedHosts",
        "TestEngineGraphMaintenanceUpdateKeepsHostsAndRemovesPublication",
        "TestEngineConfiguredServerCountZero",
        "TestEngineConfiguredServerCountOneTwo",
        "TestEngineAppPlacementOneReadyFailure",
        "TestEngineManagedUpstashPreviewPreservesComputedSecretProjection",
        "TestEngineGraphPartialCheckpointKeepsSuccessfulPredecessor",
        "TestEngineManagedUpstashStateIsProtectedAndRetained",
        "TestEngineGraphCrossHostDataAdmissionOrderingAndFailureStop",
        "TestEngineGraphCrossHostDataRemovalIsReverseStaged",
        "TestEngineGraphTraceArtifactIsSanitizedJSONL",
        "TestCloudflareOldCheckpointPreviewWithTypedRegistration",
    ],
    "provider-ssh": [
        "TestProviderProcessUsesScriptedSSHTransport",
        "TestProtocolFrameBoundaries",
        "TestLoopbackStrictKnownHostAndOptionTerminator",
    ],
    "provider-runtime": [
        "TestProviderProcessReachesSharedTemporaryRuntimeServe",
        "TestProviderLifecycleWithHostProcessTempRuntime",
        "TestProviderLifecycleWithPaymentGatewayFixture",
        "TestGatewayFixtureRejectsForeignDestructiveIdentity",
        "TestProviderRuntimeCrossHostDataAdmissionLive",
    ],
    "provider-import": ["TestEngineImportPreviewIsNoOpOrAcceptedDiff"],
}


def gate_symbols():
    return GATE_SYMBOLS


def fail(message):
    raise SystemExit(message)


def events(path):
    try:
        return [json.loads(line) for line in Path(path).read_text().splitlines() if line]
    except (OSError, json.JSONDecodeError) as error:
        fail(f"invalid Go JSON evidence: {error}")


def required_tests(raw, safe, output_name):
    expression = os.environ.get("REQUIRED_TESTS", "")
    names = expression[2:-2].split("|") if expression.startswith("^(") and expression.endswith(")$") else []
    if not names or any(not name for name in names):
        fail("invalid required test selector")
    terminal = {}
    records = []
    for event in events(raw):
        output = event.get("Output")
        if isinstance(output, str):
            for prefix in json.loads(os.environ.get("EVIDENCE_OUTPUT_PREFIXES", "[]")):
                if output.startswith(prefix):
                    if not re.fullmatch(r"[A-Za-z0-9_.: -]+\n", output):
                        fail("unsafe evidence diagnostic")
                    print(output, end="")
        name, action = event.get("Test"), event.get("Action")
        if name in names and action in {"pass", "fail", "skip"}:
            if name in terminal:
                fail(f"duplicate terminal action: {name}")
            terminal[name] = action
            records.append({key: event[key] for key in ("Action", "Test", "Elapsed") if key in event})
    for name in names:
        action = terminal.get(name, "absent")
        print(f"{name}: {action}")
        if action != "pass":
            fail(f"required test did not pass: {name}")
    safe_path = Path(safe)
    safe_path.mkdir(mode=0o700, parents=True, exist_ok=True)
    os.chmod(safe_path, 0o700)
    destination = safe_path / output_name
    destination.write_text("".join(json.dumps(record, separators=(",", ":")) + "\n" for record in records))
    os.chmod(destination, 0o600)


def resource_record(rss, output):
    try:
        peak = int(Path(rss).read_text().strip())
    except (OSError, ValueError) as error:
        fail(f"invalid resource measurement: {error}")
    elapsed = int(os.environ["RESOURCE_ELAPSED_MS"])
    if elapsed < 0 or peak < 0:
        fail("invalid resource record measurements")
    record = {"schema": "engine-graph-resource-v1", "implementation": "external-engine", "label": "engine-graph", "elapsedMilliseconds": elapsed, "peakRSSKiB": peak}
    if sorted(record) != ["elapsedMilliseconds", "implementation", "label", "peakRSSKiB", "schema"]:
        fail("invalid resource record schema")
    destination = Path(output)
    destination.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    os.chmod(destination.parent, 0o700)
    destination.write_text(json.dumps(record, separators=(",", ":")) + "\n")
    if re.search(r"AKIA|-----BEGIN [A-Z ]*PRIVATE KEY-----|authorization|argv|env|log|state|secret", destination.read_text(), re.I):
        fail("resource record is not sanitized")
    os.chmod(output, 0o600)


LIVE_TEST = "TestProviderRuntimeCrossHostDataAdmissionLive"
LIVE_MILESTONES = ["postgres-owned-container", "postgres-ready", "redis-owned-container", "redis-ready"]
LIVE_STAGES = {
    "network-setup", "sandbox-start", "data-mount-setup", "data-docker-start", "data-docker-network",
    "data-docker-storage", "data-docker-cgroup", "data-docker-helper", "data-docker-config",
    "data-docker-filesystem", "data-docker-initialization", "data-docker-containerd",
    "data-docker-containerd-timeout", "data-docker-containerd-path", "data-docker-containerd-socket",
    "data-docker-containerd-exit", "data-docker-conflict", "data-docker-resource", "data-docker-permission",
    "data-docker-timeout", "data-docker-unknown", "data-image-load", "data-sshd-start", "app-mount-setup",
    "app-docker-start", "app-docker-network", "app-docker-storage", "app-docker-cgroup", "app-docker-helper",
    "app-docker-config", "app-docker-filesystem", "app-docker-initialization", "app-docker-containerd",
    "app-docker-containerd-timeout", "app-docker-containerd-path", "app-docker-containerd-socket",
    "app-docker-containerd-exit", "app-docker-conflict", "app-docker-resource", "app-docker-permission",
    "app-docker-timeout", "app-docker-unknown", "app-image-load", "app-sshd-start", "sandboxes-ready",
    "namespace-prerequisites", "provider-start", "provider-configure", "data-ssh-probe", "data-ssh-probe-host-key",
    "data-ssh-probe-protocol", "data-ssh-probe-timeout", "data-ssh-probe-transport", "data-ssh-probe-unknown",
    "data-create", "data-create-artifact", "data-create-bootstrap", "data-create-bootstrap-remote", "data-create-host",
    "data-create-observation", "data-create-response", "data-create-timeout", "data-create-transport", "data-create-unknown",
    "data-ready-check", "app-create", "app-create-artifact", "app-create-bootstrap", "app-create-bootstrap-remote",
    "app-create-host", "app-create-observation", "app-create-response", "app-create-timeout", "app-create-transport",
    "app-create-unknown", "app-ready-check", "post-create-assertions", "complete",
}

TRACE_TESTS = [
    "TestEngineGraphFailureStopsPublication",
    "TestEngineGraphPartialCheckpointKeepsSuccessfulPredecessor",
    "TestEngineManagedUpstashStateIsProtectedAndRetained",
    "TestEngineManagedUpstashPreviewPreservesComputedSecretProjection",
    "TestEngineGraphReadyPublishesAfterOrderedHosts",
    "TestEngineGraphMaintenanceUpdateKeepsHostsAndRemovesPublication",
    "TestEngineConfiguredServerCountOneTwo",
    "TestEngineConfiguredServerCountZero",
    "TestEngineAppPlacementOneReadyFailure/alpha_failure_blocks_publication",
    "TestEngineAppPlacementOneReadyFailure/alpha_ready_publishes_once",
    "TestEngineGraphCrossHostDataAdmissionOrderingAndFailureStop/admission_then_App_then_publication",
    "TestEngineGraphCrossHostDataAdmissionOrderingAndFailureStop/data_failure_stops_App_and_publication",
    "TestEngineGraphCrossHostDataAdmissionOrderingAndFailureStop/App_failure_stops_publication",
    "TestEngineGraphCrossHostDataRemovalIsReverseStaged",
]


def trace_name(test):
    stem = re.sub(r"[^A-Za-z0-9_.-]", "_", test).strip("._")[:96] or "engine-graph"
    return f"{stem}-{hashlib.sha256(test.encode()).hexdigest()}.jsonl"


def live_records(raw, require_success=False):
    terminal = [event.get("Action") for event in events(raw) if event.get("Test") == LIVE_TEST and event.get("Action") in {"pass", "fail", "skip"}]
    if len(terminal) != 1:
        fail("live test did not have exactly one terminal action")
    outputs = [event.get("Output", "") for event in events(raw) if event.get("Test") == LIVE_TEST and isinstance(event.get("Output"), str)]
    milestone_index = 0
    stage_seen = False
    observer = None
    preflight = None
    diagnostics = {
        "snapshot": None,
        "postgres_readiness": None,
        "postgres_container": None,
        "app_progress": None,
        "assertions": None,
    }
    for output in outputs:
        if "live namespace fixture failed:" in output:
            match = re.fullmatch(r"live namespace fixture failed: ([a-z-]+)\n", output)
            if not match or stage_seen or match.group(1) not in LIVE_STAGES:
                fail("invalid provider stage marker")
            stage_seen = True
        if "live milestone:" in output:
            match = re.fullmatch(r"live milestone: ([a-z-]+)\n", output)
            if not match or milestone_index >= len(LIVE_MILESTONES) or match.group(1) != LIVE_MILESTONES[milestone_index]:
                fail("invalid live milestone marker")
            milestone_index += 1
        if "live observer:" in output:
            if not re.fullmatch(r"live observer: (observer-error|observer-inconclusive)\n", output) or observer is not None:
                fail("invalid live observer marker")
            observer = output.strip().split(": ", 1)[1]
        if "live post-create snapshot:" in output:
            match = re.fullmatch(r"live post-create snapshot: state=([a-z-]+) postgres=([a-z-]+) redis=([a-z-]+)\n", output)
            states = {"root-absent", "state-absent", "invalid", "unavailable", "not-exact", "pending-exact"}
            containers = {"not-inspected", "absent", "identity-mismatch", "exited", "not-running", "running-probe-failed", "running-unready", "ready", "unavailable", "ambiguous"}
            if not match or diagnostics["snapshot"] is not None:
                fail("invalid live post-create snapshot")
            state, postgres, redis = match.groups()
            inspected = postgres != "not-inspected" and redis != "not-inspected"
            not_inspected = postgres == "not-inspected" and redis == "not-inspected"
            if state not in states or postgres not in containers or redis not in containers or (state == "pending-exact" and not inspected) or (state != "pending-exact" and not not_inspected):
                fail("invalid live post-create snapshot")
            diagnostics["snapshot"] = match.groups()
        if "live postgres readiness:" in output:
            pattern = r"live postgres readiness: pgdata=(present|absent|failed) server=(accepting|rejecting|no-response|failed) psql=(ok|failed)\n"
            if not re.fullmatch(pattern, output) or diagnostics["postgres_readiness"] is not None:
                fail("invalid live postgres readiness")
            diagnostics["postgres_readiness"] = True
        if "live postgres container:" in output:
            pattern = r"live postgres container: exec=(ok|timeout|exit-1|exit-126|exit-127|exit-other|failed) absolute=(ok|timeout|exit-1|exit-126|exit-127|exit-other|failed) absolute-error=(none|empty|not-found|permission|cgroup|namespace|rootfs|runtime|daemon|unknown) cs=(ok|timeout|canceled|exit-1|exit-126|exit-127|exit-other|failed) co=(sentinel|empty|not-found|permission|cgroup|namespace|rootfs|runtime|daemon|cwd|user|security|resource|other|overflow) ce=(empty|present|overflow) rootfs=(present|absent|nonregular|nonexecutable|unavailable) direct=(ok|timeout|exit-1|exit-126|exit-127|exit-other|failed) direct-error=(none|empty|not-found|permission|cgroup|namespace|rootfs|runtime|daemon|unknown) state=(running|restarting|exited|created|paused|dead|removing|failed) restarts=(zero|nonzero|unknown) oom=(yes|no|unknown) error=(present|absent|unknown)\n"
            if not re.fullmatch(pattern, output) or diagnostics["postgres_container"] is not None:
                fail("invalid live postgres lifecycle")
            diagnostics["postgres_container"] = True
        if "live ssh docker preflight:" in output:
            pattern = r"live ssh docker preflight: socket=(present|missing|not-socket) container=(ok|failed) network=(ok|failed) container-discovery=(empty|unowned|owned|malformed|failed) network-discovery=(empty|unowned|owned|malformed|failed) docker-host=(set|unset) docker-context=(set|unset) docker-config=(set|unset)\n"
            if not re.fullmatch(pattern, output) or preflight is not None:
                fail("invalid live SSH Docker preflight")
            preflight = re.fullmatch(pattern, output).groups()
        if "live app progress:" in output:
            pattern = r"live app progress: start=(present|absent|unavailable) postgres=(present|absent|unavailable) redis=(present|absent|unavailable) gate=(present|absent|unavailable) launched=(present|absent|unavailable) http=(present|absent|unavailable)\n"
            if not re.fullmatch(pattern, output) or diagnostics["app_progress"] is not None:
                fail("invalid live app progress")
            diagnostics["app_progress"] = True
        if "live assertions:" in output:
            pattern = r"live assertions: data=(yes|no) app=(yes|no) env=(yes|no) pg=(yes|no) pg-deny=(yes|no) pg-catalog=(yes|no) redis=(yes|no) redis-deny=(yes|no) redis-default=(yes|no) redis-acl=(yes|no) pg-drop=(yes|no) redis-drop=(yes|no) foreign=(yes|no) pa=(o|t|r|u|f) ra=(o|t|r|u|f) pb=(o|t|r|u|f) rb=(o|t|r|u|f) pp=(yes|no) rp=(yes|no) pc=(error|[01]{8})\n"
            if not re.fullmatch(pattern, output) or diagnostics["assertions"] is not None:
                fail("invalid live assertions")
            diagnostics["assertions"] = True
    if terminal[0] == "pass" and milestone_index != len(LIVE_MILESTONES):
        fail("live observer evidence incomplete")
    if require_success:
        if terminal[0] != "pass" or milestone_index != len(LIVE_MILESTONES) or observer is not None:
            fail("live observer evidence incomplete")
        if preflight is None or preflight[0] != "present" or preflight[1] != "ok" or preflight[2] != "ok" or preflight[3] not in {"empty", "unowned"} or preflight[4] not in {"empty", "unowned"} or preflight[5:] != ("unset", "unset", "unset"):
            fail("live SSH Docker preflight incomplete")
        if any(value is not None for value in diagnostics.values()):
            fail("diagnostic-only live output is not promotable evidence")
        if stage_seen:
            fail("provider failure marker on live pass")
    return outputs


def engine_graph(raw, trace, safe, rss):
    text = Path(raw).read_text()
    try:
        required_tests(raw, safe, "engine-graph.jsonl")
    except SystemExit:
        lowered = text.lower()
        if "could not load plugin" in lowered or "no resource plugin" in lowered:
            category = "provider-load"
        elif "schema" in lowered:
            category = "provider-schema"
        elif "configure" in lowered:
            category = "provider-config"
        elif "debug_providers" in lowered or "attach" in lowered:
            category = "provider-attach"
        elif "language runtime" in lowered or "language host" in lowered:
            category = "language-runtime"
        elif "backend" in lowered or "stack" in lowered:
            category = "backend-stack"
        elif "context deadline" in lowered or "timed out" in lowered:
            category = "timeout"
        else:
            category = "other"
        print(f"Engine Graph failure category: {category}")
        raise
    for event in events(raw):
        output = event.get("Output", "")
        if isinstance(output, str) and "external Engine failure stage: " in output:
            print(output, end="")
    trace_path = Path(trace)
    if not trace_path.is_dir() or stat.S_IMODE(trace_path.stat().st_mode) != 0o700:
        fail("invalid persisted trace directory mode")
    expected = sorted(trace_name(test) for test in TRACE_TESTS)
    files = sorted(item.name for item in trace_path.iterdir())
    if files != expected:
        fail("unexpected persisted trace inventory")
    for name in files:
        path = trace_path / name
        if path.is_symlink() or not path.is_file() or stat.S_IMODE(path.stat().st_mode) != 0o600:
            fail("invalid persisted trace mode")
        lines = [line for line in path.read_text().splitlines() if line]
        if not lines:
            fail("persisted trace must contain at least one event")
        for line in lines:
            record = json.loads(line)
            event = [key for key in ("lifecycle_event", "publication_event", "summary") if isinstance(record.get(key), str) and record[key]]
            allowed = ["summary", "test_name"] if event == ["summary"] else ["test_name", event[0]] if len(event) == 1 else []
            if not record.get("test_name") or sorted(record) != sorted(allowed) or record["test_name"] not in TRACE_TESTS:
                fail("invalid persisted trace schema")
            if re.search(r"AKIA|-----BEGIN [A-Z ]*PRIVATE KEY-----|\"authorization\"\s*:|sub2api-host-v1 |\"secrets\"\s*:|CrossHost[A-Za-z0-9_]*(?:Password|Secret)", line, re.I):
                fail("persisted trace is not sanitized")
    resource_record(rss, str(Path(safe) / "engine-graph-resource.json"))

    if "could not load plugin" in text.lower() or "no resource plugin" in text.lower():
        category = "provider-load"
    elif "schema" in text.lower():
        category = "provider-schema"
    elif "configure" in text.lower():
        category = "provider-config"
    elif "debug_providers" in text.lower() or "attach" in text.lower():
        category = "provider-attach"
    elif "language runtime" in text.lower() or "language host" in text.lower():
        category = "language-runtime"
    elif "backend" in text.lower() or "stack" in text.lower():
        category = "backend-stack"
    elif "context deadline" in text.lower() or "timed out" in text.lower():
        category = "timeout"
    else:
        category = "other"
    if any(event.get("Action") in {"fail", "skip"} for event in events(raw)):
        print(f"Engine Graph failure category: {category}")


def live_diagnostic(raw):
    records = live_records(raw)
    terminal = [event.get("Action") for event in events(raw) if event.get("Test") == LIVE_TEST and event.get("Action") in {"pass", "fail", "skip"}][0]
    print(f"{LIVE_TEST}: {terminal}")
    for output in records:
        if output.startswith((
            "live namespace fixture failed:",
            "live observer:",
            "live milestone:",
            "live post-create snapshot:",
            "live postgres readiness:",
            "live postgres container:",
            "live ssh docker preflight:",
            "live app progress:",
            "live assertions:",
        )):
            print(output, end="")
    stderr = Path(raw).with_suffix(".stderr")
    if stderr.is_file() and stderr.stat().st_size:
        classify_stderr(str(stderr))


def live_candidate(raw, trace, safe, consumer_trace):
    outputs = live_records(raw, require_success=True)
    trace_path = Path(trace)
    expected = {"mx-allowlist-live.json"}
    if not trace_path.exists():
        fail("persisted trace directory is missing")
    if trace_path.exists():
        if not trace_path.is_dir() or stat.S_IMODE(trace_path.stat().st_mode) != 0o700:
            fail("invalid persisted trace directory mode")
        files = {item.name for item in trace_path.iterdir()}
        if files != expected:
            fail("unexpected live evidence inventory")
        for item in trace_path.iterdir():
            if item.is_symlink() or not item.is_file() or stat.S_IMODE(item.stat().st_mode) != 0o600:
                fail("invalid persisted trace mode")
            lines = [line for line in item.read_text().splitlines() if line]
            if len(lines) != 1:
                fail("live evidence must contain exactly one JSON record")
            for line in lines:
                record = json.loads(line)
                if item.name == "mx-allowlist-live.json":
                    required = {"test", "providerSHA256", "hostAMD64SHA256", "releasedBoundary", "dataHostPass", "appHostPass", "appDataEnvironmentAuthenticated", "appReadyAfterData", "postgresPass", "postgresWrongPasswordDenied", "postgresCatalog", "redisPass", "redisWrongPasswordDenied", "redisDefaultDenied", "redisACL", "postgresDrop", "redisDrop", "foreignTableUnchanged", "foreignTableSHA256"}
                    expected_release = "sub2api-host-controller@sha256:" + hashlib.sha256(os.environ["TARGET_SHA"].encode()).hexdigest()
                    if set(record) != required or record.get("test") != "TestProviderRuntimeCrossHostDataAdmissionLive" or record.get("releasedBoundary") != expected_release or record.get("providerSHA256") != os.environ["PROVIDER_SHA"] or record.get("hostAMD64SHA256") != os.environ["HOST_AMD64_SHA"]:
                        fail("invalid live evidence schema or identity")
                    if any(value is False for value in record.values() if isinstance(value, bool)):
                        fail("live evidence contains a failed assertion")
                    for key in ("providerSHA256", "hostAMD64SHA256", "foreignTableSHA256"):
                        if not re.fullmatch(r"[0-9a-f]{64}", record.get(key, "")):
                            fail("invalid live evidence hash")
                if re.search(r"AKIA|BEGIN [A-Z ]*PRIVATE KEY|authorization|secrets|sub2api-host-v1 |CrossHost[A-Za-z0-9_]*(Password|Secret)", line, re.I):
                    fail("persisted trace is not sanitized")
                if re.search(r"(?:[0-9]{1,3}\.){3}[0-9]{1,3}", line):
                    fail("live evidence contains an IP address")
    safe_path = Path(safe)
    safe_path.mkdir(mode=0o700, parents=True, exist_ok=True)
    os.chmod(safe_path, 0o700)
    live_file = trace_path / "mx-allowlist-live.json"
    safe_live = safe_path / "mx-allowlist-live.json"
    safe_live.write_bytes(live_file.read_bytes())
    os.chmod(safe_live, 0o600)
    live_summary = safe_path / "live.jsonl"
    live_summary.write_text(json.dumps({"Action": "pass", "Test": "TestProviderRuntimeCrossHostDataAdmissionLive"}, separators=(",", ":")) + "\n")
    os.chmod(live_summary, 0o600)
    destination = Path(consumer_trace)
    provider = os.environ["PROVIDER_SHA"]
    host = os.environ.get("HOST_AMD64_SHA", "")
    target = os.environ["TARGET_SHA"]
    if not re.fullmatch(r"[0-9a-f]{64}", provider) or not re.fullmatch(r"[0-9a-f]{64}", host):
        fail("invalid live artifact hash")
    try:
        (safe_path / "metadata.json").unlink()
    except FileNotFoundError:
        pass
    result = {"sha": target, "providerSHA256": provider, "hostAMD64SHA256": host, "providerRuntimeCrossHostAppLive": "data-then-app-pass"}
    destination.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    destination.write_text(json.dumps(result, separators=(",", ":")) + "\n")
    os.chmod(destination, 0o600)


def validate_trace(path, target):
    value = json.loads(Path(path).read_text())
    if sorted(value) != ["hostAMD64SHA256", "providerRuntimeCrossHostAppLive", "providerSHA256", "sha"] or value["sha"] != target or value["providerRuntimeCrossHostAppLive"] != "data-then-app-pass":
        fail("invalid consumed live trace")
    for key in ("hostAMD64SHA256", "providerSHA256"):
        if not re.fullmatch(r"[0-9a-f]{64}", value[key]):
            fail("invalid consumed live trace hash")


def verify_candidate_hashes(consumer, trace):
    value = json.loads(Path(trace).read_text())
    provider = hashlib.sha256((Path(consumer) / "bundle/bin/pulumi-resource-sub2api-host").read_bytes()).hexdigest()
    host = hashlib.sha256((Path(consumer) / "bundle/artifacts/sub2api-host/sub2api-host-linux-amd64").read_bytes()).hexdigest()
    if value.get("providerSHA256") != provider or value.get("hostAMD64SHA256") != host:
        fail("candidate artifact hash mismatch")


def metadata(path):
    symbols = gate_symbols()
    value = {"sha": os.environ["TARGET_SHA"], "runId": os.environ["RUN_ID"], "runUrl": os.environ["RUN_URL"], "gate": "target-release", "archive": os.environ["ARCHIVE"], "sha256": os.environ["SHA256"], "requiredGateIds": list(symbols), "gateSymbols": symbols}
    Path(path).write_text(json.dumps(value, separators=(",", ":")) + "\n")


def read_pages(path, key):
    pages = json.loads(Path(path).read_text())
    return [item for page in pages for item in page.get(key, [])]


def select_run(path, sha):
    runs = [run for run in read_pages(path, "workflow_runs") if run.get("head_sha") == sha and run.get("event") == "push" and run.get("status") == "completed" and run.get("conclusion") == "success" and run.get("path") == ".github/workflows/ci.yml" and run.get("name") == "Pull-Request CI" and str(run.get("head_branch", "")).startswith("agent/")]
    if len(runs) == 0:
        fail("missing successful ci.yml run")
    runs.sort(key=lambda run: (run.get("run_attempt", 0), run.get("id", 0)), reverse=True)
    print(runs[0]["id"])


def validate_jobs(path):
    expected = ["Verify", "Host Controller (amd64)", "Host Controller (arm64)", "Engine Graph", "Provider SSH", "Provider Runtime", "Provider Import", "Target Release"]
    jobs = json.loads(Path(path).read_text())
    jobs = [item for page in jobs for item in page.get("jobs", [])]
    for name in expected:
        found = [job for job in jobs if job.get("name") == name]
        if len(found) != 1 or found[0].get("status") != "completed" or found[0].get("conclusion") != "success":
            fail(f"invalid required gate: {name}")


def select_artifact(path, sha):
    artifacts = [item for page in json.loads(Path(path).read_text()) for item in page.get("artifacts", []) if item.get("name") == f"target-release-{sha}" and item.get("expired") is False]
    if len(artifacts) != 1:
        fail("candidate artifact missing or duplicate")
    print(artifacts[0]["id"])


def validate_consumer(path, sha, host_hash, provider_hash):
    value = json.loads(Path(path).read_text())
    if sorted(value) != ["hostAMD64SHA256", "providerRuntimeCrossHostAppLive", "providerSHA256", "sha"] or value.get("sha") != sha or value.get("providerRuntimeCrossHostAppLive") != "data-then-app-pass" or value.get("hostAMD64SHA256") != host_hash or value.get("providerSHA256") != provider_hash:
        fail("candidate live Provider Runtime trace mismatch")
    if not re.fullmatch(r"[0-9a-f]{64}", host_hash) or not re.fullmatch(r"[0-9a-f]{64}", provider_hash):
        fail("invalid candidate live Provider Runtime trace hash")


def validate_metadata(path, sha, run_id, archive, archive_hash, run_url):
    value = json.loads(Path(path).read_text())
    symbols = gate_symbols()
    expected = {"sha": sha, "runId": run_id, "runUrl": run_url, "gate": "target-release", "archive": archive, "sha256": archive_hash, "requiredGateIds": list(symbols), "gateSymbols": symbols}
    if value != expected:
        fail("candidate metadata mismatch")


def classify_stderr(path):
    text = Path(path).read_text()
    lines = [line for line in text.splitlines() if line]
    if lines and all(line.startswith("go: downloading ") for line in lines):
        category = "go-download"
    elif lines and all(line.startswith("go: ") for line in lines):
        lowered = text.lower()
        category = "go-cache" if "cache" in lowered else "go-warning" if "warning" in lowered else "go-other"
    else:
        category = "non-go"
    print(f"{LIVE_TEST} stderr: {category}")


def validate_tag_ref(value, tag_name):
    ref = json.loads(value)
    if ref.get("ref") != f"refs/tags/{tag_name}" or ref.get("object", {}).get("type") not in {"commit", "tag"} or not re.fullmatch(r"[0-9a-f]{40}", ref.get("object", {}).get("sha", "")):
        fail("invalid remote tag ref")
    print(ref["object"]["type"], ref["object"]["sha"])


def main():
    command = sys.argv[1]
    if command == "required": required_tests(sys.argv[2], sys.argv[3], sys.argv[4])
    elif command == "resource": resource_record(sys.argv[2], sys.argv[3])
    elif command == "engine-graph": engine_graph(sys.argv[2], sys.argv[3], sys.argv[4], sys.argv[5])
    elif command == "live-diagnostic": live_diagnostic(sys.argv[2])
    elif command == "live-candidate": live_candidate(sys.argv[2], sys.argv[3], sys.argv[4], sys.argv[5])
    elif command == "validate-trace": validate_trace(sys.argv[2], sys.argv[3])
    elif command == "verify-candidate-hashes": verify_candidate_hashes(sys.argv[2], sys.argv[3])
    elif command == "metadata": metadata(sys.argv[2])
    elif command == "select-run": select_run(sys.argv[2], sys.argv[3])
    elif command == "validate-jobs": validate_jobs(sys.argv[2])
    elif command == "select-artifact": select_artifact(sys.argv[2], sys.argv[3])
    elif command == "validate-consumer": validate_consumer(sys.argv[2], sys.argv[3], sys.argv[4], sys.argv[5])
    elif command == "validate-metadata": validate_metadata(sys.argv[2], sys.argv[3], sys.argv[4], sys.argv[5], sys.argv[6], sys.argv[7])
    elif command == "validate-tag-ref": validate_tag_ref(sys.argv[2], sys.argv[3])
    elif command == "classify-stderr": classify_stderr(sys.argv[2])
    else: fail(f"unknown evidence operation: {command}")


if __name__ == "__main__":
    main()
