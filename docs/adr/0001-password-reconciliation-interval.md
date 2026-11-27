# ADR 0001 - Use the existing interval for password reconciliation

**Status** Accepted

## Decision

Apply managed passwords during role reconciliation. `ExternalPostgres.spec.reconcileInterval` schedules periodic reconciliation of the descriptor, its roles, and its databases. The default is five minutes. Relevant events, such as a password Secret update, can enqueue a role sooner.

## Rationale

Applying the desired password on each successful reconcile corrects an out-of-band change even when the administrator cannot read the SCRAM verifier. The configured interval controls the normal repair cadence. Enforcing a one-hour minimum across restarts would delay repair at the default interval and require durable write history. An authentication probe would add a new connection per check and require a login-capable role and a suitable database.

## Measurements

Disposable PostgreSQL 18.6 over local TCP with TLS and synchronous commit enabled. Two runs of 200 operations per case used one client. The password update supplied a precomputed SCRAM verifier over an open admin connection. The login case required SCRAM and opened a fresh connection without executing SQL.

| Operation | Mean latency, runs 1 / 2 | WAL bytes per run, runs 1 / 2 |
| --- | ---: | ---: |
| `ALTER ROLE ... PASSWORD` | 5.301 / 4.780 ms | 44,832 / 47,992 |
| Fresh TLS/SCRAM login | 13.269 / 9.168 ms | 0 / 0 |

The benchmark measured wall-clock latency and WAL on the local fixture. Server CPU and production latency were outside its scope.
