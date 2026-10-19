#!/usr/bin/env python3
"""Copy or verify authoritative generated inputs without a YAML dependency.

Development uses config/. Release assembly supplies the verified application
release's inputs and a temporary chart directory. Generated copies remain byte
identical to their sources; edit the generator or source, never the copies.
"""

import argparse
import copy
import hashlib
import json
from pathlib import Path
import re
import shutil
import subprocess
import sys
import tarfile
import tempfile


CRD_NAMES = {'externalpostgres.postgresql.epco.io', 'databaseroles.postgresql.epco.io', 'databases.postgresql.epco.io'}


def crd_fingerprint(spec):
    """Normalize only v1 API defaults verified in Kubernetes v0.37.0.

    See apiextensions/v1/defaults.go and types.go. Preserve schemas, version
    ordering, storage and conversion settings instead of guessing compatibility.
    """
    normalized = copy.deepcopy(spec)
    names = normalized['names']
    if not names.get('singular'):
        names['singular'] = names['kind'].lower()
    if not names.get('listKind'):
        names['listKind'] = names['kind'] + 'List'
    if normalized.get('conversion') is None:
        normalized['conversion'] = {'strategy': 'None'}
    if normalized.get('preserveUnknownFields') is False:
        normalized.pop('preserveUnknownFields')
    data = json.dumps(normalized, sort_keys=True, separators=(',', ':'), ensure_ascii=False)
    return 'sha256:' + hashlib.sha256(data.encode()).hexdigest()


def crd_objects(sources):
    """Use Helm's YAML parser in an isolated chart, not application templates."""
    with tempfile.TemporaryDirectory(prefix='epco-crd-inputs-') as temporary:
        chart = Path(temporary)
        (chart / 'inputs').mkdir()
        (chart / 'templates').mkdir()
        (chart / 'Chart.yaml').write_text('apiVersion: v2\nname: epco-crd-inputs\nversion: 0.0.0\n')
        paths = []
        for source in sources:
            shutil.copyfile(source, chart / 'inputs' / source.name)
            paths.append(json.dumps('inputs/' + source.name))
        template = ('{"apiVersion":"v1","kind":"List","items":['
                    '{{ range $index, $path := list ' + ' '.join(paths) + ' }}'
                    '{{ if $index }},{{ end }}{{ $.Files.Get $path | fromYaml | toJson }}'
                    '{{ end }}]}\n')
        (chart / 'templates/inputs.yaml').write_text(template)
        rendered = subprocess.check_output(['helm', 'template', 'epco-crd-inputs', str(chart)], text=True)
        result = json.loads(rendered[rendered.index('{'):])['items']
        if len(result) != 3 or any(obj.get('kind') != 'CustomResourceDefinition' for obj in result) or {
            obj.get('metadata', {}).get('name') for obj in result
        } != CRD_NAMES:
            raise ValueError('selected inputs must contain three CRD objects')
        return result


def read_cluster_object(context, resource, name):
    command = ['kubectl', '--context', context, 'get', resource, name, '--ignore-not-found', '-o', 'json']
    result = subprocess.run(command, capture_output=True, text=True, check=False)
    if result.returncode:
        raise ValueError(f'cannot inspect {resource} {name}: {result.stderr.strip()}')
    if not result.stdout.strip():
        return None
    obj = json.loads(result.stdout)
    if not isinstance(obj, dict) or obj.get('metadata', {}).get('name') != name:
        raise ValueError(f'invalid JSON object returned while inspecting {resource} {name}')
    return obj


def extract_package(package, destination):
    """Accept regular chart files and directories only; never follow links."""
    if not package.is_file():
        raise ValueError(f'chart package file does not exist: {package}')
    with tarfile.open(package, 'r:gz') as archive:
        members = archive.getmembers()
        names = set()
        total = 0
        for member in members:
            path = Path(member.name)
            if (path.is_absolute() or '..' in path.parts or not path.parts
                    or path.parts[0] != 'external-postgres-control-operator'
                    or member.name in names or not (member.isfile() or member.isdir())):
                raise ValueError(f'unsafe chart package archive entry: {member.name}')
            names.add(member.name)
            total += member.size
        if total > 100 * 1024 * 1024 or len(members) > 1000:
            raise ValueError('chart package archive exceeds size limits')
        archive.extractall(destination, filter='data')
    return destination / 'external-postgres-control-operator'


