# External PostgreSQL Control Operator

An operator for managing roles, databases, and access across PostgreSQL clusters, including AWS RDS. The primary use case is a microservice database with a service user and a separate read-only user for an admin UI. The accepted initial API follows CloudNativePG `DatabaseRole` and `Database`, adding an external `PostgresCluster` connection descriptor, inline permissions, password generation, and application connection Secrets.

The [API baseline](docs/api-proposal.md) was accepted on 2026-10-07 and draws on CNPG, movetokube, and CNPG discussions #10826/#7872. It is the reference for implementation. [SQL object ownership](docs/sql-object-ownership.md) follows CNPG's CR/status checks, extended across namespaces with serialized reconciliation; no separate ownership ConfigMap is required. Go API types and CRDs are generated for `postgresql.epco.io/v1alpha1`; controllers are not implemented.

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

## Development

The project scaffold was initialized with [Kubebuilder v4.16.0](https://github.com/kubernetes-sigs/kubebuilder/releases/tag/v4.16.0) and the `go/v4` plugin. It uses Go 1.27.1 or newer, controller-runtime v0.25.0, and Kubernetes v0.37.0 Go libraries. The Go module is `github.com/vterdunov/external-postgres-control-operator`; the scaffold project name is `epco` and its API domain is `epco.io`.

The manager registers the `PostgresCluster`, `DatabaseRole`, and `Database` API types from `api/v1alpha1`. Generated CRDs are in `config/crd/bases`, with DeepCopy methods generated alongside the types. Controllers are not implemented, so running the manager does not manage PostgreSQL objects. The examples remain API design references.

Install [mise](https://mise.jdx.dev/) and use the pinned Go and Kubebuilder versions from `mise.toml`:

```sh
mise trust
mise install
mise run build # Generate manifests/code, format, vet, and build bin/manager
mise run test  # Generate manifests/code, format, vet, and run unit tests
mise run lint # Verify linter configuration and run the configured linters
mise run check # Run all checks, including rendering deployment manifests
```

Other Makefile targets can be run through `mise exec -- make <target>`.

The Makefile installs pinned development tools into the ignored `bin/` directory. `make test` also downloads the matching envtest binaries; controller unit tests will use them once APIs are added. End-to-end tests require Docker, Kind, and kubectl and run with `make test-e2e` against a dedicated Kind cluster.

Use the pinned Kubebuilder for subsequent scaffolding. For example, `mise exec -- kubebuilder create api --group postgresql --version v1alpha1 --kind PostgresCluster --resource --controller` creates the initial type and controller stubs; their fields and behavior must then follow the accepted API baseline. Regenerate derived files with `mise exec -- make manifests generate` instead of editing them manually.

Local agent implementation plans belong in `.agent-plans/`, which is excluded from Git. Create this directory in a fresh checkout before writing a plan. Plans and all repository documentation use English and refer to the accepted API baseline.
