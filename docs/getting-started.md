# Getting started

EPCO manages SQL objects on an existing PostgreSQL server. It does not create or operate the PostgreSQL server. The current project targets PostgreSQL 15, 16, 17, and 18; see the [root README](../README.md) for the supported-major list and the [API reference](reference/api.md) for resource fields.

Install one EPCO release per Kubernetes cluster using the [Helm or Flux instructions](../README.md#install). For upgrades, follow the [CRD upgrade instructions](../README.md#upgrades).

Create a `kubernetes.io/basic-auth` Secret in the namespace where `ExternalPostgres` will live. The Secret needs a nonempty `username` and a `password` key. Then create an `ExternalPostgres` pointing to the server and Secret. Database and role resources refer to this descriptor by name.

Choose one of the [microservice examples](../examples/README.md). A minimal setup has an `ExternalPostgres`, a LOGIN `DatabaseRole`, and a `Database` with that role as owner. For production, supply credentials through an approved Secret manager and grant the operator account only the PostgreSQL capabilities needed for the resources it manages.

Kubernetes namespace boundaries do not isolate PostgreSQL catalogs. Use distinct SQL names or distinct PostgreSQL servers when teams need independent ownership. Review [access](guides/access.md), [lifecycle](guides/lifecycle.md), and [credentials](guides/credentials.md) before applying shared-server examples.
