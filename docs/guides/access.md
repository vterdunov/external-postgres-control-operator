# Cross-namespace access

All EPCO resources are namespaced. A Database or DatabaseRole may reference an `ExternalPostgres` in another namespace when that descriptor's `spec.access.namespaceSelector` selects the consumer namespace. Without an access selector, local references are allowed and remote references are denied. A configured selector applies to local and remote references.

Administrative credentials stay in the descriptor namespace. Password sources, connection outputs, and role references stay in their application resource namespaces. An exported CA may be included in a connection Secret; administrative passwords and private keys are never exported.

When a namespace is no longer authorized, EPCO stops SQL changes, password generation, output refresh/removal, and finalizer cleanup for affected resources. Existing SQL objects and Secrets remain unchanged until access is restored. Restored access resumes normal reconciliation. Namespace authorization does not provide SQL privilege isolation: Kubernetes namespaces share the PostgreSQL server's role and database catalogs.

One live managing resource may own a SQL role or database for a canonical connection. A competing resource receives an ownership conflict before SQL changes or initial connection export. A role and database may share a SQL name because they are distinct PostgreSQL object types. See the [two-namespace example](../../examples/cross-namespace/infra-connection.yaml) and the [API reference](../reference/api.md).
