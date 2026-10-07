#!/usr/bin/env bash
set -euo pipefail
cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.."
mkdir -p bin
# Include the toolchain and base binary to invalidate cached plugin builds.
fingerprint="$(cat .custom-gcl.yml; go version; golangci-lint version)"
if [[ ! -x bin/golangci-lint-custom || ! -f bin/golangci-lint-custom.stamp ]] || [[ "$(cat bin/golangci-lint-custom.stamp)" != "$fingerprint" ]]; then
  target="$(mktemp -d "${TMPDIR:-/tmp}/epco-linter.XXXXXXXX")"
  trap 'rm -rf -- "$target"' EXIT
  golangci-lint custom --destination "$target" --name golangci-lint-custom
  [[ -x "$target/golangci-lint-custom" ]]
  mv -- "$target/golangci-lint-custom" bin/golangci-lint-custom
  printf '%s\n' "$fingerprint" > bin/golangci-lint-custom.stamp
fi
if [[ "${1:-}" == build ]]; then exit 0; fi
exec bin/golangci-lint-custom "$@"
