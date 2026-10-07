#!/usr/bin/env bash
set -euo pipefail
suite_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
project_dir=${EPCO_E2E_PROJECT_DIR:-$(cd "$suite_dir/../.." && pwd)}
run_dir=$(mktemp -d "${TMPDIR:-/tmp}/epco-e2e.XXXXXX")
suffix=$(printf '%s' "${run_dir##*.}" | tr '[:upper:]' '[:lower:]')
run_id="$(date +%s)-$$-$suffix"
cluster="epco-e2e-$run_id"
context="kind-$cluster"
image="epco/e2e:$run_id"
kubeconfig="$run_dir/kubeconfig"
artifacts=${EPCO_ARTIFACTS_DIR:-"$project_dir/artifacts/e2e/$run_id"}
mkdir -p "$artifacts"
artifacts=$(cd "$artifacts" && pwd)
command_log="$artifacts/commands.log"
: > "$command_log"
: > "$kubeconfig"
chmod 600 "$kubeconfig"
kind_owned=false
image_owned=false
child=''
node_image='kindest/node:v1.36.1@sha256:3489c7674813ba5d8b1a9977baea8a6e553784dab7b84759d1014dbd78f7ebd5'
postgres_image='postgres:16.15-bookworm'
seed=${EPCO_TEST_SEED:-$(date +%s)}
export KUBECONFIG="$kubeconfig" KUBECTL_KUBERC=false
export EPCO_E2E_CONTEXT="$context" EPCO_E2E_ARTIFACTS="$artifacts"
export EPCO_E2E_POSTGRES_NAMESPACE=epco-postgres
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

kind_owned=true
run 240 kind create cluster --name "$cluster" --image "$node_image" --kubeconfig "$kubeconfig" --wait 180s
chmod 600 "$kubeconfig"
image_owned=true
case ${EPCO_E2E_BUILD_MODE:-docker} in
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
  *) printf 'Unsupported EPCO_E2E_BUILD_MODE\n' >&2; exit 2 ;;
esac
run 120 kind load docker-image "$image" --name "$cluster"
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
run 30 kustomize build "$run_dir/overlay" -o "$run_dir/operator.yaml"
kubectl_run 60 apply -f "$run_dir/operator.yaml"
kubectl_run 180 -n epco-system rollout status deployment/epco-controller-manager --timeout=150s
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
        env:
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
run 600 go test -tags=e2e ./test/e2e/ -race -count=1 -v \
  -ginkgo.randomize-all -ginkgo.seed="$seed" -ginkgo.junit-report="$artifacts/junit.xml"
