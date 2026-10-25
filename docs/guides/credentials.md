# Passwords and connection Secrets

A `DatabaseRole` can use a supplied `kubernetes.io/basic-auth` Secret or opt into one-time password generation. The source Secret's username must match the role's effective SQL name. Generated credentials are saved before SQL changes so retries reuse the same password. Removing a password source does not implicitly rotate the SQL password.

Supplied password Secrets are never deleted by EPCO. A generated source is retained with the role's retain reclaim policy and removed with delete only after successful SQL role deletion. If an initialized generated source disappears, reconciliation reports `CredentialsMissing`; it does not silently generate a replacement password.

`Database.connectionSecrets` exports requested connection information for a role and database. Each output belongs to the Database namespace. Its Secret data includes `username`, `user`, `password`, `host`, `port`, `dbname`, `uri`, `fqdn-uri`, `jdbc-uri`, `fqdn-jdbc-uri`, `sslmode`, and `pgpass`; with a configured CA it also includes `ca.crt` and `sslrootcert`. Administrative credentials and private keys are not included.

An initial output may be published before the role, database, or requested permissions are ready, allowing a migration job to retry while EPCO reconciles SQL. Existing outputs rotate only after PostgreSQL accepts the current password. Ready remains false until SQL state and requested outputs are confirmed. An endpoint change refreshes outputs after the role/password, database, and CONNECT access are confirmed on the new endpoint.

Removing an output entry deletes only the derived Secret EPCO owns; it leaves SQL access and the password source unchanged. Deleting a Database with retain keeps its output Secrets without further updates. With delete, EPCO removes outputs after successful SQL database cleanup. The output's `ownerReferences` do not cause Kubernetes garbage collection when the Database is retained.

See the [generated-password microservice example](../../examples/microservice/service-owner-and-admin-readonly.yaml), the [supplied-password example](../../examples/microservice/orders-service-password-provided.yaml), and the [API reference](../reference/api.md).
