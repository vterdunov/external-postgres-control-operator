#!/usr/bin/env python3
"""Stage the owned migration script without editing application test sources."""

import argparse
import hashlib
import json
from pathlib import Path
import re
import sys


ANCHOR = b"func installedConnectionJob(namespace, output string) *batchv1.Job {\n\tconst script = `"
ORIGINAL_SCRIPT_SHA256 = "5fe3eff3c73931827108e92b005037df3433eb415757e5083e602f51bc9cd064"


def checksum(data):
    return hashlib.sha256(data).hexdigest()


def require(condition, message):
    if not condition:
        raise ValueError(message)


def stage(args):
    source = Path(args.source).resolve(strict=True)
    script = Path(args.script).read_bytes()
    original = source.read_bytes()
    require(original.count(ANCHOR) == 1, "expected one installed migration fixture")
    start = original.index(ANCHOR) + len(ANCHOR)
    end = original.find(b"`", start)
    require(end >= start and original[end:].startswith(b"`\n\treturn &batchv1.Job"),
            "unclosed or unsupported migration fixture")
    require(checksum(original[start:end]) == ORIGINAL_SCRIPT_SHA256,
            "unsupported original migration script profile")
    require(script and not any(value in script for value in (b"`", b"\x00", b"\r")),
            "invalid migration script literal")
    script.decode("utf-8")
    for marker in (b"Application connection established", b"Migration gate observed",
                   b"Migration completed", b"CREATE TABLE app.orders(value integer); INSERT INTO app.orders VALUES (42)"):
        require(marker in script, "migration script lost a required stage")
    for revision in (args.application_source, args.fixture_source):
        require(re.fullmatch(r"[a-f0-9]{40}", revision), "invalid source revision")

    output_argument = Path(args.output)
    require(not output_argument.is_symlink(), "overlay output must be owned")
    output = output_argument.resolve()
    backing = output / "database_connections_test.go"
    overlay = output / "go-overlay.json"
    record_argument = Path(args.record)
    require(not record_argument.is_symlink(), "overlay record must be owned")
    record = record_argument.resolve()
    require(source not in (backing, overlay, record) and record not in (backing, overlay),
            "overlay output collides with its inputs")
    require(not any(path.exists() or path.is_symlink() for path in (backing, overlay, record)),
            "overlay output already exists")
    prefix, suffix = original[:start], original[end:]
    compiled = prefix + script + suffix
    mapping = (json.dumps({"Replace": {str(source): str(backing)}}, sort_keys=True) + "\n").encode()
    inventory = {
        "application_source_sha": args.application_source,
        "fixture_source_sha": args.fixture_source,
        "original_test_sha256": checksum(original),
        "fixture_script_sha256": checksum(script),
        "compiled_test_sha256": checksum(compiled),
        "overlay_map_sha256": checksum(mapping),
        "preserved_prefix_sha256": checksum(prefix),
        "preserved_suffix_sha256": checksum(suffix),
    }
    output.mkdir(parents=True, exist_ok=True)
    record.parent.mkdir(parents=True, exist_ok=True)
    backing.write_bytes(compiled)
    overlay.write_bytes(mapping)
    record.write_text(json.dumps(inventory, indent=2, sort_keys=True) + "\n", encoding="utf-8")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for option in ("source", "script", "output", "record", "application-source", "fixture-source"):
        parser.add_argument("--" + option, required=True)
    try:
        stage(parser.parse_args())
    except (OSError, ValueError) as error:
        print("Migration fixture overlay rejected: " + str(error), file=sys.stderr)
        return 2
    return 0


if __name__ == "__main__":
    sys.exit(main())
