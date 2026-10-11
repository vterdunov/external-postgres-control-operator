# Development

The project scaffold was initialized with [Kubebuilder v4.16.0](https://github.com/kubernetes-sigs/kubebuilder/releases/tag/v4.16.0) and the `go/v4` plugin. It uses Go 1.27.1, controller-runtime v0.25.0, and Kubernetes v0.37.0 Go libraries. The Go module is `github.com/vterdunov/external-postgres-control-operator`; the scaffold project name is `epco` and its API domain is `epco.io`.

The manager registers all three API types from [api/v1alpha1](../api/v1alpha1). Generated CRDs are in [config/crd/bases](../config/crd/bases), with generated DeepCopy methods alongside the types. See [implementation status](README.md#implementation-status) and [PostgresCluster readiness](postgrescluster.md) for the runtime scope.

Install [mise](https://mise.jdx.dev/) and use the pinned Go and Kubebuilder versions from `mise.toml`:

```sh
mise trust
mise run --skip-tools tools-install
mise run build # Build bin/manager without changing sources
mise run test-unit # Fast unit tests, race detection and randomization
mise run test-architecture # Verify architecture policy against isolated fixtures
mise run test-envtest # CRD contracts and real manager/watch tests
mise run test-integration # Isolated PostgreSQL fixtures (Docker)
mise run test-e2e # Installed application/recovery scenarios in disposable Kind
mise run check-generated # Compare generated output in a temporary copy
mise run lint # Verify linter configuration and run the configured linters
mise run lint-architecture # Check production package dependency boundaries
mise run check # Build, unit tests, lint, generated checks and manifest render
```

Run `mise tasks` to list all development, image and deployment commands. For example, `IMG=registry/epco:dev mise run docker-build` builds an image. Cluster commands require an explicit context: `KUBE_CONTEXT=my-cluster IMG=registry/epco:dev mise run deploy`. Set `IGNORE_NOT_FOUND=true` for tolerant deletion.

Test commands do not generate, format, or tidy sources. `mise.lock` locks tool downloads and checksums for Linux amd64 and macOS arm64; `mise run --skip-tools tools-install` installs only project tools from the lockfile, preserving unrelated global mise settings. Refresh it with `mise lock --platform linux-x64,macos-arm64` after changing tool pins. The custom logcheck linter is built into the ignored `bin/` directory. Envtest downloads Kubernetes 1.37.0 and starts its own API/etcd; it does not run Kubernetes garbage collection or workload controllers. The reusable `test/envtest` harness exposes an uncached client, explicit scope cleanup, and an optional manager with cache synchronization and bounded shutdown. API and manual reconcile tests are separate from manager/watch tests.

## Architecture boundaries

The pinned go-arch-lint v1.19.0 enforces `.go-arch-lint.yml` during `mise run lint`, including the existing CI lint job and `mise run check`. API packages use Kubernetes apimachinery; PostgreSQL code uses API types, pgx, Kubernetes core/v1 Secret types, and stringprep for server-compatible password normalization; controllers use API types, PostgreSQL code and Kubernetes clients/runtime; `cmd` wires these components with Kubernetes runtime libraries. Standard library imports are permitted, while vendor imports require explicit allowlists. New production packages outside the configured components and imports of test helpers fail the check. Deep scanning also checks local dependency injection through interfaces.

Database's manager-backed owner, ACL grantee, default-creator and historical cleanup dependencies use a cached composite index of the canonical PostgresCluster namespace/name and effective SQL role name. Indexing bounds ordinary dependency candidate lookup; authoritative ownership conflicts retain their separate uncached check. Standalone reconciler fixtures keep their direct/fake-client behavior without sending synthetic cache selectors to the API server. Periodic reconciliation discovers PostgreSQL-only DDL and ACL drift. ACL checkpoints use optimistic status writes before SQL and catalog confirmation afterward; retries reconcile current desired state rather than replaying journal commands. Confirmed lifecycle ownership is recorded before full ACL readiness. These choices follow [Kubebuilder Good Practices](https://book.kubebuilder.io/reference/good-practices.html) and the [controller-runtime v0.25.0 FAQ](https://github.com/kubernetes-sigs/controller-runtime/blob/v0.25.0/FAQ.md).

Application exports additionally index local roleRef resource names, password sources, and desired/historical output names. Watches cover successful password-application receipts, source initialization, output deletion, configured public CA updates and name-reservation changes. Dependency update handlers map both previous and current references through the manager's cache. Export eligibility uses the applied-input receipt and administrative role/database/CONNECT catalogs, without an application login from the operator's network.

Cleanup confirms a cached missing or different-UID historical output through APIReader before retiring its durable history. A committed CREATE or recreation can reach the Secret informer after a fresh Database omission update; treating a temporary cache miss or an older cached UID as deletion would lose both cleanup responsibility and its watch mapping. This direct read is limited to confirming removal of a recorded output. Found objects retain provenance and UID/resourceVersion deletion guards; normal export inputs remain cached.

Publication also confirms a cached output whose UID disagrees with a nonempty recorded UID before rebinding history, so an older cached incarnation cannot overwrite the current cleanup identity. Retained adoption checkpoints the exact Secret UID and former Database UID before updating ownership. Pending adoption cleanup uses conditional UID/resourceVersion DELETE against that claimed former/current owner; it never retires intent solely because a GET after an uncertain UPDATE still observes the former owner. A late UPDATE either changes resourceVersion before DELETE or cannot recreate a deleted Secret. Other provenance or owner changes stay conflicts.

Architecture checks include generated API Go sources and exclude `_test.go` files. Root directories `test/`, `bin/`, `artifacts/`, `tools/`, `.agent-plans/` and `.git/` belong to a separate support component with unrestricted imports and no deep scanning. Production components cannot import support packages. These exact root-relative mappings avoid go-arch-lint v1.19.0 directory exclusions, which also skip unrelated packages sharing an excluded directory's prefix. `ignoreNotFoundComponents` permits declared directory globs to be absent in fresh checkouts; Go sources outside mapped components still fail, including new packages such as `testhelper/` and `toolshed/`. `mise run test-architecture` exercises allowed and forbidden imports in temporary copies using the real checker; it also runs within `mise run test-unit`.

## Test suites

| Task | Coverage and requirements |
| --- | --- |
| `test-unit` | Fast unit/race tests, randomized specs, runner tests, and architecture-policy tests; no container runtime or envtest assets |
| `test-envtest` | CRD admission/defaults, real manager/dependency-watch tests, generated-source, ACL dependency watches, extension ownership/output/status recovery and Database status conflicts on an isolated API server; no workload controllers or garbage collection |
| [test-integration](../test/integration/README.md) | PostgreSQL client/role/database/schema/extension/ACL/default behavior, application connection exports with actual libpq/pgJDBC consumption, TLS, restricted-administrator fixtures, and composed envtest recovery; requires a Docker-compatible runtime, a C compiler, and the pinned Java tool |
| [test-e2e](../test/e2e/README.md) | Installed manager health, metrics, CRD CRUD, SQL/credential recovery, real application migrations and CRUD, separate owner/migrator/runtime permissions, defaults/named-table access, two namespaces, paused finalizers and leader takeover; PostgreSQL 15-18 selectable, Docker-compatible runtime required |
| `check-generated` | Regenerate in a temporary copy and compare derived files without changing the checkout |

On macOS, the integration and e2e suites use OrbStack's Docker-compatible runtime. Suite-specific fixture APIs, image pins, build modes, and runtime coverage belong in their linked READMEs. Real RDS verification remains separate work.

Each runner prints its random seed. Set `EPCO_TEST_SEED` to replay it and `EPCO_ARTIFACTS_DIR` to choose the report directory (default under ignored `artifacts/`). Failure diagnostics are retained before teardown; runners handle SIGINT/SIGTERM and clean only resources owned by that run. CI separates unit/race, envtest, generation, PostgreSQL 15/16 integration, and installed PostgreSQL 15-18 e2e. Scheduled/manual integration covers PostgreSQL 15-18 and repeated seeds. Reports and diagnostics are uploaded even on failure. Installed artifacts distinguish expected image pins from measured server/client versions; the stable E2E gate requires all server majors to pass.

## API changes and generation

Use the pinned Kubebuilder for subsequent scaffolding; the three baseline resources already exist. Keep scaffold markers and update the Go types and accepted API documentation together. Regenerate derived files by running `mise run generate` followed by `mise run manifests` instead of editing them manually. Run `mise run check-generated` and `mise run test-envtest` to verify the resulting schemas and admission rules.

Local agent implementation plans belong in `.agent-plans/`, which is excluded from Git. Create this directory in a fresh checkout before writing a plan. Plans and all repository documentation use English and refer to the accepted API baseline.
