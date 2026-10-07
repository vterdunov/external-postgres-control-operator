# Installed operator e2e

Run `mise run test-e2e` with a working Docker-compatible runtime (OrbStack on
macOS). The runner creates a uniquely named Kind cluster, a private kubeconfig,
and a temporary Kustomize overlay; it never edits the installation manifests.
Kind 0.32.0, Kubernetes 1.36.1 by digest, and PostgreSQL 16.15 are pinned.

The suite verifies one healthy manager, HTTP health/readiness, HTTPS metrics
with authorized, missing and unauthorized tokens, an authenticated
PostgreSQL TCP connection, and CRUD for all existing CRDs. The installed
PostgresCluster controller authenticates real fixture credentials and recovers
readiness after its Secret is corrupted, repaired, deleted and replaced.
PostgreSQL runs in `epco-postgres`, separately from `epco-system`.

- `EPCO_TEST_SEED=123` replays randomized spec ordering.
- `EPCO_ARTIFACTS_DIR=/path/to/artifacts` selects the persistent output directory.
  Its default is `artifacts/e2e/<run-id>` in the project.
- `EPCO_E2E_BUILD_MODE=local` cross-compiles the current manager with the mise Go
  toolchain and packages it in a scratch image. The default `docker` mode builds
  the project Dockerfile. Both use unique image tags and remove them afterward.

The runner preserves command failure statuses and returns 130/143 on
SIGINT/SIGTERM. It stops descendant processes, saves diagnostics before
cleanup on failure, and deletes only its own cluster, image and temporary
files. Commands and diagnostics redact bearer tokens, passwords and PostgreSQL
DSNs. JUnit and command logs remain in the artifact directory after cleanup.

`bash test/e2e/runner_test.sh` verifies lifecycle, isolation, failure statuses,
diagnostics ordering and both signals with fake CLIs, without a runtime.
