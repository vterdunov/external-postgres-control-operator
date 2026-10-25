#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/../.."
artifacts="${EPCO_ARTIFACTS_DIR:-$PWD/artifacts/generated}"
mkdir -p "$artifacts"
exec > >(tee "$artifacts/check.log") 2>&1
controller_gen="$(mise which controller-gen)"
root="$PWD"
target="$(mktemp -d "${TMPDIR:-/tmp}/epco-generated.XXXXXXXX")"
trap 'rm -rf -- "$target"' EXIT
python3 - "$root" "$target" <<'PY'
from pathlib import Path
import shutil
import sys
root, target = map(Path, sys.argv[1:])
shutil.copytree(root, target, dirs_exist_ok=True,
                ignore=shutil.ignore_patterns('.git', 'bin', 'artifacts', '.agent-plans'))
PY
(cd "$target" && "$controller_gen" rbac:roleName=manager-role crd webhook paths='./...' output:crd:artifacts:config=config/crd/bases && "$controller_gen" object paths='./...' && python3 tools/docs.py generate)
python3 tools/chart.py check
python3 - "$root" "$target" <<'PY'
from pathlib import Path
import sys
root, target = map(Path, sys.argv[1:])
patterns = ('**/zz_generated*.go', 'config/crd/bases/*.yaml', 'config/rbac/role.yaml', 'config/webhook/manifests.yaml', 'docs/reference/api.md')
files = set()
for pattern in patterns:
    files.update(p.relative_to(root) for p in root.glob(pattern))
    files.update(p.relative_to(target) for p in target.glob(pattern))
changed = [str(p) for p in sorted(files) if not (root/p).is_file() or not (target/p).is_file() or (root/p).read_bytes() != (target/p).read_bytes()]
if changed:
    print('Generated files differ: ' + ', '.join(changed), file=sys.stderr)
    sys.exit(1)
print('Generated files match; source tree untouched.')
PY
