# Connections and TLS

`ExternalPostgres` describes a connection to an existing PostgreSQL server. Its administrative credential and optional CA or client-certificate Secrets are local to the descriptor's namespace. Database and DatabaseRole resources refer to the descriptor using `cluster.name` and, for a remote namespace, `cluster.namespace`.

The default port is `5432`, the administrative database is `postgres`, and TLS mode is `verify-full`. EPCO supports PostgreSQL TLS modes and can use system trust or a CA supplied by a local Secret. Client certificates use a local `kubernetes.io/tls` Secret. Consult the [API reference](../reference/api.md) for field names and defaults.

EPCO watches descriptor inputs and referenced Secrets. Credential, certificate, host, or port changes trigger dependent reconciliation against the current endpoint. EPCO does not copy data or migrate objects from a previous server. A Ready `ExternalPostgres` confirms its administrative connection and writable-state check; it does not prove that every requested application operation is permitted.

Administrative sessions protect catalog queries with a restricted search path. Install only trusted PostgreSQL extensions and use suitable administrative credentials. See the [connection example](../../examples/microservice/postgres-connection.yaml).
