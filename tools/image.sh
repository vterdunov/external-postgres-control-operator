#!/usr/bin/env bash
set -euo pipefail
cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.."
container_tool="${CONTAINER_TOOL:-docker}"
image="${IMG:-controller:dev}"
args=()
if [[ -n "${BASE_IMAGE:-}" ]]; then args+=(--build-arg "BASE_IMAGE=$BASE_IMAGE"); fi
args+=(--build-arg "VERSION=${VERSION:-dev}" --build-arg "REVISION=${REVISION:-$(git rev-parse HEAD 2>/dev/null || printf unknown)}")
case "${1:-}" in
  build) exec "$container_tool" build ${args[@]+"${args[@]}"} -t "$image" . ;;
  push) exec "$container_tool" push "$image" ;;
  buildx)
    builder="epco-builder-$$"
    cleanup() {
      local status=$?
      trap - EXIT
      "$container_tool" buildx rm "$builder" || true
      exit "$status"
    }
    trap cleanup EXIT
    buildkit_image=$(python3 tools/image_mirror.py reference buildkit)
    "$container_tool" buildx create --name "$builder" --use --driver-opt "image=$buildkit_image"
    "$container_tool" buildx build --builder "$builder" --push --platform="${PLATFORMS:-linux/amd64,linux/arm64}" ${args[@]+"${args[@]}"} --tag "$image" -f Dockerfile .
    ;;
  *) echo 'Expected build, push or buildx' >&2; exit 2 ;;
esac
