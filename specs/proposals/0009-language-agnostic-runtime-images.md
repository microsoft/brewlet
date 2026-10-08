# Proposal 0009 — Brewlet 2.0: language-agnostic runtime images

- **Status:** draft; not implemented. Proposed as the defining change of a
  **breaking** Brewlet 2.0 release, not as an additive 1.x mode
- **Related roadmap item:** language-agnostic runtime images (Brewlet 2.0)
- **Current contracts replaced:** application artifact (SPEC §4.1–§4.4), JDK and
  launcher inventory (SPEC §5.3, SPEC §5.4, SPEC §5.6), shim JDK selection (SPEC §6.1), JVM
  resource mapping (SPEC §10), `JavaApplication` (SPEC §8.2, SPEC §9), and the JDK/launcher
  capability labels ([CAPABILITY_LABELS](../CAPABILITY_LABELS.md))
- **Current contracts retained and generalized:** runnable-image delivery
  (SPEC §4.4), managed dependency bundles (SPEC §4.5), copy-from-image node roots (SPEC §5.3),
  `NodeProfile` pool selection and rollout (SPEC §5.6), the runc-backed shim (SPEC §6),
  `RuntimeClass/brewlet` (SPEC §7), and the admission webhook (SPEC §8.3)
- **Related designs:** [0007 — baked golden-image delivery](0007-baked-golden-image-delivery.md)

This proposal removes Java from Brewlet's core contract. Developers publish
**application artifacts** (code, plus dependencies either as layers or as
independently published layer sets) to an OCI registry. Platform teams approve
**runtime images** — digest-pinned "golden" base images such as a JRE, a Python
interpreter, a Node.js runtime, or a distroless base for static binaries — in the
Brewlet inventory. A pod selects a runtime image **by ID only**, never by digest,
and the node-resident shim composes the approved runtime with the application at
launch time.

References written as `SPEC §N` cite [`SPECIFICATION.md`](../SPECIFICATION.md);
bare `§N` references cite sections of this proposal.

It does not change shipped behavior, so [`SPECIFICATION.md`](../SPECIFICATION.md)
remains unchanged until the design is accepted and implemented.

The key words MUST, MUST NOT, SHOULD, and MAY describe the proposed 2.0 contract.

---

## 1. Summary

Brewlet 1.x separates a Java application from its JDK at launch time. On
inspection, almost nothing in that separation is Java-specific:

- A JDK "root" (SPEC §5.3) is already the **complete root filesystem** of an
  administrator-approved, digest-pinned image, copied read-only onto the node.
- The application image (SPEC §4.4) is already a set of ordinary
  `tar+gzip` layers plus a small launch descriptor.
- The shim (SPEC §6) already overlays the two and delegates to runc.

The Java-specific parts are the inventory vocabulary (distribution, feature,
launcher, `javaHome`), the launch descriptor (`mainJar`, `entry.mode`,
`addOpens`, …), and the JVM conveniences built on top (`jvm.args`, AppCDS,
`JavaApplication`, JVM resource mapping).

Brewlet 2.0 keeps the mechanism and drops the vocabulary:

```text
 Platform operator                          Developer / CI
 ─────────────────                          ──────────────
 RuntimeImage "java-21"     ─┐              app layers   (/app/...)
 RuntimeImage "python-3.12"  │ approved     dep layers   (/app/...)  ← as layers, or
 RuntimeImage "static"       │ inventory    launch layer (launch.json)  reused from an
   each pinned to a digest  ─┘                                         independent
            │                                     │                    layer set
            ▼                                     ▼
 NodeProfile: which IDs on which pools     OCI registry (digest-addressed)
            │                                     │
            ▼                                     ▼
 Provisioner installs read-only            Pod: runtimeClassName: brewlet
 runtime roots per ID + generation              annotation brewlet.sh/runtime-image: python-3.12
            │                                     │
            └──────────────┐   ┌──────────────────┘
                           ▼   ▼
               containerd → brewlet shim → overlay(runtime root ⟵ app layers) → runc
```

## 2. Motivation

- **Every language has the same problem.** Base-image sprawl, slow CVE
  remediation in language runtimes and userland, and per-team Dockerfiles are not
  Java problems. Python, Node.js, .NET, Ruby, and even static Go binaries (which
  still need CA bundles, tzdata, and a hardened userland) benefit from a
  centrally governed runtime that ships separately from application bytes.
- **The Java-specific surface is the expensive surface.** JDK feature/distribution
  matching, launcher packages, the JVM flag grammar, AppCDS cache governance, and
  the `JavaApplication` controller account for most of Brewlet's contract and
  test matrix while providing value only to one ecosystem.
- **Runtime patching without application rebuilds.** Because pods name an ID
  rather than a digest, a platform team rotates the digest behind
  `python-3.12` once; every new container on every node picks up the patched
  runtime without touching application repositories or manifests.
- **Shared runtime pages.** One read-only runtime root per ID per node is shared by
  every container that uses it, preserving Brewlet's density and pull-cost
  benefits for every language rather than only for the JVM.

## 3. Goals

