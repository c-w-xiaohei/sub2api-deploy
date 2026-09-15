#!/usr/bin/env python3
"""Create a Nix runtime descriptor from audited, prebuilt release tarballs."""
import argparse
import base64
import hashlib
import json
import re
import shutil
import subprocess
import tarfile
import tempfile
from pathlib import Path, PurePosixPath


ROOT = Path(__file__).resolve().parents[1]
COMMIT = re.compile(r"^[0-9a-f]{40}$")
TAG = re.compile(r"^v(?:0|[1-9][0-9]*)[.](?:0|[1-9][0-9]*)[.](?:0|[1-9][0-9]*)(?:-[0-9A-Za-z-]+(?:[.][0-9A-Za-z-]+)*)?$")
SHA256 = re.compile(r"^[0-9a-f]{64}$")
ROLES = {"controller", "host-environment"}
SYSTEMS = {"x86_64-linux", "aarch64-linux"}
PROJECT_COMPONENTS = [
    "bin/sub2api-deploy", "bin/go", "bin/pulumi-program", "bin/pulumi-resource-sub2api-host", "go.mod",
    "scripts/pulumi-plugins/cloudflare/pulumi-plugin.json",
    "scripts/pulumi-plugins/upstash/pulumi-plugin.json",
    "artifacts/sub2api-host/manifest.json", "artifacts/sub2api-host/sub2api-host-linux-amd64",
    "artifacts/sub2api-host/sub2api-host-linux-arm64",
]
DESCRIPTOR_FILES = [
    "flake.nix",
    "flake.lock",
    "nix/environments.nix",
    "nix/runtime-lib.nix",
    "nix/runtime-contract.json",
    "nix/controller-workspace-init.sh",
    "nix/host-activate.sh",
]


def fail(message):
    raise SystemExit("nix release descriptor: " + message)


def read_json(path, what):
    try:
        value = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as error:
        fail("invalid %s: %s" % (what, error))
    if not isinstance(value, dict):
        fail("%s must be a JSON object" % what)
    return value


def sri(raw_digest):
    return "sha256-" + base64.b64encode(bytes.fromhex(raw_digest)).decode("ascii")


def same_sha256(left, right):
    return hashlib.sha256(left.read_bytes()).digest() == hashlib.sha256(right.read_bytes()).digest()


def safe_member(member):
    path = PurePosixPath(member.name)
    if path.is_absolute() or not member.name or "\\" in member.name or ".." in path.parts or "." in path.parts:
        fail("unsafe archive member: %r" % member.name)
    if not (member.isfile() or member.isdir()):
        fail("archive member is not a regular file or directory: %r" % member.name)


def extract_archive(archive, destination):
    try:
        with tarfile.open(archive, "r:*") as bundle:
            members = bundle.getmembers()
            names = set()
            for member in members:
                safe_member(member)
                if member.name in names:
                    fail("duplicate archive member: %r" % member.name)
                names.add(member.name)
            roots = {PurePosixPath(member.name).parts[0] for member in members}
            if len(roots) != 1:
                fail("archive must contain exactly one immutable payload root")
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
                # NAR records only the executable bit, not all Unix mode bits.
                target.chmod(0o755 if member.mode & 0o111 else 0o644)
    except (OSError, tarfile.TarError) as error:
        fail("cannot safely extract %s: %s" % (archive.name, error))
    return destination / next(iter(roots))


def checked_required_paths(value, what):
    if not isinstance(value, list) or not value:
        fail("%s requiredPaths must be a nonempty list" % what)
    paths = []
    for path in value:
        relative = PurePosixPath(path) if isinstance(path, str) else None
        if relative is None or relative.is_absolute() or ".." in relative.parts or "." in relative.parts or "\\" in path:
            fail("%s requiredPaths has an unsafe path" % what)
        paths.append(path)
    if len(paths) != len(set(paths)):
        fail("%s requiredPaths has duplicate paths" % what)
    return paths


def required_paths_for(role):
    contract = read_json(ROOT / "nix/runtime-contract.json", "runtime contract")
    paths = contract.get("requiredPaths") if contract.get("schemaVersion") == 1 else None
    if not isinstance(paths, dict) or role not in paths:
        fail("runtime contract has no role: %s" % role)
    return checked_required_paths(paths[role], "runtime contract")


def supported_systems_for(role):
    contract = read_json(ROOT / "nix/runtime-contract.json", "runtime contract")
    systems = contract.get("supportedSystems") if contract.get("schemaVersion") == 1 else None
    values = systems.get(role) if isinstance(systems, dict) else None
    if not isinstance(values, list) or not values or any(system not in SYSTEMS for system in values) or len(values) != len(set(values)):
        fail("runtime contract has invalid supported systems for role: %s" % role)
    return values


def executable_paths_for(role):
    contract = read_json(ROOT / "nix/runtime-contract.json", "runtime contract")
    paths = contract.get("executablePaths") if contract.get("schemaVersion") == 1 else None
    values = checked_required_paths(paths.get(role), "runtime contract executable") if isinstance(paths, dict) else None
    if values is None or not set(values).issubset(required_paths_for(role)):
        fail("runtime contract has invalid executable paths for role: %s" % role)
    return values


