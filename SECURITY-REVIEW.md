# Kubernetes Security Assessment and Threat Model

**Repository:** `microsoft/brewlet`  
**Revision assessed:** `f6c8a06`  
**Assessment date:** 2026-09-02  
**Method:** Static, read-only STRIDE review of source code, Kubernetes resources,
Helm templates, container build files, CI/CD workflows, release automation, and
security documentation.

## Executive summary

Brewlet replaces per-image JVMs with node-resident JDKs. A privileged DaemonSet
installs a containerd shim and JDK roots on nodes, and the root-privileged shim
constructs OCI bundles, overlay lower layers, and bind mounts for tenant
workloads.

The review identified one Critical, five High, five Medium, and one Low issue.
The most serious issue is a confirmed path traversal in artifact digest
resolution: tenant-controlled pod annotations and OCI descriptor digest strings
are converted into host filesystem paths without validating that they are
canonical SHA-256 digests. A namespace tenant that can create a Brewlet pod can
cause the root shim to bind-mount arbitrary host paths, including `/`, read-only
inside the tenant container. This exposes node credentials, kubelet state, and
the Secrets and service-account tokens of co-located workloads.

Other high-impact issues allow tenant containers to write the node-shared AppCDS
cache, artifact metadata to override the pod UID/GID, admission attestations to
verify a different image from the artifact the shim executes, and mutable or
unverified upstream content to become root-executed node software.

Several controls are implemented correctly: tar extraction rejects traversal
and ignores link/device entries; JDK Java-home resolution is contained; DSSE
and in-toto verification is complete and fail-closed; dependency bundle entries
and digests are strictly validated; the NodeProfile validating webhook fails
closed; the operator and admission pods are hardened; and containerd
configuration updates are validated and rolled back.

**Overall risk: High.** Brewlet should not be used on multi-tenant clusters
until Findings 1-4 are fixed. Restricting Brewlet to dedicated, platform-owned
node pools reduces exposure but does not address the node software supply-chain
risks.

## Findings summary

| # | Severity | Finding | Confidence |
|---|----------|---------|------------|
| 1 | Critical | Tenant-controlled OCI digests can bind-mount arbitrary node paths | 9/10 |
| 2 | High | The node-shared AppCDS cache is writable from a tenant container | 8/10 |
| 3 | High | Artifact launch configuration overrides the pod UID/GID | 9/10 |
| 4 | High | Attestation enforcement verifies a different identity from the executed artifact | 8/10 |
| 5 | Medium | The launcher annotation can traverse outside the launcher root | 7/10 |
| 6 | High | Mutable, unsigned JDK images become root-executed node runtimes | 9/10 |
| 7 | High | Unverified downloaded binaries are installed on every node | 9/10 |
| 8 | Medium | `mainJar` can escape staging and select arbitrary host paths | 8/10 |
| 9 | Medium | Registry credentials can be forwarded cross-origin or over HTTP | 8/10 |
| 10 | Medium | The privileged provisioner defaults to every node | 9/10 |
| 11 | Medium | Mutable GitHub Actions and absent provenance weaken release integrity | 9/10 |
| 12 | Low | Long-lived webhook credentials and absent NetworkPolicies reduce defense in depth | 9/10 |

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
| Upstream registry or vendor | Controls mutable JDK tags and downloadable tool release assets |
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
- Provisioner image downloads, public JDK image tags, GitHub Actions, and
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

1. A tenant supplies `brewlet.sh/*` pod annotations.
2. The mutating webhook fills only missing annotations and is configured with
   `failurePolicy: Ignore`.
3. The provisioner configures containerd to forward `brewlet.sh/*` pod
   annotations to the OCI spec.
4. The root shim reads `brewlet.sh/artifact-digest` directly from the OCI spec.
5. The shim reads a manifest from the containerd content store, then resolves
   descriptor digest strings into host paths.
6. These paths become bind-mount sources or overlay lower layers in the tenant
   container.

This flow crosses the tenant-to-root trust boundary without binding the
annotation to the CRI image identity and without validating digest syntax at
the path construction sites.

## Detailed findings

### 1. Tenant-controlled OCI digests can bind-mount arbitrary node paths

