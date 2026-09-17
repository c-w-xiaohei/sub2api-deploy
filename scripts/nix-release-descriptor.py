#!/usr/bin/env python3
"""Create a small Nix descriptor for one exact-SHA controller candidate."""
import argparse
import gzip
import hashlib
import json
import re
import shutil
import stat
import subprocess
import tarfile
import tempfile
from pathlib import Path, PurePosixPath


ROOT = Path(__file__).resolve().parents[1]
COMMIT = re.compile(r"^[0-9a-f]{40}$")
TAG = re.compile(r"^v(?:0|[1-9][0-9]*)[.](?:0|[1-9][0-9]*)[.](?:0|[1-9][0-9]*)(?:-[0-9A-Za-z-]+(?:[.][0-9A-Za-z-]+)*)?$")
SHA256 = re.compile(r"^[0-9a-f]{64}$")
DESCRIPTOR_FILES = [
    "flake.nix",
    "flake.lock",
    "nix/environments.nix",
    "nix/prebuilt.nix",
    "nix/runtime-lib.nix",
    "nix/runtime-packages.nix",
    "nix/install-runtime.sh",
    "nix/controller-workspace-init.sh",
    "nix/host-activate.sh",
]
REQUIRED_PROJECT_FILES = [
    "bin/sub2api-deploy",
    "bin/go",
    "bin/pulumi-resource-sub2api-host",
    "bin/pulumi-program",
    "Pulumi.yaml",
    "go.mod",
    "artifacts/sub2api-host/manifest.json",
    "artifacts/sub2api-host/sub2api-host-linux-amd64",
    "artifacts/sub2api-host/sub2api-host-linux-arm64",
    "scripts/pulumi-plugins/cloudflare/pulumi-plugin.json",
    "scripts/pulumi-plugins/upstash/pulumi-plugin.json",
]
HOST_MANIFEST_PATH = "artifacts/sub2api-host/manifest.json"
HOST_ARTIFACTS = {
    "x86_64-linux": ("linux-amd64", "artifacts/sub2api-host/sub2api-host-linux-amd64"),
    "aarch64-linux": ("linux-arm64", "artifacts/sub2api-host/sub2api-host-linux-arm64"),
}


def fail(message):
    raise SystemExit("nix release descriptor: " + message)


def checked_descriptor_source(root, relative):
    root = Path(root)
    current = root
    for part in PurePosixPath(relative).parts:
        current /= part
        try:
            metadata = current.lstat()
        except OSError as error:
            fail("descriptor source is missing: %s (%s)" % (relative, error))
        if stat.S_ISLNK(metadata.st_mode):
            fail("descriptor source contains a symlink: %s" % relative)
    if not stat.S_ISREG(metadata.st_mode):
        fail("descriptor source is not a regular file: %s" % relative)
    return current


def extract_archive(archive, destination):
    try:
        with tarfile.open(archive, "r:*") as bundle:
            members = bundle.getmembers()
            names = set()
            roots = set()
            for member in members:
                path = PurePosixPath(member.name)
                if (
                    path.is_absolute()
                    or not member.name
                    or "\\" in member.name
                    or "." in path.parts
                    or ".." in path.parts
                ):
                    fail("unsafe archive member: %r" % member.name)
                if not (member.isfile() or member.isdir()) or member.name in names:
                    fail("archive contains a duplicate or non-regular member: %r" % member.name)
                names.add(member.name)
                roots.add(path.parts[0])
            if len(roots) != 1:
                fail("candidate archive must contain one payload root")
            for member in members:
                target = destination.joinpath(*PurePosixPath(member.name).parts)
                if member.isdir():
                    target.mkdir(parents=True, exist_ok=True)
                    continue
                target.parent.mkdir(parents=True, exist_ok=True)
                source = bundle.extractfile(member)
                if source is None:
                    fail("cannot read archive member: %r" % member.name)
                with target.open("xb") as output:
                    shutil.copyfileobj(source, output)
                target.chmod(0o755 if member.mode & 0o111 else 0o644)
    except (OSError, tarfile.TarError) as error:
        fail("cannot safely extract candidate: %s" % error)
    return destination / next(iter(roots))


