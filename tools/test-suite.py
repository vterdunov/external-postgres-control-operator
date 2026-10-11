"""Hydrate an immutable private test overlay into a clean disposable checkout."""

import argparse
import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import re
import signal
import stat
import subprocess
import sys


class Refused(Exception):
    pass


def git(root, *args):
    return subprocess.check_output(["git", "-C", str(root), *args], stderr=subprocess.DEVNULL).decode().strip()


def verify_checkout(root):
    tree = subprocess.check_output(["git", "-C", str(root), "ls-tree", "-rz", "HEAD"], stderr=subprocess.DEVNULL)
    for record in filter(None, tree.split(b"\0")):
        metadata, name = record.split(b"\t", 1)
        mode, kind, digest = metadata.split()
        path = root / os.fsdecode(name)
        no_links(root, path)
        if mode not in (b"100644", b"100755") or kind != b"blob":
            raise Refused()
        regular(path)
        data = path.read_bytes()
        actual = hashlib.sha1(b"blob " + str(len(data)).encode() + b"\0" + data).hexdigest().encode()
        expected_mode = 0o755 if mode == b"100755" else 0o644
        if actual != digest or stat.S_IMODE(path.stat().st_mode) != expected_mode:
            raise Refused()


def regular(path):
    if not stat.S_ISREG(path.lstat().st_mode):
        raise Refused()


def no_links(root, path):
    current = root
    for part in path.relative_to(root).parts:
        current /= part
        if current.is_symlink():
            raise Refused()


def load_json(path):
    regular(path)
    def unique(pairs):
        value = {}
        for key, item in pairs:
            if key in value:
                raise Refused()
            value[key] = item
        return value
    return json.loads(path.read_text(), object_pairs_hook=unique)


