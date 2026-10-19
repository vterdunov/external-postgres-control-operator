#!/usr/bin/env python3
"""Materialize release candidates and observed CI evidence using pinned tools.

Publication remains in release.py. Chart preparation has no image-write path.
"""

import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import tarfile
import tempfile

import release
import image_mirror

ROOT = Path(__file__).resolve().parents[1]
CHART_DIR = ROOT / 'charts/external-postgres-control-operator'


def run(*args, env=None, cwd=ROOT):
    subprocess.run(args, check=True, timeout=1800, env=env, cwd=cwd)


def write(path, value):
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(value, sort_keys=True, indent=2) + '\n')


def yaml_scalar(text, key):
    match = re.search(r'^' + re.escape(key) + r':\s*[\"\']?([^\"\'\s]+)[\"\']?\s*$', text, re.M)
    release.require(match is not None, 'missing YAML scalar: ' + key)
    return match[1]


def seed(stream, output):
    manifest = {'schema_version': 1, 'stream': stream, 'repository': os.environ.get('GITHUB_REPOSITORY'),
                'tag': os.environ.get('GITHUB_REF', '').removeprefix('refs/tags/'), 'source_sha': os.environ.get('GITHUB_SHA')}
    release.require(os.environ.get('GITHUB_EVENT_NAME') == 'push' and os.environ.get('GITHUB_REF', '').startswith('refs/tags/'), 'only trusted tag push can release')
    if stream == 'chart':
        metadata = (CHART_DIR / 'Chart.yaml').read_text()
        app_version = release.version(yaml_scalar(metadata, 'appVersion'))
        release.require(manifest['tag'] == 'chart-v' + yaml_scalar(metadata, 'version'), 'chart source version must match selected chart tag')
        manifest['image'] = {'repository': release.IMAGE, 'version': app_version, 'release_tag': 'v' + app_version}
    release.coordinates(manifest, stream)
    write(output, manifest)


def asset(manifest, directory, path, kind, **extra):
    entry = {'path': str(path), 'kind': kind, 'sha256': release.checksum(directory / path), **extra}
    manifest['assets'].append(entry)
    return entry


def crd_bundle(manifest, directory):
    image = manifest['image']
    inputs = [entry for entry in image['generated_inputs'] if entry['kind'] == 'crds']
    expected = {'crd/bases/postgresql.epco.io_' + name + '.yaml'
                for name in ('databaseroles', 'databases', 'externalpostgres')}
    release.require(len(inputs) == 3 and {entry.get('input_path') for entry in inputs} == expected,
                    'CRD bundle requires exactly the three generated EPCO CRDs')
    documents = []
    for entry in sorted(inputs, key=lambda item: item['input_path']):
        source = release.asset_path(directory, entry['path'])
        release.require(entry.get('source_sha') == image['source_sha'] and release.checksum(source) == entry['sha256'],
                        'CRD bundle generated input provenance mismatch')
        # Generated inputs are single YAML documents. Preserve their schemas
        # and comments, adding exactly one document boundary per input.
        document = re.sub(r'\A---[ \t]*\r?\n', '', source.read_text()).rstrip()
        release.require(document and not re.search(r'(?m)^(?:---|\.\.\.)[ \t]*(?:\r?$)', document),
                        'CRD bundle input must contain one YAML document')
        documents.append('---\n' + document + '\n')
    (directory / 'crds.yaml').write_text(''.join(documents))
    asset(manifest, directory, 'crds.yaml', 'crd_bundle', source_sha=image['source_sha'])


def signed_subject(directory, target, repository):
    reference = repository + '@' + target.get('index_digest', target.get('digest'))
    run('cosign', 'sign', '--yes', '--bundle', str(directory / target['bundle']), reference)


