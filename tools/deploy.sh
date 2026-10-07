#!/usr/bin/env bash
set -euo pipefail
cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.."
action="${1:-}"
if [[ "$action" != build-installer ]]; then
  : "${KUBE_CONTEXT:?Set KUBE_CONTEXT explicitly for cluster operations}"
fi
target="$(mktemp -d "${TMPDIR:-/tmp}/epco-deploy.XXXXXXXX")"
trap 'rm -rf -- "$target"' EXIT
cp -R config "$target/config"
case "$action" in
  build-installer|deploy)
    (cd "$target/config/manager" && kustomize edit set image "controller=${IMG:-controller:dev}")
    kustomize build "$target/config/default" > "$target/install.yaml"
    if [[ "$action" == build-installer ]]; then
      mkdir -p dist
      cp "$target/install.yaml" dist/install.yaml
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
      kubectl --context "$KUBE_CONTEXT" delete --ignore-not-found="${IGNORE_NOT_FOUND:-false}" -f "$target/crds.yaml"
    fi
    ;;
  undeploy)
    kustomize build "$target/config/default" > "$target/install.yaml"
    kubectl --context "$KUBE_CONTEXT" delete --ignore-not-found="${IGNORE_NOT_FOUND:-false}" -f "$target/install.yaml"
    ;;
  *) echo 'Expected build-installer, install, uninstall, deploy or undeploy' >&2; exit 2 ;;
esac
