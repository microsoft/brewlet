# Brewlet specification

- **Status:** authoritative contract for the **breaking** Brewlet 1.0.0 implementation,
  not an additive mode of Brewlet 0.x. Implementation and qualification are
  distinct: see the [baseline ledger](../acceptance/requirements.json) (Part I),
  the [composition ledger](../acceptance/composition-requirements.json)
  (Part II) and the [scoped qualification records](../acceptance/RESULTS.md).
- **Release identity:** [`release.json`](../release.json).
- **Origin:** consolidates the approved Proposal 0009 (Part I, §1–§15) and
  Proposal 0010 (Part II, §16); section numbers are unchanged from 0009 and
  0010 §N maps to §16.N.
- **Reference main:** `fa7085335b220e29df6affbece4558c5f1259ecb`.
- **0.x contracts replaced:** application artifact (SPEC §4.1–§4.4), JDK
  inventory (SPEC §5.3, SPEC §5.6), shim JDK selection (SPEC §6.1), JVM
  resource mapping (SPEC §10), `JavaApplication` (SPEC §8.2, SPEC §9), and the JDK
  capability labels ([0.x CAPABILITY_LABELS](https://github.com/microsoft/brewlet/blob/fa7085335b220e29df6affbece4558c5f1259ecb/specs/CAPABILITY_LABELS.md))
- **0.x contracts retained and generalized:** runnable-image delivery
  (SPEC §4.4), managed dependency bundles (SPEC §4.5), copy-from-image node roots (SPEC §5.3),
  `NodeProfile` pool selection and rollout (SPEC §5.6), the runc-backed shim (SPEC §6),
  `RuntimeClass/brewlet` (SPEC §7), and the admission webhook (SPEC §8.3)

Brewlet 1.0.0 removes Java from Brewlet's core contract. Developers publish
**application artifacts** (code, plus dependencies either as layers or as
independently published layer sets) to an OCI registry. Platform teams approve
**runtime images** — digest-pinned "golden" base images such as a JRE, a Python
interpreter, a Node.js runtime, a .NET runtime, a native WebAssembly host such as
Wasmtime or Spin, or a distroless base for static binaries — in the
Brewlet inventory. A pod selects a runtime image **by ID only**, never by digest,
and the node-resident shim composes the approved runtime with the application at
launch time.

Bare `§N` references cite sections of this specification. References written
as `SPEC §N` instead cite the **0.x** design reference,
[`SPECIFICATION.md` at main commit `fa70853`](https://github.com/microsoft/brewlet/blob/fa7085335b220e29df6affbece4558c5f1259ecb/specs/SPECIFICATION.md),
which this document replaces.

Brewlet 1.0.0 is a rewrite inspired by Brewlet 0.x on `main`.
`SPEC §N` references provide design context only;
this document defines the implemented 1.0.0 contract. Part I covers runtime images
and workload execution; Part II (§16) covers governed release composition. This
is not a claim of a stable production release.

Release numbering is independent of artifact schema versions, Kubernetes API
versions and containerd shim interface versions.

The key words MUST, MUST NOT, SHOULD, and MAY describe the 1.0.0 contract.

**Approved upstream-runtime amendment (2026-10-19).** Compatible upstream OCI
runtime images, including shell-free distroless images, need not be rebuilt
solely to create an account named `app`, an existing home, or executable-search
directories. The numeric identity and execution-safety contract below remains
mandatory. Application content is isolated from any runtime `/app` content;
it never merges with that content. Historical acceptance predating this
amendment does not verify these changed requirements.

---

## Contents

- [Part I — Language-agnostic runtime images](#part-i--language-agnostic-runtime-images) (§1–§15)
- [Part II — Governed dependency composition and remediation](#part-ii--governed-dependency-composition-and-remediation) (§16)
- [Appendix — mapping from the 0.x specification](#appendix--mapping-from-the-0x-specification)

# Part I — Language-agnostic runtime images

## 1. Summary

Brewlet 0.x separates a Java application from its JDK at launch time. On
inspection, almost nothing in that separation is Java-specific:

- A JDK "root" (SPEC §5.3) is already the **complete root filesystem** of an
  administrator-approved, digest-pinned image, copied read-only onto the node.
- The application image (SPEC §4.4) is already a set of ordinary
  `tar+gzip` layers plus a small launch descriptor.
- The shim (SPEC §6) already overlays the two and delegates to runc.

The Java-specific parts are the inventory vocabulary (distribution, feature,
`javaHome`), the launch descriptor (`mainJar`, `entry.mode`,
`addOpens`, …), and the JVM conveniences built on top (`jvm.args`, AppCDS,
`JavaApplication`, JVM resource mapping).

Brewlet 1.0.0 keeps the mechanism and drops the vocabulary:

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
  Java problems. Python, Node.js, .NET, WebAssembly guests hosted by native
  engines, Ruby, and even static Go binaries (which
  still need CA bundles, tzdata, and a hardened userland) benefit from a
  centrally governed runtime that ships separately from application bytes.
- **The Java-specific surface is the expensive surface.** JDK feature/distribution
  matching, the JVM flag grammar, and
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
- Keep the Maven plugin as an optional **Java producer** of 1.0.0 artifacts, the
  only place where Java-specific intelligence lives (§11.2).

## 4. Non-goals

- Compatibility or migration tooling for previous Brewlet releases. 1.0.0 is a
  total pivot; previous artifacts, descriptors, and deployments are outside
  this specification's contract.
- Per-workload digest pinning of the runtime. A workload that must pin an exact
  runtime digest should use an ordinary image with the stock runc RuntimeClass, or
  a dedicated, narrowly scoped runtime image ID (§7.4).
- Language-aware tuning. Brewlet injects no interpreter, VM, or GC flags; runtimes
  that need cgroup-aware tuning MUST obtain it from the runtime image itself.
- Building runtime images. Golden images are produced by the platform team's
  existing image pipeline; Brewlet only approves, distributes, and composes them.
- A generic workload CRD. `JavaApplication` is removed and is not replaced by a
  language-neutral equivalent in 1.0.0; Deployments, StatefulSets, and DaemonSets
  are the workload API.
- Per-container runtime images. A pod uses exactly one runtime image (§8.1).
- Running workloads as root (§8.4).
- Inferring the runtime image from the artifact. Admission never reads the
  registry and never fills in `brewlet.sh/runtime-image`; the workload manifest
  always states it explicitly (§9).
- Language detection in generic tooling. The CLI does not guess entrypoints,
  dependency layouts, or runtime IDs for any ecosystem.
- Sandboxes other than runc. Stronger isolation remains deferred research
  requiring a separate design and compatibility/performance proof (§14).

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
annotation `brewlet.sh/artifact-version=1`. Platform-neutral content (bytecode,
pure Python, JavaScript) SHOULD be published for every platform the fleet runs,
with identical layer digests in each platform manifest so nodes deduplicate them.
Content with native code (static binaries, wheels or addons with shared objects,
JNI libraries) MUST be published only for the platforms it supports; Kubernetes
then fails such a pod on an unsupported node at image pull, as for any image.
Producers require explicit platform declarations (§11.1); content neutrality
is an author assertion, never inferred by the CLI.

The named workload examples include **Java, Python, Node.js, .NET and
WebAssembly**, all using this same artifact and explicit launch contract (§13.2
defines which of them are qualification fixtures):

| Workload | Application contribution | Approved Linux runtime infrastructure |
| --- | --- | --- |
| Java | JARs and explicitly laid-out dependency files | A compatible JRE/JDK |
| Python | Source/bytecode and explicitly packaged dependencies | A compatible Python interpreter/userland |
| Node.js | JavaScript and explicitly packaged dependencies | A compatible Node.js runtime/userland |
| .NET | Assemblies, runtime configuration and explicitly packaged dependencies | A compatible CLR/userland, or an explicitly chosen root for self-contained output |
| WebAssembly | A core module or component, host configuration and declared external dependency files/components | A compatible native host such as Wasmtime or Spin and its complete userland |

For WebAssembly, the OCI platform still identifies the **Linux host
architecture**, not `wasi/wasm32`. Guest portability does not authorize inferred
platforms, inferred WASI interfaces, automatic engine selection or host launch
rewriting. The author explicitly declares the module/component ABI, engine
argv, guest preopens/mounts, environment and network capabilities through normal
producer/host configuration. Guest compilation and ecosystem-specific linking
remain outside Brewlet core.

The 0.x native-artifact media types (`application/vnd.brewlet.jar.layer.v1+jar`
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
  "schemaVersion": 1,
  "process": {
    "entrypoint": ["python"],
    "args": ["-m", "gunicorn", "app.wsgi:application", "--bind", "0.0.0.0:8000"],
    "workingDir": "/app",
    "env": [
      { "name": "PYTHONPATH", "value": "/app/src:/app/site-packages" },
      { "name": "PYTHONDONTWRITEBYTECODE", "value": "1" }
    ]
  },
  "runtimeImages": ["python-3.12", "python-3.12-fips"]
}
```

Field contract (unknown fields MUST be rejected at publish and launch time):

| Field | Required | Rules |
|---|---|---|
| `schemaVersion` | yes | MUST be `2`. |
| `process.entrypoint` | yes | Non-empty argv prefix. A non-absolute `entrypoint[0]` requires the runtime image's `PATH` for sandbox resolution (runc semantics); absent PATH fails with `RuntimePATHRequired` (§7.2). |
| `process.args` | no | Default arguments appended after `entrypoint`. |
| `process.workingDir` | no | Absolute path; defaults to `/app`. |
| `process.env` | no | Ordered list of unique names. `PATH` is forbidden (§8.3). |
| `runtimeImages` | no | Optional top-level allow-list of runtime image IDs the artifact was built and tested against. When present, the shim MUST refuse any other ID with `RuntimeImageIncompatible`; an empty array denies all. When absent, any approved ID is accepted at this gate. Null is invalid. |

The allow-list is deliberately strict and name-based, not a language capability
matcher. Composed, hold, and variant IDs must be explicitly listed even if they
provide the same language version. Adding such an ID requires republishing the
launch layer/config, not rebuilding unchanged application/dependency layers.
Authors wanting inventory-independent artifacts omit the optional allow-list
and accept responsibility for testing their explicitly selected workload ID.
The approved schema flattening removes `requires` entirely. The old wrapper,
including an empty object or a mix of old/new forms, MUST be rejected as an
unknown field; no compatibility alias or implicit selection is introduced.
Admission cannot precheck this constraint without registry access; mismatches
are reported at container creation.

The launch configuration intentionally contains **no** language-specific fields.
Everything a 0.x `jvm.config` expressed (`mainJar`, `entry.mode`, `addOpens`,
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
| `Labels["sh.brewlet.app"]` | `"1"` (informational only) |

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
the optional top-level `runtimeImages` allowlist and is what tooling inspects.
A future baked exporter would need to honor the same launch contract; no such
delivery mode is implemented by this specification.

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

The 0.x Maven-specific bundle contract (BOM, GAV lock, `compatibleJdks`) becomes
the Maven plugin's producer of a `java`-ecosystem layer set.

## 7. Runtime image inventory

### 7.1 `RuntimeImage` resource

Approval and placement are separated. Approval is a cluster-scoped
`RuntimeImage` (`node.brewlet.sh/v1alpha1`); placement remains `NodeProfile`.

```yaml
apiVersion: node.brewlet.sh/v1alpha1
kind: RuntimeImage
metadata:
  name: python-3.12            # the runtime image ID
spec:
  source:
    image: registry.example.com/golden/python@sha256:<64 hex>   # index or manifest
  description: CPython 3.12 on hardened Debian 12, glibc ABI
  user:                        # approved non-root numeric defaults (§8.4); immutable
    uid: 1000
    gid: 1000
  compatibility:               # informational; drives docs and tooling, not matching
    ecosystem: python
    abi: glibc-2.36
status:
  generation: 7                # increments on every spec.source.image change (rotation)
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
  immutable: they identify approved numeric defaults (§7.2, §8.4) and are part of
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
labels, or confirms the node-wide startup fence when provider-pinned labels
cannot be removed (§9.2), and the provisioner atomically publishes a revoked
local authorization record. The shim checks that record on every `Create` and rejects with
`RuntimeImageRevoked`; an offered ID or existing root alone is not approval.
Already-running tasks retain their roots until leases drain; revocation does
not kill them. A deletion finalizer retains UID-bound inventory evidence until
labels are withdrawn or their node-wide fence is confirmed, local
authorizations expire or are revoked, and root cleanup finishes. Re-creation
never adopts old-epoch roots.

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

Environment names MUST be unique. `PATH` MAY be absent for absolute executable
invocation. When present it MUST occur exactly once, contain non-empty canonical
absolute directories outside `/app`, and contain no `.` or empty component.
Missing directories are allowed and have ordinary executable-search semantics;
existing entries must be searchable directories. Root-relative symlink
resolution MUST reject escapes, loops and any traversal through `/app`,
including an alias whose final destination otherwise lies outside `/app`.
With no runtime PATH, relative initial and Exec executables fail explicitly
with `RuntimePATHRequired`, rather than inheriting CRI's default PATH or
searching the working directory.

`/app` is the reserved application prefix for every ecosystem. A runtime may
contain files, a directory or a symlink there, but its `/app` content MUST be
hidden by an independently established application boundary before mountpoint
preparation or execution. Runtime files never merge into packaged `/app`.

`RuntimeImage.spec.user` supplies the required nonzero default numeric UID/GID.
A named account, a particular username, passwd/group files and an existing home
directory are not required. Present account databases MUST be well formed and
unambiguous; optional passwd homes are metadata, not execution authority.
Software requiring named-account/NSS lookup must use a suitable runtime.
Runtime roots MUST NOT contain setuid/setgid binaries or file capabilities;
the provisioner rejects such roots rather than silently changing approved
bytes. Runtime images SHOULD be minimal, SHOULD NOT contain package managers,
and SHOULD carry an SBOM referrer. The provisioner
applies the existing safe-extraction rules (SPEC §5.3) and records
`.brewlet-source` with the resolved digest; the 0.x `.brewlet-java-home` marker is
removed.

Alongside each root the provisioner persists immutable, root-owned metadata:
inventory ID and UID, generation, source and platform manifest digests,
selected OCI platform/config `Env`, approved numeric UID/GID and optional
account/home metadata.
The `current` pointer publishes this metadata and its root as one unit.
Mutable authorization/expiry and revocation records are separately written
atomically by the fenced provisioner; the shim consumes trusted local records,
not pod-supplied environment or user data, as the runtime authority.

**Approving third-party images.** A compatible upstream digest may be approved
unchanged. Shells, package managers and account-management utilities are not
required inside the runtime: the host provisioner inspects files without
executing image code. If a source violates retained privilege or compatibility
policy, a platform team may derive and approve a new image in its own pipeline.
For example, it may remove setuid/setgid bits before publication:

```dockerfile
FROM container-registry.oracle.com/java/jdk:21
RUN find / -xdev -perm /6000 -type f -exec chmod ug-s {} +
```

Brewlet does not build or modify runtime images; licensing and redistribution
terms of the base image remain the platform team's responsibility.

**An ID is a compatibility promise.** Rotating the digest behind an ID MUST
preserve everything applications built against that ID depend on: language
minor/feature line, C library and ABI, executable/PATH layout, numeric identity, and the
presence of documented tools. A change that breaks any of these — a new major runtime, a
libc switch from glibc to musl, a changed interpreter path — MUST be published as
a new ID. This is the contract that makes digest-free workload references safe.

### 7.3 Placement and rotation (`NodeProfile`)

`NodeProfile` keeps pool selection, tolerations, mirrors, and rollout policy, and
replaces `jdks` with one list of runtime image
entries:

```yaml
apiVersion: node.brewlet.sh/v1alpha1
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
  as in 0.x. A profile containing only `OnDemand` entries MUST be able to
  publish `brewlet.sh/runtime=ready` after validating the shim, handler,
  annotation forwarding, ownership, and local authorizations, without fetching
  a runtime root. Its approved IDs advertise `available`, so the first pod can
  schedule and trigger installation. The startup fence is released only after
  this baseline and all required eager installations are ready (§9.2).
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
  has no leases and has aged past the grace period. This closes the 0.x roadmap
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
  extracted root (safe application-prefix isolation, valid optional PATH, required platforms
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
authority, so a node can advertise it before any runtime bytes are fetched,
behind the startup fence until baseline provisioning completes (§9.2).
Revoked or expired authorizations withdraw the capability advertisement,
using the confirmed node-wide fence if labels are provider-pinned.
Cluster autoscaler templates can declare `available` for scale-from-zero,
but the corresponding real-node label MUST be mutable: installation, failure,
and idle reclamation change or remove it. Provider-pinned per-ID labels are
not supported for `OnDemand` entries (§9.2).

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
them, but requires removing any provider pin on that ID's capability label
first (§9.2).

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

The workload owner grants consent with the annotation; the platform team keeps
no patch rights in application namespaces. Because RBAC cannot express
"patch only this annotation", the chart MUST bound the operator's workload
patch rights with a `ValidatingAdmissionPolicy` (`failurePolicy: Fail`,
`validationActions: [Deny]`) whenever restarts are enabled. For requests from
the operator service account to apps/v1 Deployments, StatefulSets, and
DaemonSets outside the release namespace, the policy denies everything except
an `UPDATE` that:

- targets an object whose stored version carries the workload opt-in
  annotation, `runtimeClassName: brewlet`, and a `brewlet.sh/runtime-image`
  template annotation;
- is outside `operator.rotationRestarts.protectedNamespaces` (default
  `kube-system`, `kube-public`, `kube-node-lease`) and, when set, inside
  `operator.rotationRestarts.namespaces`;
- sets `brewlet.sh/rotated-to-generation` and leaves every other pod template
  annotation, the template labels and spec, the remaining workload spec, and
  the object's labels, annotations, owner references, and finalizers
  unchanged.

The operator thus cannot use restart RBAC to change images, commands,
replicas, or opt-in state, or to touch workloads that did not consent.
Installation MUST fail on clusters without
`admissionregistration.k8s.io/v1` (Kubernetes before 1.30) unless
`operator.rotationRestarts.requireAdmissionPolicy=false` explicitly accepts
RBAC as the only bound.

`operator.rotationRestarts.namespaces` (default empty, meaning cluster-wide)
optionally narrows the feature further. When set, the operator considers only
workloads in those namespaces (`--restart-namespaces`), and workload `patch` is
granted through a Role and RoleBinding in each listed namespace instead of the
ClusterRole; cluster-wide workload access stays read-only. The release
namespace and protected namespaces cannot be listed. Listed namespaces must
exist before installation.

**Trigger and participating nodes.** A rotation tracks the approved target
generation and digest. Participants are live, `Ready=True`, schedulable,
non-deleting nodes without the startup taint, with a valid profile ownership
claim that offers the ID, either as `Eager` or as an already-installed
`OnDemand` entry. An installed
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

**Excluded nodes and recovery.** NotReady, cordoned, deleting, retiring, or
startup-fenced nodes are excluded and listed in `status.rotation.excludedNodes` with Node UID,
reason, and last observed runtime generation. Their runtime capability label
MUST be withdrawn, or the node-wide startup fence of §9.2 confirmed, before
completing the rotation. A node whose progress stalls
for `operator.rotationRestarts.nodeProgressDeadline` (default `10m`, measured
from target assignment if no progress has occurred, otherwise from its last
target-generation progress) reports `RotationStalled` and is
quarantined for this ID by withdrawing the capability label. If provider
admission refuses removal, the fallback fences the whole node, not only this
ID. Once withdrawal or that fence is confirmed, the node is recorded as
excluded rather than blocking all healthy nodes indefinitely. If neither can
be confirmed, completion remains blocked and reports the node and error;
expiry alone is not success. A node-wide fence excludes the node from every
affected ID's participant set; each rotation retains the Node UID and
node-wide exclusion reason rather than treating those runtimes as updated.

The provisioner MUST NOT republish `ready` for an excluded or returning node
until its ownership fences pass and its validated `current` matches the latest
approved target. An uninstalled `OnDemand` entry can advertise `available` and
install that latest target on demand. Existing containers keep their leases;
quarantine does not terminate them. Releasing a node-wide fence additionally
requires the recovery checks of §9.2 for every affected ID, not just the ID
whose rotation triggered it. Status and events MUST retain excluded
nodes and outstanding old-generation containers so partial completion is
visible. Recovery is to restore the node and complete provisioning, or retire
it through the existing fenced cleanup protocol, not bypass validation.

**Selection.** The operator then considers each opted-in workload whose pod
template carries `brewlet.sh/runtime-image: <id>`, and restarts it if at least
one of its running pods is observed or presumed on an older generation. Lease
evidence (§13, `status.taskObservations`) decides first:

1. A running pod whose UID appears in any retained older-generation or
   foreign-epoch binding is stale, whatever its start time. This catches pods
   that started after completion on an excluded or lagging node.
2. Otherwise, a running pod is current when a `Current`,
   `CompleteTaskAndLeaseObservation` observation of its node is fresh (at most
   two minutes old) and was taken after the pod's latest container start, and
   it lists no older binding for that pod.
3. Otherwise the start-time rule applies: the pod is stale if it started
   before `status.rotationCompletedAt`.

Missing, stale, incomplete, or foreign observations therefore fall back to
restarting, never to skipping. Workloads created or rolled after completion,
and pods proven current by a fresh complete observation, are left alone.
A workload whose controller reports availability is not complete while a
retained observation still lists an older binding for one of its pods.

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
- The application `image:` MUST be digest-pinned, exactly as in 0.x (SPEC §4.4); the
  shim's CRI identity checks are unchanged.
- All application, init, sidecar, and ephemeral containers MUST use 1.0.0
  application artifacts and the pod-wide runtime image. Ordinary-image
  pass-through is not supported. Only the CRI infrastructure sandbox takes the
  standard runc path; it does not run application code.

### 8.2 Sandbox assembly (shim `Create`)

1. **Resolve the artifact** exactly as SPEC §6.4 does today (protected CRI metadata,
   content-store digest verification, strict platform match). Recognize a 1.0.0
   artifact by `brewlet.sh/artifact-version=1` and a final `brewlet.sh/layer=launch`
   layer; any other application container image fails with `UnsupportedArtifact`.
2. **Load and validate** `launch.json` (§6.3) and the image-config mirror (§6.4).
3. **Resolve the runtime image** from the pod annotation, carried into the OCI
   runtime spec annotations by CRI. Fail with `RuntimeImageIncompatible` if
   `runtimeImages` excludes it. Check live node-local authorization,
   revocation, and inventory epoch (§7.1); a cached root without current
   authorization cannot be used. If the ID is `available` on this node,
   request installation and wait as described in §7.5 (`RuntimeImageInstalling`
   or `RuntimeImageInstallFailed` on timeout or failure). Fail with
   `RuntimeImageNotInstalled` if the node's profile does not offer the ID at all.
   Enforce any `brewlet.sh/rotated-to-generation` minimum-generation barrier
   (§7.6), then atomically resolve and take a lease on `current` so a concurrent
   rotation cannot substitute an older generation or reclaim the leased root.
4. **Materialize and validate layers** from the content store using the 1.0.0
   per-layer-digest cache contract below. The 0.x Java-oriented extractor is
   not reused as-is.
5. **Assemble the rootfs** with an application-prefix boundary: launch-layer-excluded
   application and dependency layers (top to bottom in reverse manifest order)
   over a private opaque `/app` boundary over the runtime root. The boundary
   MUST hide all inherited runtime `/app` content without modifying the approved
   root or shared payload cache. `upperdir`/`workdir` = per-container scratch. The CRI
   container root MUST be read-only at execution. Admission defaults
   `readOnlyRootFilesystem: true` and rejects explicit false for application,
   init/sidecar and ephemeral containers. The shim independently rejects a
   non-read-only OCI root with `ReadOnlyRootRequired`; missing admission cannot
   restore writable-root behavior. Packaged `/app` is explicitly mounted
   read-only as well. Writable application data uses explicit Kubernetes
   volumes, not writes to a private copy of runtime files. This deliberately
   replaces the earlier writable-root/CRI-flag-preservation contract.
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
them. With a read-only root, CRI reports the managed root's setup/metadata usage;
explicit disk-backed volumes are separately accounted by kubelet VolumeStats
and Pod ephemeral-storage limits. A volume usage delta MUST NOT be reported as
container writable-layer growth. Conformance requires actual CRI samples,
volume samples and kubelet eviction, not merely locating files on disk or
allowing an otherwise forbidden writable root. Disk exhaustion is an explicit
provisioning/container error, never a partially published cache entry.

**Runtime mount policy.** `RuntimeImage.spec.volumeMountPaths` is an optional
administrator-owned list of data/configuration mount prefixes outside `/app`.
Omitted or empty means no additional workload mount locations. It is immutable
within an inventory UID, like the numeric user/compatibility identity; changes
require a new ID. The allocation journal and validated node metadata MUST bind
the same policy, and workers MUST NOT reuse roots with a different policy.
Paths MUST be canonical absolute, sorted, unique, nonoverlapping, at most 32
entries and 1024 characters each. Root, `/app`, `/dev`, `/proc`, `/sys` and
the kubelet's `/etc/hosts`, `/etc/hostname`, `/etc/resolv.conf` locations or
their ancestors/descendants are not declarable zones.

Every workload volume or block-device destination, including read-only mounts,
MUST be beneath `/app/` or within a declared zone. A custom termination-message
path follows the same policy. Mounting the entire `/app` is forbidden. Reserved
Linux/CRI pseudo-filesystems, standard kubelet host/DNS/termination files and
the standard read-only service-account projection are narrow system exceptions,
not arbitrary workload mount permissions. Admission validates Pod destinations;
the shim independently validates the effective OCI mounts and devices against
the leased policy before delegating to runc. Violations fail with
`RuntimeMountNotAllowed`. Ordinary image file/directory permissions are not a
substitute for this boundary.

Declared zones MUST be directories (or absent mountpoints), not symlink aliases
or ancestors of runtime PATH directories. Runtime publication rejects invalid
zones with `InvalidRuntimeMountPolicy` or explicit path-validation errors.
Mount targets MUST NOT traverse symlinks, including confined application
symlinks; the standard service-account `/var/run` alias may resolve only to its
same `/run` location. Workload mount/device destinations MUST NOT overlap:
an earlier volume could otherwise redirect a later mount through source-owned
symlinks. Kubernetes system mounts retain their narrowly defined nesting.
Platforms MUST designate data/configuration zones, not runtime program/library
installations. Integrity applies outside these explicit mutable/system zones;
it is not a claim that every byte in an administrator-approved data zone is
immutable or that application-owned code cannot execute.

The shim MUST NOT infer a language, auto-create writable `/tmp`/cache mounts,
or inject runtime-specific cache flags. Platforms provide reviewed workload
templates; applications explicitly request volumes and author any cache/temp
configuration. Existing tasks retain their immutable mount namespace and leased
generation; Exec inherits it, rather than constructing a new writable view.

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
  If runtime PATH is absent, it remains absent; absolute executable invocation
  is allowed and relative initial/Exec invocation fails with `RuntimePATHRequired`.

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

Brewlet 1.0.0 workloads never run as root. The default identity is the approved
numeric `RuntimeImage.spec.user`, independent of account names in the image.

- The application image config `User` is empty, so CRI derives the process user
  from the pod `securityContext` alone.
- Admission defaults each missing effective `runAsUser` and `runAsGroup`
  independently to `RuntimeImage.spec.user`, respecting container-over-pod
  precedence, and sets `runAsNonRoot: true`. A custom UID without a group
  therefore uses the approved non-zero GID, not CRI's implicit GID 0.
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
  exec. A HOME string does not create a directory or promise writable storage.
  Numeric identities do not acquire fabricated passwd/group entries;
  `getpwuid`-based application APIs can fail. Software requiring a named account
  must use an image that provides one for the chosen identity.

## 9. Admission and scheduling

For every `runtimeClassName: brewlet` pod on CREATE, `brewlet-admission`:

- **Validates** `brewlet.sh/runtime-image` (presence, grammar) and that a
  `RuntimeImage` with that ID exists and is approved; denial reasons
  `RuntimeImageRequired`, `InvalidRuntimeImageID`, `UnknownRuntimeImage`.
- **Steers** scheduling by ANDing a requirement on the capability label
  `brewlet.sh/runtime.<id>` `In ["ready", "available"]` into **every** existing
  required `nodeAffinity` selector term, creating a term only when no required
  selector exists. It MUST NOT add a separate OR alternative that bypasses the
  author's constraints; an explicitly empty selector MUST remain unsatisfiable.
  Admission preserves `matchFields`, `nodeSelector`, and existing preferred
  rules, and appends a preferred term (weight 100) on `In ["ready"]`, so pods land on nodes that
  already have the runtime installed whenever one fits and otherwise trigger an
  on-demand install (§7.5). Admission validates placement against active,
  approved `NodeProfile` inventory, not only live Node labels. Eligible profiles
  are non-deleting, pass source/mirror policy, and have no pool ownership
  conflict. It denies with
  `NoCompatibleRuntime` only when no valid profile offers the ID; an empty pool
  yields an admitted Pending pod so its autoscaler can act. Autoscaler templates
  must publish the runtime selector and capability labels and follow the
  startup-taint/initialization contract in §9.2. Inventory approval does not
  itself prove that a provider's autoscaler template is correctly configured.
  When only profile inventory satisfies placement, admission warns with the
  profile names and leaves scheduling to Kubernetes and the configured scaler.
  This inventory check is not a guarantee that capacity satisfies all pod
  affinity, architecture, taints, resource, or volume constraints.
- **Defaults** the container user and denies root (§8.4), and **overwrites** the existing
  `brewlet.sh/artifact-*` compatibility hints (SPEC §8.3, unchanged).
- **Does not** read the application artifact from the registry, and therefore
  never defaults `brewlet.sh/runtime-image` from it. Artifact-side
  constraints (`runtimeImages`, layer rules, platforms) are enforced by
  the shim and by kubelet's pull, so fail-open admission never weakens them.

The 0.x `brewlet.sh/jdk`, `brewlet.sh/jvm-args`,
and `brewlet.sh/arch` annotations are removed and
MUST be rejected on a 1.0.0 pod (`UnsupportedAnnotation`) so a stale manifest
fails loudly instead of silently losing its intent.

### 9.1 Capability labels

The provisioner publishes, per node:

| Label | Value | Meaning |
|---|---|---|
| `brewlet.sh/runtime` | `ready` | Validated shim/handler baseline and required eager roots; `RuntimeClass/brewlet` scheduling selector. No installed root is required for an OnDemand-only profile (§7.3). |
| `brewlet.sh/runtime.<id>` | `ready` | The node has a validated `current` generation of `<id>`. |
| `brewlet.sh/runtime.<id>` | `available` | `<id>` is an `OnDemand` entry of the node's profile and is not yet installed (§7.5). |

The label value is deliberately not the digest or generation: during a rotation,
a node still converging remains schedulable for the ID because the previous
generation honors the same compatibility promise (§7.2). Generation detail is
published in the node annotation `brewlet.sh/runtime-images` as JSON
`{ "<id>": { "install": "OnDemand", "state": "ready", "inventoryUID": "<uid>", "generation": 7, "digest": "sha256:…" } }`,
where `state` is one of the §7.5 states (`ready` for an installed `Eager` ID).
Revocation, expired authorization, and rotation quarantine withdraw the
capability label, or establish the node-wide fence of §9.2 if the provider
pins it, regardless of an old generation remaining on disk. A startup-tainted
node is never runtime-ready, whatever its labels say; readiness/status and
warm-node observations MUST use that effective state rather than raw labels.
Node annotations and retained pinned labels are diagnostic or scheduling
hints, not authority for the shim.

`jdk.*` and `jdk-feature.*` labels are
removed. This section and §9.2–§9.3 define the 1.0.0 capability-label contract;
the [0.x contract](https://github.com/microsoft/brewlet/blob/fa7085335b220e29df6affbece4558c5f1259ecb/specs/CAPABILITY_LABELS.md)'s dual-publication exception does not
extend to 0.x keys.

### 9.2 Startup fencing and provider-managed labels

The startup fence is the `NoSchedule` taint
`startup-taint.cluster-autoscaler.kubernetes.io/brewlet=provisioning`.
The [0.x startup-taint contract](https://github.com/microsoft/brewlet/blob/fa7085335b220e29df6affbece4558c5f1259ecb/specs/CAPABILITY_LABELS.md#startup-taint)
is retained with the v2 capability keys and the following requirements:

- Autoscaler simulation-only labels, such as EKS node-template tags with
  `PropagateAtLaunch=false`, MUST match the profile inventory but MUST NOT
  advertise real-node readiness before provisioning. Pools that copy labels
  onto real nodes, such as Karpenter templates or AKS pool labels, MUST apply
  the startup taint before the node can accept workloads. An OnDemand
  template advertises `available`, not `ready`.
- The operator MAY claim a fresh startup-tainted node with pool-templated
  Brewlet labels only when it has no prior Brewlet profile advertisement and
  passes the existing UID-bound ownership checks. This is not permission to
  adopt an unmanaged installation or overwrite another profile's claim.
- Provisioner tolerations remain explicit in `NodeProfile`. Brewlet
  application pods MUST NOT tolerate the startup taint; admission rejects
  matching tolerations, including wildcard `Exists` tolerations, with
  `StartupTaintTolerationNotSupported`. This is a scheduling fence, not an
  execution authority: fail-open admission or an already-bound pod cannot
  bypass the shim's local authorization and generation checks.
- The authorized provisioner validates the baseline and eager roots,
  reconciles capability labels against the approved inventory, publishes the
  actual inventory and readiness, and only then removes the taint. Failure
  to remove it leaves the node unready and is reported explicitly. An
  OnDemand-only profile releases it without installing roots (§7.3).
- Providers MUST permit adding and removing this taint. AKS-managed Cluster
  Autoscaler scale-from-zero uses removable node initialization taints
  (currently preview), not pinned ordinary `--node-taints`; its pool labels
  are not simulation-only. EKS synthetic template labels and Karpenter
  `startupTaints` retain their distinct provider semantics. Provider recipes
  MUST use v2 keys and document initialization/reimage requirements rather
  than claiming that an approved profile alone enables scale-from-zero.

**Pinned labels and withdrawal.** An `Eager` entry MAY use provider-pinned
`ready` labels behind the startup fence. An `OnDemand` entry MUST NOT use a
provider-pinned per-ID capability label: `available`/`ready` transitions,
failure backoff, and idle reclamation require changing or removing that label.
Use mutable real-node labels with simulation-only templates where supported,
or use `Eager` on a provider that requires pinned capability labels.

On provisioning failure, invalidation, revocation, retirement, or quarantine,
the authorized controller/worker first attempts capability withdrawal. If
provider admission refuses label removal, it MUST apply and read back the
node-wide startup taint, retain the pinned labels, and report the refusal and
broader fence. Conflicts or Node replacement require a fresh UID/ownership
check, not a blind fallback write. If neither withdrawal nor fencing succeeds,
the operation remains blocked with an explicit error. Rotation completion and
deletion checkpoints MUST NOT treat an attempted write as confirmed fencing.
Local revocation/expiry and minimum-generation barriers remain enforced
independently of the label outcome.

The fallback blocks new scheduling for **all** Brewlet runtime IDs on the node,
not just the failed ID, and also affects ordinary pods without a matching
toleration. It does not evict running containers or discard leases.
Rotation status and events MUST report this broader impact (§7.6). Recovery
MUST revalidate ownership, authorizations, required roots, and every retained
capability against the current profile before releasing the fence. A pinned
label for an ID no longer offered, an invalid state/value, or an unsupported
OnDemand pin leaves the node fenced with `node-advertisement-failed` until the
platform owner corrects the pool configuration. Completing cleanup or uninstall
does not authorize removing this fence while stale pinned labels remain;
provider pool labels and initialization settings require explicit cleanup.

### 9.3 Architecture-aware placement

Removing `brewlet.sh/arch` does not make OCI platform validation a scheduler.
For native application content, workload authors MUST supply standard
`kubernetes.io/arch` node selection consistent with the explicitly published
platforms (§6.1, §11.1), for example:

```yaml
spec:
  template:
    metadata:
      annotations:
        brewlet.sh/runtime-image: python-3.12
    spec:
      runtimeClassName: brewlet
      nodeSelector:
        kubernetes.io/arch: arm64
      containers:
        - name: api
          image: registry.example.com/apps/api@sha256:<64 hex>
```

Required node affinity with `In ["amd64", "arm64"]` is the equivalent for a
multi-platform workload. Admission preserves these constraints and does not
deny solely because no matching architecture is currently ready: an eligible
profile can admit pending capacity, leaving provider templates and the
autoscaler to satisfy placement. Profiles do not infer application platforms.
Admission never fetches the artifact to derive or verify its architecture.
Without correct workload constraints, an unsupported placement can fail at
image pull; `ImagePullBackOff` does not itself request a different architecture
or reschedule the bound pod.

## 10. `RuntimeClass`

`RuntimeClass/brewlet` retains one handler, one scheduling selector, and one
overhead (SPEC §7). Its selector remains `brewlet.sh/runtime=ready`, with the
generalized readiness and startup-fence semantics of §7.3 and §9.1–§9.2.
Runtime image selection is per pod rather than per RuntimeClass
(see §15 for the rejected per-ID RuntimeClass alternative). `overhead.podFixed`
SHOULD be revisited, since 0.x sized it as "JVM/runtime baseline"; in 1.0.0 it
covers only shim and staging overhead.

## 11. What happens to Java

Removed from Brewlet core:

- JDK inventory, `javaHome`, and the `/opt/jdk` alias;
- the 0.x launch descriptor (`mainJar`, `entry`, `addOpens`, …) and its argv
  expansion rules, including the `jvm.args` entrypoint-selector guard;
- the JVM resource mapping of SPEC §10 (modern JVMs are container-aware);
- the `JavaApplication` CRD and controller, and `status.selectedJdk`.

Re-expressed generically:

| 0.x concept | 1.0.0 equivalent |
|---|---|
| JDK `temurin-21` in `NodeProfile.jdks` | `RuntimeImage java-21` built from the same digest-pinned JDK image |
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
brewlet build registry.example.com/apps/api \
  --platform linux/amd64 \
  --launch ./launch.json \
  --layer dependency=./venv/lib/python3.12/site-packages:/app/site-packages \
  --layer app=./src:/app/src
brewlet push registry.example.com/apps/api
```

The CLI MUST:

- validate `launch.json` against the schema and reject unknown fields;
- require explicit `--platform <os/arch[/variant]>` declarations, with no
  host-platform or language-based inference. Repeating this flag produces an
  image index with one manifest per platform. An unprefixed `--layer` asserts
  that its bytes work unchanged on every listed platform; a layer written
  `--layer <os/arch[/variant]>:<role>=<src>:<dest>` belongs only to that
  declared platform, and naming an undeclared platform fails with
  `InvalidPlatform`. Each platform receives its layer-set layers, then the
  applicable `--layer`s in command-line order, and MUST end up with an `app`
  layer. Layer sets are platform-neutral. Manifests produced elsewhere MAY be
  combined through an explicitly supplied index descriptor (`brewlet index`);
  no automatic native-file discovery or grouping occurs;
- enforce the layer content rules of §6.2 at build time (paths under `/app`,
  world-readable, no devices, setuid, or escaping links), producing the same
  reason codes the shim would;
- build deterministic layers in the given order, append the launch layer last,
  and mirror `launch.json` into the image config (§6.4);
- consume layer sets (§6.5) named with `--layer-set <ref>` by reusing their
  layers byte-for-byte.

`brewlet build` writes the result to a local OCI layout (by default
`.brewlet/images/<sanitized-ref>`, or `--store`); it never contacts a registry
except to read `--layer-set` inputs. `brewlet push <ref>` publishes that layout,
or any other local OCI layout given with `--store`, after verifying every blob
digest, and MUST NOT rewrite manifest, config or layer bytes. Build options are
rejected by `push`, so publication can never silently rebuild.

Determinism is for identical source bytes, modes, mtimes, descriptors, and
platform declarations: entries are sorted, ownership rendered as `0:0`, and
compression metadata is fixed. File modes and mtimes are preserved, not silently
normalized; invalid permissions and extended attributes are rejected before
publish. Layer entries never carry xattrs; on macOS the producer ignores only
the host-local `com.apple.provenance` and `com.apple.quarantine` attributes. Authors repair source permissions explicitly. This avoids changing
executable semantics or invalidating AppCDS metadata to make packaging pass.

The CLI MUST NOT inspect application content to derive any field: it never
detects a language, guesses an entrypoint, splits dependencies, chooses
`runtimeImages`, or writes workload manifests. What the author wrote is
what ships. `brewlet inspect <image>` prints the effective `launch.json` and
layer table so authors and reviewers can check the result.

`brewlet run <ref> --runtime <id>=<image>` is a local test aid. It applies the
§8.2 assembly to a built application and the runtime image the author names,
with Docker or Podman, honoring `launch.json` process fields. The mapping is
explicit (`runtimeImages` is optional, so `run` does not require the ID to be
listed). It MUST NOT publish or tag the merged result; the production
artifact remains the runtime-free application image.

`brewlet k8s` is an administrative convenience for cluster operators, not part
of packaging. Its `runtime add`, `runtime rotate` and `runtime remove`
subcommands create, re-pin and delete `RuntimeImage` resources (§7.1) through
the Kubernetes API, after the same preflight checks the controller would
perform, and can print the resource instead of applying it for GitOps.
`runtime remove` MUST refuse while workloads or node profiles still bind the
runtime unless explicitly forced. `list`, `status` and `doctor` are read-only.

### 11.2 The Maven plugin: Java-specific producer

The Maven plugin is the one place where Java knowledge remains. It MAY:

- generate `launch.json` argv from the project: main class or JPMS module,
  classpath or module path, `--enable-preview`, `--add-opens`, and system
  properties;
- split dependencies into layers (for example `/app/lib` and `/app/mods`) or
  consume a published `java` layer set;
- attach a prebuilt AppCDS archive as an ordinary application file and add the matching
  flags to argv;
- propose `runtimeImages` from `maven.compiler.release` and a configured
  mapping to inventory IDs (for example `21 → java-21`), so the
  allow-list reflects the bytecode level the project actually targets;
- warn when the generated argv requires a newer runtime than the configured IDs
  provide.

Its output is an ordinary 1.0.0 artifact that the shim treats exactly like one
produced by the CLI. A developer can always replace the generated `launch.json`
with a hand-written one. The plugin does not generate Kubernetes manifests and
does not write `brewlet.sh/runtime-image`; choosing the runtime for a workload
remains an explicit decision in the reviewed manifest.

## 12. Security model

- **Trust roots.** The only executable bytes outside `/app` are runtime roots
  extracted from administrator-approved, digest-pinned `RuntimeImage` sources,
  apart from explicitly mounted Kubernetes volumes. The application image is
  verified by digest exactly as in 0.x; ordinary-image sidecars cannot bypass
  this contract.
- **Runtime file-path confinement.** §6.2 confines application and dependency
  layers to `/app`, and §7.2 isolates and hides the runtime's `/app`. This
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
  `/opt/brewlet`, fail-open admission, and runc isolation retain their 0.x
  properties and caveats (SPEC §11).

## 13. Observability

- Shim event `RuntimeImageResolved` on each container start with ID, generation,
  inventory UID, and digest; the same values are exported as
  `brewlet_container_runtime_image_info{namespace,pod,pod_uid,container,container_id,id,inventory_uid,generation,digest}`
  from each node's live shim leases.
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
  Without metrics, `RuntimeImage.status.taskObservations` answers the same
  question with RBAC on one cluster-scoped resource: the operator copies each
  node's complete, fresh lease snapshot, keeping only bindings older than the
  approved generation or from a foreign epoch, with the pod namespace, name,
  UID, container, generation, and digest. The `OldGenerationTasks` condition
  is `True` while any such binding remains. Observations are retained, and
  marked not `Current`, when a node stops reporting, so silence never reads as
  "patched". They are diagnostics and restart-selection input (§7.6), never
  cleanup or revocation authority.
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
  `StartupTaintTolerationNotSupported`, `node-advertisement-failed`,
  `ownership-fence-failed`.
  `RuntimePATHRequired` identifies relative executable invocation without a
  runtime-supplied PATH.

### 13.1 Control-plane and node acceptance coverage

In addition to the producer/shim fixtures (§6.4), acceptance requires the
following integration scenarios. API/fake-client tests prove admission and
reconciliation decisions; real-node tests MUST additionally prove taint-based
scheduling, annotation propagation, installation, and generation enforcement.
Provider-pinned-label refusals MUST be exercised through an admission
interceptor or provider validation, not just by setting labels in a fixture.

| Scenario | Required outcome |
|---|---|
| Zero-node pool with valid inventory | Admit with affinity and a warning naming eligible profiles; exclude deleting, invalid, or conflicting profiles. Distinguish this API result from verified provider scale-from-zero. |
| OnDemand-only pool | Publish baseline readiness and `available` labels, release the startup fence without fetching roots, then install on first use and permit the retried container start. |
| Joining node with templated `ready` labels and startup taint | Do not count it as ready or schedule application pods there; claim only a fresh, unadvertised node under UID ownership fences. Reject application tolerations that bypass the fence. |
| Existing multi-term required/preferred affinity, `matchFields`, and node selectors | Preserve all author constraints; AND the runtime requirement into every OR alternative, keep an empty selector unsatisfiable, and append rather than replace warm-node preference. |
| Native application with no ready node of its required architecture | Preserve standard architecture selection, admit pending capacity, and schedule only on that architecture when it becomes available. An unconstrained unsupported pull must not be reported as successful autoscaling. |
| Pinned label removal/value-change refusal | Use and confirm the node-wide fallback for Eager withdrawal; reject readiness for an OnDemand pin or stale capability, and retain the fence through cleanup until pool configuration is corrected. |
| Both withdrawal and fallback-taint writes fail | Report blocked progress; do not complete rotation exclusion or deletion based on an unconfirmed fence. |
| Rotation/revocation behind retained pinned labels | Report node-wide exclusions for all affected IDs; preserve running leases and reject unauthorized or below-barrier starts even for already-bound pods or fail-open admission. Revalidate all affected inventory before releasing the fence. |
| Claim-release resource-version race or recreated Node | Re-read and revalidate Node UID, owner, and absence of runtime state before retrying a conditional release; never release a replacement/foreign claim. |
| Rotation restart selection with lease evidence | Restart a pod named in a retained older-generation binding even if it started after completion; skip a pre-completion pod only when a fresh, complete, post-start observation of its node lists no older binding; otherwise fall back to the start-time rule. |
| Operator restart writes under the chart admission policy | Allow only a `brewlet.sh/rotated-to-generation` change on opted-in Brewlet workloads; deny image, command, replica, label, opt-in, and other annotation changes, create/delete, non-opted-in workloads, protected namespaces, and namespaces outside a configured allowlist. Fail installation without policy support unless explicitly waived. |

### 13.2 Language-neutral workload acceptance

The qualification matrix MUST include Python, the single native-language
qualification fixture, and WebAssembly. Java, Node.js, .NET and PHP remain
example fixtures that use the same producer, artifact and explicit launch
contract; each runs the real-node runtime contract in its own non-gating daily
workflow against `main` (Java also through the Maven producer). Those runs are
not qualification evidence and do not gate pull requests, promotion or
release. A native-language result does not implicitly qualify a Wasm guest. The WebAssembly cases cover both the Wasmtime WASIp1
command and the Spin SDK HTTP component on the existing runc-backed path,
including actual guest execution, declared dependency reads, non-root host
identity, read-only payloads, guest access boundaries and explicit Exec/probe
semantics. A command example MUST NOT be labeled an HTTP service test.

Record native architecture, guest/toolchain and host/runtime source provenance
for every scope. Platform-built engine roots that preserve pinned upstream
engine binaries are not unchanged upstream engine OCI images. No native
runwasi backend, language detector or Wasm workload CRD is introduced.

Runtime/engine updates retain the same runtime inventory, generation, lease,
revocation and cleanup rules as the other workloads. Updating a separately
composed external payload cannot patch dependencies compiled into a `.wasm`
file; that requires a guest rebuild. Governed bundle composition, replacement
and observed dependency attribution are additionally qualified under
[§16.11.1](#16111-webassembly-composition-companion).

## 14. Open questions

These are deferred research directions, not supported alternatives or committed
implementation work:

- **Stronger sandbox isolation.** A backend such as gVisor or a VM-based sandbox
  needs a separate language-neutral design and measured compatibility, security,
  lifecycle, resource and runtime-sharing proof. It is not a validated binary
  substitution behind the current shim and must never silently fall back to runc.
- **Baked export.** Clusters that cannot install the shim might benefit from a
  conventional image combining an approved runtime with application layers.
  This needs explicit signing, launch/confinement and runtime-CVE rebuild/
  redeployment rules. It loses late-bound runtime selection and is distinct
  from §16, which composes application/dependency layers while leaving
  the runtime node-provided.

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

0.x runnable images use the `brewlet.sh/jvm-config` annotation. A dedicated
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

# Part II — Governed dependency composition and remediation

## 16. Managed dependency composition

- **Scope:** Brewlet 1.0.0 producer, release and policy workflows.
- **Extends:** [§6.5](#65-dependencies-as-layers-or-independently).
- **Does not change:** runtime-image selection, node provisioning, runc execution,
  startup fencing, leases or host cleanup.
- **Reference implementation:** `7077c1d3b7c2019ad10b8d3160f60a15dac9c930`.

The requirements below are the approved implementation contract. Qualification
must be recorded separately; code presence is not acceptance evidence.
This approval does not authorize a built-in vulnerability scanner, a runtime
dependency resolver or a new workload CRD.

### 16.1 Recommendation

Complete the platform-managed dependency experience around the existing generic
layer-set model:

1. The platform publishes an immutable dependency payload with a complete lock,
   SBOM and verifiable publisher provenance.
2. Application producers retain normal development workflows and publish
   explicit application artifacts.
3. Release tooling composes pinned application and dependency artifacts into
   one final application image, preserving application-layer bytes.
4. A separate release-builder identity attests the exact final composition.
5. Fleet inventory traces actual running image digests to dependency evidence.
6. Remediation replaces the dependency input, tests the new composition, and
   rolls native workloads to a new application digest.

The developer-facing delivery configuration can therefore name the application
and approved dependency set. The rendered Kubernetes workload still names
only the final application image and its explicit approved runtime ID.

An SBOM is an inventory, not executable content. The published dependency
artifact MUST contain the actual files; no package-manager resolution from
SBOM entries occurs in the shim, operator or generic composer.

### 16.2 Existing foundations and actual gaps

This table records the pinned historical rewrite baseline above. Implemented release
interfaces and their qualified scopes are documented in the
[composition guide](../docs/managed-composition.md) and the separate ledger.

| Concern | Already in 1.0.0 | Proposed work |
| --- | --- | --- |
| Dependency publication | Generic OCI layer sets, complete lock validation when a lock is supplied, mandatory SBOM, optional signed provenance. | A clear managed-publication workflow and release policy. |
| Java project intelligence | Optional Maven producer checks the explicit source BOM and resolved scoped graph, rejects nested JARs and generates argv. | Improve platform/application examples; retain these checks outside core. |
| Consumption | `push --layer-set` consumes verified dependency layers with exact digest/diff-ID reuse. | Consume an already-published application artifact without needing its original directories. |
| Composition evidence | `brewlet.sh/layerset-evidence`. | Final-image-bound, signed composition evidence and verification. |
| Operations | Native Kubernetes workloads and scoped execution evidence. | Running-fleet inventory, dependency remediation and end-to-end qualification. |

Main's managed bundle implementation is the behavioral reference, not a
scaffold to copy wholesale. In particular, main also composes before deployment;
its tier-19 update scenario starts from retained thin JARs, not a general
two-published-artifact composition command.

### 16.3 Goals and non-goals

#### Goals

- Make approved dependency consumption practical for platform and application
  teams without turning generic core into a language toolchain.
- Preserve compressed application and dependency layer descriptors and diff IDs
  across composition and dependency-only updates.
- Make the selected dependencies, producer identities and actual deployed
  image traceable and independently verifiable.
- Separate content integrity, publisher authorization, composition and
  application compatibility; none implies the others.
- Qualify the Python fixture with real independently published sets and a
  compatible unchanged upstream runtime image; other native languages are
  examples, not qualification scopes (§13.2).
- Qualify WebAssembly through both Wasmtime and Spin, preserving explicit guest
  capabilities and actual platform-built host provenance in a separate companion
  scope (§16.11.1); do not treat native-language receipts as implicit Wasm proof.

#### Non-goals

- A dependency reference that causes a node/shim to fetch another OCI artifact.
- A dependency inventory CRD, workload CRD, admission-time image rewrite or
  dependency restart controller inside the Brewlet operator.
- Maven/npm/pip/NuGet resolution or version-range interpretation in generic core.
- Hot replacement of libraries in running processes.
- Claiming binary/API compatibility from an SBOM, a version number or a signature.
- Restoring 0.x artifact formats or JVM-specific launch rewriting.
- A new generic producer plugin framework.

### 16.4 Roles and trust

| Role | Responsibility |
| --- | --- |
| Platform dependency publisher | Select and test the dependency closure, publish payload/lock/SBOM, sign its provenance. |
| Application producer | Build application bytes and explicit launch metadata; preserve producer-specific build evidence. |
| Release composer | Authorize pinned inputs, compose the output, sign what was actually selected. |
| Application deployment workflow | Approve/test the candidate and update its ordinary workload manifest. |
| Cluster policy | Verify required final-image evidence and identities before permitting deployment. |

These are artifact/publication roles, not two owners of the application
workload. Runtime infrastructure approval remains a separate platform concern.

Bundle-publisher keys and release-builder keys MUST represent distinct trust
decisions. A bundle signature does not authorize every application using it;
a composition signature does not prove that the application passed its tests.
Private keys stay in CI secret delivery, never in descriptors or app layers.

Managed composition MUST require an explicit trust policy. The production
policy requires trusted dependency publisher provenance and a signed output.
An explicitly selected development policy MAY permit unsigned inputs/outputs,
but MUST NOT label them production-verified.
Present invalid/untrusted provenance MUST NOT be treated as unsigned.
Key rotation MAY accept multiple keys under explicit identity/subject rules.

### 16.5 Initial bounded contract

Version 1 supports **one complete managed dependency set and one explicit
Linux platform per composition**. This matches the current Maven managed
workflow and keeps replacement unambiguous.

This restriction applies to the new composition operation, not to existing
generic `push` capabilities. Merging partial dependency catalogs, resolving
overlaps between independently governed sets and multi-platform composition
are not silently inferred. They require a subsequent explicit contract.

#### 16.5.1 Inputs

All remote inputs MUST be canonical digest-pinned references. Mutable tags
must be resolved and approved by the delivery workflow before invocation.

- An application input is a valid version-1 application OCI graph with
  explicit app layers, launch metadata and platform/config consistency.
- Initial composition accepts application inputs with no dependency-role
  layers. Structural validity does not mean that this payload image is a
  ready-to-run workload before dependency composition.
- Replacement from a previously composed image requires supported, verified
  composition evidence identifying its application and dependency descriptors.
  An unsigned annotation or a layer-role label alone is insufficient to decide
  which layers may be removed.
- Arbitrary previously bundled dependencies MUST be rejected rather than
  guessed, stripped or silently carried forward.
- Managed layer sets MUST provide a complete verified lock, exactly one valid
  SBOM and the provenance required by the selected policy. This is stricter
  than treating every otherwise-valid unlocked generic layer set as managed.
- Input platform selection MUST be explicit and unambiguous. Selecting a
  member of an index MUST verify its descriptor and relationship to that index.

An application payload can be built against dependency set A and later
delivered with set B. Original build provenance MUST continue to say A;
the composer MUST NOT rewrite it to claim the application was built or tested
against B.

#### 16.5.2 Layout and launch

The composer MUST:

- validate all input descriptors, sizes, hashes, diff IDs and current artifact
  constraints before producing a deployable output;
- use explicit layer roles and validated composition evidence, not filename,
  extension or language detection;
- preserve app-layer compressed bytes, order and diff IDs;
- reuse the selected dependency payload layers byte-for-byte;
- preserve launch-layer bytes by default and regenerate the OCI config's
  rootfs diff-ID list/config mirror for the actual output graph;
- keep optional top-level `runtimeImages` semantics unchanged;
- require an explicit `--launch` input for any requested launch replacement,
  validating and recording that replacement rather than inferring argv.

The composer MUST NOT move files to make a dependency layout fit a launch
descriptor automatically. For example, `/app/lib` is a producer contract, not
a Java-specific extraction rule in the composer.

Managed dependency paths MUST NOT be shadowed, removed by whiteouts or
redirected through symlinks by an application layer. The same existing safe
composition/path-resolution mechanisms SHOULD be reused to compare the
effective managed file inventory and hashes with the selected set.
Ambiguous collisions fail closed.

This is filesystem-level protection, not a language semantic guarantee.
It cannot prove that a JAR contains no shaded duplicate classes, that an
application's loader uses only approved files, or that a dynamic package is
API-compatible. Relevant producer validation and testing remain necessary.

#### 16.5.3 Output and publication

The output is an ordinary version-1 application OCI image: dependency payload,
preserved app payload and exactly one launch layer. Kubelet still pulls one
application image per container.

Equivalent pinned inputs and explicit options SHOULD produce the same output
manifest/config digests. Timestamps and signatures belong in referrers rather
than forcing otherwise-identical application layers to change.

No output tag, accepted manifest or successful release handoff may be published
after a validation or required-signing failure. Registry uploads of immutable
blobs may precede a failed operation; such unreferenced blobs are not a
successfully published release.
Credentials MUST remain origin-scoped during cross-registry copies/mounts.

### 16.6 Producer interface

The release interface is:

```sh
brewlet bundle registry.example.com/apps/orders:candidate \
  --application registry.example.com/builds/orders@sha256:<application-digest> \
  --layer-set registry.example.com/platform/java-web@sha256:<set-digest> \
  --platform linux/arm64 \
  --policy release-policy.yaml \
  --signing-key "$COMPOSER_SIGNING_KEY_FILE" \
  --builder-identity https://ci.example.com/release-composer \
  --push-result composed-image.json
```

`--application` is either an initial application payload or a previously
composed image satisfying §16.5.1. The supplied set is the complete desired
dependency closure, not an implicit subset update.
The signing environment variable contains a file path, not private key bytes.

The existing `build`/`push` and Maven workflows remain valid. Maven can continue to
derive the graph and launch from project inputs; the new operation is for
artifact-to-artifact release composition, not a replacement Maven resolver.

In a GitOps/release system, the two pinned references are pipeline inputs.
After verification and testing, it updates an ordinary Deployment image digest.
No proposed fields in this section are Kubernetes/Brewlet Pod annotations.

### 16.7 Final composition attestation

Define a versioned generic predicate, emitted as a signed OCI referrer and
bound to the exact output image manifest digest.
It MUST bind at least:

- application source root and selected platform-manifest digests;
- platform and preserved application-layer descriptors/diff IDs;
- selected dependency-set, config, lock and SBOM digests;
- dependency descriptors/diff IDs and their effective ordering;
- original/effective launch digests and output config digest;
- parent composition digest for replacement, where applicable;
- release-builder identity and predicate version.

This supplements existing layer-set publisher provenance. A generic verifier
must not need Maven GAVs, JAR inspection, Python package resolution or CLR
understanding to verify the final composition.
Java-specific graph/thin-JAR evidence can be bound as producer-owned evidence,
without becoming mandatory language-specific core fields.

An `inspect`/verification interface MUST distinguish unsigned structural
evidence, verified publisher provenance, verified composition provenance and
any separately verified test/release approval. It MUST NOT collapse these into
one ambiguous `verified` flag.

The verifier SHOULD be callable by external admission policy. Initial delivery
MUST NOT claim Brewlet 0.x's Ratify parity until a concrete integration is implemented,
its policy is documented and real accept/reject cases are exercised.
The inventory-only Brewlet admission path does not become a registry resolver.

### 16.8 Compatibility and dependency-only remediation

Layer replacement changes the deployed dependency closure, even when no
application code is recompiled. Therefore:

- initial Maven publication retains current exact graph/BOM/scoped-hash checks;
- generic replacement records old and new materials honestly;
- the application deployment workflow (§16.4) MUST run its configured
  compatibility/integration checks against the final candidate image digest
  before deploying a dependency-only release. Brewlet's composer and verifier do
  not execute these checks; they report `testReleaseApproval: not-evaluated`
  (§16.7) and MUST NOT infer a pass from a signature;
- changing package versions/hashes does not authorize changing runtime IDs,
  executable paths or process security implicitly;
- compatibility failures stop the release; they never fall back to locally
  resolving/bundling another graph or bypassing provenance;
- rollback selects a prior approved final image digest, subject to current
  policy; revocation does not grant an unsigned emergency bypass.

For Java, a retained thin JAR may work with a patched library without
recompilation. That must be tested; the old Maven build graph cannot be relabeled
as the newly tested graph. Equivalent ecosystem-specific responsibilities stay
in application CI/producer tooling for Python, Node and .NET.

### 16.9 Fleet inventory and remediation workflow

First provide a read-only inventory/export path that:

1. selects explicit namespaces/workloads;
2. records actual Pod UID/container ID and observed running image identity;
3. resolves/verifies the application OCI digest and composition evidence;
4. follows the exact dependency-set/SBOM bindings;
5. exports package/file identities, versions and PURLs with their provenance.

Do not assume every CRI `imageID` is the image manifest digest. Correlate
protected/observed identities and verified OCI membership rather than guessing
from a tag, desired Deployment spec or an unrelated config digest.

Missing evidence, inaccessible registries, invalid signatures and ambiguous
identity MUST be reported as **unverified**, not clean.
Namespace exclusions and stale snapshots must remain visible in the report.

Vulnerability evaluation consumes these facts through an explicitly configured
scanner/advisory source. Generic core MUST NOT apply one naive version comparator
to Maven, PEP 440, npm and NuGet ranges.
Fixture advisories can drive deterministic tests; they are not a production
vulnerability database.

The release workflow then publishes a replacement set, composes candidates,
tests them, updates native workload manifests and observes actual rollout.
Post-remediation inventory MUST show the new running application digests and
dependency identities. It MUST NOT report success from a template patch alone.
No dependency-update loop is added to the node provisioner.

### 16.10 Implementation milestones

| Milestone | Deliverable | Exit criteria |
| --- | --- | --- |
| P1: contract and producer experience | Review this contract; add concrete platform/application Maven projects and neutral publication examples. | Current mechanisms are documented/tested; new commands remain explicitly proposed until implemented. |
| P2: initial generic composition | App-payload plus one managed set, strict paths/platforms, byte-preserving output and deterministic result handoff. | No language inference; all malformed/collision/unknown-input cases fail; initial composition works locally. |
| P3: composition trust | Versioned predicate, signing, verification, key/identity policy and inspect output. | Subject/digest/order tampering and unsigned downgrade fail; signing failures publish no accepted output. |
| P4: dependency replacement | Accept verified prior compositions; replace the set while preserving app layers/launch unless explicitly changed. | Original build evidence stays intact; compatibility tests run against the new digest; no unverified layer stripping. |
| P5: fleet/remediation qualification | Read-only inventory, scanner integration boundary, release/GitOps example and real-node matrix. | Affected/unaffected/unverified cases, complete rollout observation and rollback/revocation behavior are proven. |

P3 depends on P2; P4 depends on P2/P3; P5's release scenario depends on P4.
Read-only inventory prototyping and P1 documentation can be developed separately.
No parallel orchestration or credit budget is authorized by this plan.

### 16.11 Required acceptance matrix

The final feature is not qualified by separate tests of its constituent parts.
For the native-language qualification case (Python, §13.2):

1. Publish an independent locked dependency set, SBOM and publisher provenance.
2. Publish an immutable application payload through its normal external
   toolchain/producer.
3. Compose and verify the final image; prove exact app/dependency layer and
   diff-ID reuse, launch/config consistency and conflict rejection.
4. Run on a pinned compatible **unchanged upstream runtime image** with actual
   HTTP/dependency, probes/Exec, identity, filesystem and resource observations.
5. Publish a patched set, recompose the unchanged application layers, test the
   new candidate and roll a native workload with controlled availability.
6. Observe the actual updated dependencies and image digests in running Pods;
   verify old processes are replaced normally rather than hot-patched.
7. Complete normal workload/profile cleanup and exact-owned fixture destruction.

Native arm64 local proof and native amd64 CI proof are separate scopes.
Cross-builds do not substitute for either. An unchanged runtime input must be
discovered and policy-qualified (for example, a reviewed chiseled candidate);
an adapted root must not be relabeled unchanged.
An unavailable compliant input remains an explicit matrix blocker.

#### 16.11.1 WebAssembly composition companion

WebAssembly is a required additional workload category, not implicitly covered
by the native-language case. Qualify **both** the Wasmtime WASIp1 command
and the Spin SDK HTTP component on native arm64 and native amd64 separately:

1. Publish a complete locked external dependency payload, CycloneDX SBOM and
   publisher provenance independently of the guest/application artifact.
2. Compose and verify a signed final image. Exercise required-trust and managed
   path-conflict rejection without publishing accepted output after failure.
3. Observe the actual initial guest and Wasmtime guest Exec or Spin HTTP/probe
   behavior, host Exec identity, non-root restrictions, read-only dependencies
   and exclusion of the runtime filesystem and trusted engine cache.
4. After original workloads are running, publish a changed external payload and
   recompose from the verified prior image. Preserve the exact `.wasm` file
   hashes, application/launch descriptors and diff IDs. Verify the old workloads
   retain the old dependency and the candidate actually consumes the new bytes.
5. Roll ordinary native Deployments only after candidate checks. Verify new
   container/Pod identities, unchanged selected host source/epoch/generation,
   real guest-visible dependency changes and pre/post observed inventory/SBOM
   attribution. Controlled zero-failed-request observation applies to Spin HTTP,
   not to a fabricated HTTP endpoint for the Wasmtime command.
6. Complete normal profile/task/root cleanup and exact-owned fixture destruction.

Record the host images' actual provenance. The existing examples use
platform-built roots combining pinned distroless C/C++ userland with unchanged,
checksum-pinned engine binaries; do not call these unchanged upstream engine
OCI images or weaken the native-language unchanged-root requirement.

Replacing external files/components that a guest explicitly consumes is not a
patch for code statically linked into a `.wasm` file. Compiled-in dependency
updates require a guest rebuild; host-engine/OS updates require runtime-image
rotation. Neither a fixture asset update nor an SBOM proves compiled-in library
remediation or complete application CVE coverage.

Additional mandatory cases:

- wrong BOM/graph/scope/classifier/hash, missing members and thin-JAR failures
  through the optional Maven producer;
- wrong keys/identities/subjects, malformed present provenance, expired/revoked
  trust policy and each allowed/denied signing combination;
- managed-path shadowing, whiteouts, symlink aliases and attempted file injection;
- source/output platform mismatch and ambiguous index membership;
- missing/invalid final composition evidence and no accepted output after failure;
- old/new source references and app-layer digests preserved across replacement;
- affected control/staging workloads and unknown images handled correctly;
- zero dropped requests for the deliberately configured availability scenario,
  not a universal zero-downtime product guarantee.

Keep a requirements-to-tests checklist for these obligations. Publish exact
inputs, commands, results and limitations; preserve failed runs as failed.
The [Part I baseline ledger](../acceptance/requirements.json) does not
qualify this feature.

### 16.12 Documentation and release boundaries

Publish an end-to-end platform-to-application guide, generic composition
reference, trust/attestation reference and remediation runbook.
Examples must use explicit platforms, immutable references and readable
configuration, not an invented runtime dependency annotation.

Document which source/app formats the new composer accepts and which it
rejects. Archive existing receipts rather than rewriting them as evidence for
new semantics. Keep installation/source-revision compatibility explicit.

This approval authorizes the bounded producer/release work,
not changes to the rejected shim-pulled dependency design (§15).

### 16.13 References

- [Main managed bundle workflow at the inspected revision](https://github.com/microsoft/brewlet/blob/fa7085335b220e29df6affbece4558c5f1259ecb/docs/managed-dependency-bundles.md)
- [Main bundle validation](https://github.com/microsoft/brewlet/blob/fa7085335b220e29df6affbece4558c5f1259ecb/core/internal/artifact/dependency_bundle.go)
- [Main CVE remediation scenario](https://github.com/microsoft/brewlet/blob/fa7085335b220e29df6affbece4558c5f1259ecb/integration-tests/e2e/tier19-cve-remediation.sh)
- [Historical rewrite Maven managed consumption](https://github.com/microsoft/brewlet/blob/7077c1d3b7c2019ad10b8d3160f60a15dac9c930/maven-plugin/README.md#managed-consumption)
- [Historical rewrite layer-set implementation](https://github.com/microsoft/brewlet/blob/7077c1d3b7c2019ad10b8d3160f60a15dac9c930/pkg/artifact/layerset.go)

## Appendix — mapping from the 0.x specification

This specification replaces the [0.x `SPECIFICATION.md`](https://github.com/microsoft/brewlet/blob/fa7085335b220e29df6affbece4558c5f1259ecb/specs/SPECIFICATION.md).
Each 0.x section maps to 1.0.0 as follows:

| 0.x section | Change | Source in this specification |
|---|---|---|
| SPEC §1–§3 | Reframe as language-agnostic | §1–§3 |
| SPEC §4 | Replace | §6 |
| SPEC §5.3–§5.4 | Replace | §7.1–§7.2 |
| SPEC §5.6 | Update `NodeProfile`, add on-demand install and startup fencing | §7.3, §7.5, §9.2 |
| SPEC §6.1 | Replace `Create` lifecycle | §8.2–§8.4 |
| SPEC §7 | Generalize readiness; retain handler/selector and revisit overhead | §9.1–§9.2, §10 |
| SPEC §8 | Add rotation restarts to the operator | §7.6 |
| SPEC §8.2, §9 | Remove `JavaApplication` | §11 |
| SPEC §8.3 | Replace admission rules; preserve affinity and native-platform placement | §9 |
| SPEC §10 | Remove JVM resource mapping | §11 |
| SPEC §11 | Add non-root enforcement | §8.4, §12 |
| SPEC §13 | Move AppCDS guidance to Maven plugin docs | §11 |
| SPEC §14 | Update reason codes, annotations, host layout, and acceptance coverage | §7.3, §13 |
| [CAPABILITY_LABELS](https://github.com/microsoft/brewlet/blob/fa7085335b220e29df6affbece4558c5f1259ecb/specs/CAPABILITY_LABELS.md) | New v2 key family and provider/startup-fence semantics | §9.1–§9.3 |
