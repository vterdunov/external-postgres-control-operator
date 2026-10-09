# Documentation

The [API baseline](api-proposal.md) was accepted on 2026-10-07. Its filename is retained for existing links; it defines the accepted contract. Future extensions are marked explicitly. This index records what the checkout implements, separately from that contract.

## Implementation status

| Area | Current checkout |
| --- | --- |
| PostgresCluster, DatabaseRole, Database | Go types, generated CRDs, defaults, and admission rules; CRD admission tests are in [test/envtest/crd_test.go](../test/envtest/crd_test.go) |
| PostgresCluster runtime | Implemented authentication/writable-state check, TLS, local Secret watches, bounded retries, and Ready status |
| DatabaseRole runtime | Implemented role attributes, both membership policies, targeted parameter cleanup, supplied/generated password application, observation, and retain/delete reclaim |
| Database runtime | Implemented create-or-update, database properties/owners, target connections, schema lifecycle, observation, explicit absence, and retain/delete finalizers; ACLs, connection exports and extensions remain pending |
| FDW/server runtime | API fields retained; runtime excluded by the [roadmap scope](roadmap.md#scope-boundaries). Requests must report UnsupportedFeature/Ready=False |
| Cross-namespace consumers and SQL ownership | DatabaseRole and Database implement Namespace selectors, dependency watches, recorded-manager conflicts with UID exclusion, and cancelable per-key serialization |
| Managed-state recovery | Both SQL controllers persist sticky management after confirmed success. DatabaseRole additionally persists generated-source initialization and endpoint-scoped membership/grantor and parameter cleanup intent before SQL. Database ACL bookkeeping remains pending |
| RDS | Accepted SQL capability requirements and examples; compatibility with a real RDS instance has not been established by the local PostgreSQL suites |

PostgresCluster Ready confirms only its administrative connection check. DatabaseRole Ready confirms role reconciliation; Database Ready confirms the database and listed schemas on the current connection. Observation reports Observed with applied unset. Requests for unimplemented permissions, defaults, connection outputs, extensions or FDW/server behavior report UnsupportedFeature/Ready=False, including explicit database absence. Application connection exports remain pending; a generated role password source does not imply SQL readiness.

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
