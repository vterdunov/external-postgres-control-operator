#!/usr/bin/env bash
set -euo pipefail
suite_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
fixture=$(mktemp -d "${TMPDIR:-/tmp}/epco-e2e-runner-test.XXXXXX")
fixture=$(cd "$fixture" && pwd)
trap 'rm -rf "$fixture"' EXIT
mkdir -p "$fixture/bin" "$fixture/source/config/default" "$fixture/source/test/e2e"
printf 'original manifest\n' > "$fixture/source/config/default/kustomization.yaml"
source_checksum=$(cksum "$fixture/source/config/default/kustomization.yaml")
cp "$suite_dir/database_connections_test.go" "$fixture/source/test/e2e/database_connections_test.go"
go_source_checksum=$(cksum "$fixture/source/test/e2e/database_connections_test.go")
unset EPCO_PG_VERSION EPCO_E2E_BUILD_MODE EPCO_E2E_CLIENT_IMAGE EPCO_E2E_POSTGRES_IMAGE
unset EPCO_E2E_IMAGE_REFERENCE EPCO_E2E_SUITE EPCO_DELIVERY_CHART_PACKAGE EPCO_DELIVERY_IMAGE_REFERENCE
unset EPCO_DELIVERY_IMAGE_SOURCE_SHA EPCO_DELIVERY_CHART_SOURCE_SHA EPCO_DELIVERY_LOCAL_IMAGE
unset EPCO_E2E_INSTALL_MODE
export E2E_FAKE_REAL_PYTHON
E2E_FAKE_REAL_PYTHON=$(python3 -c 'import sys; print(sys.executable)')
export EPCO_TEST_SEED=60211015
export E2E_FAKE_REVISION=0123456789abcdef0123456789abcdef01234567
cat > "$fixture/bin/fake" <<'MOCK'
#!/usr/bin/env bash
set -eu
name=${0##*/}
if [[ $name == python3 && ${1:-} != */delivery/lifecycle.py && ${1:-} != */tools/chart.py ]]; then
  exec "$E2E_FAKE_REAL_PYTHON" "$@"
fi
printf '%s %s\n' "$name" "$*" >> "$E2E_FAKE_LOG"
case "$name $*" in
  'python3 '*'/tools/chart.py install '*)
    printf '%s\n' "$PWD" > "$E2E_FAKE_LOG.chart-cwd" ;;
  'python3 '*'/delivery/lifecycle.py'*)
    [[ ${1:-} == */delivery/lifecycle.py ]] || exit 94
    printf '%s\n' "${EPCO_E2E_CONTEXT:-}" "${KUBECONFIG:-}" \
      "${EPCO_DELIVERY_CHART_PACKAGE:-}" "${EPCO_DELIVERY_IMAGE_REFERENCE:-}" \
      "${EPCO_DELIVERY_IMAGE_SOURCE_SHA:-}" "${EPCO_DELIVERY_CHART_SOURCE_SHA:-}" \
      > "$E2E_FAKE_LOG.delivery-env"
    [[ ${E2E_FAKE_FAIL:-} != delivery ]] || exit 37 ;;
  'kind create cluster'*)
    if [[ ${E2E_FAKE_FAIL:-} == kind ]]; then exit 17; fi
    while [[ $# -gt 0 ]]; do
      if [[ $1 == --name ]]; then [[ $2 =~ ^[a-z0-9.-]+$ ]] || exit 97; fi
      if [[ $1 == --kubeconfig ]]; then
        [[ $(ls -l "$2") == -rw-------* ]] || exit 96
        printf 'private kubeconfig\n' > "$2"; break; fi
      shift
    done ;;
  'kind delete cluster'*)
    if [[ ${E2E_FAKE_FAIL:-} == cleanup || ${E2E_FAKE_FAIL:-} == test-cleanup ]]; then exit 29; fi ;;
  'kind export logs'*)
    if [[ ${E2E_FAKE_FAIL:-} == diagnostics ]]; then exit 27; fi
    for diagnostic_dir; do :; done
    mkdir -p "$diagnostic_dir"
    printf 'password=e2e-secret postgresql://user:e2e-secret@localhost/test Authorization: Bearer aaa.bbb.ccc\n' > "$diagnostic_dir/fixture.log" ;;
  'docker build'*)
    builder_override=false
    previous=''
    for argument in "$@"; do
      if [[ $previous == --build-arg && $argument == BASE_IMAGE=* ||
        $argument == --build-arg=BASE_IMAGE=* ]]; then
        builder_override=true
      fi
      previous=$argument
    done
    if [[ ${!#} == . && $builder_override == true ]]; then
      printf 'FAIL Docker fixture build must use the checked-in Dockerfile builder default\n' >&2
      exit 95
    fi ;;
  'git '*rev-parse*) printf '%s\n' "$E2E_FAKE_REVISION" ;;
  'go env GOARCH') printf 'arm64\n' ;;
  'kubectl '*)
    [[ " $* " == *' --context kind-epco-e2e-'* ]] || exit 99
    [[ " $* " == *' --kubeconfig '* ]] || exit 98
    if [[ ${E2E_FAKE_FAIL:-} == apply && " $* " == *' apply '* ]]; then exit 19; fi
    if [[ " $* " == *' exec '* && " $* " == *client_connection_check_interval* ]]; then
      if [[ ${E2E_FAKE_PG_SETTING:-100} != missing ]]; then
        printf '%s\n' "${E2E_FAKE_PG_SETTING:-100}"
      fi
      exit 0
    fi
    while [[ $# -gt 0 ]]; do
      if [[ $1 == -f && $2 == */postgres.yaml ]]; then
        postgres_image=$(awk '/^[[:space:]]*image:/ { print $2 }' "$2")
        printf '%s\n' "$postgres_image" > "$E2E_FAKE_LOG.server-image"
        "$E2E_FAKE_REAL_PYTHON" - "$2" > "$E2E_FAKE_LOG.server-setting" <<'PY'
import json
import sys

with open(sys.argv[1], encoding="utf-8") as source:
    args = [json.loads(line.split("args:", 1)[1].strip())
            for line in source if line.lstrip().startswith("args: [")]
print("100ms" if args == [["postgres", "-c", "client_connection_check_interval=100ms"]]
      else "unconfigured")
PY
        if [[ $postgres_image != "${E2E_FAKE_POSTGRES_IMAGE:-docker.io/library/postgres:16.15-bookworm}" ]]; then
          printf 'FAIL PostgreSQL fixture image: %s\n' "$postgres_image" >&2
          exit 95
        fi
      fi
      shift
    done ;;
  'kustomize build'*)
    while [[ $# -gt 0 ]]; do
      if [[ $1 == -o ]]; then printf 'apiVersion: v1\nkind: Namespace\nmetadata:\n  name: epco-system\n' > "$2"; break; fi
      shift
    done ;;
  'go test'*)
    printf '%s\n' "$PWD" > "$E2E_FAKE_LOG.behavior-cwd"
    compile_only=false
    previous=''
    overlay=''
    for argument in "$@"; do
      if [[ $previous == -run && $argument == '^$' || $argument == '-run=^$' ]]; then
        compile_only=true
      fi
      if [[ $previous == -overlay ]]; then overlay=$argument; fi
      if [[ $argument == -overlay=* ]]; then overlay=${argument#-overlay=}; fi
      previous=$argument
    done
    if [[ -n $overlay ]]; then
      phase=runtime
      if [[ $compile_only == true ]]; then phase=compile; fi
      cp "$overlay" "$E2E_FAKE_LOG.overlay-$phase"
      "$E2E_FAKE_REAL_PYTHON" - "$overlay" "$E2E_FAKE_LOG.derived-$phase" <<'PY'
import json
from pathlib import Path
import sys

replacement = next(iter(json.loads(Path(sys.argv[1]).read_text())["Replace"].values()))
Path(sys.argv[2]).write_bytes(Path(replacement).read_bytes())
PY
    fi
    if [[ $compile_only == true ]]; then
      printf '%s\0' "$@" > "$E2E_FAKE_LOG.compile-argv"
      printf '%s\n' "$KUBECONFIG" > "$E2E_FAKE_LOG.compile-kubeconfig"
      printf 'password=e2e-secret postgresql://user:e2e-secret@localhost/test Authorization: Bearer aaa.bbb.ccc\n'
      if [[ ${E2E_FAKE_FAIL:-} == compile ]]; then exit 31; fi
      exit 0
    fi
    printf '%s\0' "$@" > "$E2E_FAKE_LOG.argv"
    printf '%s\n' "${EPCO_PG_VERSION:-}" > "$E2E_FAKE_LOG.major"
    printf '%s\n' "${EPCO_E2E_POSTGRES_IMAGE:-}" > "$E2E_FAKE_LOG.server-env"
    printf '%s\n' "${EPCO_E2E_CLIENT_IMAGE:-}" > "$E2E_FAKE_LOG.client-env"
    printf '%s\n' "$EPCO_E2E_CONTEXT" > "$E2E_FAKE_LOG.context"
    printf '%s\n' "$KUBECONFIG" > "$E2E_FAKE_LOG.kubeconfig"
    printf '%s\n' "$EPCO_E2E_ARTIFACTS" > "$E2E_FAKE_LOG.artifacts"
    printf 'password=e2e-secret postgresql://user:e2e-secret@localhost/test Authorization: Bearer aaa.bbb.ccc\n'
    case ${E2E_FAKE_FAIL:-} in
      test|diagnostics|test-cleanup) exit 23 ;;
      signal) kill -TERM "$PPID"; sleep 30 ;;
      interrupt) kill -INT "$PPID"; sleep 30 ;;
    esac ;;
