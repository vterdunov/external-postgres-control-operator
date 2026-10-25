# Project roadmap

This internal record tracks implementation gates and delivery evidence. It is not a promise of release availability or production deployment. Runtime coverage belongs to the [test suite guides](../test/).

| Stage | Status | Scope | Evidence or remaining gate |
| --- | --- | --- | --- |
| F0 | DONE | API, CRDs, connection readiness, CI and architecture checks | Foundations merged and covered by project checks |
| R1 | DONE | Roles, memberships, parameters, passwords and reclaim | PG15-18 and installed-controller evidence recorded in PR #5 |
| R2 | DONE | Databases, schemas, ownership and reclaim | PG15-18 and installed-controller evidence recorded in PR #6 |
| R3 | DONE | Declarative database, schema, table, sequence and default privileges | PG15-18 and installed-controller evidence recorded in PR #7 |
| R4 | DONE | Application connection Secrets and rotation | PG15-18 and installed-controller evidence recorded in PR #8 |
| R5 | DONE | Extension lifecycle and dependency handling | PG15-18 and installed-controller evidence recorded in PR #9 |
| R6 | DONE | Installed application and recovery scenarios | Native PostgreSQL 15-18: 823 passed plus 9 expected capability skips; Kind: 19/19 per major, plus 20 recovery/version checks |
| R7 | IN_PROGRESS | Independent image/chart releases, immutable assets, signatures, verified install and safe removal | Local chart lifecycle 26/26 and PG16 chart application 19/19; full managed RDS and PostgreSQL 15-18 capability acceptance remains pending |

Recorded local gates do not complete provider qualification. Full managed RDS and PostgreSQL 15-18 capability acceptance remains pending. R7 evidence and CI state must be refreshed from the release source before changing its status.

## Scope limits

FDW and server runtime reconciliation is unsupported and must report `UnsupportedFeature` with `Ready=False`. IAM operator authentication and credential export, production provisioning, and data migration are outside scope. The operator manages an existing PostgreSQL server and retains CRDs when its controller is removed.
