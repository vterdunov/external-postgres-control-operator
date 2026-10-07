#!/usr/bin/env bash
set -euo pipefail
cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.."
container_tool="${CONTAINER_TOOL:-docker}"
image="${IMG:-controller:dev}"
args=()
if [[ -n "${BASE_IMAGE:-}" ]]; then args+=(--build-arg "BASE_IMAGE=$BASE_IMAGE"); fi
case "${1:-}" in
  build) exec "$container_tool" build ${args[@]+"${args[@]}"} -t "$image" . ;;
  push) exec "$container_tool" push "$image" ;;
  buildx)
    target="$(mktemp -d "${TMPDIR:-/tmp}/epco-buildx.XXXXXXXX")"
    builder="epco-builder-$$"
    cleanup() {
      local status=$?
      trap - EXIT
      "$container_tool" buildx rm "$builder" || true
      rm -rf -- "$target"
      exit "$status"
    }
    trap cleanup EXIT
    # Preserve the original Dockerfile while targeting the build host.
    sed "s/^FROM .* AS builder$/FROM --platform=\${BUILDPLATFORM} \${BASE_IMAGE} AS builder/" Dockerfile > "$target/Dockerfile"
    "$container_tool" buildx create --name "$builder" --use
    "$container_tool" buildx build --builder "$builder" --push --platform="${PLATFORMS:-linux/arm64,linux/amd64,linux/s390x,linux/ppc64le}" ${args[@]+"${args[@]}"} --tag "$image" -f "$target/Dockerfile" .
    ;;
  *) echo 'Expected build, push or buildx' >&2; exit 2 ;;
esac
