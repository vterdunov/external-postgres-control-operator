#!/usr/bin/env bash
set -euo pipefail
suite_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
project_dir=${EPCO_E2E_PROJECT_DIR:-$(cd "$suite_dir/../.." && pwd)}
major=${EPCO_PG_VERSION:-16}
case $major in
  15) server_version=15.19 ;;
  16) server_version=16.15 ;;
  17) server_version=17.11 ;;
  18) server_version=18.6 ;;
  *) printf 'Unsupported EPCO_PG_VERSION: %s (expected 15, 16, 17 or 18)\n' "$major" >&2; exit 2 ;;
esac
build_mode=${EPCO_E2E_BUILD_MODE:-docker}
case $build_mode in
  docker|local|prebuilt) ;;
  *) printf 'Unsupported EPCO_E2E_BUILD_MODE\n' >&2; exit 2 ;;
esac
suite=${EPCO_E2E_SUITE:-operator}
install_mode=${EPCO_E2E_INSTALL_MODE:-kustomize}
case $install_mode in
  kustomize|chart) ;;
  *) printf 'Unsupported EPCO_E2E_INSTALL_MODE\n' >&2; exit 2 ;;
esac
case $suite in
  operator|delivery) ;;
  *) printf 'Unsupported EPCO_E2E_SUITE\n' >&2; exit 2 ;;