- Define a language-agnostic **application artifact** consisting of application
  layers, optional dependency layers, and a **launch configuration layer**.
- Define an approved **runtime image inventory** whose entries are identified by
  a stable, human-meaningful **ID** and resolved to a digest only by the platform.
- Require pods using `runtimeClassName: brewlet` to name exactly one runtime image
  ID through an annotation, with **no digest** in the workload.
- Keep Kubernetes-native semantics: `command`, `args`, `env`, `workingDir`,
  `securityContext`, probes, volumes, and resources behave as for any container.
- Guarantee that the platform-owned runtime cannot be shadowed by application
  content, so a runtime patch is actually effective.
- Allow dependencies to be published and governed **independently** from
  applications while still being delivered as ordinary image layers.
- Let platform teams approve large runtime inventories without installing every
  runtime on every node, by installing selected runtimes on first use.
- Keep the Maven plugin as an optional **Java producer** of 2.0 artifacts.

## 4. Non-goals

- In-place migration of 1.x clusters or artifacts. Under the
  [pre-GA compatibility policy](../../docs/compatibility.md), 2.0 is installed by
  teardown and reinstallation; 1.x artifacts are republished (§13).
- Per-workload digest pinning of the runtime. A workload that must pin an exact
  runtime digest should use an ordinary image with the stock runc RuntimeClass, or
  a dedicated, narrowly scoped runtime image ID (§7.4).
- Language-aware tuning. Brewlet injects no interpreter, VM, or GC flags; runtimes
  that need cgroup-aware tuning MUST obtain it from the runtime image itself.
- Building runtime images. Golden images are produced by the platform team's
  existing image pipeline; Brewlet only approves, distributes, and composes them.
- Sandboxes other than runc. [Proposal 0006](0006-sandbox-isolation-tiers.md)
  remains the venue for stronger isolation.

## 5. Terminology

| Term | Meaning |
|---|---|
| **Runtime image** | An administrator-approved OCI image (or image index) that supplies the complete userland and language runtime a workload executes on. Also called a *golden image*. |
| **Runtime image ID** | The stable name of a runtime image in the inventory, for example `java-21` or `python-3.12`. Never contains a digest or tag. |
| **Generation** | One digest bound to an ID at a point in time. Rotating the digest creates a new generation of the same ID. |
| **On-demand install** | Installing a runtime image ID on a node only when a container on that node first needs it (§7.5). |
| **Runtime root** | A runtime image generation unpacked read-only on a node by the provisioner. |
| **Application artifact** | The developer's OCI image: application layers, optional dependency layers, and one launch layer. |
| **Launch layer** | The layer carrying `launch.json`, the authoritative launch configuration (§6.3). |
| **Layer set** | An independently published OCI artifact of dependency layers that application artifacts reuse byte-for-byte (§6.5). |

## 6. Application artifact

### 6.1 Shape

An application artifact is a **runnable OCI image** in the sense of SPEC §4.4: kubelet
pulls it with the standard image path, and the shim reads it from the containerd
content store by digest. It MUST be published as an OCI image index when it
targets more than one platform, and MAY be a single image manifest otherwise.

Each platform manifest MUST contain:

1. an `application/vnd.oci.image.config.v1+json` config (§6.4);
2. one or more **application layers**, media type
   `application/vnd.oci.image.layer.v1.tar+gzip` (or `+zstd`), descriptor
   annotation `brewlet.sh/layer=app`;
3. zero or more **dependency layers**, same media types, descriptor annotation
   `brewlet.sh/layer=dependency`; and
4. exactly one **launch layer**, same media types, descriptor annotation
   `brewlet.sh/layer=launch`, which MUST be the **last** layer.

The manifest MUST carry `artifactType`-independent identification through the
annotation `brewlet.sh/artifact-version=2`. Platform-neutral content (bytecode,
pure Python, JavaScript) SHOULD be published for every platform the fleet runs,
with identical layer digests in each platform manifest so nodes deduplicate them.
Content with native code (static binaries, wheels or addons with shared objects,
JNI libraries) MUST be published only for the platforms it supports; Kubernetes
then fails such a pod on an unsupported node at image pull, as for any image.

The 1.x native-artifact media types (`application/vnd.brewlet.jar.layer.v1+jar`
and siblings) and the `brewlet.sh/jvm-config` annotation are removed.

### 6.2 Layer content rules

Application and dependency layers are ordinary rootfs-relative tars, but every
entry MUST lie under `app/` (that is, `/app` in the sandbox). The shim MUST
reject, before any mount, a layer containing:

- any path outside `app/`, or any path that is absolute, contains `..`, or
  escapes `app/` through a symlink or hardlink target;
- device, FIFO, or socket nodes;
- setuid or setgid bits;
- whiteouts or opaque-directory markers that target anything outside `app/`.

These rules make the runtime root **unshadowable**: no application or dependency
layer can replace `/usr`, `/etc`, `/lib`, or the language runtime. A patched
runtime generation is therefore effective for every workload that uses its ID.
Violations fail the container with reason `LayerPathViolation`.

