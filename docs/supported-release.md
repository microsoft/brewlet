# Prerelease distribution and recovery

Release infrastructure is not a published or qualified release. The initial
candidate identity is **2.0.0-alpha.1**, not GA. No live distribution/transition
receipt is supplied by this change. Missing current real-node evidence and
release-owner policy still block publication.

## One identity, explicit distribution surfaces

[`release.json`](https://github.com/microsoft/brewlet/blob/brewlet-2.0/release.json)
is authoritative. Its version maps unchanged to chart version/appVersion,
Maven version and component image tags; Git tags add `v`. A candidate manifest
records the immutable source commit, production/qualification-input hashes,
payload hashes, image index/child digests and actual build tools. `brewlet version`
reports JSON with version, commit and native platform. Unstamped source builds
report `dev`/`unknown`, not a release.

| Surface | Intended coordinate | Targets |
| --- | --- | --- |
| CLI | GitHub prerelease asset `brewlet_VERSION_OS_ARCH.tar.gz` | Linux/macOS amd64/arm64 |
| Operator | `ghcr.io/microsoft/brewlet-operator:VERSION` | Linux amd64/arm64 |
| Admission | `ghcr.io/microsoft/brewlet-admission:VERSION` | Linux amd64/arm64 |
| Node worker and shim | `ghcr.io/microsoft/brewlet-node-provisioner:VERSION` | Linux amd64/arm64 |
| Helm | `oci://ghcr.io/microsoft/charts/brewlet`, version `VERSION` | Matching CRDs and digest-pinned component images |
| Optional Maven producer | `sh.brewlet:brewlet-maven-plugin:VERSION` | Java 17+; matching Brewlet CLI |

These are intended coordinates, not evidence that a version exists there.
There are no Windows archives, floating GA/latest aliases, or supported Go
library/module API. The `v2alpha1` Kubernetes API does not promise arbitrary
cross-release state compatibility.

## Build and inspect without publishing

From a clean committed checkout, with Go, Maven, Helm, Docker Buildx and the
pinned Syft tool available:

```sh
make release-check
make release-candidate
```

`scripts/release_tools.py` installs explicitly named Syft/ORAS/ghx binaries
into a caller-selected directory after validating checked-in SHA-256 pins.
It does not install globally, create keys or configure accounts. The workflows
pin action commits, Go and Helm versions; candidate metadata records actual
Maven/Helm versions. Reproducibility comparisons require matching toolchains and
base digests; this is not a claim of toolchain-independent binary identity.

Candidate output is under ignored `dist/candidate/`. Archives have fixed
timestamps, owners, modes and ordering. OCI images are built from a committed
source export, never a directory containing credentials or private test state.
The chart is repackaged deterministically with exact image index digests. Maven
binary, POM, sources and Javadoc are retained as exact bytes; the release profile
uses a source timestamp and excludes Javadoc wall-clock timestamps.

The manual `release-build.yml` workflow builds these outputs, checks packaging
and generates SPDX project SBOMs. Native jobs execute the archived CLI on all
four targets and sign their outcome records. Downloaded artifacts must come
from an exact successful same-repository/source workflow run. No current build
job publishes a package or executes a node installation.

## Legal and project supply-chain evidence

CLI archives include target-specific generated NOTICE and Brewlet LICENSE.
Final images include their generated notices and license, retaining base-image
legal resources. Binary/source/Javadoc JARs and the chart carry legal resources.
See [third-party notices](third-party-notices.md). Actual archive/JAR/image
contents are inspected; a Dockerfile string check is not distribution proof.

Project SPDX documents describe the exact CLI payloads, Maven runtime closure,
chart, image platform filesystems and image-index children. The primary subject
has the payload digest and explicit package relationships. The index SBOM is a
manifest-set inventory; it is not a scan of an arbitrary default platform.
These are **Brewlet distribution SBOMs**, separate from the product's application
composition and dependency-layer SBOM/signature features.

GitHub OIDC provenance signs the payload/inventory/SBOM bytes and native outcome
records. Publication attests exact registry index and chart digests; index
membership binds each native image manifest. Verification pins repository,
workflow, workflow/source SHA and subject digest, and rejects self-hosted
signers. Hashes alone do not authenticate a publisher.

The owner must provide a reviewed `publicationPolicy.legalApproval` record
with `approved: true`, an authoritative `reference`, and
`containerPolicy: no-additional-text-required`, **only if that is the owner's
actual determination**. If additional policy text is required, publication
remains blocked until authoritative text is supplied and its packaging is
implemented and verified. Do not manufacture an approval record.

The trusted existing Maven GPG fingerprint also remains unset. Maven Central
requires that owner-selected fingerprint, an existing signing key/passphrase and
existing Central credentials. No secret values belong in source. Namespace
ownership, credential/environment availability, registry permissions/visibility,
attestation entitlement and native hosted-runner availability require owner
confirmation before use.

## Qualification and explicit publication

Ordinary PR checks remain separate from strict promotion. `release.yml` is
manual only: it calls both ordinary CI and the existing reusable strict
promotion gate. All publication depends on their exact success plus validated
artifact/transition evidence. A failed, skipped, cancelled or missing
dependency is not success. There is no acceptance opt-out.

Publication requires an existing tag resolving to the exact candidate source,
explicit `publish-vVERSION` confirmation, the successful candidate run and both
authorized native transition runs. The workflow does not create tags. It
rechecks source, signed receipts, artifact digests and owner policy before using
publishing credentials. An old source receipt cannot qualify changed inputs.

Image transfer preserves verified OCI digests; the chart must resolve to the
same package bytes. Central submission signs a bundle of the prebuilt JAR/POM
bytes rather than rebuilding. GitHub receives a prerelease, never GA/latest.
An existing differing version/tag/asset is refused, not overwritten.

Publication across GHCR, Central and GitHub is **not transactional**. A failed
run can leave published components; do not infer support from a partial release.
Preserve `publication-reconciliation` outputs and any Central deployment ID.
Inspect existing immutable bytes before retrying; an in-flight/partial Central
deployment requires owner reconciliation, not another blind submission.
Never retag different bytes to hide a failure.

After an authorized publication, from its trusted source checkout with ghx,
ORAS, Helm, Go and the already trusted Maven public key available:

```sh
python3 scripts/verify_release.py --source <full-reviewed-source-commit>
```

Verification fetches published assets, checksums, chart/images and Maven
coordinates independently, verifies signed subjects and GPG identity, and checks
the packaged contracts. Supply read credentials through normal CLI/environment
configuration if required, never command-line secrets. Missing artifacts,
attestations or credentials are failures, not optional checks.

## Bounded transitions and recovery

| Transition | Policy and current evidence |
| --- | --- |
| Same `2.0.0-alpha.1` candidate, clean reinstall | Only initial eligible test pair; requires exact native amd64/arm64 distribution receipts before publication. Not yet verified. |
| 0.x/1.x to 2.0 | No supported in-place transition or old-state reuse. Re-author workloads and review a separate migration. |
| Different 2.0 snapshots/prereleases | Unsupported until immutable source/target artifacts, cleanup/recovery limits and genuine evidence are explicitly reviewed. |
| Helm/binary downgrade or mixed releases | No compatibility or rollback promise. |

Follow [safe removal](uninstallation.md): stop writers, inventory original UIDs
and recovery obligations, drain application/Exec/mount references, let production
profile cleanup complete, then uninstall without bypassing hooks. A failed hook,
retained finalizer or missing host is an unresolved obligation, not authority to
delete state. Never erase roots, leases, journals, claims or shared CRDs to force
progress. Verify host containerd reversal and CRI health before reinstalling.

Retain shared CRDs/inventory when appropriate; Helm does not generally upgrade
or delete CRDs for you. Review schemas and apply the exact target package's CRDs
before a matching install. Restore data through its separately governed backup/
recovery process. Helm rollback cannot repair incompatible persisted state.
Reinstalling a prior release would itself require a reviewed clean-install
procedure, not a claimed downgrade.

`release-transition.yml` is an explicitly authorized disposable-node test,
separate from ordinary CI and the existing complete/managed/Wasm live scopes.
It consumes exact candidate archives/OCI layouts/chart, runs real workloads,
drains and tears down through production paths, checks host reversal, retains
schemas, reinstalls the same candidate and verifies new workload execution.
Only its own private fixture resources may be destroyed. The manual
`authorize-disposable-drain-reinstall` input is required per native architecture.
Offline orchestration tests do not establish real-node support, cloud-provider
behavior or missing-host recovery proof.
