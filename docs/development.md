# Development

Use the versions pinned in `mise.toml` and `mise.lock`:

```sh
mise trust
mise install --locked
mise run --skip-tools tools-install
```

## Build and tests

Run a focused suite while developing, then the project check before submitting changes:

```sh
mise run build
mise run test-unit
mise run test-envtest
mise run test-integration
mise run test-e2e
mise run lint
mise run check-generated
mise run check
```

The unit and envtest suite sources live under `test/unit/` and `test/envtest/`. The [integration suite](../test/integration/README.md), [e2e suite](../test/e2e/README.md), [delivery checks](../test/delivery/README.md), and [release checks](../test/release/README.md) describe their environments and coverage. Integration tests require Docker; e2e tests use disposable Kind. Use explicit Kubernetes contexts for commands that access a cluster.

## Generated files and API docs

Edit API definitions under `api/v1alpha1`. Generate DeepCopy code and CRDs using the pinned mise tasks. Never hand-edit generated code, CRDs, chart inputs, or API documentation. The pinned `crd-ref-docs` reads Go API comments and validation markers directly from `api/`, so documentation changes do not require regenerating CRDs first. Its EPCO config and Markdown template live under `tools/crd-ref-docs/`. The template exposes CEL rules because upstream's default Markdown template omits them. Generate and verify the published API reference with:

```sh
mise run docs-generate
mise run check-docs
```

## Delivery commands

Application and chart releases use separate semantic versions. Release packaging, signature verification, installation, upgrades, and safe removal are described in the [root installation guide](../README.md#install) and the release tooling under `tools/`. Commands that write to a Kubernetes cluster require an explicit context. CRDs are retained when the controller is uninstalled.
