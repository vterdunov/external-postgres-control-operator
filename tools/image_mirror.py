"""Preserve pinned multi-platform indexes in immutable EPCO mirror tags."""

import argparse
import json
from pathlib import Path
import re
import subprocess
import sys


DEFAULT_CATALOG = Path(__file__).resolve().parents[1] / 'test/images/catalog.json'
REPOSITORY = 'ghcr.io/vterdunov/external-postgres-control-operator'
DIGEST = re.compile(r'sha256:[a-f0-9]{64}')
SOURCE = re.compile(
    r'(?:docker\.io|gcr\.io)/[a-z0-9]+(?:[._-][a-z0-9]+)*'
    r'(?:/[a-z0-9]+(?:[._-][a-z0-9]+)*)+:[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}'
    r'@sha256:[a-f0-9]{64}')
DESTINATION = re.compile(re.escape(REPOSITORY) + r':mirror-[a-z0-9][a-z0-9_.-]{0,120}')
HTTP_METHODS = {'GET', 'HEAD', 'POST', 'PUT', 'PATCH', 'DELETE', 'OPTIONS', 'TRACE', 'CONNECT'}


def require(condition, message):
    if not condition:
        raise ValueError(message)


def unique_fields(pairs):
    result = {}
    for key, value in pairs:
        require(key not in result, 'duplicate catalog field: ' + key)
        result[key] = value
    return result


def read_catalog(path):
    catalog = json.loads(Path(path).read_text(encoding='utf-8'), object_pairs_hook=unique_fields)
    require(isinstance(catalog, dict) and set(catalog) == {'images'}, 'invalid image catalog fields')
    entries = catalog['images']
    require(isinstance(entries, list) and entries, 'image catalog must contain images')
    names, sources, destinations = set(), set(), set()
    for entry in entries:
        require(isinstance(entry, dict) and set(entry) == {'name', 'source', 'destination'},
                'invalid image entry fields')
        require(all(isinstance(value, str) for value in entry.values()), 'image entry fields must be strings')
        name, source, destination = entry['name'], entry['source'], entry['destination']
        require(re.fullmatch(r'[a-z0-9]+(?:-[a-z0-9]+)*', name), 'invalid image name')
        require(SOURCE.fullmatch(source), 'source must pin an approved upstream image digest')
        require(DESTINATION.fullmatch(destination), 'destination must use the EPCO mirror tag namespace')
        require(name not in names and source not in sources and destination not in destinations,
                'duplicate image name, source or destination')
        names.add(name)
        sources.add(source)
        destinations.add(destination)
    return entries


def reference(name, catalog=DEFAULT_CATALOG):
    for entry in read_catalog(catalog):
        if entry['name'] == name:
            return entry['destination'] + '@' + entry['source'].rsplit('@', 1)[1]
    raise ValueError('unknown mirrored image: ' + name)


def crane(*arguments, allow_absent=False, timeout=120):
    result = subprocess.run(['crane', *arguments], capture_output=True, text=True, timeout=timeout)
    if result.returncode:
        # Crane logs HTTP method prefixes when HEAD falls back to GET. Those
        # describe requests, while registry error codes determine absence.
        codes = set(re.findall(r'\b([A-Z][A-Z_]+):', result.stderr)) - HTTP_METHODS
        if allow_absent and codes and codes <= {'MANIFEST_UNKNOWN', 'NAME_UNKNOWN'}:
            return None
        raise RuntimeError('crane ' + arguments[0] + ' failed: ' + result.stderr.strip())
    return result.stdout.strip()


def registry_digest(image, *, allow_absent=False):
    value = crane('digest', image, allow_absent=allow_absent)
    require(value is None or DIGEST.fullmatch(value), 'registry returned an invalid image digest')
    return value


def sync(catalog=DEFAULT_CATALOG):
    for entry in read_catalog(catalog):
        source, destination = entry['source'], entry['destination']
        expected = source.rsplit('@', 1)[1]
        require(registry_digest(source) == expected, 'pinned source digest mismatch: ' + entry['name'])
        current = registry_digest(destination, allow_absent=True)
        if current is not None:
            require(current == expected, 'immutable mirror tag digest mismatch: ' + entry['name'])
        else:
            # Copy defaults to all platforms. No-clobber rechecks before copying;
            # workflow concurrency serializes writers managed by this repository.
            crane('copy', '--no-clobber', source, destination, timeout=900)
            require(registry_digest(destination) == expected, 'copied index digest mismatch: ' + entry['name'])
        print(entry['name'] + ': ' + destination + '@' + expected)


def main():
    parser = argparse.ArgumentParser()
    commands = parser.add_subparsers(dest="command", required=True)
    synchronize = commands.add_parser("sync")
    synchronize.add_argument("--catalog", type=Path, default=DEFAULT_CATALOG)
    resolve = commands.add_parser("reference")
    resolve.add_argument("name")
    resolve.add_argument("--catalog", type=Path, default=DEFAULT_CATALOG)
    args = parser.parse_args()
    try:
        if args.command == 'sync':
            sync(args.catalog)
        else:
            print(reference(args.name, args.catalog))
    except (OSError, ValueError, RuntimeError, subprocess.SubprocessError) as error:
        print('Image mirror: ' + str(error), file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