def image_candidate(manifest, directory):
    version = manifest['tag'].removeprefix('v')
    reference = release.IMAGE + ':' + version
    # Check again immediately before writes. A prior incomplete version requires
    # explicit investigation, never a silent new build at the same version tag.
    release.require(release.command('crane', 'digest', reference, allow_absent=True) is None, 'application version tag already exists')
    scanner = image_mirror.reference('sbom-scanner')
    run('docker', 'buildx', 'build', '--platform', 'linux/amd64,linux/arm64', '--provenance=mode=max',
        '--attest=type=sbom,generator=' + scanner,
        '--build-arg', 'VERSION=' + version, '--build-arg', 'REVISION=' + manifest['source_sha'],
        '--tag', reference, '--push', '--metadata-file', str(directory / 'build-metadata.json'), '.')
    index_digest = release.digest(release.command('crane', 'digest', reference))
    index = json.loads(release.command('crane', 'manifest', release.IMAGE + '@' + index_digest))
    platforms = []
    for descriptor in index.get('manifests', []):
        platform = descriptor.get('platform', {})
        if platform.get('os') == 'linux' and platform.get('architecture') in ('amd64', 'arm64'):
            platforms.append({'os': 'linux', 'architecture': platform['architecture'], 'digest': release.digest(descriptor['digest']),
                              'bundle': 'image-' + platform['architecture'] + '.sigstore.json'})
    platforms.sort(key=lambda entry: entry['architecture'])
    release.require(len(platforms) == 2, 'built image lacks both runnable platforms')
    image = {'repository': release.IMAGE, 'version': version, 'release_tag': manifest['tag'], 'source_sha': manifest['source_sha'],
             'index_digest': index_digest, 'platforms': platforms, 'bundle': 'image-index.sigstore.json', 'generated_inputs': []}
    manifest['image'] = image
    sources = [(path, 'crd/bases/' + path.name, 'crds') for path in sorted((ROOT / 'config/crd/bases').glob('*.yaml'))]
    sources.append((ROOT / 'config/rbac/role.yaml', 'rbac/role.yaml', 'rbac'))
    for source, input_path, kind in sources:
        name = 'application-' + source.name
        shutil.copyfile(source, directory / name)
        entry = asset(manifest, directory, name, kind, input_path=input_path)
        image['generated_inputs'].append({**entry, 'source_sha': image['source_sha']})
    # The whole config bundle also binds legacy delivery routes to this source.
    with tarfile.open(directory / 'application-config.tar.gz', 'w:gz') as archive:
        archive.add(ROOT / 'config', arcname='config')
    asset(manifest, directory, 'application-config.tar.gz', 'application_config')
    env = {**os.environ, 'IMG': reference + '@' + index_digest}
    run('bash', 'tools/deploy.sh', 'build-installer', env=env)
    for source, name, kind in [('install.yaml', 'installer.yaml', 'installer'), ('uninstall.yaml', 'controller-removal.yaml', 'controller_removal')]:
        shutil.copyfile(ROOT / 'dist' / source, directory / name)
        asset(manifest, directory, name, kind)
    asset(manifest, directory, 'build-metadata.json', 'build_metadata')
    for subject in [image, *platforms]:
        signed_subject(directory, subject, release.IMAGE)
        asset(manifest, directory, subject['bundle'], 'signature_bundle')


def selected_inputs(image, original, inputs):
    for entry in image['generated_inputs']:
        path = Path(entry.get('input_path', ''))
        release.require(path == Path('rbac/role.yaml') or (path.parent == Path('crd/bases') and path.suffix == '.yaml'), 'released generated input layout is invalid')
        destination = inputs / path
        destination.parent.mkdir(parents=True, exist_ok=True)
        shutil.copyfile(release.asset_path(original, entry['path']), destination)


def stage_chart(image, version, original, destination):
    shutil.copytree(CHART_DIR, destination)
    with tempfile.TemporaryDirectory(prefix='epco-chart-inputs-') as temporary:
        selected_inputs(image, original, Path(temporary))
        run(sys.executable, str(ROOT / 'tools/chart.py'), 'sync', '--chart-dir', str(destination), '--inputs-dir', temporary)
        run(sys.executable, str(ROOT / 'tools/chart.py'), 'check', '--chart-dir', str(destination), '--inputs-dir', temporary)
    metadata = (destination / 'Chart.yaml').read_text()
    metadata = re.sub(r'^version:.*$', 'version: ' + version, metadata, flags=re.M)
    metadata = re.sub(r'^appVersion:.*$', 'appVersion: "' + image['version'] + '"', metadata, flags=re.M)
    (destination / 'Chart.yaml').write_text(metadata)
    values = (destination / 'values.yaml').read_text()
    for key, value in [('repository', release.IMAGE), ('tag', image['version']), ('digest', image['index_digest'])]:
        values, count = re.subn(r'^(  ' + key + r':).*$', lambda match: match[1] + ' "' + value + '"', values, count=1, flags=re.M)
        release.require(count == 1, 'chart image default is missing: ' + key)
    (destination / 'values.yaml').write_text(values)
    run('helm', 'lint', str(destination), '--strict')