Layers stack in manifest order, so later application layers may overwrite
earlier dependency files under `/app`. Producers SHOULD order layers from least to
most frequently changed (dependency layers first) to maximize reuse.

### 6.3 Launch layer and `launch.json`

The launch layer is a tar containing exactly one regular file, `launch.json`, at
the archive root. It is a layer rather than a config blob or annotation so that
kubelet can pull and unpack the image unchanged (as SPEC §4.4 requires of every layer)
while the configuration remains a content-addressed object covered by the
manifest digest and any signature over it. The shim reads it from the content
store and **never mounts it** into the sandbox.

```json
{
  "schemaVersion": 2,
  "process": {
    "entrypoint": ["python"],
    "args": ["-m", "gunicorn", "app.wsgi:application", "--bind", "0.0.0.0:8000"],
    "workingDir": "/app",
    "env": [
      { "name": "PYTHONPATH", "value": "/app/src:/app/site-packages" },
      { "name": "PYTHONDONTWRITEBYTECODE", "value": "1" }
    ]
  },
  "requires": {
    "runtimeImages": ["python-3.12", "python-3.12-fips"]
  }
}
```

Field contract (unknown fields MUST be rejected at publish and launch time):

| Field | Required | Rules |
|---|---|---|
| `schemaVersion` | yes | MUST be `2`. |
| `process.entrypoint` | yes | Non-empty argv prefix. A non-absolute `entrypoint[0]` is resolved through the runtime image's `PATH` inside the sandbox (runc semantics). |
| `process.args` | no | Default arguments appended after `entrypoint`. |
| `process.workingDir` | no | Absolute path; defaults to `/app`. |
| `process.env` | no | Ordered list of unique names. `PATH` is forbidden (§8.3). |
| `requires.runtimeImages` | no | Allow-list of runtime image IDs the artifact was built and tested against. When present, the shim MUST refuse any other ID with `RuntimeImageIncompatible`. When absent, any approved ID is accepted. |

The launch configuration intentionally contains **no** language-specific fields.
Everything a 1.x `jvm.config` expressed (`mainJar`, `entry.mode`, `addOpens`,
system properties, preview flags, AppCDS hints) becomes plain argv, produced by a
language-aware tool such as the Maven plugin (§11).

### 6.4 Image config

To give pods ordinary Kubernetes override semantics without Brewlet-specific
parsing, producers MUST mirror the launch configuration into the image config:

| Image config field | Value |
|---|---|
| `Entrypoint` | `process.entrypoint` |
| `Cmd` | `process.args` |
| `WorkingDir` | `process.workingDir` (or `/app`) |
| `Env` | `process.env` rendered as `NAME=value` |
| `User` | MUST be empty (§8.4) |
| `Labels["sh.brewlet.app"]` | `"2"` (informational only) |

The shim MUST verify that the config mirrors `launch.json` exactly and fail with
`LaunchConfigMismatch` otherwise. Because the config is authoritative only by
virtue of matching the launch layer, CRI's normal merge of the image config with
the pod spec (`command` replaces `Entrypoint`, `args` replaces `Cmd`, `env` and
`workingDir` override) then yields exactly Kubernetes semantics, and the shim
consumes the CRI-produced process fields as-is (§8.3).

`launch.json` remains the authoritative, versioned document: it carries
`requires`, it is what tooling inspects, and it is the input to the bake exporter
of [proposal 0007](0007-baked-golden-image-delivery.md).

### 6.5 Dependencies: as layers or independently

Dependencies reach the node only as **layers of the application artifact** — the
kubelet pulls exactly one image per container, and the shim never pulls on its
own. They may be governed in two ways:

1. **Bundled layers.** The producer builds dependency layers alongside the
   application (for example, `/app/lib/*.jar`, `/app/site-packages`,
   `/app/node_modules`).
2. **Independent layer sets.** A platform or library team publishes a layer set
   artifact, generalizing SPEC §4.5 managed dependency bundles:

   | Component | Media type |
   |---|---|
   | Artifact type | `application/vnd.brewlet.layerset.v1+json` |
   | Config | `application/vnd.brewlet.layerset.config.v1+json` (name, version, ecosystem, layer digests and diff IDs, optional `compatibleRuntimeImages`) |
   | Layers | `application/vnd.oci.image.layer.v1.tar+gzip`, `brewlet.sh/layer=dependency` |
   | Lock (optional) | `application/vnd.brewlet.layerset.lock.v1+json` — ecosystem-neutral inventory of `{ecosystem, name, version, path, sha256}` entries |

   A producer that consumes a layer set MUST copy (or cross-repository mount) its
   layers **byte-for-byte**, preserving digest and diff ID, and MUST record the
   binding in the manifest annotation `brewlet.sh/layerset-evidence` (the
   generalization of `brewlet.sh/managed-dependency-evidence`). Identical digests
   let registries and nodes store each layer set once, regardless of how many
   applications consume it, and let admission policy (for example the existing
   Ratify/Gatekeeper example) require approved layer sets by digest.

The 1.x Maven-specific bundle contract (BOM, GAV lock, `compatibleJdks`) becomes
the Maven plugin's producer of a `java`-ecosystem layer set.

