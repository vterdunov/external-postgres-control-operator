#!/usr/bin/env python3
"""Fail-closed verification and immutable publication of EPCO release records.

The manifest is an inventory, never an authority for CI, signatures or runtime
success. Those claims are checked against GitHub, GHCR and Sigstore separately.
All external commands use argument arrays and bounded execution.
"""

import argparse
import hashlib
import io
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tarfile
import tempfile
from urllib.parse import quote
import zipfile

REPOSITORY = 'vterdunov/external-postgres-control-operator'
IMAGE = 'ghcr.io/' + REPOSITORY
CHART = 'ghcr.io/vterdunov/charts/external-postgres-control-operator'
ISSUER = 'https://token.actions.githubusercontent.com'
CHECKPOINT = 'release-candidate.tar.gz'
CHECKPOINT_BUNDLE = 'release-candidate.sigstore.json'
GATES = {
    'test.yml': ['test-unit', 'test-envtest', 'check-generated', 'PostgreSQL 15', 'PostgreSQL 16'],
    'lint.yml': ['Run on Ubuntu'], 'test-e2e.yml': ['E2E'],
    'delivery.yml': ['Release validation', 'Platforms'],
}
SEMVER = re.compile(r'(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?')


def require(condition, message):
    if not condition:
        raise ValueError(message)


def version(value):
    match = SEMVER.fullmatch(value) if isinstance(value, str) else None
    require(match is not None, 'invalid published SemVer: ' + str(value))
    if match[4]:
        require(all(not item.isdigit() or item == '0' or not item.startswith('0')
                    for item in match[4].split('.')), 'leading-zero prerelease identifier')
    return value


def sha(value):
    require(isinstance(value, str) and re.fullmatch(r'[a-f0-9]{40}', value), 'invalid source SHA')
    return value


def digest(value):
    require(isinstance(value, str) and re.fullmatch(r'sha256:[a-f0-9]{64}', value), 'invalid subject digest')
    return value


