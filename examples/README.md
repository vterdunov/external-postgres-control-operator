# Examples

These manifests use the current `postgresql.epco.io/v1alpha1` API. They are
reference configurations; replace example hostnames and prepare the referenced
Secrets and PostgreSQL objects for your environment before applying them. No
manifest contains a usable credential.

## Prerequisites and apply order

Install the operator and CRDs first. Create the `ExternalPostgres` credentials
Secret in the same namespace as its resource, with `username` and `password`
keys (type `kubernetes.io/basic-auth` is suitable). With TLS omitted, the
connection uses `verify-full` and system trust. If the server needs a custom CA,
create the referenced Secret and key before applying the resource. If client
certificate authentication is configured, create the referenced Secret with
`tls.crt` and `tls.key`.

Apply the `ExternalPostgres` resource before its `DatabaseRole` and `Database`
resources. Prepare any password Secrets referenced by `passwordSecret` first.
For generated passwords, the operator creates the named Secret. Some examples
refer to existing SQL objects or roles, such as database owners and parent
roles; create those objects or apply the corresponding role resources first.

Cross-namespace resources require the `ExternalPostgres` namespace's
`spec.access.namespaceSelector` to select the namespaces containing the
`Database` and `DatabaseRole` resources. The example selector uses the
Kubernetes namespace-name label. Apply the connection descriptor before the
resources in those namespaces.

## Scenarios

- [Minimal setup](scenarios/minimal.yaml) creates a connection, login role, and
  database with the default policies.
- [Full database and role configuration](scenarios/full.yaml) demonstrates
  common role, database, schema, ACL, default-privilege, and extension fields.
- [AWS RDS](scenarios/aws-rds.yaml) shows a custom CA and application roles;
  supply the referenced Secrets and ensure the `rds_iam` role exists if using
  the IAM example role.
- [Multiple PostgreSQL connections](scenarios/multi-cluster.yaml) applies the
  same SQL role name to two different connection descriptors. Create both
  descriptors and referenced parent roles first.
- [Read-only application access](scenarios/readonly.yaml) grants read access
  to current and future tables and sequences. The owner role must exist.
- [LLM read-only ACL progression](scenarios/llm-readonly.yaml) grants SELECT
  on named tables. The variants
  [with an absent grant](scenarios/llm-readonly-with-absent-grant.yaml) and
  [after removing a grant](scenarios/llm-readonly-after-removal.yaml) are
  alternative snapshots for demonstrating explicit revocation and an updated
  desired state. Apply one snapshot at a time; they are not a multi-file bundle.

## Application examples

- [Service-owned database with a read-only admin](microservice/service-owner-and-admin-readonly.yaml)
  gives the service role database ownership.
- [Separate migration and service roles](microservice/separate-migrations-and-admin-readonly.yaml)
  uses a non-login owner, migration role, and runtime roles.
- [Connection descriptor](microservice/postgres-connection.yaml) is a starting
  point for the microservice examples.
- [Provided application password](microservice/orders-service-password-provided.yaml)
  shows the Secret shape for `passwordSecret`; replace the placeholder password
  before applying.
- [Service deployment](microservice/orders-service-deployment.yaml) and
  [read-only admin deployment](microservice/orders-admin-deployment.yaml) show
  how a workload can consume the exported connection Secret. Replace the
  illustrative container images before using them.
- [Cross-namespace connection](cross-namespace/infra-connection.yaml),
  [orders resources](cross-namespace/orders-service-and-admin-readonly.yaml),
  and [billing resources](cross-namespace/billing-service-and-admin-readonly.yaml)
  demonstrate a shared connection descriptor with namespace-scoped resources.

The standalone resources in these examples often reference the same illustrative
connection name. Apply them only with a matching `ExternalPostgres` resource,
or adapt the names and namespaces together.
