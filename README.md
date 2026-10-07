# External PostgreSQL Control Operator

An operator for managing roles, databases, and access across PostgreSQL clusters, including AWS RDS. The primary use case is a microservice database with a service user and a separate read-only user for an admin UI. The accepted initial API follows CloudNativePG `DatabaseRole` and `Database`, adding an external `PostgresCluster` connection descriptor, inline permissions, password generation, and application connection Secrets.

The [API baseline](docs/api-proposal.md) was accepted on 2026-10-07 and draws on CNPG, movetokube, and CNPG discussions #10826/#7872. It is the reference for implementation. [SQL object ownership](docs/sql-object-ownership.md) follows CNPG's CR/status checks, extended across namespaces with serialized reconciliation; no separate ownership ConfigMap is required. Go API types and CRDs are generated for `postgresql.epco.io/v1alpha1`. The first controller implements `PostgresCluster` connection readiness; `DatabaseRole` and `Database` reconciliation are future stages.

Initial connection Secrets are published as soon as their validated connection inputs and password are available, allowing migration Jobs to retry while SQL becomes ready. Secret existence does not imply Ready. Managed permission history is stored in the live CR's status; a recreated CR starts a new management session, and historical retained grants require explicit ensure=absent entries for removal. Extended memberships with per-membership options are deferred; inRoles follows CNPG.

The accepted API supports [cross-namespace access](docs/cross-namespace-access.md): a shared infrastructure connection authorizes application namespaces through a standard Kubernetes LabelSelector, and microservice teams manage their databases and roles locally. Omitting cluster.namespace keeps references local. SQL names need no advance allowlist; CR/status ownership checks resolve collisions. The shared [connection](examples/cross-namespace/infra-connection.yaml) and complete [orders](examples/cross-namespace/orders-service-and-admin-readonly.yaml) and [billing](examples/cross-namespace/billing-service-and-admin-readonly.yaml) examples demonstrate this contract.

| Example | Purpose |
| --- | --- |
| [postgres-connection.yaml](examples/microservice/postgres-connection.yaml) | Shared operator connection referencing existing administrative and CA Secrets |
| [service-owner-and-admin-readonly.yaml](examples/microservice/service-owner-and-admin-readonly.yaml) | The service owns its database and runs migrations; the admin UI gets SELECT. The operator creates passwords and two connection Secrets |
| [separate-migrations-and-admin-readonly.yaml](examples/microservice/separate-migrations-and-admin-readonly.yaml) | Separate owner and migrator, runtime DML, admin SELECT, and three connection Secrets |
| [orders-service-password-provided.yaml](examples/microservice/orders-service-password-provided.yaml) | Supplied basic-auth password as an alternative to generation |
| [orders-service-deployment.yaml](examples/microservice/orders-service-deployment.yaml) | The service consumes DATABASE_URL and a CA from its connection Secret |
| [orders-admin-deployment.yaml](examples/microservice/orders-admin-deployment.yaml) | The admin UI consumes DATABASE_URL and a CA from its separate read-only connection Secret |
| [infra-connection.yaml](examples/cross-namespace/infra-connection.yaml) | Shared connection in infra, selecting orders and billing namespaces |
| [orders-service-and-admin-readonly.yaml](examples/cross-namespace/orders-service-and-admin-readonly.yaml) | Orders database, service user, read-only admin user, and connection outputs in orders |
| [billing-service-and-admin-readonly.yaml](examples/cross-namespace/billing-service-and-admin-readonly.yaml) | Billing database, service user, read-only admin user, and connection outputs in billing |
| [minimal.yaml](examples/api-proposal/minimal.yaml) | Connection, LOGIN role, and database with minimal fields |
| [full.yaml](examples/api-proposal/full.yaml) | Full owner/reader/writer configuration, schemas, extension, and permissions |
| [readonly.yaml](examples/api-proposal/readonly.yaml) | Reader group and user, updating the Database from minimal |
| [multi-cluster.yaml](examples/api-proposal/multi-cluster.yaml) | The same SQL username on two independent clusters |
| [aws-rds.yaml](examples/api-proposal/aws-rds.yaml) | RDS grants/default privileges, password users, and IAM users |
| [llm-readonly.yaml](examples/api-proposal/llm-readonly.yaml) | SELECT on explicitly listed existing tables |
| [llm-readonly-after-removal.yaml](examples/api-proposal/llm-readonly-after-removal.yaml) | Removing orders from the permission set removes its managed SELECT |
| [llm-readonly-with-absent-grant.yaml](examples/api-proposal/llm-readonly-with-absent-grant.yaml) | Optional absent state on orders SELECT while preserving invoices SELECT |