def chart_candidate(manifest, directory):
    image = manifest['image']
    chart_version = manifest['tag'].removeprefix('chart-v')
    with tempfile.TemporaryDirectory(prefix='epco-released-image-') as temporary:
        original = Path(temporary)
        release.original_image(manifest, original)
        for entry in image['generated_inputs']:
            shutil.copyfile(original / entry['path'], directory / entry['path'])
            manifest['assets'].append({key: value for key, value in entry.items() if key != 'source_sha'})
        for subject in [image, *image['platforms']]:
            shutil.copyfile(original / subject['bundle'], directory / subject['bundle'])
            asset(manifest, directory, subject['bundle'], 'signature_bundle')
        # Keep original config and legacy installers with their source provenance.
        original_manifest = release.downloaded_manifest(original)
        for entry in original_manifest['assets']:
            if entry['kind'] in ('application_config', 'installer', 'controller_removal'):
                shutil.copyfile(original / entry['path'], directory / entry['path'])
                manifest['assets'].append(entry)
        with tempfile.TemporaryDirectory(prefix='epco-chart-stage-') as staging:
            chart_dir = Path(staging) / 'external-postgres-control-operator'
            stage_chart(image, chart_version, original, chart_dir)
            run('helm', 'package', str(chart_dir), '--destination', str(directory))
    archive = 'external-postgres-control-operator-' + chart_version + '.tgz'
    release.require(release.command('crane', 'digest', release.CHART + ':' + chart_version, allow_absent=True) is None, 'chart version tag already exists')
    run('helm', 'push', str(directory / archive), 'oci://ghcr.io/vterdunov/charts')
    chart = {'repository': release.CHART, 'version': chart_version, 'app_version': image['version'], 'archive': archive,
             'digest': release.digest(release.command('crane', 'digest', release.CHART + ':' + chart_version)), 'bundle': 'chart.sigstore.json'}
    manifest['chart'] = chart
    asset(manifest, directory, archive, 'chart')
    signed_subject(directory, chart, release.CHART)
    asset(manifest, directory, chart['bundle'], 'signature_bundle')


def prepare(preflight, directory):
    manifest = json.loads(preflight.read_text())
    release.require(manifest.get('completed') is False, 'completed immutable release needs no build')
    release.coordinates(manifest, manifest['stream'])
    release.source_gates(manifest)
    directory.mkdir(parents=True, exist_ok=True)
    release.require(not any(directory.iterdir()), 'candidate output directory must be empty; unrelated files cannot become release assets')
    record = release.release(manifest['tag'])
    if record is not None:
        release.require(record.get('draft') is True, 'release already published')
        # Resume only verified complete drafts. Never rebuild an existing OCI tag.
        recovered = (release.download_checkpoint(record, directory, manifest) if release.checkpoint_record(record)
                     and len(record['assets']) == 2 else release.download_release(record, directory, expected=manifest, locked=False))
        if recovered['stream'] == 'image':
            if not (directory / 'external-postgres-control-operator-0.0.0-image-validation.tgz').is_file():
                validation_chart(recovered['image'], directory)
        write(directory / 'manifest.json', recovered)
        return recovered
    manifest.pop('completed', None)
    manifest['assets'] = []
    manifest['checksum_manifest'] = 'SHA256SUMS'
    manifest['checksum_bundle'] = 'SHA256SUMS.sigstore.json'
    if manifest['stream'] == 'image':
        image_candidate(manifest, directory)
        validation_chart(manifest['image'], directory)
    else:
        chart_candidate(manifest, directory)
    crd_bundle(manifest, directory)
    evidence = {**manifest, 'image_source_sha': manifest['image']['source_sha'], 'image_index_digest': manifest['image']['index_digest'],
                'chart_digest': manifest.get('chart', {}).get('digest')}
    write(directory / 'release-evidence.json', evidence)
    asset(manifest, directory, 'release-evidence.json', 'evidence')
    (directory / 'SHA256SUMS').write_text(''.join(entry['sha256'] + '  ' + entry['path'] + '\n' for entry in manifest['assets']))
    run('cosign', 'sign-blob', '--yes', '--bundle', str(directory / manifest['checksum_bundle']), str(directory / 'SHA256SUMS'))
    release.validate(manifest, directory, manifest['stream'])
    release.registry_proof(manifest, directory)
    write(directory / 'manifest.json', manifest)
    save_checkpoint(manifest, directory)
    return manifest


