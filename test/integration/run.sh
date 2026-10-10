#!/usr/bin/env bash
set -euo pipefail
# Give each child its own process group, including compiler descendants.
set -m

suite_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
project_dir="$(cd -- "$suite_dir/../.." && pwd)"
run_id="epco-integration-$(date +%s)-$$-${RANDOM}"
artifacts="${EPCO_ARTIFACTS_DIR:-$project_dir/artifacts/integration/$run_id}"
mkdir -p -- "$artifacts"
artifacts="$(cd -- "$artifacts" && pwd)"
export EPCO_TEST_RUN_ID="$run_id" EPCO_ARTIFACTS_DIR="$artifacts"
export EPCO_PG_VERSION="${EPCO_PG_VERSION:-16}"
binary_dir="$(mktemp -d "${TMPDIR:-/tmp}/epco-integration-bin.XXXXXX")"
export EPCO_TEST_TMPDIR="$binary_dir"
child_pid=""

# Bound Docker cleanup even if the daemon stops responding.
run_bounded() {
  local command_pid watchdog_pid result=0
  "$@" &
  command_pid=$!
  (
    sleep 15
    kill -TERM -- "-$command_pid" 2>/dev/null || true
    sleep 1
    kill -KILL -- "-$command_pid" 2>/dev/null || true
  ) >/dev/null 2>&1 &
  watchdog_pid=$!
  wait "$command_pid" || result=$?
  # The command may exit on TERM before descendants do. Always reap its group.
  kill -KILL -- "-$command_pid" 2>/dev/null || true
  kill -KILL -- "-$watchdog_pid" 2>/dev/null || true
  wait "$watchdog_pid" 2>/dev/null || true
  return "$result"
}

cleanup() {
  local status=$?
  trap - EXIT INT TERM
  local containers
  # Command substitutions disable Bash process groups on Linux, even with
  # monitor mode enabled. Capture through a file so watchdog groups stay usable.
  if ! run_bounded docker ps -aq --filter "label=epco.test.run=$run_id" > "$binary_dir/containers" 2>/dev/null; then
    if [[ "$status" -eq 0 ]]; then status=1; fi
  fi
  containers="$(cat -- "$binary_dir/containers")"
  if [[ -n "$containers" ]]; then
    while IFS= read -r container; do
      if [[ "$status" -ne 0 && -x "$binary_dir/integration.test" ]]; then
        EPCO_TEST_DIAGNOSTICS_ID="$container" run_bounded "$binary_dir/integration.test" >/dev/null 2>&1 || true
      fi
      if ! run_bounded docker rm --force --volumes "$container" >/dev/null 2>&1 && [[ "$status" -eq 0 ]]; then
        status=1
      fi
    done <<< "$containers"
  fi
  rm -rf -- "$binary_dir"
  exit "$status"
}
interrupt() {
  local code="$1" iteration
  trap - INT TERM
  if [[ -n "$child_pid" ]]; then
    kill -TERM -- "-$child_pid" 2>/dev/null || true
    for ((iteration=0; iteration<20; iteration++)); do
      if ! kill -0 "$child_pid" 2>/dev/null; then break; fi
      sleep 0.25
    done
    kill -KILL -- "-$child_pid" 2>/dev/null || true
    wait "$child_pid" 2>/dev/null || true
  fi
  exit "$code"
}
trap cleanup EXIT
trap 'interrupt 130' INT
trap 'interrupt 143' TERM

cd -- "$project_dir"
if [[ -z "${KUBEBUILDER_ASSETS:-}" ]]; then
  mise exec -- setup-envtest use 1.37.0 -p path --bin-dir "$project_dir/bin" > "$binary_dir/envtest-assets" &
  child_pid=$!
  wait "$child_pid"
  child_pid=""
  KUBEBUILDER_ASSETS="$(cat -- "$binary_dir/envtest-assets")"
  export KUBEBUILDER_ASSETS
fi
# Compile separately so runtime signals reach the test process directly.
mise exec -- go test -c -race -mod=readonly -tags=integration -o "$binary_dir/integration.test" ./test/integration &
child_pid=$!
wait "$child_pid"
child_pid=""
"$binary_dir/integration.test" -test.v -test.timeout=10m -ginkgo.randomize-all -ginkgo.seed="${EPCO_TEST_SEED:-$(date +%s)}" -ginkgo.junit-report="$artifacts/junit.xml" "$@" &
child_pid=$!
wait "$child_pid"
