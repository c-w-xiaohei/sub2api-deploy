#!/usr/bin/env python3
"""Measure one fixed Go compile/link target inside a cgroup-v2 child."""

from __future__ import annotations

import argparse
import json
import math
import os
from pathlib import Path
import re
import signal
import shutil
import subprocess
import sys
import tempfile
import time


BASELINE_SHA = "46be3e20299b1c2e43251a52e48a942248d6199b"
SCHEMA = "pulumi-resource-comparison-v1"
TOOLCHAIN = "go1.25.11"
ARCHITECTURE = "amd64"
CGROUP_ROOT = Path("/sys/fs/cgroup")
SHA_PATTERN = re.compile(r"^[0-9a-f]{40}$")

TARGETS = {
    "cli": ("build", "./cmd/sub2api-deploy"),
    "engine": ("test", "./internal/integration/enginegraph"),
    "import": ("test", "./internal/integration/providerimport"),
}


class MeasurementUnavailable(Exception):
    """The required cgroup-v2 measurement boundary is not available."""


def kill_process_group(process: subprocess.Popen[bytes]) -> None:
    try:
        os.killpg(process.pid, signal.SIGKILL)
    except ProcessLookupError:
        pass


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(add_help=True)
    parser.add_argument("--label", choices=sorted(TARGETS), required=True)
    parser.add_argument(
        "--revision", choices=("baseline", "candidate"), required=True
    )
    parser.add_argument("--output", type=Path, required=True)
    return parser.parse_args()


def append_private_error(log_path: Path, argv: list[str], input_text: str = "") -> None:
    """Run a fixed privileged helper while keeping its diagnostics private."""
    with log_path.open("ab") as log:
        result = subprocess.run(
            ["sudo", "-n", *argv],
            input=input_text if input_text else None,
            text=bool(input_text),
            stdout=subprocess.DEVNULL,
            stderr=log,
            check=False,
        )
    if result.returncode != 0:
        raise MeasurementUnavailable


def read_private_text(path: Path) -> str:
    try:
        return path.read_text(encoding="ascii").strip()
    except (OSError, UnicodeError) as error:
        raise MeasurementUnavailable from error


def remove_cgroup(path: Path, log_path: Path) -> bool:
    try:
        append_private_error(log_path, ["rmdir", str(path)])
    except MeasurementUnavailable:
        return False
    return not path.exists()


def create_measurement_cgroup(log_path: Path) -> Path:
    if not CGROUP_ROOT.is_dir():
        raise MeasurementUnavailable

    try:
        controllers = read_private_text(CGROUP_ROOT / "cgroup.controllers").split()
        subtree_control = read_private_text(CGROUP_ROOT / "cgroup.subtree_control").split()
    except MeasurementUnavailable:
        raise

    if "memory" not in controllers:
        raise MeasurementUnavailable
    if "memory" not in subtree_control:
        try:
            append_private_error(
                log_path,
                ["tee", str(CGROUP_ROOT / "cgroup.subtree_control")],
                "+memory\n",
            )
            subtree_control = read_private_text(
                CGROUP_ROOT / "cgroup.subtree_control"
            ).split()
        except MeasurementUnavailable:
            raise
        if "memory" not in subtree_control:
            raise MeasurementUnavailable

    cgroup_path = CGROUP_ROOT / f"sub2api-resource-{os.getpid()}"
    if cgroup_path.exists():
        raise MeasurementUnavailable

    try:
        append_private_error(log_path, ["mkdir", "-m", "0700", str(cgroup_path)])
        append_private_error(
            log_path,
            [
                "chown",
                "-R",
                f"{os.getuid()}:{os.getgid()}",
                str(cgroup_path),
            ],
        )
        if not (cgroup_path / "memory.peak").is_file():
            raise MeasurementUnavailable
        read_private_text(cgroup_path / "memory.peak")
    except MeasurementUnavailable:
        remove_cgroup(cgroup_path, log_path)
        raise
    return cgroup_path


