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
  `python-3.12` once; new containers pick up the patched runtime as their nodes
  converge, without touching application repositories or manifests.
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
  probes, volumes, and resources retain their normal override behavior.
  Intentional restrictions are runtime-owned `PATH`, non-root identities and
  no privilege escalation, synthesized `HOME`, and read-only packaged `/app`
  content (§8.2–§8.4).
- Guarantee that application layers cannot replace platform-owned runtime
  file paths; this does not patch application-bundled copies of libraries.
- Allow dependencies to be published and governed **independently** from
  applications while still being delivered as ordinary image layers.
- Let platform teams approve large runtime inventories without installing every
  runtime on every node, by installing selected runtimes on first use.
- Keep the `brewlet` CLI language-agnostic: it packages and validates
  hand-written descriptors and never derives launch or platform fields from
  application content (§11.1).
- Keep the Maven plugin as an optional **Java producer** of 2.0 artifacts, the
  only place where Java-specific intelligence lives (§11.2).

## 4. Non-goals

- Compatibility or migration tooling for previous Brewlet releases. 2.0 is a
  total pivot; previous artifacts, descriptors, and deployments are outside
  this proposal's contract.
- Per-workload digest pinning of the runtime. A workload that must pin an exact
  runtime digest should use an ordinary image with the stock runc RuntimeClass, or
  a dedicated, narrowly scoped runtime image ID (§7.4).
- Language-aware tuning. Brewlet injects no interpreter, VM, or GC flags; runtimes
  that need cgroup-aware tuning MUST obtain it from the runtime image itself.
- Building runtime images. Golden images are produced by the platform team's
  existing image pipeline; Brewlet only approves, distributes, and composes them.
- A generic workload CRD. `JavaApplication` is removed and is not replaced by a
  language-neutral equivalent in 2.0; Deployments, StatefulSets, and DaemonSets
  are the workload API.
- Per-container runtime images. A pod uses exactly one runtime image (§8.1).
- Running workloads as root (§8.4).
- Inferring the runtime image from the artifact. Admission never reads the
  registry and never fills in `brewlet.sh/runtime-image`; the workload manifest
  always states it explicitly (§9).
- Language detection in generic tooling. The CLI does not guess entrypoints,
  dependency layouts, or runtime IDs for any ecosystem.
- Sandboxes other than runc. [Proposal 0006](0006-sandbox-isolation-tiers.md)
  remains the venue for stronger isolation.

## 5. Terminology

| Term | Meaning |
|---|---|
| **Runtime image** | An administrator-approved OCI image (or image index) that supplies the complete userland and language runtime a workload executes on. Also called a *golden image*. |
| **Runtime image ID** | The stable name of a runtime image in the inventory, for example `java-21` or `python-3.12`. Never contains a digest or tag. |
| **Generation** | One digest bound to an ID within a `RuntimeImage` resource UID (the inventory epoch). Rotating the digest creates a new generation within that epoch. |
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
Producers require explicit platform declarations (§11.1); content neutrality
is an author assertion, never inferred by the CLI.

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
- regular files that are not readable by others, directories that are not
  readable and searchable by others, or files with any execute bit but no
  execute bit for others (arbitrary non-root identities are supported, §8.4);
- extended attributes, including `security.capability`;
- whiteouts or opaque-directory markers that target anything outside `app/`.

OCI whiteout/opaque tar markers are metadata, not packaged executable files:
their standardized empty-file encoding is validated separately and is exempt
from regular-file readability rules. Device entries in input tars remain
forbidden; privileged overlay whiteout representations may only be produced
internally by the validated materializer.

These rules protect **runtime-owned file paths**: no application or dependency
layer can replace `/usr`, `/etc`, or `/lib`. Applications can still bundle their
own libraries under `/app` or change language/library lookup through environment
variables. Runtime rotation does not remediate those copies; the application
team must rebuild them. This is file-path confinement, not a guarantee about
every library a process loads.
Violations fail the container with reason `LayerPathViolation`.

Layers stack in manifest order, so later application layers may overwrite
earlier dependency files under `/app`. Producers SHOULD order layers from least to
most frequently changed (dependency layers first) to maximize reuse.
Safe relative symlinks are allowed; absolute symlinks, including a virtualenv
link to `/usr/bin/python`, are not. Python producers can package site-packages
and use the golden image's interpreter instead of shipping such a virtualenv.

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

The allow-list is deliberately strict and name-based, not a language capability
matcher. Composed, hold, and variant IDs must be explicitly listed even if they
provide the same language version. Adding such an ID requires republishing the
launch layer/config, not rebuilding unchanged application/dependency layers.
Authors wanting inventory-independent artifacts omit the optional allow-list
and accept responsibility for testing their explicitly selected workload ID.
Admission cannot precheck this constraint without registry access; mismatches
are reported at container creation.

The launch configuration intentionally contains **no** language-specific fields.
Everything a 1.x `jvm.config` expressed (`mainJar`, `entry.mode`, `addOpens`,
system properties, preview flags, AppCDS hints) becomes plain argv. Outside the
Java ecosystem, the application author writes `launch.json` by hand and the CLI
packages it unchanged (§11.1); for Java projects the Maven plugin MAY generate it
(§11.2).

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

