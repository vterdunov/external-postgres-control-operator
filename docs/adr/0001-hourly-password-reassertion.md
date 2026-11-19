# ADR 0001 - Hourly password reassertion with restricted catalog access

**Status** Accepted

## Decision

When the administrative connection can read the SCRAM verifier, compare it with the desired password and preserve a match. With restricted access, reuse a committed write while the exact inputs, SQL role OID, and server startup time match. Reassert the password on the first successful reconcile at or after one hour since that write. Changes to the password source, role identity, or target invalidate the evidence immediately.

## Rationale

A direct password change can leave the Secret version and SQL role OID unchanged. Periodic reassertion repairs this drift without access to the verifier or an application login. At the default five-minute reconcile interval, this reduces steady-state password writes from about 288 to 24 per role per day. An out-of-band change can persist until the hourly deadline and the next successful reconcile. A fresh SCRAM login every five minutes would add about 288 connections per role per day and requires a login-capable role and a suitable database.

## Measurements

Disposable PostgreSQL 18.6 over local TCP with TLS and synchronous commit enabled. Two runs of 200 operations per case used one client. The password update supplied a precomputed SCRAM verifier over an open admin connection. The login case required SCRAM and opened a fresh connection without executing SQL.

| Operation | Mean latency, runs 1 / 2 | WAL bytes per run, runs 1 / 2 |
| --- | ---: | ---: |
| `ALTER ROLE ... PASSWORD` | 5.301 / 4.780 ms | 44,832 / 47,992 |
| Fresh TLS/SCRAM login | 13.269 / 9.168 ms | 0 / 0 |

The benchmark measured wall-clock latency and WAL on the local fixture. Server CPU and production latency were outside its scope.
