# SQL object ownership and reconciliation

Status: selected design for the [accepted API baseline](api-proposal.md), updated 2026-10-10. Follow inspected CNPG behavior for retries, existing SQL objects, CR conflicts, and reclaim policies. DatabaseRole and Database implement this ownership/lifecycle contract. Database implements status-backed ACL cleanup and recovery within a live CR UID. See [implementation status](README.md#implementation-status) and the implemented [PostgresCluster connection check](postgrescluster.md).

## Verified CNPG behavior

The comparison uses the clean CNPG checkout at commit 63d0bd5a77dce5d86edc010e4d935defdf48e1af, dated 2026-10-06.

Both Database and DatabaseRole invoke [detectConflictingManagers](https://github.com/cloudnative-pg/cloudnative-pg/blob/63d0bd5a77dce5d86edc010e4d935defdf48e1af/internal/management/controller/common.go#L249) before initial SQL reconciliation. A resource that has reconciled previously skips this initial conflict check. Otherwise, CNPG lists resources of that type in the current namespace.

[ensureManagedResourceExclusivity](https://github.com/cloudnative-pg/cloudnative-pg/blob/63d0bd5a77dce5d86edc010e4d935defdf48e1af/api/v1/generic_funcs.go#L35) looks for another CR with the same cluster reference and SQL name that has reconciled previously. HasReconciliations means status.observedGeneration > 0. The new CR is marked failed with an error saying the SQL object is already managed by another Kubernetes object.

[Database reconciliation](https://github.com/cloudnative-pg/cloudnative-pg/blob/63d0bd5a77dce5d86edc010e4d935defdf48e1af/internal/management/controller/database_controller.go#L361) updates an existing database and creates a missing one. [DatabaseRole reconciliation](https://github.com/cloudnative-pg/cloudnative-pg/blob/63d0bd5a77dce5d86edc010e4d935defdf48e1af/internal/management/controller/databaserole_controller.go#L514) similarly updates an existing role. SQL existence alone is not a conflict, and there is no separate adoption handshake in these paths.

After SQL succeeds but the status write fails, normal retries inspect current SQL and reconcile again. The inspected paths do not use operation markers, staging databases, a durable initialization journal, or a special unknown-create recovery workflow.

On deletion with reclaim=delete, a CR that never recorded a successful reconciliation must not drop the SQL object. CNPG checks this for both databases and roles, so deleting a rejected duplicate does not delete the surviving manager's object. This also means that SQL created before a failed first status write can remain if that CR is deleted before a successful retry. The selected design accepts this upstream behavior instead of adding a separate recovery protocol.

Deleting a CR with reclaim=retain leaves SQL in place and removes the CR/status record. Recreating a CR then uses normal create-or-update reconciliation, provided there is no other live manager for that object.

CNPG does not check these resources across namespaces in this path. Its Cluster reference is namespace-local, and each Cluster has its own PostgreSQL instance managers. The generic helper's comparison of metadata.name alone is valid only because its list is restricted to one namespace.

## Adaptation for external PostgreSQL

Use one canonical PostgresCluster per logical PostgreSQL server. The management key consists of its namespace-qualified Kubernetes reference, SQL object type (role or database), and effective SQL name. The application CR namespace is not part of the key. One role and one database can share a name because their types differ. Automatic discovery of connection aliases and independent Kubernetes control planes managing one server are outside the initial design.

For a new managing CR:

1. Authorize its Namespace against the current PostgresCluster selector.
2. List relevant CRs across namespaces that reference the same canonical connection.
3. Exclude the current CR by UID, not metadata.name alone.
4. Look for another CR of the same type and SQL name with recorded management.
5. If found, report OwnershipConflict without SQL mutation or connection export for the competing CR.
6. Otherwise, create a missing SQL object or reconcile an existing one, then record confirmed lifecycle management. Publish the successful generation only after all requested work is confirmed.

Temporary failure does not erase a manager's recorded reconciliation. Same-UID retries continue managing the same SQL object. A pure observer is not a manager merely because it recorded an observed generation; a former manager retains its management record while its CR remains.

Both SQL controllers record sticky `status.hasManagedSQL` only after confirmed successful managed lifecycle reconciliation. For Database, confirmed database/schema management reserves the SQL name even when ACLs are pending, for example while a migration has not created a named table. Journal intent alone does not acquire ownership. Full Ready, appliedGeneration and last successful reconciliation still require declared ACLs and required cleanup. Database rechecks the authoritative CR UID and current inputs after acquiring its lock and before SQL/status publication. A deleted/replaced or changed snapshot cannot publish success for a new resource. Database deletion checks actual SQL ownership and does not wait for the desired owner role to become Ready. Unsupported desired child declarations do not block an authorized reclaim-delete finalizer for a previously managed database.

A live Database with ensure=absent still records management of its SQL name. A competing CR cannot recreate that database while its recorded manager remains.

The intended winner is the first recorded manager. Kubernetes can accept both CRs, and strict kubectl apply ordering is not guaranteed. The CNPG list/status check is not an atomic distributed claim. No separate ownership ConfigMap, SQL claim table, or UID tombstone is required for this conflict model. Permission bookkeeping is a separate requirement for declarative removal of grants.

## Administrative service capability

Database owns lazy repair of missing SET authority for eligible recorded manage-mode DatabaseRoles on the same canonical descriptor. It checks both resource namespaces against the current descriptor. Recorded management wins over unreconciled contenders; SQL existence alone is not evidence of eligibility. Existing usable SQL authority remains sufficient. A deleting role may supply historical cleanup or actual-owner DROP authority without a desired-owner Ready gate. No new CRD/status fields or separate capability journal are involved.

The helper makes bounded, backend-authorized, additive membership grants and confirms identity, exact grantor-specific options and effective SET (MEMBER on PostgreSQL 15). PostgreSQL 16+ new rows use SET=true, INHERIT=false, ADMIN=false; existing rows change only SET and preserve independent grantors. ACL preparation repairs only concrete owners/creators or exact cleanup grantors before candidate intent is checkpointed, never ordinary grantees. Extension-only operations acquire no service membership. ALTER DATABASE OWNER retains its inherited-old-owner requirement and does not acquire INHERIT.

Service grants are retained across shared Database lifecycles, owner changes, retain/delete, management-policy changes and connection rotation. Database does not revoke shared administrative memberships. Native DROP ROLE removes memberships only when dependency checks permit role deletion. Credentials rotation and resource removal do not offboard old SQL administrators; explicit revocation is separate. Current-state retry recovers SQL success followed by failed status publication without rollback of retained service access.

## Current connection and endpoint changes

Database and DatabaseRole cluster references remain immutable, following CNPG. PostgresCluster is our external connection descriptor, which CNPG does not provide.

Changing the descriptor's connection settings, or recreating it under the same namespace/name, selects the current connection. Requeue dependent resources, reconcile their current desired SQL state on that connection, and refresh requested connection Secrets. Do not stop merely because the descriptor UID or endpoint changed. The operator does not copy data, migrate existing tables, or clean up objects on the previous server.

A connection change must trigger SQL reconciliation even when the dependent CR generation and password Secret version are unchanged. An applied result for the previous connection does not confirm SQL readiness at the new endpoint. Initial publication may precede SQL readiness under the connection Secret contract. Refresh an existing output for a changed endpoint after reconciling its role/password, database, and CONNECT on the current target; unrelated table permissions do not block that refresh. This is a dependency trigger, not an approval or rebind workflow.

## Leadership and normal retries

Use controller-runtime leader election with a Kubernetes Lease. Only the active leader runs reconcilers. Serialize resources for the same management key inside that process, read conflict decisions authoritatively, and propagate cancellation and SQL timeouts. On leadership loss, stop reconciliation and close the operator's SQL connections. Do not release leadership while continuing background mutation work.

CNPG's [operator manager](https://github.com/cloudnative-pg/cloudnative-pg/blob/63d0bd5a77dce5d86edc010e4d935defdf48e1af/internal/cmd/manager/controller/controller.go#L115) uses leader election. Its SQL controllers run in the designated primary instance. The [primary Lease implementation](https://github.com/cloudnative-pg/cloudnative-pg/blob/63d0bd5a77dce5d86edc010e4d935defdf48e1af/internal/cmd/manager/instance/run/lease/runnable.go#L519) stops the instance manager and PostgreSQL after confirmed takeover by another holder. Our operator cannot stop an external PostgreSQL server; its Lease controls operator work only.

This is ordinary Kubernetes leader-election behavior, not a guarantee that an already submitted SQL command is fenced by the database server. [client-go](https://pkg.go.dev/k8s.io/client-go/tools/leaderelection) explicitly does not provide fencing. SQL advisory locks or fencing tokens are not additional requirements of this initial design.

## State and authorization boundaries

The [API baseline](api-proposal.md#permissions-based-on-the-cnpg-proposals) defines managed-grant/pending-change persistence before SQL mutation and recovery within the same CR UID. SQL-name conflict checks do not replace that bookkeeping. CR deletion ends permission history; retained SQL grants require explicit absent entries for historical cleanup in a recreated CR. Database persists concrete target-scoped bindings in `managedACLTargets`, the selected `activeACLTarget`, pending changes and pre-operation grantor/owner snapshots. Target and per-binding revisions prevent using stale OIDs after endpoint changes or descriptor recreation. Inactive targets are retained but never contacted; recovery re-resolves identity only on the current target. A checkpoint must fit the 768 KiB serialized whole-resource budget before its corresponding SQL mutation. Catalog confirmation permits pruning completed responsibility; uncertain identity or an unobserved external owner merge retains pending cleanup rather than revoking an independent grant. Explicit absent can resolve that requested removal. See [runtime status and constraints](api-proposal.md#lifecycle-ownership-and-status).

Within a trusted target revision, a replacement database OID retires its old child/default bindings before lookup. Before staging another owner change, resolve and checkpoint the original pending transition so cleanup provenance survives successive changes. ACL transactions confirm the affected object and role identities and exact ACL effect before commit, then reread afterward. A mismatch rolls back and retains pending responsibility. Catalog/access reads use the configured client deadline through full result consumption. Obsolete administrator CONNECT pins use the same remaining-access check as other revokes, preserving the current administrator's access without adding unnecessary pins.

[Passwords and connection Secrets](api-proposal.md#passwords-and-connection-secrets) define initial publication before SQL readiness, rotation after SQL password application, retained Secret reuse, and conditional provenance/ownership updates. Reusing retained credentials does not restore deleted ACL history. Apply authorization and SQL-name conflicts before any initial export.

[Namespace access removal](cross-namespace-access.md#access-removal-and-resumption) stops SQL and Secret changes and finalizer cleanup until authorization returns, including for an existing manager. Ownership does not bypass that policy.

## Validation

Test normal create/update retries after SQL and status failures; existing objects; same-name CR recreation with retain and a fresh ACL history; conflicts within and across namespaces; equal metadata.name values; failed first reconciliation and losing-CR deletion; and ensure=absent name management. Also test Lease takeover and cancellation, declarative grant removal within one CR UID, historical cleanup through explicit absent after recreation, managed-grant recovery after failed state writes and controller restart, rename/owner changes, initial connection export before SQL readiness, current-source password rotation, retained Secret reuse/conflicts, endpoint updates, descriptor recreation, and access removal/resumption including pending finalizers.

These are implementation checks, not claims of tested controller behavior. Static documentation and YAML checks cannot validate runtime reconciliation.