def hydrate(source, suite, reference):
    source, suite = source.resolve(strict=True), suite.resolve(strict=True)
    if source == suite or source.is_relative_to(suite) or suite.is_relative_to(source):
        raise Refused()
    if not re.fullmatch(r"[0-9a-f]{40}", reference):
        raise Refused()
    if git(source, "status", "--porcelain", "--untracked-files=all") or git(suite, "status", "--porcelain", "--untracked-files=all"):
        raise Refused()
    tracked = set(git(source, "ls-tree", "-r", "--name-only", "HEAD").splitlines())
    for path in source.rglob("*"):
        relative = path.relative_to(source)
        if relative.parts[0] == ".git":
            continue
        if "__pycache__" in relative.parts or ((path.is_file() or path.is_symlink()) and (relative.parts[0] == "test" or path.name.endswith("_test.go")) and relative.as_posix() not in tracked):
            raise Refused()
    verify_checkout(source)
    verify_checkout(suite)
    lock = load_json(source / "test-suite.lock.json")
    if set(lock) != {"version", "repository", "revision"} or type(lock["version"]) is not int or lock["version"] != 1:
        raise Refused()
    if not re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", lock["repository"]):
        raise Refused()
    if lock["revision"] != reference or git(suite, "rev-parse", "HEAD") != reference:
        raise Refused()
    manifest = load_json(suite / "manifest.json")
    if set(manifest) != {"version", "files"} or type(manifest["version"]) is not int or manifest["version"] != 1 or not isinstance(manifest["files"], list) or not manifest["files"]:
        raise Refused()
    overlay = suite / "overlay"
    if overlay.is_symlink() or not overlay.is_dir():
        raise Refused()
    files = {}
    for entry in manifest["files"]:
        if not isinstance(entry, dict) or set(entry) != {"path", "sha256", "mode"}:
            raise Refused()
        name = entry["path"]
        if not isinstance(name, str) or not name or "\\" in name or "\x00" in name:
            raise Refused()
        path = PurePosixPath(name)
        if path.is_absolute() or path.as_posix() != name or any(p in (".", "..") or p.startswith(".") for p in path.parts):
            raise Refused()
        if not (path.parts[0] == "test" and len(path.parts) > 1 or path.name.endswith("_test.go")):
            raise Refused()
        if name in files or entry["mode"] not in ("100644", "100755") or not re.fullmatch(r"[0-9a-f]{64}", entry["sha256"]):
            raise Refused()
        origin = overlay / name
        no_links(overlay, origin)
        regular(origin)
        mode = 0o755 if entry["mode"] == "100755" else 0o644
        if stat.S_IMODE(origin.stat().st_mode) != mode:
            raise Refused()
        data = origin.read_bytes()
        if hashlib.sha256(data).hexdigest() != entry["sha256"]:
            raise Refused()
        target = source / name
        no_links(source, target)
        if target.exists():
            raise Refused()
        for parent in target.parents:
            if parent == source:
                break
            if parent.exists() and not parent.is_dir():
                raise Refused()
        files[name] = (data, mode)
    actual = set()
    for path in overlay.rglob("*"):
        if path.is_symlink() or not (path.is_dir() or path.is_file()):
            raise Refused()
        if path.is_file():
            actual.add(path.relative_to(overlay).as_posix())
    if actual != set(files):
        raise Refused()
    for name in files:
        if any(parent.as_posix() in files for parent in PurePosixPath(name).parents):
            raise Refused()
    # Directory descriptors and exclusive creation prevent following swapped links
    # or overwriting a file created concurrently. Keep descriptors for rollback.
    descriptors = []
    created_files, created_dirs = [], []
    flags = os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW
    root_fd = os.open(source, flags)
    descriptors.append(root_fd)
    directories = {(): root_fd}
    def terminate(signum, frame):
        raise KeyboardInterrupt()
    previous_term = signal.signal(signal.SIGTERM, terminate)
    try:
        for name, (data, mode) in files.items():
            directory = root_fd
            prefix = ()
            parts = PurePosixPath(name).parts
            for part in parts[:-1]:
                prefix += (part,)
                if prefix in directories:
                    directory = directories[prefix]
                    continue
                try:
                    os.mkdir(part, mode=0o755, dir_fd=directory)
                    created_dirs.append((directory, part))
                except FileExistsError:
                    pass
                directory = os.open(part, flags, dir_fd=directory)
                descriptors.append(directory)
                directories[prefix] = directory
            fd = os.open(parts[-1], os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, mode, dir_fd=directory)
            created_files.append((directory, parts[-1]))
            with os.fdopen(fd, "wb") as output:
                output.write(data)
                output.flush()
                os.fchmod(output.fileno(), mode)
                os.fsync(output.fileno())
    except BaseException:
        for directory, name in reversed(created_files):
            os.unlink(name, dir_fd=directory)
        for directory, name in reversed(created_dirs):
            os.rmdir(name, dir_fd=directory)
        raise
    finally:
        signal.signal(signal.SIGTERM, previous_term)
        for fd in reversed(descriptors):
            os.close(fd)
    print(f"Private suite hydrated: source={git(source, 'rev-parse', 'HEAD')} suite={reference}")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    commands.add_parser("require")
    command = commands.add_parser("hydrate")
    command.add_argument("--source", type=Path, required=True)
    command.add_argument("--suite", type=Path, required=True)
    command.add_argument("--ref", required=True)
    args = parser.parse_args()
    try:
        if args.command == "require":
            root = Path(__file__).resolve().parents[1]
            required = ("test/envtest/run.sh", "test/integration/run.sh", "test/e2e/run.sh", "test/architecture/policy_test.py", "test/runners/run.sh")
            if not all((root / path).is_file() for path in required):
                print("Private tests are absent. Hydrate the pinned private suite first.", file=sys.stderr)
                return 1
            return 0
        hydrate(args.source, args.suite, args.ref)
    except (Exception, KeyboardInterrupt):
        print("Private suite hydration refused", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
