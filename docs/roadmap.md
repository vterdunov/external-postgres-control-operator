# Project roadmap

Updated: 2026-10-10. This roadmap tracks delivery; detailed plans belong in gitignored `.agent-plans/YYYY-MM-DD-topic.md`. The [API baseline](api-proposal.md), [namespace access](cross-namespace-access.md), and [SQL ownership](sql-object-ownership.md) remain authoritative. See [implementation status](README.md#implementation-status) and [development](development.md) for runtime coverage and checks.

## Established foundations

**F0 - Foundations.** Result: agreed API, types/generated CRDs/admission, PostgresCluster readiness, isolated suites/CI, and architecture checks. Gate: implementation and coverage are documented in linked guides and checked by the required suites. Role/database runtime is tracked in R1/R2.

## Delivery stages

Plan stages in this order; detailed plans define increments and checks.

**R1 - Roles and passwords.** Result: attributes, both membership policies, parameters with persistent managed cleanup, observation/reclaim, and retained credential reuse. Supplied/generated sources use persist-before-SQL, latest-source retries/rotation, and no implicit rotation after source loss. Include authorization/conflicts, protected roles, dependencies, cancellation, and restricted administration. Gate: SQL/Secret tests prove updates, cleanup, recovery, rotation, and retain/delete.

**R2 - Databases and schemas.** Result: create-or-update, mutable/creation-only properties, owners, target connections, observation, explicit absence, reclaim, finalizers, and shared authorization/ownership integration. Gate: restricted-administrator tests prove retries, owner changes, schema lifecycle, protected objects, and blocked deletion without destructive fallback.

R1/R2 implement accepted decisions: Namespace LabelSelector authorization; cross-namespace CR/status conflicts with UID exclusion and canonical descriptor/type/SQL-name keys; ordinary leader Lease, per-key serialization, and cancellation. Reconcile current endpoints without UID pinning/data migration. Denied namespaces pause mutations, exports, and finalizers until authorization returns. No ownership ConfigMap or redesign stage is required.

**R3 - Declarative grants and defaults.** Result: database/schema/table/sequence ACLs; global/schema defaults for tables, sequences, functions, and types; named-table LLM access. Persist managed grants, pending changes, and object/grantor bindings in live-CR status before mutations; omission revokes managed grants and explicit absent enforces cleanup. Gate: reread ACLs confirms results, preserves operator CONNECT on the current target, and proves restart/spec-change recovery, rename/owner changes, and new-object defaults. CR recreation starts fresh history; old retained grants require explicit absent.

**R4 - Application connection Secrets.** Result: initial publication before blocking SQL/ACL conditions, while Ready remains false until all requirements succeed. Existing outputs rotate after SQL accepts the current source; endpoint refresh confirms role/password, database, and CONNECT. Pending tables for another role do not block refresh. Gate: tests prove namespace-local references, provenance/conflicts/retained reuse, TLS/URI escaping, early migration consumption, recovery, and exclusion of administrative credentials.

**R5 - Extensions.** Result: extension version/schema and present/absent reconciliation. Omission retains the extension; no implicit destructive operations. Gate: version/capability and restricted-administrator tests confirm supported changes and clear unsupported/dependency failures.

**R6 - Complete installed scenarios.** Result: PostgreSQL 15-18 coverage for migrations, runtime DML, read-only users, separate owner/migrator, named-table access, and two application namespaces. Gate: installed-operator scenarios verify recovery, source/output rotation, leader changes/cancellation, conflicts, and authorization removal/resumption with pending finalizers. Use isolated environments and explicit Kubernetes contexts.

**R7 - Delivery and controlled pilot.** Result: existing image, Kustomize/installer delivery, operations, and pilot documentation. Gate: isolated self-managed PostgreSQL and real RDS 15/16+ evidence establishes capability limits for pilot review. This roadmap authorizes neither provisioning nor production deployment.

## Scope boundaries

FDW/server runtime is excluded; API types/CRDs retain their fields. Requests report `UnsupportedFeature` with `Ready=False`. Each implemented controller must neither silently ignore requested unimplemented behavior nor mark it Ready. IAM operator authentication and exports are outside project scope; ordinary `rds_iam` membership remains role configuration. Explicit membership-option fields and post-CR-deletion ACL-history restoration remain deferred. No separate ownership/grant-history stores or SQL-comment journals, strict SQL fencing, application restarts, provisioning, or data migration.

## Progress and handoffs

Table statuses are authoritative. Keep IDs stable. Update status (`TODO`, `IN_PROGRESS`, `BLOCKED`, `DONE`) and records as evidence changes; explain `n/a`. Close stages only with gate evidence and independent review. Distinguish merged code, schema acceptance, and deployed behavior.

| ID | Status | Detailed plan | PR | Evidence | Blocker | Next step |
| --- | --- | --- | --- | --- | --- | --- |
| F0 | DONE | n/a: established | [#1 tests](https://github.com/vterdunov/external-postgres-control-operator/pull/1), [#2 PostgresCluster](https://github.com/vterdunov/external-postgres-control-operator/pull/2), [#3 architecture](https://github.com/vterdunov/external-postgres-control-operator/pull/3) | Linked guides/suites | None | Maintain coverage |
| R1 | DONE | Reviewed local R1 plan and independent acceptance | [#5 roles and passwords](https://github.com/vterdunov/external-postgres-control-operator/pull/5), merged | check; envtest 41/41; PG15 49 + 2 expected skips, PG16-18 51/51 each; Kind 7/7 | None | Maintain role coverage |
| R2 | DONE | Reviewed local R2 plan, architecture corrections and independent acceptance | [#6 databases and schemas](https://github.com/vterdunov/external-postgres-control-operator/pull/6), merged | check; envtest 51/51; PG15 85 + 2 expected skips, PG16-18 87/87 each; prior Kind 10/10; recovery/concurrency replay | None | Maintain lifecycle coverage |
| R3 | DONE | Reviewed local R3 plan, behavioral TDD and independently accepted Go/operator/DBA architecture corrections | Pending: local branch | Standard check; envtest 55/55; PG15 131 + 6 expected PG16-only skips, PG16-18 137/137 each; Kind 11/11; all five corrections independently accepted | None | Maintain ACL coverage; plan R4 |
| R4 | TODO | Pending | Pending | Pending | None recorded | Plan after R3 |
| R5 | TODO | Pending | Pending | Pending | None recorded | Plan after R4 |
| R6 | TODO | Pending | Pending | Pending | None recorded | Plan after R5 |
| R7 | TODO | Pending | Pending | Pending | None recorded | Plan after R6 |

Feature plans assign separate test-author, implementation, and independent-validation agents. Test authors demonstrate failing behavioral tests before handoff. Implementation/review inspect CNPG and cite paths, approaches, and justified departures; write EPCO independently. Run scoped mise checks, fix failures/flakiness, keep suite support together, and never hand-edit generated files. R2 is merged in PR #6. R3 is complete locally on `feat/r3-declarative-grants`, including the five Go/operator/DBA architecture corrections, behavioral regression evidence, repeated full gates and independent acceptance. Its PR remains pending; installed evidence comes from disposable Kind. Production deployment and real RDS validation remain future delivery work.