esac
MOCK
chmod +x "$fixture/bin/fake"
for cli in kind kubectl kustomize docker go git python3; do ln -s fake "$fixture/bin/$cli"; done

release_runner_cases() {
  local mode status expected reference package application_source chart_source major patch server_image client_image
  reference="ghcr.io/vterdunov/external-postgres-control-operator:0.1.0@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
  application_source=1111111111111111111111111111111111111111
  chart_source=2222222222222222222222222222222222222222
  package="$fixture/external-postgres-control-operator-0.2.1.tgz"
  printf 'fixture archive dispatched to the lifecycle fake\n' > "$package"
  for mode in prebuilt chart-operator delivery delivery-failure invalid-reference missing-package invalid-image-source invalid-chart-source; do
    export E2E_FAKE_LOG="$fixture/release-$mode.log" E2E_FAKE_FAIL=none
    export EPCO_E2E_PROJECT_DIR="$fixture/source" EPCO_ARTIFACTS_DIR="$fixture/artifacts/release-$mode"
    export EPCO_E2E_BUILD_MODE=prebuilt EPCO_E2E_IMAGE_REFERENCE="$reference"
    export EPCO_DELIVERY_IMAGE_REFERENCE="$reference" EPCO_DELIVERY_CHART_PACKAGE="$package"
    export EPCO_DELIVERY_IMAGE_SOURCE_SHA="$application_source" EPCO_DELIVERY_CHART_SOURCE_SHA="$chart_source"
    unset EPCO_E2E_SUITE EPCO_PG_VERSION E2E_FAKE_POSTGRES_IMAGE EPCO_E2E_INSTALL_MODE
    : > "$E2E_FAKE_LOG"
    expected=0
    case $mode in
      chart-operator) export EPCO_E2E_INSTALL_MODE=chart ;;
      delivery) export EPCO_E2E_SUITE=delivery ;;
      delivery-failure) export EPCO_E2E_SUITE=delivery E2E_FAKE_FAIL=delivery; expected=37 ;;
      invalid-reference) export EPCO_E2E_IMAGE_REFERENCE=ghcr.io/vterdunov/external-postgres-control-operator:0.1.0; expected=2 ;;
      missing-package) export EPCO_E2E_SUITE=delivery EPCO_DELIVERY_CHART_PACKAGE="$fixture/missing.tgz"; expected=2 ;;
      invalid-image-source) export EPCO_E2E_SUITE=delivery EPCO_DELIVERY_IMAGE_SOURCE_SHA=main; expected=2 ;;
      invalid-chart-source) export EPCO_E2E_SUITE=delivery EPCO_DELIVERY_CHART_SOURCE_SHA=main; expected=2 ;;
    esac
    if [[ $expected != 2 ]]; then
      case $mode in
        prebuilt) major=15; patch=15.19 ;;
        chart-operator) major=17; patch=17.11 ;;
        delivery) major=18; patch=18.6 ;;
        delivery-failure) major=16; patch=16.15 ;;
      esac
      server_image="docker.io/library/postgres:$patch-bookworm"
      client_image=docker.io/library/postgres:16.15-bookworm
      export EPCO_PG_VERSION=$major E2E_FAKE_POSTGRES_IMAGE=$server_image
    fi
    set +e
    PATH="$fixture/bin:$PATH" bash "$suite_dir/run.sh" > "$E2E_FAKE_LOG.output" 2>&1
    status=$?
    set -e
    if [[ $status != "$expected" ]]; then
      cat "$E2E_FAKE_LOG.output"
      printf 'FAIL e2e runner: %s expected %s, got %s\n' "$mode" "$expected" "$status" >&2
      return 1
    fi
    if grep -Eq '^docker (build|push|tag|image rm)|^go build |^kind load docker-image' "$E2E_FAKE_LOG"; then
      printf 'FAIL e2e runner: %s rebuilt, loaded or removed a reused public image\n' "$mode" >&2
      return 1
    fi
    if [[ $expected == 2 ]]; then
      if grep -Eq '^(kind|docker|kubectl) ' "$E2E_FAKE_LOG"; then
        printf 'FAIL e2e runner: %s acquired resources before validating release inputs\n' "$mode" >&2
        return 1
      fi
    else
      [[ $(grep -c '^kind delete cluster --name epco-e2e-' "$E2E_FAKE_LOG") == 1 ]] || return 1
      grep -q 'kubectl .* --context kind-epco-e2e-.*postgres.yaml' "$E2E_FAKE_LOG" || return 1
      [[ $(cat "$E2E_FAKE_LOG.server-image") == "$server_image" ]] || return 1
      grep -Fqx "server expected version: $patch" "$EPCO_ARTIFACTS_DIR/fixture-expected.txt" || return 1
      grep -Fqx "server expected image: $server_image" "$EPCO_ARTIFACTS_DIR/fixture-expected.txt" || return 1
      grep -Fqx 'client expected version: 16.15' "$EPCO_ARTIFACTS_DIR/fixture-expected.txt" || return 1
      grep -Fqx "client expected image: $client_image" "$EPCO_ARTIFACTS_DIR/fixture-expected.txt" || return 1
      if [[ $mode == prebuilt || $mode == chart-operator ]]; then
        grep -q '^go test ' "$E2E_FAKE_LOG" || return 1
        [[ $(cat "$E2E_FAKE_LOG.major") == "$major" ]] || return 1
        [[ $(cat "$E2E_FAKE_LOG.server-env") == "$server_image" ]] || return 1
        [[ $(cat "$E2E_FAKE_LOG.client-env") == "$client_image" ]] || return 1
        grep -Fq -- "$reference" "$EPCO_ARTIFACTS_DIR/fixture-expected.txt" || return 1
        if [[ $mode == chart-operator ]]; then
          local chart_command
          chart_command=$(awk '/^python3 .*tools\/chart.py install / { print; exit }' "$E2E_FAKE_LOG")
          for value in "--package $package" '--release epco' '--namespace epco-system' '--context kind-epco-e2e-' "--image-reference $reference"; do
            [[ $chart_command == *"$value"* ]] || { printf 'FAIL e2e runner: chart operator matrix omitted %s\n' "$value" >&2; return 1; }
          done
          if grep -Eq '^kustomize build|kubectl .*operator.yaml' "$E2E_FAKE_LOG"; then
            printf 'FAIL e2e runner: chart operator matrix silently installed Kustomize\n' >&2
            return 1
          fi
          [[ $(cat "$E2E_FAKE_LOG.behavior-cwd") == "$fixture/source" ]] || return 1
        fi
      else
        if grep -Eq '^go test |^kustomize build|kubectl .*operator.yaml' "$E2E_FAKE_LOG"; then
          printf 'FAIL e2e runner: %s installed the default manager or ran the Go suite\n' "$mode" >&2
          return 1
        fi
        [[ -f "$E2E_FAKE_LOG.delivery-env" ]] || return 1
        local recorded=()
        while IFS= read -r value; do recorded+=("$value"); done < "$E2E_FAKE_LOG.delivery-env"
        [[ ${recorded[0]} == kind-epco-e2e-* && -n ${recorded[1]} ]] || return 1
        [[ ${recorded[2]} == "$package" && ${recorded[3]} == "$reference" ]] || return 1
        [[ ${recorded[4]} == "$application_source" && ${recorded[5]} == "$chart_source" ]] || return 1
        local postgres_line lifecycle_line
        postgres_line=$(awk '/kubectl .*rollout status deployment\/postgres/ { print NR; exit }' "$E2E_FAKE_LOG")
        lifecycle_line=$(awk '/^python3 .*delivery\/lifecycle.py/ { print NR; exit }' "$E2E_FAKE_LOG")
        [[ -n $postgres_line && -n $lifecycle_line && $postgres_line -lt $lifecycle_line ]] || return 1
        if [[ $mode == delivery-failure ]]; then
          local diagnostic_line cleanup_line
          diagnostic_line=$(awk '/^kind export logs/ { print NR; exit }' "$E2E_FAKE_LOG")
          cleanup_line=$(awk '/^kind delete cluster/ { print NR; exit }' "$E2E_FAKE_LOG")
          [[ -n $diagnostic_line && $diagnostic_line -lt $cleanup_line ]] || return 1
          grep -Fq 'Exit status 37' "$EPCO_ARTIFACTS_DIR/runner-junit.xml" || return 1
        fi
      fi
    fi
    [[ $(cksum "$fixture/source/config/default/kustomization.yaml") == "$source_checksum" ]] || return 1
    [[ $(cksum "$fixture/source/test/e2e/database_connections_test.go") == "$go_source_checksum" ]] || return 1
    printf 'PASS e2e runner: release %s\n' "$mode"
  done
  unset EPCO_E2E_BUILD_MODE EPCO_E2E_IMAGE_REFERENCE EPCO_E2E_SUITE
  unset EPCO_E2E_INSTALL_MODE
  unset EPCO_DELIVERY_IMAGE_REFERENCE EPCO_DELIVERY_CHART_PACKAGE
  unset EPCO_DELIVERY_IMAGE_SOURCE_SHA EPCO_DELIVERY_CHART_SOURCE_SHA
}

