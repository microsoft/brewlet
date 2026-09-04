# Security

Brewlet keeps **container-grade isolation** (runc) while adopting a Wasm-grade
developer experience. This page covers the isolation model, defaults, digest
verification, and the one genuinely sharp edge: privileged node provisioning.

See also [SPECIFICATION §11](https://github.com/microsoft/brewlet/blob/main/specs/SPECIFICATION.md).

---

## Isolation parity with containers

Execution is **runc-backed**, so a Brewlet workload gets the same isolation
primitives as any ordinary pod:

- **namespaces** (pid/net/mount/ipc/uts) and **cgroup v2** resource control;
- **seccomp / AppArmor** profiles;
- **CNI** networking (a real, isolated pod netns and pod IP).

The JAR is treated as **untrusted code** and runs inside that sandbox — nothing about
"it's just a JAR" weakens the boundary relative to a container image.

---

## Non-root by default

`JavaApplication` workloads and standalone OCI bundles default to the
unprivileged UID/GID `65532:65532`. Generated `JavaApplication` Pods also use
`RuntimeDefault` seccomp, disable privilege escalation, and drop all Linux
capabilities. For raw Pods or Deployments, set the identity with Pod
`securityContext`:

```yaml
spec:
  securityContext:
    runAsNonRoot: true
    runAsUser: 1000
    runAsGroup: 1000
  runtimeClassName: brewlet
  containers:
    - name: app
      image: registry.example.com/team/app@sha256:REPLACE_WITH_IMAGE_DIGEST
      securityContext:
        allowPrivilegeEscalation: false
        readOnlyRootFilesystem: true          # the JDK root is RO already
        capabilities: { drop: ["ALL"] }
```

The CRI-populated OCI process user is authoritative: the Brewlet shim preserves
it unchanged. Process credentials are forbidden in artifact launch metadata, and
an artifact config containing `user` is rejected before runc starts the process.
Explicit root therefore requires a trusted deployment/runtime choice,
such as a raw Pod `securityContext` or standalone `brewlet bundle --uid 0 --gid
0`; untrusted artifact bytes cannot request it.

The JDK runtime root is mounted **read-only** and shared; only a small per-container
upper/scratch layer is writable.

Artifact metadata never selects a host path. The `mainJar` and `cds.archive`
filenames are validated as bare filenames — no path separator, wildcard or parent
reference — at publish time, when the launch config is decoded (including from a
runnable image's `brewlet.sh/jvm-config` annotation), and again in the shim before
the file is bind-mounted. Resolution additionally verifies that every path it hands
to the shim is contained by the per-image staging directory, so a malicious image
cannot point the read-only JAR or AppCDS mount at an arbitrary host file.

---

## Artifact integrity

Because the JAR is a first-class OCI artifact, standard supply-chain controls apply:

- **Digest-pin** every runnable image reference (`repo@sha256:…`); tag-only
  Kubernetes execution is rejected. The admission webhook overwrites
  `brewlet.sh/artifact-digest` as a compatibility hint from the selected Pod
  image. The shim takes the exact target digest from containerd's protected CRI
  requested-image metadata, requires the protected
  `io.kubernetes.cri.image-name` OCI annotation to name the same target, resolves
  that target directly from the content store, and verifies the selected
  platform manifest's config digest against CRI's recorded image identity.
  Tenant-controlled Brewlet annotations never select executable content. See
  [Building & publishing](building-and-publishing.md#4-pin-to-a-digest).
- **Every descriptor digest is validated before it becomes a path.** Config,
  JAR, classpath, modulepath, and CDS descriptors inside a manifest are
  tenant-authored data, so each must match `sha256:` followed by exactly 64
  lowercase hex characters. Resolved blob paths are then independently
  re-checked to confirm they stay under the content store's `blobs/sha256`
  directory, so a malformed or traversing digest yields an error rather than a
  host path.
- **Blob bytes are verified before they are read, staged, or mounted.** Every
  blob is hashed and compared against its declared descriptor digest, including
  the JAR and CDS archives that are bind-mounted into the container. A blob that
  is present at the right path but does not match its digest is rejected.

### Supply-chain attestations

[Managed dependency bundles](managed-dependency-bundles.md) can carry optional
bundle-publisher provenance. A final thin-JAR image can independently carry a
DSSE envelope containing an in-toto Statement v1 whose managed-dependency
predicate binds the image digest, thin-JAR verdict, application JAR, bundle,
dependency layer, lock, SBOM, source BOM, and application-builder identity.
Signatures use ECDSA P-256 and a `keyid` derived from the public key.

The two identities are separate trust roles. Bundle-publisher trust is verified
upstream while composing the image; the final-image predicate exposes only the
application-builder identity. Identity values are free-form signed strings
trusted through the configured key, not OIDC- or Fulcio-issued identities.

Deploy [Ratify/Gatekeeper admission enforcement](admission-enforcement.md) to
require the final-image attestation for `runtimeClassName: brewlet` pods. It
requires an OCI 1.1 Referrers-API registry and digest-pinned images, verifies
the Pod image before admission, and fails closed when trusted evidence cannot
be discovered or verified.

---

## Centralized JDK CVE management

The single biggest security win: the JVM lives on the node, shared across workloads.
**Patching the node JDK patches every workload at once** — no rebuilding and
re-pushing hundreds of images to ship a JVM CVE fix. See
[JDK management](jdk-management.md#patching-upgrading-jdks).

---

## The sharp edge: privileged provisioning

Node provisioning is **privileged and mutates the host**. The
`brewlet-node-provisioner` DaemonSet:

- runs privileged with `hostPID`;
- writes the shim to the host `PATH` and JDK/launcher roots under `/opt/brewlet`;
- writes an imported containerd drop-in when supported, or a backed-up primary
  config otherwise, and activates it through the host service manager.

Mitigations and guardrails:

| Guardrail | How |
|---|---|
| **Provisioning is scoped, but broad by default** | The chart's default `NodeProfile` provisions **every** node (§5.6). To limit the blast radius, disable it (`defaultProfile.enabled=false`) and define named `NodeProfile`s scoped to platform-owned pools. The legacy standalone DaemonSet instead touches only nodes carrying the `brewlet.sh/provision=true` **label**. |
| **Scope to platform-owned pools** | Use named `NodeProfile` pools (or the `brewlet.sh/provision` label for the standalone path) to restrict provisioning to nodes your platform team controls. Do **not** provision shared/hostile multi-tenant nodes. |
| **Build inputs fail closed** | The provisioner verifies repository-pinned SHA-256 values for `kubectl`, `ctr`, `crictl`, and downloaded notices before extraction. Its runtime image receives only verified outputs, all repository Dockerfile bases are digest-pinned, and CI and release workflows reject corrupt assets or future unpinned/download-bypass changes. |
| **Sources are immutable before host access** | Every JDK and launcher source must be a fully qualified, tagless `repository@sha256:<digest>` reference with an explicit path. All entries are validated before shim installation or any host containerd/filesystem operation. Brewlet has no built-in runtime catalog. |
| **Mirror destinations are externally allowlisted** | Configure exact destination registry hosts through `security.allowedSourceMirrorHosts`; empty disables mirrors. Admission, reconciliation, and the provisioner reject malformed, duplicate, self, or unapproved mappings, while approved rewrites retain the reviewed digest. |
| **Admission is not the security boundary** | The operator repeats source, mirror, and pool validation before creating a privileged DaemonSet. Invalid profiles receive `Ready=False` with reason `InvalidProfile`; their DaemonSet and profile-owned node advertisements are removed. |
| **Stale provisioners cannot republish readiness** | Provisioner DaemonSets are deleted in the foreground, reconciliation waits for their pods to terminate, and each managed provisioner rechecks its profile UID, generation, and deletion state immediately before advertising node capabilities. |
| **Stale or hostile image roots are not trusted** | Existing JDK roots are not executed until `.brewlet-source` matches the newly verified digest. Launcher images are pulled and mounted for host-side copying; they are not executed, do not receive host networking, and do not receive a writable host bind mount. |
| **Host mutation is validated and reversible** | The provisioner rejects containerd servers older than 2.0, validates the effective config before activation, checks the live runtime handler afterward, and restores known-good configuration if restart or health checks fail. Nodes remain unready until JDK smoke tests and launcher executable checks pass. |
| **The operator is unprivileged** | The operator only talks to the API server; only the DaemonSet it manages is privileged. |
| **Webhook outages do not cross the security boundary** | Pod and NodeProfile transport failures default to `Ignore`; the shim and NodeProfile reconciler independently repeat the security-critical checks before execution or privileged work. Set `admission.nodeProfileFailurePolicy=Fail` after certificate bootstrap when synchronous profile-write rejection is preferred. |
| **Webhook credentials can rotate automatically** | The simple Helm path uses a 90-day self-signed certificate. Enable `admission.certManager` for continuous issuance, renewal, and CA-bundle injection. |
| **Control-plane endpoints can be isolated** | Enable `networkPolicy` with explicit API-server CIDRs and monitoring peers to restrict webhook and metrics ingress. |
| **Releases are independently verifiable** | Every workflow action is pinned to a full commit SHA, release write permissions are scoped to the individual publishing jobs, and the release workflow publishes SLSA build provenance for each component image, the OCI chart, and every GitHub Release asset. See [Verify a release](installation.md#verify-a-release). |

> ⚠️ Treat enabling Brewlet on a node the same way you'd treat any privileged
> node-bootstrap DaemonSet (a pattern also used for node runtime installation in
> the Wasm ecosystem). Document the blast radius.

---

## Multi-tenancy guidance

- **Trusted tenants / your own services:** runc isolation is equivalent to ordinary
  containers — appropriate as-is.
- **Untrusted or hostile JARs:** keep Brewlet provisioning off shared nodes. runc
  provides the same trust boundary as a normal container, no more and no less.

---

## Hardening checklist

- [ ] Provision only platform-owned node pools; use named `NodeProfile`s for
      operator-managed installations or `brewlet.sh/provision` only for the
      legacy standalone path. See
      [Capability labels and autoscaling](capability-labels-and-autoscaling.md).
- [ ] Build component images only from repository-pinned base-image digests and
      checksum-verified provisioner assets.
- [ ] Pin component images and OCI artifacts to **digests**. Published charts
      already record the digests the release built; keep them rather than
      overriding `images.tag`.
- [ ] Verify build provenance for every Brewlet artifact you install with
      `./scripts/verify-release-provenance.sh <version>` (or `gh attestation
      verify`) before promoting a release. See
      [Verify a release](installation.md#verify-a-release).
- [ ] Use reviewed SHA-256 digest pins and absolute paths for every JDK and
      launcher source; never use mutable tags.
- [ ] Allowlist only platform-operated mirror hosts and ensure mirrored OCI
      manifests retain their original digests.
- [ ] Run workloads `runAsNonRoot`, drop capabilities, `readOnlyRootFilesystem`.
- [ ] Enable **cert-manager** for automatic admission webhook certificate
      renewal. See [Configuration](configuration.md#admission-webhook).
- [ ] Enable Brewlet **NetworkPolicies** with the real kubelet and API-server
      source CIDRs and only the monitoring namespace/pods that need metrics
      access.
- [ ] Require trusted final-image managed-dependency attestations with
      [admission enforcement](admission-enforcement.md); pin images and the
      verifier plugin to digests.
- [ ] Plan JDK patch cadence — it's now a single centralized lever.

Future security capabilities are tracked in the [roadmap](https://github.com/microsoft/brewlet/blob/main/ROADMAP.md#security-and-isolation).
