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
module = importlib.util.module_from_spec(SPEC)


class DescriptorTests(unittest.TestCase):
    def setUp(self):
        SPEC.loader.exec_module(module)

    def fixture(self, directory):
        sha = "a" * 40
        archive = directory / ("sub2api-controller-%s.tar.gz" % sha)
        amd64 = b"amd64"
        arm64 = b"arm64"
        files = {
            "bin/sub2api-deploy": b"deploy", "bin/go": b"go", "bin/pulumi-resource-sub2api-host": b"provider",
            "bin/pulumi-program": b"program", "Pulumi.yaml": b"name: fixture\n", "go.mod": b"module fixture\n",
            "artifacts/sub2api-host/sub2api-host-linux-amd64": amd64,
            "artifacts/sub2api-host/sub2api-host-linux-arm64": arm64,
            "scripts/pulumi-plugins/cloudflare/pulumi-plugin.json": b"{}\n",
            "scripts/pulumi-plugins/upstash/pulumi-plugin.json": b"{}\n",
        }
        files["artifacts/sub2api-host/manifest.json"] = (json.dumps({
            "schemaVersion": 1,
            "release": "sub2api-host-controller@sha256:" + hashlib.sha256(sha.encode()).hexdigest(),
            "linux-amd64": {
                "path": "sub2api-host-linux-amd64",
                "sha256": hashlib.sha256(amd64).hexdigest(),
                "size": len(amd64),
            },
            "linux-arm64": {
                "path": "sub2api-host-linux-arm64",
                "sha256": hashlib.sha256(arm64).hexdigest(),
                "size": len(arm64),
            },
        }, sort_keys=True) + "\n").encode()
        with tarfile.open(archive, "w:gz") as bundle:
            for path, value in files.items():
                member = tarfile.TarInfo("bundle/" + path)
                member.size = len(value)
                member.mode = 0o755 if path.startswith("bin/") or path.endswith(("sub2api-host-linux-amd64", "sub2api-host-linux-arm64")) else 0o644
                bundle.addfile(member, io.BytesIO(value))
        metadata = directory / "metadata.json"
        metadata.write_text(json.dumps({"sha": sha, "archive": archive.name, "sha256": hashlib.sha256(archive.read_bytes()).hexdigest()}))
        nar = directory / "nar"
        nar.write_text("#!/bin/sh\nprintf 'sha256-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=\\n'\n")
        nar.chmod(0o755)
        return sha, archive, metadata, nar

    def generate(self, directory):
        sha, archive, metadata, nar = self.fixture(directory)
        output = directory / "out"
        module.main(["generate", "--candidate-archive", str(archive), "--metadata", str(metadata), "--tag", "v1.2.3", "--project-sha", sha, "--repository", "example/project", "--output-dir", str(output), "--nar-command", str(nar)])
        return output

    def test_generates_small_descriptor_for_exact_candidate(self):
        with tempfile.TemporaryDirectory() as temporary:
            output = self.generate(Path(temporary))
            lock = json.loads((output / "runtime-release.json").read_text())
            self.assertEqual(lock["schemaVersion"], 2)
            self.assertEqual(lock["controller"]["narHash"], "sha256-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
            self.assertEqual(lock["controller"]["url"], "https://github.com/example/project/releases/download/v1.2.3/sub2api-controller-%s.tar.gz" % ("a" * 40))
            self.assertEqual(lock["controller"]["hostPayload"], {
                "aarch64-linux": {
                    "path": "artifacts/sub2api-host/sub2api-host-linux-arm64",
                    "sha256": hashlib.sha256(b"arm64").hexdigest(),
                },
                "x86_64-linux": {
                    "path": "artifacts/sub2api-host/sub2api-host-linux-amd64",
                    "sha256": hashlib.sha256(b"amd64").hexdigest(),
                },
            })
            self.assertEqual(lock["controller"]["hostRelease"], "sub2api-host-controller@sha256:" + hashlib.sha256(("a" * 40).encode()).hexdigest())
            files = {path.relative_to(output / "descriptor").as_posix() for path in (output / "descriptor").rglob("*") if path.is_file()}
            self.assertEqual(files, set(module.DESCRIPTOR_FILES + ["nix/runtime-release.json"]))
            archive = output / "nix-runtime-descriptor-v1.2.3.tar.gz"
            self.assertEqual((output / (archive.name + ".sha256")).read_text(), "%s  %s\n" % (hashlib.sha256(archive.read_bytes()).hexdigest(), archive.name))

    def test_rejects_tampered_candidate(self):
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            sha, archive, metadata, nar = self.fixture(directory)
            archive.write_bytes(archive.read_bytes() + b"tampered")
            with self.assertRaisesRegex(SystemExit, "metadata"):
                module.main(["generate", "--candidate-archive", str(archive), "--metadata", str(metadata), "--tag", "v1.2.3", "--project-sha", sha, "--repository", "example/project", "--output-dir", str(directory / "out"), "--nar-command", str(nar)])

    def test_rejects_candidate_symlink_before_reading_it(self):
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            sha, archive, metadata, nar = self.fixture(directory)
            alias = directory / "candidate.tar.gz"
            alias.symlink_to(archive)
            with self.assertRaisesRegex(SystemExit, "symlink"):
                module.main(["generate", "--candidate-archive", str(alias), "--metadata", str(metadata), "--tag", "v1.2.3", "--project-sha", sha, "--repository", "example/project", "--output-dir", str(directory / "out"), "--nar-command", str(nar)])

    def test_rejects_host_manifest_release_not_bound_to_project_sha(self):
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            sha, archive, metadata, nar = self.fixture(directory)
            with tarfile.open(archive, "r:gz") as source, tarfile.open(directory / "wrong-release.tar.gz", "w:gz") as target:
                for member in source.getmembers():
                    data = source.extractfile(member) if member.isfile() else None
                    if member.name.endswith("manifest.json"):
                        manifest = json.loads(data.read())
                        manifest["release"] = "sub2api-host-controller@sha256:" + "b" * 64
                        value = (json.dumps(manifest, sort_keys=True) + "\n").encode()
                        member.size = len(value)
                        data = io.BytesIO(value)
                    target.addfile(member, data)
            archive.unlink()
            (directory / "wrong-release.tar.gz").rename(archive)
            metadata.write_text(json.dumps({"sha": sha, "archive": archive.name, "sha256": hashlib.sha256(archive.read_bytes()).hexdigest()}))
            with self.assertRaisesRegex(SystemExit, "release identity"):
                module.main(["generate", "--candidate-archive", str(archive), "--metadata", str(metadata), "--tag", "v1.2.3", "--project-sha", sha, "--repository", "example/project", "--output-dir", str(directory / "out"), "--nar-command", str(nar)])

    def test_rejects_descriptor_source_symlink_or_symlink_ancestor(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary) / "root"
            real = Path(temporary) / "real"
            for relative in module.DESCRIPTOR_FILES:
                target = root / relative
                target.parent.mkdir(parents=True, exist_ok=True)
                target.write_text("fixture")
            real.mkdir()
            (real / "flake.nix").write_text("fixture")
            (root / "flake.nix").unlink()
            (root / "flake.nix").symlink_to(real / "flake.nix")
            old_root = module.ROOT
            module.ROOT = root
            try:
                sha, archive, metadata, nar = self.fixture(Path(temporary))
                with self.assertRaisesRegex(SystemExit, "symlink"):
                    module.main(["generate", "--candidate-archive", str(archive), "--metadata", str(metadata), "--tag", "v1.2.3", "--project-sha", sha, "--repository", "example/project", "--output-dir", str(Path(temporary) / "out"), "--nar-command", str(nar)])
            finally:
                module.ROOT = old_root

            (root / "flake.nix").unlink()
            (root / "nix").rename(root / "nix-real")
            (root / "nix").symlink_to(real)
            (root / "nix" / "runtime-lib.nix").parent.mkdir(parents=True, exist_ok=True)
            (root / "nix" / "runtime-lib.nix").symlink_to(real / "flake.nix")
            with self.assertRaisesRegex(SystemExit, "symlink"):
                module.checked_descriptor_source(root, "nix/runtime-lib.nix")

    def test_rejects_unsafe_archive_member(self):
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            sha, archive, metadata, nar = self.fixture(directory)
            with tarfile.open(archive, "w:gz") as bundle:
                member = tarfile.TarInfo("bundle/../../escape")
                member.size = 1
                bundle.addfile(member, io.BytesIO(b"x"))
            metadata.write_text(json.dumps({"sha": sha, "archive": archive.name, "sha256": hashlib.sha256(archive.read_bytes()).hexdigest()}))
            with self.assertRaisesRegex(SystemExit, "unsafe archive"):
                module.main(["generate", "--candidate-archive", str(archive), "--metadata", str(metadata), "--tag", "v1.2.3", "--project-sha", sha, "--repository", "example/project", "--output-dir", str(directory / "out"), "--nar-command", str(nar)])

    def test_rejects_host_manifest_that_does_not_match_candidate_bytes(self):
        mutations = [
            (lambda record: record.update({"sha256": "0" * 64}), "checksum"),
            (lambda record: record.update({"size": 999}), "size"),
            (lambda record: record.update({"path": "other"}), "path"),
        ]
        for mutate, message in mutations:
            with self.subTest(message=message), tempfile.TemporaryDirectory() as temporary:
                directory = Path(temporary)
                sha, archive, metadata, nar = self.fixture(directory)
                with tarfile.open(archive, "r:gz") as source, tarfile.open(directory / "mutated.tar.gz", "w:gz") as target:
                    for member in source.getmembers():
                        data = source.extractfile(member) if member.isfile() else None
                        if member.name.endswith("manifest.json"):
                            manifest = json.loads(data.read())
                            mutate(manifest["linux-amd64"])
                            value = (json.dumps(manifest, sort_keys=True) + "\n").encode()
                            member.size = len(value)
                            data = io.BytesIO(value)
                        target.addfile(member, data)
                archive.unlink()
                (directory / "mutated.tar.gz").rename(archive)
                metadata.write_text(json.dumps({"sha": sha, "archive": archive.name, "sha256": hashlib.sha256(archive.read_bytes()).hexdigest()}))
                with self.assertRaisesRegex(SystemExit, message):
                    module.main(["generate", "--candidate-archive", str(archive), "--metadata", str(metadata), "--tag", "v1.2.3", "--project-sha", sha, "--repository", "example/project", "--output-dir", str(directory / "out"), "--nar-command", str(nar)])

    def test_rejects_incomplete_host_manifest(self):
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            sha, archive, metadata, nar = self.fixture(directory)
            with tarfile.open(archive, "r:gz") as source, tarfile.open(directory / "incomplete.tar.gz", "w:gz") as target:
                for member in source.getmembers():
                    data = source.extractfile(member) if member.isfile() else None
                    if member.name.endswith("manifest.json"):
                        manifest = json.loads(data.read())
                        del manifest["linux-arm64"]
                        value = (json.dumps(manifest, sort_keys=True) + "\n").encode()
                        member.size = len(value)
                        data = io.BytesIO(value)
                    target.addfile(member, data)
            archive.unlink()
            (directory / "incomplete.tar.gz").rename(archive)
            metadata.write_text(json.dumps({"sha": sha, "archive": archive.name, "sha256": hashlib.sha256(archive.read_bytes()).hexdigest()}))
            with self.assertRaisesRegex(SystemExit, "schema"):
                module.main(["generate", "--candidate-archive", str(archive), "--metadata", str(metadata), "--tag", "v1.2.3", "--project-sha", sha, "--repository", "example/project", "--output-dir", str(directory / "out"), "--nar-command", str(nar)])

    def test_descriptor_archive_is_deterministic(self):
        with tempfile.TemporaryDirectory() as temporary:
            output = self.generate(Path(temporary))
            first, second = output / "first.tar.gz", output / "second.tar.gz"
            module.write_descriptor_archive(output / "descriptor", first)
            module.write_descriptor_archive(output / "descriptor", second)
            self.assertEqual(first.read_bytes(), second.read_bytes())


if __name__ == "__main__":
    unittest.main()
