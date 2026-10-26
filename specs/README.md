# Brewlet specifications

Current Brewlet is a rewrite. Brewlet 0.x on `main` was its inspiration, not a
compatibility target. The authoritative contract for this branch is
[`SPECIFICATION.md`](SPECIFICATION.md):

- **Part I (§1–§15)** — language-agnostic runtime images, inventory,
  provisioning, workload execution, admission, the v2 capability-label and
  startup-fence contract (§9.1–§9.3), security and observability.
- **Part II (§16)** — governed dependency composition and remediation.

Implementations and user documentation must follow it. Substantial changes
should begin as a reviewable design under [`proposals/`](proposals/README.md).
Qualification is recorded separately in the
[acceptance ledger](../acceptance/README.md).

## Citations

Bare `§N` references cite sections of `SPECIFICATION.md`.
References written as `SPEC §N` cite the 0.x design reference,
[`SPECIFICATION.md` at main commit `fa70853`](https://github.com/microsoft/brewlet/blob/fa7085335b220e29df6affbece4558c5f1259ecb/specs/SPECIFICATION.md),
and its [capability labels](https://github.com/microsoft/brewlet/blob/fa7085335b220e29df6affbece4558c5f1259ecb/specs/CAPABILITY_LABELS.md).
They identify behavior that current Brewlet replaces, retains or generalizes. Requirements
apply only as restated by the current specification and acceptance ledgers.

## Versioning and compatibility

[`release.json`](../release.json) is the authoritative release identity. The
[compatibility policy](../docs/compatibility-policy.md) governs cross-release
promises; there is no compatibility layer for 0.x artifacts, APIs or labels.

## Implementations

- [core runtime](..) — CLI, containerd shim, and node provisioner
- [Kubernetes platform](../kubernetes) — operator, admission webhooks, CRDs,
  manifests, and Helm chart
- [Maven plugin](../maven-plugin) — optional Java producer

Changes here describe the contract; implementation-specific behavior belongs in
the directory that owns that component. A contract change that affects multiple
implementations should update the relevant components and
[integration tests](../integration-tests) in the same pull request where practical.