def validation_chart(image, directory):
    with tempfile.TemporaryDirectory(prefix='epco-image-lifecycle-chart-') as temporary:
        chart = Path(temporary) / 'external-postgres-control-operator'
        stage_chart(image, '0.0.0-image-validation', directory, chart)
        run('helm', 'package', str(chart), '--destination', str(directory))


def save_checkpoint(manifest, directory):
    archive_path = directory / release.CHECKPOINT
    sources = [source for source in directory.iterdir() if source.name not in (release.CHECKPOINT, release.CHECKPOINT_BUNDLE)]
    with tarfile.open(archive_path, 'w:gz') as archive:
        for source in sorted(sources):
            archive.add(source, arcname='candidate/' + source.name, recursive=False)
    run('cosign', 'sign-blob', '--yes', '--bundle', str(directory / release.CHECKPOINT_BUNDLE), str(archive_path))
    release.command('cosign', 'verify-blob', *release.policy(manifest['stream'], manifest['tag'], manifest['source_sha']),
                    '--bundle', str(directory / release.CHECKPOINT_BUNDLE), str(archive_path))
    release.tag_source(manifest)
    release.require(release.release(manifest['tag']) is None, 'release appeared during candidate preparation; refusing overwrite')
    release.command('gh', 'release', 'create', manifest['tag'], '--repo', release.REPOSITORY, '--verify-tag', '--draft',
                    '--title', manifest['tag'], '--notes', 'Prepared candidate; runtime acceptance and immutable publication pending.')
    release.command('gh', 'release', 'upload', manifest['tag'], str(archive_path), str(directory / release.CHECKPOINT_BUNDLE), '--repo', release.REPOSITORY)


def workspace(manifest_path, application):
    manifest = json.loads(manifest_path.read_text())
    release.validate(manifest, manifest_path.parent, manifest['stream'])
    release.registry_proof(manifest, manifest_path.parent)
    source = release.command('git', '-C', str(application), 'rev-parse', 'HEAD')
    release.require(source == manifest['image']['source_sha'], 'runtime application checkout must match released image source')
    # Keep behavior assertions from the application checkout. Inject only current
    # delivery tooling and the signed release's authoritative configuration.
    for path in ['tools/chart.py', 'tools/deploy.sh']:
        target = application / path
        target.parent.mkdir(parents=True, exist_ok=True)
        shutil.copyfile(ROOT / path, target)
    bundle = next(entry for entry in manifest['assets'] if entry['kind'] == 'application_config')
    with tarfile.open(release.asset_path(manifest_path.parent, bundle['path']), 'r:gz') as archive:
        for member in archive.getmembers():
            path = Path(member.name)
            release.require(not path.is_absolute() and '..' not in path.parts and path.parts[0] == 'config'
                            and (member.isfile() or member.isdir()), 'unsafe released application config archive')
        archive.extractall(application, filter='data')
    for entry in manifest['image']['generated_inputs']:
        release.require(release.checksum(application / 'config' / entry['input_path']) == entry['sha256'], 'runtime released config provenance mismatch')


