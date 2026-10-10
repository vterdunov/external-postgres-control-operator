# Documentation

The [API baseline](api-proposal.md) was accepted on 2026-10-07. Its filename is retained for existing links; it defines the accepted contract. Future extensions are marked explicitly. This index records what the checkout implements, separately from that contract.

## Implementation status

| Area | Current checkout |
| --- | --- |
| PostgresCluster, DatabaseRole, Database | Go types, generated CRDs, defaults, and admission rules; CRD admission tests are in [test/envtest/crd_test.go](../test/envtest/crd_test.go) |
| PostgresCluster runtime | Implemented authentication/writable-state check, TLS, local Secret watches, bounded retries, and Ready status |
| DatabaseRole runtime | Implemented role attributes, both membership policies, targeted parameter cleanup, supplied/generated password application, observation, and retain/delete reclaim |
| Database runtime | Implemented database/schema/extension lifecycle, observation, reclaim, database/schema/table/sequence ACLs, named-table allowlists, creator-specific global/schema defaults, and application connection Secrets |
| FDW/server runtime | API fields retained; runtime excluded by the [roadmap scope](roadmap.md#scope-boundaries). Requests must report UnsupportedFeature/Ready=False |
| Cross-namespace consumers and SQL ownership | DatabaseRole and Database implement Namespace selectors, dependency watches, recorded-manager conflicts with UID exclusion, and cancelable per-key serialization |
| Managed-state recovery | Both SQL controllers persist sticky management after confirmed lifecycle success. DatabaseRole records generated-source initialization, exact password-application receipts, and membership/parameter cleanup; Database records output publication/cleanup history, target-scoped ACL bindings, pending changes, and owner transitions before SQL |
| RDS | Accepted SQL capability requirements and examples; compatibility with a real RDS instance has not been established by the local PostgreSQL suites |

PostgresCluster Ready confirms only its administrative connection check. DatabaseRole Ready confirms role reconciliation; Database Ready requires the database, listed schemas and extensions, declared ACLs/defaults, requested connection outputs, and required managed cleanup to be confirmed on the current connection. Initial outputs can exist before SQL is reachable or named tables exist. Existing outputs refresh after the current source is applied to SQL and role/database/CONNECT eligibility is confirmed; unrelated pending permissions or extensions do not block an eligible refresh. Confirmed lifecycle management reserves the SQL name even while a named table or extension is pending; it does not establish full Ready or appliedGeneration. Observation checks requested extension presence, version and schema, reports Observed with applied unset when they match, and creates no outputs or ACL management history. FDW/server behavior reports UnsupportedFeature/Ready=False, including explicit database absence. A generated role password source or an initial application output does not imply SQL readiness.

Extensions retain unspecified installed versions and schemas. Explicit changes require native PostgreSQL capabilities and existing authority; omission retains the extension, while explicit absence uses RESTRICT. See [extension reconciliation](api-proposal.md#extension-reconciliation) for prerequisites, ordering and failure behavior.

ACL recovery is scoped to the live CR UID and selected endpoint. Journal checkpoints include the whole-resource 768 KiB budget; unresolved identity or owner-merge provenance preserves cleanup responsibility with Ready=False. Durable history is retained in full. Optional permission reports are bounded; `permissionsOmitted` counts details omitted from status, with named/database/schema/default outcomes prioritized over broad relation details. Effective access is reported separately from managed direct grants. See [ACL status and constraints](api-proposal.md#lifecycle-ownership-and-status). The [roadmap](roadmap.md) records validation gates; source and authored tests alone do not establish a completed release or deployment.

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
