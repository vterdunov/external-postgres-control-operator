#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/../.."
artifacts="${EPCO_ARTIFACTS_DIR:-$PWD/artifacts/unit}"
mkdir -p "$artifacts"
seed="${EPCO_TEST_SEED:-$(date +%s)}"
printf 'Ginkgo seed: %s\n' "$seed"
go run -mod=readonly github.com/onsi/ginkgo/v2/ginkgo \
  --timeout=5m --race --randomize-all --seed="$seed" --keep-going \
  --output-dir="$artifacts" --junit-report=junit.xml --json-report=report.json \
  ./internal/controller ./internal/postgres ./test/utils
