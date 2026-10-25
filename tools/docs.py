#!/usr/bin/env python3
"""Generate the Go-source API reference and check repository Markdown links."""

import argparse
from pathlib import Path
import re
import subprocess
import sys
from tempfile import TemporaryDirectory
from urllib.parse import unquote, urlsplit


REFERENCE = Path('docs/reference/api.md')
CONFIG = Path('tools/crd-ref-docs/config.yaml')
TEMPLATES = Path('tools/crd-ref-docs/markdown')


def expected_reference(root):
    """Render Go API source through the pinned upstream crd-ref-docs binary."""
    root = Path(root).resolve()
    with TemporaryDirectory(prefix='epco-crd-ref-docs-') as temporary:
        output = Path(temporary) / 'api.md'
        command = [
            'crd-ref-docs', '--source-path', str(root / 'api'),
            '--config', str(root / CONFIG), '--renderer', 'markdown',
            '--templates-dir', str(root / TEMPLATES),
            '--output-path', str(output),
        ]
        subprocess.run(command, cwd=root, check=True, timeout=180)
        # Upstream formats the NUL escape in Pattern markers as a literal NUL byte.
        return output.read_text().replace('\x00', r'\x00').rstrip('\n') + '\n'


def check_api_reference(root):
    """Report missing or stale output without changing any file."""
    expected = expected_reference(root)
    target = Path(root) / REFERENCE
    if not target.is_file():
        return [f'{REFERENCE}: missing; run mise run docs-generate']
    if target.read_text() != expected:
        return [f'{REFERENCE}: stale; run mise run docs-generate']
    return []


def prose(text):
    """Remove fenced and inline code before interpreting Markdown links."""
    lines = []
    fence = None
    for line in text.splitlines():
        match = re.match(r'^\s{0,3}(`{3,}|~{3,})', line)
        if match:
            marker = match[1]
            if fence is None:
                fence = marker
            elif marker[0] == fence[0] and len(marker) >= len(fence):
                fence = None
            lines.append('')
        else:
            lines.append(line if fence is None else '')
    return re.sub(r'(`+).*?\1', '', '\n'.join(lines), flags=re.DOTALL)


def anchors(text):
    result = set()
    counts = {}
    for heading in re.findall(r'^\s{0,3}#{1,6}\s+(.+?)\s*#*\s*$', text, re.MULTILINE):
        heading = re.sub(r'\[([^]]+)\]\([^)]*\)', r'\1', heading)
        heading = re.sub(r'<[^>]+>', '', heading).lower()
        slug = re.sub(r'[^\w\- ]', '', heading).replace(' ', '-')
        count = counts.get(slug, 0)
        counts[slug] = count + 1
        result.add(slug + (f'-{count}' if count else ''))
    result.update(re.findall(r'<a\s+(?:name|id)=["\']([^"\']+)', text, re.IGNORECASE))
    return result


def check_markdown_links(root):
    """Check local inline and reference links in README, docs and examples."""
    root = Path(root).resolve()
    sources = {root / 'README.md'}
    for folder in ('docs', 'examples'):
        sources.update((root / folder).rglob('*.md'))
    diagnostics = []
    for source in sorted(path for path in sources if path.is_file()):
        text = prose(source.read_text())
        references = {name.strip().lower(): target for name, target in re.findall(
            r'^\s{0,3}\[([^]]+)\]:\s*(<[^>]+>|\S+)', text, re.MULTILINE)}
        targets = re.findall(r'\[[^]\n]*\]\(\s*(<[^>]+>|[^\s)]+)(?:\s+["\'][^\n]*?["\'])?\s*\)', text)
        for label, name in re.findall(r'\[([^]\n]+)\]\[([^]\n]*)\]', text):
            reference = (name or label).strip().lower()
            if reference in references:
                targets.append(references[reference])
            else:
                diagnostics.append(f'{source.relative_to(root)}: undefined link reference [{reference}]')
        targets.extend(references.values())
        for target in targets:
            target = target.removeprefix('<').removesuffix('>')
            parsed = urlsplit(target)
            if parsed.scheme or parsed.netloc:
                continue
            destination = (source.parent / unquote(parsed.path)).resolve() if parsed.path else source
            if not destination.exists():
                diagnostics.append(f'{source.relative_to(root)}: missing local link target {target}')
            elif parsed.fragment and destination.suffix.lower() == '.md':
                if unquote(parsed.fragment) not in anchors(destination.read_text()):
                    diagnostics.append(f'{source.relative_to(root)}: missing Markdown fragment {target}')
    return sorted(set(diagnostics))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('action', choices=('generate', 'check'))
    args = parser.parse_args()
    root = Path(__file__).resolve().parents[1]
    if args.action == 'generate':
        content = expected_reference(root)
        (root / REFERENCE).parent.mkdir(parents=True, exist_ok=True)
        (root / REFERENCE).write_text(content)
        return
    diagnostics = check_api_reference(root) + check_markdown_links(root)
    for diagnostic in diagnostics:
        print(diagnostic, file=sys.stderr)
    if diagnostics:
        sys.exit(1)
    print('API reference and local Markdown links match.')


if __name__ == '__main__':
    main()
