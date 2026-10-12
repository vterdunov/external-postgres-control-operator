#!/usr/bin/env python3
"""Check manager metadata and native startup without registry publication."""

import argparse
import json
import platform
import subprocess
import sys


def check(image, version, source):
    inspected = json.loads(subprocess.check_output(['docker', 'image', 'inspect', image], text=True))[0]
    expected_arch = {'x86_64': 'amd64', 'aarch64': 'arm64', 'arm64': 'arm64'}[platform.machine()]
    config = inspected['Config']
    labels = config.get('Labels', {})
    if (inspected['Architecture'] != expected_arch or inspected['Os'] != 'linux'
            or config['User'] not in ('65532', '65532:65532')
            or labels.get('org.opencontainers.image.source') != 'https://github.com/vterdunov/external-postgres-control-operator'
            or labels.get('org.opencontainers.image.revision') != source
            or labels.get('org.opencontainers.image.version') != version):
        raise ValueError('native platform, nonroot user or image metadata mismatch')
    result = subprocess.run(['docker', 'run', '--rm', '--network=none', image, '--help'],
                            capture_output=True, text=True, timeout=30, check=True)
    if '--leader-elect' not in result.stdout + result.stderr or '--health-probe-bind-address' not in result.stdout + result.stderr:
        raise ValueError('native manager startup did not expose expected manager flags')
    print(json.dumps({'architecture': expected_arch, 'image': image, 'version': version, 'source_sha': source, 'user': config['User'], 'startup': 'success'}))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--image', required=True)
    parser.add_argument('--version', required=True)
    parser.add_argument('--source', required=True)
    args = parser.parse_args()
    check(args.image, args.version, args.source)


if __name__ == '__main__':
    try:
        main()
    except (ValueError, KeyError, OSError, subprocess.CalledProcessError, subprocess.TimeoutExpired) as error:
        sys.exit(str(error))