fixture_runner_cases() {
  local mode status expected reference package chart_source application_source
  local rollout_line measurement_line behavior_line failures=0 before_failures
  reference="ghcr.io/vterdunov/external-postgres-control-operator:0.1.0@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
  package="$fixture/external-postgres-control-operator-0.2.1.tgz"
  printf 'fixture archive dispatched to the lifecycle fake\n' > "$package"
  application_source=1111111111111111111111111111111111111111
  chart_source=2222222222222222222222222222222222222222
  for mode in docker prebuilt chart delivery zero missing malformed; do
    before_failures=$failures
    export E2E_FAKE_LOG="$fixture/fixture-$mode.log" E2E_FAKE_FAIL=none
    export EPCO_E2E_PROJECT_DIR="$fixture/source" EPCO_ARTIFACTS_DIR="$fixture/artifacts/fixture-$mode"
    export EPCO_PG_VERSION=18 E2E_FAKE_POSTGRES_IMAGE=docker.io/library/postgres:18.6-bookworm
    export E2E_FAKE_PG_SETTING=100
    unset EPCO_E2E_BUILD_MODE EPCO_E2E_IMAGE_REFERENCE EPCO_E2E_INSTALL_MODE EPCO_E2E_SUITE
    unset EPCO_DELIVERY_CHART_PACKAGE EPCO_DELIVERY_IMAGE_REFERENCE
    unset EPCO_DELIVERY_IMAGE_SOURCE_SHA EPCO_DELIVERY_CHART_SOURCE_SHA
    case $mode in
      prebuilt|chart|delivery|zero|missing|malformed)
        export EPCO_E2E_BUILD_MODE=prebuilt EPCO_E2E_IMAGE_REFERENCE="$reference" ;;
    esac
    case $mode in
      chart|delivery)
        export EPCO_DELIVERY_CHART_PACKAGE="$package" EPCO_DELIVERY_IMAGE_REFERENCE="$reference"
        export EPCO_DELIVERY_IMAGE_SOURCE_SHA="$application_source" EPCO_DELIVERY_CHART_SOURCE_SHA="$chart_source" ;;
    esac
    if [[ $mode == chart ]]; then export EPCO_E2E_INSTALL_MODE=chart; fi
    if [[ $mode == delivery ]]; then export EPCO_E2E_SUITE=delivery; fi
    case $mode in zero) E2E_FAKE_PG_SETTING=0 ;; missing) E2E_FAKE_PG_SETTING=missing ;;
      malformed) E2E_FAKE_PG_SETTING="invalid-setting" ;; esac
    export E2E_FAKE_PG_SETTING
    : > "$E2E_FAKE_LOG"
    set +e
    PATH="$fixture/bin:$PATH" bash "$suite_dir/run.sh" > "$E2E_FAKE_LOG.output" 2>&1
    status=$?
    set -e
    if [[ $mode == zero || $mode == missing || $mode == malformed ]]; then
      if [[ $status == 0 || -f $E2E_FAKE_LOG.argv || -f $E2E_FAKE_LOG.delivery-env ]]; then
        printf 'FAIL fixture %s: invalid measured setting reached the behavior suite (status %s)\n' "$mode" "$status" >&2
        failures=$((failures + 1))
      fi
      if [[ $(grep -c '^kind delete cluster --name epco-e2e-' "$E2E_FAKE_LOG") != 1 ||
            ! -f $EPCO_ARTIFACTS_DIR/runner-junit.xml ]]; then
        printf 'FAIL fixture %s: owned Kind cleanup or failure report missing\n' "$mode" >&2
        failures=$((failures + 1))
      fi
      continue
    fi
    if [[ $status != 0 ]]; then
      printf 'FAIL fixture %s: expected success, got %s\n' "$mode" "$status" >&2
      failures=$((failures + 1))
      continue
    fi
    if [[ ! -f $E2E_FAKE_LOG.server-setting || $(cat "$E2E_FAKE_LOG.server-setting") != 100ms ]]; then
      printf 'FAIL fixture %s: PostgreSQL server args do not configure 100ms\n' "$mode" >&2
      failures=$((failures + 1))
    fi
    rollout_line=$(awk '/kubectl .*rollout status deployment\/postgres/ { print NR; exit }' "$E2E_FAKE_LOG")
    measurement_line=$(awk '/^kubectl .* exec .*client_connection_check_interval/ { print NR; exit }' "$E2E_FAKE_LOG")
    if [[ $mode == delivery ]]; then
      behavior_line=$(awk '/^python3 .*delivery\/lifecycle.py/ { print NR; exit }' "$E2E_FAKE_LOG")
    else
      behavior_line=$(awk '/^go test / && !/-run (\^\$|-run=\^\$)/ { print NR; exit }' "$E2E_FAKE_LOG")
    fi
    if [[ -z $rollout_line || -z $measurement_line || -z $behavior_line ||
          $rollout_line -ge $measurement_line || $measurement_line -ge $behavior_line ]]; then
      printf 'FAIL fixture %s: actual setting must be measured after rollout and before behavior\n' "$mode" >&2
      failures=$((failures + 1))
    fi
    for metadata in "$EPCO_ARTIFACTS_DIR/fixture-expected.txt" "$EPCO_ARTIFACTS_DIR/fixture-settings.txt"; do
      if [[ ! -f $metadata ]] ||
          ! grep -Fqx 'server client connection check interval ms: 100' "$metadata"; then
        printf 'FAIL fixture %s: sanitized setting metadata missing in %s\n' "$mode" "${metadata##*/}" >&2
        failures=$((failures + 1))
      fi
    done
    if [[ $(grep -c '^kind delete cluster --name epco-e2e-' "$E2E_FAKE_LOG") != 1 ]] ||
        grep -REq 'e2e-secret|aaa[.]bbb[.]ccc' "$EPCO_ARTIFACTS_DIR" ||
        [[ $(cksum "$fixture/source/config/default/kustomization.yaml") != "$source_checksum" ||
          $(cksum "$fixture/source/test/e2e/database_connections_test.go") != "$go_source_checksum" ]]; then
      printf 'FAIL fixture %s: cleanup, sanitization, or source immutability\n' "$mode" >&2
      failures=$((failures + 1))
    fi
    if ((failures == before_failures)); then
      printf 'PASS fixture %s\n' "$mode"
    fi
  done
  [[ $failures == 0 ]]
}

