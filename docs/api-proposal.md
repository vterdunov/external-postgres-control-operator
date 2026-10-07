# User-facing API baseline

Status: accepted initial API baseline, 2026-10-07. The CRDs and controllers described here are not implemented. The example API group `postgresql.epco.io/v1alpha1` remains a placeholder.

Implementation follows the resource fields, defaults, declarative permission reconciliation, password management, and connection Secret contracts defined here and demonstrated in the examples. Changes to this baseline require agreement and corresponding documentation and example updates. Future extensions explicitly described as such are outside the initial implementation scope.

A PostgreSQL cluster means one logical PostgreSQL server with shared role and database catalogs. Its writable endpoint can belong to CNPG, a self-managed PostgreSQL installation, or a cloud service. This operator manages SQL objects and access through a supplied connection.

The baseline defines three CRDs - `PostgresCluster`, `DatabaseRole`, and `Database`. Base role and database fields follow CNPG; external connection settings and ACL management are extensions. Permissions belong to `Database`. SQL execution adapts to the server version, ACLs, and connection privileges without a user-facing `provider` field.

The primary use case is a database for a microservice, a LOGIN user for the service, and a separate LOGIN user for the admin UI with read-only access. Each gets its own connection Secret. A service that runs its own migrations can own its database. With separate migrations, a NOLOGIN role owns the database, a migrator belongs to that role, the runtime user gets DML privileges, and the admin user gets SELECT. Dedicated reader/writer groups are supported but not required for each pair of users.

## Reference implementations and CNPG proposal status

The following local checkouts were analyzed:

- CloudNativePG: `63d0bd5a77dce5d86edc010e4d935defdf48e1af`, commit dated 2026-10-06.
- movetokube postgres-operator: `68e0978969e43f2116674fb368dd187ad5146412`, commit dated 2026-09-07.

This comparison applies to those checkouts. Installed operator versions and live PostgreSQL/RDS instances were not inspected.