## 7. Runtime image inventory

### 7.1 `RuntimeImage` resource

Approval and placement are separated. Approval is a cluster-scoped
`RuntimeImage` (`node.brewlet.sh/v2alpha1`); placement remains `NodeProfile`.

```yaml
apiVersion: node.brewlet.sh/v2alpha1
kind: RuntimeImage
metadata:
  name: python-3.12            # the runtime image ID
spec:
  source:
    image: registry.example.com/golden/python@sha256:<64 hex>   # index or manifest
  description: CPython 3.12 on hardened Debian 12, glibc ABI
  defaults:
    user: 10001                # numeric; applied by admission when the pod sets none (§8.4)
  compatibility:               # informational; drives docs and tooling, not matching
    ecosystem: python
    abi: glibc-2.36
status:
  generation: 7                # increments on every digest change
  digest: sha256:<64 hex>
  conditions: [ { type: Approved, status: "True" } ]
```

- `metadata.name` **is** the ID. It MUST be an RFC 1123 DNS label of at most 55
  characters so that `brewlet.sh/runtime.<id>` is a valid label key (§9).
- `spec.source.image` MUST be a canonical `repo@sha256:<64 lowercase hex>`
  reference, subject to the existing source policy and mirror allowlist (SPEC §5.3).
  Tags are rejected.
- Changing `spec.source.image` is a **rotation**: the controller increments
  `status.generation` and every node that offers the ID converges to the new
  generation (§7.3). Changing any other identity-bearing field requires a new ID.
- Creating, updating, or deleting `RuntimeImage` is the approval act; it SHOULD be
  restricted to platform administrators through RBAC. Validating admission applies
  the same checks as the reconciler, which remains the enforcement point.

### 7.2 Golden image requirements

A runtime image MUST be a valid OCI image whose platforms cover every node
architecture of the pools that offer it. Its **config** contributes:

- `Env` — the base environment, including the authoritative `PATH` (§8.3);
- nothing else. `Entrypoint`, `Cmd`, `WorkingDir`, `User`, `ExposedPorts`,
  `Volumes`, and `Healthcheck` are ignored; the application artifact and the pod
  own those.

Its root filesystem MUST NOT contain a non-empty `/app`; the shim requires `/app`
to be absent or an empty directory so application content never merges with
runtime content. Runtime images SHOULD be minimal, SHOULD NOT contain setuid
binaries or package managers, and SHOULD carry an SBOM referrer. The provisioner
applies the existing safe-extraction rules (SPEC §5.3) and records
`.brewlet-source` with the resolved digest; the 1.x `.brewlet-java-home` marker is
removed.

**An ID is a compatibility promise.** Rotating the digest behind an ID MUST
preserve everything applications built against that ID depend on: language
minor/feature line, C library and ABI, `PATH` layout, and the presence of
documented tools. A change that breaks any of these — a new major runtime, a
libc switch from glibc to musl, a changed interpreter path — MUST be published as
a new ID. This is the contract that makes digest-free workload references safe.

### 7.3 Placement and rotation (`NodeProfile`)

`NodeProfile` keeps pool selection, tolerations, mirrors, and rollout policy, and
replaces `jdks`, `launchers`, and `appCDS` with one list of runtime image
entries:

```yaml
apiVersion: node.brewlet.sh/v2alpha1
kind: NodeProfile
metadata: { name: web }
spec:
  nodePool: { names: ["web"] }
  runtimeImages:
    - id: java-21                 # install: Eager is the default
    - id: python-3.12
    - id: node-22
      install: OnDemand           # installed on first use on each node (§7.5)
      idleTTL: 72h                # optional; uninstall after 72h without leases
    - id: static
      install: OnDemand
  rollout:
    maxUnavailable: 1
    validate: true
    containerdRestart: validated
```

- `id` is required and unique within the profile. `install` is `Eager`
  (default) or `OnDemand`; any other value is rejected. `idleTTL` is permitted
  only with `OnDemand`.
- Unknown IDs make the profile not ready (`UnknownRuntimeImage`) without blocking
  other IDs.
- `Eager` IDs are installed when the node joins the pool and gate node readiness
  as in 1.x.
- The provisioner installs each generation under
  `/opt/brewlet/runtimes/<id>/<generation>/` read-only and atomically flips
  `/opt/brewlet/runtimes/<id>/current` once extraction and validation succeed.
- **New containers** always resolve the `current` generation at `Create`.
  **Running containers** keep the generation they started on; the shim holds a
  lease on it for the task lifetime. A non-current generation is reclaimed once it
  has no leases and has aged past the grace period. This closes the 1.x roadmap
  item on reference-counted root collection for the general case.
- Validation (`rollout.validate: true`) executes no workload code; it checks the
  extracted root (`/app` empty, `PATH` entries present, required platforms
  covered). Image-specific smoke tests remain the image pipeline's job.

### 7.4 Pinning without digests

Workloads never name a digest. A team that needs to hold a specific generation
while others move forward asks the platform team for a **separate ID** (for
example `java-21-legacy-tls`) whose digest the platform team chooses to freeze.
Holding is therefore an explicit, inventory-visible, administrator-owned decision
rather than a digest scattered across application manifests.

