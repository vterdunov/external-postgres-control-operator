#!/usr/bin/env bash
set -euo pipefail
suite_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
fixture=$(mktemp -d "${TMPDIR:-/tmp}/epco-e2e-runner-test.XXXXXX")
trap 'rm -rf "$fixture"' EXIT
mkdir -p "$fixture/bin" "$fixture/source/config/default"
printf 'original manifest\n' > "$fixture/source/config/default/kustomization.yaml"
source_checksum=$(cksum "$fixture/source/config/default/kustomization.yaml")
cat > "$fixture/bin/fake" <<'MOCK'
#!/usr/bin/env bash
set -eu
name=${0##*/}
printf '%s %s\n' "$name" "$*" >> "$E2E_FAKE_LOG"
case "$name $*" in
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
    mirror_arg=false
    previous=''
    for argument in "$@"; do
      if [[ $previous == --build-arg && $argument == BASE_IMAGE=public.ecr.aws/docker/library/golang:1.27.1 ||
        $argument == --build-arg=BASE_IMAGE=public.ecr.aws/docker/library/golang:1.27.1 ]]; then
        mirror_arg=true
      fi
      previous=$argument
    done
    if [[ ${!#} == . && $mirror_arg != true ]]; then
      printf 'FAIL Docker fixture build requires the mirrored Go BASE_IMAGE argument\n' >&2
      exit 95
    fi ;;
  'kubectl '*)
    [[ " $* " == *' --context kind-epco-e2e-'* ]] || exit 99
    [[ " $* " == *' --kubeconfig '* ]] || exit 98
    if [[ ${E2E_FAKE_FAIL:-} == apply && " $* " == *' apply '* ]]; then exit 19; fi
    while [[ $# -gt 0 ]]; do
      if [[ $1 == -f && $2 == */postgres.yaml ]]; then
        postgres_image=$(awk '/^[[:space:]]*image:/ { print $2 }' "$2")
        if [[ $postgres_image != public.ecr.aws/docker/library/postgres:16.15-bookworm ]]; then
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
    printf 'password=e2e-secret postgresql://user:e2e-secret@localhost/test Authorization: Bearer aaa.bbb.ccc\n'
    case ${E2E_FAKE_FAIL:-} in
      test|diagnostics|test-cleanup) exit 23 ;;
      signal) kill -TERM "$PPID"; sleep 30 ;;
      interrupt) kill -INT "$PPID"; sleep 30 ;;
    esac ;;
esac
MOCK
chmod +x "$fixture/bin/fake"
for cli in kind kubectl kustomize docker go; do ln -s fake "$fixture/bin/$cli"; done
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
  [[ $(find "$fixture/source" -type f | wc -l | tr -d ' ') == 1 ]]
  [[ $(cksum "$fixture/source/config/default/kustomization.yaml") == "$source_checksum" ]]
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
