# External PostgreSQL Control Operator

Manage PostgreSQL roles, databases, and permissions with Kubernetes manifests.
EPCO connects to existing PostgreSQL servers and keeps database access and application credentials in sync.

Supports PostgreSQL **15, 16, 17, and 18**.

## Features

- [Roles, databases, schemas, and extensions](examples/api-proposal/full.yaml).
- [Separate owner, migration, runtime, and read-only access](examples/microservice/separate-migrations-and-admin-readonly.yaml).
- [Permissions for specific tables](examples/api-proposal/llm-readonly.yaml), including [grant removal](examples/api-proposal/llm-readonly-after-removal.yaml).
- [Generated passwords and application connection Secrets](examples/microservice/service-owner-and-admin-readonly.yaml), or [passwords you provide](examples/microservice/orders-service-password-provided.yaml).
- [Shared PostgreSQL connections across namespaces](examples/cross-namespace/infra-connection.yaml).
- [TLS connections](examples/microservice/postgres-connection.yaml), with [AWS RDS examples](examples/api-proposal/aws-rds.yaml).

## How it works

`DatabaseRole` and `Database` share a `PostgresCluster` connection. EPCO uses these declarations to manage SQL objects on your PostgreSQL server.

```mermaid
---
config:
  theme: base
  fontFamily: "system-ui, sans-serif"
  htmlLabels: false
  themeCSS: |
    .node rect { rx: 8px; ry: 8px; stroke-width: 1px; filter: drop-shadow(0px 3px 1px #3c383680) drop-shadow(0px 9px 7px #3c383659); }
    .cluster rect { rx: 12px; ry: 12px; filter: drop-shadow(0px 4px 2px #28282866) drop-shadow(0px 9px 9px #28282859); }
  themeVariables:
    fontFamily: "system-ui, sans-serif"
    fontSize: "14px"
    primaryColor: "#d2b98b"
    primaryTextColor: "#3c3836"
    primaryBorderColor: "#8c755e"
    lineColor: "#8c755e"
    textColor: "#3c3836"
    clusterBkg: "#ddc9a4"
    clusterBorder: "#8c755e"
    titleColor: "#3c3836"
    edgeLabelBackground: "#ddc9a4"
  flowchart:
    nodeSpacing: 16
    rankSpacing: 28
    padding: 12
    diagramPadding: 24
    subGraphTitleMargin:
      top: 6
      bottom: 8
---
flowchart TB
    subgraph resources["Kubernetes custom resources"]
        direction TB
        database["Database<br/>Schemas and permissions"]
        role["DatabaseRole<br/>Role and password"]
        cluster["PostgresCluster<br/>Connection and TLS"]
        database -->|spec.cluster| cluster
        role -->|spec.cluster| cluster
        database -.->|"optional roleRef<br/>connection Secrets"| role
    end

    resources -->|desired state| operator["EPCO operator"]
    operator -->|SQL| postgres[("External PostgreSQL")]

    classDef source fill:#e0c89b,stroke:#8c755e,color:#3c3836
    classDef output fill:#c2c18d,stroke:#8c755e,color:#3c3836
    classDef consumer fill:#cca98d,stroke:#8c755e,color:#3c3836
    class cluster source
    class database,role output
    class operator,postgres consumer
```

## Install

Choose Helm or Flux for a fresh installation. Install one EPCO release per Kubernetes cluster.

### Helm

```sh
helm install epco oci://ghcr.io/vterdunov/charts/external-postgres-control-operator \
  --version 0.1.9 \
  --namespace epco-system \
  --create-namespace
```

### Flux

Add these resources to your Flux configuration:

```yaml
apiVersion: source.toolkit.fluxcd.io/v1
kind: OCIRepository
metadata:
  name: epco
  namespace: flux-system
spec:
  interval: 1h
  url: oci://ghcr.io/vterdunov/charts/external-postgres-control-operator
  ref:
    tag: "0.1.9"
  layerSelector:
    mediaType: application/vnd.cncf.helm.chart.content.v1.tar+gzip
    operation: copy
---
apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: epco
  namespace: flux-system
spec:
  interval: 30m
  releaseName: epco
  targetNamespace: epco-system
  chartRef:
    kind: OCIRepository
    name: epco
  install:
    createNamespace: true
    crds: Create
    strategy:
      name: RetryOnFailure
  upgrade:
    crds: CreateReplace
    strategy:
      name: RetryOnFailure
```

### Upgrades

With Flux, update `spec.ref.tag` in the OCIRepository. Flux updates the CRDs before upgrading the operator.

With Helm CLI, apply the target release's CRDs, then upgrade its chart.
Replace `X.Y.Z` with a chart version that includes `crds.yaml` (added after `0.1.9`):

```sh
EPCO_VERSION=X.Y.Z
EPCO_CRDS_URL="https://github.com/vterdunov/external-postgres-control-operator/releases/download/chart-v${EPCO_VERSION}/crds.yaml"
kubectl apply --server-side --force-conflicts -f "$EPCO_CRDS_URL" &&
kubectl wait --for=condition=Established --timeout=120s -f "$EPCO_CRDS_URL" &&
helm upgrade epco oci://ghcr.io/vterdunov/charts/external-postgres-control-operator \
  --version "$EPCO_VERSION" --namespace epco-system --wait
```

Pass any custom `--values` options to `helm upgrade`. Both methods keep CRDs on uninstall.

See the [example guide](docs/examples.md) to connect your PostgreSQL server and configure application access.
The [documentation](docs/README.md) covers the API, lifecycle, and development.

## License

Licensed under the [Apache License 2.0](LICENSE).