overlay_runner_cases() {
  local source="$fixture/source/test/e2e/database_connections_test.go" source_hash status
  local failures=0
  mkdir -p "${source%/*}"
  source_hash=$go_source_checksum
  export E2E_FAKE_LOG="$fixture/overlay-valid.log" E2E_FAKE_FAIL=none E2E_FAKE_PG_SETTING=100
  export EPCO_E2E_PROJECT_DIR="$fixture/source" EPCO_ARTIFACTS_DIR="$fixture/artifacts/overlay-valid"
  unset EPCO_E2E_BUILD_MODE EPCO_E2E_IMAGE_REFERENCE EPCO_E2E_INSTALL_MODE EPCO_E2E_SUITE
  : > "$E2E_FAKE_LOG"
  set +e
  PATH="$fixture/bin:$PATH" bash "$suite_dir/run.sh" > "$E2E_FAKE_LOG.output" 2>&1
  status=$?
  set -e
  if [[ $status != 0 ]]; then
    printf 'FAIL overlay runner: valid profile returned %s\n' "$status" >&2
    failures=$((failures + 1))
  else
    for phase in compile runtime; do
      if [[ ! -f $E2E_FAKE_LOG.overlay-$phase || ! -f $E2E_FAKE_LOG.derived-$phase ]]; then
        printf 'FAIL overlay runner: %s Go invocation omitted overlay\n' "$phase" >&2
        failures=$((failures + 1))
      fi
    done
    if [[ -f $E2E_FAKE_LOG.overlay-compile && -f $E2E_FAKE_LOG.overlay-runtime ]]; then
      cmp -s "$E2E_FAKE_LOG.overlay-compile" "$E2E_FAKE_LOG.overlay-runtime" ||
        { printf 'FAIL overlay runner: compile and runtime used different maps\n' >&2; failures=$((failures + 1)); }
    fi
    if [[ ! -f $EPCO_ARTIFACTS_DIR/fixture-overlay.json ]] ||
        ! grep -Fq 'application_source_sha' "$EPCO_ARTIFACTS_DIR/fixture-overlay.json" ||
        ! grep -Fq 'fixture_source_sha' "$EPCO_ARTIFACTS_DIR/fixture-overlay.json"; then
      printf 'FAIL overlay runner: source provenance record missing\n' >&2
      failures=$((failures + 1))
    fi
  fi
  [[ $(cksum "$source") == "$source_hash" ]] ||
    { printf 'FAIL overlay runner: original Go fixture source changed\n' >&2; failures=$((failures + 1)); }
  [[ $(grep -c '^kind delete cluster --name epco-e2e-' "$E2E_FAKE_LOG") == 1 ]] ||
    { printf 'FAIL overlay runner: owned Kind cleanup missing\n' >&2; failures=$((failures + 1)); }

  # Reject a stale or ambiguous source profile before acquiring Kind resources.
  cat >> "$source" <<'DUPLICATE'

func installedConnectionJob(namespace, output string) *batchv1.Job {
	const script = `duplicate`
DUPLICATE
  export E2E_FAKE_LOG="$fixture/overlay-invalid.log" EPCO_ARTIFACTS_DIR="$fixture/artifacts/overlay-invalid"
  : > "$E2E_FAKE_LOG"
  set +e
  PATH="$fixture/bin:$PATH" bash "$suite_dir/run.sh" > "$E2E_FAKE_LOG.output" 2>&1
  status=$?
  set -e
  if [[ $status == 0 ]] || grep -Eq '^kind create cluster|^go test .* -count=1' "$E2E_FAKE_LOG"; then
    printf 'FAIL overlay runner: ambiguous profile reached Kind or behavior suite (status %s)\n' "$status" >&2
    failures=$((failures + 1))
  fi
  cp "$suite_dir/database_connections_test.go" "$source"
  [[ $(cksum "$source") == "$source_hash" ]] ||
    { printf 'FAIL overlay runner: original source was not restored\n' >&2; failures=$((failures + 1)); }
  [[ $failures == 0 ]]
}

if [[ ${EPCO_E2E_RUNNER_TEST_SCOPE:-all} == overlay ]]; then
  overlay_runner_cases
  exit
fi
if [[ ${EPCO_E2E_RUNNER_TEST_SCOPE:-all} == fixture ]]; then
  fixture_runner_cases
  exit
fi
if [[ ${EPCO_E2E_RUNNER_TEST_SCOPE:-all} == release ]]; then
  release_runner_cases
  exit
fi
for failure in none kind apply test signal interrupt diagnostics cleanup test-cleanup; do
  export E2E_FAKE_LOG="$fixture/$failure.log" E2E_FAKE_FAIL=$failure
  export EPCO_E2E_PROJECT_DIR="$fixture/source" EPCO_ARTIFACTS_DIR="$fixture/artifacts/$failure"
  set +e
  PATH="$fixture/bin:$PATH" bash "$suite_dir/run.sh" > "$fixture/$failure.output" 2>&1
  status=$?
  set -e
  case $failure in none) expected=0 ;; kind) expected=17 ;; apply) expected=19 ;; test) expected=23 ;; signal) expected=143 ;; interrupt) expected=130 ;; diagnostics|test-cleanup) expected=23 ;; cleanup) expected=1 ;; esac
  if [[ $status != "$expected" ]]; then
    cat "$fixture/$failure.output"
    printf 'FAIL %s: expected %s, got %s\n' "$failure" "$expected" "$status" >&2
    exit 1
  fi
  [[ $(grep -c '^kind delete cluster --name epco-e2e-' "$E2E_FAKE_LOG") == 1 ]]
  [[ -f "$EPCO_ARTIFACTS_DIR/commands.log" ]]
  if [[ $failure == signal || $failure == interrupt ]]; then
    grep -q REDACTED_DSN "$EPCO_ARTIFACTS_DIR/commands.log"
  fi
  if grep -REq 'e2e-secret|aaa[.]bbb[.]ccc' "$EPCO_ARTIFACTS_DIR"; then
    printf 'FAIL secrets leaked in %s artifacts\n' "$failure" >&2
    exit 1
  fi
  if [[ $status -ne 0 ]]; then [[ -f "$EPCO_ARTIFACTS_DIR/runner-junit.xml" ]]; fi
  [[ $(find "$fixture/source" -type f | wc -l | tr -d ' ') == 2 ]]
  [[ $(cksum "$fixture/source/config/default/kustomization.yaml") == "$source_checksum" ]]
  [[ $(cksum "$fixture/source/test/e2e/database_connections_test.go") == "$go_source_checksum" ]]
  created=$(awk '/^kind create cluster/ { print $5 }' "$E2E_FAKE_LOG")
  deleted=$(awk '/^kind delete cluster/ { print $5 }' "$E2E_FAKE_LOG")
  [[ $created == "$deleted" ]]
  if [[ $failure != none && $failure != cleanup ]]; then
    diagnostics=$(grep -n '^kind export logs' "$E2E_FAKE_LOG" | cut -d: -f1)
    cleanup=$(grep -n '^kind delete cluster' "$E2E_FAKE_LOG" | cut -d: -f1)
    [[ -n $diagnostics && $diagnostics -lt $cleanup ]]
  fi
  printf 'PASS e2e runner: %s\n' "$failure"
