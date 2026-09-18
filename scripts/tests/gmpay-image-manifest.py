#!/usr/bin/env python3
"""Resolve and validate the exact manifest used by the GM Pay image test."""

import hashlib
import json
import re
import sys
from pathlib import Path


DIGEST = re.compile(r"sha256:[0-9a-f]{64}")
SECRET_VALUE = re.compile(
    r"((?:authorization|token|password|secret|api[_-]?key|private[_-]?key)\s*[=:]\s*[\"']?)([^,\s}\"']+)",
    re.IGNORECASE,
)
JSON_SECRET_VALUE = re.compile(
    r'(?P<prefix>"(?:authorization|token|password|secret|api[_-]?key|private[_-]?key)"\s*:\s*)'
    r'(?P<quote>["\']?)(?P<value>[^"\'\r\n]*)(?P=quote)',
    re.IGNORECASE,
)
BEARER_VALUE = re.compile(r"(Bearer\s+)(\S+)", re.IGNORECASE)


def valid_digest(value):
    if not isinstance(value, str) or DIGEST.fullmatch(value) is None:
        raise ValueError("invalid sha256 digest")
    return value


def load_document(path):
    raw = Path(path).read_bytes()
    try:
        document = json.loads(raw)
    except (UnicodeDecodeError, json.JSONDecodeError) as error:
        raise ValueError("invalid JSON manifest") from error
    if not isinstance(document, dict):
        raise ValueError("manifest must be a JSON object")
    return raw, document


def manifest_config(document):
    config = document.get("config")
    if not isinstance(config, dict):
        raise ValueError("manifest config is missing")
    return valid_digest(config.get("digest"))


def resolve(path, architecture):
    raw, document = load_document(path)
    manifests = document.get("manifests")
    if manifests is None:
        platform_digest = "sha256:" + hashlib.sha256(raw).hexdigest()
        config_digest = manifest_config(document)
        kind = "single"
    else:
        if not isinstance(manifests, list):
            raise ValueError("manifest list is invalid")
        matches = [
            item
            for item in manifests
            if isinstance(item, dict)
            and isinstance(item.get("platform"), dict)
            and item["platform"].get("os") == "linux"
            and item["platform"].get("architecture") == architecture
        ]
        if len(matches) != 1:
            raise ValueError("expected exactly one native platform manifest")
        platform_digest = valid_digest(matches[0].get("digest"))
        config_digest = None
        kind = "index"
    return {
        "platformDigest": platform_digest,
        "configDigest": config_digest,
        "kind": kind,
    }


def redact(text):
    def redact_json(match):
        quote = match.group("quote")
        return f'{match.group("prefix")}{quote}REDACTED{quote}'

    text = JSON_SECRET_VALUE.sub(redact_json, text)
    text = BEARER_VALUE.sub(r"\1REDACTED", text)
    return SECRET_VALUE.sub(r"\1REDACTED", text)


def main(argv):
    if len(argv) < 2:
        raise ValueError("usage: gmpay-image-manifest.py resolve|config|redact ...")
    mode = argv[1]
    if mode == "resolve" and len(argv) == 4:
        result = resolve(argv[2], argv[3])
        print(json.dumps(result, sort_keys=True))
        return
    if mode == "config" and len(argv) == 3:
        _, document = load_document(argv[2])
        print(json.dumps({"configDigest": manifest_config(document)}, sort_keys=True))
        return
    if mode == "redact" and len(argv) == 2:
        print(redact(sys.stdin.read()), end="")
        return
    raise ValueError("invalid helper arguments")


if __name__ == "__main__":
    try:
        main(sys.argv)
    except (OSError, ValueError) as error:
        print(str(error), file=sys.stderr)
        raise SystemExit(2)