**Canonical mirror.** Missing optional `args` and `env` render as empty arrays;
absent or null config arrays compare as empty arrays. `entrypoint` remains
non-empty, and a missing launch `workingDir` renders as `/app` (an empty config
`WorkingDir` is not equivalent). Array order and strings are compared exactly,
including environment order; duplicate environment names are rejected.
Config `User` is absent or empty. `ExposedPorts`, `Volumes`, `Healthcheck`, and
`StopSignal` MUST be absent or empty: they must not introduce behavior outside
the launch contract. Other labels are informational and are ignored for mirror
comparison. OCI `architecture`, `os`, `variant`, and `rootfs.diff_ids` must match
the declared platform and verified layers. Non-execution metadata such as
`created`, `author`, and `history` is ignored.

The CLI, Maven plugin, and shim MUST share published conformance fixtures for
valid and invalid descriptors, canonical mirrors, layer paths and links,
permissions, whiteouts, and platform declarations. Acceptance requires all
three implementations to produce the same normalized process fields and
reason codes; separate language implementations do not justify contract drift.

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
  user:                        # the image's non-root `app` account (§8.4); immutable
    uid: 1000
    gid: 1000
  compatibility:               # informational; drives docs and tooling, not matching
    ecosystem: python
    abi: glibc-2.36
status:
  generation: 7                # increments on every digest change
  digest: sha256:<64 hex>
  conditions: [ { type: Approved, status: "True" } ]