def release_child_into_cgroup(
    cgroup_path: Path,
    command: list[str],
    environment: dict[str, str],
    log_path: Path,
) -> tuple[int, float, int]:
    release_read, release_write = os.pipe()

    with log_path.open("ab") as log:
        try:
            process = subprocess.Popen(
                [
                    "/bin/sh",
                    "-c",
                    f'read -r release <&{release_read}\nexec "$@"',
                    "measure-child",
                    *command,
                ],
                cwd=Path.cwd(),
                env=environment,
                stdin=subprocess.DEVNULL,
                stdout=log,
                stderr=log,
                close_fds=True,
                pass_fds=(release_read,),
                start_new_session=True,
            )
        except BaseException:
            os.close(release_read)
            os.close(release_write)
            raise MeasurementUnavailable

        try:
            append_private_error(
                log_path,
                ["tee", str(cgroup_path / "cgroup.procs")],
                f"{process.pid}\n",
            )
        except MeasurementUnavailable:
            kill_process_group(process)
            process.wait()
            os.close(release_read)
            os.close(release_write)
            raise

        os.close(release_read)
        started = time.monotonic_ns()
        try:
            os.write(release_write, b"1\n")
        except OSError as error:
            kill_process_group(process)
            process.wait()
            raise MeasurementUnavailable from error
        finally:
            os.close(release_write)
        try:
            return_code = process.wait(timeout=1100)
        except BaseException as error:
            kill_process_group(process)
            process.wait()
            raise MeasurementUnavailable from error

    elapsed = (time.monotonic_ns() - started) / 1_000_000_000
    try:
        peak_text = read_private_text(cgroup_path / "memory.peak")
        peak_bytes = int(peak_text)
    except (MeasurementUnavailable, ValueError) as error:
        raise MeasurementUnavailable from error
    if peak_bytes < 0 or elapsed < 0:
        raise MeasurementUnavailable
    return return_code, elapsed, peak_bytes


def source_sha(log_path: Path) -> str:
    try:
        with log_path.open("ab") as log:
            result = subprocess.run(
                ["git", "rev-parse", "HEAD"],
                cwd=Path.cwd(),
                stdin=subprocess.DEVNULL,
                stdout=subprocess.PIPE,
                stderr=log,
                text=True,
                check=False,
            )
        value = result.stdout.strip()
    except OSError as error:
        raise MeasurementUnavailable from error
    if result.returncode != 0 or not SHA_PATTERN.fullmatch(value):
        raise MeasurementUnavailable
    return value


def toolchain_and_target(log_path: Path) -> tuple[str, str, str]:
    try:
        with log_path.open("ab") as log:
            result = subprocess.run(
                ["go", "env", "GOVERSION", "GOOS", "GOARCH"],
                cwd=Path.cwd(),
                stdin=subprocess.DEVNULL,
                stdout=subprocess.PIPE,
                stderr=log,
                text=True,
                check=False,
            )
        values = result.stdout.splitlines()
    except OSError as error:
        raise MeasurementUnavailable from error
    if result.returncode != 0 or values != [TOOLCHAIN, "linux", ARCHITECTURE]:
        raise MeasurementUnavailable
    return values[0], values[1], values[2]


def fixed_record(
    args: argparse.Namespace,
    source: str | None,
    status: str,
    command_exit: int | None = None,
    wall_seconds: float | None = None,
    peak_bytes: int | None = None,
) -> dict[str, object]:
    return {
        "architecture": ARCHITECTURE,
        "cgo_enabled": False,
        "command_exit": command_exit,
        "gomaxprocs": 2,
        "gocache": "private-cold",
        "gomodcache": "shared-predownloaded",
        "goos": "linux",
        "label": args.label,
        "memory_peak_source": "cgroup-v2-memory.peak",
        "parallelism": 2,
        "revision": args.revision,
        "schema": SCHEMA,
        "source_sha": source,
        "status": status,
        "toolchain": TOOLCHAIN,
        "wall_seconds": wall_seconds,
        "cgroup_memory_peak_bytes": peak_bytes,
    }


