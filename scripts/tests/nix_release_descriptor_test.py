#!/usr/bin/env python3
import hashlib
import importlib.util
import io
import json
import tarfile
import tempfile
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
SPEC = importlib.util.spec_from_file_location("nix_release_descriptor", ROOT / "scripts/nix-release-descriptor.py")
nix_release_descriptor = importlib.util.module_from_spec(SPEC)


class ReleaseDescriptorTests(unittest.TestCase):
    def setUp(self):
        SPEC.loader.exec_module(nix_release_descriptor)

    def make_archive(self, directory, name, role="controller", system="x86_64-linux", inventory_mutation=None, omit=None, overrides=None):
        archive = directory / name
        required = nix_release_descriptor.required_paths_for(role)
        executable = set(nix_release_descriptor.executable_paths_for(role))
        directories = {path for path in required if path.endswith("/plugins")}
        files = {path: (b"#!/bin/sh\nexit 0\n" if path.startswith(("bin/", "tools/bin/")) else b"fixture")
                 for path in required if path not in directories and path != "share/sub2api-runtime/inventory.json"}
        if role == "controller":
            files["workspace/bin/pulumi-program"] = files["bin/pulumi-program"]
            files["Pulumi.yaml"] = files["workspace/Pulumi.yaml"]
            files["workspace/bin/plugins/cloudflare/pulumi-resource-cloudflare"] = b"#!/bin/sh\nexit 0\n"
        files.update(overrides or {})
        if omit:
            files.pop(omit, None)
        files["share/sub2api-runtime/inventory.json"] = b"pending"
        inventory = {
            "schemaVersion": 1,
            "role": role,
            "system": system,
            "version": "v1.2.3",
            "projectCommit": "a" * 40,
            "requiredPaths": required,
            "files": [{"path": path, "sha256": hashlib.sha256(value).hexdigest()} for path, value in files.items() if path != "share/sub2api-runtime/inventory.json"],
        }
        if inventory_mutation:
            inventory_mutation(inventory)
        files["share/sub2api-runtime/inventory.json"] = json.dumps(inventory, sort_keys=True).encode()
        with tarfile.open(archive, "w:gz") as bundle:
            for path, value in files.items():
                member = tarfile.TarInfo("payload/" + path)
                member.size = len(value)
                member.mode = 0o755 if path in executable or "/plugins/" in path else 0o644
                bundle.addfile(member, io.BytesIO(value))
        return archive

    def manifest(self, archive, role="controller", system="x86_64-linux", candidate_archive=None):
        candidate_archive = candidate_archive or archive
        return {
            "schemaVersion": 1,
            "projectCommit": "a" * 40,
            "knownCandidateSHA256": {archive.name: hashlib.sha256(archive.read_bytes()).hexdigest()},
            "ciCandidate": {
                "runId": "123",
                "archive": candidate_archive.name,
                "archiveSha256": hashlib.sha256(candidate_archive.read_bytes()).hexdigest(),
                "projectCommit": "a" * 40,
            },
            "artifacts": [{
                "role": role,
                "system": system,
                "version": "v1.2.3",
                "projectCommit": "a" * 40,
                "asset": archive.name,
                "requiredPaths": nix_release_descriptor.required_paths_for(role),
            }],
        }

    def nar_tool(self, directory):
        # This deterministic stand-in validates the CLI boundary only; it is not a production NAR implementation.
        tool = directory / "fake-nar"
        tool.write_text("#!/bin/sh\n[ -d \"$1\" ] || exit 2\nprintf 'sha256-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=\\n'\n")
        tool.chmod(0o755)
        return tool

    def run_generate(self, directory, manifest, nar=None, candidate=None, require_complete=False):
        manifest_path = directory / "manifest.json"
        manifest_path.write_text(json.dumps(manifest))
        output = directory / ("output-" + manifest["artifacts"][0]["asset"])
        candidate = candidate or directory / manifest["ciCandidate"]["archive"]
        args = ["generate", "--input-dir", str(directory), "--manifest", str(manifest_path), "--candidate-archive", str(candidate), "--tag", "v1.2.3", "--project-sha", "a" * 40, "--output-dir", str(output), "--nar-command", str(nar or self.nar_tool(directory))]
        if require_complete:
            args.append("--require-complete")
        nix_release_descriptor.main(args)
        return output

    def test_generates_lock_and_descriptor_from_audited_tarball(self):
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            archive = self.make_archive(directory, "controller.tar.gz")
            output = self.run_generate(directory, self.manifest(archive))
            lock = json.loads((output / "runtime-release.json").read_text())
            artifact = lock["artifacts"][0]
            self.assertEqual(artifact["role"], "controller")
            self.assertEqual(artifact["projectCommit"], "a" * 40)
            self.assertEqual(artifact["archiveSha256"], nix_release_descriptor.sri(hashlib.sha256(archive.read_bytes()).hexdigest()))
            self.assertEqual(artifact["narHash"], "sha256-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
            self.assertEqual(artifact["requiredPaths"], self.manifest(archive)["artifacts"][0]["requiredPaths"])
            with tempfile.TemporaryDirectory() as extracted:
                payload = nix_release_descriptor.extract_archive(archive, Path(extracted))
                self.assertEqual((payload / "bin/sub2api-deploy").stat().st_mode & 0o777, 0o755)
                self.assertEqual((payload / "workspace/Pulumi.yaml").stat().st_mode & 0o777, 0o644)
            self.assertEqual((output / "descriptor/flake.nix").read_text(), (ROOT / "flake.nix").read_text())
            self.assertTrue((output / "descriptor/nix/runtime-lib.nix").is_file())
            self.assertTrue((output / "descriptor/flake.lock").is_file())
            self.assertTrue((output / "descriptor/nix/controller-workspace-init.sh").is_file())
            self.assertFalse((output / "descriptor/nix/tests").exists())
            self.assertFalse((output / "descriptor/nix/runtime-release.json").read_text() == (ROOT / "nix/runtime-release.json").read_text())
            descriptor_files = {path.relative_to(output / "descriptor").as_posix() for path in (output / "descriptor").rglob("*") if path.is_file()}
            self.assertEqual(descriptor_files, {
                "flake.nix", "flake.lock", "nix/environments.nix", "nix/runtime-lib.nix",
                "nix/runtime-contract.json",
                "nix/controller-workspace-init.sh", "nix/host-activate.sh", "nix/runtime-release.json",
            })

    def test_rejects_missing_audited_asset_and_tampered_candidate_bytes(self):
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            archive = self.make_archive(directory, "controller.tar.gz")
            manifest = self.manifest(archive)
            archive.write_bytes(archive.read_bytes() + b"tampered")
            with self.assertRaises(SystemExit):
                self.run_generate(directory, manifest)
            manifest["artifacts"][0]["asset"] = "missing.tar.gz"
            with self.assertRaises(SystemExit):
                self.run_generate(directory, manifest)

    def test_rejects_self_referential_or_mismatched_inventory(self):
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            archive = self.make_archive(directory, "controller.tar.gz", inventory_mutation=lambda inventory: inventory.update({"archiveSha256": "b" * 64}))
            with self.assertRaises(SystemExit):
                self.run_generate(directory, self.manifest(archive))
            archive = self.make_archive(directory, "mismatch.tar.gz", system="aarch64-linux")
            with self.assertRaises(SystemExit):
                self.run_generate(directory, self.manifest(archive))

    def test_rejects_unsafe_tar_members(self):
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            archive = directory / "controller.tar.gz"
            with tarfile.open(archive, "w:gz") as bundle:
                member = tarfile.TarInfo("payload/bin/link")
                member.type = tarfile.SYMTYPE
                member.linkname = "/etc/passwd"
                bundle.addfile(member)
            with self.assertRaises(SystemExit):
                self.run_generate(directory, self.manifest(archive))

    def test_rejects_required_paths_that_disagree_between_manifest_and_inventory(self):
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            archive = self.make_archive(directory, "controller.tar.gz")
            manifest = self.manifest(archive)
            manifest["artifacts"][0]["requiredPaths"] = ["bin/other"]
            with self.assertRaises(SystemExit):
                self.run_generate(directory, manifest)

    def test_rejects_missing_or_nonexecutable_required_entrypoint(self):
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            bad = self.make_archive(directory, "bad.tar.gz", omit="bin/pulumi")
            with self.assertRaisesRegex(SystemExit, "required runtime file is missing"):
                self.run_generate(directory, self.manifest(bad))

            archive = self.make_archive(directory, "mode.tar.gz")
            with tarfile.open(archive, "r:gz") as source, tarfile.open(directory / "bad-mode.tar.gz", "w:gz") as target:
                for member in source.getmembers():
                    if member.name == "payload/bin/pulumi":
                        member.mode = 0o644
                    data = source.extractfile(member) if member.isfile() else None
                    target.addfile(member, data)
            bad_mode = directory / "bad-mode.tar.gz"
            with self.assertRaisesRegex(SystemExit, "not executable"):
                self.run_generate(directory, self.manifest(bad_mode))

            archive = self.make_archive(directory, "bad-workspace-mode.tar.gz")
            with tarfile.open(archive, "r:gz") as source, tarfile.open(directory / "bad-workspace.tar.gz", "w:gz") as target:
                for member in source.getmembers():
                    if member.name == "payload/workspace/bin/pulumi-program":
                        member.mode = 0o644
                    data = source.extractfile(member) if member.isfile() else None
                    target.addfile(member, data)
            bad_workspace = directory / "bad-workspace.tar.gz"
            with self.assertRaisesRegex(SystemExit, "not executable"):
                self.run_generate(directory, self.manifest(bad_workspace))

    def test_rejects_project_component_that_differs_from_ci_candidate(self):
        compared = nix_release_descriptor.PROJECT_COMPONENTS + ["workspace/bin/pulumi-program", "workspace/Pulumi.yaml"]
        for index, path in enumerate(compared):
            with self.subTest(path=path), tempfile.TemporaryDirectory() as temporary:
                directory = Path(temporary)
                candidate = self.make_archive(directory, "candidate.tar.gz")
                runtime = self.make_archive(directory, "runtime-%d.tar.gz" % index, overrides={path: b"tampered"})
                manifest = self.manifest(runtime, candidate_archive=candidate)
                with self.assertRaisesRegex(SystemExit, "differs from the tested CI candidate"):
                    self.run_generate(directory, manifest, candidate=candidate)

    def test_complete_descriptor_uses_the_supported_role_system_matrix(self):
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            controller = self.make_archive(directory, "controller.tar.gz")
            host_amd64 = self.make_archive(directory, "host-amd64.tar.gz", role="host-environment")
            host_arm64 = self.make_archive(directory, "host-arm64.tar.gz", role="host-environment", system="aarch64-linux")
            manifest = self.manifest(controller)
            for archive, system in ((host_amd64, "x86_64-linux"), (host_arm64, "aarch64-linux")):
                manifest["knownCandidateSHA256"][archive.name] = hashlib.sha256(archive.read_bytes()).hexdigest()
                manifest["artifacts"].append({
                    "role": "host-environment",
                    "system": system,
                    "version": "v1.2.3",
                    "projectCommit": "a" * 40,
                    "asset": archive.name,
                    "requiredPaths": nix_release_descriptor.required_paths_for("host-environment"),
                })
            output = self.run_generate(directory, manifest, candidate=controller, require_complete=True)
            lock = json.loads((output / "runtime-release.json").read_text())
            self.assertEqual({(entry["role"], entry["system"]) for entry in lock["artifacts"]}, {
                ("controller", "x86_64-linux"),
                ("host-environment", "x86_64-linux"),
                ("host-environment", "aarch64-linux"),
            })


if __name__ == "__main__":
    unittest.main()