done

fail() {
  printf 'FAIL e2e runner: %s\n' "$*" >&2
  exit 1
}

prepare_case() {
  local name=$1
  export E2E_FAKE_LOG="$fixture/$name.log" E2E_FAKE_FAIL=none
  export EPCO_E2E_PROJECT_DIR="$fixture/source" EPCO_ARTIFACTS_DIR="$fixture/artifacts/$name"
  : > "$E2E_FAKE_LOG"
}

run_case() {
  set +e
  PATH="$fixture/bin:$PATH" bash "$suite_dir/run.sh" "$@" > "$E2E_FAKE_LOG.output" 2>&1
  status=$?
  set -e
}

assert_metadata() {
  local server_version=$1 build_mode=$2 metadata="$fixture/metadata" file
  : > "$metadata"
  # Commands alone are not run metadata. Accept any readable suite-owned format.
  while IFS= read -r -d '' file; do
    case ${file##*/} in commands.log|*junit*.xml) continue ;; esac
    cat "$file" >> "$metadata"
  done < <(find "$EPCO_ARTIFACTS_DIR" -type f -print0)
  local value
  for value in "$E2E_FAKE_REVISION" "$server_version" "$E2E_FAKE_POSTGRES_IMAGE" \
    "$client_image" "$client_version" "$manager_image" "$build_mode" "$context" "$EPCO_TEST_SEED"; do
    grep -Fq -- "$value" "$metadata" || fail "metadata does not identify $value"
  done
  for field in source server client expected 'build.?mode' context seed; do
    grep -Eiq -- "$field" "$metadata" || fail "metadata omits $field field"
  done
}

previous_context='' previous_image='' previous_kubeconfig=''
for selection in default 15 16 17 18 local; do
  unset EPCO_PG_VERSION EPCO_E2E_BUILD_MODE
  case $selection in
    default) major=16; patch=16.15; build_mode=docker ;;
    15) major=15; patch=15.19; build_mode=docker ;;
    16) major=16; patch=16.15; build_mode=docker ;;
    17) major=17; patch=17.11; build_mode=docker ;;
    18) major=18; patch=18.6; build_mode=docker ;;
    local) major=17; patch=17.11; build_mode=local; export EPCO_E2E_BUILD_MODE=local ;;
  esac
  if [[ $selection != default ]]; then export EPCO_PG_VERSION=$major; fi
  export E2E_FAKE_POSTGRES_IMAGE="docker.io/library/postgres:$patch-bookworm"
  prepare_case "version-$selection"
  run_case
  if [[ $status != 0 ]]; then
    cat "$E2E_FAKE_LOG.output"
    fail "$selection selection returned $status"
  fi
  [[ $(cat "$E2E_FAKE_LOG.server-image") == "$E2E_FAKE_POSTGRES_IMAGE" ]] || fail "$selection server image"
  [[ $(cat "$E2E_FAKE_LOG.major") == "$major" ]] || fail "$selection selected major was not propagated to suite"
  [[ $(cat "$E2E_FAKE_LOG.server-env") == "$E2E_FAKE_POSTGRES_IMAGE" ]] || fail "$selection server image was not propagated to suite"
  client_image=$(cat "$E2E_FAKE_LOG.client-env")
  client_version=16.15
  [[ $client_image == docker.io/library/postgres:16.15-bookworm ]] || fail "$selection suite client image is not the pinned Docker Hub PostgreSQL 16.15 fixture"
  context=$(cat "$E2E_FAKE_LOG.context")
  kubeconfig=$(cat "$E2E_FAKE_LOG.kubeconfig")
  manager_image=$(awk '/^kind load docker-image/ { print $4 }' "$E2E_FAKE_LOG")
  [[ $(cat "$E2E_FAKE_LOG.artifacts") == "$EPCO_ARTIFACTS_DIR" ]] || fail "$selection suite artifact path"
  grep -Fq -- "-ginkgo.seed=$EPCO_TEST_SEED" "$E2E_FAKE_LOG" || fail "$selection randomized seed"
  [[ $context == kind-epco-e2e-* && $context != "$previous_context" ]] || fail "$selection context reused"
  [[ $manager_image == epco/e2e:* && $manager_image != "$previous_image" ]] || fail "$selection manager image reused"
  [[ -n $kubeconfig && $kubeconfig != "$previous_kubeconfig" && ! -e ${kubeconfig%/*} ]] || fail "$selection temporary directory not isolated/cleaned"
  grep -Fq -- "docker image rm $manager_image" "$E2E_FAKE_LOG" || fail "$selection manager image not cleaned"
  assert_metadata "$patch" "$build_mode"
  if grep -REq 'e2e-secret|aaa[.]bbb[.]ccc' "$EPCO_ARTIFACTS_DIR"; then
    fail "$selection artifacts leaked credentials"
  fi
  [[ $(cksum "$fixture/source/config/default/kustomization.yaml") == "$source_checksum" ]] || fail "$selection modified project sources"
  [[ $(cksum "$fixture/source/test/e2e/database_connections_test.go") == "$go_source_checksum" ]] ||
    fail "$selection modified original Go fixture"
  previous_context=$context previous_image=$manager_image previous_kubeconfig=$kubeconfig
  printf 'PASS e2e runner: version %s (%s)\n' "$selection" "$patch"
done

unset EPCO_E2E_BUILD_MODE E2E_FAKE_POSTGRES_IMAGE
for invalid in 14 19 16.15 latest 016 '16 17' '-1'; do
  export EPCO_PG_VERSION=$invalid
  prepare_case "invalid-${invalid// /_}"
  run_case
  [[ $status == 2 ]] || fail "invalid version '$invalid': expected usage status 2, got $status"
  if grep -Eq '^(kind|docker) ' "$E2E_FAKE_LOG"; then
    fail "invalid version '$invalid' acquired or mutated Kind/Docker resources"
  fi
  grep -Fq EPCO_PG_VERSION "$E2E_FAKE_LOG.output" || fail "invalid version '$invalid' has no useful error"
  printf 'PASS e2e runner: invalid version %s\n' "$invalid"
done

unset EPCO_PG_VERSION
prepare_case focused-recovery
focused_args=(
  '-ginkgo.focus=Installed (rotation|recovery)'
  '-ginkgo.seed=60211016'
  '-ginkgo.label-filter=R6 && !slow'
)
run_case "${focused_args[@]}"
[[ $status == 0 ]] || fail "focused recovery returned $status"
observed_args=()
while IFS= read -r -d '' argument; do
  observed_args+=("$argument")
done < "$E2E_FAKE_LOG.argv"
[[ ${#observed_args[@]} -ge ${#focused_args[@]} ]] || fail "focused recovery arguments were omitted"
argument_offset=$((${#observed_args[@]} - ${#focused_args[@]}))
for ((index=0; index<${#focused_args[@]}; index++)); do
  [[ ${observed_args[argument_offset + index]} == "${focused_args[index]}" ]] ||
    fail "focused recovery argument $index was omitted or split: ${focused_args[index]}"
done
printf 'PASS e2e runner: exact focused recovery arguments\n'

# Cold race compilation must complete before the runner owns cluster resources.
# Keep the existing runtime failure injections above separate from this phase.
compile_failures=0
for compilation in success failure; do
  prepare_case "precompile-$compilation"
  if [[ $compilation == failure ]]; then export E2E_FAKE_FAIL=compile; fi
  run_case
  problems=()
  if [[ $compilation == failure ]]; then
    [[ $status == 31 ]] || problems+=("compile failure status: expected 31, got $status")
    if grep -Eq '^(kind|docker|kubectl) ' "$E2E_FAKE_LOG"; then
      problems+=("compile failure acquired or touched cluster/image resources")
    fi
    [[ ! -f $E2E_FAKE_LOG.argv ]] || problems+=("compile failure reached the runtime suite")
    if [[ -f $EPCO_ARTIFACTS_DIR/runner-junit.xml ]]; then
      grep -Fq 'Exit status 31' "$EPCO_ARTIFACTS_DIR/runner-junit.xml" ||
        problems+=("compile failure report did not preserve status 31")
    else
      problems+=("compile failure report missing")
    fi
  else
    [[ $status == 0 ]] || problems+=("successful precompile returned $status")
    compile_line=$(awk '/^go test / && / -run (\^\$|-run=\^\$)/ { print NR; exit }' "$E2E_FAKE_LOG")
    acquisition_line=$(awk '/^(kind|docker|kubectl) / { print NR; exit }' "$E2E_FAKE_LOG")
    if [[ -z $compile_line || -z $acquisition_line || $compile_line -ge $acquisition_line ]]; then
      problems+=("mandatory compile-only invocation did not precede Kind/image acquisition")
    fi
    [[ -f $E2E_FAKE_LOG.argv ]] || problems+=("successful precompile did not reach runtime suite")
  fi
  if [[ -f $E2E_FAKE_LOG.compile-argv ]]; then
    compile_args=()
    while IFS= read -r -d '' argument; do compile_args+=("$argument"); done < "$E2E_FAKE_LOG.compile-argv"
    expected_compile_args=(test -tags=e2e ./test/e2e/ -race -run '^$')
    base_compile_args=()
    skip_overlay_value=false
    for argument in "${compile_args[@]}"; do
      if [[ $skip_overlay_value == true ]]; then skip_overlay_value=false; continue; fi
      if [[ $argument == -overlay ]]; then skip_overlay_value=true; continue; fi
      if [[ $argument == -overlay=* ]]; then continue; fi
      base_compile_args+=("$argument")
    done
    if ((${#base_compile_args[@]} != ${#expected_compile_args[@]})); then
      problems+=("compile-only invocation changed literal argv or selected behaviors")
    else
      for ((index=0; index<${#expected_compile_args[@]}; index++)); do
        [[ ${base_compile_args[index]} == "${expected_compile_args[index]}" ]] ||
          problems+=("compile-only argument $index differs from required literal argv")
      done
    fi
    [[ $(grep -c '^go test .* -run ' "$E2E_FAKE_LOG") == 1 ]] ||
      problems+=("compile-only phase was repeated")
    compile_kubeconfig=$(cat "$E2E_FAKE_LOG.compile-kubeconfig")
    [[ -n $compile_kubeconfig && ! -e ${compile_kubeconfig%/*} ]] ||
      problems+=("compile-only private temporary directory not cleaned")
  else
    problems+=("mandatory compile-only invocation missing")
  fi
  [[ $(cksum "$fixture/source/config/default/kustomization.yaml") == "$source_checksum" ]] ||
    problems+=("compilation changed tracked project source")
  [[ $(cksum "$fixture/source/test/e2e/database_connections_test.go") == "$go_source_checksum" ]] ||
    problems+=("compilation changed original Go fixture")
  [[ $(find "$fixture/source" -type f | wc -l | tr -d ' ') == 2 ]] ||
    problems+=("compilation created project files")
  if grep -REq 'e2e-secret|aaa[.]bbb[.]ccc' "$EPCO_ARTIFACTS_DIR"; then
    problems+=("compile-only artifacts leaked credentials")
  fi
  if ((${#problems[@]} > 0)); then
    printf 'FAIL e2e runner: precompile %s: %s\n' "$compilation" "${problems[*]}" >&2
    compile_failures=$((compile_failures + 1))
  else
    printf 'PASS e2e runner: precompile %s\n' "$compilation"
  fi
done
[[ $compile_failures == 0 ]]
release_runner_cases
fixture_runner_cases
overlay_runner_cases
"$E2E_FAKE_REAL_PYTHON" -m unittest discover -s "$suite_dir" -p fixture_overlay_test.py
"$E2E_FAKE_REAL_PYTHON" -m unittest discover -s "$suite_dir" -p migration_fixture_test.py
shellcheck "$suite_dir/fixtures/migration.sh"
