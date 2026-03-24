# ext-postgres-operator Helm Chart

This Helm chart deploys the External Postgres Operator, which provides a way to manage PostgreSQL databases and users in a Kubernetes environment.

## Installation

To install the chart, use the `helm upgrade --install` command:

```bash
helm upgrade --install -n operators ext-postgres-operator oci://ghcr.io/vterdunov/external-postgres-control-operator/charts/ext-postgres-operator
```

## Compatibility

**NOTE:** Helm chart version `>= 3.0.0` requires External Secret Operator version `>= 0.17.0`. Ensure that you are using the correct versions to avoid compatibility issues.

**NOTE:** Helm chart version `>= 2.0.0` is only compatible with the Postgres Operator version `2.0.0`. Ensure that you are using the correct versions to avoid compatibility issues.