**Severity:** Critical  
**Confidence:** 9/10  
**STRIDE:** Information Disclosure, Elevation of Privilege  
**CWE:** CWE-22, CWE-20, CWE-345  
**Type:** Confirmed vulnerability

**Attacker prerequisites:** Permission to create a Pod in one namespace and
ability to publish an image to a registry the node can pull from.

**Evidence:**

- `provisioner/entrypoint.sh:466-468` configures
  `pod_annotations = ["brewlet.sh/*"]`.
- `kubernetes/internal/admission/mutate.go:61-70` fills artifact annotations
  only when they are empty and does not validate tenant-supplied values.
- `kubernetes/charts/brewlet/values.yaml:90-91` defaults the mutating webhook to
  `failurePolicy: Ignore`.
- `core/shim/cmd/containerd-shim-brewlet-v2/service_linux.go:286-303` copies
  `brewlet.sh/artifact-digest` into `ManifestDigest`.
- `core/shim/cmd/containerd-shim-brewlet-v2/resolver.go:78-103` splits an
  arbitrary digest string and passes its components to `filepath.Join`.
- `core/internal/artifact/artifact.go:350-357` represents OCI descriptor
  digests as unrestricted strings.
- `core/internal/artifact/blobs.go:96-140` resolves descriptor digests into
  host paths and checks only that the resulting path exists.
- `core/shim/cmd/containerd-shim-brewlet-v2/service_linux.go:549,564-566`
  bind-mounts the resolved JAR path into the container.

**Attack path and impact:**

1. The attacker publishes content containing a Brewlet manifest whose JAR
   descriptor digest is a traversal value such as
   `sha256:../../../../../..`.
2. The attacker causes containerd to pull the content onto a target node.
3. A Brewlet pod supplies the content blob digest in
   `brewlet.sh/artifact-digest`.
4. `filepath.Join` cleans the descriptor traversal and resolves the supposed
   content-store blob path to `/`.
5. `os.Stat("/")` succeeds and the root shim bind-mounts the host root
   filesystem read-only into the attacker container.
6. The workload can read kubelet credentials and mounted Secrets and tokens for
   every co-located pod. On a control-plane node it may also reach control-plane
   PKI and etcd data.

This is a container-escape-equivalent host read primitive and a realistic path
to cluster compromise.

**Remediation:**

- Require every digest used in path construction to match
  `^sha256:[0-9a-f]{64}$`; return an error instead of a path on mismatch.
- Apply validation in `contentBlobPath`, `Store.BlobPath`, manifest parsing,
  and every layer selector.
- Verify blob bytes against the declared digest before use.
- Bind artifact identity to the CRI-resolved image as described in Finding 4.

**Remediation test:**

- Add table tests for traversal, missing/extra hex digits, uppercase hex,
  unsupported algorithms, and empty components.
- Add a fuzz test asserting that every resolved content path remains under the
  content-store root.
- Add an integration test proving a mismatched or traversal digest fails task
  creation and produces no mount.

### 2. The node-shared AppCDS cache is writable from a tenant container — remediated