def environment(manifest_path):
    manifest = json.loads(manifest_path.read_text())
    image = manifest['image']
    values = {'EPCO_E2E_BUILD_MODE': 'prebuilt', 'EPCO_E2E_IMAGE_REFERENCE': release.IMAGE + ':' + image['version'] + '@' + image['index_digest'],
              'EPCO_DELIVERY_IMAGE_REFERENCE': release.IMAGE + ':' + image['version'] + '@' + image['index_digest'],
              'EPCO_DELIVERY_IMAGE_SOURCE_SHA': image['source_sha'], 'EPCO_DELIVERY_CHART_SOURCE_SHA': manifest['source_sha'],
              'EPCO_RELEASE_MANIFEST': str(manifest_path.resolve()), 'EPCO_E2E_RUNTIME_REPORT': 'true'}
    if manifest['stream'] == 'chart':
        values['EPCO_DELIVERY_CHART_PACKAGE'] = str((manifest_path.parent / manifest['chart']['archive']).resolve())
    else:
        # Image publication precedes charts. Its lifecycle smoke uses a temporary
        # local chart assembled from these exact new image inputs; no chart writes.
        values['EPCO_DELIVERY_CHART_PACKAGE'] = str((manifest_path.parent / 'external-postgres-control-operator-0.0.0-image-validation.tgz').resolve())
    release.require(Path(values['EPCO_DELIVERY_CHART_PACKAGE']).is_file(), 'candidate lifecycle chart archive is missing')
    with Path(os.environ['GITHUB_ENV']).open('a') as output:
        for key, value in values.items():
            release.require('\n' not in value, 'invalid CI environment value')
            output.write(key + '=' + value + '\n')


def observed_runtime(manifest_path, output):
    manifest = json.loads(manifest_path.read_text())
    context = os.environ.get('EPCO_E2E_CONTEXT', '')
    kubeconfig = os.environ.get('KUBECONFIG', '')
    release.require(context.startswith('kind-epco-e2e-') and Path(kubeconfig).is_file(), 'runtime observation requires owned Kind context')
    prefix = ['kubectl', '--kubeconfig', kubeconfig, '--context', context, '--request-timeout=30s']
    pods = json.loads(release.command(*prefix, '-n', 'epco-system', 'get', 'pods', '-o', 'json'))['items']
    managers = [pod for pod in pods if pod['metadata'].get('labels', {}).get('control-plane') == 'controller-manager' and not pod['metadata'].get('deletionTimestamp')]
    release.require(managers and all(all(container['ready'] for container in pod['status']['containerStatuses']) for pod in managers), 'manager Pod is not Ready')
    expected = release.IMAGE + ':' + manifest['image']['version'] + '@' + manifest['image']['index_digest']
    valid = {entry['digest'] for entry in manifest['image']['platforms']} | {manifest['image']['index_digest']}
    nodes = json.loads(release.command(*prefix, 'get', 'nodes', '-o', 'json'))['items']
    observations = []
    for pod in managers:
        image_id = pod['status']['containerStatuses'][0]['imageID']
        actual = 'sha256:' + image_id.split('sha256:')[-1]
        release.require(pod['spec']['containers'][0]['image'] == expected and actual in valid, 'manager pulled imageID differs from accepted runnable platform')
        node = next(node for node in nodes if node['metadata']['name'] == pod['spec']['nodeName'])
        platform = next(entry for entry in manifest['image']['platforms'] if entry['architecture'] == node['status']['nodeInfo']['architecture'])
        # CRI implementations may expose the index or resolved manifest as
        # imageID. Both are signed subjects. Native startup independently checks
        # the pulled runnable architecture before this installed regression.
        release.require(actual in (platform['digest'], manifest['image']['index_digest']), 'runtime manifest does not match node architecture')
        observations.append({'imageID': image_id, 'architecture': platform['architecture'], 'platform_digest': platform['digest'],
                             'podUID': pod['metadata']['uid']})
    lease = json.loads(release.command(*prefix, '-n', 'epco-system', 'get', 'lease', 'ccd893cc.epco.io', '-o', 'json'))
    release.require(any(lease['spec'].get('holderIdentity', '').startswith(pod['metadata']['name'] + '_') for pod in managers), 'manager did not acquire shared leader Lease')
    fixture = output.parent / 'fixture-observed.txt'
    release.require(fixture.is_file(), 'full application suite did not produce measured PostgreSQL/client versions')
    write(output, {'source_sha': manifest['source_sha'], 'image_source_sha': manifest['image']['source_sha'], 'subjects': release.subjects(manifest),
                   'pg_major': os.environ['EPCO_PG_VERSION'], 'context': context, 'observations': observations,
                   'leaseUID': lease['metadata']['uid'], 'fixture_observed': fixture.read_text()})