### 7.5 On-demand installation

A large inventory installed eagerly on every node costs provisioning time, disk,
and registry egress for runtimes most nodes never run. An `OnDemand` entry
defers installation of an ID on each node until a container on that node first
needs it.

**Per-node states.** For each `OnDemand` ID the provisioner tracks one of:

| State | Capability label `brewlet.sh/runtime.<id>` | Meaning |
|---|---|---|
| `available` | `available` | Approved for the node's pool; not installed. |
| `installing` | `available` | An install request is being processed. |
| `ready` | `ready` | A validated `current` generation is installed. |
| `failed` | *(absent until backoff expires)* | The last install attempt failed; reported as a `provision-error`. |

`available` is derived from the profile alone, so a node is labelled as soon as
it joins the pool and before any runtime bytes are fetched. Cluster autoscaler
node templates can therefore declare `available` labels statically and scale a
pool from zero for an `OnDemand` ID.

**Install request.** When `Create` (§8.2) resolves an ID that is `available` on
the node, the shim records an install request and does not fetch anything
itself:

1. The shim writes an empty marker `/opt/brewlet/runtimes/.requests/<id>` in a
   root-owned, host-only directory that is never mounted into a sandbox. The
   marker carries no data other than the already-validated ID in its name;
   creating it is idempotent, so concurrent containers produce one request.
2. The provisioner — still the only writer of runtime roots — accepts the
   request only if `<id>` is an `OnDemand` entry of the node's current profile
   and its `RuntimeImage` is approved; any other marker is deleted and logged.
   It installs the ID's `current` generation with the same digest pinning,
   mirror policy, extraction, and validation as an `Eager` install (§7.2,
   §7.3), then updates the label to `ready` and removes the marker.
3. The shim waits for the generation to become `current` for at most a bounded
   interval (default 30 seconds, always below the kubelet runtime request
   timeout). If installation completes, `Create` proceeds normally. Otherwise
   it fails with `RuntimeImageInstalling`; kubelet retries container creation
   with its standard backoff, and a later attempt succeeds once the ID is
   `ready`. No pod is rescheduled and no workload code runs before the runtime
   is fully installed and validated.

**Failure.** A failed install moves the ID to `failed`, removes the capability
label so the scheduler stops placing new pods for that ID on the node, and
retries with exponential backoff, returning to `available` before each retry.
While `failed`, `Create` returns `RuntimeImageInstallFailed` without issuing a
new request.

**Rotation.** Once installed, an `OnDemand` ID follows the same rotation and
lease rules as an `Eager` ID (§7.3). Rotating a `RuntimeImage` never installs it
on nodes where it is still `available`.

**Idle reclamation.** With `idleTTL` set, an installed `OnDemand` ID whose
generations have held no shim lease for `idleTTL` is uninstalled and returns to
`available`. Without `idleTTL`, it stays installed until removed from the
profile.

**Profile changes.** Switching an entry from `OnDemand` to `Eager` installs it
on every node in the pool. Switching from `Eager` to `OnDemand` leaves existing
installations in place as `ready` (subject to `idleTTL`) rather than removing
them.

## 8. Workload contract

### 8.1 Pod selection

A pod opts in with `runtimeClassName: brewlet` and MUST carry exactly one
runtime image ID:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata: { name: orders }
spec:
  template:
    metadata:
      annotations:
        brewlet.sh/runtime-image: java-21    # ID only — no tag, no digest
    spec:
      runtimeClassName: brewlet
      containers:
        - name: orders
          image: registry.example.com/apps/orders@sha256:<64 hex>
          args: ["-Xmx768m", "-jar", "/app/orders.jar"]   # optional override of launch args
