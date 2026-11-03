# Example guide

These manifests show how to configure connections, roles, databases, permissions, and application credentials. See the [API reference](api-proposal.md) for field details and the [implementation status](README.md#implementation-status) for supported behavior.

## Choose a resource set

| Example | Purpose |
| --- | --- |
| [postgres-connection.yaml](../examples/microservice/postgres-connection.yaml) | Shared operator connection referencing existing administrative and CA Secrets |
| [service-owner-and-admin-readonly.yaml](../examples/microservice/service-owner-and-admin-readonly.yaml) | The service owns its database and runs migrations; the admin UI gets SELECT. The operator creates passwords and two connection Secrets |
| [separate-migrations-and-admin-readonly.yaml](../examples/microservice/separate-migrations-and-admin-readonly.yaml) | Separate owner and migrator, runtime DML, admin SELECT, and three connection Secrets |
| [orders-service-password-provided.yaml](../examples/microservice/orders-service-password-provided.yaml) | Supplied basic-auth password as an alternative to generation |
| [orders-service-deployment.yaml](../examples/microservice/orders-service-deployment.yaml) | The service consumes DATABASE_URL and a CA from its connection Secret |
| [orders-admin-deployment.yaml](../examples/microservice/orders-admin-deployment.yaml) | The admin UI consumes DATABASE_URL and a CA from its separate read-only connection Secret |
| [infra-connection.yaml](../examples/cross-namespace/infra-connection.yaml) | Shared connection in infra, selecting orders and billing namespaces |
| [orders-service-and-admin-readonly.yaml](../examples/cross-namespace/orders-service-and-admin-readonly.yaml) | Orders database, service user, read-only admin user, and connection outputs in orders |
| [billing-service-and-admin-readonly.yaml](../examples/cross-namespace/billing-service-and-admin-readonly.yaml) | Billing database, service user, read-only admin user, and connection outputs in billing |
| [minimal.yaml](../examples/api-proposal/minimal.yaml) | Connection, LOGIN role, and database with minimal fields |
| [full.yaml](../examples/api-proposal/full.yaml) | Full owner/reader/writer configuration, schemas, extension, and permissions |
| [role-table-permissions.yaml](../examples/api-proposal/role-table-permissions.yaml) | Repeated app_user rules with different table sets and privileges, plus an orders_reader all-table rule |
| [readonly.yaml](../examples/api-proposal/readonly.yaml) | Reader group and user, updating the Database from minimal |
| [multi-cluster.yaml](../examples/api-proposal/multi-cluster.yaml) | The same SQL username on two independent clusters |
| [aws-rds.yaml](../examples/api-proposal/aws-rds.yaml) | RDS grants/default privileges, password users, and IAM users |
| [llm-readonly.yaml](../examples/api-proposal/llm-readonly.yaml) | SELECT on explicitly listed existing tables |
| [llm-readonly-after-removal.yaml](../examples/api-proposal/llm-readonly-after-removal.yaml) | Removing orders from the permission set removes its managed SELECT |
| [llm-readonly-with-absent-grant.yaml](../examples/api-proposal/llm-readonly-with-absent-grant.yaml) | Optional absent state on orders SELECT while preserving invoices SELECT |

Choose one microservice variant and use the shared postgres-connection example. Deployment images are placeholders. The supplied-password example replaces the orders-service role declaration during initial configuration. Minimal/full/RDS/LLM are other alternative resource sets. Readonly extends minimal and updates Database/app; LLM after-removal and with-absent-grant are alternative updates of the original LLM example. Prerequisites, Secret lifecycle, declarative permission cleanup, CNPG differences, table access limits, IAM, and ADMIN/INHERIT/SET behavior are documented in the API baseline. Removing a previously declared permission within the same CR UID removes its managed grant; ensure=absent controls only the grant and also supports cleanup after CR recreation.

Do not apply alternative sets together under overlapping resource or SQL names. For independent deployments on the same server, choose distinct Kubernetes resource and SQL names; separate namespaces alone do not isolate SQL catalogs. Use separate canonical connections only for separate logical servers.

The role-table-permissions example is an alternative Database/orders configuration. It assumes ExternalPostgres/pg-prod, SQL roles orders_owner, app_user and orders_reader, and existing app.customers, app.products, app.orders and app.order_items tables. It grants app_user SELECT on customers/products, SELECT/INSERT on orders/order_items, and UPDATE on orders through a second rule for that same table. The reader rule omits tables and therefore selects all current tables, plus later tables discovered on reconciliation. Add these rules to an existing managing Database rather than creating a competing resource.

Permission recipients use role throughout database, schema, sequence and default-privilege maps. Table rules use role, optional tables, required privileges and optional ensure. An omitted tables selector means all tables; an explicit empty selection is rejected at admission. Explicit null is preserved and fails reconciliation before SQL rather than selecting all tables. Table names are literal identifiers in the containing schema, including names such as `*`, `ALL` or `a.b`; do not prepend the schema name. See the [permission contract](api-proposal.md#declarative-permissions) for selector validation, duplicate/overlap rejection, atomic-list ownership and SQL examples.

These source examples target the role-oriented schema for fresh installations. Use a matching CRD/controller release and strict field validation; they are not compatible with older releases whose permission entries use name. The selected shape has no legacy conversion or supported rollback to the older permission schema.