def measured_versions(report):
    text = report.get('fixture_observed', '')
    release.require(isinstance(text, str) and len(text) <= 8192, 'bounded measured PostgreSQL/client versions are required')
    server = re.search(r'(?m)^server (?:observed )?version:\s*([0-9]+\.[0-9]+)\s*$', text)
    if server is None:
        server = re.search(r'(?m)^server actual version: PostgreSQL ([0-9]+\.[0-9]+)(?:\s|$)', text)
    client = release.CLIENT_VERSION_LINE.search(text)
    release.require(server is not None and server[1].split('.')[0] == report['pg_major']
                    and client is not None and client[1] == '16.15', 'measured server/client versions do not match runtime fixture')
    number = re.search(r'(?m)^server_version_num:\s*([0-9]+)\s*$', text)
    if number:
        release.require(int(number[1]) // 10000 == int(report['pg_major']), 'measured server version number disagrees with selected major')
    return {'server': server[1], 'client': client[1]}


def aggregate(manifest_path, reports, output):
    manifest = json.loads(manifest_path.read_text())
    installed = [release.clean_installed(json.loads(path.read_text()), manifest) for path in reports.rglob('release-installed.json')]
    release.require(len(installed) == 8, 'runtime requires PostgreSQL 15-18 on both release architectures')
    matrix = set()
    for report in installed:
        release.require(report['source_sha'] == manifest['source_sha'] and report['image_source_sha'] == manifest['image']['source_sha']
                        and report['subjects'] == release.subjects(manifest), 'installed evidence source/subject mismatch')
        arches = {entry['architecture'] for entry in report['observations']}
        release.require(len(arches) == 1, 'runtime report contains ambiguous architecture')
        architecture = next(iter(arches))
        platform = next((entry for entry in manifest['image']['platforms'] if entry['architecture'] == architecture), None)
        release.require(platform is not None, 'unaccepted runtime architecture')
        release.require(isinstance(report.get('context'), str) and re.fullmatch(r'kind-epco-e2e-[a-z0-9-]+', report['context']), 'runtime fixture context is not owned')
        release.require(isinstance(report.get('leaseUID'), str) and re.fullmatch(r'[A-Za-z0-9-]{1,128}', report['leaseUID']), 'observed leader Lease UID is required')
        for observation in report['observations']:
            image_id = observation.get('imageID', '')
            measured = re.search(r'sha256:[a-f0-9]{64}$', image_id) if isinstance(image_id, str) else None
            release.require(measured and measured[0] in (platform['digest'], manifest['image']['index_digest'])
                            and observation.get('platform_digest') == platform['digest'], 'observed Pod imageID/platform digest mismatch')
            release.require(isinstance(observation.get('podUID'), str) and re.fullmatch(r'[A-Za-z0-9-]{1,128}', observation['podUID']), 'observed Pod UID is required')
        measured_versions(report)
        matrix.add((arches.pop(), report['pg_major']))
    release.require(matrix == {(arch, str(pg)) for arch in ('amd64', 'arm64') for pg in (15, 16, 17, 18)}, 'runtime matrix coverage is incomplete')
    lifecycle_paths = list(reports.rglob('delivery-lifecycle.json'))
    release.require(len(lifecycle_paths) == 1, 'single complete lifecycle evidence is required')
    lifecycle = release.clean_lifecycle(json.loads(lifecycle_paths[0].read_text()), manifest)
    release.require(lifecycle.get('scope') == 'selected artifact lifecycle' and lifecycle.get('imageSourceSHA') == manifest['image']['source_sha']
                    and lifecycle.get('chartSourceSHA') == manifest['source_sha'], 'lifecycle evidence source/scope mismatch')
    expected_ref = release.IMAGE + ':' + manifest['image']['version'] + '@' + manifest['image']['index_digest']
    release.require(lifecycle.get('imageReference') == expected_ref, 'lifecycle image identity mismatch')
    archive = manifest.get('chart', {}).get('archive', 'external-postgres-control-operator-0.0.0-image-validation.tgz')
    release.require(lifecycle.get('chartArchiveSHA256') == release.checksum(manifest_path.parent / archive), 'lifecycle chart archive mismatch')
    required = {'CR readiness, real application SQL and credential controls', 'second release refused in same and other namespace',
                'same-UID removed named-table grant revoked in real SQL; unrelated access preserved',
                'unknown schema refused before schema or manager writes; known schema reinstalls',
                'same-package Helm upgrade and controller revision rollback mechanics',
                'live, terminating and real list-denied retirement refusal', 'delivery lifecycle complete',
                'restricted namespace admits healthy manager and rejects privileged Pod',
                'NetworkPolicy actual CNI allow-deny-allow and node probe continuity'}
    gates = lifecycle.get('gates', [])
    release.require(required.issubset({entry.get('gate') for entry in gates if entry.get('result') == 'passed'}), 'lifecycle gate observations are incomplete')
    measured = [entry['podImageID'] for entry in gates if 'podImageID' in entry]
    allowed = {entry['digest'] for entry in manifest['image']['platforms']} | {manifest['image']['index_digest']}
    release.require(measured and all('sha256:' + value.split('sha256:')[-1] in allowed for value in measured), 'lifecycle Pod imageIDs do not match selected signed subjects')
    write(output, {'repository': release.REPOSITORY, 'stream': manifest['stream'], 'tag': manifest['tag'], 'source_sha': manifest['source_sha'],
                   'image_source_sha': manifest['image']['source_sha'], 'image_index_digest': manifest['image']['index_digest'],
                   'chart_digest': manifest.get('chart', {}).get('digest'), 'subjects': release.subjects(manifest),
                   'gates': {'platforms': 'success', 'runtime': 'success', 'lifecycle': 'success', 'pg15-18': 'success'},
                   'observed_runtime': installed, 'observed_lifecycle': lifecycle})


def bind_runtime(manifest_path, artifact_id):
    manifest = json.loads(manifest_path.read_text())
    if 'measured_runtime' in manifest:
        return
    manifest['runtime_evidence'] = {'run_id': int(os.environ['GITHUB_RUN_ID']), 'run_attempt': int(os.environ.get('GITHUB_RUN_ATTEMPT', '1')),
                                  'artifact_id': int(artifact_id)}
    write(manifest_path, manifest)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('action', choices=('seed', 'prepare', 'workspace', 'environment', 'observe', 'aggregate', 'bind-runtime'))
    parser.add_argument('--stream', choices=('image', 'chart'))
    parser.add_argument('--manifest', type=Path)
    parser.add_argument('--output', type=Path)
    parser.add_argument('--application', type=Path)
    parser.add_argument('--reports', type=Path)
    parser.add_argument('--artifact-id')
    args = parser.parse_args()
    if args.action == 'seed':
        seed(args.stream, args.output)
    elif args.action == 'prepare':
        prepare(args.manifest, args.output)
    elif args.action == 'workspace':
        workspace(args.manifest.resolve(), args.application.resolve())
    elif args.action == 'environment':
        environment(args.manifest.resolve())
    elif args.action == 'observe':
        observed_runtime(args.manifest.resolve(), args.output.resolve())
    elif args.action == 'aggregate':
        aggregate(args.manifest.resolve(), args.reports, args.output)
    else:
        bind_runtime(args.manifest, args.artifact_id)


if __name__ == '__main__':
    try:
        main()
    except (ValueError, KeyError, TypeError, OSError, subprocess.CalledProcessError, tarfile.TarError) as error:
        sys.exit(str(error))