def write_record(path: Path, record: dict[str, object]) -> None:
    path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    temporary = path.with_name(f".{path.name}.tmp-{os.getpid()}")
    temporary.write_text(json.dumps(record, sort_keys=True) + "\n", encoding="ascii")
    os.chmod(temporary, 0o600)
    os.replace(temporary, path)


def measure(args: argparse.Namespace, private_dir: Path, log_path: Path) -> dict[str, object]:
    if os.environ.get("CI") != "true":
        raise MeasurementUnavailable
    source = source_sha(log_path)
    expected_source = (
        BASELINE_SHA if args.revision == "baseline" else os.environ.get("GITHUB_SHA")
    )
    if not expected_source or not SHA_PATTERN.fullmatch(expected_source):
        raise MeasurementUnavailable
    if source != expected_source:
        raise MeasurementUnavailable
    toolchain_and_target(log_path)

    gomodcache = os.environ.get("GOMODCACHE")
    if not gomodcache or not Path(gomodcache).is_dir():
        raise MeasurementUnavailable

    target_kind, package = TARGETS[args.label]
    output_binary = private_dir / f"{args.revision}-{args.label}.bin"
    gocache = private_dir / "gocache"
    gocache.mkdir(mode=0o700)
    environment = {
        "CGO_ENABLED": "0",
        "GOMAXPROCS": "2",
        "GOCACHE": str(gocache),
        "GOMODCACHE": gomodcache,
        "GOARCH": ARCHITECTURE,
        "GOENV": "off",
        "GOFLAGS": "",
        "GOOS": "linux",
        "GOPROXY": "off",
        "GOSUMDB": "off",
        "GOTOOLCHAIN": "local",
        "HOME": os.environ.get("HOME", ""),
        "PATH": os.environ.get("PATH", ""),
        "TMPDIR": str(private_dir / "tmp"),
    }
    (private_dir / "tmp").mkdir(mode=0o700)
    if target_kind == "build":
        command = [
            "go",
            "build",
            "-p=2",
            "-o",
            str(output_binary),
            package,
        ]
    else:
        command = [
            "go",
            "test",
            "-p=2",
            "-c",
            "-o",
            str(output_binary),
            package,
        ]

    # Each revision starts with the same host page-cache condition on this disposable CI VM.
    append_private_error(log_path, ["sync"])
    append_private_error(log_path, ["tee", "/proc/sys/vm/drop_caches"], "3\n")
    cgroup_path = create_measurement_cgroup(log_path)
    try:
        try:
            return_code, elapsed, peak_bytes = release_child_into_cgroup(
                cgroup_path, command, environment, log_path
            )
        except MeasurementUnavailable:
            raise
        return fixed_record(
            args,
            source,
            "measured" if return_code == 0 else "compile_failed",
            command_exit=return_code,
            wall_seconds=elapsed,
            peak_bytes=peak_bytes,
        )
    finally:
        if not remove_cgroup(cgroup_path, log_path):
            raise MeasurementUnavailable


def main() -> int:
    args = parse_args()
    output = args.output
    private_dir: Path | None = None
    if os.environ.get("CI") != "true":
        return 2
    source: str | None = None
    try:
        output.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
        private_dir = Path(tempfile.mkdtemp(prefix=".pulumi-resource-", dir=output.parent))
        os.chmod(private_dir, 0o700)
        log_path = private_dir / "diagnostic.log"
        try:
            record = measure(args, private_dir, log_path)
            write_record(output, record)
            return 0 if record["status"] == "measured" else 1
        except MeasurementUnavailable:
            try:
                source = source_sha(log_path)
            except MeasurementUnavailable:
                source = None
            write_record(output, fixed_record(args, source, "measurement_unavailable"))
            return 2
    except BaseException:
        # Do not expose command, environment, or tool diagnostics in the report/log.
        try:
            write_record(output, fixed_record(args, source, "measurement_unavailable"))
        except BaseException:
            return 2
        return 2
    finally:
        if private_dir is not None:
            shutil.rmtree(private_dir, ignore_errors=True)


if __name__ == "__main__":
    sys.exit(main())
