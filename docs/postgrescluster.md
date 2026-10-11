# PostgresCluster readiness

Create a namespace-local `kubernetes.io/basic-auth` Secret with a nonempty `username` and a `password` key. An empty password is accepted. Reference it from `spec.credentialsSecret.name` and set `spec.connection.host`. Defaults are port `5432`, database `postgres`, TLS `verify-full`, reconcile interval `5m`, connect timeout `10s`, and statement timeout `30s`. The SQL timeout is rounded up to milliseconds; values above PostgreSQL's maximum of `2147483647` milliseconds are reported as `InvalidConfiguration`.

All six PostgreSQL TLS modes are supported. A `caSecret` supplies PEM certificates from the selected local Secret key and replaces system trust; without it the operator uses system roots. A client certificate must come from a local `kubernetes.io/tls` Secret with a valid `tls.crt`/`tls.key` pair. Mode `disable` ignores TLS Secret references. Mode `require` with a supplied CA verifies the certificate chain, following [PostgreSQL SSL semantics](https://www.postgresql.org/docs/18/libpq-ssl.html).

The controller watches referenced Secrets, so creating a missing Secret, rotating credentials or certificates, or deleting a Secret triggers a check. Changes to connection inputs trigger a check too. Successful checks repeat at `reconcileInterval`; expected failures retry within `min(30s, reconcileInterval)`. Kubernetes API failures use controller backoff.

`Ready=True` and `status.applied=true` record a successful check and update `appliedGeneration` and `lastSuccessfulReconcileTime`. Failure sets `Ready=False` and `applied=false` while retaining the last successful generation and time. `observedGeneration` records the checked spec. Error reasons are `SecretNotFound`, `InvalidSecret`, `InvalidConfiguration`, `ConnectionUnavailable`, and `NotWritable`; status does not expose credentials or raw driver errors. Results are discarded when authoritative reads show changed inputs or a conflicting status write.

The [namespace authorization contract](cross-namespace-access.md) will be enforced by DatabaseRole and Database reconciliation. This connection check does not depend on the selector.

## Scope and implementation

The manager runs only the PostgresCluster reconciler. Each check opens a fresh connection and runs a read-only query for recovery and transaction read-only state. It performs no SQL object or credential mutations and needs no CREATEDB, CREATEROLE, or SUPERUSER privileges. Ready confirms this administrative connection check; it does not confirm permissions to manage application SQL objects or application readiness.

The default deployment enables Lease leader election through `--leader-elect`. The CLI flag defaults to false, including `mise run run`; enable it explicitly when running multiple managers. SQL-object serialization described in the [ownership design](sql-object-ownership.md) remains a requirement for the future role/database controllers.

Sources: [API types](../api/v1alpha1/postgrescluster_types.go), [controller](../internal/controller/postgrescluster_controller.go), [PostgreSQL client](../internal/postgres/cluster.go), and [manager wiring](../cmd/main.go). Unit tests cover configuration and stale-result handling; envtest covers real API admission and Secret watches. [Private integration and E2E validation](development.md#private-validation) cover real PostgreSQL connections and installed-controller recovery.