```

- The annotation value MUST match the ID grammar; values containing `@`, `:`, or
  `/` are rejected (`InvalidRuntimeImageID`). There is **no default**: a missing
  annotation is denied by admission and failed by the shim
  (`RuntimeImageRequired`).
- The annotation is pod-wide. Every container of the pod that the shim executes
  uses the same runtime image. Per-container selection is an open question (§15).
- The application `image:` MUST be digest-pinned, exactly as in 1.x (SPEC §4.4); the
  shim's CRI identity checks are unchanged.
- Init and sidecar containers that are ordinary images MAY be present; as in 1.x,
  the shim runs them through the standard runc path when their image is not a
  Brewlet 2.0 application artifact.

### 8.2 Sandbox assembly (shim `Create`)

1. **Resolve the artifact** exactly as SPEC §6.4 does today (protected CRI metadata,
   content-store digest verification, strict platform match). Recognize a 2.0
   artifact by `brewlet.sh/artifact-version=2` and a final `brewlet.sh/layer=launch`
   layer; anything else that is not a plain image fails with `UnsupportedArtifact`.
2. **Load and validate** `launch.json` (§6.3) and the image-config mirror (§6.4).
3. **Resolve the runtime image** from the pod annotation, carried into the OCI
   runtime spec annotations by CRI. Fail with `RuntimeImageIncompatible` if
   `requires.runtimeImages` excludes it. If the ID is `available` on this node,
   request installation and wait as described in §7.5 (`RuntimeImageInstalling`
   or `RuntimeImageInstallFailed` on timeout or failure). Fail with
   `RuntimeImageNotInstalled` if the node's profile does not offer the ID at all.
   Take a lease on the resolved `current` generation.
4. **Stage and validate layers** (§6.2) from the content store into the existing
   verified per-digest stage.
5. **Assemble the rootfs** as an overlay: `lowerdir` = launch-layer-excluded
   application and dependency layers (top to bottom in reverse manifest order)
   over the runtime root; `upperdir`/`workdir` = per-container scratch. The CRI
   `readOnlyRootFilesystem` flag is preserved as in 1.x.
6. **Generate `config.json`** from the CRI-provided spec with the process fields
   of §8.3 and the user of §8.4; resources, mounts, namespaces, and the CNI
   network namespace pass through unchanged. No language-specific arguments are
   injected.
7. **Delegate to runc.** Signals, exit codes, logs, probes, and `kubectl exec`
   behave as in SPEC §6.2.

### 8.3 Process fields

- **argv and working directory** are taken verbatim from the CRI-provided spec,
  which already reflects Kubernetes `command`/`args`/`workingDir` over the
  mirrored image config.
- **Environment** is merged in increasing precedence: runtime image `Env`, then
  the CRI-provided environment (image config mirror of `process.env`, then pod
  `env`/`envFrom`, then Kubernetes service variables).
- **`PATH` is runtime-owned.** The shim always uses the runtime image's `PATH`
  and discards any `PATH` from CRI, because containerd injects a default `PATH`
  that is indistinguishable from an explicit one and because executable lookup
  must be stable across rotations. Admission emits a warning when a pod sets
  `PATH`. Applications add directories through ecosystem variables or
  absolute `entrypoint` paths.

### 8.4 User identity

The application image config `User` is empty, so CRI derives the process user
from the pod `securityContext` alone. When a pod sets no `runAsUser`, admission
injects `RuntimeImage.spec.defaults.user` (if declared) into the container
`securityContext`. Because admission is fail-open, clusters that require
non-root execution SHOULD additionally enforce Pod Security `restricted`; the
shim does not invent a user.

## 9. Admission and scheduling

For every `runtimeClassName: brewlet` pod on CREATE, `brewlet-admission`:

- **Validates** `brewlet.sh/runtime-image` (presence, grammar) and that a
  `RuntimeImage` with that ID exists and is approved; denial reasons
  `RuntimeImageRequired`, `InvalidRuntimeImageID`, `UnknownRuntimeImage`.
- **Steers** scheduling by injecting a required `nodeAffinity` term on the
  capability label `brewlet.sh/runtime.<id>` `In ["ready", "available"]` and a
  preferred term (weight 100) on `In ["ready"]`, so pods land on nodes that
  already have the runtime installed whenever one fits and otherwise trigger an
  on-demand install (§7.5). Admission denies with `NoCompatibleRuntime` when no
  node carries either value for the ID.
- **Defaults** the container user (§8.4) and **overwrites** the existing
  `brewlet.sh/artifact-*` compatibility hints (SPEC §8.3, unchanged).
- **Does not** read the application artifact from the registry. Artifact-side
  constraints (`requires.runtimeImages`, layer rules, platforms) are enforced by
  the shim and by kubelet's pull, so fail-open admission never weakens them.

The 1.x `brewlet.sh/jdk`, `brewlet.sh/launcher`, `brewlet.sh/jvm-args`,
`brewlet.sh/cds-regenerate`, and `brewlet.sh/arch` annotations are removed and
MUST be rejected on a 2.0 pod (`UnsupportedAnnotation`) so a stale manifest
fails loudly instead of silently losing its intent.

### 9.1 Capability labels

The provisioner publishes, per node:

| Label | Value | Meaning |
|---|---|---|
| `brewlet.sh/runtime` | `ready` | Unchanged; `RuntimeClass/brewlet` scheduling selector. |
| `brewlet.sh/runtime.<id>` | `ready` | The node has a validated `current` generation of `<id>`. |
| `brewlet.sh/runtime.<id>` | `available` | `<id>` is an `OnDemand` entry of the node's profile and is not yet installed (§7.5). |

The label value is deliberately not the digest or generation: during a rotation,
a node still converging remains schedulable for the ID because the previous
generation honors the same compatibility promise (§7.2). Generation detail is
published in the node annotation `brewlet.sh/runtime-images` as JSON
`{ "<id>": { "install": "OnDemand", "state": "ready", "generation": 7, "digest": "sha256:…" } }`,
where `state` is one of the §7.5 states (`ready` for an installed `Eager` ID).

`jdk.*`, `jdk-feature.*`, `launcher.*`, and `appcds-regeneration` labels are
removed. The [capability label contract](../CAPABILITY_LABELS.md) gains a v2 key
family and its dual-publication exception does not extend to 1.x keys.

## 10. `RuntimeClass`

`RuntimeClass/brewlet` is unchanged (SPEC §7): one handler, one scheduling selector,
one overhead. Runtime image selection is per pod rather than per RuntimeClass
(see §16 for the rejected per-ID RuntimeClass alternative). `overhead.podFixed`
SHOULD be revisited, since 1.x sized it as "JVM/runtime baseline"; in 2.0 it
covers only shim and staging overhead.

## 11. What happens to Java

Removed from Brewlet core:

- JDK and launcher inventory, `javaHome`, the `/opt/jdk` alias, launcher
  packages such as `jaz` as a separate node package;
- the 1.x launch descriptor (`mainJar`, `entry`, `addOpens`, …) and its argv
  expansion rules, including the `jvm.args` entrypoint-selector guard;
- AppCDS node-side regeneration, its cache, sentinel, and capability label;
- the JVM resource mapping of SPEC §10 (modern JVMs are container-aware);
- the `JavaApplication` CRD and controller, and `status.selectedJdk`.

Re-expressed generically:

| 1.x concept | 2.0 equivalent |
|---|---|
| JDK `temurin-21` in `NodeProfile.jdks` | `RuntimeImage java-21` built from the same digest-pinned JDK image |
| Launcher `jaz` | A runtime image whose `PATH` provides `jaz` (for example `java-21-jaz`); the application's `entrypoint` names it |
| `jvm.config` | `launch.json` argv generated by the Maven plugin |
| Classpath / module-path layers | Dependency layers under `/app/lib` or `/app/mods`, referenced from argv |
| Managed dependency bundle | `java`-ecosystem layer set (§6.5) |
| Shipped AppCDS archive | An ordinary file in an application layer plus `-XX:SharedArchiveFile=… -Xshare:auto` in argv. Because rotations change the exact JDK build, the archive is best-effort by design. |
| `JavaApplication` | A plain Deployment; ecosystem-specific CRDs are out of scope (§15) |

The **Maven plugin survives as a producer**: it builds `launch.json` from the
project (main class, JPMS module, preview flags, `--add-opens`), splits
dependencies into layers or consumes a published layer set, optionally emits an
AppCDS archive, records `requires.runtimeImages` from configuration, and pushes a
2.0 artifact. It no longer generates Kubernetes manifests. The `brewlet` CLI
becomes the generic producer for every other ecosystem:

```bash
brewlet push registry.example.com/apps/api \
  --layer app=./src:/app/src \
  --layer dependency=./venv/lib/python3.12/site-packages:/app/site-packages \
  --requires-runtime python-3.12 \
  --entrypoint python -- -m api
