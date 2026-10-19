# Cross-namespace access

Status: accepted as part of the initial [API baseline](api-proposal.md), 2026-10-07. DatabaseRole and Database implement namespace authorization and removal/resumption, including paused finalizers. ExternalPostgres connection readiness is implemented independently of this selector; see [implementation status](README.md#implementation-status).

## Use case and resource placement

The platform team owns one external PostgreSQL connection in an infrastructure namespace. Microservice teams create their Database and DatabaseRole resources in their application namespaces and consume their own connection Secrets. They do not receive the administrative SQL credentials. Namespaces are authorized using the standard Kubernetes LabelSelector. SQL name collisions use CNPG's model of CR/status checks, extended across namespaces, without a platform-maintained list of approved SQL names or separate ownership ConfigMaps.

| Namespace | Resources | Owner |
| --- | --- | --- |
| infra | ExternalPostgres, its administrative credentials Secret, its CA Secret | Platform team |
| orders | Database, DatabaseRole resources, password sources, connection outputs | Orders service team |
| billing | Independent Database, DatabaseRole resources, password sources, connection outputs | Billing service team |

This differs from CNPG's namespace-local DatabaseRole contract, which requires its Cluster and passwordSecret to be in the same namespace. [CNPG role management](https://cloudnative-pg.io/docs/devel/declarative_role_management/).

## Cluster reference

Database.spec.cluster and DatabaseRole.spec.cluster support an optional namespace:

```yaml
cluster:
  name: pg-prod
  namespace: infra
```

If namespace is omitted, it resolves to the referencing CR's namespace. Existing manifests keep their meaning. The stored cluster reference is immutable after acceptance, including whether namespace was omitted. A local reference cannot later be rewritten with an explicit namespace; set it at creation if needed. Connection settings in the referenced ExternalPostgres remain mutable. Updating its endpoint or recreating the descriptor under the same namespace/name selects the current connection without a stored-UID rebind approval; dependents are reconciled again and their connection outputs are refreshed. This does not migrate data or remove objects from the former server.

Only the cluster reference crosses namespaces in this extension. Database.connectionSecrets[].roleRef remains local to the database namespace. Application password sources remain local to their DatabaseRole. The administrative credentialsSecret and tls.caSecret are resolved in the ExternalPostgres namespace.

## Namespace selector

Naming a remote ExternalPostgres does not authorize its use. The platform team configures ExternalPostgres.spec.access.namespaceSelector with the standard metav1.LabelSelector shape. It selects Namespace objects by their metadata.labels, not labels on Database or DatabaseRole resources. Both matchLabels and matchExpressions are supported. [Kubernetes LabelSelector](https://kubernetes.io/docs/reference/kubernetes-api/definitions/label-selector-v1-meta/).

```yaml
access:
  namespaceSelector:
    matchExpressions:
      - key: kubernetes.io/metadata.name
        operator: In
        values:
          - orders
          - billing
```

The immutable kubernetes.io/metadata.name label is set automatically on Namespace objects and allows explicit name selection through the standard selector. [Namespace labels](https://kubernetes.io/docs/concepts/overview/working-with-objects/namespaces/#automatic-labelling).

Selection by a platform-managed label works as well:

```yaml
access:
  namespaceSelector:
    matchLabels:
      postgresql.epco.io/pg-prod: "true"
```

The selected Namespace needs that label:

```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: orders
  labels:
    postgresql.epco.io/pg-prod: "true"
```

Without an access block, local references retain the accepted baseline behavior and cross-namespace references are denied. With an access block, the selector applies to both local and remote referencing resources. An omitted or null namespaceSelector matches no namespaces; an explicit namespaceSelector: {} matches all namespaces, following standard LabelSelector semantics. Local namespaces have no automatic exemption from a configured selector. Namespace selection does not require advance registration of SQL database or role names.

Access is checked against the Namespace named by the referencing resource's metadata.namespace. It is not inferred from the operator's ServiceAccount or the GitOps controller's identity. Check authorization before ownership checks, SQL mutation, password generation, and connection export. Namespace label and selector updates trigger reevaluation; the periodic reconciliation also checks them. A rejected request reports AccessDenied without changing SQL objects or publishing new credentials.

## SQL names and ownership

PostgreSQL roles are global within a database cluster, and database names identify shared server objects. Kubernetes namespaces do not create separate PostgreSQL catalogs. [PostgreSQL database roles](https://www.postgresql.org/docs/current/database-roles.html).

The [SQL object ownership contract](sql-object-ownership.md#adaptation-for-external-postgresql) defines one recorded manager per canonical connection, object type, and SQL name across namespaces. A competing CR reports OwnershipConflict before SQL mutation or initial connection export. Existing SQL objects without a competing manager use create-or-update reconciliation. Namespace authorization precedes this conflict check.

For example, orders/Database orders and billing/Database orders can both be created as Kubernetes objects, but only one can manage SQL database orders on infra/pg-prod. DatabaseRole collisions use the same rule independently of database names. A role and database can share the same SQL name because their object types differ.

An already managed SQL object cannot be reconciled by a competing CR; a new SQL name needs no platform-side allowlist. When there is no competing live manager, existing SQL objects use CNPG-style create-or-update reconciliation without an explicit adoption policy.

Role attributes, SQL owner names, inRoles, and permission grantees retain their baseline meaning. The extended memberships API remains deferred. Namespace authorization alone does not grant memberships or administrative SQL attributes. For a concrete supported Database operation, missing service SET access may be acquired only for a recorded currently managed DatabaseRole on the same canonical descriptor, with both the Database namespace and role manager namespace authorized. A PostgreSQL 16+ schema-owner transfer may acquire narrowly required operator-recipient INHERIT from concrete actual owners under the same gates. Existing sufficient SQL authority needs no new grant or manager eligibility requirement. Application roles receive no additional administrative attributes, memberships or CREATE grants for this repair. See [administrative service capability](sql-object-ownership.md#administrative-service-capability), including retained access and ambient inheritance effects. Secret sources and roleRef exports remain namespace-local. Authorized initial connection export may precede SQL readiness under the baseline contract; access denial and ownership conflicts still prevent publication.

Namespace authorization and collision ownership are the scope of this extension. A separate policy for stronger SQL privilege isolation can be designed if required; it is not introduced implicitly by moving the connection to another namespace. Mandatory databaseNames, roleNames, and allowedMembershipRoles are excluded.

## Access removal and resumption

Removing namespace authorization stops work on the affected resources. SQL objects, grants, passwords, and existing connection Secrets remain as they are. The operator reports AccessDenied but does not connect to SQL, generate passwords, refresh or remove outputs, or execute finalizer cleanup while access is denied. There is no cleanup exception for a formerly authorized owner. If an already managed CR with a finalizer is deleted, it remains pending until authorization is restored; removing finalizers manually is outside this controller contract. Restoring access triggers normal reconciliation of the current spec and current source Secrets. This does not revoke application SQL login access by itself, and this extension does not add IAM token export support.

A role manager's namespace authorization also gates new service-capability repair and historical cleanup that needs it. Restoring its labels requeues Databases on that canonical connection, including work whose actual SQL owner is absent from spec/history. Previously retained service memberships remain in SQL; authorization removal does not revoke existing SQL access. Existing sufficient SQL authority needs no new membership acquisition.

RabbitMQ Messaging Topology Operator similarly reports a condition and stops reconciliation for forbidden cross-namespace references; see [forbidden cross-namespace objects](https://www.rabbitmq.com/kubernetes/operator/using-topology-operator#important-information-about-forbidden-cross-namespace-objects). Strimzi exposes analogous pause/unmanaged behaviors, where changes are ignored or a Kafka topic remains in place; see [Strimzi pause and unmanaged-topic procedures](https://strimzi.io/docs/operators/1.2.0/deploying). These are behavior comparisons, not identical namespace authorization APIs.

## Kubernetes RBAC and Secrets

Service teams can create/update/delete Database and DatabaseRole resources and consume application Secrets in their own namespace. They cannot edit the infrastructure ExternalPostgres or its access policy and cannot read its administrative Secret. The manager reads connection descriptors, Namespace labels, and role dependencies, and reconciles namespace-local password sources; its [generated RBAC](../config/rbac/role.yaml) does not establish application-team isolation. Platform RBAC must enforce the team boundaries above. Kubernetes Role/RoleBinding permissions can be granted per namespace. [Kubernetes RBAC](https://kubernetes.io/docs/reference/access-authn-authz/rbac/).

The shared CA can be copied into each application connection output because it is public trust material. Administrative passwords and client private keys are not copied. Cross-namespace Secret references and roleRef exports are outside the accepted API.

## Examples and validation

[infra-connection.yaml](../examples/cross-namespace/infra-connection.yaml) belongs to the platform team. It assumes existing infra/pg-admin and infra/pg-ca Secrets and selects both orders and billing namespaces. [orders-service-and-admin-readonly.yaml](../examples/cross-namespace/orders-service-and-admin-readonly.yaml) and [billing-service-and-admin-readonly.yaml](../examples/cross-namespace/billing-service-and-admin-readonly.yaml) belong to the respective service teams. The three files together describe two microservices on one external PostgreSQL server. The infra, orders, and billing namespaces must already exist.

| Namespace | SQL database | SQL service user | SQL read-only admin user | Application connection Secrets |
| --- | --- | --- | --- | --- |
| orders | orders | orders_service | orders_admin_readonly | orders-service-db, orders-admin-readonly-db |
| billing | billing | billing_service | billing_admin_readonly | billing-service-db, billing-admin-readonly-db |

Each service owns its database and public schema and runs migrations under its service login. The admin UI gets CONNECT, schema USAGE, and table SELECT. Default privileges grant SELECT on future tables created by that service login. Each role has its own generated password source in the application's namespace. Connection outputs are also local to that namespace. A service can consume its connection Secret's uri key through DATABASE_URL; its admin UI consumes the corresponding read-only Secret. CA mounting follows the baseline's connection Secret contract.

These examples deliberately use different SQL names on the shared PostgreSQL server. Reusing metadata.name across Kubernetes namespaces is allowed, but setting the same effective SQL name would trigger the CR/status ownership conflict check. Independent migrations can instead use the baseline's separate owner/migrator pattern; the namespace-qualified cluster reference and selector remain the same.

Implementation must validate matchLabels and matchExpressions against Namespace labels, omitted/null/empty selectors, local and remote references under the same selector, label and selector updates, SQL ownership conflicts within and across namespaces, protected objects, create-or-update retries, retained objects, endpoint changes and descriptor recreation, local password and export references, and the absence of administrative credentials in application outputs. Access removal must leave SQL and Secrets unchanged and must not run finalizer cleanup; test resumption after access returns. Static YAML checks do not prove authorization or ownership concurrency behavior.