def checked_inventory(payload, entry, project_sha, tag):
    path = payload / "share/sub2api-runtime/inventory.json"
    if not path.is_file() or path.is_symlink():
        fail("payload inventory is missing or not a regular file")
    inventory = read_json(path, "payload inventory")
    for name, expected in (("schemaVersion", 1), ("role", entry["role"]), ("system", entry["system"]), ("version", tag), ("projectCommit", project_sha)):
        if inventory.get(name) != expected:
            fail("payload inventory %s does not match audited artifact" % name)
    if "archiveSha256" in inventory or "narHash" in inventory:
        fail("payload inventory must not contain its enclosing archive or NAR hash")
    files = inventory.get("files")
    if not isinstance(files, list) or not files:
        fail("payload inventory files must be a nonempty list")
    seen = set()
    for record in files:
        if not isinstance(record, dict) or not isinstance(record.get("path"), str) or not SHA256.fullmatch(record.get("sha256", "")):
            fail("payload inventory has an invalid file record")
        relative = PurePosixPath(record["path"])
        if relative.is_absolute() or ".." in relative.parts or "." in relative.parts or "\\" in record["path"] or record["path"] in seen:
            fail("payload inventory has an unsafe or duplicate path")
        seen.add(record["path"])
        file_path = payload.joinpath(*relative.parts)
        if not file_path.is_file() or file_path.is_symlink():
            fail("inventory file is missing or not regular: %s" % record["path"])
        if hashlib.sha256(file_path.read_bytes()).hexdigest() != record["sha256"]:
            fail("inventory digest mismatch: %s" % record["path"])
    actual = {item.relative_to(payload).as_posix() for item in payload.rglob("*") if item.is_file() and not item.is_symlink()}
    if actual - {"share/sub2api-runtime/inventory.json"} != seen:
        fail("payload inventory must cover every regular payload file except itself")
    inventory_paths = inventory.get("requiredPaths")
    manifest_paths = entry.get("requiredPaths")
    if inventory_paths is not None and manifest_paths is not None and inventory_paths != manifest_paths:
        fail("payload inventory and audited manifest disagree on requiredPaths")
    required_paths = checked_required_paths(
        manifest_paths if manifest_paths is not None else inventory_paths,
        "audited manifest" if manifest_paths is not None else "payload inventory",
    )
    if required_paths != required_paths_for(entry["role"]):
        fail("requiredPaths does not match the role runtime contract")
    executable_paths = set(executable_paths_for(entry["role"]))
    for required in required_paths:
        required_path = payload.joinpath(*PurePosixPath(required).parts)
        if required.endswith("/plugins"):
            if not required_path.is_dir() or required_path.is_symlink() or not any(required_path.rglob("*")):
                fail("required plugin directory is missing, unsafe, or empty")
        elif not required_path.is_file() or required_path.is_symlink():
            fail("required runtime file is missing or unsafe: %s" % required)
        elif required in executable_paths and not required_path.stat().st_mode & 0o111:
            fail("required runtime entrypoint is not executable: %s" % required)
    plugin_root = payload / "workspace/bin/plugins"
    if entry["role"] == "controller":
        plugin_files = [path for path in plugin_root.rglob("*") if path.is_file() and not path.is_symlink()]
        if not plugin_files or any(not path.stat().st_mode & 0o111 for path in plugin_files):
            fail("controller plugin payload must contain executable regular files")
    return hashlib.sha256(path.read_bytes()).hexdigest(), required_paths


def verify_project_components(payload, candidate):
    for relative in PROJECT_COMPONENTS:
        runtime_file = payload / relative
        candidate_file = candidate / relative
        if not runtime_file.is_file() or runtime_file.is_symlink() or not candidate_file.is_file() or candidate_file.is_symlink():
            fail("runtime or CI candidate project component is missing: %s" % relative)
        if not same_sha256(runtime_file, candidate_file):
            fail("runtime project component differs from the tested CI candidate: %s" % relative)
    workspace_program = payload / "workspace/bin/pulumi-program"
    if not same_sha256(workspace_program, candidate / "bin/pulumi-program"):
        fail("runtime workspace Program differs from the tested CI candidate")
    if not same_sha256(payload / "workspace/Pulumi.yaml", candidate / "Pulumi.yaml"):
        fail("runtime workspace Pulumi.yaml differs from the tested CI candidate")


