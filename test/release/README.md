# Release behavior tests

Run with `mise exec -- python3 -m unittest discover -s test/release -p '*_test.py' -v`.
This suite uses private temporary files and executable command fakes. It performs
no network operations. It supplements real CI, registry, signature and installed
runtime acceptance; it does not establish cryptographic or live release proof.

The production CLI contract agreed with the coordinator is:

```
python3 tools/release.py image|chart --manifest PATH --phase validate|preflight|verify|publish
```

`validate` is pure. `preflight` accepts only release coordinates and returns JSON
including `completed: true` after verifying an existing immutable release. It
must run before workflow build or publication. `verify` validates prepared
candidate bytes and remote proof. `publish` signs only that stream's subjects,
finishes and verifies the draft, publishes last and checks immutability. Existing
immutable releases use a read-only verification branch in every external phase.
Success returns a JSON object. Failures return nonzero and a useful diagnostic.
Asset paths are relative to the manifest directory and cannot escape it.

The schema-version-1 fixture is the machine-readable example of the input:
independent image/chart versions and source SHAs, immutable original-image
coordinates, subject digests, generated CRD/RBAC provenance, local asset
checksums and signature bundles. A seed preflight manifest omits candidate
digests/assets. `required_workflows` is not an authority: the production tool
must enforce its own stable allowlist and cannot accept empty caller-defined
gates.

Trusted source gates use completed `push`/`main` runs at the exact source SHA,
latest relevant run/current attempt, and these stable names:

| Workflow | Required jobs |
| --- | --- |
| `test.yml` | `test-unit`, `test-envtest`, `check-generated`, `PostgreSQL 15`, `PostgreSQL 16` |
| `lint.yml` | `Run on Ubuntu` |
| `test-e2e.yml` | `E2E` |
| `delivery.yml` | `Release validation`, `Platforms` |

Publish runtime evidence references a GitHub run/attempt/artifact, not a caller's
`passed` boolean. The tool verifies the appropriate tag release workflow,
repository/source/ref, a completed successful `Runtime acceptance` job and the
artifact's ownership and exact `release-runtime-<attempt>` name. The downloaded `release-runtime.json` binds the exact
subjects and both source identities. The current release run may still be in
progress while its runtime job has completed. Existing image-release proof is
checked separately from the chart-source gates.

External boundaries use normal `gh`, `crane`, `helm` and `cosign` executables
from PATH. The fixtures record commands, source claims, authentication state,
downloads and writes. Verification failures are tool exit failures rather than
synthetic `signature_valid` booleans in the candidate manifest.

`helper_test.py` exercises copied production preparation/workspace helpers in
owned temporary source trees. Helm conversion/lint/package and Kustomize
installer rendering use the real pinned local tools. Container/registry/GitHub
boundaries remain offline fakes. It checks actual packaged inputs/defaults,
durable candidate recovery, nondeterministic signature reuse, measured runtime
aggregation/persistence, source pairing and native/installed identity guards.

`crd_bundle_test.py` checks the CRD-only `crds.yaml` download for both release
streams, including selected older-application inputs, deterministic output and
the signed checksum, evidence, checkpoint and publication inventories.

`workflow_test.py` parses the real workflows with Helm's YAML parser and checks
their shared queued release lock and attempt-scoped artifact selections. The
real pinned actionlint also validates both workflows. `source_pairing_test.py`
captures the lifecycle helper's legacy command cwd and bundle paths.
