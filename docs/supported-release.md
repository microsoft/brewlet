# Release distribution and recovery

Releases are automated: pushing a `vVERSION` tag that matches
[`release.json`](https://github.com/microsoft/brewlet/blob/main/release.json)
builds, verifies and publishes every distribution surface. The current
release identity is **0.8.0**.

**Any version below 1.0.0 is a beta.** Betas (`0.y.z`), and any version with an
`-alpha.N`, `-beta.N` or `-rc.N` suffix, are published as GitHub *prereleases*
and are never marked latest. The 0.8.0+ betas preview the Brewlet 1.0.0
product line described in this documentation; "legacy 0.x" refers to the
0.7.x and earlier releases from the `brewlet-0.x-java` branch. A plain `X.Y.Z`
version at or above 1.0.0 is a stable release and is marked latest.

## One identity, explicit distribution surfaces

[`release.json`](https://github.com/microsoft/brewlet/blob/main/release.json)
is authoritative. Its version maps unchanged to chart version/appVersion,
Maven version and component image tags; Git tags add `v`. A candidate manifest
records the immutable source commit, production/qualification-input hashes,
payload hashes, image index/child digests and actual build tools. `brewlet version`
reports JSON with version, commit and native platform. Unstamped source builds
report `dev`/`unknown`, not a release.

| Surface | Intended coordinate | Targets |
| --- | --- | --- |
| CLI | GitHub release asset `brewlet_VERSION_OS_ARCH.tar.gz` | Linux/macOS amd64/arm64 |
| Operator | `ghcr.io/microsoft/brewlet-operator:VERSION` | Linux amd64/arm64 |
| Admission | `ghcr.io/microsoft/brewlet-admission:VERSION` | Linux amd64/arm64 |
| Node worker and shim | `ghcr.io/microsoft/brewlet-node-provisioner:VERSION` | Linux amd64/arm64 |
| Helm | `oci://ghcr.io/microsoft/charts/brewlet`, version `VERSION` | Matching CRDs and digest-pinned component images |
| Optional Maven producer | `sh.brewlet:brewlet-maven-plugin:VERSION` | Java 17+; matching Brewlet CLI |

These are intended coordinates, not evidence that a version exists there.
There are no Windows archives, floating GA/latest aliases, or supported Go
library/module API. The `v1alpha1` Kubernetes API does not promise arbitrary
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

The reusable `release-build.yml` workflow builds these outputs, checks
packaging and generates SPDX project SBOMs. Native jobs execute the archived
CLI on all four targets and sign their outcome records; a Maven consumer job
resolves the exact plugin on Java 17 and 21. The build workflow never publishes
a package or executes a node installation.

To dry-run a release without publishing anything, dispatch
`.github/workflows/release.yml` manually (Actions → *Brewlet release* → *Run
workflow*) on the branch to check. A dispatch only runs the identity check and
the candidate build; the publish job runs exclusively for a tag push.

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

Stable releases (1.0.0 and later without a suffix) additionally require a
reviewed `publicationPolicy.legalApproval` record with `approved: true`, an
authoritative `reference`, and `containerPolicy: no-additional-text-required`,
**only if that is the owner's actual determination**. If additional policy
text is required, stable publication remains blocked until authoritative text
is supplied and its packaging is implemented and verified. Do not manufacture
an approval record. Betas do not require this record.

`publicationPolicy.mavenSigningFingerprint` pins the existing Maven Central
signing key (the same key that signed the legacy 0.x plugin releases). The
publish job runs in the `maven-central` GitHub environment, which supplies
`MAVEN_GPG_KEY`, `MAVEN_GPG_PASSPHRASE`, `MAVEN_CENTRAL_USERNAME` and
`MAVEN_CENTRAL_PASSWORD`. No secret values belong in source; the job imports
the key into an ephemeral keyring and refuses a key that does not match the
pinned fingerprint.

## Cutting a release

1. Update the version in `release.json`, `maven-plugin/pom.xml`, the chart
   `version`/`appVersion` and image tags in `values.yaml` (plus documented
   coordinates). `make release-check` fails on any identity drift.
   Optionally add curated notes in `release-notes/VERSION.md`; they are placed
   above the generated changelog. Betas without curated notes get a standard
   "not stable" warning.
2. Merge to `main` and wait for **Brewlet CI** to pass on the merge commit.
3. Tag that exact commit and push the tag:

   ```sh
   git tag v0.8.0 <green-main-commit>
   git push origin v0.8.0
   ```

The `release.yml` workflow then:

- checks that the tag equals `v` + `release.json` version;
- requires a successful **Brewlet CI** push run on `main` for the tagged commit
  (waiting for an in-progress run), so only reviewed, green main commits ship;
- builds and verifies the candidate with `release-build.yml` (tests, legal
  checks, reproducibility, SBOMs, provenance, native CLI and Maven consumer
  smoke tests);
- in the `publish` job, re-verifies the candidate, attestations, tag commit and
  owner policy **before** any credential is used, then signs the Maven bytes,
  transfers images and chart to GHCR, submits the signed bundle to Maven
  Central, publishes the GitHub release with generated notes and independently
  verifies every published surface.

A failed, skipped or cancelled prerequisite is not success; there is no
acceptance opt-out. The workflow never creates or moves tags. Strict promotion
qualification (`promotion-qualification.yml`) and the disposable-node
transition test (`release-transition.yml`) remain separate manual workflows for
owners who want additional evidence before tagging.

Image transfer preserves verified OCI digests; the chart must resolve to the
same package bytes. Central submission signs a bundle of the prebuilt JAR/POM
bytes rather than rebuilding. Betas become GitHub prereleases, never latest.
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
| Same candidate, clean reinstall | The only eligible test pair, exercised by the manual `release-transition.yml`. Not required for beta publication. |
| Legacy 0.7.x and earlier to 0.8.0+ | No supported in-place transition or old-state reuse. Re-author workloads and review a separate migration. |
| Between different betas/prereleases | Unsupported until immutable source/target artifacts, cleanup/recovery limits and genuine evidence are explicitly reviewed. |
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
