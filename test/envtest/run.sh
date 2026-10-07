#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$repo_root"

if [[ -z "${KUBEBUILDER_ASSETS:-}" ]]; then
  KUBEBUILDER_ASSETS="$(mise exec -- setup-envtest use 1.37.0 -p path --bin-dir "$repo_root/bin")"
  export KUBEBUILDER_ASSETS
fi

exec python3 test/envtest/run.py "$@"
