# SQL object ownership and reconciliation

Status: selected design for the [accepted API baseline](api-proposal.md), updated 2026-10-07. Follow inspected CNPG behavior for retries, existing SQL objects, CR conflicts, and reclaim policies. Controllers are not implemented.

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
6. Otherwise, create a missing SQL object or reconcile an existing one, then record the successful generation.

Temporary failure does not erase a manager's recorded reconciliation. Same-UID retries continue managing the same SQL object. A pure observer is not a manager merely because it recorded an observed generation; a former manager retains its management record while its CR remains.

The intended winner is the first recorded manager. Kubernetes can accept both CRs, and strict kubectl apply ordering is not guaranteed. The CNPG list/status check is not an atomic distributed claim. No separate ownership ConfigMap, SQL claim table, or UID tombstone is required for this conflict model. Permission bookkeeping is a separate requirement for declarative removal of grants.

## Current connection and endpoint changes

Database and DatabaseRole cluster references remain immutable, following CNPG. PostgresCluster is our external connection descriptor, which CNPG does not provide.

Changing the descriptor's connection settings, or recreating it under the same namespace/name, selects the current connection. Requeue dependent resources, reconcile their current desired SQL state on that connection, and refresh requested connection Secrets. Do not stop merely because the descriptor UID or endpoint changed. The operator does not copy data, migrate existing tables, or clean up objects on the previous server.

A connection change must trigger SQL reconciliation even when the dependent CR generation and password Secret version are unchanged. An applied result for the previous connection does not confirm SQL readiness at the new endpoint. Initial publication may precede SQL readiness under the connection Secret contract. Refresh an existing output for a changed endpoint after reconciling its role/password, database, and CONNECT on the current target; unrelated table permissions do not block that refresh. This is a dependency trigger, not an approval or rebind workflow.

## Leadership and normal retries

Use controller-runtime leader election with a Kubernetes Lease. Only the active leader runs reconcilers. Serialize resources for the same management key inside that process, read conflict decisions authoritatively, and propagate cancellation and SQL timeouts. On leadership loss, stop reconciliation and close the operator's SQL connections. Do not release leadership while continuing background mutation work.

CNPG's [operator manager](https://github.com/cloudnative-pg/cloudnative-pg/blob/63d0bd5a77dce5d86edc010e4d935defdf48e1af/internal/cmd/manager/controller/controller.go#L115) uses leader election. Its SQL controllers run in the designated primary instance. The [primary Lease implementation](https://github.com/cloudnative-pg/cloudnative-pg/blob/63d0bd5a77dce5d86edc010e4d935defdf48e1af/internal/cmd/manager/instance/run/lease/runnable.go#L519) stops the instance manager and PostgreSQL after confirmed takeover by another holder. Our operator cannot stop an external PostgreSQL server; its Lease controls operator work only.

This is ordinary Kubernetes leader-election behavior, not a guarantee that an already submitted SQL command is fenced by the database server. [client-go](https://pkg.go.dev/k8s.io/client-go/tools/leaderelection) explicitly does not provide fencing. SQL advisory locks or fencing tokens are not additional requirements of this initial design.

## Declarative permissions and current passwords

Permissions describe the desired managed grant set. Entries default to ensure=present. Removing a previously declared grant, role entry, table entry, or permission block removes its managed access; ensure=absent on a grantee entry is an optional explicit absence state. Neither permission removal nor an absent grant drops the SQL table. Database/schema/table/default-privilege permissions are not present in the inspected CNPG Database API. This desired-state and omission behavior deliberately differs from its [FDW/server usage helper](https://github.com/cloudnative-pg/cloudnative-pg/blob/63d0bd5a77dce5d86edc010e4d935defdf48e1af/internal/management/controller/database_controller_sql.go#L596) and [proposal #10826](https://github.com/cloudnative-pg/cloudnative-pg/issues/10826).

Persist the managed grant set, resolved object/grantor bindings, and sufficient pending changes in the live Database CR's status before SQL mutations. After a failed state write, controller restart, or newer spec within the same CR UID, inspect current SQL and reconcile to the latest desired set, including cleanup. A removed grant must not be forgotten or blindly reapplied. This bookkeeping is required by the chosen declarative API; it does not add a user-facing resource or archive passwords. The previous command-style grant/revoke and stop-managing-on-removal decision is superseded.

CR deletion removes this status history. A recreated CR starts a new permission-management session even when its namespace/name matches the previous CR. With retain, historical SQL grants stay in place until explicitly declared absent; omission in the recreated spec does not revoke them. Removal from the list still revokes grants tracked by the current live CR UID. Retained password-source and connection-output reuse remains supported independently. Restoring ACL history across CR deletion is deferred: do not create a grant-history ConfigMap, SQL journal, or controller metadata in SQL comments for this purpose.

The source Secret contains the current desired password. Secret updates are ordinary rotation, and retries use the latest source. Track the applied Secret resourceVersion, as CNPG does, without password snapshots, archived versions, or rollback credentials. Password generation and Database connection outputs are our extensions; retained sources/outputs can be reused after the former resource UID retires, with provenance checked and ownership updated conditionally. No unrelated Secret is overwritten.

Initial connection outputs can be published once their current inputs and password source are available, after authorization and SQL-name/reference/Secret checks. Do not require SQL readiness before initial publication: a migration Job may use the Secret and retry while roles, the database, or named tables are pending. Secret existence does not imply Ready. Existing-output password rotation remains ordered after SQL accepts the current source; pending table permissions for another role do not block that refresh. Deployment tooling owns migration ordering and retry behavior.

## Namespace access removal

When a Namespace no longer matches the access selector, stop work on its resources. Keep SQL and existing Secrets unchanged. Report AccessDenied and do not run finalizer cleanup, including for previously managed resources with reclaim=delete. An existing finalizer remains pending until authorization returns. Resumption uses current resource specs, connection settings, and source Secrets.

The accepted [cross-namespace contract](cross-namespace-access.md) records comparisons with RabbitMQ and Strimzi. CNPG has no equivalent external connection namespace selector.

## Validation

Test normal create/update retries after SQL and status failures; existing objects; same-name CR recreation with retain and a fresh ACL history; conflicts within and across namespaces; equal metadata.name values; failed first reconciliation and losing-CR deletion; and ensure=absent name management. Also test Lease takeover and cancellation, declarative grant removal within one CR UID, historical cleanup through explicit absent after recreation, managed-grant recovery after failed state writes and controller restart, rename/owner changes, initial connection export before SQL readiness, current-source password rotation, retained Secret reuse/conflicts, endpoint updates, descriptor recreation, and access removal/resumption including pending finalizers.

These are implementation checks, not claims of tested controller behavior. Static documentation and YAML checks cannot validate runtime reconciliation.
