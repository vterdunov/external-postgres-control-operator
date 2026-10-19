#!/usr/bin/env bash
set -euo pipefail
cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.."
action="${1:-}"
case "$action" in
  build-installer|install|uninstall|deploy|undeploy) ;;
  *) echo 'Expected build-installer, install, uninstall, deploy or undeploy' >&2; exit 2 ;;
esac
if [[ "$action" != build-installer ]]; then
  : "${KUBE_CONTEXT:?Set KUBE_CONTEXT explicitly for cluster operations}"
fi
if [[ "$action" == uninstall && "${ACK_CRD_RETIREMENT:-}" != delete-all-epco-crds ]]; then
  echo 'CRD retirement requires ACK_CRD_RETIREMENT=delete-all-epco-crds' >&2
  exit 1
fi
target="$(mktemp -d "${TMPDIR:-/tmp}/epco-deploy.XXXXXXXX")"
trap 'rm -rf -- "$target"' EXIT
cp -R config "$target/config"
controller_bundle() {
  python3 - "$1" "$2" <<'PY'
from pathlib import Path
import re
import sys
source, destination = map(Path, sys.argv[1:])
allowed = {'Deployment', 'Service', 'ServiceAccount', 'Role', 'RoleBinding', 'ClusterRole', 'ClusterRoleBinding'}
documents = []
for document in re.split(r'(?m)^---\s*$', source.read_text()):
    match = re.search(r'(?m)^kind:\s*(\S+)', document)
    if match and match[1] in allowed:
        documents.append(document.strip() + '\n')
destination.write_text('---\n'.join(documents))
PY
}
case "$action" in
  build-installer|deploy)
    (cd "$target/config/manager" && kustomize edit set image "controller=${IMG:-controller:dev}")
    kustomize build "$target/config/default" > "$target/install.yaml"
    if [[ "$action" == build-installer ]]; then
      mkdir -p dist
      cp "$target/install.yaml" dist/install.yaml
      controller_bundle "$target/install.yaml" dist/uninstall.yaml
    else
      kubectl --context "$KUBE_CONTEXT" apply -f "$target/install.yaml"
    fi
    ;;
  install|uninstall)
    kustomize build "$target/config/crd" > "$target/crds.yaml"
    if [[ ! -s "$target/crds.yaml" ]]; then echo 'No CRDs to process; skipping.'; exit 0; fi
    if [[ "$action" == install ]]; then
      kubectl --context "$KUBE_CONTEXT" apply -f "$target/crds.yaml"
    else
      # A successful empty list for every kind is required before the first write.
      for resource in externalpostgres databaseroles databases; do
        kubectl --context "$KUBE_CONTEXT" get "$resource.postgresql.epco.io" --all-namespaces -o json > "$target/$resource.json"
        python3 - "$target/$resource.json" <<'PY'
from pathlib import Path
import json
import sys
path = Path(sys.argv[1])
try:
    result = json.loads(path.read_text())
    if (not isinstance(result, dict) or not isinstance(result.get('items'), list)
            or not isinstance(result.get('kind'), str) or not result['kind'].endswith('List')
            or not isinstance(result.get('apiVersion'), str) or not result['apiVersion']):
        raise ValueError('expected a Kubernetes JSON list with items')
    if result['items']:
        raise ValueError('custom resources still exist, including terminating resources')
except (ValueError, OSError) as error:
    sys.exit(f'CRD retirement refused for {path.stem}: {error}')
PY
      done
      kubectl --context "$KUBE_CONTEXT" delete --ignore-not-found="${IGNORE_NOT_FOUND:-false}" -f "$target/crds.yaml"
    fi
    ;;
  undeploy)
    kustomize build "$target/config/default" > "$target/install.yaml"
    controller_bundle "$target/install.yaml" "$target/uninstall.yaml"
    kubectl --context "$KUBE_CONTEXT" delete --ignore-not-found="${IGNORE_NOT_FOUND:-false}" -f "$target/uninstall.yaml"
    ;;
  *) echo 'Expected build-installer, install, uninstall, deploy or undeploy' >&2; exit 2 ;;
esac
