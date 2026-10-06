# Brewlet specifications

The current reference documents in this directory are the authoritative source
for Brewlet architecture and compatibility contracts. Implementations and user
documentation must follow [`SPECIFICATION.md`](SPECIFICATION.md); substantial changes should begin as a
reviewable design in [`proposals/`](proposals/) and remain on the
[roadmap](../ROADMAP.md) until they ship.

## Versioning and citations

The specification is a living, versioned contract for the current release.
The [pre-GA compatibility policy](../docs/compatibility.md) governs cross-release
promises: superseded Brewlet interfaces have no automatic retention obligation,
and release updates default to safe teardown/reinstallation. Incompatible changes
must be documented explicitly; versioned contracts require explicit transitions,
not an assumed migration period unless a named exception requires one.
Compatible clarifications and additions may land incrementally.
Brewlet components and documentation cite
requirements by section using the existing `§N` convention (for example, `§4.2`).

The `**Version:**` header in `SPECIFICATION.md` tracks the Brewlet release version
without the `v` prefix, including any prerelease suffix. Before creating a release
tag or dispatching the release workflow, update and commit this header to match
the intended release (for example, `x.y.z` for `vx.y.z`). Check it locally with
`bash scripts/check-release-version.sh "$RELEASE_VERSION"` from the repository
root, setting `RELEASE_VERSION` to the intended version without the `v` prefix.
The release workflow refuses to publish if the checked-out specification does
not match.

Public reference contracts:

- [Capability labels](CAPABILITY_LABELS.md) — stable node scheduling labels,
  admission affinity semantics, and autoscaler recipes. Its contract versioning
  and dual-publication migration period remain an explicit pre-GA exception,
  not a general installation-upgrade promise.

## Implementations

- [core runtime](..) — CLI,
  containerd shim, and node provisioner
- [Kubernetes platform](../kubernetes) — Kubernetes
  operator, admission webhooks, CRDs, manifests, and Helm chart
- [Maven plugin](../maven-plugin) — Maven build
  and publishing integration

Changes here describe the contract; implementation-specific behavior belongs in
the directory that owns that component.

## Current contracts, proposed designs, and historical records

The specification documents only behavior available in Brewlet releases.
Proposed functionality belongs in the [roadmap](../ROADMAP.md), with unimplemented
engineering designs under [`proposals/`](proposals/README.md). Proposals are not
current contracts.

Implemented design proposals and dated security assessments are retained in the
[archive](../archive/README.md) as non-normative historical records. Their original
assumptions, rationale, and remediation statements do not define current
guarantees; use the current reference contracts above and
[security guidance](../docs/security.md) instead.

## Supporting project areas

- [user and operator documentation](../docs/)
- [integration tests](../integration-tests) — cross-component validation and
  fixture applications

A specification change that affects multiple implementations should update the
relevant components and integration tests in the same pull request where practical.