def nar_hash(command, path):
    try:
        result = subprocess.run([command, str(path)], check=True, text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    except (OSError, subprocess.CalledProcessError) as error:
        fail("cannot calculate candidate NAR hash: %s" % error)
    value = result.stdout.strip()
    if not re.fullmatch(r"sha256-[A-Za-z0-9+/]{43}=", value):
        fail("NAR hash command returned an invalid SHA-256")
    return value


def checked_host_manifest(payload):
    manifest_path = payload / HOST_MANIFEST_PATH
    if not manifest_path.is_file() or manifest_path.is_symlink():
        fail("Host artifact manifest is missing or not a regular file")
    try:
        manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
    except (OSError, UnicodeDecodeError, json.JSONDecodeError) as error:
        fail("Host artifact manifest is invalid: %s" % error)
    if not isinstance(manifest, dict) or set(manifest) != {"schemaVersion", "release", "linux-amd64", "linux-arm64"}:
        fail("Host artifact manifest has an invalid schema")
    if manifest["schemaVersion"] != 1 or not isinstance(manifest["release"], str) or not manifest["release"]:
        fail("Host artifact manifest has an invalid schema")
    host_payload = {}
    for system, (key, expected_path) in HOST_ARTIFACTS.items():
        record = manifest.get(key)
        if not isinstance(record, dict) or set(record) != {"path", "sha256", "size"}:
            fail("Host artifact manifest has an invalid schema")
        if record["path"] != PurePosixPath(expected_path).name:
            fail("Host artifact manifest path does not bind to %s" % system)
        if not isinstance(record["sha256"], str) or SHA256.fullmatch(record["sha256"]) is None:
            fail("Host artifact manifest checksum is invalid: %s" % system)
        if not isinstance(record["size"], int) or isinstance(record["size"], bool) or not 0 <= record["size"] <= 67108864:
            fail("Host artifact manifest size is invalid: %s" % system)
        artifact = payload / expected_path
        if not artifact.is_file() or artifact.is_symlink() or not artifact.stat().st_mode & 0o111:
            fail("Host artifact is missing, not regular, or not executable: %s" % system)
        actual_size = artifact.stat().st_size
        actual_hash = hashlib.sha256(artifact.read_bytes()).hexdigest()
        if record["size"] != actual_size:
            fail("Host artifact size does not match candidate bytes: %s" % system)
        if record["sha256"] != actual_hash:
            fail("Host artifact checksum does not match candidate bytes: %s" % system)
        host_payload[system] = {"path": expected_path, "sha256": actual_hash}
    return manifest["release"], host_payload


def write_descriptor_archive(descriptor, archive):
    descriptor = Path(descriptor)
    with Path(archive).open("wb") as raw, gzip.GzipFile(filename="", mode="wb", fileobj=raw, mtime=0) as compressed:
        with tarfile.open(fileobj=compressed, mode="w", format=tarfile.USTAR_FORMAT) as bundle:
            for path in sorted(item for item in descriptor.rglob("*") if item.is_file() and not item.is_symlink()):
                info = tarfile.TarInfo("descriptor/" + path.relative_to(descriptor).as_posix())
                info.size = path.stat().st_size
                info.mode = 0o755 if path.stat().st_mode & 0o111 else 0o644
                info.uid = info.gid = info.mtime = 0
                info.uname = info.gname = ""
                with path.open("rb") as source:
                    bundle.addfile(info, source)


def generate(args):
    valid_repository = re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", args.repository)
    if not TAG.fullmatch(args.tag) or not COMMIT.fullmatch(args.project_sha) or not valid_repository:
        fail("tag, project SHA, or repository is invalid")
    candidate_input = Path(args.candidate_archive)
    try:
        candidate_metadata = candidate_input.lstat()
    except OSError as error:
        fail("candidate archive is missing: %s" % error)
    if not stat.S_ISREG(candidate_metadata.st_mode) or stat.S_ISLNK(candidate_metadata.st_mode):
        fail("candidate archive must be a regular non-symlink file")
    candidate = candidate_input.resolve()
    metadata = json.loads(Path(args.metadata).read_text(encoding="utf-8"))
    digest = hashlib.sha256(candidate.read_bytes()).hexdigest() if candidate.is_file() and not candidate.is_symlink() else ""
    expected_name = "sub2api-controller-%s.tar.gz" % args.project_sha
    metadata_matches = (
        metadata.get("sha") == args.project_sha
        and metadata.get("archive") == expected_name
        and metadata.get("sha256") == digest
        and SHA256.fullmatch(digest) is not None
    )
    if not metadata_matches:
        fail("candidate metadata does not bind the exact archive bytes")
    output = Path(args.output_dir).resolve()
    if output.exists():
        fail("output directory already exists")
    output.mkdir(parents=True)
    descriptor = output / "descriptor"
    for relative in DESCRIPTOR_FILES:
        source = checked_descriptor_source(ROOT, relative)
        target = descriptor / relative
        target.parent.mkdir(parents=True, exist_ok=True)
        shutil.copy2(source, target)
    with tempfile.TemporaryDirectory() as temporary:
        payload = extract_archive(candidate, Path(temporary))
        for required in REQUIRED_PROJECT_FILES:
            if not (payload / required).is_file():
                fail("candidate misses required project file: %s" % required)
        host_release, host_payload = checked_host_manifest(payload)
        expected_host_release = "sub2api-host-controller@sha256:" + hashlib.sha256(args.project_sha.encode()).hexdigest()
        if host_release != expected_host_release:
            fail("Host release identity does not match project SHA")
        tree_hash = nar_hash(args.nar_command, payload)
    lock = {
        "schemaVersion": 2,
        "controller": {
            "version": args.tag,
            "projectCommit": args.project_sha,
            "url": "https://github.com/%s/releases/download/%s/%s" % (args.repository, args.tag, expected_name),
            "narHash": tree_hash,
            "hostRelease": host_release,
            "hostPayload": host_payload,
        },
    }
    lock_path = descriptor / "nix/runtime-release.json"
    lock_path.parent.mkdir(parents=True, exist_ok=True)
    lock_path.write_text(json.dumps(lock, indent=2, sort_keys=True) + "\n", encoding="utf-8")
    shutil.copy2(lock_path, output / "runtime-release.json")
    archive = output / ("nix-runtime-descriptor-%s.tar.gz" % args.tag)
    write_descriptor_archive(descriptor, archive)
    digest = hashlib.sha256(archive.read_bytes()).hexdigest()
    (output / (archive.name + ".sha256")).write_text("%s  %s\n" % (digest, archive.name), encoding="utf-8")


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    command = parser.add_subparsers(dest="command", required=True).add_parser("generate")
    command.add_argument("--candidate-archive", required=True)
    command.add_argument("--metadata", required=True)
    command.add_argument("--tag", required=True)
    command.add_argument("--project-sha", required=True)
    command.add_argument("--repository", required=True)
    command.add_argument("--output-dir", required=True)
    command.add_argument("--nar-command", required=True)
    generate(parser.parse_args(argv))


if __name__ == "__main__":
    main()
