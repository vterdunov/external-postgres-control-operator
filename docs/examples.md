# Examples

The [examples catalog](../examples/README.md) lists working manifests by scenario, including standalone microservices, shared PostgreSQL infrastructure, AWS RDS, read-only table access, and credential options.

Start with one microservice resource set and its connection descriptor. Treat alternative manifests as alternatives: applying overlapping examples together can create competing Kubernetes managers or duplicate SQL names. Separate Kubernetes namespaces do not create separate PostgreSQL catalogs.

Before applying an example, review [cross-namespace access](guides/access.md), [permissions and lifecycle](guides/lifecycle.md), and [credentials](guides/credentials.md). The API details are in the [generated API reference](reference/api.md).