Reference types: [RoleConfiguration](https://github.com/cloudnative-pg/cloudnative-pg/blob/63d0bd5a77dce5d86edc010e4d935defdf48e1af/api/v1/cluster_types.go#L2723), [DatabaseRoleSpec](https://github.com/cloudnative-pg/cloudnative-pg/blob/63d0bd5a77dce5d86edc010e4d935defdf48e1af/api/v1/databaserole_types.go#L55), [DatabaseSpec](https://github.com/cloudnative-pg/cloudnative-pg/blob/63d0bd5a77dce5d86edc010e4d935defdf48e1af/api/v1/database_types.go#L55).

| Source | Observed status | Relevance to this API |
| --- | --- | --- |
| [CNPG #10826](https://github.com/cloudnative-pg/cloudnative-pg/issues/10826) | OPEN, milestone 1.31.0 | Proposes `Database.spec.permissions` for CONNECT/CREATE/TEMPORARY with explicit grant/revoke |
| [CNPG #7872](https://github.com/cloudnative-pg/cloudnative-pg/issues/7872) | OPEN, milestone 1.31.0 | Discusses schema CREATE/USAGE and a separate extension for grants on all tables in a schema |
| [CNPG PR #10829](https://github.com/cloudnative-pg/cloudnative-pg/pull/10829) | MERGED 2026-06-05 | Shared SQL helper for GRANT/REVOKE and special handling of PUBLIC |

The inspected `DatabaseSpec` does not contain database/schema `permissions`. A shared SQL helper does not make the proposed public API available. Named table permissions and default privileges below are extensions of this operator.

### Compatibility boundary with CNPG

| Feature | Origin |
| --- | --- |
| Standard DatabaseRole attributes, passwordSecret, inRoles, reclaim policy | CNPG names and base defaults |
| Standard Database fields, schemas with name/owner/ensure, extensions, FDWs/servers | CNPG format |
| Database/schema permission maps | Based on open CNPG proposals, absent from the inspected DatabaseSpec; name/ensure and removal cleanup are this operator's contract |
| PostgresCluster with connection/credentials/TLS and namespace access | External connection descriptor; cluster.name and optional cluster.namespace reference it instead of a CNPG Cluster |
| defaultPrivileges and named tablePermissions | Extensions for SQL access management |
| passwordGeneration and Database.connectionSecrets | Extensions for generating a role password and exporting a connection to a particular database; output Secret keys follow CNPG |
| parameters, managementPolicy, membershipPolicy | Operator extensions for role settings, observation, and membership reconciliation |
| Omitted spec.name resolves to metadata.name | A convenience extension; CNPG requires spec.name |

Matching base fields does not make a complete manifest interchangeable with CNPG. Operator extensions implement the user requirements described in this baseline. Permissions describe desired state: removing a previously declared permission revokes its managed grant. The privilege maps resemble #10826, but entries use name/ensure rather than its name/type command format. This is a deliberate difference from CNPG's FDW/server UsageSpec and the open grant/revoke proposal, selected to keep permission edits declarative.

### Why permissions belong to Database

`DatabaseGrant` was an earlier design option: a separate resource linked a role to a database and managed its ACL independently of the role lifecycle. This can help when database ownership and access requests have separate RBAC, but it is not a required PostgreSQL or CNPG entity.

Issue #10826 considers a separate database privilege CRD excessive because it splits ownership of an object's permissions from the object itself. To follow CNPG closely, this proposal uses `Database.permissions` and nested schema settings. One reconciler manages the database and its declared ACLs; SQL role attributes and memberships remain in `DatabaseRole`.

Issue #10826 covers privileges on the database object. CONNECT allows a connection but does not grant SELECT on tables. Table grants and default privileges are separate requirements.

## Resources and references

| Resource | Represents | Relationships |
| --- | --- | --- |
| `PostgresCluster` | Connection settings for one PostgreSQL cluster | Many roles and databases |
| `DatabaseRole` | One SQL role: a LOGIN user or NOLOGIN group | One cluster, multiple memberships |
| `Database` | One SQL database, listed schemas/extensions/FDWs, and their permissions | One cluster, one owner, multiple grantees |

All resources are namespaced. Database and DatabaseRole may reference a PostgresCluster in another namespace through cluster.namespace; omission resolves to the referencing CR's namespace. Cross-namespace use requires authorization by PostgresCluster.spec.access.namespaceSelector. Administrative credentials and CA references remain local to PostgresCluster; password sources and connection outputs remain local to their application resources, and roleRef exports remain local to Database. The [cross-namespace access contract](cross-namespace-access.md) is part of this accepted initial API and defines the selector defaults and shared infrastructure examples.

SQL operations use the supplied connection's privileges. A namespace scopes Kubernetes resources and Secret references; PostgreSQL object names still belong to the shared server catalogs. CR/status ownership checks resolve competing declarations of the same SQL object. A separate privilege isolation policy would require additional authorization rules whether references are local or cross-namespace.

`cluster.name` and optional `cluster.namespace` reference a Kubernetes `PostgresCluster`. `owner`, `inRoles[]`, and `permissions.*[].name` contain SQL names, following CNPG owner/inRoles/UsageSpec. If the operator manages the referenced role, dependent SQL operations wait for it to become ready. Otherwise, that SQL role must already exist on the same PostgreSQL cluster. Initial connection export can use validated credential sources before the role becomes ready, as described below.

`Database.connectionSecrets[].roleRef.name` references a Kubernetes `DatabaseRole` so its SQL name and password source can be resolved unambiguously. It is a resource name rather than a SQL grantee name. The role and database must reference the same PostgresCluster.

`public` in permissions represents the special PUBLIC grantee. The controller emits the PUBLIC keyword without quotes and quotes ordinary SQL identifiers correctly.

## Defaults

| Field | Baseline behavior |
| --- | --- |
| `DatabaseRole.cluster.name`, `Database.cluster.name` | Required; no automatic cluster selection |
| `DatabaseRole.cluster.namespace`, `Database.cluster.namespace` | Namespace of the referencing CR |
| `PostgresCluster.access` | Without access, local references are allowed and remote references are denied; a configured namespace selector applies to both |
| `DatabaseRole.name`, `Database.name` | Default to metadata.name; the effective SQL name is then immutable |
| `Database.owner` | Required in manage mode |
| `DatabaseRole.login` | false |
| `DatabaseRole.inherit` | true |
| `superuser`, `createdb`, `createrole`, `replication`, `bypassrls` | false |
| `DatabaseRole.connectionLimit` | -1 |
| `DatabaseRole.validUntil` | No password expiration |
| `DatabaseRole.inRoles` | Empty list |
| `DatabaseRole.membershipPolicy` | authoritative, following CNPG; managed limits changes to memberships owned by this operator |
| `DatabaseRole.passwordSecret` | Without either password source, the password is unmanaged and a new role has no password |
| `DatabaseRole.passwordGeneration` | Disabled; explicitly enabled with secretName as an alternative to passwordSecret |
| `DatabaseRole.disablePassword` | false; true sets PASSWORD NULL |
| `databaseRoleReclaimPolicy`, `databaseReclaimPolicy` | retain |
| `Database.ensure` | present |
| `Database.template` | PostgreSQL default template1 at creation |
| `Database.encoding` and locale fields | Inherited from the template when omitted |
| `Database.allowConnections`, `connectionLimit`, `isTemplate` | true/-1/false for a new database; omitted fields do not change an existing database |
| `Database.schemas`, `extensions`, `fdws`, `servers` | Empty lists of managed objects |
| `Database.connectionSecrets` | Empty list; no connection Secrets are created unless requested |
| `connectionSecrets[].name`, `roleRef.name` | Required output Secret name and DatabaseRole reference |
| `connectionSecrets[].sslRootCertPath` | /etc/postgresql/ca.crt when cluster.tls.caSecret is configured; omitted from the connection otherwise |
| `schemas[].ensure`, `extensions[].ensure`, and equivalents | present |
| `schemas[].owner` | Database owner for a new schema; an existing schema is not reassigned without an explicit owner |
| `permissions` and `defaultPrivileges` | Initially omitted scopes are unmanaged; removal of grants previously declared within the same CR UID revokes them |
| `permissions.*[].ensure` | present; absent explicitly requests removal of that grant |
| `schemas[].tablePermissions` | Empty list; tables are not created or dropped |
| `managementPolicy` for roles/databases | manage; observe only inspects the SQL object, including ACLs |
| Existing SQL roles/databases | Create when missing; reconcile existing objects when no competing managing CR is recorded, following CNPG |
| `PostgresCluster.connection.port`, `.database` | 5432, postgres |
| `PostgresCluster.tls.mode` | verify-full using the operator client's trust store or the supplied CA Secret |
| `PostgresCluster.reconcileInterval` | 5m, plus reactions to resource and Secret changes |
| `PostgresCluster.connectTimeout`, `statementTimeout` | 10s, 30s |

Defaulting `spec.name` from `metadata.name` needs a webhook or a documented effective-name mechanism; a static OpenAPI default cannot copy metadata. SQL names are checked against the target server's `max_identifier_length`, usually 63 bytes. Automatic truncation is prohibited.

Omitted standard role attributes use CNPG defaults. Reconciling an existing role can change its attributes and authoritative memberships, so current values must be collected before preparing the manifest. Omitted optional mutable database fields remain unchanged. There is no existingObjectPolicy or separate adoption handshake; normal reconciliation follows CNPG's create-or-update behavior.

## Minimal and full examples

[minimal.yaml](../examples/api-proposal/minimal.yaml) contains a connection, LOGIN role, and database with minimal fields. It assumes basic-auth Secrets `pg-admin` and `app-credentials`. The user's Secret username matches the effective SQL role name. The endpoint certificate must be trusted by the operator client's trust store.

```yaml
apiVersion: postgresql.epco.io/v1alpha1
kind: DatabaseRole
metadata:
  name: app
spec:
  cluster:
    name: pg-prod
  login: true
  passwordSecret:
    name: app-credentials
---
apiVersion: postgresql.epco.io/v1alpha1
kind: Database
metadata:
  name: app
spec:
  cluster:
    name: pg-prod
  owner: app
```

A minimal NOLOGIN group needs only metadata.name and cluster.name. LOGIN and password generation are never enabled implicitly.

Use `passwordGeneration: {secretName: app-credentials}` instead of `passwordSecret` to request password generation. The password source and connection Secret contracts are described below.

[full.yaml](../examples/api-proposal/full.yaml) includes a TLS connection, all standard LOGIN role attributes, NOLOGIN owner/reader/writer roles, database parameters, schemas, an extension, explicit database/schema/table/sequence permissions, and default privileges.

Database field names follow CNPG: `name`, `cluster`, `owner`, `ensure`, `template`, `encoding`, `locale`, `localeProvider`, `localeCollate`, `localeCType`, `icuLocale`, `icuRules`, `builtinLocale`, `collationVersion`, `isTemplate`, `allowConnections`, `connectionLimit`, `tablespace`, `databaseReclaimPolicy`, `schemas`, `extensions`, `fdws`, `servers`.

Creation-only fields do not change after acceptance. They apply when a database is created; an existing database is not recreated to apply them, following CNPG. Changing the database owner does not reassign its tables. Locale, extension, FDW, and tablespace capabilities are checked on the target server. `localeProvider` is available in [PostgreSQL 15](https://www.postgresql.org/docs/15/sql-createdatabase.html), despite the inspected CNPG type's comment saying "from 16".

The concise `inRoles` field follows CNPG's names-only membership reconciliation and PostgreSQL role-attribute semantics. On PG16+, changing the role's inherit attribute does not normalize the INHERIT option of existing memberships. NOINHERIT alone does not prevent SET ROLE. Undeclared ordinary memberships of an authoritative role are revoked; the operator's administrative memberships have separate ownership. The proposed `memberships` field with `adminOption`, `inheritOption`, and `setOption` is deferred because the inspected CNPG API has no equivalent. It is outside the initial CRD and controller scope.

`parameters` defines ALTER ROLE SET support. Only listed parameters and settings previously managed by this operator are reconciled, without RESET ALL. validUntil controls password expiration rather than all authentication methods.

## Passwords and connection Secrets

A role password and database connection settings have different scopes. The password belongs to one DatabaseRole on a PostgreSQL cluster; host/port belong to PostgresCluster; dbname belongs to Database. One role can connect to several databases with the same password, so connections are exported from Database.

### Password source

The CNPG-compatible option is a supplied `kubernetes.io/basic-auth` Secret referenced by `DatabaseRole.passwordSecret.name`. The username/password keys are required, and username must match the role's spec.name. The password is not stored in the CR. [CNPG password management](https://cloudnative-pg.io/docs/devel/declarative_role_management/).

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: orders-service-credentials
type: kubernetes.io/basic-auth
stringData:
  username: orders_service
  password: replace-with-your-password
---
apiVersion: postgresql.epco.io/v1alpha1
kind: DatabaseRole
metadata:
  name: orders-service
spec:
  cluster:
    name: pg-prod
  name: orders_service
  login: true
  passwordSecret:
    name: orders-service-credentials
```

This demonstrates the input Secret structure. For actual delivery, keep the password out of a plaintext Git manifest; the input Secret can be maintained by the user's chosen secret manager. This operator reads a supplied Secret without rewriting it.

The second option is an extension for one-time password generation:

```yaml
apiVersion: postgresql.epco.io/v1alpha1
kind: DatabaseRole
metadata:
  name: orders-service
spec:
  cluster:
    name: pg-prod
  name: orders_service
  login: true
  passwordGeneration:
    secretName: orders-service-credentials
```

The operator generates a password from 32 cryptographically random bytes, encodes it as base64url, and persists username/password in the named basic-auth Secret before the SQL operation. Reconciliation retries reuse the saved password. Persisting it first prevents losing the password if execution fails between SQL and Kubernetes API operations. The source and initial application connection Secret can exist before SQL reconciliation completes; their existence does not imply SQL readiness.

passwordGeneration is mutually exclusive with passwordSecret and disablePassword=true. The generated source name is fixed after initial configuration. An unrelated Secret with the same name is not overwritten. Losing the source after role acceptance produces CredentialsMissing instead of unrequested rotation. Supplied Secrets are never deleted; generated credentials are retained with databaseRoleReclaimPolicy=retain and removed under delete only after successful SQL role deletion.

Recreating a DatabaseRole can reuse a generated source retained from the same namespace/resource name and SQL role name after the previous UID has retired. Verify its operator ownership metadata and basic-auth username, and update ownership conditionally. This reuses its current password; it never generates a replacement merely because the CR UID changed. An unrelated source or a source still managed by another live CR remains a conflict. This is an operator extension because CNPG DatabaseRole consumes a supplied password Secret.

Changing password in passwordSecret requests a password change. With passwordGeneration, initial generation creates the source; subsequent explicitly requested changes to its password are also applied to the SQL role. The current source Secret is the desired password. For rotation of existing connection outputs, the operator watches Secret changes, applies the current password to PostgreSQL, and then refreshes every connection Secret for the role. A failed SQL operation preserves existing outputs instead of publishing the rotated password. Initial publication is allowed before SQL readiness, as described below. A retry uses the current source, including a newer rotation. Record the applied Secret resourceVersion as CNPG does; no password snapshots, archived credential versions, or rollback password are maintained. PostgreSQL, multiple Secrets, and running clients cannot be updated atomically; reconciliation must retry safely and clients need an explicit refresh process.

Without either source, CNPG behavior is preserved: the password is unmanaged and a new role has no password. Password-based exports require the client's password, not a SCRAM verifier or MD5 hash.

### Application connection Secrets

The `Database.spec.connectionSecrets` list associates this database with a ready LOGIN role and an output Secret name:

```yaml
connectionSecrets:
  - name: orders-service-db
    roleRef:
      name: orders-service
  - name: orders-admin-readonly-db
    roleRef:
      name: orders-admin-readonly
```

The service connects as orders_service; the admin UI connects as orders_admin_readonly. SQL privileges are declared separately in permissions; exporting a Secret does not grant access. A readonly name does not select a physical replica. These examples connect both users to the writable endpoint and limit the admin user to SELECT.

The first version exports connections for LOGIN roles with passwordSecret or passwordGeneration. IAM and certificate authentication need a separate contract for exporting static parameters without a permanent password. IAM tokens are not stored in Secrets. PostgresCluster administrative credentials are never used as application credentials.

Output Secrets have type `kubernetes.io/basic-auth`. Core keys follow [CNPG application Secrets](https://cloudnative-pg.io/docs/devel/applications/) and the inspected [CNPG generator](https://github.com/cloudnative-pg/cloudnative-pg/blob/63d0bd5a77dce5d86edc010e4d935defdf48e1af/pkg/specs/secrets.go):

| Key | Source / value |
| --- | --- |
| username, user | DatabaseRole.spec.name; user is an alias |
| password | Current DatabaseRole source for initial publication; rotated passwords update existing outputs after PostgreSQL accepts them |
| host, port | PostgresCluster.spec.connection; port is a string, default "5432" |
| dbname | Database.spec.name, not the administrative PostgresCluster.connection.database |
| uri | PostgreSQL URI with username/password/host/port/dbname and sslmode |
| jdbc-uri | JDBC URI with the corresponding user/password/TLS parameters |
| fqdn-uri, fqdn-jdbc-uri | Aliases of uri/jdbc-uri; the external hostname is used without adding a Kubernetes service domain |
| pgpass | .pgpass content for this host/port/dbname/user with required escaping |
| sslmode | PostgresCluster.spec.tls.mode |
| ca.crt | Public CA copied from cluster.tls.caSecret when configured |
| sslrootcert | CA path inside the application container when a CA is included |

Host/port identify the external endpoint, which must be reachable by the applications. URI generation uses a URL encoder with correct handling of password characters, SQL names, and IPv6. [PostgreSQL URI syntax](https://www.postgresql.org/docs/current/libpq-connect.html#LIBPQ-CONNSTRING).

When a CA is configured, uri/jdbc-uri include sslrootcert=/etc/postgresql/ca.crt. The application mounts ca.crt from its connection Secret at that path. A different path can be specified with connectionSecrets[].sslRootCertPath. Without caSecret, trust roots are configured through the chosen client driver; defaults are not assumed to match across libraries. The operator's administrative TLS private key is not copied to applications. Password connection examples assume the endpoint supports the chosen authentication method. [PostgreSQL sslrootcert](https://www.postgresql.org/docs/current/libpq-connect.html#LIBPQ-CONNECT-SSLROOTCERT).

Password sources and connection outputs have different names, each with one managing controller. Other databases can export separate connections for the same role, reusing its password with their own dbname. A Secret cannot be both a password source and a connection output, nor can several Database resources use the same output name. An unrelated existing Secret produces ConnectionSecretConflict. Secrets and references remain in the database namespace. CRs/status/logs never contain passwords or password-bearing DSNs. The initial API treats a namespace as one trust boundary: permission to edit connectionSecrets permits publication of credentials for managed roles in that namespace. Delegation to less trusted tenants requires separate export authorization.

For initial publication in manage mode with ensure=present, create each requested connection Secret as soon as the current connection settings, effective database and SQL role names, and validated password source are available. Authorization, SQL-name conflict checks, matching cluster references, and source/output ownership checks still apply before publication. Generated passwords are persisted in their source first. SQL role/database creation, CONNECT grants, and table/default-privilege reconciliation may still be pending. Export does not wait for DatabaseRole Ready, Database Ready, or the existence of every table named in permissions.

Secret existence is separate from readiness. Ready requires the SQL role to have accepted the current password version, the database and all declared ACLs to be reconciled, effective CONNECT, and successful export. A migration Job can consume the initial Secret in the same deployment wave and retry connection or SQL errors while the operator finishes reconciliation. Deployment tooling owns ordering and retries, including ensuring that default privileges are installed before relying on them for newly created objects. A Job that creates tables named in permissions must not wait for the Database's overall Ready condition first.

On transient failure, an existing output Secret is preserved and a condition records the incomplete update. Rotation of an existing output waits for SQL password application; table permissions pending for another role do not block that credential refresh. An endpoint change requires reconciling the exported role/password, database, and CONNECT on the current target before refreshing an existing output. Deleting a derived Secret recreates it with the current password from its source after SQL application. Removing a connectionSecrets entry deletes only the operator's former output Secret, without changing SQL access or the source password. Deleting Database with retain keeps existing outputs without further updates; delete removes them after successful SQL deletion. Retention must avoid ownerReferences that would garbage-collect credentials when a retained CR is deleted. A recreated Database may reuse outputs retained from the same namespace/resource name after the former UID has retired, with ownership checked and updated conditionally. This does not authorize overwriting unrelated Secrets.

Database.ensure=absent removes connection outputs after confirmed SQL deletion, regardless of the database reclaim policy. Role password sources remain governed by their own role lifecycle.

An application consumes its own connection Secret. DATABASE_URL references the uri key through secretKeyRef; individual keys can instead map to driver-specific variables. Secret changes do not update environment variables in a running container: clients need a rollout or supported credential reload. Updated Secret files also need to be reread by the application. The operator does not restart applications implicitly. [Kubernetes Secret environment variables](https://kubernetes.io/docs/tasks/inject-data-application/distribute-credentials-secure/#define-container-environment-variables-using-secret-data).

## Primary examples: microservice and read-only admin UI

The shared [postgres-connection.yaml](../examples/microservice/postgres-connection.yaml) uses existing pg-admin and pg-ca Secrets. The operator generates independent passwords and connection outputs for the service and admin UI. Standard SQL flags and reclaim policies use CNPG defaults.

[service-owner-and-admin-readonly.yaml](../examples/microservice/service-owner-and-admin-readonly.yaml) is the shorter variant. orders_service owns the database and public schema and creates tables during migrations. orders_admin_readonly gets CONNECT, schema USAGE, and SELECT on existing tables. Default privileges FOR ROLE orders_service IN SCHEMA public grant SELECT on future tables when they are created. The admin user has no owner membership or DML grants. Outputs are orders-service-db and orders-admin-readonly-db. Migrations under another SQL user require that creator's defaultPrivileges block.

[separate-migrations-and-admin-readonly.yaml](../examples/microservice/separate-migrations-and-admin-readonly.yaml) separates privileges. orders_owner is a NOLOGIN owner; orders_migrator belongs to it and performs DDL; orders_service gets SELECT/INSERT/UPDATE/DELETE and sequence USAGE/SELECT; orders_admin_readonly gets table SELECT. Defaults cover both orders_owner and orders_migrator, allowing object creation under the migrator login or SET ROLE owner. To keep object ownership consistent, migrations should explicitly SET ROLE orders_owner. A third output, orders-migrator-db, is supplied only to the migration process.

The migration process can consume orders-migrator-db as soon as it is published and retry until PostgreSQL is usable. This supports a single deployment wave containing the Database and a migration Job: pending named-table grants do not prevent initial Secret publication. Application rollout order and migration retry behavior belong to the deployment tooling.

Choose either variant during initial configuration. Switching an existing database between them does not migrate table ownership automatically. Read-only access here is enforced by table privileges; views, memberships, PUBLIC grants, and SECURITY DEFINER functions can provide independent access and must be considered when used.

[orders-service-password-provided.yaml](../examples/microservice/orders-service-password-provided.yaml) replaces the service role declaration when choosing a supplied password instead of generation. [orders-service-deployment.yaml](../examples/microservice/orders-service-deployment.yaml) and [orders-admin-deployment.yaml](../examples/microservice/orders-admin-deployment.yaml) demonstrate separate connection Secrets, DATABASE_URL, and a CA mount. Container images are placeholders and must be replaced. The service does not need the input credential Secret name or administrative credentials.

## Permissions based on the CNPG proposals

Each entry contains a SQL grantee name and an optional ensure: present|absent, defaulting to present. The privilege maps follow the CNPG proposal's structure; desired-state entries and removal cleanup are this operator's contract:

```yaml
permissions:
  connect:
    - name: public
      ensure: absent
    - name: app_reader
  temporary:
    - name: public
      ensure: absent
schemas:
  - name: app
    permissions:
      usage:
        - name: app_reader
      create:
        - name: app_reader
          ensure: absent
```

Database permissions cover CONNECT/CREATE/TEMPORARY. Schema permissions cover USAGE/CREATE. WITH GRANT OPTION and GRANTED BY are excluded from the first public API. If supported, `all` is expanded into the privilege set available for that object type; conflicting entries for a grantee are rejected.

Each entry declares a SQL role and the desired presence of one privilege. name is the SQL grantee name; ensure defaults to present. The controller grants the privilege when missing and maintains it while declared. The normal way to withdraw access is to remove that role from the privilege list. Removing a tablePermissions entry revokes its previously managed grants; removing an entire permission block or replacing it with an empty list does the same for that scope. These edits never drop a table or delete its data.

An optional ensure=absent on the same grantee entry explicitly declares that the privilege must be absent. It also handles privileges that exist before the first reconciliation, such as default PUBLIC CONNECT. Removing an absent entry stops its absence enforcement and does not grant the privilege back. type=grant/revoke is not part of this operator's permissions API. Database.ensure and schemas[].ensure control SQL object lifecycle; ensure on a permission controls only the grant.

Reason for this decision: the manifest must describe the desired permission set, and removing a declared permission must actually remove the access it gave. The user's normal workflow must not require retaining a separate revoke command. An explicit absent state is optional, not a prerequisite for list cleanup. This supersedes the earlier command-style grant/revoke and stop-managing-on-removal decision. Compatibility with CNPG is preserved where possible, but its proposed omission semantics do not satisfy this requirement.

The controller persists the managed permission set and sufficient pending-change information in the live Database CR's status before SQL mutations. Recovery within the same CR UID compares current SQL with the current spec, including removed entries; it must not forget a successfully issued grant if a subsequent status write failed, or blindly replay a grant withdrawn in a newer spec. Keep the resolved object and grantor bindings needed for cleanup after a rename or owner change, and re-resolve them against the current connection. Do not use old object identifiers blindly after an endpoint change, and do not clean up the former server. This is permission bookkeeping, not password snapshots or a separate ownership claim store. Cleanup is limited to managed grants; unrelated grantees are not purged. Ready remains false until required cleanup is confirmed.

Deleting the CR removes that status history. A recreated CR, including the same namespace/resource name with a new UID, starts a new permission-management session. With retain, SQL grants survive; leaving them out of the recreated spec does not identify them as former managed grants and does not revoke them. Explicit ensure=absent entries remove historical retained grants. Subsequent edits to the new live CR again use ordinary declarative removal. Credential-source and output-Secret reuse follows its separate retained-Secret contract. History restoration after CR deletion is deferred; the initial implementation creates no grant-history ConfigMap, SQL journal, or controller metadata in SQL comments.

Removing or marking a default privilege absent changes future-object defaults; it does not retroactively remove table privileges created by that default. Existing-table access is managed through tablePermissions or schema-wide table permissions. Removing a schema/extension declaration leaves its SQL object but cleans up its associated managed permissions. SQL objects are dropped only through their explicit lifecycle settings.

### Reading all tables in a schema

`schemas[].permissions.tables` manages ALL TABLES IN SCHEMA; `sequences` manages ALL SEQUENCES IN SCHEMA. This extends the direction discussed in #7872. Read-only table access requires CONNECT, schema USAGE, and table SELECT. Sequence SELECT is optional when reading sequence state is needed.

```yaml
schemas:
  - name: app
    permissions:
      usage:
        - name: app_reader
      tables:
        select:
          - name: app_reader
      sequences:
        select:
          - name: app_reader
    defaultPrivileges:
      - forRole: app_owner
        tables:
          select:
            - name: app_reader
```

[readonly.yaml](../examples/api-proposal/readonly.yaml) extends the minimal example with a reader group and user and updates the same Database/app resource.

Read/write access explicitly grants table SELECT/INSERT/UPDATE/DELETE and sequence USAGE/SELECT. CREATE, TRUNCATE, and EXECUTE are not added implicitly. Owners and migration users are configured separately.

`ALTER DEFAULT PRIVILEGES` applies to the actual object creator. `forRole` is required because the database owner may not create the tables. A migrator creating under its login needs its own block; SET ROLE owner uses the owner's block. Group default privileges are not inherited when creating objects. [PostgreSQL](https://www.postgresql.org/docs/current/sql-alterdefaultprivileges.html).

`Database.spec.defaultPrivileges[]` specifies global defaults within the database, without IN SCHEMA. `schemas[].defaultPrivileges[]` specifies defaults for that schema. Both use forRole and privilege maps. Revoking PUBLIC EXECUTE on future functions needs a global block because a schema-level revoke cannot override a global default grant.

Periodic ACL reconciliation also handles newly created tables. Default privileges grant access immediately when the specified role creates an object, without waiting for the next reconciliation.

### Explicit table allowlist for an LLM agent

PostgreSQL supports granting SELECT only on named tables:

```sql
GRANT CONNECT ON DATABASE orders TO llm_agent;
GRANT USAGE ON SCHEMA app TO llm_agent;
GRANT SELECT ON TABLE app.orders, app.invoices TO llm_agent;
```

The `schemas[].tablePermissions[]` extension declares permissions on existing named tables. The controller does not manage their DDL or data.

```yaml
schemas:
  - name: app
    permissions:
      usage:
        - name: llm_agent
    tablePermissions:
      - name: orders
        permissions:
          select:
            - name: llm_agent
      - name: invoices
        permissions:
          select:
            - name: llm_agent
```

[llm-readonly.yaml](../examples/api-proposal/llm-readonly.yaml) provides a separate LOGIN role and management of an existing database. Database orders, owner orders_owner, and tables app.orders/app.invoices must already exist, and the operator connection must have the required privileges. If that SQL database already has a managing Database CR, add these permissions to that CR; do not create a competing manager. llm_agent has no broad memberships, schema-wide table grants, or defaults granting access to future tables.

Every allowed table is named explicitly. A missing table with ensure=present permissions produces DependenciesNotReady/TableNotFound without creating it. A missing table with only absent permissions already satisfies absence. A new table with another name does not automatically receive SELECT for the agent. Dropping and recreating a named table requires another ACL reconciliation. If a managed table is renamed outside its declared name, cleanup must remove the old managed grant using its recorded binding before completing reconciliation. DROP/recreate is distinct from RENAME.

To remove SELECT on app.orders, delete its orders entry or remove llm_agent from that entry's select list. Keeping only invoices removes the managed SELECT on orders and preserves invoices SELECT. The table is not dropped. Alternatively, keep the llm_agent entry on orders with ensure=absent.

[llm-readonly-after-removal.yaml](../examples/api-proposal/llm-readonly-after-removal.yaml) updates the same Kubernetes resources and keeps only invoices. [llm-readonly-with-absent-grant.yaml](../examples/api-proposal/llm-readonly-with-absent-grant.yaml) shows the alternative explicit absent state on orders SELECT. Apply the original followed by either updated state. Both remove the same managed direct grant.

Cleanup uses PostgreSQL REVOKE, which is not a DENY. Access through another role, PUBLIC, or an independent grant requires separate evaluation; removing one managed direct grant does not eliminate every independent access path.

SELECT on allowed tables does not deny access to others. Effective privileges include PUBLIC and memberships. A restricted agent must not receive pg_read_all_data, a schema-wide reader membership, object ownership, or other broad grants. Views and SECURITY DEFINER functions can expose other tables and need separate consideration. RLS continues to apply where configured. [GRANT](https://www.postgresql.org/docs/current/sql-grant.html), [Views](https://www.postgresql.org/docs/current/sql-createview.html), [Functions](https://www.postgresql.org/docs/current/sql-createfunction.html), [Row security](https://www.postgresql.org/docs/current/ddl-rowsecurity.html).

Named table management exceeds the CNPG scope discussed in #7872, which proposes ALL TABLES IN SCHEMA. It is justified here by the explicit requirement to restrict an LLM agent to selected tables.

## ACL reconciliation with a non-superuser connection

The controller normalizes current ACLs before calculating changes. A NULL ACL represents the object's default ACL. For a database, use `COALESCE(datacl, acldefault('d', datdba))`; grantee OID 0 represents PUBLIC. This case is documented in the [#10826 comment](https://github.com/cloudnative-pg/cloudnative-pg/issues/10826#issuecomment-5162760434).

```sql
SELECT d.datname, a.grantee, a.grantor, a.privilege_type, a.is_grantable
FROM pg_catalog.pg_database AS d
CROSS JOIN LATERAL aclexplode(
  COALESCE(d.datacl, acldefault('d', d.datdba))
) AS a
WHERE d.datname = $1;
```

Schemas and relations are normalized using their current ACLs, owners, and object types. Resolve an authorized grantor for desired grants and previously managed bindings that need cleanup. Effective access through PUBLIC or inheritance is checked separately. Only required changes are applied; managed bindings and pending changes survive retries and restarts.

GRANT can emit warnings and apply only part of a request when grant options are insufficient. Successful Exec is not sufficient for Ready: reread ACLs and confirm the result after changes. [PostgreSQL GRANT](https://www.postgresql.org/docs/current/sql-grant.html).

Revoking PUBLIC CONNECT must preserve the operator SQL user's required access. Database-level DDL can use the administrative database, but schemas/extensions/table ACLs need a connection to the target database. Apply operations in an order that preserves effective CONNECT.

## AWS RDS without a provider field

`provider: awsRds` was initially proposed as an SQL adapter switch. These operations do not require it: the same authorization checks apply to any PostgreSQL connection without SUPERUSER. Server version, current user attributes, memberships, ACLs, and available settings are inspected through SQL.

An RDS administrator is not a native PostgreSQL SUPERUSER. The operator uses supplied credentials and does not grant rds_superuser to application users or itself to fix permissions. superuser=true retains its literal PostgreSQL meaning and fails when unsupported by the server or connection. [AWS](https://docs.aws.amazon.com/AmazonRDS/latest/UserGuide/Appendix.PostgreSQL.CommonDBATasks.Roles.rds_superuser.html).

movetokube's [aws.go](https://github.com/movetokube/postgres-operator/blob/68e0978969e43f2116674fb368dd187ad5146412/pkg/postgres/aws.go) grants the owner role to the operator user during database creation and uses memberships for other administrative operations. This is `GRANT app_owner TO operator_login`; application users do not need rds_superuser.

| Operation | Required authorization checks |
| --- | --- |
| CREATE DATABASE OWNER | CREATEDB for the executing user and access to the intended owner role |
| GRANT/REVOKE on tables/schema/database | Object ownership or applicable grant options |
| ALTER DEFAULT PRIVILEGES FOR ROLE | Access to the actual object creator role |
| GRANT group TO user | Authority to manage that group's membership |
| ALTER ROLE / membership on PG16+ | ADMIN OPTION and applicable SET/INHERIT options |

PG16+ separates ADMIN, INHERIT, and SET membership options. Authority to administer a role does not guarantee SET ROLE access. Inspect pg_auth_members, grantor identity, and the version's supported pg_has_role checks; an administrative role name alone is insufficient. [Role membership](https://www.postgresql.org/docs/current/role-membership.html), [ALTER ROLE](https://www.postgresql.org/docs/current/sql-alterrole.html).

For new managed owner/group/creator roles, the controller may maintain the administrative memberships needed by its SQL user, using authority already granted over those roles. Missing privileges on an existing role block reconciliation. These administrative relationships have separate ownership and are not added to application users.

GRANT inside a database can use an authorized SET LOCAL ROLE owner in a pinned transaction. CREATE DATABASE runs outside a transaction as an administrative user with CREATEDB and an OWNER clause; switching to a NOLOGIN owner without CREATEDB does not authorize creation. A pooled connection must not be returned with another active role.

Memberships must account for existing options, grantors, concurrent reconciliations, and dependencies of ADMIN grants. An unconditional grant/revoke around each SQL operation is unsafe. The older wrapper's plain GRANT does not replace PG16+ option checks. Removing an administrative membership never uses implicit CASCADE.

Standard role attributes are updated only when they differ. For example, an already matching superuser=false does not need ALTER ROLE NOSUPERUSER, which a non-superuser administrator may not be authorized to execute.

RDS restricted password management is checked against the actual `rds.restrict_password_commands` setting and connection privileges. It is a PASSWORD/VALID UNTIL capability check and does not require a provider field on every resource. Extensions and protected service roles are also checked against server capabilities.

### RDS IAM users and operator IAM authentication

An SQL account for IAM is an ordinary DatabaseRole with membership:

```yaml
apiVersion: postgresql.epco.io/v1alpha1
kind: DatabaseRole
metadata:
  name: reporting-iam
spec:
  cluster:
    name: rds-prod
  name: reporting_iam
  login: true
  disablePassword: true
  inRoles:
    - rds_iam
    - orders_reader
```

This SQL configuration needs no provider field. Membership in rds_iam does not create an AWS IAM principal or enable IAM database authentication on the instance. The client separately needs AWS credentials and rds-db:connect; the instance needs IAM database authentication enabled. No password Secret is issued for this role. [AWS DB accounts](https://docs.aws.amazon.com/AmazonRDS/latest/UserGuide/UsingWithRDS.IAMDBAuth.DBAccounts.html), [IAM policy](https://docs.aws.amazon.com/AmazonRDS/latest/UserGuide/UsingWithRDS.IAMDBAuth.IAMPolicy.html).

IAM takes precedence over password authentication for an RDS user with effective rds_iam membership. An incompatible password-management request must produce a clear error. Authentication tokens are short-lived and are not stored as permanent passwords. [AWS IAM authentication](https://docs.aws.amazon.com/AmazonRDS/latest/UserGuide/UsingWithRDS.IAMDBAuth.html).

If the operator connects using IAM, its connection needs a separate authentication method, such as `authentication.awsIam` with username and region, using workload identity for AWS credentials. It is mutually exclusive with password credentialsSecret. This selects token signing explicitly rather than a backend-wide provider. Current examples use password authentication for the administrative connection.

[aws-rds.yaml](../examples/api-proposal/aws-rds.yaml) includes an RDS connection and CA, owner, migrator, reader, password and IAM LOGIN users, and inline permissions/default privileges. No resource requests rds_superuser. Defaults cover both owner and migrator because either can create objects under its own SQL identity.

## One role across databases and servers

| Relationship | Representation |
| --- | --- |
| One role -> several databases on one PostgreSQL cluster | One DatabaseRole and its SQL name in multiple Database permissions; a connection Secret per database when needed |
| One role -> several schemas/tables in a database | Multiple scoped permissions in that Database |
| User -> several groups | inRoles |
| Same SQL name -> independent PostgreSQL servers | A separate DatabaseRole per cluster |
| Primary and physical replicas | One replicated catalog managed through the writable endpoint |

[multi-cluster.yaml](../examples/api-proposal/multi-cluster.yaml) defines analyst-prod and analyst-dwh with the same SQL name analyst but different clusters and Secrets. Connections and reader groups are assumed to exist. A shared passwordSecret is technically possible in one namespace, but rotation across two servers is not atomic.

A DatabaseRole has one cluster reference. If a single user-facing resource must target several servers in the future, a separate DatabaseRoleSet could create ordinary child DatabaseRoles from a template and target list, with per-target overrides, independent status, and targetRemovalPolicy=retain. SQL objects on independent servers remain separate.

## SQL name collisions

Use CNPG's CR/status model to resolve SQL name collisions. The ownership key is the canonical namespace-qualified PostgresCluster reference, object type (role or database), and effective SQL name; the referencing CR's namespace is not part of this key. Use one connection descriptor per logical PostgreSQL server. Before applying a new managing CR, check other CRs across namespaces for an already recorded manager of the same key. The losing CR reports OwnershipConflict and makes no SQL changes or generated connection exports for that object.

This means the first recorded manager, not strict ordering of kubectl apply requests. Both CRs can be accepted by the Kubernetes API. Retrying the winning CR with the same UID resumes its ownership; temporary failure does not erase its recorded management. Existing SQL objects without a competing live manager are reconciled normally, as in CNPG. SQL existence alone is not a conflict.

[SQL object ownership](sql-object-ownership.md) describes the verified CNPG behavior and the selected adaptation: CR/status checks across namespaces, normal create-or-update retries, and Lease leader election with serialization by SQL object key inside the active process. No separate ownership ConfigMap, database-initialization checkpoint, SQL marker, or staging database is required. Declarative permission removal requires managed-grant and pending-change bookkeeping independently of SQL name conflicts. A live Database with ensure=absent still records management of its name.

## Lifecycle, ownership, and status

- Role/database names and cluster references are immutable, following CNPG. The referenced PostgresCluster connection settings may change, including its endpoint, or the descriptor may be recreated under the same namespace/name. Reconcile against the current connection without a stored-UID rebind approval. Requeue dependents and refresh connection outputs. This does not copy data or clean up the previous server.
- Database.ensure=absent explicitly requests DROP regardless of reclaim policy. Reclaim policies apply when a Kubernetes CR is deleted. Reserved databases are protected.
- DatabaseRole does not support ensure=absent; deleting its CR with databaseRoleReclaimPolicy=delete requests DROP.
- Role deletion does not automatically run DROP OWNED or REASSIGN OWNED. Dependencies in any database block DROP. Forced session termination needs an explicit request if supported.
- Deleting Database with retain preserves SQL objects and ACLs and ends its permission-management history. A recreated CR starts new management; historical retained grants require explicit ensure=absent for cleanup. Removing a permission within the same live CR UID revokes its managed grant. Removing a schema/extension declaration leaves the SQL object but cleans up associated managed permissions. ensure=absent on a SQL object requests DROP with RESTRICT; ensure=absent on a permission removes only that grant.
- Observe changes neither SQL attributes nor permissions and does not generate passwords or publish connection outputs. It requires ensure=present and reclaim=retain. An observer that never managed SQL does not reserve its name; a former manager keeps its recorded management while its CR remains. Existing database ACL management uses normal manage mode with its actual owner and selected settings.
- A SQL role or database has one managing CR through its canonical connection, across namespaces. Automatically resolving aliases or coordinating independent control planes is outside the initial design. CNPG and this operator need explicit ownership boundaries when using the same SQL objects.
- Each ACL tuple has one spec owner. Conflicting broad and named-table rules for the same grantee are rejected. User-declared memberships and operator administrative memberships have separate ownership.
- Predefined PostgreSQL/service roles and the administrative connection account are protected from adoption, modification, and deletion; permitted memberships in them are considered separately.
- Ready requires confirmed reconciliation, including declared permissions and connection exports. An initial connection Secret can exist while Ready is false. A positive grant does not prove that every other access path is absent. Effective privileges and errors are reported without promising an audit of function bodies.
- Passwords and password-bearing DSNs never appear in logs/status. The platform team controls administrative Secrets and PostgresCluster.

Keep `status.applied`, `observedGeneration`, and `message`; add `effectiveName`, `appliedGeneration`, `lastSuccessfulReconcileTime`, and conditions. Permission results should identify schema/table/grantee. Error reasons include ConnectionUnavailable, NotWritable, SecretNotFound, CredentialsMissing, ConnectionSecretConflict, DependenciesNotReady, TableNotFound, OwnershipConflict, InsufficientPrivileges, UnsupportedFeature, and DeletionBlocked.

## Deferred features

Restoring managed permission history after CR deletion is outside the initial implementation. Retained SQL objects use a fresh management session and explicit absent grants for historical cleanup. A future design may consider durable records in Kubernetes or SQL comments, but neither mechanism is required or implemented by this baseline. This scope decision does not defer status persistence and retry recovery while the same CR UID remains alive.

The extended role memberships API with adminOption, inheritOption, and setOption is also deferred. The inspected CNPG role API supplies inRoles and the role-level inherit attribute, which remain supported. Server-side membership option checks needed for non-superuser administration, including RDS, remain an implementation requirement; deferring public fields does not remove those SQL authorization checks.

## Examples and validation

- [minimal.yaml](../examples/api-proposal/minimal.yaml): connection, user, and database.
- [full.yaml](../examples/api-proposal/full.yaml): full configuration with inline permissions.
- [readonly.yaml](../examples/api-proposal/readonly.yaml): reader and update of Database/app.
- [multi-cluster.yaml](../examples/api-proposal/multi-cluster.yaml): the same SQL username on two servers.
- [aws-rds.yaml](../examples/api-proposal/aws-rds.yaml): RDS, grants/default privileges, password/IAM.
- [llm-readonly.yaml](../examples/api-proposal/llm-readonly.yaml): SELECT on explicitly named tables.
- [llm-readonly-after-removal.yaml](../examples/api-proposal/llm-readonly-after-removal.yaml): updating the same live CR removes orders from the declared permission set and removes its managed SELECT.
- [llm-readonly-with-absent-grant.yaml](../examples/api-proposal/llm-readonly-with-absent-grant.yaml): explicit absent state on orders SELECT, also suitable for historical cleanup after CR recreation.

Minimal/full/RDS/LLM are alternative examples with overlapping names; use different namespaces or resource names. Readonly extends minimal and updates Database/app. LLM after-removal and with-absent-grant are alternative updates of the original example rather than separate SQL objects. The primary microservice variants are described above.

Before claiming RDS compatibility, test generic PostgreSQL and real RDS on both sides of PG16: role/database creation and update with different owners, object grants, defaults for different creators, ADMIN/SET/INHERIT, password restrictions, IAM membership, protected roles, and explicit grant/revoke dependencies. Local PostgreSQL does not reproduce all RDS privilege hooks.

LLM tests must cover initial named-table SELECT, denial on initially unlisted and newly created tables, removal of a role or table entry within the same CR UID, empty or removed blocks, optional absent grants, rename and DROP/recreate, and broad membership/PUBLIC access cases. ACL diff tests must cover NULL/default ACLs, PUBLIC OID 0, partial GRANT with warnings, preservation of operator access, new relations, owner/grantor changes, and default-privilege cleanup separately from current-table cleanup. Test SQL success followed by a failed state write and a newer spec withdrawing the grant; cleanup must survive controller restart for the same CR without forgetting that grant. Test retain/delete/recreate separately: omitted historical grants remain, explicit absent removes them, and ordinary list removal works for grants managed in the new session.

Microservice tests must cover runtime DML, denial of admin INSERT/UPDATE/DELETE/DDL, immediate access to tables created by each declared creator, and sequence permissions. Test initial Secret publication with SQL role/database creation or named-table permissions still pending, Ready=false despite Secret existence, a migration Job using retry in the same deployment wave, and eventual ACL reconciliation after the Job creates the tables. Credential tests must cover supplied/generated sources, password persistence before SQL, retry using the latest source after partial failure, source loss without unrequested rotation, roleRef and cluster validation, source/output name conflicts, independent service/admin outputs, shared roles across databases, ordinary rotation fan-out without snapshots, TLS CA export, URI escaping, retained Secret reuse after CR recreation, and endpoint changes. Authorization and ownership conflicts must block early export. Existing outputs must not publish a rotated password before SQL accepts it, and pending grants for another role must not block that refresh. Ready must remain false when SQL accepts a new password but an output update fails, until reconciliation repairs the requested outputs. Initial CRDs expose inRoles without the deferred memberships/options fields.

YAML syntax and example relationships are checked statically. CRD schema validation and SQL/e2e validation require implemented CRDs/controllers and test server connections.
