# External PostgreSQL Control Operator

EPCO manages roles, databases, and access on external PostgreSQL servers, including AWS RDS. Its primary use case is a microservice database with a service login and a separate read-only login for an admin UI. The accepted API draws on CloudNativePG DatabaseRole and Database, adding an external PostgresCluster connection descriptor, inline permissions, password generation, and application connection Secrets.

The three `postgresql.epco.io/v1alpha1` resources have Go types and generated CRDs. The manager currently implements **PostgresCluster connection readiness**. DatabaseRole and Database reconciliation, namespace authorization, SQL ownership, permissions, generated passwords, and application connection exports await implementation. Accepting their manifests does not apply those SQL changes. See the [documentation index and implementation status](docs/README.md).

## Documentation

| Guide | Purpose |
| --- | --- |
| [Project roadmap](docs/roadmap.md) | Delivery stages, completion gates, scope, and progress tracking |
| [Accepted API baseline](docs/api-proposal.md) | Resource fields, defaults, passwords, connection exports, permissions, and lifecycle contracts |
| [PostgresCluster readiness](docs/postgrescluster.md) | Implemented connection checks, TLS, Secret watches, retries, and status |
| [Cross-namespace access](docs/cross-namespace-access.md) | Shared infrastructure connections, namespace selectors, authorization removal, and team placement |
| [SQL object ownership](docs/sql-object-ownership.md) | Canonical connection identity, competing CRs, retries, endpoint changes, and leadership |
| [Example guide](docs/examples.md) | Microservice variants, cross-namespace setups, RDS, and named-table access |
| [Development](docs/development.md) | Pinned tools, testing, architecture checks, generation, and deployment commands |

## Build and test

Install [mise](https://mise.jdx.dev/) and use the project's pinned tools:

```sh
mise trust
mise run --skip-tools tools-install
mise run build
mise run check
```

[Development](docs/development.md) lists the individual suites and runtime prerequisites. Cluster operations require an explicit `KUBE_CONTEXT`; the [API examples](docs/examples.md) describe the accepted target behavior and are not a complete working application deployment yet.
