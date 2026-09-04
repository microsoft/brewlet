# Kubernetes Security Assessment and Threat Model

**Repository:** `microsoft/brewlet`  
**Revision assessed:** `f6c8a06`  
**Assessment date:** 2026-09-02  
**Remediation status verified:** `92c179a` (2026-09-03)

**Method:** Static, read-only STRIDE review of source code, Kubernetes resources,
Helm templates, container build files, CI/CD workflows, release automation, and
security documentation.

## Executive summary

Brewlet replaces per-image JVMs with node-resident JDKs. A privileged DaemonSet
installs a containerd shim and JDK roots on nodes, and the root-privileged shim
constructs OCI bundles, overlay lower layers, and bind mounts for tenant
workloads.

The review originally identified one Critical, five High, five Medium, and one
Low issue. The Critical finding (1), all five High findings (2, 3, 4, 6, and 7),
Medium findings 9 and 11, and the Low finding (12) have since been remediated
through merged pull requests.

Finding 1 was a confirmed path traversal in OCI descriptor digest resolution:
descriptor digest strings were converted into host filesystem paths without
validating that they were canonical SHA-256 digests, so a namespace tenant able
to publish and run a malicious digest-pinned Brewlet image could cause the root
shim to bind-mount arbitrary host paths, including `/`, read-only inside the
tenant container. Every digest used in path construction is now validated,
every resolved path is independently confined to the content store, and blob
bytes are verified against their declared descriptor digest before they are
read, staged, or mounted.

The remediations bind executed content to protected CRI image identity, confine
content-addressed paths and verify blob contents before use, isolate the
node-shared AppCDS cache, preserve the CRI-selected pod UID/GID, require
explicit digest-pinned JDK and launcher sources, and verify every provisioner
download and external container image.

Several controls are implemented correctly: tar extraction rejects traversal
and ignores link/device entries; JDK Java-home resolution is contained; DSSE
and in-toto verification is complete and fail-closed; dependency bundle entries
and digests are strictly validated; the NodeProfile validating webhook fails
closed; the operator and admission pods are hardened; and containerd
configuration updates are validated and rolled back.

**Overall risk: Medium.** The container-escape-equivalent host read primitives are
closed. The remaining open findings are all Medium: the launcher annotation path
input and the provisioning default that targets every node. Restricting Brewlet
to dedicated, platform-owned node pools further reduces exposure.

## Findings summary