```

- `metadata.name` **is** the ID. It MUST match
  `[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*`
  and be at most 55 characters: lowercase DNS subdomain components, allowing
  dots as in `python-3.12`, with no empty component. This also makes
  `brewlet.sh/runtime.<id>` a valid label key (§9). Admission, the shim, CLI,
  and Maven plugin use the same grammar.
- `spec.source.image` MUST be a canonical `repo@sha256:<64 lowercase hex>`
  reference, subject to the existing source policy and mirror allowlist (SPEC §5.3).
  Tags are rejected.
- `spec.user.uid` and `spec.user.gid` are required, MUST be non-zero, and are
  immutable: they identify the image's `app` account (§7.2, §8.4) and are part of
  the ID's compatibility promise.
- Changing `spec.source.image` is a **rotation**: the controller increments
  `status.generation` monotonically within `metadata.uid`; recovery must never
  reuse a generation for a different digest. Nodes that have the ID installed
  converge to the new generation (§7.3). Changing any other identity-bearing
  field requires a new ID.
- Creating, updating, or deleting `RuntimeImage` is the approval act; it SHOULD be
  restricted to platform administrators through RBAC. Validating admission applies
  the same checks as the reconciler, which remains the enforcement point.

**Revocation and inventory epochs.** `metadata.uid` is the inventory epoch,
persisted with every generation and restart barrier. Deleting and recreating
the same name creates a different epoch; a barrier or root from the old epoch
MUST NOT authorize a start against the new resource. Generations are never
compared across epochs.

Deleting the resource, setting `Approved=False`, or making its source invalid
revokes authorization for new starts. The operator withdraws its capability
labels and the provisioner atomically publishes a revoked local authorization
record. The shim checks that record on every `Create` and rejects with
`RuntimeImageRevoked`; an offered ID or existing root alone is not approval.
Already-running tasks retain their roots until leases drain; revocation does
not kill them. A deletion finalizer retains UID-bound inventory evidence until
labels are withdrawn, local authorizations expire or are revoked, and root
cleanup finishes. Re-creation never adopts old-epoch roots.

Node-local authorization has a short expiry (default `5m`, renewed by the
provisioner only after fresh inventory and ownership checks). An unreachable
node cannot retain approval indefinitely: expired authorization makes new
starts fail closed with `RuntimeImageAuthorizationExpired`, even when
admission fails open. Revocation propagation is bounded by this expiry, not
instantaneous. Operators may shorten it at the availability cost of blocking
new starts during control-plane outages. Provisioner restart cannot extend an
authorization without rechecking the cluster.

### 7.2 Golden image requirements

A runtime image MUST be a valid OCI image whose platforms cover every node
architecture of the pools that offer it. Its **config** contributes:

- `Env` — the base environment, including the authoritative `PATH` (§8.3);
- nothing else. `Entrypoint`, `Cmd`, `WorkingDir`, `User`, `ExposedPorts`,
  `Volumes`, and `Healthcheck` are ignored; the application artifact and the pod
  own those.

The selected config MUST provide exactly one `PATH` with non-empty absolute
directories outside `/app`, no `.` or empty component, and no duplicate
environment names. Validation verifies those directories against the root.

Its root filesystem MUST NOT contain a non-empty `/app`; the shim requires `/app`
to be absent or an empty directory so application content never merges with
runtime content. `/app` is the only application prefix for every ecosystem.

Its root filesystem MUST define a non-root account named **`app`** in
`/etc/passwd` (and its primary group in `/etc/group`) whose UID and GID equal
`RuntimeImage.spec.user`, with an existing home directory. The provisioner
verifies this during validation and refuses a generation that does not match.
Runtime roots MUST NOT contain setuid/setgid binaries or file capabilities;
the provisioner rejects such roots rather than silently changing approved
bytes. Runtime images SHOULD be minimal, SHOULD NOT contain package managers,
and SHOULD carry an SBOM referrer. The provisioner
applies the existing safe-extraction rules (SPEC §5.3) and records
`.brewlet-source` with the resolved digest; the 1.x `.brewlet-java-home` marker is
removed.

Alongside each root the provisioner persists immutable, root-owned metadata:
inventory ID and UID, generation, source and platform manifest digests,
selected OCI platform/config `Env`, and the validated `app` UID/GID/home.
The `current` pointer publishes this metadata and its root as one unit.
Mutable authorization/expiry and revocation records are separately written
atomically by the fenced provisioner; the shim consumes trusted local records,
not pod-supplied environment or user data, as the runtime authority.

**Deriving from third-party images.** Vendor images rarely meet these rules out
of the box. A platform team customizes them in its own image pipeline and
approves the result, not the vendor image. For example, a golden image based on
the Oracle JDK container image adds the `app` account, removes any content under
`/app`, strips setuid/setgid bits, and optionally removes package managers and
shells before being pushed and pinned by digest:

```dockerfile
FROM container-registry.oracle.com/java/jdk:21
RUN groupadd --gid 1000 app \
 && useradd --uid 1000 --gid app --home-dir /home/app --create-home --shell /sbin/nologin app \
 && rm -rf /app \
 && find / -xdev -perm /6000 -type f -exec chmod ug-s {} +
```

Brewlet does not build or modify runtime images; licensing and redistribution
terms of the base image remain the platform team's responsibility.

**An ID is a compatibility promise.** Rotating the digest behind an ID MUST
preserve everything applications built against that ID depend on: language
minor/feature line, C library and ABI, `PATH` layout, the `app` account, and the
presence of documented tools. A change that breaks any of these — a new major runtime, a
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
  `/opt/brewlet/runtimes/<id>/<inventory-uid>/<generation>/` read-only and atomically flips
  `/opt/brewlet/runtimes/<id>/current` once extraction and validation succeed.
- **New containers** always resolve the `current` generation at `Create`.
  **Running containers** keep the generation they started on; the shim holds a
  durable, root-owned lease record binding the container/task identity to the
  inventory UID and generation for the task lifetime. Lease acquisition,
  `current` resolution, and collection share a per-ID lock. Shim restart
  recovers leases from durable records and live task/mount evidence; uncertainty
  blocks collection. A lease is released only after the task, exec processes,
  and rootfs mounts are gone. A non-current generation is reclaimed once it
  has no leases and has aged past the grace period. This closes the 1.x roadmap
  item on reference-counted root collection for the general case.
- Provisioning configures containerd's runtime-handler pod-annotation
  allow-list to forward `brewlet.sh/runtime-image` and
  `brewlet.sh/rotated-to-generation` into each application container's OCI
  spec. Annotation propagation is not assumed from arbitrary pod metadata.
  Readiness validation checks this configuration; missing runtime selection
  fails closed, and an end-to-end barrier test covers initial and replacement
  pods.
- Required security and identity validation cannot be disabled.
  Validation executes no workload code; it checks the
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

`available` is derived from the approved inventory and valid profile/ownership
authority, so a node is labelled as soon as it joins the pool and before any
runtime bytes are fetched. Revoked or expired authorizations never advertise
`available` or `ready`. Cluster autoscaler
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
   and its `RuntimeImage` is approved; an unauthorized marker is deleted and
   logged only after the provisioner has verified its ownership authority.
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

**Ownership fencing.** On-demand processing MUST use the same exclusive
ownership and claim-fencing protocol as eager provisioning (SPEC §5.6); it is
not an independent writer authority. Before any host mutation, the worker
MUST verify the Node UID, the UID-bound node claim, its literal
`BREWLET_PROFILE_UID`, and durable provisioning authority for the current
profile UID and generation. The profile MUST not be deleting or retiring this
target. The worker rechecks those fences immediately before flipping `current`
and publishing `ready`, and verifies that the ID remains authorized and the
approved digest/generation has not changed.

A failed initial fence leaves host state, markers, and node advertisements
untouched and reports `ownership-fence-failed`. Losing authority during an
installation stops publication and further host mutation; staged data is left
for the authorized provisioning or retirement worker to reconcile. Retargeting
and deletion stop and join on-demand workers before cleanup or claim release.
Markers confer no authority and MUST be revalidated by any successor worker,
never replayed using a retired profile's configuration.

**Failure.** A failed install moves the ID to `failed`, removes the capability
label so the scheduler stops placing new pods for that ID on the node, and
retries with exponential backoff, returning to `available` before each retry.
While `failed`, `Create` returns `RuntimeImageInstallFailed` without issuing a
new request.
Ownership-fence failures instead follow the non-mutating rules above; approval
revocation follows §7.1 rather than retrying an unauthorized installation.

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

### 7.6 Opt-in rolling restart on rotation

A rotation reaches running containers only when they restart (§7.3). Workloads
that want a patched runtime promptly opt in to an operator-driven rolling
restart.

**Enablement.** The feature has two opt-ins:

1. **Cluster:** the chart value `operator.rotationRestarts.enabled` (default
   `false`) grants the operator `get`/`list`/`watch`/`patch` on Deployments,
   StatefulSets, and DaemonSets, plus read access to their Pods for restart
   selection and rollout observation. Without it the operator has no such
   workload-patching RBAC.
2. **Workload:** the annotation `brewlet.sh/restart-on-rotation: "true"` on the
   Deployment, StatefulSet, or DaemonSet object (not its pod template).

**Trigger and participating nodes.** A rotation tracks the approved target
generation and digest. Participants are live, `Ready=True`, schedulable,
non-deleting nodes with a valid profile ownership claim that offers the ID,
either as `Eager` or as an already-installed `OnDemand` entry. An installed
`OnDemand` entry remains a participant while updating or retrying a failed
rotation; changing its state from `ready` does not silently remove it.
Uninstalled `OnDemand` entries do not participate and are not installed by a
rotation.

The operator sets `RotationComplete=True` and `status.rotationCompletedAt`
when all participants report the target generation as validated and `current`.
The condition records the target generation and digest; it means completion
on participating nodes, not that every node or running container is patched.
Node deletion, readiness, cordon, ownership, and installation changes cause
reconciliation of the participant set. A newer target invalidates completion
for the previous target. The completion timestamp is recorded once per target,
not rewritten on each reconciliation or when an excluded node recovers.

**Excluded nodes and recovery.** NotReady, cordoned, deleting, or retiring nodes
are excluded and listed in `status.rotation.excludedNodes` with Node UID,
reason, and last observed runtime generation. Their runtime capability label
MUST be withdrawn before completing the rotation. A node whose progress stalls
for `operator.rotationRestarts.nodeProgressDeadline` (default `10m`, measured
from target assignment if no progress has occurred, otherwise from its last
target-generation progress) reports `RotationStalled` and is
quarantined for this ID by withdrawing the capability label. Once withdrawal
is confirmed, it is recorded as excluded rather than blocking all healthy
nodes indefinitely. If withdrawal cannot be confirmed, completion remains
blocked and reports the node and error; expiry alone is not success.

The provisioner MUST NOT republish `ready` for an excluded or returning node
until its ownership fences pass and its validated `current` matches the latest
approved target. An uninstalled `OnDemand` entry can advertise `available` and
install that latest target on demand. Existing containers keep their leases;
quarantine does not terminate them. Status and events MUST retain excluded
nodes and outstanding old-generation containers so partial completion is
visible. Recovery is to restore the node and complete provisioning, or retire
it through the existing fenced cleanup protocol, not bypass validation.

**Selection.** The operator then considers each opted-in workload whose pod
template carries `brewlet.sh/runtime-image: <id>`, and restarts it if at least
one of its running pods started before `status.rotationCompletedAt`. Workloads
created or rolled after completion are therefore left alone.

**Mechanism.** The operator patches the pod template annotation
`brewlet.sh/rotated-to-generation` with a JSON object
`{"id":"java-21","inventoryUID":"<RuntimeImage UID>","generation":7}`.
This is the same template-change mechanism as
`kubectl rollout restart`: the workload's own controller performs the rollout
and honors its update strategy (`maxUnavailable`, `maxSurge`, partitions,
`minReadySeconds`). The annotation also makes the patch idempotent: a workload
whose template already records the same ID, epoch, and current generation is
not patched again. Per-workload status is keyed by workload UID and target
epoch/generation so controller restarts cannot repeat a completed patch.

The annotation is also a **minimum-generation barrier**, propagated by CRI to
the shim. Its ID MUST match the pod's runtime image ID, its inventory UID MUST
match the currently authorized epoch, and its generation MUST be a positive
integer. An epoch mismatch fails with `RuntimeImageEpochMismatch`, never a
numerical comparison or silent barrier removal. For that ID and epoch,
`Create` MUST refuse a local
generation older than the requested generation with `RuntimeImageUpdating`
after waiting for authorized provisioning under §7.5's bounded wait rules,
and let kubelet retry. An uninstalled `OnDemand` entry uses the install marker;
an already-installed entry, whether `Eager` or `OnDemand`, waits for the
provisioner's normal rotation reconciliation rather than creating a first-use
install request. A newer approved generation satisfies the barrier. Malformed
barrier values fail with `InvalidRuntimeGeneration`. This applies even to a pod
already bound to an excluded node or placed without admission; a scheduling
label alone cannot prevent stale starts. The shim never changes `current`
itself. Switching a workload's runtime ID or recreating its inventory resource
requires explicitly removing or updating the barrier. The operator MUST NOT
automatically overwrite an old-epoch barrier as though it were a normal rotation;
it reports the mismatch until the workload owner acknowledges the new epoch.

**Unsupported or deferred rollouts.** A StatefulSet or DaemonSet using
`OnDelete` receives the same template barrier, but the operator reports
`RestartNotApplicable` in
per-workload restart status and emits an event explaining that manual pod
replacement is required. Template changes do not trigger replacement under
`OnDelete`; subsequent manual replacements inherit the safety barrier.
The operator MUST NOT delete pods or change the update strategy.
Paused Deployments and partitioned StatefulSets are reported as
`RestartDeferred` while their controller settings prevent a full rollout; a
template patch is not evidence that their old-generation pods were replaced.
These workloads do not count as completed restarts.

**Pacing.** At most `operator.rotationRestarts.maxConcurrent` workloads (default
`5`) roll at the same time cluster-wide. A slot is freed when the rollout
completes or exceeds `operator.rotationRestarts.workloadProgressDeadline`
(default `10m`, or an earlier Deployment progress-deadline failure); this bound
also applies to controllers without a native progress deadline. Deferred and
non-actionable workloads do not retain a slot. A failed rollout is reported and
does not block other workloads. A newer rotation of the same ID supersedes
pending restarts for the older generation.

**Cold nodes and stuck pods.** Restart-created pods retain the normal
`ready` preference and may use `available` nodes; requiring only warm nodes
would prevent legitimate autoscaling. Per-workload status distinguishes
`RuntimeImageInstalling` from an application rollout failure and records the
blocked Pod UID and node. Cold installation time counts toward the configured
workload progress deadline; exceeding it reports `RestartDeferred` for runtime
installation, frees the slot, and continues observing eventual recovery rather
than claiming the runtime is unhealthy or repeatedly patching the template.

Pods already bound to excluded nodes can remain blocked by the barrier;
container-create failures do not reschedule them. Status reports
`RestartDeferred` with the node, Pod UID, and minimum generation. The operator
does not bypass the barrier, delete pods, or override volume/node affinity to
repair this. Recovery is to restore authorized provisioning on that node, or
have the workload owner drain/replace the pod using the controller's normal
availability and storage constraints. A reported rollout timeout is not a
completed runtime restart.

**Out of scope.** Bare pods, Jobs, and CronJobs are not restarted (their next
pod uses the current generation). Other workload controllers can watch the
`RotationComplete` condition themselves. GitOps tools SHOULD ignore
`brewlet.sh/rotated-to-generation` on pod templates to avoid reporting drift.

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
  uses the same runtime image; there is no per-container form. A workload that
  needs several languages in one sandbox uses a runtime image the platform team
  composes for that purpose (for example, `java-21-python-3.12`), approved like
  any other ID.
- The application `image:` MUST be digest-pinned, exactly as in 1.x (SPEC §4.4); the
  shim's CRI identity checks are unchanged.
- All application, init, sidecar, and ephemeral containers MUST use 2.0
  application artifacts and the pod-wide runtime image. Ordinary-image
  pass-through is not supported. Only the CRI infrastructure sandbox takes the
  standard runc path; it does not run application code.

### 8.2 Sandbox assembly (shim `Create`)

1. **Resolve the artifact** exactly as SPEC §6.4 does today (protected CRI metadata,
   content-store digest verification, strict platform match). Recognize a 2.0
   artifact by `brewlet.sh/artifact-version=2` and a final `brewlet.sh/layer=launch`
   layer; any other application container image fails with `UnsupportedArtifact`.
2. **Load and validate** `launch.json` (§6.3) and the image-config mirror (§6.4).
3. **Resolve the runtime image** from the pod annotation, carried into the OCI
   runtime spec annotations by CRI. Fail with `RuntimeImageIncompatible` if
   `requires.runtimeImages` excludes it. Check live node-local authorization,
   revocation, and inventory epoch (§7.1); a cached root without current
   authorization cannot be used. If the ID is `available` on this node,
   request installation and wait as described in §7.5 (`RuntimeImageInstalling`
   or `RuntimeImageInstallFailed` on timeout or failure). Fail with
   `RuntimeImageNotInstalled` if the node's profile does not offer the ID at all.
   Enforce any `brewlet.sh/rotated-to-generation` minimum-generation barrier
   (§7.6), then atomically resolve and take a lease on `current` so a concurrent
   rotation cannot substitute an older generation or reclaim the leased root.
4. **Materialize and validate layers** from the content store using the 2.0
   per-layer-digest cache contract below. The 1.x Java-oriented extractor is
   not reused as-is.
5. **Assemble the rootfs** as an overlay: `lowerdir` = launch-layer-excluded
   application and dependency layers (top to bottom in reverse manifest order)
   over the runtime root; `upperdir`/`workdir` = per-container scratch. The CRI
   `readOnlyRootFilesystem` flag is preserved as in 1.x. Packaged `/app` is
   explicitly mounted read-only, independent of that flag. Writable application
   data uses explicit Kubernetes volumes (including mounts beneath `/app`) or
   runtime-provided writable directories; image layers are not writable storage.
6. **Generate `config.json`** from the CRI-provided spec with the process fields
   of §8.3 and the user of §8.4; resources, mounts, namespaces, and the CNI
   network namespace pass through unchanged. No language-specific arguments are
   injected.
7. **Delegate to runc.** Signals, exit codes, and logs retain normal behavior.
   Exec, exec probes, and exec lifecycle hooks use the shim's `Exec` handling
   specified in §8.3, not unmodified CRI process environment.

**Materialization contract.** Compressed layers are streamed from the local
content store while verifying the descriptor digest and the uncompressed diff
ID. Both gzip and zstd layer formats are supported. Extraction occurs only in
an isolated temporary cache entry, never directly over a runtime or live stage;
the entry becomes usable through atomic publication after complete verification
and validation. Cache entries are keyed by layer digest and extraction-policy
version, immutable and shared across artifacts. Whole compressed layers MUST
NOT be loaded into memory merely for extraction.

The extractor preserves regular-file contents, executable mode bits, and
mtimes (including those required by shipped AppCDS archives), and accepts
directories and confined relative symlinks. Hardlinks must reference a regular
file in the same layer under `app/`; unresolved/escaping links fail validation.
Ownership is normalized to `0:0`, never trusted to select the process identity.
Extended attributes are rejected. OCI whiteouts and opaque markers are
validated as scoped to `/app` and converted to the snapshotter's overlay
semantics; they are not ordinary `.wh.*` files in the final filesystem.
Validation handles link traversal across the composed layers, not just textual
archive path prefixes.

The cache is on persistent node storage at `/var/lib/brewlet/layers`, not
`/tmp`. Container upper/work directories use containerd-managed snapshot
storage. Cache leases and live mount references prevent collection while in
use. The provisioner reports shared cache/runtime-root disk consumption
separately; per-container writable snapshots MUST be integrated with CRI
filesystem usage so kubelet ephemeral-storage accounting and eviction see
them. Claiming accounting support requires a conformance test of CRI usage and
eviction, not merely locating files on disk. Disk exhaustion is an explicit
provisioning/container error, never a partially published cache entry.

### 8.3 Process fields

- **argv and working directory** are taken verbatim from the CRI-provided spec,
  which already reflects Kubernetes `command`/`args`/`workingDir` over the
  mirrored image config.
- **Environment** is merged in increasing precedence: runtime image `Env`, then
  the CRI-provided environment. Within CRI, image-config defaults are overlaid
  by kubelet's environment: service variables fill only names not explicitly
  supplied through pod `envFrom`/`env`; explicit `env` wins over `envFrom`.
  The shim uses the final CRI values and does not reimplement that resolution.
- **`PATH` is runtime-owned.** The shim always uses the runtime image's `PATH`
  and discards any `PATH` from CRI, because containerd injects a default `PATH`
  that is indistinguishable from an explicit one and because executable lookup
  must be stable across rotations. Admission emits a warning when a pod sets
  `PATH`. Applications add directories through ecosystem variables or
  absolute `entrypoint` paths.

**Exec parity.** `Create` persists the effective merged environment (including
`PATH` and `HOME`), identity, and leased inventory UID/generation in protected
per-container state. The shim MUST override `Exec` and rewrite the
`ExecProcessRequest.Spec` using that state before delegating to runc. Exec
commands, probes, and lifecycle hooks retain the requested argv, terminal, and
working-directory behavior but use the same effective environment and non-root
security restrictions as the container's initial process. They use its leased
runtime, not a newly rotated `current`. Missing state fails with
`ExecStateUnavailable` rather than falling back to containerd's default
environment. Conformance tests cover
relative executable lookup and runtime `Env`/`HOME` for both initial and exec
processes; editing bundle `config.json` alone does not establish exec parity.

### 8.4 User identity

Brewlet 2.0 workloads never run as root. The default identity is the runtime
image's `app` account.

- The application image config `User` is empty, so CRI derives the process user
  from the pod `securityContext` alone.
- Admission defaults each missing effective `runAsUser` and `runAsGroup`
  independently to `RuntimeImage.spec.user`, respecting container-over-pod
  precedence, and sets `runAsNonRoot: true`. A custom UID without a group
  therefore uses the non-zero `app` GID, not CRI's implicit GID 0.
- An explicit non-zero `runAsUser` is permitted (for example, platforms that
  assign UIDs per namespace). Files under `/app` are world-readable (§6.2), so any
  non-root UID can run the application.
- Admission denies UID or primary GID 0, supplementary GID 0, and
  `runAsNonRoot: false` with
  `RootNotSupported`.
- Admission defaults `allowPrivilegeEscalation: false`, drops all Linux
  capabilities, and rejects privileged containers, explicit privilege
  escalation, or added capabilities with `PrivilegeEscalationNotSupported`.
  The shim independently enforces `noNewPrivileges` and empty capability sets
  for initial and exec processes; ordinary runc privilege defaults are not
  sufficient to promise no root.
- **The shim enforces the rule independently**: a process UID, primary GID,
  or supplementary GID of 0 in the CRI-provided spec fails `Create` or `Exec`
  with `RootNotSupported`. Because CRI yields
  UID 0 when no user is set, a pod that bypassed fail-open admission fails closed
  rather than running as root.
- When the pod does not set `HOME`, the shim sets it from the runtime image's
  `/etc/passwd` entry for the process UID, or `/tmp` when the UID has no entry.
  This synthesized fallback is a deliberate Brewlet behavior, also applied to
  exec. Custom UIDs do not acquire passwd/group entries; `getpwuid`-based
  application APIs can fail. Use the default `app` identity for software that
  requires a named account.

## 9. Admission and scheduling

For every `runtimeClassName: brewlet` pod on CREATE, `brewlet-admission`:

- **Validates** `brewlet.sh/runtime-image` (presence, grammar) and that a
  `RuntimeImage` with that ID exists and is approved; denial reasons
  `RuntimeImageRequired`, `InvalidRuntimeImageID`, `UnknownRuntimeImage`.
- **Steers** scheduling by injecting a required `nodeAffinity` term on the
  capability label `brewlet.sh/runtime.<id>` `In ["ready", "available"]` and a
  preferred term (weight 100) on `In ["ready"]`, so pods land on nodes that
  already have the runtime installed whenever one fits and otherwise trigger an
  on-demand install (§7.5). Admission validates placement against active,
  approved `NodeProfile` inventory, not only live Node labels. Eligible profiles
  are non-deleting, pass source/mirror policy, and have no pool ownership
  conflict. It denies with
  `NoCompatibleRuntime` only when no valid profile offers the ID; an empty pool
  yields an admitted Pending pod so its autoscaler can act. Autoscaler templates
  must publish the runtime selector and capability labels and follow the
  supported startup-taint/initialization contract. Inventory approval does not
  itself prove that a provider's autoscaler template is correctly configured.
  When only profile inventory satisfies placement, admission warns with the
  profile names and leaves scheduling to Kubernetes and the configured scaler.
- **Defaults** the container user and denies root (§8.4), and **overwrites** the existing
  `brewlet.sh/artifact-*` compatibility hints (SPEC §8.3, unchanged).
- **Does not** read the application artifact from the registry, and therefore
  never defaults `brewlet.sh/runtime-image` from it. Artifact-side
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
`{ "<id>": { "install": "OnDemand", "state": "ready", "inventoryUID": "<uid>", "generation": 7, "digest": "sha256:…" } }`,
where `state` is one of the §7.5 states (`ready` for an installed `Eager` ID).
Revocation, expired authorization, and rotation quarantine withdraw the
capability label regardless of an old generation remaining on disk. Node
annotations are diagnostic, not authority for the shim.

`jdk.*`, `jdk-feature.*`, `launcher.*`, and `appcds-regeneration` labels are
removed. The [capability label contract](../CAPABILITY_LABELS.md) gains a v2 key
family and its dual-publication exception does not extend to 1.x keys.

## 10. `RuntimeClass`

`RuntimeClass/brewlet` is unchanged (SPEC §7): one handler, one scheduling selector,
one overhead. Runtime image selection is per pod rather than per RuntimeClass
(see §15 for the rejected per-ID RuntimeClass alternative). `overhead.podFixed`
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
| `JavaApplication` | A plain Deployment, StatefulSet, or DaemonSet; no replacement CRD (§4) |

### 11.1 The `brewlet` CLI: language-agnostic packaging

The CLI is the producer for every ecosystem and knows none of them. The author
writes `launch.json` (§6.3) by hand and declares which directories become which
layers; the CLI validates and packages them:

```bash
brewlet push registry.example.com/apps/api \
  --platform linux/amd64 \
  --launch ./launch.json \
  --layer dependency=./venv/lib/python3.12/site-packages:/app/site-packages \
  --layer app=./src:/app/src
```

The CLI MUST:

- validate `launch.json` against the schema and reject unknown fields;
- require explicit `--platform <os/arch[/variant]>` declarations, with no
  host-platform or language-based inference. Repeating this flag asserts that
  the supplied layers work unchanged on every listed platform. Different
  native layer groups are published as separate platform manifests and combined
  through an explicitly supplied index descriptor; no automatic native-file
  discovery or grouping occurs;
- enforce the layer content rules of §6.2 at build time (paths under `/app`,
  world-readable, no devices, setuid, or escaping links), producing the same
  reason codes the shim would;
- build deterministic layers in the given order, append the launch layer last,
  mirror `launch.json` into the image config (§6.4), and push;
- consume layer sets (§6.5) named with `--layer-set <ref>` by reusing their
  layers byte-for-byte.

Determinism is for identical source bytes, modes, mtimes, descriptors, and
platform declarations: entries are sorted, ownership rendered as `0:0`, and
compression metadata is fixed. File modes and mtimes are preserved, not silently
normalized; invalid permissions and extended attributes are rejected before
publish. Authors repair source permissions explicitly. This avoids changing
executable semantics or invalidating AppCDS metadata to make packaging pass.

The CLI MUST NOT inspect application content to derive any field: it never
detects a language, guesses an entrypoint, splits dependencies, chooses
`requires.runtimeImages`, or writes Kubernetes manifests. What the author wrote is
what ships. `brewlet inspect <image>` prints the effective `launch.json` and
layer table so authors and reviewers can check the result.

### 11.2 The Maven plugin: Java-specific producer

The Maven plugin is the one place where Java knowledge remains. It MAY:

- generate `launch.json` argv from the project: main class or JPMS module,
  classpath or module path, `--enable-preview`, `--add-opens`, and system
  properties;
- split dependencies into layers (for example `/app/lib` and `/app/mods`) or
  consume a published `java` layer set;
- emit an AppCDS archive as an ordinary application file and add the matching
  flags to argv;
- propose `requires.runtimeImages` from `maven.compiler.release` and a configured
  mapping to inventory IDs (for example `21 → java-21, java-21-jaz`), so the
  allow-list reflects the bytecode level the project actually targets;
- warn when the generated argv requires a newer runtime than the configured IDs
  provide.

Its output is an ordinary 2.0 artifact that the shim treats exactly like one
produced by the CLI. A developer can always replace the generated `launch.json`
with a hand-written one. The plugin does not generate Kubernetes manifests and
does not write `brewlet.sh/runtime-image`; choosing the runtime for a workload
remains an explicit decision in the reviewed manifest.

## 12. Security model

- **Trust roots.** The only executable bytes outside `/app` are runtime roots
  extracted from administrator-approved, digest-pinned `RuntimeImage` sources,
  apart from explicitly mounted Kubernetes volumes. The application image is
  verified by digest exactly as in 1.x; ordinary-image sidecars cannot bypass
  this contract.
- **Runtime file-path confinement.** §6.2 confines application and dependency
  layers to `/app`, and §7.2 requires the runtime's `/app` to be empty. This
  prevents layer replacement of runtime paths, not loading application-bundled
  libraries through `LD_*`, language lookup variables, or explicit volume
  mounts. Those dependencies remain application-owned remediation.
- **Packaged files are not secret storage.** World-readable `/app` supports
  arbitrary non-root UIDs, not per-user confidentiality inside a sandbox.
  Credentials MUST NOT be baked into these layers; use appropriately scoped
  Kubernetes Secret volumes or another secret-delivery mechanism.
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
- **No root.** Every Brewlet container runs with a non-zero UID, defaulted by
  admission and enforced by the shim (§8.4), with non-zero groups, no new
  privileges, and no Linux capabilities. Golden roots reject setuid/setgid
  binaries and file capabilities; application layers reject xattrs.
- **Restart RBAC is opt-in.** The operator's permission to patch Deployments,
  StatefulSets, and DaemonSets exists only when rotation restarts are enabled
  in the chart (§7.6).
- **Unchanged boundaries.** The privileged provisioner, host-path layout under
  `/opt/brewlet`, fail-open admission, and runc isolation retain their 1.x
  properties and caveats (SPEC §11).

## 13. Observability

- Shim event `RuntimeImageResolved` on each container start with ID, generation,
  inventory UID, and digest; the same values are exported as
  `brewlet_container_runtime_image_info{id,inventory_uid,generation,digest}`.
- `brewlet_runtime_image_generations{id,state}` on each node, and a controller
  condition `RotationProgressing` / `RotationComplete` per `RuntimeImage`.
- Rotation restarts (§7.6) report `status.restarts` counts per `RuntimeImage`
  and per-workload outcomes, including `RestartNotApplicable`,
  `RestartDeferred`, and rollout failures, with corresponding workload events.
  Rotation status records participating and excluded Node UIDs, their observed
  generations, and exclusion reasons; `RotationStalled` identifies nodes that
  exceeded the progress deadline. Completion does not clear these diagnostics.
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
  `LayerPathViolation`, `UnsupportedAnnotation`, `RootNotSupported`,
  `RuntimeImageUpdating`, `InvalidRuntimeGeneration`, `RuntimeImageEpochMismatch`,
  `RuntimeImageRevoked`, `RuntimeImageAuthorizationExpired`,
  `PrivilegeEscalationNotSupported`, `ExecStateUnavailable`,
  `ownership-fence-failed`.

## 14. Open questions

1. **Baked export ([proposal 0007](0007-baked-golden-image-delivery.md)).**
   Should Brewlet offer a standard way to bake an application and its runtime
   image into one ordinary image, using the same `RuntimeImage` inventory and
   `launch.json`, for clusters without the shim?

   - *For:* runs on clusters that cannot install a privileged provisioner or a
     custom shim (some managed or regulated platforms); works with VM-isolated
     sandboxes such as Kata; ordinary scanners, signing, and `docker run` for
     local development all work unchanged; and it gives adopters a way out,
     which lowers the risk of adopting Brewlet. Composition is well defined
     because §6–§8 already specify layer order, environment, user, and `PATH`.
   - *Against:* baked images lose the central property of this design: a
     rotation does not patch them, so each rotation requires a rebuild and
     redeploy of every baked application. The shared read-only runtime per node
     and its pull savings are lost. Every runtime generation produces a new
     application digest, so digests return to workload manifests. Brewlet would
     support and test two execution paths whose behavior can drift (read-only
     `/app`, root rejection, and `PATH` ownership become build-time checks rather
     than shim enforcement), and it must define who signs the baked result.
   - *Option:* keep 0007 separate and later, but hold this proposal to
     deterministic composition rules so a bake exporter can be added without
     changing the 2.0 contract.

## 15. Alternatives considered

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
| SPEC §8 | Add rotation restarts to the operator | §7.6 |
| SPEC §8.2, §9 | Remove `JavaApplication` | §11 |
| SPEC §8.3 | Replace admission rules | §9 |
| SPEC §10 | Remove JVM resource mapping | §11 |
| SPEC §11 | Add non-root enforcement | §8.4, §12 |
| SPEC §13 | Move AppCDS guidance to Maven plugin docs | §11 |
| SPEC §14 | Update reason codes, annotations, host layout | §7.3, §13 |
| [CAPABILITY_LABELS](../CAPABILITY_LABELS.md) | New v2 key family | §9.1 |
