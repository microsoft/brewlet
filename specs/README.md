# Brewlet specifications

The authoritative Brewlet contract is
[`SPECIFICATION.md`](SPECIFICATION.md):

- **Part I (§1–§15)** — language-agnostic runtime images, inventory,
  provisioning, workload execution, admission, the v2 capability-label and
  startup-fence contract (§9.1–§9.3), security and observability.

Implementations and user documentation must follow it. Substantial changes
should begin as a reviewable design under [`proposals/`](proposals/README.md).
Qualification is recorded separately in the
[acceptance ledger](../acceptance/README.md).

## Citations

Bare `§N` references and references written as `SPEC §N` cite sections of
`SPECIFICATION.md`.

## Versioning and compatibility

[`release.json`](../release.json) is the authoritative release identity. The
[compatibility policy](../docs/compatibility-policy.md) governs cross-release
promises.

## Implementations

- [core runtime](..) — CLI, containerd shim, and node provisioner
- [Kubernetes platform](../kubernetes) — operator, admission webhooks, CRDs,
  manifests, and Helm chart
- [Maven plugin](../maven-plugin) — optional Java producer

Changes here describe the contract; implementation-specific behavior belongs in
the directory that owns that component. A contract change that affects multiple
implementations should update the relevant components and
[integration tests](../integration-tests) in the same pull request where practical.