def install(args, parser):
    if not all((args.package, args.release, args.namespace, args.context)):
        parser.error('install requires --package, --release, --namespace and explicit --context')
    if not all(re.fullmatch(r'[a-z0-9]([-a-z0-9]*[a-z0-9])?', name) for name in (args.release, args.namespace)):
        parser.error('release and namespace must be valid DNS labels')
    package = args.package.resolve()
    values = []
    if args.image_reference:
        match = re.fullmatch(r'([A-Za-z0-9][A-Za-z0-9./:_-]*):([A-Za-z0-9_][A-Za-z0-9_.-]*)(?:@(sha256:[a-f0-9]{64}))?', args.image_reference)
        if not match:
            parser.error('image reference requires repository:tag with an optional sha256 digest')
        values = ['--set-string', 'image.repository=' + match[1], '--set-string', 'image.tag=' + match[2],
                  '--set-string', 'image.digest=' + (match[3] or '')]
    with tempfile.TemporaryDirectory(prefix='epco-chart-install-') as temporary:
        if not package.is_file():
            raise ValueError(f'chart package file does not exist: {package}')
        # Keep the bytes inspected and the bytes installed identical even if the
        # caller's download path changes while cluster preflights are running.
        snapshot = Path(temporary) / 'verified-chart.tgz'
        shutil.copyfile(package, snapshot)
        package = snapshot
        selected_values = []
        for index, source_values in enumerate(args.values):
            if not source_values.is_file():
                raise ValueError(f'values file does not exist: {source_values}')
            snapshot_values = Path(temporary) / f'values-{index}.yaml'
            shutil.copyfile(source_values, snapshot_values)
            selected_values.extend(['--values', str(snapshot_values)])
        values = selected_values + values
        chart = extract_package(package, Path(temporary))
        sources = sorted((chart / 'crds').glob('*.yaml'))
        candidates = crd_objects(sources)
        compatibility = json.loads((chart / 'crd-compatibility.json').read_text())
        if compatibility.get('apiVersion') != 1 or set(compatibility.get('crds', {})) != CRD_NAMES:
            raise ValueError('package schema compatibility metadata is invalid')
        for candidate in candidates:
            name = candidate['metadata']['name']
            proof = compatibility['crds'][name]
            fingerprint = crd_fingerprint(candidate['spec'])
            allowed = proof.get('compatibleFingerprints')
            if (proof.get('candidateFingerprint') != fingerprint or not isinstance(allowed, list)
                    or fingerprint not in allowed or not all(re.fullmatch(r'sha256:[a-f0-9]{64}', value) for value in allowed)):
                raise ValueError(f'package schema fingerprint metadata does not match {name}')
        # Retain the exact local render from the frozen package and values.
        rendered = Path(temporary) / 'rendered.yaml'
        with rendered.open('w') as stream:
            subprocess.run(['helm', 'template', args.release, str(package), '--namespace', args.namespace, *values],
                           check=True, stdout=stream, timeout=120)
        sentinel = read_cluster_object(args.context, 'clusterrole', 'epco-manager-role')
        if sentinel is not None:
            metadata = sentinel['metadata']
            annotations = metadata.get('annotations', {})
            if (metadata.get('labels', {}).get('app.kubernetes.io/managed-by') != 'Helm'
                    or annotations.get('meta.helm.sh/release-name') != args.release
                    or annotations.get('meta.helm.sh/release-namespace') != args.namespace):
                raise ValueError('manager ownership conflict: choose one delivery route and one Helm release per cluster')
        for candidate in candidates:
            name = candidate['metadata']['name']
            current = read_cluster_object(args.context, 'crd', name)
            if current is None:
                continue
            fingerprint = crd_fingerprint(current['spec'])
            if fingerprint not in compatibility['crds'][name]['compatibleFingerprints']:
                raise ValueError(f'unknown or incompatible retained schema fingerprint for CRD {name}')
            versions = {version['name'] for version in candidate['spec']['versions']}
            if not set(current.get('status', {}).get('storedVersions', [])).issubset(versions):
                raise ValueError(f'unsupported stored schema version for CRD {name}')
        # Let the target API validate complete resource semantics and admission
        # without persisting anything, before applying even the first CRD.
        subprocess.run(['kubectl', '--context', args.context, '--request-timeout=30s',
                        'apply', '--dry-run=server', '-f', str(rendered)],
                       check=True, stdout=subprocess.DEVNULL, timeout=120)
        # Kubernetes and Helm are not atomic. A failed write may leave compatible
        # CRDs applied, but the manager starts only after all CRDs are Established.
        for source in sources:
            subprocess.run(['kubectl', '--context', args.context, 'apply', '-f', str(source)], check=True)
        for candidate in candidates:
            subprocess.run(['kubectl', '--context', args.context, 'wait', '--for=condition=Established',
                            'crd/' + candidate['metadata']['name'], '--timeout=120s'], check=True)
        subprocess.run(['helm', 'upgrade', '--install', args.release, str(package), '--namespace', args.namespace,
                        '--kube-context', args.context, '--skip-crds', '--wait', '--timeout', '120s', *values], check=True)


