#!/usr/bin/env python3
import hashlib
import importlib.util
import json
import os
import re
import stat
import tempfile
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
SPEC = importlib.util.spec_from_file_location("ci_evidence", ROOT / "scripts/ci-evidence.py")
ci_evidence = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(ci_evidence)


class EvidenceTests(unittest.TestCase):
    def write_jsonl(self, directory, events):
        path = directory / "events.jsonl"
        path.write_text("".join(json.dumps(event) + "\n" for event in events))
        return path

    def required(self, events, selector):
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            raw = self.write_jsonl(directory, events)
            previous = os.environ.get("REQUIRED_TESTS")
            os.environ["REQUIRED_TESTS"] = selector
            try:
                ci_evidence.required_tests(raw, directory / "safe", "required.jsonl")
            finally:
                if previous is None:
                    del os.environ["REQUIRED_TESTS"]
                else:
                    os.environ["REQUIRED_TESTS"] = previous

    def test_required_accepts_passing_terminal_records(self):
        self.required([{"Test": "Alpha", "Action": "pass"}], "^(Alpha)$")

    def test_verify_contract_rejects_each_missing_symbol(self):
        symbols = ci_evidence.GATE_SYMBOLS["verify"]
        selector = "^(" + "|".join(symbols) + ")$"
        events = [{"Test": symbol, "Action": "pass"} for symbol in symbols]
        self.required(events, selector)
        for missing in symbols:
            with self.subTest(missing=missing), self.assertRaises(SystemExit):
                self.required([event for event in events if event["Test"] != missing], selector)

    def test_host_controller_metadata_matches_workflow_selector(self):
        workflow = (ROOT / ".github/workflows/ci.yml").read_text()
        selectors = re.findall(r"tests='\^\(([^']+)\)\$'", workflow)
        host = [value.split("|") for value in selectors if "TestRegisterFoundationGraph" in value]
        self.assertEqual(host, [ci_evidence.GATE_SYMBOLS["host-controller"]])

    def test_required_rejects_fail_skip_and_duplicate_terminals(self):
        for events in (
            [{"Test": "Alpha", "Action": "fail"}],
            [{"Test": "Alpha", "Action": "skip"}],
            [{"Test": "Alpha", "Action": "pass"}, {"Test": "Alpha", "Action": "pass"}],
        ):
            with self.assertRaises(SystemExit):
                self.required(events, "^(Alpha)$")

    def test_required_rejects_unsafe_diagnostics(self):
        with self.assertRaises(SystemExit):
            previous = os.environ.get("EVIDENCE_OUTPUT_PREFIXES")
            os.environ["EVIDENCE_OUTPUT_PREFIXES"] = json.dumps(["diagnostic: "])
            try:
                self.required([{"Test": "Alpha", "Action": "pass", "Output": "diagnostic: secret=bad!\n"}], "^(Alpha)$")
            finally:
                if previous is None:
                    del os.environ["EVIDENCE_OUTPUT_PREFIXES"]
                else:
                    os.environ["EVIDENCE_OUTPUT_PREFIXES"] = previous

    def test_required_rejects_malformed_json_evidence(self):
        with tempfile.TemporaryDirectory() as temporary:
            raw = Path(temporary) / "events.jsonl"
            raw.write_text("not-json\n")
            previous = os.environ.get("REQUIRED_TESTS")
            os.environ["REQUIRED_TESTS"] = "^(Alpha)$"
            try:
                with self.assertRaises(SystemExit):
                    ci_evidence.required_tests(raw, Path(temporary) / "safe", "required.jsonl")
            finally:
                if previous is None:
                    del os.environ["REQUIRED_TESTS"]
                else:
                    os.environ["REQUIRED_TESTS"] = previous

    def test_engine_graph_accepts_go_normalized_subtest_trace_names(self):
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            raw = self.write_jsonl(directory, [{"Test": test, "Action": "pass"} for test in ci_evidence.GATE_SYMBOLS["engine-graph"]])
            trace = directory / "trace"
            trace.mkdir(mode=0o700)
            for test in ci_evidence.TRACE_TESTS:
                name = ci_evidence.trace_name(test)
                path = trace / name
                path.write_text(json.dumps({"test_name": test, "summary": "complete"}) + "\n")
                path.chmod(0o600)
            rss = directory / "rss"
            rss.write_text("1\n")
            previous = os.environ.get("RESOURCE_ELAPSED_MS")
            previous_tests = os.environ.get("REQUIRED_TESTS")
            os.environ["RESOURCE_ELAPSED_MS"] = "1"
            os.environ["REQUIRED_TESTS"] = "^(" + "|".join(ci_evidence.GATE_SYMBOLS["engine-graph"]) + ")$"
            try:
                ci_evidence.engine_graph(raw, trace, directory / "safe", rss)
            finally:
                if previous is None:
                    del os.environ["RESOURCE_ELAPSED_MS"]
                else:
                    os.environ["RESOURCE_ELAPSED_MS"] = previous
                if previous_tests is None:
                    del os.environ["REQUIRED_TESTS"]
                else:
                    os.environ["REQUIRED_TESTS"] = previous_tests

    def test_engine_graph_rejects_space_named_trace_inventory(self):
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            raw = self.write_jsonl(directory, [{"Test": test, "Action": "pass"} for test in ci_evidence.GATE_SYMBOLS["engine-graph"]])
            trace = directory / "trace"
            trace.mkdir(mode=0o700)
            for test in ci_evidence.TRACE_TESTS:
                wrong = test.replace("_", " ")
                path = trace / ci_evidence.trace_name(wrong)
                path.write_text(json.dumps({"test_name": wrong, "summary": "complete"}) + "\n")
                path.chmod(0o600)
            rss = directory / "rss"
            rss.write_text("1\n")
            previous = os.environ.get("RESOURCE_ELAPSED_MS")
            previous_tests = os.environ.get("REQUIRED_TESTS")
            os.environ["RESOURCE_ELAPSED_MS"] = "1"
            os.environ["REQUIRED_TESTS"] = "^(" + "|".join(ci_evidence.GATE_SYMBOLS["engine-graph"]) + ")$"
            try:
                with self.assertRaises(SystemExit):
                    ci_evidence.engine_graph(raw, trace, directory / "safe", rss)
            finally:
                if previous is None:
                    del os.environ["RESOURCE_ELAPSED_MS"]
                else:
                    os.environ["RESOURCE_ELAPSED_MS"] = previous
                if previous_tests is None:
                    del os.environ["REQUIRED_TESTS"]
                else:
                    os.environ["REQUIRED_TESTS"] = previous_tests

    def test_candidate_hashes_recompute_extracted_files(self):
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            provider = directory / "bundle/bin/pulumi-resource-sub2api-host"
            host = directory / "bundle/artifacts/sub2api-host/sub2api-host-linux-amd64"
            provider.parent.mkdir(parents=True)
            host.parent.mkdir(parents=True)
            provider.write_bytes(b"provider")
            host.write_bytes(b"host")
            trace = directory / "consumer-trace.json"
            trace.write_text(json.dumps({"providerSHA256": hashlib.sha256(b"provider").hexdigest(), "hostAMD64SHA256": hashlib.sha256(b"host").hexdigest()}))
            ci_evidence.verify_candidate_hashes(directory, trace)
            host.write_bytes(b"tampered")
            with self.assertRaises(SystemExit):
                ci_evidence.verify_candidate_hashes(directory, trace)

    def test_live_candidate_accepts_content_addressed_release_identity(self):
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            raw = directory / "live.jsonl"
            events = [
                {"Action": "output", "Test": "TestProviderRuntimeCrossHostDataAdmissionLive", "Output": "live ssh docker preflight: socket=present container=ok network=ok container-discovery=empty network-discovery=empty docker-host=unset docker-context=unset docker-config=unset\n"},
            ]
            events.extend({"Action": "output", "Test": "TestProviderRuntimeCrossHostDataAdmissionLive", "Output": "live milestone: %s\n" % milestone} for milestone in ci_evidence.LIVE_MILESTONES)
            events.append({"Action": "pass", "Test": "TestProviderRuntimeCrossHostDataAdmissionLive"})
            raw.write_text("".join(json.dumps(event) + "\n" for event in events))
            trace = directory / "trace"
            trace.mkdir(mode=0o700)
            target = "a" * 40
            provider = "b" * 64
            host = "c" * 64
            record = {key: True for key in (
                "dataHostPass", "appHostPass", "appDataEnvironmentAuthenticated", "appReadyAfterData",
                "postgresPass", "postgresWrongPasswordDenied", "postgresCatalog", "redisPass",
                "redisWrongPasswordDenied", "redisDefaultDenied", "redisACL", "postgresDrop",
                "redisDrop", "foreignTableUnchanged",
            )}
            record.update({
                "test": "TestProviderRuntimeCrossHostDataAdmissionLive",
                "providerSHA256": provider,
                "hostAMD64SHA256": host,
                "releasedBoundary": "sub2api-host-controller@sha256:" + hashlib.sha256(target.encode()).hexdigest(),
                "foreignTableSHA256": "d" * 64,
            })
            live = trace / "mx-allowlist-live.json"
            live.write_text(json.dumps(record) + "\n")
            live.chmod(0o600)
            previous = {key: os.environ.get(key) for key in ("TARGET_SHA", "PROVIDER_SHA", "HOST_AMD64_SHA")}
            os.environ.update({"TARGET_SHA": target, "PROVIDER_SHA": provider, "HOST_AMD64_SHA": host})
            try:
                ci_evidence.live_candidate(raw, trace, directory / "safe", directory / "consumer-trace.json")
            finally:
                for key, value in previous.items():
                    if value is None:
                        del os.environ[key]
                    else:
                        os.environ[key] = value

    def test_metadata_rejects_archive_hash_tampering(self):
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary) / "metadata.json"
            symbols = ci_evidence.gate_symbols()
            value = {"sha": "a" * 40, "runId": "1", "runUrl": "https://example.test/1", "gate": "target-release", "archive": "sub2api-controller-" + "a" * 40 + ".tar.gz", "sha256": "b" * 64, "requiredGateIds": list(symbols), "gateSymbols": symbols}
            path.write_text(json.dumps(value))
            ci_evidence.validate_metadata(path, "a" * 40, "1", value["archive"], "b" * 64, "https://example.test/1")
            value["sha256"] = "c" * 64
            path.write_text(json.dumps(value))
            with self.assertRaises(SystemExit):
                ci_evidence.validate_metadata(path, "a" * 40, "1", value["archive"], "b" * 64, "https://example.test/1")


if __name__ == "__main__":
    unittest.main()
