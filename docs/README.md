# Documentation

The [API baseline](api-proposal.md) was accepted on 2026-10-07. Its filename is retained for existing links; it defines the accepted contract. Future extensions are marked explicitly. This index records what the checkout implements, separately from that contract.

## Implementation status

| Area | Current checkout |
| --- | --- |
| PostgresCluster, DatabaseRole, Database | Go types, generated CRDs, defaults, and admission rules; CRD admission tests are in [test/envtest/crd_test.go](../test/envtest/crd_test.go) |
| PostgresCluster runtime | Implemented authentication/writable-state check, TLS, local Secret watches, bounded retries, and Ready status |
| DatabaseRole runtime | Not implemented: role attributes/memberships, password application/generation, observation, and reclaim cleanup |
| Database runtime | Not implemented: SQL database/child objects, ACL reconciliation, connection exports, observation, and reclaim cleanup |
| FDW/server runtime | API fields retained; runtime excluded by the [roadmap scope](roadmap.md#scope-boundaries). Requests must report UnsupportedFeature/Ready=False |
| Cross-namespace consumers and SQL ownership | Accepted contracts and schema fields; authorization watches, conflict detection, and SQL-object serialization await the role/database controllers |
| Managed-state recovery | Current status exposes common results, role password Secret resourceVersion, and per-permission outcomes. Durable managed-grant/pending-change/object-grantor bookkeeping is required by the contract but is not yet represented in the Database status schema or implemented |
| RDS | Accepted SQL capability requirements and examples; compatibility with a real RDS instance has not been established by the local PostgreSQL suites |

PostgresCluster Ready confirms only its administrative connection check. DatabaseRole/Database manifests can be admitted without triggering reconciliation. Application Secret existence and overall SQL readiness have separate meanings under the accepted export contract.

## Reading guide

| Document | Authoritative scope |
| --- | --- |
| [Project roadmap](roadmap.md) | Selected runtime scope, delivery stages, gates, and progress records |
| [API baseline](api-proposal.md) | Accepted fields/defaults, credentials, connection outputs, declarative ACLs, lifecycle, and deferred public API |
| [PostgresCluster readiness](postgrescluster.md) | Current connection runtime, status reasons, and operational limits |
| [Cross-namespace access](cross-namespace-access.md) | Namespace authorization, selector semantics, denial/resumption, and resource placement |
| [SQL object ownership](sql-object-ownership.md) | SQL-name management keys, recorded-manager conflicts, retry behavior, endpoint changes, and leadership |
| [Examples](examples.md) | Resource-set selection and prerequisites |
| [Development](development.md) | Tooling, architecture, test workflow, and generation/deployment commands |
| [Integration suite](../test/integration/README.md), [e2e suite](../test/e2e/README.md) | Suite-specific fixtures, runtime prerequisites, options, and coverage |

## Keeping docs current

For API changes, update the [Go types](../api/v1alpha1), generate CRDs/DeepCopy files using mise, and update the API baseline and affected examples together. Runtime claims must agree with [manager wiring](../cmd/main.go), [controllers](../internal/controller), and their behavioral tests. Update the implementation table when a controller stage lands.

Keep detailed rules in the authoritative document above and link to them elsewhere. Historical alternatives are not additional requirements. CNPG comparisons describe pinned local source snapshots; upstream issue status is a dated observation. Local agent plans live in gitignored `.agent-plans/` and do not establish implemented behavior.