def nar_hash(command, payload):
    try:
        result = subprocess.run([command, str(payload)], check=True, text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    except OSError as error:
        fail("cannot execute fixed NAR hash command: %s" % error)
    except subprocess.CalledProcessError as error:
        fail("fixed NAR hash command failed: %s" % error.stderr.strip())
    value = result.stdout.strip()
    if not re.fullmatch(r"sha256-[A-Za-z0-9+/]{43}=", value):
        fail("fixed NAR hash command did not return one SRI SHA-256 hash")
    return value


def generate(args):
    if not TAG.fullmatch(args.tag) or not COMMIT.fullmatch(args.project_sha):
        fail("tag and project SHA must be immutable release identities")
    source = Path(args.input_dir).resolve()
    manifest = read_json(Path(args.manifest), "audited manifest")
    if manifest.get("schemaVersion") != 1 or manifest.get("projectCommit") != args.project_sha:
        fail("audited manifest identity does not match project SHA")
    candidates = manifest.get("knownCandidateSHA256")
    artifacts = manifest.get("artifacts")
    if not isinstance(candidates, dict) or not isinstance(artifacts, list) or not artifacts:
        fail("audited manifest requires knownCandidateSHA256 mapping and artifacts")
    expected = {(role, system) for role in ROLES for system in supported_systems_for(role)}
    if args.require_complete and {(item.get("role"), item.get("system")) for item in artifacts if isinstance(item, dict)} != expected:
        fail("a complete runtime descriptor requires every role/system pair in the runtime contract")
    output = Path(args.output_dir).resolve()
    if output.exists():
        fail("output directory already exists")
    output.mkdir(parents=True)
    candidate_record = manifest.get("ciCandidate")
    candidate_archive = Path(args.candidate_archive).resolve()
    if not isinstance(candidate_record, dict) or candidate_record.get("projectCommit") != args.project_sha:
        fail("audited manifest has no matching CI candidate identity")
    if not candidate_archive.is_file() or candidate_archive.is_symlink() or candidate_record.get("archive") != candidate_archive.name:
        fail("tested CI candidate archive is missing or mismatched")
    candidate_sha = hashlib.sha256(candidate_archive.read_bytes()).hexdigest()
    if candidate_record.get("archiveSha256") != candidate_sha:
        fail("tested CI candidate archive bytes do not match the audited manifest")
    locked = []
    identities = set()
    with tempfile.TemporaryDirectory() as candidate_temporary:
        candidate_payload = extract_archive(candidate_archive, Path(candidate_temporary))
        for entry in artifacts:
            if not isinstance(entry, dict):
                fail("artifact entry must be an object")
            role, system, asset = entry.get("role"), entry.get("system"), entry.get("asset")
            if role not in ROLES or system not in supported_systems_for(role) or not isinstance(asset, str) or PurePosixPath(asset).name != asset:
                fail("artifact role, system, or asset is invalid")
            if entry.get("version") != args.tag or entry.get("projectCommit") != args.project_sha:
                fail("artifact identity must match tag and project SHA")
            if (role, system) in identities:
                fail("duplicate role/system artifact")
            identities.add((role, system))
            candidate = candidates.get(asset)
            archive = source / asset
            if not isinstance(candidate, str) or not SHA256.fullmatch(candidate) or not archive.is_file() or archive.is_symlink():
                fail("audited candidate asset is missing or invalid: %s" % asset)
            raw_sha = hashlib.sha256(archive.read_bytes()).hexdigest()
            if raw_sha != candidate:
                fail("candidate bytes do not match audited SHA256: %s" % asset)
            with tempfile.TemporaryDirectory() as temporary:
                payload = extract_archive(archive, Path(temporary))
                inventory_sha, required_paths = checked_inventory(payload, entry, args.project_sha, args.tag)
                if role == "controller":
                    verify_project_components(payload, candidate_payload)
                tree_hash = nar_hash(args.nar_command, payload)
            locked.append({"role": role, "system": system, "version": args.tag, "asset": asset, "archiveSha256": sri(raw_sha), "narHash": tree_hash, "inventorySha256": sri(inventory_sha), "projectCommit": args.project_sha, "requiredPaths": required_paths})
    descriptor = output / "descriptor"
    for relative in DESCRIPTOR_FILES:
        source_file = ROOT / relative
        if not source_file.is_file():
            fail("descriptor allowlist source is missing: %s" % relative)
        target = descriptor / relative
        target.parent.mkdir(parents=True, exist_ok=True)
        shutil.copy2(source_file, target)
    (descriptor / "nix/runtime-release.json").write_text(json.dumps({"schemaVersion": 1, "artifacts": locked}, indent=2) + "\n")
    shutil.copy2(descriptor / "nix/runtime-release.json", output / "runtime-release.json")


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    command = parser.add_subparsers(dest="command", required=True).add_parser("generate")
    command.add_argument("--input-dir", required=True)
    command.add_argument("--manifest", required=True)
    command.add_argument("--candidate-archive", required=True)
    command.add_argument("--tag", required=True)
    command.add_argument("--project-sha", required=True)
    command.add_argument("--output-dir", required=True)
    command.add_argument("--nar-command", required=True, help="fixed Nix CLI wrapper: <command> <unpacked-payload>")
    command.add_argument("--require-complete", action="store_true")
    args = parser.parse_args(argv)
    generate(args)


if __name__ == "__main__":
    main()