**Severity:** High  
**Confidence:** 8/10  
**STRIDE:** Tampering, Elevation of Privilege  
**CWE:** CWE-732, CWE-269, CWE-349  
**Type:** Confirmed architectural vulnerability
**Status:** Remediated by [issue #20](https://github.com/microsoft/brewlet/issues/20)

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
  content-verified resolved platform-manifest digest, and the exact JDK build.
  Tenant artifact annotations are not used as cache identity.
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

### 3. Artifact launch configuration overrides the pod UID/GID

**Severity:** High  
**Confidence:** 9/10  
**STRIDE:** Elevation of Privilege  
**CWE:** CWE-250, CWE-863  
**Type:** Confirmed vulnerability and insecure default

**Attacker prerequisites:** Control of the executed artifact's config blob.

**Evidence:**

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

### 4. Attestation enforcement verifies a different identity from the executed artifact

**Severity:** High  
**Confidence:** 8/10  
**STRIDE:** Spoofing, Tampering  
**CWE:** CWE-345, CWE-807  
**Type:** Confirmed admission bypass

**Attacker prerequisites:** Permission to create a Brewlet Pod in a cluster
using the documented Ratify/Gatekeeper policy.

**Evidence:**

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

### 6. Mutable, unsigned JDK images become root-executed node runtimes

**Severity:** High  
**Confidence:** 9/10  
**STRIDE:** Tampering, Elevation of Privilege  
**CWE:** CWE-494, CWE-1357  
**Type:** Architectural risk and insecure default

**Attacker prerequisites:** Compromise of an upstream mutable JDK tag, or
control of a NodeProfile registry mirror through cluster/GitOps compromise.

**Evidence:**

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
  validate registry mirrors and prevents curated distributions from supplying
  a custom pinned source.
- `core/shim/cmd/containerd-shim-brewlet-v2/service_linux.go:429` uses the
  resulting JDK tree as a workload rootfs lower layer.

**Attack path and impact:** An upstream tag or unvalidated mirror serves a
malicious image. The provisioner copies it to the node and root-executes its
Java binary during validation. The malicious tree then underlies every Brewlet
workload on that node. This provides root execution and persistent compromise
across the provisioned fleet.

**Remediation:** Ship and enforce a signed distribution-to-digest map, support
digest-pinned curated JDKs, verify signatures before extraction, validate and
allowlist mirror hosts, and remove host networking from launcher installation.

**Remediation test:** Assert curated source resolution returns `@sha256:`
references. Reject mirror values containing schemes, whitespace, empty hosts,
or unapproved destinations. Verify a tampered image fails closed before node
readiness labels are applied.

### 7. Unverified downloaded binaries are installed on every node

**Severity:** High  
**Confidence:** 9/10  
**STRIDE:** Tampering  
**CWE:** CWE-494  
**Type:** Confirmed supply-chain vulnerability

**Attacker prerequisites:** Compromise of, or an on-path position to, the
upstream release assets used during the provisioner image build.

**Evidence:**

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

### 8. `mainJar` can escape staging and select arbitrary host paths

**Severity:** Medium  
**Confidence:** 8/10  
**STRIDE:** Information Disclosure  
**CWE:** CWE-22  
**Type:** Confirmed vulnerability

**Attacker prerequisites:** Control of the runnable image's
`brewlet.sh/jvm-config` metadata.

**Evidence:**

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

### 9. Registry credentials can be forwarded cross-origin or over HTTP

**Severity:** Medium  
**Confidence:** 8/10  
**STRIDE:** Information Disclosure, Spoofing  
**CWE:** CWE-522, CWE-918, CWE-20  
**Type:** Confirmed vulnerability

**Attacker prerequisites:** A malicious or compromised registry to which a
developer or CI job authenticates.

**Evidence:**

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

### 11. Mutable GitHub Actions and absent provenance weaken release integrity

**Severity:** Medium  
**Confidence:** 9/10  
**STRIDE:** Tampering  
**CWE:** CWE-1357, CWE-829  
**Type:** Supply-chain architectural risk

**Attacker prerequisites:** Compromise of an action repository or ability to
move a referenced action tag.

**Evidence:**

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

### 12. Long-lived webhook credentials and absent NetworkPolicies

**Severity:** Low  
**Confidence:** 9/10  
**STRIDE:** Spoofing, Information Disclosure  
**CWE:** CWE-1188, CWE-295  
**Type:** Defense-in-depth recommendation

**Attacker prerequisites:** Permission to read the webhook Secret or Helm
release data, or pod-network access to exposed component endpoints.

**Evidence:**

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

## Most dangerous attack chains

### A. Namespace tenant to cluster compromise

A namespace tenant seeds a malicious manifest blob, supplies its digest through
the Brewlet annotation, and uses a traversal descriptor so the root shim mounts
host `/`. The tenant reads kubelet credentials and every co-located workload's
Secrets and service-account tokens, then replays the most privileged credential
against the API server. Default control-plane provisioning increases the chance
that control-plane PKI is exposed directly.

### B. Cross-tenant JVM code injection

An attacker publishes an artifact requesting UID 0, bypasses the Pod's
non-root intent, requests CDS regeneration using the victim's artifact key, and
wins the writer election. The attacker receives the shared cache read-write,
replaces the victim archive, and the victim JVM maps attacker-controlled class
metadata on its next launch.

### C. Upstream software to root on every node

A mutable JDK tag, registry mirror, or unverified tool archive is compromised.
The provisioner pulls or downloads it, copies it to the host, and executes it as
root with access to host namespaces or the containerd socket. The compromised
JDK tree then becomes a lower layer for every Brewlet workload.

## Documentation mismatches

| Claim | Location | Implemented behavior |
|-------|----------|----------------------|
| The JVM is non-root unless root is explicitly requested through the Pod security context | `specs/SPECIFICATION.md:1228-1229`; `docs/security.md` | Artifact `user.uid/gid` overrides the Pod-derived OCI user |
| Every runtime root arrives through a content-addressable, digest-verified pull | `specs/SPECIFICATION.md:664` | Curated JDKs use mutable tags without signature or digest enforcement |
| Only a small per-container upper/scratch layer is writable | `docs/security.md` | CDS regeneration mounts the node-shared cache read-write |
| The shim resolves the application from the content store by digest as an integrity control | `docs/security.md` | The digest is tenant-settable, unvalidated, and not bound to the image |
| Ratify/Gatekeeper requires the final-image attestation and fails closed | `docs/security.md` | Admission verifies `image`; the shim can execute a different annotation-selected artifact |
| Operators can pin all component images and OCI artifacts to digests | `docs/security.md` | Curated JDK sources cannot currently be digest-pinned |
| The fail-open mutating webhook is presented as a security guardrail | `docs/security.md` | It improves availability but cannot enforce Brewlet artifact identity |

## Prioritized remediation roadmap

### P0: Before multi-tenant or production use

1. Validate every digest used in a path and verify blob content.
2. Bind shim artifact selection to the CRI image identity and reject annotation
   mismatches.
3. Make Brewlet admission overwrite security-sensitive annotations and fail
   closed for Brewlet RuntimeClass pods.
4. Remove tenant write access to the shared AppCDS directory and partition
   cache state.
5. Prevent artifact launch configuration from raising UID/GID privilege.

### P1: Next release

1. Sanitize and allowlist launcher names.
2. Restrict `mainJar` to a contained bare filename.
3. Verify every binary downloaded by the provisioner image and digest-pin base
   images.
4. Support digest-pinned, signature-verified curated JDK images.
5. Validate and allowlist NodeProfile registry mirrors.
6. Enforce same-origin HTTPS registry token realms and exact loopback matching.

### P2: Defense in depth

1. Pin GitHub Actions by SHA, narrow workflow permissions, and publish
   signatures and provenance.
2. Make node provisioning opt-in and exclude control-plane nodes by default.
3. Add NetworkPolicy templates and automated short-lived webhook certificates.
4. Correct the documented guarantees listed above.

## Suggested GitHub issues

1. **Validate OCI digests before path construction and verify blob content.**
   Covers Finding 1.
2. **Bind shim artifact resolution to the CRI image identity and make Brewlet
   admission fail closed.** Covers Finding 4 and the identity portion of
   Finding 1.
3. **Stop mounting the shared AppCDS cache directory into workloads.** Covers
   Finding 2.
4. **Prevent artifact configuration from overriding Pod UID/GID.** Covers
   Finding 3.
5. **Validate launcher and `mainJar` path components.** Covers Findings 5 and 8,
   which share the same path-input validation fix.
6. **Support signed, digest-pinned curated JDK sources and validate registry
   mirrors.** Covers Finding 6.
7. **Verify provisioner binary downloads and pin base images.** Covers Finding
   7.
8. **Restrict registry token authentication to approved HTTPS origins.** Covers
   Finding 9.
9. **Pin Actions and publish release signatures and provenance.** Covers
   Finding 11.
10. **Make privileged node provisioning opt-in and exclude control-plane
    nodes.** Covers Finding 10.
11. **Add NetworkPolicies and automated webhook certificate rotation.** Covers
    Finding 12.
12. **Align security documentation with enforced controls.** Covers all
    documentation mismatches.

## Residual risk after remediation

Even after the identified input-validation and identity-binding issues are
fixed, Brewlet retains a high-trust node architecture. A root containerd shim
and privileged provisioner intentionally control runtime configuration and
shared node software. Dedicated Brewlet node pools, strong workload admission,
restricted NodeProfile administration, immutable and signed node software,
runtime monitoring, and rapid credential rotation remain necessary. Without a
stronger sandbox such as a virtual-machine-based RuntimeClass, a vulnerability
in the shim, JDK, launcher, or OCI runtime can still cross workload and node
boundaries.

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
