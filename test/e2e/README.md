# Installed operator e2e

See the [development guide](../../docs/development.md#test-suites) for the full test workflow and [implementation status](../../docs/README.md#implementation-status) for the current controller scope.

Run `mise run test-e2e` with a working Docker-compatible runtime (OrbStack on
macOS). The runner creates a uniquely named Kind cluster, a private kubeconfig,
and a temporary Kustomize overlay; it never edits the installation manifests.
Kind 0.32.0 and Kubernetes 1.36.1 by digest are pinned. `EPCO_PG_VERSION`
selects PostgreSQL 15.19, 16.15, 17.11 or 18.6; 16 is the default. Unsupported
values fail before runtime resources are acquired. Application Jobs use a
separately pinned PostgreSQL 16.15 client on every server major.

Kind, server and client images are pulled from the project's public GHCR mirror.
Their complete upstream indexes and immutable digests are recorded in
[`test/images/catalog.json`](../images/catalog.json). The node image includes
Kind's preloaded Kubernetes, networking and storage images. The operator suite
prepares the exact native client on every node before starting scenario timers.

The suite verifies one healthy manager, HTTP health/readiness, HTTPS metrics
with authorized, missing and unauthorized tokens, an authenticated
PostgreSQL TCP connection, and CRUD for all existing CRDs. The installed
ExternalPostgres controller authenticates real fixture credentials and recovers
readiness after its Secret is corrupted, repaired, deleted and replaced.
DatabaseRole scenarios verify generated-source rotation, retain/GC safety, conditional reuse after recreation, delete finalizers paused by authorization removal, and ordinary Lease takeover after leader termination. Database scenarios verify restricted owner/schema transfer with data and table-owner preservation, RESTRICT and connected-session deletion recovery, retain/recreate, authorization-paused cleanup and cancellation of blocked SQL before leader takeover. The R3 permission scenario exercises installed database/schema grants, creator defaults, denied access outside the named-table allowlist, named-table removal with data preservation, and retained grants removed by explicit absent after CR recreation. This describes authored coverage; the [roadmap](../../docs/roadmap.md) records completed validation gates. PostgreSQL runs in `epco-postgres`, separately from `epco-system`.

- `EPCO_TEST_SEED=123` replays randomized spec ordering.
- `EPCO_PG_VERSION=15|16|17|18` selects the installed PostgreSQL server.
- `EPCO_ARTIFACTS_DIR=/path/to/artifacts` selects the persistent output directory.
  Its default is `artifacts/e2e/<run-id>` in the project.
- `EPCO_E2E_BUILD_MODE=local` cross-compiles the current manager with the mise Go
  toolchain and packages it in a scratch image. The default `docker` mode builds
  the project Dockerfile. Both use unique image tags and remove them afterward.

Before acquiring Kind or the manager image, the runner compiles the race-enabled
e2e package with no tests selected. Compilation has its own bounded phase; the
full or focused test execution keeps its existing separate 600-second guard.
Commands and reports distinguish preparation from behavioral execution.

Extension scenarios install and relocate hstore, retain it after declaration omission, protect dependent application data with RESTRICT, and converge after the dependency is removed. Application output scenarios verify initial publication and eligible password rotation while an unrelated extension remains unavailable.

The R6 application scenarios cover a NOLOGIN owner, a separate authenticated
migrator, non-owner runtime CRUD and sequence access, creation-time read-only
defaults, and named-table access boundaries. Two application namespaces reuse
Kubernetes names with distinct SQL names, prove declared database isolation,
and exercise conflicts and authorization removal with separate live and
deleting resources. Supplied/generated password rotation, retained provenance
conflicts/reuse, and persisted grant cleanup across manager restart complete
the application lifecycle. The existing blocked-session Lease scenario proves
operator cancellation and takeover independently of fixture blocker cleanup.

Migration Jobs consume read-only namespace-local connection Secrets without an
API token or administrative credentials. Passwords remain in private passfiles,
not process arguments. Fresh pgx probes use the private fixture context's
port-forward and report SQLSTATE only, distinguishing permission denials from
password-authentication failures. A migration gate confirms creator defaults
before DDL while a missing named table still keeps full Database Ready false.
The disposable server requires SCRAM for host connections, including loopback
port-forward probes. Rotation checks first prove that a known wrong password
fails with SQLSTATE 28P01 on the same connection path.

`fixture-expected.txt` records selected image pins, source revision, build mode,
context and seed. `fixture-observed.txt` records independently measured server
and application-client versions. JUnit records the exact scenarios executed.
CI runs all four server majors with separate artifacts and a stable `E2E` gate
that requires every matrix job to succeed. Completed local gates belong in the
[roadmap](../../docs/roadmap.md); authored scenarios alone do not establish a pass.

The runner forwards additional Go/Ginkgo arguments for focused replay:

```sh
EPCO_PG_VERSION=16 EPCO_TEST_SEED=123 mise exec -- bash test/e2e/run.sh \
  '-ginkgo.focus=R6 Installed application scenarios'
```

The complete default-Dockerfile suite remains the acceptance gate. Focused
replays supplement it and preserve their own seed, reports and diagnostics.

The runner preserves command failure statuses and returns 130/143 on
SIGINT/SIGTERM. It stops descendant processes, saves diagnostics before
cleanup on failure, and deletes only its own cluster, image and temporary
files. Commands and diagnostics redact bearer tokens, passwords and PostgreSQL
DSNs. JUnit and command logs remain in the artifact directory after cleanup.

`bash test/e2e/runner_test.sh` verifies lifecycle, isolation, failure statuses,
diagnostics ordering and both signals with fake CLIs, without a runtime.