def checksum(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def asset_path(directory, name):
    require(isinstance(name, str) and name and not Path(name).is_absolute()
            and '..' not in Path(name).parts, 'asset path escapes manifest directory')
    target = directory / name
    require(target.resolve().is_relative_to(directory.resolve()), 'asset symlink escapes manifest directory')
    require(target.is_file(), 'missing release asset: ' + name)
    return target


def coordinates(manifest, stream):
    require(manifest.get('schema_version') == 1 and manifest.get('stream') == stream, 'invalid release schema/stream')
    require(manifest.get('repository') == REPOSITORY, 'release repository is not allowlisted')
    sha(manifest.get('source_sha'))
    prefix = 'v' if stream == 'image' else 'chart-v'
    tag = manifest.get('tag')
    require(isinstance(tag, str) and tag.startswith(prefix), 'wrong release tag stream')
    release_version = version(tag[len(prefix):])
    manifest['prerelease'] = '-' in release_version
    manifest['make_latest'] = stream == 'image' and not manifest['prerelease']
    if stream == 'chart':
        image = manifest.get('image', {})
        require(image.get('repository') == IMAGE, 'image repository is not allowlisted')
        version(image.get('version'))
        require(image.get('release_tag') == 'v' + image['version'], 'image release version/tag mismatch')
    return release_version


def inventory(manifest, directory):
    assets = manifest.get('assets')
    require(isinstance(assets, list) and assets, 'release assets are missing')
    records = {}
    for entry in assets:
        name = entry.get('path')
        require(name not in records, 'duplicate release asset')
        path = asset_path(directory, name)
        require(re.fullmatch(r'[a-f0-9]{64}', entry.get('sha256', ''))
                and checksum(path) == entry['sha256'], 'release asset checksum mismatch: ' + str(name))
        records[name] = entry
    sums_name = manifest.get('checksum_manifest')
    bundle_name = manifest.get('checksum_bundle')
    require(sums_name not in records and bundle_name not in records, 'checksum inventory must not include itself')
    sums = asset_path(directory, sums_name).read_text().splitlines()
    expected = {name: entry['sha256'] for name, entry in records.items()}
    actual = {}
    for line in sums:
        match = re.fullmatch(r'([a-f0-9]{64})  (.+)', line)
        require(match is not None and match[2] not in actual, 'invalid checksum inventory')
        actual[match[2]] = match[1]
    require(actual == expected, 'checksum inventory does not exactly cover release assets')
    asset_path(directory, bundle_name)
    return records


def validate(manifest, directory, stream):
    release_version = coordinates(manifest, stream)
    image = manifest['image']
    require(image.get('repository') == IMAGE, 'image repository is not allowlisted')
    version(image.get('version'))
    sha(image.get('source_sha'))
    require(image.get('release_tag') == 'v' + image['version'], 'image release tag mismatch')
    digest(image.get('index_digest'))
    if stream == 'image':
        require(image['version'] == release_version and image['source_sha'] == manifest['source_sha'], 'image source/version mismatch')
    platforms = image.get('platforms', [])
    require(len(platforms) == 2 and {(p.get('os'), p.get('architecture')) for p in platforms}
            == {('linux', 'amd64'), ('linux', 'arm64')}, 'exactly two runnable Linux platforms are required')
    require(len({digest(p.get('digest')) for p in platforms} | {image['index_digest']}) == 3, 'duplicate subject digest')
    records = inventory(manifest, directory)
    for subject in [image, *platforms] + ([manifest['chart']] if stream == 'chart' else []):
        name = subject.get('bundle')
        require(name in records and records[name].get('kind') == 'signature_bundle', 'subject bundle is absent from inventory')
        parsed = json.loads(asset_path(directory, name).read_text())
        require(parsed.get('mediaType', '').startswith('application/vnd.dev.sigstore.bundle.'), 'standardized Sigstore bundle required')
    inputs = image.get('generated_inputs', [])
    require(inputs and {'crds', 'rbac'}.issubset({entry.get('kind') for entry in inputs}), 'generated CRD/RBAC inputs required')
    for entry in inputs:
        require(entry.get('source_sha') == image['source_sha'] and records.get(entry.get('path')) == {
            key: value for key, value in entry.items() if key != 'source_sha'
        }, 'generated input provenance mismatch')
    if stream == 'chart':
        chart = manifest['chart']
        require(chart.get('repository') == CHART and chart.get('version') == release_version
                and chart.get('app_version') == image['version'], 'chart coordinates/version mismatch')
        digest(chart.get('digest'))
        require(records.get(chart.get('archive'), {}).get('kind') == 'chart', 'chart archive missing from inventory')
        with tarfile.open(asset_path(directory, chart['archive']), 'r:gz') as archive:
            names = set()
            for member in archive.getmembers():
                path = Path(member.name)
                require(not path.is_absolute() and '..' not in path.parts and member.name not in names
                        and (member.isfile() or member.isdir()) and path.parts[0] == 'external-postgres-control-operator', 'unsafe chart archive')
                names.add(member.name)
            chart_yaml = archive.extractfile('external-postgres-control-operator/Chart.yaml').read().decode()
            values = archive.extractfile('external-postgres-control-operator/values.yaml').read().decode()
            require(re.search(r'^version:\s*[\"\']?' + re.escape(chart['version']) + r'[\"\']?\s*$', chart_yaml, re.M)
                    and re.search(r'^appVersion:\s*[\"\']?' + re.escape(image['version']) + r'[\"\']?\s*$', chart_yaml, re.M), 'archive chart metadata mismatch')
            for field, value in [('repository', IMAGE), ('tag', image['version']), ('digest', image['index_digest'])]:
                require(re.search(r'^\s+' + field + r':\s*[\"\']?' + re.escape(value) + r'[\"\']?\s*$', values, re.M), 'archive default image mismatch')
    else:
        require({'installer', 'controller_removal'}.issubset({entry['kind'] for entry in records.values()}), 'installer/removal assets missing')
    require(any(entry.get('kind') == 'evidence' for entry in records.values()), 'release evidence missing')
    return manifest


def command(*args, env=None, binary=False, allow_absent=False):
    result = subprocess.run(args, capture_output=True, timeout=300, env=env)
    if result.returncode:
        error = result.stderr.decode(errors='replace').strip()
        if allow_absent and ('HTTP 404:' in error or 'gh: Not Found (HTTP 404)' in error
                             or 'MANIFEST_UNKNOWN:' in error):
            return None
        raise ValueError(' '.join(args[:3]) + ': ' + error)
    return result.stdout if binary else result.stdout.decode().strip()


def api(endpoint, allow_absent=False, binary=False):
    value = command('gh', 'api', 'repos/' + REPOSITORY + '/' + endpoint, allow_absent=allow_absent, binary=binary)
    return value if value is None or binary else json.loads(value)


def release(tag):
    record = api('releases/tags/' + quote(tag, safe=''), allow_absent=True)
    if record is not None:
        return record
    # The tag endpoint omits drafts. Listing needs push access to see them.
    pages = json.loads(command('gh', 'api', 'repos/' + REPOSITORY + '/releases?per_page=100', '--paginate', '--slurp'))
    require(isinstance(pages, list) and all(isinstance(page, list) for page in pages), 'invalid GitHub release list')
    records = [item for page in pages for item in page]
    require(all(isinstance(item, dict) for item in records), 'invalid GitHub release record')
    matches = [item for item in records if item.get('tag_name') == tag]
    require(len(matches) <= 1, 'ambiguous GitHub releases for tag: ' + tag)
    return matches[0] if matches else None


def trusted_run(run, source, workflow, branch):
    require(run.get('head_sha') == source and run.get('head_branch') == branch and run.get('event') == 'push'
            and run.get('path', '').split('@')[0] == '.github/workflows/' + workflow
            and run.get('repository', {}).get('full_name') == REPOSITORY
            and run.get('head_repository', {}).get('full_name') == REPOSITORY, 'untrusted source/workflow run')


def successful_jobs(run, required):
    attempt = run.get('run_attempt')
    require(isinstance(attempt, int) and attempt > 0, 'missing workflow attempt')
    jobs = api(f'actions/runs/{run["id"]}/attempts/{attempt}/jobs?per_page=100')['jobs']
    for name in required:
        matches = [job for job in jobs if job.get('name') == name]
        require(len(matches) == 1 and matches[0].get('status') == 'completed'
                and matches[0].get('conclusion') == 'success', 'required current-attempt job did not pass: ' + name)


def tag_source(manifest):
    tag = api('git/ref/tags/' + quote(manifest['tag'], safe=''))['object']
    visited = set()
    while tag.get('type') == 'tag':
        sha(tag.get('sha'))
        require(tag['sha'] not in visited and len(visited) < 32, 'cyclic or excessive annotated tag chain')
        visited.add(tag['sha'])
        tag = api('git/tags/' + tag['sha'])['object']
    require(tag.get('type') == 'commit' and tag.get('sha') == manifest['source_sha'], 'tag no longer resolves to release source')


def source_gates(manifest):
    tag_source(manifest)
    compare = api('compare/' + manifest['source_sha'] + '...main')
    require(compare.get('status') in ('ahead', 'identical')
            and compare.get('merge_base_commit', {}).get('sha') == manifest['source_sha'], 'release source is not on trusted main')
    for workflow, jobs in GATES.items():
        data = api('actions/workflows/' + workflow + '/runs?head_sha=' + manifest['source_sha'] + '&branch=main&event=push&per_page=100')
        runs = data.get('workflow_runs', [])
        require(runs, 'missing trusted gate: ' + workflow)
        newest = max(runs, key=lambda run: (run.get('created_at', ''), run['id']))
        trusted_run(newest, manifest['source_sha'], workflow, 'main')
        require(newest.get('status') == 'completed' and newest.get('conclusion') == 'success', 'latest gate run did not pass: ' + workflow)
        successful_jobs(newest, jobs)


def policy(stream, tag, source):
    return ['--certificate-identity', f'https://github.com/{REPOSITORY}/.github/workflows/release-{stream}.yml@refs/tags/{tag}',
            '--certificate-oidc-issuer', ISSUER, '--certificate-github-workflow-repository', REPOSITORY,
            '--certificate-github-workflow-ref', 'refs/tags/' + tag, '--certificate-github-workflow-trigger', 'push',
            '--certificate-github-workflow-sha', source]


def subjects(manifest):
    image = manifest['image']
    return [IMAGE + '@' + image['index_digest'], *[IMAGE + '@' + p['digest'] for p in image['platforms']]] + (
        [CHART + '@' + manifest['chart']['digest']] if manifest['stream'] == 'chart' else [])


def registry_proof(manifest, directory, env=None):
    image = manifest['image']
    require(command('crane', 'digest', IMAGE + ':' + image['version'], env=env) == image['index_digest'], 'image version tag digest mismatch')
    index = json.loads(command('crane', 'manifest', IMAGE + '@' + image['index_digest'], env=env))
    runnable = [(p.get('platform', {}).get('os'), p.get('platform', {}).get('architecture'), p.get('digest'))
                for p in index.get('manifests', []) if p.get('platform', {}).get('os') != 'unknown'
                and p.get('annotations', {}).get('vnd.docker.reference.type') != 'attestation-manifest']
    require(set(runnable) == {(p['os'], p['architecture'], p['digest']) for p in image['platforms']}, 'runnable platform subjects do not match signed index')
    for subject in [image, *image['platforms']]:
        ref = IMAGE + '@' + subject.get('index_digest', subject.get('digest'))
        flags = policy('image', image['release_tag'], image['source_sha'])
        command('cosign', 'verify', *flags, ref, env=env)
        command('cosign', 'verify-blob-attestation', *flags, '--bundle', str(asset_path(directory, subject['bundle'])),
                '--digest', ref.split('@sha256:')[1], '--digestAlg', 'sha256', '--type', 'https://sigstore.dev/cosign/sign/v1', env=env)
        if subject is not image:
            config = json.loads(command('crane', 'config', ref, env=env))['config']
            labels = config.get('Labels', {})
            require(config.get('User') in ('65532', '65532:65532') and labels.get('org.opencontainers.image.revision') == image['source_sha']
                    and labels.get('org.opencontainers.image.version') == image['version']
                    and labels.get('org.opencontainers.image.source') == 'https://github.com/' + REPOSITORY, 'image runtime metadata mismatch')
    if manifest['stream'] == 'chart':
        chart = manifest['chart']
        ref = CHART + '@' + chart['digest']
        require(command('crane', 'digest', CHART + ':' + chart['version'], env=env) == chart['digest'], 'chart version tag digest mismatch')
        contents = json.loads(command('crane', 'manifest', ref, env=env))
        require(contents.get('config', {}).get('mediaType') == 'application/vnd.cncf.helm.config.v1+json'
                and any(layer.get('mediaType') == 'application/vnd.cncf.helm.chart.content.v1.tar+gzip'
                        and layer.get('digest') == 'sha256:' + checksum(asset_path(directory, chart['archive']))
                        for layer in contents.get('layers', [])), 'chart OCI content differs from archive')
        flags = policy('chart', manifest['tag'], manifest['source_sha'])
        command('cosign', 'verify', *flags, ref, env=env)
        command('cosign', 'verify-blob-attestation', *flags, '--bundle', str(asset_path(directory, chart['bundle'])),
                '--digest', chart['digest'].removeprefix('sha256:'), '--digestAlg', 'sha256', '--type', 'https://sigstore.dev/cosign/sign/v1', env=env)
    command('cosign', 'verify-blob', *policy(manifest['stream'], manifest['tag'], manifest['source_sha']),
            '--bundle', str(asset_path(directory, manifest['checksum_bundle'])), str(asset_path(directory, manifest['checksum_manifest'])), env=env)


def downloaded_manifest(directory):
    evidence = directory / 'release-evidence.json'
    manifest = json.loads(evidence.read_text())
    # Evidence cannot contain its own checksum. The signed checksum inventory
    # supplies the final self-record when reconstructing the downloaded inventory.
    known = {entry['path'] for entry in manifest['assets']}
    for line in (directory / manifest['checksum_manifest']).read_text().splitlines():
        match = re.fullmatch(r'([a-f0-9]{64})  (.+)', line)
        require(match is not None, 'invalid downloaded checksum inventory')
        if match[2] not in known:
            require(match[2] == 'release-evidence.json', 'unsigned evidence omitted an unexpected asset')
            manifest['assets'].append({'path': match[2], 'sha256': match[1], 'kind': 'evidence'})
    return manifest


def download_release(record, directory, expected=None, locked=True):
    require(record.get('draft') is not True and record.get('immutable') is True if locked else record.get('draft') is True,
            'release must be immutable and published' if locked else 'release must be a draft')
    command('gh', 'release', 'download', record['tag_name'], '--repo', REPOSITORY, '--dir', str(directory))
    manifest = downloaded_manifest(directory)
    require(manifest['tag'] == record['tag_name'], 'downloaded release tag mismatch')
    tag_source(manifest)
    validate(manifest, directory, manifest['stream'])
    names = {entry['path'] for entry in manifest['assets']} | {manifest['checksum_manifest'], manifest['checksum_bundle']}
    remote_assets = {entry['name']: entry for entry in record['assets']}
    require(names == set(remote_assets), 'release asset set is incomplete or unexpected')
    for name in names:
        require(remote_assets[name].get('digest') == 'sha256:' + checksum(asset_path(directory, name)), 'GitHub asset digest mismatch: ' + name)
    if expected:
        require(all(manifest.get(key) == expected.get(key) for key in ('stream', 'tag', 'repository', 'source_sha')), 'locked release coordinates mismatch')
        for key in ('image', 'chart'):
            if key in expected:
                require(all(manifest.get(key, {}).get(field) == value for field, value in expected[key].items()), 'locked release subject mismatch')
    registry_proof(manifest, directory)
    if locked:
        command('gh', 'release', 'verify', record['tag_name'], '--repo', REPOSITORY)
        for name in sorted(names):
            command('gh', 'release', 'verify-asset', record['tag_name'], str(directory / name), '--repo', REPOSITORY)
    return manifest


def checkpoint_record(record):
    return {CHECKPOINT, CHECKPOINT_BUNDLE}.issubset({entry['name'] for entry in record.get('assets', [])})


def download_checkpoint(record, directory, expected):
    require(record.get('draft') is True and checkpoint_record(record), 'verified candidate checkpoint requires an existing draft')
    with tempfile.TemporaryDirectory(prefix='epco-candidate-download-') as temporary:
        downloaded = Path(temporary)
        command('gh', 'release', 'download', record['tag_name'], '--repo', REPOSITORY, '--dir', str(downloaded))
        inventory = {entry['name']: entry for entry in record['assets']}
        for name in (CHECKPOINT, CHECKPOINT_BUNDLE):
            require(inventory[name].get('digest') == 'sha256:' + checksum(asset_path(downloaded, name)), 'candidate checkpoint GitHub asset digest mismatch')
        command('cosign', 'verify-blob', *policy(expected['stream'], expected['tag'], expected['source_sha']),
                '--bundle', str(downloaded / CHECKPOINT_BUNDLE), str(downloaded / CHECKPOINT))
        extracted = downloaded / 'verified'
        with tarfile.open(downloaded / CHECKPOINT, 'r:gz') as archive:
            members = archive.getmembers()
            names = set()
            total = 0
            for member in members:
                path = Path(member.name)
                require(not path.is_absolute() and '..' not in path.parts and path.parts and path.parts[0] == 'candidate'
                        and member.name not in names and (member.isfile() or member.isdir()), 'unsafe candidate checkpoint archive')
                names.add(member.name)
                total += member.size
            require(len(members) <= 2000 and total <= 100 * 1024 * 1024, 'candidate checkpoint exceeds size limits')
            archive.extractall(extracted, filter='data')
        candidate = extracted / 'candidate'
        manifest = json.loads((candidate / 'manifest.json').read_text())
        require(all(manifest.get(key) == expected.get(key) for key in ('repository', 'stream', 'tag', 'source_sha')), 'candidate checkpoint coordinates mismatch')
        for key in ('image', 'chart'):
            if key in expected:
                require(all(manifest.get(key, {}).get(field) == value for field, value in expected[key].items()), 'candidate checkpoint subject identity mismatch')
        validate(manifest, candidate, manifest['stream'])
        tag_source(manifest)
        registry_proof(manifest, candidate)
        directory.mkdir(parents=True, exist_ok=True)
        require(not any(directory.iterdir()), 'candidate recovery destination must be empty')
        for source in candidate.iterdir():
            require(source.is_file(), 'unexpected candidate checkpoint subdirectory')
            (directory / source.name).write_bytes(source.read_bytes())
        for name in (CHECKPOINT, CHECKPOINT_BUNDLE):
            (directory / name).write_bytes((downloaded / name).read_bytes())
        return manifest


def original_image(manifest, directory):
    image = manifest['image']
    record = release(image['release_tag'])
    require(record is not None, 'selected immutable image release does not exist')
    original = download_release(record, directory)
    require(original['stream'] == 'image' and original['image']['version'] == image['version'], 'selected image release identity mismatch')
    for field, value in image.items():
        require(original['image'].get(field) == value, 'selected image differs from its immutable original release: ' + field)
    return original['image']


def runtime_proof(manifest):
    proof = manifest.get('runtime_evidence', {})
    require(isinstance(proof.get('run_id'), int) and isinstance(proof.get('artifact_id'), int), 'trusted runtime run/artifact coordinates required')
    endpoint = 'actions/runs/' + str(proof['run_id'])
    attempt = proof.get('run_attempt')
    require(attempt is None or isinstance(attempt, int) and attempt > 0, 'invalid recorded runtime attempt')
    run = api(endpoint + ('/attempts/' + str(attempt) if attempt else ''))
    require(attempt is None or run.get('run_attempt') == attempt, 'runtime attempt ownership mismatch')
    trusted_run(run, manifest['source_sha'], 'release-' + manifest['stream'] + '.yml', manifest['tag'])
    successful_jobs(run, ['Runtime acceptance'])
    artifacts = api(f'actions/runs/{run["id"]}/artifacts?per_page=100').get('artifacts', [])
    matches = [entry for entry in artifacts if entry.get('id') == proof['artifact_id']]
    require(len(matches) == 1 and matches[0].get('name') == ('release-runtime-' + str(attempt) if attempt else 'release-runtime') and matches[0].get('expired') is False
            and matches[0].get('workflow_run', {}).get('id') == run['id']
            and matches[0].get('workflow_run', {}).get('head_sha') == manifest['source_sha'], 'runtime artifact ownership mismatch')
    data = api(f'actions/artifacts/{proof["artifact_id"]}/zip', binary=True)
    with zipfile.ZipFile(io.BytesIO(data)) as archive:
        require(archive.namelist() == ['release-runtime.json'], 'unexpected runtime artifact contents')
        payload = json.loads(archive.read('release-runtime.json'))
    expected = {'repository': REPOSITORY, 'stream': manifest['stream'], 'tag': manifest['tag'], 'source_sha': manifest['source_sha'],
                'image_source_sha': manifest['image']['source_sha'], 'image_index_digest': manifest['image']['index_digest'],
                'chart_digest': manifest.get('chart', {}).get('digest'), 'subjects': subjects(manifest),
                'gates': {'platforms': 'success', 'runtime': 'success', 'lifecycle': 'success', 'pg15-18': 'success'}}
    require(all(payload.get(key) == value for key, value in expected.items()), 'runtime evidence does not bind accepted subjects/source/gates')
    sanitized = dict(expected)
    if 'observed_runtime' in payload:
        sanitized['observed_runtime'] = [clean_installed(report, manifest) for report in payload['observed_runtime']]
        require(len(sanitized['observed_runtime']) == 8 and {(report['observations'][0]['architecture'], report['pg_major'])
                for report in sanitized['observed_runtime']} == {(arch, str(pg)) for arch in ('amd64', 'arm64') for pg in (15, 16, 17, 18)}, 'durable measured runtime matrix is incomplete')
    if 'observed_lifecycle' in payload:
        sanitized['observed_lifecycle'] = clean_lifecycle(payload['observed_lifecycle'], manifest)
    return sanitized


def clean_installed(report, manifest):
    require(report.get('source_sha') == manifest['source_sha'] and report.get('image_source_sha') == manifest['image']['source_sha']
            and report.get('subjects') == subjects(manifest), 'measured runtime source/subjects mismatch')
    context = report.get('context', '')
    uid = report.get('leaseUID', '')
    require(isinstance(context, str) and re.fullmatch(r'kind-epco-e2e-[a-z0-9-]+', context), 'runtime fixture context is not owned')
    require(isinstance(uid, str) and re.fullmatch(r'[A-Za-z0-9-]{1,128}', uid), 'observed leader Lease UID is required')
    observations = report.get('observations', [])
    require(isinstance(observations, list) and observations, 'measured runtime Pod observations missing')
    clean = []
    for observation in observations:
        architecture = observation.get('architecture')
        platform = next((entry for entry in manifest['image']['platforms'] if entry['architecture'] == architecture), None)
        image_id = observation.get('imageID', '')
        measured = re.search(r'sha256:[a-f0-9]{64}$', image_id) if isinstance(image_id, str) else None
        pod_uid = observation.get('podUID', '')
        require(platform is not None and measured and measured[0] in (platform['digest'], manifest['image']['index_digest'])
                and observation.get('platform_digest') == platform['digest'], 'observed Pod imageID/platform digest mismatch')
        require(isinstance(pod_uid, str) and re.fullmatch(r'[A-Za-z0-9-]{1,128}', pod_uid), 'observed Pod UID is required')
        clean.append({'imageID': IMAGE + '@' + measured[0], 'architecture': architecture, 'platform_digest': platform['digest'], 'podUID': pod_uid})
    require(len({entry['architecture'] for entry in clean}) == 1, 'ambiguous measured runtime architecture')
    major = report.get('pg_major')
    require(major in ('15', '16', '17', '18'), 'invalid observed PostgreSQL major')
    text = report.get('fixture_observed', '')
    require(isinstance(text, str) and len(text) <= 8192, 'bounded measured server/client versions required')
    server = re.search(r'(?m)^server (?:(?:observed|actual) )?version:\s*(?:PostgreSQL )?([0-9]+\.[0-9]+)(?:\s|$)', text)
    client = re.search(r'(?m)^client (?:(?:observed|actual) )?version:\s*(?:psql \(PostgreSQL\) )?([0-9]+\.[0-9]+)\s*$', text)
    require(server and server[1].split('.')[0] == major and client and client[1] == '16.15', 'measured server/client versions do not match runtime fixture')
    number = re.search(r'(?m)^server_version_num:\s*([0-9]+)\s*$', text)
    require(number is None or int(number[1]) // 10000 == int(major), 'measured server version number mismatch')
    return {'source_sha': manifest['source_sha'], 'image_source_sha': manifest['image']['source_sha'], 'subjects': subjects(manifest),
            'pg_major': major, 'context': context, 'observations': clean, 'leaseUID': uid,
            'fixture_observed': f'server observed version: {server[1]}\nclient observed version: {client[1]}\n'}


def clean_lifecycle(report, manifest):
    require(report.get('scope') == 'selected artifact lifecycle' and report.get('imageSourceSHA') == manifest['image']['source_sha']
            and report.get('chartSourceSHA') == manifest['source_sha']
            and report.get('imageReference') == IMAGE + ':' + manifest['image']['version'] + '@' + manifest['image']['index_digest'], 'lifecycle identity mismatch')
    archive_hash = report.get('chartArchiveSHA256', '')
    require(isinstance(archive_hash, str) and re.fullmatch(r'[a-f0-9]{64}', archive_hash), 'invalid observed chart archive digest')
    allowed = {entry['digest'] for entry in manifest['image']['platforms']} | {manifest['image']['index_digest']}
    gates = []
    psa_gate = 'restricted namespace admits healthy manager and rejects privileged Pod'
    cni_gate = 'NetworkPolicy actual CNI allow-deny-allow and node probe continuity'
    known = {'CR readiness, real application SQL and credential controls', 'second release refused in same and other namespace',
             'same-UID removed named-table grant revoked in real SQL; unrelated access preserved',
             'unknown schema refused before schema or manager writes; known schema reinstalls',
             'same-package Helm upgrade and controller revision rollback mechanics', 'live, terminating and real list-denied retirement refusal',
             'delivery lifecycle complete', 'manager readiness and selected image',
             'kustomize stop retains API, CRs, durable histories, credentials and SQL',
             'installer stop retains API, CRs, durable histories, credentials and SQL',
             'live-CR retirement refuses with no resource loss', 'real RBAC list denial retains every CRD and application object',
             psa_gate, cni_gate}
    for entry in report.get('gates', []):
        if entry.get('gate') not in known:
            continue
        require(entry.get('result') == 'passed', 'lifecycle observation failed')
        record = {'gate': entry['gate'], 'result': 'passed'}
        if entry['gate'] == psa_gate:
            require(entry.get('admission') == 'actual API server', 'restricted Pod Security admission must be observed at the API server')
            record['admission'] = 'actual API server'
        if entry['gate'] == cni_gate:
            images = entry.get('cniDaemonImages')
            reference = (r'(?:[a-z0-9]+(?:[._-][a-z0-9]+)*(?::[0-9]{1,5})?/)?'
                         r'(?:[a-z0-9]+(?:[._-][a-z0-9]+)*/)*[a-z0-9]+(?:[._-][a-z0-9]+)*'
                         r'(?::[A-Za-z0-9_][A-Za-z0-9_.-]{0,127})?(?:@sha256:[a-f0-9]{64})?')
            require(isinstance(images, list) and 1 <= len(images) <= 16
                    and all(isinstance(value, str) and len(value) <= 512 and re.fullmatch(reference, value) for value in images),
                    'bounded actual CNI daemon image references are required')
            record['cniDaemonImages'] = list(images)
            for field, result in (('metricsSameNamespace', 'allowed'), ('metricsOtherNamespace', 'denied'), ('podHealthPort', 'denied')):
                require(entry.get(field) == result, 'actual CNI ingress observation mismatch: ' + field)
                record[field] = result
        if 'podImageID' in entry:
            match = re.search(r'sha256:[a-f0-9]{64}$', entry['podImageID'])
            require(match and match[0] in allowed, 'lifecycle imageID mismatch')
            record['podImageID'] = IMAGE + '@' + match[0]
        if 'leaseUID' in entry:
            require(re.fullmatch(r'[A-Za-z0-9-]{1,128}', entry['leaseUID']), 'invalid lifecycle Lease UID')
            record['leaseUID'] = entry['leaseUID']
        if entry.get('privateComparison') == 'passed':
            record['privateComparison'] = 'passed'
        gates.append(record)
    for name in (psa_gate, cni_gate):
        require(sum(entry['gate'] == name for entry in gates) == 1, 'exactly one successful actual security observation is required: ' + name)
    return {'scope': 'selected artifact lifecycle', 'imageSourceSHA': manifest['image']['source_sha'], 'chartSourceSHA': manifest['source_sha'],
            'imageReference': IMAGE + ':' + manifest['image']['version'] + '@' + manifest['image']['index_digest'],
            'chartArchiveSHA256': archive_hash, 'gates': gates,
            'rollbackScope': 'same-package revision mechanics; no cross-version compatibility claim'}


def refresh_inventory(manifest, directory):
    for entry in manifest['assets']:
        if entry['kind'] != 'evidence':
            entry['sha256'] = checksum(asset_path(directory, entry['path']))
    evidence = {**manifest, 'assets': [entry for entry in manifest['assets'] if entry['kind'] != 'evidence']}
    (directory / 'release-evidence.json').write_text(json.dumps(evidence, sort_keys=True, indent=2) + '\n')
    for entry in manifest['assets']:
        entry['sha256'] = checksum(asset_path(directory, entry['path']))
    (directory / manifest['checksum_manifest']).write_text(''.join(entry['sha256'] + '  ' + entry['path'] + '\n' for entry in manifest['assets']))


def publish(manifest, directory, existing, canonical=False):
    checkpoint = existing is not None and checkpoint_record(existing) and len(existing['assets']) == 2
    if existing is not None and not checkpoint and not canonical:
        with tempfile.TemporaryDirectory(prefix='epco-release-canonical-') as temporary:
            saved = download_release(existing, Path(temporary), expected={key: manifest[key] for key in ('repository', 'stream', 'tag', 'source_sha', 'image', 'chart') if key in manifest}, locked=False)
            # Finalized runtime coordinates and signatures are a durable record.
            # A rerun's freshly bound artifact cannot replace attached bytes.
            return publish(saved, Path(temporary), existing, canonical=True)
    measured = runtime_proof(manifest)
    tag_source(manifest)
    stream = manifest['stream']
    targets = [manifest['chart']] if stream == 'chart' else [manifest['image'], *manifest['image']['platforms']]
    repository = CHART if stream == 'chart' else IMAGE
    if existing is None:
        for target in targets:
            command('cosign', 'sign', '--yes', '--bundle', str(directory / target['bundle']), repository + '@' + target.get('index_digest', target.get('digest')))
    elif checkpoint:
        with tempfile.TemporaryDirectory(prefix='epco-release-checkpoint-') as temporary:
            saved = download_checkpoint(existing, Path(temporary), {key: manifest[key] for key in ('repository', 'stream', 'tag', 'source_sha', 'image', 'chart') if key in manifest})
            require(saved['assets'] == manifest['assets'], 'candidate inventory differs from durable checkpoint')
            for name in (CHECKPOINT, CHECKPOINT_BUNDLE):
                (directory / name).write_bytes((Path(temporary) / name).read_bytes())
                manifest['assets'].append({'path': name, 'kind': 'candidate_checkpoint', 'sha256': checksum(directory / name)})
    else:
        # Re-signing generates different certificate/bundle bytes. A complete
        # verified draft is the durable checkpoint and must remain byte-identical.
        with tempfile.TemporaryDirectory(prefix='epco-release-resume-') as temporary:
            download_release(existing, Path(temporary), expected=manifest, locked=False)
    if existing is None or checkpoint:
        manifest['measured_runtime'] = measured
        runtime_asset = directory / 'release-runtime.json'
        runtime_asset.write_text(json.dumps(measured, sort_keys=True, indent=2) + '\n')
        manifest['assets'].append({'path': runtime_asset.name, 'kind': 'runtime_evidence', 'sha256': checksum(runtime_asset)})
        refresh_inventory(manifest, directory)
        command('cosign', 'sign-blob', '--yes', '--bundle', str(directory / manifest['checksum_bundle']), str(directory / manifest['checksum_manifest']))
    validate(manifest, directory, stream)
    registry_proof(manifest, directory)
    # The token-bearing registry session cannot establish public availability.
    with tempfile.TemporaryDirectory(prefix='epco-release-anonymous-') as temporary:
        env = {**os.environ, 'DOCKER_CONFIG': temporary, 'HELM_REGISTRY_CONFIG': temporary + '/helm.json'}
        registry_proof(manifest, directory, env=env)
    names = {entry['path'] for entry in manifest['assets']} | {manifest['checksum_manifest'], manifest['checksum_bundle']}
    if existing is None:
        command('gh', 'release', 'create', manifest['tag'], '--repo', REPOSITORY, '--verify-tag', '--draft', '--title', manifest['tag'], '--notes', 'Verified EPCO ' + stream + ' release.')
        existing = release(manifest['tag'])
    require(existing.get('draft') is True, 'existing release is not a draft')
    remote = {entry['name']: entry for entry in existing.get('assets', [])}
    require(set(remote).issubset(names), 'draft has unexpected assets; refusing overwrite')
    for name, entry in remote.items():
        require(entry.get('digest') == 'sha256:' + checksum(directory / name), 'draft asset differs; refusing overwrite: ' + name)
    missing = sorted(names - set(remote))
    if missing:
        command('gh', 'release', 'upload', manifest['tag'], *[str(directory / name) for name in missing], '--repo', REPOSITORY)
    with tempfile.TemporaryDirectory(prefix='epco-release-draft-') as temporary:
        download_release(release(manifest['tag']), Path(temporary), expected=manifest, locked=False)
    tag_source(manifest)
    edit = ['gh', 'release', 'edit', manifest['tag'], '--repo', REPOSITORY, '--draft=false', '--latest', str(manifest['make_latest']).lower(), '--prerelease=' + str(manifest['prerelease']).lower()]
    command(*edit)
    with tempfile.TemporaryDirectory(prefix='epco-release-locked-') as temporary:
        download_release(release(manifest['tag']), Path(temporary), expected=manifest)
    return {**manifest, 'completed': True}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('stream', choices=('image', 'chart'))
    parser.add_argument('--manifest', type=Path, required=True)
    parser.add_argument('--phase', choices=('validate', 'preflight', 'verify', 'publish'), required=True)
    args = parser.parse_args()
    path = args.manifest.resolve()
    manifest = json.loads(path.read_text())
    coordinates(manifest, args.stream)
    if args.phase == 'validate':
        output = validate(manifest, path.parent, args.stream)
    else:
        existing = release(manifest['tag'])
        if existing and existing.get('draft') is not True:
            with tempfile.TemporaryDirectory(prefix='epco-release-existing-') as temporary:
                output = {**download_release(existing, Path(temporary), expected=manifest), 'completed': True}
        else:
            source_gates(manifest)
            if existing and existing.get('draft') is True and checkpoint_record(existing) and args.phase == 'preflight':
                with tempfile.TemporaryDirectory(prefix='epco-preflight-checkpoint-') as temporary:
                    manifest = download_checkpoint(existing, Path(temporary), manifest)
            if args.stream == 'chart':
                with tempfile.TemporaryDirectory(prefix='epco-release-image-') as temporary:
                    manifest['image'] = original_image(manifest, Path(temporary))
            if args.phase == 'preflight':
                reference = (IMAGE if args.stream == 'image' else CHART) + ':' + version(manifest['tag'].removeprefix('chart-v').removeprefix('v'))
                require(command('crane', 'digest', reference, allow_absent=True) is None or existing is not None, 'version tag already exists without a recoverable draft; refusing overwrite')
                output = {**manifest, 'completed': False}
            else:
                validate(manifest, path.parent, args.stream)
                registry_proof(manifest, path.parent)
                output = publish(manifest, path.parent, existing) if args.phase == 'publish' else {**manifest, 'completed': False}
    print(json.dumps(output, sort_keys=True))


if __name__ == '__main__':
    try:
        main()
    except (ValueError, KeyError, TypeError, OSError, tarfile.TarError, zipfile.BadZipFile, subprocess.TimeoutExpired) as error:
        sys.exit(str(error))