```

## 12. Security model

- **Trust roots.** The only executable bytes outside `/app` are runtime roots
  extracted from administrator-approved, digest-pinned `RuntimeImage` sources.
  The application image is verified by digest exactly as in 1.x.
- **Unshadowable runtime.** §6.2 confines application and dependency content to
  `/app`, and §7.2 requires the runtime's `/app` to be empty, so remediation of a
  runtime CVE cannot be undone by an application layer.
- **Digest-free workloads are not trust-free.** The ID is resolved only from
  node-local state written by the provisioner from the approved inventory; the pod
  annotation selects among approved roots but cannot introduce one. A mutable tag
  never participates.
- **Rotation is a privileged act.** Updating a `RuntimeImage` changes code under
  every workload using the ID on the next container start. RBAC on
  `RuntimeImage`, signature verification of golden images (a natural extension of
  the existing Ratify example), and audit logging SHOULD be treated accordingly.
- **On-demand requests cannot widen the inventory.** A container can only cause
  installation of an ID that its node's profile already lists as `OnDemand` and
  whose `RuntimeImage` is approved. The request channel is a root-owned,
  host-only directory, the provisioner validates every request against its
  profile, and installation uses the same digest-pinned source and validation as
  an eager install. The worst a workload can do is install, earlier than it
  otherwise would be, a runtime the administrator already approved for the pool;
  `idleTTL` bounds how long such an install occupies disk without use.
- **Unchanged boundaries.** The privileged provisioner, host-path layout under
  `/opt/brewlet`, fail-open admission, and runc isolation retain their 1.x
  properties and caveats (SPEC §11).

## 13. Migration from 1.x

2.0 is a breaking release. The supported path is:

1. Teardown 1.x per the uninstallation guide.
2. For each `NodeProfile.jdks` entry, create a `RuntimeImage` from the same
   digest; for each launcher, publish a runtime image that includes it.
3. Republish applications with the 2.0 Maven plugin or CLI. A
   `brewlet migrate` command MAY convert a 1.x runnable image to a 2.0 artifact by
   reusing its layers byte-for-byte, relocating them under `/app` when needed,
   and rendering `launch.json` from `brewlet.sh/jvm-config` using the 1.x
   expansion order.
4. Replace `JavaApplication` resources with Deployments carrying
   `brewlet.sh/runtime-image`.

## 14. Observability

- Shim event `RuntimeImageResolved` on each container start with ID, generation,
  and digest; the same values are exported as
  `brewlet_container_runtime_image_info{id,generation,digest}`.
- `brewlet_runtime_image_generations{id,state}` on each node, and a controller
  condition `RotationProgressing` / `RotationComplete` per `RuntimeImage`.
- Operators answer "which pods still run the vulnerable generation?" from the
  info metric, since the pod spec intentionally does not record a digest.
- Provisioner metrics `brewlet_runtime_image_install_requests_total{id,outcome}`
  and `brewlet_runtime_image_install_duration_seconds{id}` for on-demand
  installs, and a container event `RuntimeImageInstalling` when `Create` waits
  for one.
- New reason codes: `RuntimeImageRequired`, `InvalidRuntimeImageID`,
  `UnknownRuntimeImage`, `NoCompatibleRuntime`, `RuntimeImageNotInstalled`,
  `RuntimeImageInstalling`, `RuntimeImageInstallFailed`,
  `RuntimeImageIncompatible`, `UnsupportedArtifact`, `LaunchConfigMismatch`,
  `LayerPathViolation`, `UnsupportedAnnotation`.

## 15. Open questions

1. **Restart policy on rotation.** Should the operator offer opt-in rolling
   restarts of workloads whose containers run a non-current generation, or is
   that left to existing tooling once the info metric exists?
2. **Per-container runtime images.** Is a per-container annotation form
   (`brewlet.sh/runtime-image.<container>`) worth the complexity for multi-language
   pods, or should those pods mix one Brewlet container with ordinary images?
3. **Generic workload CRD.** Is there enough value in a language-neutral
   `Application` resource, or are Deployments plus Helm/Kustomize sufficient?
4. **Default user.** Should a runtime image's declared default user also be
   enforced by the shim when admission is bypassed, at the cost of the shim
   distinguishing an explicit `runAsUser: 0`?
5. **Artifact-declared runtime image.** Should `requires.runtimeImages` with a
   single entry let admission default the pod annotation? Doing so requires
   admission to read registries, which 1.x deliberately avoids.
6. **Prefix flexibility.** Is `/app` sufficient for every ecosystem, or do some
   (for example, tools expecting `/opt/<name>`) need a declared, still
   runtime-disjoint, set of application prefixes?
7. **Baked export.** Should [proposal 0007](0007-baked-golden-image-delivery.md)'s
   bake become the standard fallback for clusters without the shim, using the
   same `RuntimeImage` inventory and `launch.json`?

## 16. Alternatives considered

### One RuntimeClass per runtime image ID

`runtimeClassName: brewlet-python-3-12` with a per-class node selector would
remove the pod annotation and let the scheduler, rather than admission, route
pods. It was rejected for the default design because every inventory change
would create or delete cluster-scoped RuntimeClass objects, the RuntimeClass
handler would still need the ID out of band, and policies keyed on
`runtimeClassName: brewlet` would fragment. It remains viable as an operator
convenience layered on top of the annotation.

### Launch configuration in a manifest annotation or config blob

1.x runnable images use the `brewlet.sh/jvm-config` annotation. A dedicated
layer was preferred so the configuration is a first-class, content-addressed
blob with its own digest for referrers and tooling, and so the same artifact
shape serves registry-native and kubelet-pulled use. An annotation is equally
covered by the manifest digest, so this is a preference rather than a security
property.

### Digest in the pod annotation

Allowing `brewlet.sh/runtime-image: python-3.12@sha256:…` would give
reproducibility at the cost of the central value of the design: runtime
remediation without touching every workload. §7.4's dedicated IDs provide holds
without scattering digests.

### Allow application layers anywhere

Treating application layers like any image layer would support more existing
images, but would let an application shadow runtime files and silently defeat a
rotation. Confinement to `/app` is the property that makes digest-free runtime
references trustworthy.

### Shim-pulled dependency artifacts

Letting `launch.json` reference independently published dependency artifacts that
the shim pulls at `Create` would avoid copying layers into each application
image. It was rejected because the shim would need registry credentials outside
kubelet's image-pull path, cold starts would gain a second pull, and digest
reuse of layer sets (§6.5) already delivers the storage and governance benefits.

## Appendix — specification integration points

On acceptance, `SPECIFICATION.md` would be revised as follows:

| SPECIFICATION section | Change | Source in this proposal |
|---|---|---|
| SPEC §1–§3 | Reframe as language-agnostic | §1–§3 |
| SPEC §4 | Replace | §6 |
| SPEC §5.3–§5.4 | Replace | §7.1–§7.2 |
| SPEC §5.6 | Update `NodeProfile`, add on-demand install | §7.3, §7.5 |
| SPEC §6.1 | Replace `Create` lifecycle | §8.2–§8.4 |
| SPEC §7 | Retain; revisit overhead | §10 |
| SPEC §8.2, §9 | Remove `JavaApplication` | §11 |
| SPEC §8.3 | Replace admission rules | §9 |
| SPEC §10 | Remove JVM resource mapping | §11 |
| SPEC §13 | Move AppCDS guidance to Maven plugin docs | §11 |
| SPEC §14 | Update reason codes, annotations, host layout | §7.3, §14 |
| [CAPABILITY_LABELS](../CAPABILITY_LABELS.md) | New v2 key family | §9.1 |
