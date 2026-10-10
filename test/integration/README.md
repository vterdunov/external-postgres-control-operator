# PostgreSQL integration fixtures

See the [development guide](../../docs/development.md#test-suites) for the full test workflow and [implementation status](../../docs/README.md#implementation-status) for the current controller scope.

Run `mise run test-integration`. OrbStack or another Docker-compatible daemon must
be available. `EPCO_PG_VERSION=15|16|17|18` selects a pinned patch image; 16 is the
default. `EPCO_TEST_SEED` replays randomization. `EPCO_ARTIFACTS_DIR` selects the
artifact directory, including JUnit and failure logs.

Application connection tests use actual libpq 18.6 and pgJDBC 42.7.10 parsers and
SQL sessions. Install the pinned Java tool through mise and provide a C compiler.
On macOS, install libpq 18.6 through Homebrew; Linux fixtures obtain its library
and runtime dependencies from the pinned PostgreSQL 18.6 image. The suite fetches
the JDBC jar from Maven and verifies its SHA256. `EPCO_TEST_JDBC_JAR` can select a
local jar with the same verified checksum. Credentials reach these clients only
through private stdin and files, with child diagnostic output suppressed.

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
controller integration tests combine them with `test/envtest` directly. The runner prepares pinned envtest assets; an explicit `KUBEBUILDER_ASSETS` is honored.
Call `Fixture.Close` after assertions, and on failure save `Server.Diagnostics`
before `Server.Close`. Clients can be closed repeatedly. Cleanup deletes the exact
labelled container and fixture database/role names. It leaves unrelated Docker
resources untouched. The runner also handles failed startup, SIGINT and SIGTERM.

Unit fixture tests do not start Docker. Runtime specs require the `integration`
build tag. Production DatabaseRole SQL tests cover attributes, memberships/grantors, parameters, passwords, restricted administration, and dependency-blocked deletion. Database tests cover creation-only/versioned locale capabilities, changed-only mutable properties, owner transfer without table reassignment, target connections, schema owners/absence, protected objects, connected-session deletion blocks and cancellation. Restricted fixtures explicitly provision CREATEDB, SET/INHERIT and object CREATE only where needed; production never grants missing capabilities. Composed tests use a real API and SQL server to cover status/checkpoint failures, ordinary retry after committed or unknown database creation, never-recorded deletion safety, restart cleanup, rotation during Apply, absent-name reservations and endpoint A -> B -> A recovery. PostgreSQL 15-18 is a test matrix;
Amazon RDS requires testing against a real RDS instance separately.

R3 permission specs cover database/schema grants, PUBLIC and operator CONNECT, disabled-target database ACLs, broad tables/sequences and SQL-only new relations, actual-creator global/schema defaults, named-table allowlists, missing tables, rename and DROP/recreate, and restricted grantor/catalog confirmation. Composed recovery specs cover failed pre-SQL checkpoints, committed SQL with failed status, current-spec withdrawal after restart, retain/recreate history, observation/authorization pauses, endpoint return, owner transitions and the whole-resource journal budget. Independent validation specs exercise inherited grantor ambiguity, NULL versus empty ACLs, public-schema owner authority and unobserved owner merges. These are authored coverage descriptions; the [roadmap](../../docs/roadmap.md) records completed validation gates.