esac
if [[ $build_mode == prebuilt ]]; then
  image_reference=${EPCO_E2E_IMAGE_REFERENCE:-}
  if [[ ! $image_reference =~ ^ghcr\.io/vterdunov/external-postgres-control-operator:[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?@sha256:[a-f0-9]{64}$ ]]; then
    printf 'Prebuilt mode requires the public application tag and sha256 digest\n' >&2
    exit 2
  fi
  image_tag=${image_reference#*:}
  image_tag=${image_tag%%@*}
fi
if [[ $suite == delivery || $install_mode == chart ]]; then
  if [[ ! -f ${EPCO_DELIVERY_CHART_PACKAGE:-} || ${EPCO_DELIVERY_CHART_PACKAGE:-} != *.tgz
      || ! ${EPCO_DELIVERY_IMAGE_SOURCE_SHA:-} =~ ^[a-f0-9]{40}$
      || ! ${EPCO_DELIVERY_CHART_SOURCE_SHA:-} =~ ^[a-f0-9]{40}$ ]]; then
    printf 'Delivery requires a local chart archive and separate image/chart source SHAs\n' >&2
    exit 2
  fi
  if [[ $build_mode == prebuilt && ${EPCO_DELIVERY_IMAGE_REFERENCE:-} != "$image_reference" ]]; then
    printf 'Delivery image reference must match the selected prebuilt image\n' >&2
    exit 2
  fi
fi
run_dir=$(mktemp -d "${TMPDIR:-/tmp}/epco-e2e.XXXXXX")
suffix=$(printf '%s' "${run_dir##*.}" | tr '[:upper:]' '[:lower:]')
run_id="$(date +%s)-$$-$suffix"
cluster="epco-e2e-$run_id"
context="kind-$cluster"
image="epco/e2e:$run_id"
if [[ $build_mode == prebuilt ]]; then image=$image_reference; fi
if [[ $suite == delivery && ${EPCO_DELIVERY_LOCAL_IMAGE:-false} == true ]]; then
  export EPCO_DELIVERY_IMAGE_REFERENCE="$image"
fi
kubeconfig="$run_dir/kubeconfig"
artifacts=${EPCO_ARTIFACTS_DIR:-"$project_dir/artifacts/e2e/$run_id"}
case $artifacts in
  /*) ;;
  *) artifacts="$PWD/$artifacts" ;;
esac
mkdir -p "$artifacts"
command_log="$artifacts/commands.log"
: > "$command_log"
: > "$kubeconfig"
chmod 600 "$kubeconfig"
kind_owned=false
image_owned=false
child=''
node_image='kindest/node:v1.36.1@sha256:3489c7674813ba5d8b1a9977baea8a6e553784dab7b84759d1014dbd78f7ebd5'
postgres_image="docker.io/library/postgres:$server_version-bookworm"
client_version=16.15
client_image="docker.io/library/postgres:$client_version-bookworm"
seed=${EPCO_TEST_SEED:-$(date +%s)}
export KUBECONFIG="$kubeconfig" KUBECTL_KUBERC=false
export EPCO_E2E_CONTEXT="$context" EPCO_E2E_ARTIFACTS="$artifacts"
export EPCO_E2E_POSTGRES_NAMESPACE=epco-postgres
export EPCO_PG_VERSION="$major" EPCO_E2E_POSTGRES_IMAGE="$postgres_image" EPCO_E2E_CLIENT_IMAGE="$client_image"
printf 'E2e seed: %s\nArtifacts: %s\n' "$seed" "$artifacts"

redact() {
  sed -E \
    -e 's/(Bearer )[A-Za-z0-9._-]+/\1[REDACTED]/g' \
    -e 's#postgres(ql)?://[^[:space:]"<>]+#[REDACTED_DSN]#g' \
    -e 's/((password|token)[=:][[:space:]]*)[^[:space:],;]+/\1[REDACTED]/g'
}

stop_child() {
  [[ -n $child ]] || return 0
  kill -TERM -- "-$child" 2>/dev/null || true
  for ((attempt=0; attempt<25; attempt++)); do
    kill -0 -- "-$child" 2>/dev/null || break
    sleep 0.2
  done
  kill -KILL -- "-$child" 2>/dev/null || true
  wait "$child" 2>/dev/null || true
  child=''
}

# Commands have bounded execution and preserve their original failure status.
run() {
  local limit=$1
  shift
  local output="$run_dir/command-output" status=0 ticks=0
  printf 'Running: %s\n' "$*" | redact | tee -a "$command_log"
  # Give long-running commands their own group; keep short helpers outside job control.
  set -m
  "$@" > "$output" 2>&1 &
  child=$!
  set +m
  while kill -0 "$child" 2>/dev/null; do
    if ((ticks >= limit * 5)); then
      stop_child
      cat "$output" | redact | tee -a "$command_log"
      printf 'Command timed out after %ss\n' "$limit" | tee -a "$command_log"
      return 124
    fi
    sleep 0.2
    ticks=$((ticks + 1))
  done
  wait "$child" || status=$?
  child=''
  cat "$output" | redact | tee -a "$command_log"
  return "$status"
}

kubectl_run() {
  local limit=$1
  shift
  run "$limit" kubectl --kubeconfig "$kubeconfig" --context "$context" --request-timeout=30s "$@"
}

cleanup() {
  local status=$?
  trap - EXIT
  trap '' INT TERM
  local interrupted=false
  [[ -z $child ]] || interrupted=true
  stop_child
  if [[ $interrupted == true && -f "$run_dir/command-output" ]]; then
    # Retain partial command output before diagnostics reuse the capture file.
    cat "$run_dir/command-output" | redact | tee -a "$command_log" || true
  fi
  if [[ $status -ne 0 && $kind_owned == true ]]; then
    # Persist diagnostics before removing any run-owned resources.
    run 60 kind export logs --name "$cluster" "$artifacts/kind" || true
    kubectl_run 40 get pods,deployments,services -A -o wide || true
    kubectl_run 40 get events -A --sort-by=.lastTimestamp || true
    kubectl_run 40 -n epco-system describe deployment epco-controller-manager || true
    kubectl_run 40 -n epco-system logs deployment/epco-controller-manager --all-containers=true || true
    kubectl_run 40 -n epco-postgres logs deployment/postgres || true
    if [[ -d "$artifacts/kind" ]]; then
      while IFS= read -r -d '' log_file; do
        redact < "$log_file" > "$run_dir/redacted"
        cat "$run_dir/redacted" > "$log_file"
      done < <(find "$artifacts/kind" -type f -print0)
    fi
  fi
  if [[ $kind_owned == true ]]; then
    if ! run 120 kind delete cluster --name "$cluster"; then
      [[ $status -ne 0 ]] || status=1
    fi
  fi
  if [[ $image_owned == true ]]; then
    if ! run 30 docker image rm "$image"; then
      [[ $status -ne 0 ]] || status=1
    fi
  fi
  if ! rm -rf "$run_dir"; then
    [[ $status -ne 0 ]] || status=1
  fi
  if [[ $status -ne 0 ]]; then
    cat > "$artifacts/runner-junit.xml" <<REPORT
<?xml version="1.0" encoding="UTF-8"?>
<testsuites><testsuite name="EPCO e2e runner" tests="1" failures="1"><testcase name="runner lifecycle"><failure message="Exit status $status">See commands.log and Kind diagnostics.</failure></testcase></testsuite></testsuites>
REPORT
  fi
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
cd "$project_dir"
source_revision=$(git rev-parse HEAD)
cat > "$artifacts/fixture-expected.txt" <<METADATA
source revision: $source_revision
server expected version: $server_version
server expected image: $postgres_image
server expected host authentication: scram-sha-256
server client connection check interval ms: 100
client expected version: $client_version
client expected image: $client_image
manager image: $image
build mode: $build_mode
context: $context
seed: $seed
METADATA
if (($# > 0)); then
  printf 'suite argument: %s\n' "$@" | redact >> "$artifacts/fixture-expected.txt"
fi

if [[ $suite == operator ]]; then run 600 go test -tags=e2e ./test/e2e/ -race -run '^$'; fi

kind_owned=true
run 240 kind create cluster --name "$cluster" --image "$node_image" --kubeconfig "$kubeconfig" --wait 180s
chmod 600 "$kubeconfig"
if [[ $build_mode != prebuilt ]]; then image_owned=true; fi
case $build_mode in
  docker) run 900 docker build -t "$image" . ;;
  local)
    arch=$(go env GOARCH)
    run 300 env CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -o "$run_dir/manager" ./cmd/main.go
    cat > "$run_dir/Dockerfile" <<'DOCKERFILE'
FROM scratch
COPY manager /manager
USER 65532:65532
ENTRYPOINT ["/manager"]
DOCKERFILE
    run 120 docker build -t "$image" "$run_dir"
    ;;
  prebuilt) ;;
  *) printf 'Unsupported EPCO_E2E_BUILD_MODE\n' >&2; exit 2 ;;
esac
if [[ $build_mode != prebuilt ]]; then run 120 kind load docker-image "$image" --name "$cluster"; fi
if [[ $suite == operator ]]; then
if [[ $install_mode == chart ]]; then
  kubectl_run 30 create namespace epco-system
  run 240 python3 "$suite_dir/../../tools/chart.py" install \
    --package "$EPCO_DELIVERY_CHART_PACKAGE" --release epco --namespace epco-system \
    --context "$context" --image-reference "$image"
else
cp -R "$project_dir/config" "$run_dir/config"
mkdir -p "$run_dir/overlay"
cat > "$run_dir/overlay/kustomization.yaml" <<OVERLAY
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
- ../config/default
images:
- name: controller
  newName: epco/e2e
  newTag: $run_id
OVERLAY
if [[ $build_mode == prebuilt ]]; then
  cat > "$run_dir/overlay/kustomization.yaml" <<OVERLAY
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
- ../config/default
images:
- name: controller
  newName: ghcr.io/vterdunov/external-postgres-control-operator
  newTag: $image_tag
  digest: ${image_reference##*@}
OVERLAY
fi
run 30 kustomize build "$run_dir/overlay" -o "$run_dir/operator.yaml"
kubectl_run 60 apply -f "$run_dir/operator.yaml"
fi
kubectl_run 180 -n epco-system rollout status deployment/epco-controller-manager --timeout=150s
fi
password=$(od -An -N24 -tx1 /dev/urandom | tr -d ' \n')
cat > "$run_dir/postgres.yaml" <<POSTGRES
apiVersion: v1
kind: Namespace
metadata:
  name: epco-postgres
---
apiVersion: v1
kind: Secret
metadata:
  name: postgres
  namespace: epco-postgres
stringData:
  password: $password
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: postgres
  namespace: epco-postgres
spec:
  replicas: 1
  selector:
    matchLabels:
      app: postgres
  template:
    metadata:
      labels:
        app: postgres
    spec:
      containers:
      - name: postgres
        image: $postgres_image
        args: ["postgres", "-c", "client_connection_check_interval=100ms"]
        env:
        - name: POSTGRES_INITDB_ARGS
          value: --auth-host=scram-sha-256
        - name: POSTGRES_PASSWORD
          valueFrom:
            secretKeyRef:
              name: postgres
              key: password
        - name: PGPASSWORD
          valueFrom:
            secretKeyRef:
              name: postgres
              key: password
        ports:
        - containerPort: 5432
        readinessProbe:
          exec:
            command: [sh, -c, 'psql -h 127.0.0.1 -U postgres -Atc "SELECT 1" | grep -qx 1']
          initialDelaySeconds: 2
          periodSeconds: 2
        resources:
          requests:
            cpu: 50m
            memory: 128Mi
          limits:
            memory: 256Mi
---
apiVersion: v1
kind: Service
metadata:
  name: postgres
  namespace: epco-postgres
spec:
  selector:
    app: postgres
  ports:
  - port: 5432
    targetPort: 5432
POSTGRES
kubectl_run 60 apply -f "$run_dir/postgres.yaml"
kubectl_run 240 -n epco-postgres rollout status deployment/postgres --timeout=210s
kubectl_run 30 -n epco-postgres exec deployment/postgres -- \
  psql -X -h 127.0.0.1 -U postgres -Atc \
  "SELECT (EXTRACT(EPOCH FROM current_setting('client_connection_check_interval')::interval) * 1000)::bigint"
interval_ms=$(cat "$run_dir/command-output")
if [[ $interval_ms != 100 ]]; then
  printf 'Test PostgreSQL client connection check interval must be 100ms\n' >&2
  exit 2
fi
printf 'server client connection check interval ms: %s\n' "$interval_ms" > "$artifacts/fixture-settings.txt"
if [[ $suite == delivery ]]; then
  run 1200 python3 "$suite_dir/../delivery/lifecycle.py"
else
  run 600 go test -tags=e2e ./test/e2e/ -race -count=1 -v \
    -ginkgo.randomize-all -ginkgo.seed="$seed" -ginkgo.junit-report="$artifacts/junit.xml" "$@"
  if [[ ${EPCO_E2E_RUNTIME_REPORT:-false} == true ]]; then
    run 60 python3 "$suite_dir/../../tools/release_ci.py" observe \
      --manifest "$EPCO_RELEASE_MANIFEST" --output "$artifacts/release-installed.json"
  fi
fi