Choose one microservice variant and use the shared postgres-connection example. Deployment images are placeholders. The supplied-password example replaces the orders-service role declaration during initial configuration. Minimal/full/RDS/LLM are other alternative resource sets. Readonly extends minimal and updates Database/app; LLM after-removal and with-absent-grant are alternative updates of the original LLM example. Prerequisites, Secret lifecycle, declarative permission cleanup, CNPG differences, table access limits, IAM, and ADMIN/INHERIT/SET behavior are documented in the API baseline. Removing a previously declared permission within the same CR UID removes its managed grant; ensure=absent controls only the grant and also supports cleanup after CR recreation.

## PostgresCluster readiness

Create a namespace-local `kubernetes.io/basic-auth` Secret with a nonempty `username` and a `password` key. An empty password is accepted. Reference it from `spec.credentialsSecret.name` and set `spec.connection.host`. Defaults are port `5432`, database `postgres`, TLS `verify-full`, reconcile interval `5m`, connect timeout `10s`, and statement timeout `30s`. The SQL timeout is rounded up to milliseconds; values above PostgreSQL's maximum of `2147483647` milliseconds are reported as `InvalidConfiguration`.

All six PostgreSQL TLS modes are supported. A `caSecret` supplies PEM certificates from the selected local Secret key and replaces system trust; without it the operator uses system roots. A client certificate must come from a local `kubernetes.io/tls` Secret with a valid `tls.crt`/`tls.key` pair. Mode `disable` ignores TLS Secret references. Mode `require` with a supplied CA verifies the certificate chain, following [PostgreSQL SSL semantics](https://www.postgresql.org/docs/18/libpq-ssl.html).

The controller watches referenced Secrets, so creating a missing Secret, rotating credentials or certificates, or deleting a Secret triggers a check. Changes to connection inputs trigger a check too. Successful checks repeat at `reconcileInterval`; expected failures retry within `min(30s, reconcileInterval)`. Kubernetes API failures use controller backoff.

`Ready=True` and `status.applied=true` record a successful check and update `appliedGeneration` and `lastSuccessfulReconcileTime`. Failure sets `Ready=False` and `applied=false` while retaining the last successful generation and time. `observedGeneration` records the checked spec. Error reasons are `SecretNotFound`, `InvalidSecret`, `InvalidConfiguration`, `ConnectionUnavailable`, and `NotWritable`; status does not expose credentials or raw driver errors. Results are discarded when authoritative reads show changed inputs or a conflicting status write.

The `access.namespaceSelector` contract is unchanged. Consumer authorization will be implemented with DatabaseRole or Database; this connection check does not depend on the selector.

## Development

The project scaffold was initialized with [Kubebuilder v4.16.0](https://github.com/kubernetes-sigs/kubebuilder/releases/tag/v4.16.0) and the `go/v4` plugin. It uses Go 1.27.1, controller-runtime v0.25.0, and Kubernetes v0.37.0 Go libraries. The Go module is `github.com/vterdunov/external-postgres-control-operator`; the scaffold project name is `epco` and its API domain is `epco.io`.

The manager registers the `PostgresCluster`, `DatabaseRole`, and `Database` API types from `api/v1alpha1`. Generated CRDs are in `config/crd/bases`, with DeepCopy methods generated alongside the types. The manager runs the `PostgresCluster` controller. It opens a fresh connection for each check and uses a read-only query to confirm authentication, absence of recovery, and a writable session. No CREATEDB, CREATEROLE, or SUPERUSER privileges are required. Role, database, permission, and application Secret examples remain API design references.

Install [mise](https://mise.jdx.dev/) and use the pinned Go and Kubebuilder versions from `mise.toml`:

```sh
mise trust
mise run --skip-tools tools-install
mise run build # Build bin/manager without changing sources
mise run test-unit # Fast unit tests, race detection and randomization
mise run test-envtest # CRD contracts and real manager/watch tests
mise run test-integration # Isolated PostgreSQL fixtures (Docker)
mise run test-e2e # Deployment smoke tests in disposable Kind
mise run check-generated # Compare generated output in a temporary copy
mise run lint # Verify linter configuration and run the configured linters
mise run check # Build, unit tests, lint, generated checks and manifest render
```

Run `mise tasks` to list all development, image and deployment commands. For example, `IMG=registry/epco:dev mise run docker-build` builds an image. Cluster commands require an explicit context: `KUBE_CONTEXT=my-cluster IMG=registry/epco:dev mise run deploy`. Set `IGNORE_NOT_FOUND=true` for tolerant deletion.

Test commands do not generate, format, or tidy sources. `mise.lock` locks tool downloads and checksums for Linux amd64 and macOS arm64; `mise run --skip-tools tools-install` installs only project tools from the lockfile, preserving unrelated global mise settings. Refresh it with `mise lock --platform linux-x64,macos-arm64` after changing tool pins. The custom logcheck linter is built into the ignored `bin/` directory. Envtest downloads Kubernetes 1.37.0 and starts its own API/etcd; it does not run Kubernetes garbage collection or workload controllers. The reusable `test/envtest` harness exposes an uncached client, explicit scope cleanup, and an optional manager with cache synchronization and bounded shutdown. API and manual reconcile tests are separate from manager/watch tests.

PostgreSQL integration uses one TLS-enabled container per suite and unique restricted roles/databases per spec. `test/integration.Start` and `Server.NewFixture` can be combined with the envtest harness for future controller tests. Set `EPCO_PG_VERSION=15`, `16`, `17`, or `18` to select the pinned image; the default is 16. Unit tests need no container runtime or envtest assets.

E2e uses Kind 0.32.0 and a digest-pinned Kubernetes 1.36.1 node, a private kubeconfig, an explicit context, a temporary deployment overlay, and PostgreSQL in its own namespace. On macOS, use OrbStack's Docker-compatible runtime. The runner builds the project Dockerfile by default; `EPCO_E2E_BUILD_MODE=local` instead packages a locally cross-compiled manager for quicker local smoke checks. Tests cover deployment, health, protected metrics, SQL readiness, CRD CRUD, and PostgresCluster readiness recovery after credential Secret changes. RDS verification remains separate work.

Each runner prints its random seed. Set `EPCO_TEST_SEED` to replay it and `EPCO_ARTIFACTS_DIR` to choose the report directory (default under ignored `artifacts/`). Failure diagnostics are retained before teardown; runners handle SIGINT/SIGTERM and clean only resources owned by that run. CI separates unit/race, envtest, generation, PostgreSQL 15/16 integration, and PostgreSQL 16 e2e. Scheduled/manual integration covers PostgreSQL 15-18 and repeated seeds. Reports and diagnostics are uploaded even on failure.

Use the pinned Kubebuilder for subsequent scaffolding. For example, `mise exec -- kubebuilder create api --group postgresql --version v1alpha1 --kind PostgresCluster --resource --controller` creates the initial type and controller stubs; their fields and behavior must then follow the accepted API baseline. Regenerate derived files by running `mise run generate` followed by `mise run manifests` instead of editing them manually.

Local agent implementation plans belong in `.agent-plans/`, which is excluded from Git. Create this directory in a fresh checkout before writing a plan. Plans and all repository documentation use English and refer to the accepted API baseline.
