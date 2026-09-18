#!/usr/bin/env python3
import json
import re
import stat
import subprocess
import tempfile
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
SCRIPT = ROOT / "scripts/tests/gmpay-image-contract-test.sh"
HELPER = ROOT / "scripts/tests/gmpay-image-manifest.py"
WORKFLOW = ROOT / ".github/workflows/ci.yml"


class GmPayImageContractTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.script = SCRIPT.read_text()
        cls.workflow = WORKFLOW.read_text()

    def test_manifest_helper_is_executable(self):
        self.assertTrue(HELPER.exists())
        self.assertTrue(HELPER.stat().st_mode & stat.S_IXUSR)

    @staticmethod
    def job_block(workflow, job_name):
        lines = workflow.splitlines()
        start = lines.index(f"  {job_name}:")
        end = next(
            (index for index in range(start + 1, len(lines)) if re.match(r"^  [A-Za-z0-9_-]+:", lines[index])),
            len(lines),
        )
        return "\n".join(lines[start:end])

    @staticmethod
    def run_blocks(workflow):
        lines = workflow.splitlines()
        blocks = []
        index = 0
        while index < len(lines):
            match = re.match(r"^        run:\s*(.*)$", lines[index])
            if not match:
                index += 1
                continue
            value = match.group(1)
            if value == "|":
                index += 1
                block = []
                while index < len(lines) and (not lines[index].strip() or lines[index].startswith("          ")):
                    block.append(lines[index][10:] if lines[index].startswith("          ") else "")
                    index += 1
                blocks.append("\n".join(block) + "\n")
            else:
                blocks.append(value)
                index += 1
        return blocks

    def test_script_is_executable_and_has_exact_release_contract(self):
        self.assertTrue(SCRIPT.exists())
        self.assertTrue(SCRIPT.stat().st_mode & stat.S_IXUSR)
        self.assertRegex(self.script, r"(?m)^IMAGE='gmwallet/epusdt:v2\.0\.0'$")
        self.assertNotRegex(self.script, r"(?i)(?:^|[/:])latest(?:$|[\s'\"])")

    def run_helper(self, mode, raw, *arguments):
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary) / "manifest.json"
            path.write_bytes(raw)
            command = ["python3", str(HELPER), mode, str(path), *arguments]
            input_data = None
            if mode == "redact":
                command = ["python3", str(HELPER), mode]
                input_data = raw.decode()
            result = subprocess.run(
                command,
                text=True,
                capture_output=True,
                input=input_data,
            )
            return result

    def test_manifest_helper_selects_native_index_and_exact_config(self):
        amd = "a" * 64
        arm = "b" * 64
        index = json.dumps(
            {
                "schemaVersion": 2,
                "manifests": [
                    {"mediaType": "application/vnd.oci.image.manifest.v1+json", "digest": f"sha256:{amd}", "platform": {"os": "linux", "architecture": "amd64"}},
                    {"mediaType": "application/vnd.oci.image.manifest.v1+json", "digest": f"sha256:{arm}", "platform": {"os": "linux", "architecture": "arm64"}},
                    {"mediaType": "application/vnd.in-toto+json", "digest": "sha256:" + "c" * 64, "platform": {"os": "unknown", "architecture": "unknown"}},
                ],
            },
            separators=(",", ":"),
        ).encode()
        selected = self.run_helper("resolve", index, "amd64")
        self.assertEqual(selected.returncode, 0, selected.stderr)
        self.assertEqual(json.loads(selected.stdout)["platformDigest"], f"sha256:{amd}")

        config = "d" * 64
        manifest = json.dumps(
            {"schemaVersion": 2, "config": {"mediaType": "application/vnd.oci.image.config.v1+json", "digest": f"sha256:{config}"}, "layers": []},
            separators=(",", ":"),
        ).encode()
        resolved_config = self.run_helper("config", manifest)
        self.assertEqual(resolved_config.returncode, 0, resolved_config.stderr)
        self.assertEqual(json.loads(resolved_config.stdout)["configDigest"], f"sha256:{config}")

    def test_manifest_helper_rejects_missing_duplicate_and_invalid_metadata(self):
        valid = lambda digest, arch: {"digest": digest, "platform": {"os": "linux", "architecture": arch}}
        missing = {"manifests": [valid("sha256:" + "a" * 64, "arm64")]}
        duplicate = {"manifests": [valid("sha256:" + "a" * 64, "amd64"), valid("sha256:" + "b" * 64, "amd64")]}
        invalid_digest = {"manifests": [valid("sha256:" + "A" * 64, "amd64")]}
        for document in (missing, duplicate, invalid_digest):
            with self.subTest(document=document):
                result = self.run_helper("resolve", json.dumps(document).encode(), "amd64")
                self.assertNotEqual(result.returncode, 0)
        invalid_config = {"config": {"digest": "sha256:" + "G" * 64}, "layers": []}
        result = self.run_helper("config", json.dumps(invalid_config).encode())
        self.assertNotEqual(result.returncode, 0)

    def test_manifest_helper_hashes_single_manifest_and_redacts_secrets(self):
        config = "e" * 64
        raw = b'{"schemaVersion":2,"config":{"digest":"sha256:' + config.encode() + b'"},"layers":[]}'
        result = self.run_helper("resolve", raw, "arm64")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(json.loads(result.stdout)["platformDigest"], "sha256:" + __import__("hashlib").sha256(raw).hexdigest())
        self.assertEqual(json.loads(result.stdout)["configDigest"], f"sha256:{config}")

        redacted = self.run_helper("redact", b'token=top-secret Authorization: Bearer very-secret password: "also-secret"\n')
        self.assertEqual(redacted.returncode, 0, redacted.stderr)
        self.assertIn("REDACTED", redacted.stdout)
        self.assertNotRegex(redacted.stdout, r"top-secret|very-secret|also-secret")

    def test_redaction_handles_quoted_json_secrets_without_truncating_fields(self):
        log = (
            '{"event":"startup","token":"json-token-secret",'
            '"authorization":"Bearer json-bearer-secret",'
            '"password":"json-password-secret",'
            '"api_key":"json-api-secret",'
            '"private_key":"json-private-secret",'
            '"request_id":"keep-request-id","message":"keep-message"}\n'
        ).encode()
        redacted = self.run_helper("redact", log)
        self.assertEqual(redacted.returncode, 0, redacted.stderr)
        for secret in (
            "json-token-secret",
            "json-bearer-secret",
            "json-password-secret",
            "json-api-secret",
            "json-private-secret",
        ):
            self.assertNotIn(secret, redacted.stdout)
        self.assertIn('"request_id":"keep-request-id"', redacted.stdout)
        self.assertIn('"message":"keep-message"', redacted.stdout)
        self.assertGreaterEqual(redacted.stdout.count("REDACTED"), 5)

    def test_script_uses_native_platform_inspection_and_records_identity(self):
        for contract in (
            "docker image inspect",
            "Architecture",
            "RepoDigests",
            "image_id",
            "repo_digest",
            "EXPECTED_ARCH",
            "docker top",
            "docker buildx imagetools inspect --raw \"$IMAGE\"",
            "docker buildx imagetools inspect --raw \"$IMMUTABLE_IMAGE\"",
            "gmpay-image-manifest.py",
            "IMMUTABLE_IMAGE",
            "platform_digest",
            "sha256:[0-9a-f]{64}",
        ):
            with self.subTest(contract=contract):
                self.assertIn(contract, self.script)
        self.assertRegex(self.script, r"docker\s+pull\s+\"\$IMMUTABLE_IMAGE\"")
        self.assertNotRegex(self.script, r"docker\s+pull\s+\"\$IMAGE\"")
        self.assertRegex(self.script, r"timeout\s+[0-9]+s\s+docker\s+pull\s+\"\$IMMUTABLE_IMAGE\"")

    def test_script_resolves_tag_before_pull_and_runs_only_immutable_reference(self):
        tag_resolution = self.script.index('docker buildx imagetools inspect --raw "$IMAGE"')
        immutable_resolution = self.script.index('docker buildx imagetools inspect --raw "$IMMUTABLE_IMAGE"')
        immutable_pull = self.script.index('docker pull "$IMMUTABLE_IMAGE"')
        immutable_run = self.script.index('"$IMMUTABLE_IMAGE" >/dev/null')
        self.assertLess(tag_resolution, immutable_resolution)
        self.assertLess(immutable_resolution, immutable_pull)
        self.assertLess(immutable_pull, immutable_run)
        self.assertNotIn('"$IMAGE" >/dev/null', self.script)

    def test_script_has_single_container_lifecycle_and_exact_runtime_settings(self):
        self.assertGreaterEqual(len(re.findall(r"^start_container\s*\(\)", self.script, re.MULTILINE)), 1)
        self.assertGreaterEqual(len(re.findall(r"^start_container$", self.script, re.MULTILINE)), 2)
        self.assertGreaterEqual(len(re.findall(r"docker\s+rm\s+-f", self.script)), 1)
        for contract in (
            "--env EPUSDT_CONFIG=/data/.env",
            "--volume \"$data_dir:/data\"",
            "--restart unless-stopped",
            "--network none",
            "PortBindings",
            "RestartPolicy",
            "Mounts",
            "EPUSDT_CONFIG=/data/.env",
        ):
            with self.subTest(contract=contract):
                self.assertIn(contract, self.script)
        run_block = re.search(
            r"docker\s+run\s+--detach\s+\\\n(?P<args>(?:.*\\\n)+)\s+\"\$IMMUTABLE_IMAGE\"",
            self.script,
        ).group("args")
        self.assertNotRegex(run_block, r"(?m)^\s+(?:-p|--publish)(?:\s|\\|$)")
        self.assertRegex(self.script, r"(?m)^trap .*EXIT")
        self.assertIn('docker rm -f "$container"', self.script)
        self.assertIn('rm -rf "$data_dir"', self.script)

    def test_script_checks_wget_bounded_readiness_and_replacement_persistence(self):
        self.assertRegex(self.script, r"docker\s+exec .*command\s+-v\s+wget")
        self.assertIn("wget -q -O /dev/null http://localhost:8000/", self.script)
        self.assertRegex(self.script, r"while .*SECONDS.*READINESS_DEADLINE_SECONDS")
        self.assertRegex(self.script, r"sleep \"\$READINESS_POLL_SECONDS\"")
        self.assertIn("assert_wget_and_readiness", self.script)
        self.assertIn("process_count", self.script)
        self.assertRegex(self.script, r"timeout .*docker exec .*wget -q -O /dev/null http://localhost:8000/")
        self.assertRegex(self.script, r"timeout .*docker exec .*command -v wget")
        self.assertGreaterEqual(self.script.count("assert_one_process"), 2)
        self.assertNotIn("-eo pid=", self.script)
        self.assertIn("process_count=$((process_lines - 1))", self.script)
        self.assertIn("compatibility-sentinel", self.script)
        self.assertIn('docker exec "$container" sh -ceu', self.script)
        self.assertIn('> "/data/$2"', self.script)
        self.assertRegex(self.script, r"test \"\$\(cat .*\$sentinel")
        self.assertRegex(self.script, r"docker exec .*cat .*sentinel")
        self.assertRegex(self.script, r"docker\s+ps\s+--filter\s+\"label=\$TEST_LABEL\"")

    def test_script_canonicalizes_bind_sources_and_writes_nonempty_phase_failure_artifact(self):
        self.assertIn('realpath -e -- "$data_dir"', self.script)
        self.assertIn('realpath -e -- "$inspected_source"', self.script)
        self.assertIn('phase=', self.script)
        self.assertIn("printf 'phase=%s\\nstatus=%s\\n' \"$phase\" \"$1\"", self.script)
        self.assertIn('test -s "$failure_log"', self.script)

    def test_early_pull_failure_writes_phase_artifact_without_docker(self):
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            fake_bin = directory / "bin"
            fake_bin.mkdir()
            fake_docker = fake_bin / "docker"
            fake_docker.write_text(
                "#!/bin/sh\n"
                "case \"$1\" in\n"
                "  pull) exit 17 ;;\n"
                "  container) exit 1 ;;\n"
                "  rm) exit 0 ;;\n"
                "  *) exit 1 ;;\n"
                "esac\n"
            )
            fake_docker.chmod(0o755)
            evidence = directory / "evidence.json"
            environment = dict(__import__("os").environ)
            environment.update(
                {
                    "EXPECTED_ARCH": "amd64",
                    "EVIDENCE_FILE": str(evidence),
                    "PATH": f"{fake_bin}:{environment['PATH']}",
                    "TMPDIR": str(directory),
                }
            )
            result = subprocess.run(
                ["bash", str(SCRIPT)],
                env=environment,
                text=True,
                capture_output=True,
            )
            self.assertNotEqual(result.returncode, 0)
            failure = Path(f"{evidence}.failure.log")
            self.assertTrue(failure.is_file())
            self.assertRegex(failure.read_text(), r"(?m)^phase=tag-manifest-inspect$")
            self.assertRegex(failure.read_text(), r"(?m)^status=[1-9][0-9]*$")

    def test_script_does_not_have_unbounded_polling_or_sleep(self):
        self.assertNotRegex(self.script, r"while\s+true|while\s*:\s*;|until\s+false")
        for sleep in re.findall(r"(?m)^\s*sleep\s+([^\s;]+)", self.script):
            self.assertIn(sleep, ('"$READINESS_POLL_SECONDS"', "'$READINESS_POLL_SECONDS'"))

    def test_script_rejects_transactional_or_secret_bearing_operations(self):
        self.assertNotRegex(self.script, r"(?i)\b(wallet|rpc|payment|callback)\b")
        self.assertNotRegex(self.script, r"\b(curl|nc|psql|redis-cli|openssl)\b")
        self.assertNotRegex(self.script, r"https?://(?!localhost:8000)")
        self.assertIn("REDACTED", self.script)

    def test_workflow_has_native_architecture_matrix_and_exact_sha_gate(self):
        job = self.job_block(self.workflow, "gmpay-image-contract")
        self.assertIn("    timeout-minutes: 15", job)
        self.assertIn("    runs-on: ${{ matrix.runner }}", job)
        self.assertIn("          - arch: amd64\n            runner: ubuntu-24.04", job)
        self.assertIn("          - arch: arm64\n            runner: ubuntu-24.04-arm", job)
        self.assertIn("      - name: Verify exact target SHA", job)
        self.assertIn("gmpay-image-contract-test.sh", job)
        self.assertIn("gmpay-image-contract-v1", job)
        self.assertIn("timeout-minutes: 15", job)
        self.assertIn('re.fullmatch(r"sha256:[0-9a-f]{64}", evidence["platformDigest"])', job)
        self.assertIn('"platformDigest"', job)
        self.assertIn('"configDigest"', job)
        self.assertIn('"immutableImage"', job)
        self.assertIn('evidence["imageId"] == evidence["configDigest"]', job)

    def test_workflow_run_blocks_are_valid_bash_after_expression_substitution(self):
        expression = re.compile(r"\$\{\{.*?\}\}")
        for index, run in enumerate(self.run_blocks(self.workflow)):
            normalized = expression.sub("workflow_expression", run)
            result = subprocess.run(
                ["bash", "-n"],
                input=normalized,
                text=True,
                capture_output=True,
            )
            self.assertEqual(result.returncode, 0, f"workflow run block {index}: {result.stderr}")


if __name__ == "__main__":
    unittest.main()
