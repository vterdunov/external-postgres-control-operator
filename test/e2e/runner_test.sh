#!/usr/bin/env bash
set -euo pipefail
suite_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
fixture=$(mktemp -d "${TMPDIR:-/tmp}/epco-e2e-runner-test.XXXXXX")
trap 'rm -rf "$fixture"' EXIT
mkdir -p "$fixture/bin" "$fixture/source/config/default"
printf 'original manifest\n' > "$fixture/source/config/default/kustomization.yaml"
source_checksum=$(cksum "$fixture/source/config/default/kustomization.yaml")
unset EPCO_PG_VERSION EPCO_E2E_BUILD_MODE EPCO_E2E_CLIENT_IMAGE EPCO_E2E_POSTGRES_IMAGE
export EPCO_TEST_SEED=60211015
export E2E_FAKE_REVISION=0123456789abcdef0123456789abcdef01234567
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
  'git '*rev-parse*) printf '%s\n' "$E2E_FAKE_REVISION" ;;
  'go env GOARCH') printf 'arm64\n' ;;
  'kubectl '*)
    [[ " $* " == *' --context kind-epco-e2e-'* ]] || exit 99
    [[ " $* " == *' --kubeconfig '* ]] || exit 98
    if [[ ${E2E_FAKE_FAIL:-} == apply && " $* " == *' apply '* ]]; then exit 19; fi
    while [[ $# -gt 0 ]]; do
      if [[ $1 == -f && $2 == */postgres.yaml ]]; then
        postgres_image=$(awk '/^[[:space:]]*image:/ { print $2 }' "$2")
        printf '%s\n' "$postgres_image" > "$E2E_FAKE_LOG.server-image"
        if [[ $postgres_image != "${E2E_FAKE_POSTGRES_IMAGE:-public.ecr.aws/docker/library/postgres:16.15-bookworm}" ]]; then
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
for cli in kind kubectl kustomize docker go git; do ln -s fake "$fixture/bin/$cli"; done
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
  export E2E_FAKE_POSTGRES_IMAGE="public.ecr.aws/docker/library/postgres:$patch-bookworm"
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
  case $client_image in
    public.ecr.aws/docker/library/postgres:15.19-bookworm) client_version=15.19 ;;
    public.ecr.aws/docker/library/postgres:16.15-bookworm) client_version=16.15 ;;
    public.ecr.aws/docker/library/postgres:17.11-bookworm) client_version=17.11 ;;
    public.ecr.aws/docker/library/postgres:18.6-bookworm) client_version=18.6 ;;
    *) fail "$selection suite client image is absent or unpinned" ;;
  esac
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