def main():
    root = Path(__file__).resolve().parents[1]
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('action', choices=('sync', 'check', 'install'))
    parser.add_argument('--chart-dir', type=Path, default=root / 'charts/external-postgres-control-operator')
    parser.add_argument('--inputs-dir', type=Path, default=root / 'config')
    parser.add_argument('--package', type=Path)
    parser.add_argument('--release')
    parser.add_argument('--namespace')
    parser.add_argument('--context')
    parser.add_argument('--image-reference')
    parser.add_argument('--values', action='append', type=Path, default=[])
    args = parser.parse_args()
    if args.action == 'install':
        install(args, parser)
        return
    sources = sorted((args.inputs_dir / 'crd/bases').glob('*.yaml'))
    role = args.inputs_dir / 'rbac/role.yaml'
    if len(sources) != 3 or not role.is_file():
        parser.error('inputs require all three CRDs in crd/bases and rbac/role.yaml')
    expected = {Path('crds') / path.name: path.read_bytes() for path in sources}
    expected[Path('generated/manager-role.yaml')] = role.read_bytes()
    objects = crd_objects(sources)
    compatibility = {'apiVersion': 1, 'crds': {}}
    for obj in objects:
        fingerprint = crd_fingerprint(obj['spec'])
        compatibility['crds'][obj['metadata']['name']] = {
            'candidateFingerprint': fingerprint,
            'compatibleFingerprints': [fingerprint],
        }
    expected[Path('generated/crds.json')] = (json.dumps(objects, sort_keys=True, indent=2) + '\n').encode()
    expected[Path('crd-compatibility.json')] = (json.dumps(compatibility, sort_keys=True, indent=2) + '\n').encode()
    generated = set()
    for directory in ('crds', 'generated'):
        generated.update(path.relative_to(args.chart_dir) for path in (args.chart_dir / directory).glob('*.yaml'))
    generated.update(path for path in (Path('generated/crds.json'), Path('crd-compatibility.json'))
                     if (args.chart_dir / path).is_file())
    drift = [str(path) for path in sorted(set(expected) | generated)
             if path not in expected or not (args.chart_dir / path).is_file()
             or (args.chart_dir / path).read_bytes() != expected[path]]
    if args.action == 'check':
        if drift:
            sys.exit('Generated chart inputs differ: ' + ', '.join(drift))
        print('Generated chart inputs match selected application source.')
        return
    for path in generated - set(expected):
        (args.chart_dir / path).unlink()
    for path, data in expected.items():
        target = args.chart_dir / path
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_bytes(data)
    print('Generated chart inputs synchronized from ' + str(args.inputs_dir))


if __name__ == '__main__':
    try:
        main()
    except (OSError, ValueError, KeyError, TypeError, tarfile.TarError, subprocess.CalledProcessError, subprocess.TimeoutExpired) as error:
        sys.exit(str(error))