| # | Severity | Finding | Issue | PR | Confidence |
|---|----------|---------|-------|----|------------|
| 1 | Critical | Tenant-controlled OCI digests can bind-mount arbitrary node paths **(Remediated)** | [#19](https://github.com/microsoft/brewlet/issues/19) | [#43](https://github.com/microsoft/brewlet/pull/43) | 9/10 |
| 2 | High | The node-shared AppCDS cache is writable from a tenant container **(Remediated)** | [#20](https://github.com/microsoft/brewlet/issues/20) | [#31](https://github.com/microsoft/brewlet/pull/31) | 8/10 |
| 3 | High | Artifact launch configuration overrides the pod UID/GID **(Remediated)** | [#21](https://github.com/microsoft/brewlet/issues/21) | [#37](https://github.com/microsoft/brewlet/pull/37) | 9/10 |
| 4 | High | Attestation enforcement verifies a different identity from the executed artifact **(Remediated)** | [#22](https://github.com/microsoft/brewlet/issues/22) | [#33](https://github.com/microsoft/brewlet/pull/33) | 8/10 |
| 5 | Medium | The launcher annotation can traverse outside the launcher root | [#23](https://github.com/microsoft/brewlet/issues/23) | — | 7/10 |
| 6 | High | Mutable, unsigned JDK images become root-executed node runtimes **(Remediated)** | [#24](https://github.com/microsoft/brewlet/issues/24) | [#34](https://github.com/microsoft/brewlet/pull/34) | 9/10 |
| 7 | High | Unverified downloaded binaries are installed on every node **(Remediated)** | [#25](https://github.com/microsoft/brewlet/issues/25) | [#32](https://github.com/microsoft/brewlet/pull/32) | 9/10 |
| 8 | Medium | `mainJar` can escape staging and select arbitrary host paths **(Remediated)** | [#26](https://github.com/microsoft/brewlet/issues/26) | [#41](https://github.com/microsoft/brewlet/pull/41) | 8/10 |
| 9 | Medium | Registry credentials can be forwarded cross-origin or over HTTP **(Remediated)** | [#27](https://github.com/microsoft/brewlet/issues/27) | [#61](https://github.com/microsoft/brewlet/pull/61) | 8/10 |
| 10 | Medium | The privileged provisioner defaults to every node | [#28](https://github.com/microsoft/brewlet/issues/28) | — | 9/10 |
| 11 | Medium | Mutable GitHub Actions and absent provenance weaken release integrity **(Remediated)** | [#29](https://github.com/microsoft/brewlet/issues/29) | [#42](https://github.com/microsoft/brewlet/pull/42) | 9/10 |
| 12 | Low | Long-lived webhook credentials and absent NetworkPolicies reduce defense in depth **(Remediated)** | [#30](https://github.com/microsoft/brewlet/issues/30) | [#39](https://github.com/microsoft/brewlet/pull/39) | 9/10 |

## Threat model

### Assets requiring protection

1. Node root filesystems and the integrity of `/opt/brewlet`,
   `/etc/containerd`, and `/usr/local/bin`.
2. Kubelet client credentials and configuration under `/var/lib/kubelet`.
3. Secrets, projected service-account tokens, and persistent data mounted into
   every workload on a Brewlet-enabled node.
4. Node JDK and launcher roots used as shared OCI rootfs lower layers.
5. The node-shared AppCDS cache under `/opt/brewlet/cds`.
6. The integrity and identity of executed application artifacts.
7. The containerd socket, runtime configuration, and Brewlet shim binary.
8. Operator, admission, and provisioner service-account privileges.
9. Admission webhook private keys and attestation trust anchors.
10. Release artifacts, Helm charts, container images, Maven artifacts, and
    developer registry credentials.

### Actors and attacker capabilities

| Actor | Relevant capability |
|-------|---------------------|
| Namespace tenant | Can create a Pod or Deployment in one namespace and control its image, annotations, RuntimeClass, and security context |
| Artifact publisher | Controls an OCI manifest, config blob, descriptors, and application launch metadata |
| Cluster or GitOps administrator | Controls NodeProfiles, Helm values, registry mirrors, and component image references |
| Upstream registry or vendor | Controls runtime image content and downloadable tool release assets |
| CI/CD supply-chain attacker | Can compromise or retag a referenced GitHub Action or upstream build dependency |
| Malicious registry | Can issue authentication challenges and return attacker-controlled manifests, blobs, and headers |
| Compromised co-tenant workload | Has code execution inside a Brewlet sandbox on the same node as a victim |

The primary attacker for tenant isolation findings requires only namespaced
workload creation, not cluster-scoped RBAC or direct node access.

### Entry points

- Pod `runtimeClassName`, `brewlet.sh/*` annotations, image references, and
  security contexts.
- `JavaApplication` and `NodeProfile` custom resources.
- OCI manifest/config JSON, layer descriptors, dependency bundles, and launch
  configuration.
- Node labels and annotations used for scheduling and fleet compatibility.
- Helm values and rendered RBAC, webhook, RuntimeClass, and DaemonSet resources.
- Registry authentication challenges, token endpoints, manifests, and blobs.
- Provisioner image downloads, JDK and launcher source images, GitHub Actions, and
  release workflows.

### Trust boundaries

1. Tenant pod metadata to the root containerd shim.
2. Untrusted OCI metadata and content to shim path and mount construction.
3. Container sandbox to node filesystems through overlay and bind mounts.
4. One tenant workload to another through node-shared JDK and AppCDS state.
5. Kubernetes API objects to privileged host mutation by the provisioner.
6. Public registries and internet downloads to root-executed host software.
7. Developer credentials to third-party registry authentication endpoints.
8. Third-party GitHub Actions to published release artifacts and packages.

### Privileged components

- `containerd-shim-brewlet-v2` runs as root on the node and constructs the
  workload OCI spec, mounts, and overlay.
- The node provisioner is privileged, uses `hostPID`, enters host namespaces,
  mounts the containerd socket and host directories, and tolerates every taint.
- The operator can create and update DaemonSets cluster-wide and patch Nodes.
- The provisioner service account can patch and update Nodes cluster-wide.
- The admission webhook can mutate every Pod on create, although its pod itself
  is correctly hardened.

### Critical data and control flow

1. A tenant publishes and requests a digest-pinned Brewlet image containing
   attacker-controlled OCI descriptors.
2. Containerd preserves the protected CRI requested-image and resolved-config
   identities.
3. The root shim validates the requested target, image-name annotation, config
   digest, and selected platform-manifest digest.
4. The shim reads the verified manifest from the containerd content store, then
   resolves its config and layer descriptor digest strings into host paths.
5. Config and layer descriptor digests are not validated before
   `contentBlobPath` and `Store.BlobPath` construct those paths.
6. These paths become bind-mount sources or overlay lower layers in the tenant
   container.

Artifact selection is now bound to protected CRI image identity, but the
selected image still crosses the tenant-to-root trust boundary without
validating every descriptor digest at its path construction site.

## Detailed findings

### 1. Tenant-controlled OCI digests can bind-mount arbitrary node paths — remediated

**Severity:** Critical  
**Confidence:** 9/10  
**STRIDE:** Information Disclosure, Elevation of Privilege  
**CWE:** CWE-22, CWE-20, CWE-345  
**Type:** Confirmed vulnerability

**Status:** Remediated by [issue #19](https://github.com/microsoft/brewlet/issues/19)
via [pull request #43](https://github.com/microsoft/brewlet/pull/43). The
artifact-identity portion had previously been remediated by
[issue #22](https://github.com/microsoft/brewlet/issues/22) via
[pull request #33](https://github.com/microsoft/brewlet/pull/33), which bound
execution to the protected CRI image identity but did not constrain the
descriptors *inside* the manifest.

**Attacker prerequisites:** Permission to create a Pod in one namespace and
ability to publish an image to a registry the node can pull from.

**Original evidence (before remediation):**

- `core/shim/cmd/containerd-shim-brewlet-v2/service_linux.go:351-380` derives
  the executable manifest from protected CRI image identity rather than the
  tenant artifact-digest annotation.
- `core/shim/cmd/containerd-shim-brewlet-v2/image_identity_linux.go:73-115`
  validates the requested target, resolved config, and platform manifest as
  canonical SHA-256 digests.
- `core/internal/artifact/artifact.go:346-365` still represented config and layer
  descriptor digests as unrestricted strings.
- `core/shim/cmd/containerd-shim-brewlet-v2/resolver.go:94-102` split an
  arbitrary descriptor digest and passed its components to `filepath.Join`.
- `core/internal/artifact/artifact.go:401-408` constructed local-layout blob
  paths without validating the digest.
- `core/internal/artifact/blobs.go:126-169` resolved config and layer
  descriptors through those path functions without validating their syntax or
  verifying their bytes against the declared digest.
- `core/shim/cmd/containerd-shim-brewlet-v2/service_linux.go:659-692`
  bind-mounts the resolved JAR path into the container.

**Attack path and impact:**

1. The attacker publishes content containing a Brewlet manifest whose JAR
   descriptor digest is a traversal value such as
   `sha256:../../../../../..`.
2. The attacker requests that digest-pinned image in a Brewlet pod, and
   containerd pulls it onto the target node.
3. The shim correctly binds execution to that requested image and reads its
   manifest.
4. `filepath.Join` cleans the malicious layer-descriptor traversal and resolves
   the supposed content-store blob path to `/`.
5. `os.Stat("/")` succeeds and the root shim bind-mounts the host root
   filesystem read-only into the attacker container.
6. The workload can read kubelet credentials and mounted Secrets and tokens for
   every co-located pod. On a control-plane node it may also reach control-plane
   PKI and etcd data.

This is a container-escape-equivalent host read primitive and a realistic path
to cluster compromise.

**Remediation applied:**

- `core/internal/artifact/digest.go` is the single choke point for digest
  handling: `ValidateDigest` enforces `^sha256:[0-9a-f]{64}$`, and `BlobPathIn`
  independently re-checks with `filepath.Rel`/`filepath.IsLocal` that the
  constructed path remains under `<root>/blobs/sha256`.
- `BlobSource.BlobPath` returns `(string, error)`, so no caller can obtain a
  path for an invalid digest. `Store.BlobPath`, `contentStoreSource`, and
  `contentBlobPath` all route through `BlobPathIn`.
- `ReadVerifiedBlob` replaces the separate manifest and store read paths and
  covers the native config, `Store.Resolve`, and dependency-bundle reads.
  Runnable-image layers are read through it as well.
- Natively bind-mounted JAR, classpath, modulepath, and CDS blobs are verified
  with a streaming SHA-256 (`verifiedBlobPath`) instead of a bare `os.Stat`
  presence check. Descriptor `Size` remains advisory, but the hash is always
  verified, so an absent size cannot disable verification.
- `runnableStageDir` derives from a validated digest hex rather than an
  unchecked `strings.Cut` fallback.

**Remediation test:**

- `core/internal/artifact/digest_test.go` covers traversal, missing and extra
  hex digits, uppercase hex, unsupported algorithms, missing prefix, embedded
  separators, and empty components, plus path-containment assertions.
- `FuzzBlobPathInStaysUnderContentStore` asserts every resolved content path
  remains under the content-store root.
- `core/shim/cmd/containerd-shim-brewlet-v2/resolver_traversal_test.go`
  reproduces the attack with a self-consistent manifest carrying hostile
  payload descriptors and asserts no host path is returned.
- Tier 3b of the e2e harness (`integration-tests/e2e/tier3-runc.sh`) proves a
  traversal descriptor and a mismatched blob both fail bundle creation and
  produce no mount.

### 2. The node-shared AppCDS cache is writable from a tenant container — remediated

**Severity:** High  
**Confidence:** 8/10  
**STRIDE:** Tampering, Elevation of Privilege  
**CWE:** CWE-732, CWE-269, CWE-349  
**Type:** Confirmed architectural vulnerability

**Status:** Remediated by [issue #20](https://github.com/microsoft/brewlet/issues/20)
via [pull request #31](https://github.com/microsoft/brewlet/pull/31)

**Attacker prerequisites:** Permission to create a Brewlet Pod. Root inside the
sandbox increases reliability and is available through Finding 3 or a workload
that does not request non-root execution.

**Original evidence (before remediation):**

- `core/internal/runtime/cds_regen.go:28-35` defines the node-global
  `/opt/brewlet/cds` cache.
- `core/shim/cmd/containerd-shim-brewlet-v2/service_linux.go:470-476` derives
  the cache key from tenant-controlled artifact annotations.
- `core/internal/runtime/cds_regen.go:193,206-231` elects a writer through a
  best-effort marker file.
- `core/shim/cmd/containerd-shim-brewlet-v2/service_linux.go:496-510` mounts the
  entire cache directory read-write for the elected container.
- `core/internal/runtime/cds_regen.go:159-168` configures victim JVMs to consume
  the shared archive by key.

**Attack path and impact:**

1. An attacker requests CDS regeneration and wins the writer election.
2. The whole cache, including other tenants' entries, is mounted read-write.
3. The attacker overwrites or deletes a victim's archive, using a matching
   artifact key when necessary.
4. A victim JVM on the same node maps the poisoned archive.

The immediate impact is cross-tenant cache corruption and denial of service.
A valid malicious AppCDS archive may inject class metadata into the victim JVM,
yielding code execution with the victim workload identity.

**Implemented remediation:**

- Cache identity is the SHA-256 of the trusted CRI sandbox namespace, the
  content-verified resolved platform-manifest digest, the exact JDK build, and
  the trusted CRI process UID. Tenant artifact annotations are not used as cache
  identity, and workloads with different UIDs never share an owner-private
  entry.
- Each entry is `<cache>/<key>/archive.jsa`; only `<cache>/<key>` is mounted at
  `/run/brewlet/cds`. The node-global cache root and external `<key>.writer`
  election marker are never exposed to the workload.
- Writers receive only their UID/GID-owned private directory read-write;
  consumers receive it read-only. Both mounts add `nosuid,nodev,noexec`.
- Brewlet rejects symlink/non-regular archives, recreates an elected writer
  entry before host-side seeding, and never consumes legacy flat cache entries.
- Kubernetes regeneration is default-deny through
  `NodeProfile.spec.appCDS.regenerationEnabled`. Admission denies and steers
  requests early, while the shim authoritatively requires the root-owned host
  policy sentinel even when the webhook fails open.

**Remediation test:** Tier 8 runs victim and attacker writers in separate
namespaces on the same node, asserts distinct private bundle mount sources,
proves the attacker cannot enumerate or address the victim entry, tampers with
the attacker archive, and verifies the victim bytes and mapped consumer remain
unchanged.

### 3. Artifact launch configuration overrides the pod UID/GID — remediated

**Severity:** High  
**Confidence:** 9/10  
**STRIDE:** Elevation of Privilege  
**CWE:** CWE-250, CWE-863  
**Type:** Confirmed vulnerability and insecure default

**Status:** Remediated by [issue #21](https://github.com/microsoft/brewlet/issues/21)
via [pull request #37](https://github.com/microsoft/brewlet/pull/37)

**Attacker prerequisites:** Control of the executed artifact's config blob.

**Original evidence at the assessed revision:**

- `core/shim/cmd/containerd-shim-brewlet-v2/service_linux.go:517-521`
  unconditionally overwrites `spec.Process.User.UID` and `.GID` from artifact
  configuration after CRI has applied the pod security context.
- `core/internal/artifact/artifact.go:203-206,216-318` defines the values but
  does not reject root, negative, or out-of-range IDs.
- `specs/SPECIFICATION.md:1228-1229` claims non-root execution unless the pod
  security context explicitly requests root.

**Attack path and impact:** An artifact requests UID/GID 0 while the Pod uses
`runAsNonRoot: true` and `runAsUser: 1000`. Admission and kubelet checks see a
compliant Pod, but the shim changes the final OCI process user to root. This
defeats Pod Security expectations and makes writable volumes and the AppCDS
cache available with root privileges.

**Remediation:** Treat the CRI-populated user as authoritative. Artifact
configuration must never raise privilege or replace a non-zero Pod UID/GID with
zero. Reject negative and out-of-range values and fail launch when the artifact
requests a prohibited identity.

**Remediation test:** Unit test a spec with UID 1000 and artifact UID 0 and
assert the final UID remains 1000 or launch fails. Add an end-to-end test in a
Pod Security `restricted` namespace.

**Resolution:** Artifact launch config no longer has a `user` field and strict
decoding rejects any artifact that supplies one. The production shim preserves
the CRI-populated OCI process user unchanged, generated `JavaApplication`
workloads and standalone bundles default to `65532:65532`, and the live
containerd E2E tier covers both UID/GID preservation and fail-closed rejection
of a re-hashed root-requesting artifact in a Pod Security `restricted`
namespace.

### 4. Attestation enforcement verifies a different identity from the executed artifact — remediated

**Severity:** High  
**Confidence:** 8/10  
**STRIDE:** Spoofing, Tampering  
**CWE:** CWE-345, CWE-807  
**Type:** Confirmed admission bypass

**Status:** Remediated by [issue #22](https://github.com/microsoft/brewlet/issues/22)
via [pull request #33](https://github.com/microsoft/brewlet/pull/33)

**Attacker prerequisites:** Permission to create a Brewlet Pod in a cluster
using the documented Ratify/Gatekeeper policy.

**Original evidence at the assessed revision:**

- `admission/deploy/40-gatekeeper-constrainttemplate.yaml:38-53` submits
  `spec.containers[].image` to Ratify.
- `admission/ratify-verifier/verify.go:109-112` and
  `core/pkg/attest/attest.go:260-262` bind the attestation to the subject image
  digest.
- `core/shim/cmd/containerd-shim-brewlet-v2/service_linux.go:302` instead
  executes content selected by `brewlet.sh/artifact-digest`.
- `core/shim/cmd/containerd-shim-brewlet-v2/resolver.go:46-66` does not
  cross-check the annotation against the container image identity.

**Attack path and impact:** The attacker uses a legitimately attested image in
the Pod to satisfy Gatekeeper but points `brewlet.sh/artifact-digest` at
different, unattested content already seeded in the node content store. The
shim executes the unattested artifact while admission records success. The
documented final-image guarantee therefore does not apply to the code that
runs.

**Remediation:**

- Derive artifact identity from the CRI-resolved image digest in the shim.
- Reject any annotation that disagrees with that identity.
- Have the webhook overwrite, rather than fill, artifact identity annotations.
- Fail closed for Brewlet RuntimeClass pods.
- Consider on-node signature verification by the shim.

**Remediation test:** With Ratify/Gatekeeper enabled, submit an attested image
and a mismatched artifact digest and assert the Pod is rejected or the runtime
refuses to create the container.

**Resolution:** The webhook overwrites image-derived compatibility hints, while
the shim independently requires containerd's protected CRI requested-image
metadata to contain a digest-pinned reference. It resolves that exact target
directly from the content store, requires
`io.kubernetes.cri.image-name` to name the same digest, and verifies the
selected platform manifest's config digest against CRI's recorded image
identity. It never selects executable content through a mutable config-digest
image alias. Conflicting hints and tag-only requests fail closed.

### 5. The launcher annotation can traverse outside the launcher root

**Severity:** Medium  
**Confidence:** 7/10  
**STRIDE:** Information Disclosure  
**CWE:** CWE-22  
**Type:** Confirmed traversal with constrained exploitation

**Attacker prerequisites:** Permission to create a Brewlet Pod.

**Evidence:**

- `core/shim/cmd/containerd-shim-brewlet-v2/bundle_prepare.go:320-336` joins the
  raw launcher annotation to the launcher roots directory.
- `core/internal/artifact/artifact.go:160-165` performs no sanitization.
- `core/shim/cmd/containerd-shim-brewlet-v2/service_linux.go:295,429-435,
  530-537` uses the selected directory as an overlay lower layer and bind-mount
  source.
- Unlike JDK selection, no `.brewlet-active` inventory allowlist is applied.

**Attack path and impact:** A value such as `../../..` resolves the selected
launcher root to `/`, and the existing guard can succeed. The same raw string
is also used as `argv[0]`, which currently prevents a demonstrated runnable
container for the traversal payload. The host directory is nevertheless
accepted into privileged mount construction and could become directly
exploitable after minor launcher-path changes.

**Remediation:** Restrict launcher names to a safe DNS-like token, reject path
separators and dot segments, apply the active launcher inventory allowlist, and
verify with `filepath.Rel` that the resolved root remains under the configured
launcher directory.

**Remediation test:** Table-test traversal and separator variants and assert
they fail before mount construction.

### 6. Mutable, unsigned JDK images become root-executed node runtimes — remediated

**Severity:** High  
**Confidence:** 9/10  
**STRIDE:** Tampering, Elevation of Privilege  
**CWE:** CWE-494, CWE-1357  
**Type:** Architectural risk and insecure default

**Status:** Remediated by [issue #24](https://github.com/microsoft/brewlet/issues/24)
via [pull request #34](https://github.com/microsoft/brewlet/pull/34)

**Attacker prerequisites:** Compromise of an upstream mutable JDK tag, or
control of a NodeProfile registry mirror through cluster/GitOps compromise.

**Original evidence at the assessed revision:**

- `provisioner/entrypoint.sh:344-369` selects mutable tags such as
  `docker.io/library/eclipse-temurin:<feature>` and
  `mcr.microsoft.com/openjdk/jdk:<feature>-ubuntu`.
- `provisioner/entrypoint.sh:376-397` copies the complete image rootfs onto the
  host.
- `provisioner/entrypoint.sh:280-305` executes the copied Java binary through
  `chroot` as root.
- `provisioner/entrypoint.sh:420-430` runs a mutable launcher image with host
  networking and a writable host bind mount.
- `kubernetes/internal/controller/nodeprofile_resources.go:155-170` passes
  registry mirrors to the provisioner.
- `kubernetes/internal/controller/nodeprofile_validate.go:24-100` does not
  validate registry mirrors and prevents implicitly mapped distributions from
  supplying a custom pinned source.
- `core/shim/cmd/containerd-shim-brewlet-v2/service_linux.go:429` uses the
  resulting JDK tree as a workload rootfs lower layer.

**Attack path and impact:** An upstream tag or unvalidated mirror serves a
malicious image. The provisioner copies it to the node and root-executes its
Java binary during validation. The malicious tree then underlies every Brewlet
workload on that node. This provides root execution and persistent compromise
across the provisioned fleet.

**Remediation:** Remove implicit distribution mappings and require every JDK and
launcher to declare an administrator-reviewed, digest-pinned source and absolute
path. Validate and allowlist mirror hosts, and mount/copy launcher images without
host networking or a writable host bind mount.

**Remediation test:** Reject every JDK or launcher without a canonical
`@sha256:` source and absolute path. Reject mirror values containing schemes,
whitespace, empty hosts, or unapproved destinations. Verify invalid input fails
closed before any host operation or node readiness label.

**Resolution:** `NodeProfile` now requires an explicit structured source for
every JDK and launcher. Admission and reconciliation require canonical
digest-pinned OCI references and clean absolute source paths, and registry
mirror destinations must match an operator-controlled exact-host allowlist.
The provisioner validates the complete inventory before host mutation, mounts
digest-specific source images, copies JDK roots, and installs launcher regular
files without executing source images with host networking or writable host
bind mounts.

### 7. Unverified downloaded binaries are installed on every node — remediated

**Severity:** High  
**Confidence:** 9/10  
**STRIDE:** Tampering  
**CWE:** CWE-494  
**Type:** Confirmed supply-chain vulnerability

**Status:** Remediated by [issue #25](https://github.com/microsoft/brewlet/issues/25)
via [pull request #32](https://github.com/microsoft/brewlet/pull/32)

**Attacker prerequisites:** Compromise of, or an on-path position to, the
upstream release assets used during the provisioner image build.

**Original evidence at the assessed revision:**

- `provisioner/Dockerfile:55-72` downloads `kubectl`, `ctr`, and `crictl`
  without checksum or signature verification.
- `provisioner/Dockerfile:41-43,80-90` uses source commit arguments only for
  license text, not binary integrity.
- The Dockerfile base images are referenced by mutable tags.
- `provisioner/entrypoint.sh:258-264` installs downloaded `ctr` and `crictl`
  binaries into host `/usr/local/bin`.
- `provisioner/entrypoint.sh:233-244` executes them in host namespaces against
  the containerd socket.

**Attack path and impact:** A tampered release archive is incorporated into the
provisioner image and copied as a root executable to every node. A malicious
`ctr` then has direct access to the host containerd socket.

**Remediation:** Pin published SHA-256 values and verify before extraction or
installation, digest-pin base images, and prefer trusted multi-stage copies
where practical.

**Remediation test:** Corrupt each downloaded artifact during a build test and
assert the build fails. Add a CI policy that requires checksum verification for
Dockerfile downloads and digest pins for `FROM`.

**Resolution:** The provisioner now stores architecture-specific SHA-256
manifests for `kubectl`, `containerd`, and `crictl`, plus checksums for their
license files. A dedicated HTTPS-only download helper verifies each asset during
download, and the Dockerfile re-verifies all six files immediately before
extraction. Every external `FROM` image is pinned by full SHA-256 digest, the
runtime image receives only verified outputs and no longer contains `curl`, and
repository policy tests reject unpinned images or direct Dockerfile downloads.
Build corruption tests cover every downloaded binary and license asset.

### 8. `mainJar` can escape staging and select arbitrary host paths — remediated

**Severity:** Medium  
**Confidence:** 8/10  
**STRIDE:** Information Disclosure  
**CWE:** CWE-22  
**Type:** Confirmed vulnerability

**Status:** Remediated by [issue #26](https://github.com/microsoft/brewlet/issues/26)
via [pull request #41](https://github.com/microsoft/brewlet/pull/41)

**Attacker prerequisites:** Control of the runnable image's
`brewlet.sh/jvm-config` metadata.

**Original evidence at the assessed revision:**

- `core/internal/artifact/blobs.go:163-178` joins `cfg.MainJar` directly to the
  application staging directory.
- `core/internal/artifact/artifact.go:216-318` validates `cds.archive` as a
  bare filename but does not apply the same rule to `MainJar`.
- `core/shim/cmd/containerd-shim-brewlet-v2/service_linux.go:549,565` uses the
  resulting path as a bind-mount source.

**Attack path and impact:** A `mainJar` value containing `../` or an absolute
path escapes the staging directory. The root shim then bind-mounts the selected
host file or directory inside the tenant container, providing a second
host-file disclosure path independent of descriptor digest traversal.

**Remediation:** Validate `MainJar` as a bare filename with no separators,
wildcards, or dot segments, and enforce staging-directory containment with
`filepath.Rel` before returning resolved paths.

**Remediation test:** Reject `../x.jar`, `/etc/passwd`, `a/b.jar`, and `..`;
assert all successfully resolved JAR and CDS paths remain under staging.

**Resolution:** `JVMConfig.Validate` now rejects a `mainJar` carrying a path
separator, wildcard, parent reference, or surrounding whitespace, and the
pre-existing `cds.archive` check shares the same validator so the two rules
cannot drift. The rule applies at publish time, whenever a launch config is
decoded — including from a runnable image's `brewlet.sh/jvm-config` annotation —
and at launch. Runnable-blob resolution additionally confirms with `filepath.Rel`
that every JAR and CDS path it returns is contained by the per-image staging
directory, and the shim plus the runtime staging and bundle helpers re-check the
filename before using it as a bind-mount source or destination, so the root mount
fails closed rather than following image-chosen metadata. The Maven plugin
applies the identical rule at publish time. Tests reject `../x.jar`,
`../../etc/passwd`, `/etc/passwd`, `a/b.jar`, `..`, `.`, wildcards, and padded
names through config validation, through image resolution, and at the shim, and
assert that successfully resolved JAR and CDS paths remain under staging.

### 9. Registry credentials can be forwarded cross-origin or over HTTP — remediated

**Severity:** Medium  
**Confidence:** 8/10  
**STRIDE:** Information Disclosure, Spoofing  
**CWE:** CWE-522, CWE-918, CWE-20  
**Type:** Confirmed vulnerability

**Status:** Remediated by [issue #27](https://github.com/microsoft/brewlet/issues/27)
via [pull request #61](https://github.com/microsoft/brewlet/pull/61)

**Attacker prerequisites:** A malicious or compromised registry to which a
developer or CI job authenticates.

**Original evidence at the assessed revision:**

- `maven-plugin/src/main/java/sh/brewlet/maven/plugin/oci/RegistryClient.java:
  769-797` accepts an authentication challenge `realm` without validating its
  host or scheme and attaches registry Basic credentials to the token request.
- `RegistryClient.java:497-510` already contains same-origin enforcement for
  pagination links but does not reuse it for authentication.
- `RegistryClient.java:753-763,813-820` attaches credentials while selecting
  plaintext HTTP through a prefix check for registry strings beginning with
  `localhost` or `127.`.

**Attack path and impact:** A malicious registry responds with a token realm on
an attacker-controlled origin. The Maven client sends the developer's registry
username and password to that origin. Hostnames such as
`localhost.attacker.example` can also trigger plaintext HTTP and expose Basic
credentials on the network. Stolen push credentials can lead to artifact
tampering.

**Remediation:** Require HTTPS and same-origin token realms unless explicitly
allowlisted, never attach Basic credentials cross-origin, use exact loopback
host matching, and require an explicit insecure-registry option before sending
credentials over HTTP.

**Remediation test:** Simulate cross-origin and HTTP authentication challenges
and assert no request containing `Authorization` is sent. Test exact behavior
for `localhost`, loopback addresses, ports, and prefix-lookalike public names.

**Resolution:** Every transport and credential-forwarding decision now runs
through a single `RegistryTrustPolicy`. Plaintext HTTP is selected only for
exact loopback authorities (`localhost`, an IPv4 literal in `127.0.0.0/8`,
`::1`, with or without a port) or an authority listed in the new
`insecureRegistries` option, so `localhost.attacker.example`, `127.example.com`,
and `notlocalhost` stay on HTTPS. `authedRequest` attaches the bearer token or
Basic credentials only when the request target is same-origin with the
configured registry, which covers registry-controlled URLs such as the
blob-upload `Location`; those URLs are still followed, but uncredentialed, and a
plaintext upload location for a non-insecure authority is rejected outright. A
`Bearer` challenge realm is validated before it is contacted at all — absolute
`http`/`https` only, no embedded userinfo, and HTTPS unless the realm authority
is itself insecure-eligible — and credentials are exchanged only with a
same-origin realm, the built-in Docker Hub mapping
(`docker.io`/`index.docker.io`/`registry-1.docker.io`/`registry.hub.docker.com`
→ `auth.docker.io`), or an authority named in the new `allowedTokenRealms`
option; any other realm fails the build with an actionable message instead of
forwarding credentials. Anonymous flows still complete, having nothing to leak.
The registry authority itself must be a bare `host[:port]`, so a value such as
`attacker.example@registry.example.com` can no longer retarget requests, and the
pre-existing pagination same-origin check now shares the policy's comparison.
Tests drive live HTTP servers to assert that an untrusted cross-origin realm
receives no request at all, that a same-origin realm exchange succeeds and reuses
the issued token, that an allowlisted cross-origin realm receives credentials,
that an anonymous cross-origin exchange sends none, and that a cross-origin
upload `Location` receives the PUT without `Authorization`; unit tests cover
`localhost`, loopback literals, bracketed IPv6, explicit ports, lookalike public
names, Docker Hub realm lookalikes, and malformed configuration entries.

### 10. The privileged provisioner defaults to every node

**Severity:** Medium  
**Confidence:** 9/10  
**STRIDE:** Elevation of Privilege  
**CWE:** CWE-250, CWE-732  
**Type:** Insecure default and architectural risk

**Attacker prerequisites:** Cluster/GitOps administrator access or compromise
of the operator service account.

**Evidence:**

- `kubernetes/internal/controller/nodeprofile_resources.go:218-265` creates a
  privileged, `hostPID` DaemonSet with an `Exists` toleration for every taint
  and host mounts for `/opt/brewlet`, `/etc/containerd`, `/usr/local/bin`, and
  `/run/containerd/containerd.sock`.
- `kubernetes/charts/brewlet/values.yaml:48-50` enables the default NodeProfile.
- `kubernetes/internal/controller/nodeprofile_resources.go:53-55,79-101`
  selects every node when the default profile has no named pools.
- `kubernetes/charts/brewlet/templates/operator.yaml:32-34` grants cluster-wide
  DaemonSet creation and update.

**Attack path and impact:** A default Helm installation schedules the
provisioner on worker and control-plane nodes because the blanket toleration
defeats control-plane taints. The pod can mutate containerd, write host
executables, and control the runtime socket. Operator compromise therefore
becomes root compromise of any node, including the control plane.

**Remediation:** Default the profile to disabled or require explicit node-pool
selection, remove the blanket toleration, exclude control-plane nodes, minimize
hostPath write access, and document the blast radius.

**Remediation test:** Render the chart and assert there is no blanket toleration
or default control-plane placement. In a multi-node test cluster, verify no
provisioner lands on the control-plane node under default values.

### 11. Mutable GitHub Actions and absent provenance weaken release integrity — remediated

**Severity:** Medium  
**Confidence:** 9/10  
**STRIDE:** Tampering  
**CWE:** CWE-1357, CWE-829  
**Type:** Supply-chain architectural risk

**Status:** Remediated by [issue #29](https://github.com/microsoft/brewlet/issues/29)
and [pull request #42](https://github.com/microsoft/brewlet/pull/42)

**Attacker prerequisites:** Compromise of an action repository or ability to
move a referenced action tag.

**Original evidence at the assessed revision:**

- `.github/workflows/release.yml:47,53,207,216` and the other workflows use
  action version tags instead of full commit SHAs.
- `.github/workflows/release.yml:17-19` grants `contents: write` and
  `packages: write` at workflow scope.
- No workflow produces cosign signatures, attestations, or SLSA provenance.
- `.github/workflows/release.yml:211-213` checksums CLI archives but not Maven
  artifacts.
- `.github/workflows/ci.yml:82` and `kubernetes/Makefile` install
  `setup-envtest` from a mutable release branch.
- Helm image helpers publish version-tag references with
  `IfNotPresent`, not recorded immutable digests.

**Attack path and impact:** A moved action tag executes attacker code in a
release job with repository and package write permissions. The attacker can
replace GitHub Releases and GHCR images, while consumers have no independent
signature or provenance to detect the replacement.

**Remediation:** Pin every action to a full commit SHA, scope write permissions
to the jobs that need them, enable automated action updates, sign images and
release artifacts, publish build provenance, checksum Maven artifacts, and pin
`setup-envtest`.

**Remediation test:** Add a workflow lint rule rejecting non-SHA `uses:`
references and release smoke tests that verify signatures and attestations for
newly published artifacts.

**Resolution:** Every `uses:` reference across all five workflows is now pinned
to a full commit SHA with a trailing version comment, and `setup-envtest` is
installed from an immutable module pseudo-version instead of the mutable
`release-0.19` branch. The release workflow declares `contents: read` at workflow
scope and grants `contents`, `packages`, `id-token`, and `attestations` writes
only to the jobs that publish, so build-only jobs can no longer replace a release
or a package.

`actions/attest-build-provenance` now publishes SLSA v1 build provenance for each
component image, the OCI Helm chart, and every GitHub Release asset; image and
chart attestations are pushed to GHCR as OCI referrers so they can be verified
from the registry alone. Released charts record the immutable digest of each
image they were built against, so an install resolves
`ghcr.io/microsoft/brewlet-*@sha256:…` rather than a tag that can be repointed,
and packaging fails if a rendered chart is not digest-pinned. Release checksums
now cover the CLI archives and the Maven jar and pom by explicit pattern and fail
closed when any component is missing.

Both remediation tests are automated. `scripts/check-workflow-security.sh` (with
its own self-tests in `scripts/check-workflow-security_test.sh`, wired into
`make check` and CI) rejects any non-SHA `uses:` reference and any workflow-scope
write permission. `scripts/verify-release-provenance.sh` verifies every published
image, the chart, and every release asset against the release workflow's signer
identity; the release workflow runs it against the version it just published, and
the website release smoke test runs it for every release at or after `0.4.0`.
Dependabot keeps the SHA pins current.

- `.github/workflows/release.yml:47,53,207,216` and the other workflows use
  action version tags instead of full commit SHAs.
- `.github/workflows/release.yml:17-19` grants `contents: write` and
  `packages: write` at workflow scope.
- No workflow produces cosign signatures, attestations, or SLSA provenance.
- `.github/workflows/release.yml:211-213` checksums CLI archives but not Maven
  artifacts.
- `.github/workflows/ci.yml:82` and `kubernetes/Makefile` install
  `setup-envtest` from a mutable release branch.
- Helm image helpers publish version-tag references with
  `IfNotPresent`, not recorded immutable digests.

**Attack path and impact:** A moved action tag executes attacker code in a
release job with repository and package write permissions. The attacker can
replace GitHub Releases and GHCR images, while consumers have no independent
signature or provenance to detect the replacement.

**Remediation:** Pin every action to a full commit SHA, scope write permissions
to the jobs that need them, enable automated action updates, sign images and
release artifacts, publish build provenance, checksum Maven artifacts, and pin
`setup-envtest`.

**Remediation test:** Add a workflow lint rule rejecting non-SHA `uses:`
references and release smoke tests that verify signatures and attestations for
newly published artifacts.

### 12. Long-lived webhook credentials and absent NetworkPolicies — remediated

**Severity:** Low  
**Confidence:** 9/10  
**STRIDE:** Spoofing, Information Disclosure  
**CWE:** CWE-1188, CWE-295  
**Type:** Defense-in-depth recommendation

**Status:** Remediated by [issue #30](https://github.com/microsoft/brewlet/issues/30)
via [pull request #39](https://github.com/microsoft/brewlet/pull/39)

**Attacker prerequisites:** Permission to read the webhook Secret or Helm
release data, or pod-network access to exposed component endpoints.

**Original evidence at the assessed revision:**

- `kubernetes/charts/brewlet/templates/admission.yaml:8-9` creates a ten-year
  CA and leaf certificate.
- `kubernetes/charts/brewlet/templates/admission.yaml:58-68,169,196` stores the
  key in a Secret and embeds the CA in webhook configurations.
- The chart has no NetworkPolicy resources.
- `kubernetes/charts/brewlet/templates/metrics.yaml` exposes unauthenticated
  metrics on the cluster pod network.

**Attack path and impact:** Theft of the long-lived key permits webhook
impersonation for years unless administrators rotate it manually. Any pod can
reach metrics or component endpoints in clusters without external network
policy, disclosing runtime and node inventory.

**Remediation:** Use shorter-lived certificates and automated rotation, prefer
cert-manager, and ship optional NetworkPolicies that restrict webhook and
metrics ingress.

**Remediation test:** Validate certificate lifetime in rendered manifests and
verify an unrelated namespace cannot reach protected endpoints when policies
are enabled.

**Resolution:** The dependency-free default now issues fresh self-signed
webhook credentials on Helm install or upgrade with a configurable 90-day
validity. Clusters can opt into cert-manager-managed issuance, renewal, CA
injection, and certificate hot reload. Optional ingress NetworkPolicies restrict
the admission webhook, operator metrics, and node metrics to explicitly
configured API-server, kubelet, and scraper peers, and fail rendering when
required trusted sources are absent. In-cluster E2E coverage validates
cert-manager issuance and reload as well as attributable denial of unauthorized
metrics access.

## Correctly implemented controls

- Tar extraction rejects non-local paths, verifies containment, and ignores
  symlink, hardlink, and device entries:
  `core/internal/runtime/launch.go:417-467` and
  `core/internal/artifact/blobs.go:246-308`.
- Java-home selection resolves symlinks and enforces containment:
  `core/shim/cmd/containerd-shim-brewlet-v2/bundle_prepare.go:256-284`.
- JDK selection is gated by the `.brewlet-active` inventory:
  `bundle_prepare.go:203-205,224-226,241-254`.
- Go DSSE/in-toto verification validates payload type, key ID, PAE, ECDSA
  signature, statement type, predicate type, and subject digest:
  `core/pkg/attest/attest.go:226-262`.
- Java DSSE verification enforces equivalent P-256, signature, builder, and
  subject checks in
  `maven-plugin/src/main/java/sh/brewlet/maven/plugin/supplychain/Dsse.java`.
- Ratify verification fails closed on invalid outcomes:
  `admission/ratify-verifier/verify.go:56-113`.
- Dependency bundle entries are flat regular JAR files with per-entry digest
  verification:
  `core/internal/artifact/dependency_bundle.go:469-531`.
- Launch config parsing rejects unknown fields, and `cds.archive` is restricted
  to a bare filename:
  `core/internal/artifact/artifact.go:303-343`.
- The NodeProfile validating webhook uses `failurePolicy: Fail` and validates
  JDK source image references:
  `kubernetes/internal/controller/nodeprofile_validate.go:24-100` and
  `kubernetes/charts/brewlet/templates/admission.yaml:180-183`.
- Containerd reconfiguration is validated and rolled back transactionally:
  `provisioner/entrypoint.sh:596-611,741-772`.
- Provisioner downloads are checksum-verified twice, external build images are
  digest-pinned, and repository policy tests reject direct Dockerfile downloads:
  `provisioner/Dockerfile:28,51,67-120,140`,
  `provisioner/download-verified.sh`, and
  `scripts/check-container-build-security.sh`.
- Operator and admission pods run non-root with a read-only root filesystem,
  no privilege escalation, and all capabilities dropped:
  `kubernetes/charts/brewlet/templates/operator.yaml:99,136-139` and
  `kubernetes/charts/brewlet/templates/admission.yaml:110,141-144`.
- Registry pagination links are origin-locked and cycle-detected:
  `RegistryClient.java:347,451,497-510`.
- The CLI installer uses HTTPS, a temporary extraction directory, and SHA-256
  verification: `site/install.sh:59-95`.
- Workflows avoid `pull_request_target`, `workflow_run`, and untrusted event
  interpolation in shell commands. Release version input is validated.
- No hardcoded production credentials were identified.

## Most dangerous attack chains and remediation status

### A. Historical namespace tenant to cluster compromise — remediated

The original chain began with a namespace tenant publishing a digest-pinned
malicious image whose manifest carried a traversal descriptor. Protected CRI
image identity ensured the shim executed the requested image, but descriptor
paths were unvalidated, so the root shim could mount host `/` — exposing kubelet
credentials and every co-located workload's Secrets and service-account tokens
for replay against the API server. Descriptor digests are now rejected unless
they are canonical `sha256:<64 hex>`, every resolved path is independently proven
to stay under the content store's blob directory, and blob bytes are verified
against the digest that named them before they are read, staged, or mounted.
Default control-plane provisioning (Finding 10) remains open and still widens
the blast radius of any future node-level primitive.

### B. Historical cross-tenant JVM code injection — remediated

The original chain combined artifact-requested UID 0 with a writable global
AppCDS cache. Artifact credentials are now rejected, CRI process identity is
preserved, cache keys include trusted namespace, verified manifest, JDK build,
and process UID, and workloads see only their private cache-entry directory.

### C. Historical upstream software to root on every node — remediated

The original chain relied on mutable runtime tags, unrestricted mirrors, and
unverified tool archives. Runtime sources are now explicit and digest-pinned,
mirror targets are allowlisted, launcher images are mounted rather than run, and
downloaded tools and external build images are checksum- or digest-pinned.

## Documentation alignment status

| Claim | Location | Implemented behavior |
|-------|----------|----------------------|
| The JVM is non-root unless root is explicitly requested through the Pod security context | `specs/SPECIFICATION.md:1228-1229`; `docs/security.md` | **Remediated:** artifact credentials are rejected and the CRI-populated user is authoritative |
| Every runtime root arrives through a content-addressable, digest-verified pull | `specs/SPECIFICATION.md:664` | **Remediated:** every JDK and launcher source is explicit and digest-pinned |
| Only a small per-container upper/scratch layer is writable | `docs/security.md` | **Remediated:** CDS regeneration exposes only a private per-key entry; consumers mount it read-only and the elected writer alone receives it read-write |
| The shim resolves the application from the content store by digest as an integrity control | `docs/security.md` | **Remediated:** artifact selection is bound to protected CRI image identity, every descriptor digest is validated before path construction, resolved paths are confined to the content store, and blob bytes are verified against their declared digest before use |
| Ratify/Gatekeeper requires the final-image attestation and fails closed | `docs/security.md` | **Remediated:** the shim resolves the exact digest-pinned CRI request and verifies its target, config, and platform-manifest identity |
| Operators can pin all component images and OCI artifacts to digests | `docs/security.md` | **Remediated:** JDK and launcher sources require explicit digest-pinned references |
| The fail-open mutating webhook is presented as a security guardrail | `docs/security.md` | **Remediated for artifact identity:** the webhook overwrites compatibility hints, while the shim independently fails closed using protected CRI metadata |

## Prioritized remediation roadmap

### P0: Before multi-tenant or production use

1. **Remediated:** Validate every digest used in a path and verify blob content.
2. **Remediated:** Bind shim artifact selection to the CRI image identity and
   reject annotation mismatches.
3. **Remediated:** Make Brewlet admission overwrite security-sensitive
   annotations, with the shim enforcing artifact identity independently.
4. **Remediated:** Remove tenant write access to the shared AppCDS directory and
   partition cache state.
5. **Remediated:** Prevent artifact launch configuration from raising UID/GID
   privilege.

### P1: Next release

1. Sanitize and allowlist launcher names.
2. **Remediated:** Restrict `mainJar` to a contained bare filename.
3. **Remediated:** Verify every binary downloaded by the provisioner image and
   digest-pin base images.
4. **Remediated:** Require administrator-provided, digest-pinned JDK and launcher
   images, and validate and allowlist NodeProfile registry mirrors.
5. Enforce same-origin HTTPS registry token realms and exact loopback matching.

### P2: Defense in depth

1. **Remediated:** Pin GitHub Actions by SHA, narrow workflow permissions, and
   publish signatures and provenance.
2. Make node provisioning opt-in and exclude control-plane nodes by default.
3. **Remediated:** Add NetworkPolicy templates and automated short-lived webhook
   certificates.
4. Keep security documentation aligned with the remaining open controls.

## Suggested GitHub issues

1. **Validate OCI digests before path construction and verify blob content
   (remediated in issue #19 and pull request #43).** Covers Finding 1.
2. **Bind shim artifact resolution to the CRI image identity (remediated in
   issue #22 and pull request #33).** Covers Finding 4 and the identity portion
   of Finding 1.
3. **Stop mounting the shared AppCDS cache directory into workloads (remediated
   in issue #20 and pull request #31).** Covers Finding 2.
4. **Prevent artifact configuration from overriding Pod UID/GID (remediated in
   issue #21 and pull request #37).** Covers Finding 3.
5. **Validate launcher and `mainJar` path components.** Covers Findings 5 and 8,
   which share the same path-input validation fix. The `mainJar` half is
   remediated in issue #26 and pull request #41; the launcher half (Finding 5)
   remains open.
6. **Require digest-pinned administrator-provided runtime sources and validate
   registry mirrors (remediated in issue #24 and pull request #34).** Covers
   Finding 6.
7. **Verify provisioner binary downloads and pin base images (remediated in
   issue #25 and pull request #32).** Covers Finding 7.
8. **Restrict registry token authentication to approved HTTPS origins.** Covers
   Finding 9.
9. **Pin Actions and publish release signatures and provenance (remediated in
   issue #29 and pull request #42).** Covers Finding 11.
10. **Make privileged node provisioning opt-in and exclude control-plane
    nodes.** Covers Finding 10.
11. **Add NetworkPolicies and automated webhook certificate rotation
    (remediated in issue #30 and pull request #39).** Covers Finding 12.
12. **Align security documentation with enforced controls.** Covers remaining
    documentation mismatches and future remediation updates.

## Residual risk after remediation

Even after the remaining findings are fixed, Brewlet retains a high-trust node
architecture. A root containerd shim and privileged provisioner intentionally
control runtime configuration and shared node software. Dedicated Brewlet node
pools, strong workload admission, restricted NodeProfile administration,
immutable and signed node software, runtime monitoring, and rapid credential
rotation remain necessary. Without a stronger sandbox such as a
virtual-machine-based RuntimeClass, a vulnerability in the shim, JDK, launcher,
or OCI runtime can still cross workload and node boundaries.

## Assessment limitations

- No live cluster was provisioned and no exploit was executed. Findings are
  based on static control-flow and data-flow analysis.
- Finding 5 is intentionally reported as constrained because the same traversal
  string is currently used as the executable path and prevents a demonstrated
  successful workload start.
- Third-party dependencies were not scanned against an external vulnerability
  database.
- Integration harnesses were not executed because they mutate clusters and
  hosts.
- Ratify/Gatekeeper runtime behavior, Helm certificate lifecycle behavior, and
  containerd handling of custom artifact media types were analyzed from source
  and configuration rather than observed in a running cluster.
