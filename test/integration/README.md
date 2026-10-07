# PostgreSQL integration fixtures

Run `mise run test-integration`. OrbStack or another Docker-compatible daemon must
be available. `EPCO_PG_VERSION=15|16|17|18` selects a pinned patch image; 16 is the
default. `EPCO_TEST_SEED` replays randomization. `EPCO_ARTIFACTS_DIR` selects the
artifact directory, including JUnit and failure logs.

The suite uses one disposable container on a random loopback port. Every spec gets
unique database and login names. The clients require TLS and verify a generated
suite CA. Fixture clients expose a database administrator and a restricted login;
catalog assertions and SQL execution both check grant/revoke behavior. SQL
readiness uses an authenticated `SELECT 1` over the same TLS connection as tests.

Production PostgresCluster client tests cover all six TLS modes, transport order
and fallback, hostname and CA verification, required client certificates,
incorrect credentials, read-only sessions, recovery after repaired inputs, and
connection cleanup after cancellation. Fixture certificate keys are generated
per run. TLS policy changes use suite-owned isolated PostgreSQL resources.

`Start(ctx, Options{...})` and `Server.NewFixture(ctx)` are untagged Go helpers, so
future controller integration tests can combine them with `test/envtest` directly.
Call `Fixture.Close` after assertions, and on failure save `Server.Diagnostics`
before `Server.Close`. Clients can be closed repeatedly. Cleanup deletes the exact
labelled container and fixture database/role names. It leaves unrelated Docker
resources untouched. The runner also handles failed startup, SIGINT and SIGTERM.

Unit fixture tests do not start Docker. Runtime specs require the `integration`
build tag. Controllers are not simulated here. PostgreSQL 15-18 is a test matrix;
Amazon RDS requires testing against a real RDS instance separately.
