# Brewlet specification

- **Status:** authoritative contract for the **breaking** Brewlet 1.0.0 implementation,
  not an additive mode of Brewlet 0.x. Implementation and qualification are
  distinct: see the [baseline ledger](../acceptance/requirements.json) (Part I),
  and the [scoped qualification records](../acceptance/RESULTS.md).
- **Release identity:** [`release.json`](../release.json).
- **Origin:** consolidates approved Proposal 0009 (Part I, §1–§15); section
  numbers are unchanged from that proposal.
- **Reference main:** `fa7085335b220e29df6affbece4558c5f1259ecb`.
- **0.x contracts retained and generalized:** runnable-image delivery
  (SPEC §4.4), copy-from-image node roots (SPEC §5.3),
  `NodeProfile` pool selection and rollout (SPEC §5.6), the runc-backed shim (SPEC §6),
  `RuntimeClass/brewlet` (SPEC §7), and the admission webhook (SPEC §8.3)

Brewlet 1.0.0 defines a language-agnostic core contract. Developers publish
**application artifacts** (code and any libraries or other dependencies as
ordinary application files) to an OCI registry. Platform teams approve
**runtime images** — digest-pinned "golden" base images such as a JRE, a Python
interpreter, a Node.js runtime, a .NET runtime, a native WebAssembly host such as
Wasmtime or Spin, or a distroless base for static binaries — in the
Brewlet inventory. Each managed container in a Pod selects its runtime image
**by ID only**, never by digest, through a Pod annotation named for that
container. `runtimeClassName: brewlet` is Pod-wide; containers without a
selection retain ordinary OCI behavior. The node-resident shim composes each
selected container's approved runtime with its application at launch time.

Bare `§N` references cite sections of this specification. References written
as `SPEC §N` instead cite the **0.x** design reference,
[`SPECIFICATION.md` at main commit `fa70853`](https://github.com/microsoft/brewlet/blob/fa7085335b220e29df6affbece4558c5f1259ecb/specs/SPECIFICATION.md),
which this document replaces.

Brewlet 1.0.0 is a rewrite inspired by Brewlet 0.x on `main`.
`SPEC §N` references provide design context only;
this document defines the implemented 1.0.0 contract for runtime images and
workload execution. This is not a claim of a stable production release.

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

# Part I — Language-agnostic runtime images

## 1. Summary

Brewlet 0.x separates a Java application from its JDK at launch time. On
inspection, almost nothing in that separation is Java-specific:

- A JDK "root" (SPEC §5.3) is already the **complete root filesystem** of an
  administrator-approved, digest-pinned image, copied read-only onto the node.
- The application image (SPEC §4.4) is already a set of ordinary
  `tar+gzip` layers plus a small launch descriptor.
- The shim (SPEC §6) already overlays the two and delegates to runc.

Brewlet 1.0.0 expresses this flow in language-neutral terms:

```text
 Platform operator                          Developer / CI
 ─────────────────                          ──────────────
 RuntimeImage "java-21"     ─┐              app layers   (/app/...)
 RuntimeImage "python-3.12"  │ approved     dep layers   (/app/...)  ← as layers, or
 RuntimeImage "static"       │ inventory    launch layer (launch.json)  reused from an
   each pinned to a digest  ─┘                                         independent
            ▼                                     ▼
 NodeProfile: which IDs on which pools     OCI registry (digest-addressed)
            │                                     │
            ▼                                     ▼
 Provisioner installs read-only            Pod: RuntimeClass/brewlet
 runtime roots per ID + generation          Per-container annotation:
                                             runtime-image.brewlet.sh/<name>: <id>
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
- **One core serves every ecosystem.** A generic runtime-image inventory and
  argv-based launch configuration apply across languages without per-language
  matching or flag parsing.
- **Runtime patching without application rebuilds.** Because pods name an ID
  rather than a digest, a platform team rotates the digest behind
  `python-3.12` once; new containers pick up the patched runtime as their nodes
  converge, without touching application repositories or manifests.
- **Shared runtime pages.** One read-only runtime root per ID per node is shared by
  every container that uses it, preserving Brewlet's density and pull-cost
  benefits for every language rather than only for the JVM.

## 3. Goals

- Define a language-agnostic **application artifact** consisting of application
  application layers, and a **launch configuration layer**.
- Define an approved **runtime image inventory** whose entries are identified by
  a stable, human-meaningful **ID** and resolved to a digest only by the platform.
- Let each explicitly selected container in a `runtimeClassName: brewlet` Pod
  name an approved runtime image ID through Pod metadata, with **no digest** in
  the workload, while leaving unselected containers as ordinary OCI workloads.
- Keep Kubernetes-native semantics: `args`, `env`, `workingDir`, probes,
  volumes, and resources retain their normal override behavior. Like a
  Dockerfile built `FROM` the runtime image, the runtime image `ENTRYPOINT` is
  always the argv prefix; Kubernetes `command` is ignored (§6.3).
  Intentional restrictions are runtime-owned `PATH`, non-root identities and
  no privilege escalation, synthesized `HOME`, and read-only packaged `/app`
  content (§8.2–§8.4).
- Guarantee that application layers cannot replace platform-owned runtime
  file paths; this does not patch application-bundled copies of libraries.
- Package application dependencies as ordinary application files; Brewlet does
  not resolve packages or publish dependency artifacts separately.
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
- A generic workload CRD. Deployments, StatefulSets, and DaemonSets are the
  workload API.
- Running workloads as root (§8.4).
- Inferring the runtime image from the artifact. Admission never reads the
  registry and never fills in per-container runtime selections; the workload
  manifest always states managed selections explicitly (§9).
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
| **Application artifact** | The developer's OCI image: application layers and one launch layer. |
| **Launch layer** | The layer carrying `launch.json`, the authoritative launch configuration (§6.3). |

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
3. exactly one **launch layer**, same media types, descriptor annotation
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
| Java | JARs and explicitly laid-out library files | A compatible JRE/JDK |
| Python | Source/bytecode and packaged dependency files | A compatible Python interpreter/userland |
| Node.js | JavaScript and packaged dependency files | A compatible Node.js runtime/userland |
| .NET | Assemblies, runtime configuration and packaged dependency files | A compatible CLR/userland, or an explicitly chosen root for self-contained output |
| WebAssembly | A core module or component, host configuration and other application files | A compatible native host such as Wasmtime or Spin and its complete userland |

For WebAssembly, the OCI platform still identifies the **Linux host
architecture**, not `wasi/wasm32`. Guest portability does not authorize inferred
platforms, inferred WASI interfaces, automatic engine selection or host launch
rewriting. The author explicitly declares the module/component ABI, engine
argv, guest preopens/mounts, environment and network capabilities through normal
producer/host configuration. Guest compilation and ecosystem-specific linking
remain outside Brewlet core.

### 6.2 Layer content rules

Application layers are ordinary rootfs-relative tars, but every entry MUST lie
under `app/` (that is, `/app` in the sandbox). The shim MUST
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

These rules protect **runtime-owned file paths**: no application layer can
replace `/usr`, `/etc`, or `/lib`. Applications can still bundle their
own libraries under `/app` or change language/library lookup through environment
variables. Runtime rotation does not remediate those copies; the application
team must rebuild them. This is file-path confinement, not a guarantee about
every library a process loads.
Violations fail the container with reason `LayerPathViolation`.

Layers stack in manifest order, so later application layers may overwrite
earlier files under `/app`. Producers SHOULD order layers from least to most
frequently changed to maximize reuse.
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
    "command": ["python"],
    "args": ["-m", "gunicorn", "app.wsgi:application", "--bind", "0.0.0.0:8000"],
    "workingDir": "/app",
    "env": [
      { "name": "PYTHONPATH", "value": "/app/src:/app/site-packages" },
      { "name": "PYTHONDONTWRITEBYTECODE", "value": "1" }
    ]
  }
}
```

Field contract (unknown fields MUST be rejected at publish and launch time):

| Field | Required | Rules |
|---|---|---|
| `schemaVersion` | yes | MUST be `1`. |
| `process` | yes | Object; it MAY be empty. |
| `process.command` | no | Application argv, the downstream equivalent of the leading part of Dockerfile `CMD`. If present and non-empty, `command[0]` MUST be non-empty. |
| `process.args` | no | Arguments appended after `command`. |
| `process.workingDir` | no | Absolute path; defaults to `/app`. |
| `process.env` | no | Ordered list of unique names. `PATH` is forbidden (§8.3). |

**Initial process composition.** Brewlet composes the initial argv as a
Dockerfile built `FROM` the runtime image would:

1. The application `CMD` is `process.command` followed by `process.args`. It is
   defined when either field is present, even as an empty array; an explicit
   empty value clears the inherited runtime `CMD`.
2. The effective `CMD` is the first of: non-empty Kubernetes `args`; the
   application `CMD`, when defined; the runtime image config `Cmd`.
3. argv is the runtime image config `Entrypoint` followed by the effective
   `CMD`. Kubernetes `command` is always ignored, so a workload cannot replace
   a runtime-owned `ENTRYPOINT`.
4. An empty argv, or an empty `argv[0]`, fails with `LaunchCommandRequired`.
   A non-absolute `argv[0]` requires the runtime image's `PATH` for sandbox
   resolution (runc semantics); absent PATH fails with `RuntimePATHRequired`
   (§7.2). argv is exact; no shell is inserted.

The runtime `Entrypoint` and `Cmd` come from the leased runtime generation, not
from the application artifact. Exec, probes, and lifecycle hooks run their own
argv and do not pass through the runtime `ENTRYPOINT`.

Workloads select an approved runtime explicitly. Authors are responsible for
testing applications against the selected runtime.

The launch configuration intentionally contains **no** language-specific fields.
Everything a 0.x `jvm.config` expressed (`mainJar`, `entry.mode`, `addOpens`,
system properties, preview flags, AppCDS hints) becomes plain argv. Outside the
Java ecosystem, the application author writes `launch.json` by hand and the CLI
packages it unchanged (§11.1); for Java projects the Maven plugin MAY generate it
(§11.2).

### 6.4 Image config

Producers MUST mirror the launch configuration into the image config so that
containerd produces a valid CRI process and the shim can verify it:

| Image config field | Value |
|---|---|
| `Entrypoint` | `["/.brewlet/runtime-entrypoint"]`, a placeholder for the runtime `ENTRYPOINT` |
| `Cmd` | `process.command` followed by `process.args`; absent when both are absent |
| `WorkingDir` | `process.workingDir` (or `/app`) |
| `Env` | `process.env` rendered as `NAME=value` |
| `User` | MUST be empty (§8.4) |
| `Labels["sh.brewlet.app"]` | `"1"` (informational only) |

The shim MUST verify that the config mirrors `launch.json` exactly and fail with
`LaunchConfigMismatch` otherwise. Because the config is authoritative only by
virtue of matching the launch layer, CRI's normal merge of the image config with
the pod spec (`env` and `workingDir` override) yields Kubernetes semantics for
those fields. The placeholder `Entrypoint` is never executed: the shim
recomposes argv from `launch.json`, the runtime generation, and the Kubernetes
`args` recorded by CRI (§6.3, §8.3).

**Canonical mirror.** Missing optional `env` renders as an empty array;
absent or null config arrays compare as empty arrays. A missing launch `workingDir` renders as `/app` (an empty config
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

`launch.json` remains the authoritative, versioned document inspected by tooling.
A future baked exporter would need to honor the same launch contract; no such
delivery mode is implemented by this specification.

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
- `Entrypoint` and `Cmd` — composed with the application launch as a
  downstream Dockerfile would (§6.3);
- nothing else. `WorkingDir`, `User`, `ExposedPorts`, `Volumes`, and
  `Healthcheck` are ignored; the application artifact and the pod own those.

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
`.brewlet-source` with the resolved digest.

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
  allow-list to forward `runtime-image.brewlet.sh/*` and
  `runtime-generation.brewlet.sh/*` into each application container's OCI
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
  annotation, `runtimeClassName: brewlet`, and at least one runtime selection
  for a declared container;
- is outside `operator.rotationRestarts.protectedNamespaces` (default
  `kube-system`, `kube-public`, `kube-node-lease`) and, when set, inside
  `operator.rotationRestarts.namespaces`;
- advances at most one `runtime-generation.brewlet.sh/<runtime-id>` barrier for
  a runtime selected by a declared container, leaving every other pod template
  annotation, the template labels and spec, the remaining workload spec, and
  the object's labels, annotations, owner references, and finalizers
  unchanged.

The operator thus cannot use restart RBAC to change images, commands,
replicas, or opt-in state, or to touch workloads that did not consent.
The chart requires Kubernetes 1.30 or later, where
`admissionregistration.k8s.io/v1` is available. The runtime-selection policy is
installed independently of the mutating webhook and rotation-restart opt-in.

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

**Selection.** The operator then considers each opted-in workload with a
declared container selecting this RuntimeImage ID, and restarts the whole Pod
template if at least one of its running pods is observed or presumed on an
older generation of this ID. Lease
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
`runtime-generation.brewlet.sh/<runtime-id>` with a JSON object
`{"id":"java-21","inventoryUID":"<RuntimeImage UID>","generation":7}`.
This is the same template-change mechanism as
`kubectl rollout restart`: the workload's own controller performs the rollout
and honors its update strategy (`maxUnavailable`, `maxSurge`, partitions,
`minReadySeconds`). The annotation also makes the patch idempotent: a workload
whose template already records the same ID, epoch, and current generation is
not patched again. Per-workload status is keyed by workload UID and target
epoch/generation so controller restarts cannot repeat a completed patch.

The annotation is also a **minimum-generation barrier**, propagated by CRI to
the shim. Its ID MUST match the corresponding container's selected runtime ID, its inventory UID MUST
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
`runtime-generation.brewlet.sh/<id>` on pod templates to avoid reporting drift.

## 8. Workload contract

### 8.1 Pod and container selection

A Pod uses `runtimeClassName: brewlet`. Each managed container selects its
runtime through a Pod annotation whose suffix is the exact container name:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata: { name: orders }
spec:
  template:
    metadata:
      annotations:
        runtime-image.brewlet.sh/orders: java-21
        runtime-image.brewlet.sh/telemetry: otel
    spec:
      runtimeClassName: brewlet
      containers:
        - name: orders
          image: registry.example.com/apps/orders@sha256:<64 hex>
          args: ["-Xmx768m", "-jar", "/app/orders.jar"]   # optional override of launch args
        - name: telemetry
          image: registry.example.com/apps/telemetry@sha256:<64 hex>
        - name: collector
          image: registry.example.com/otel-collector:latest
```

- The annotation key MUST contain a valid Kubernetes container name and its
  value MUST match the RuntimeImage ID grammar. Empty, malformed, or invalid
  selections fail closed; an absent selection means ordinary OCI execution.
- Selection is scoped to one Pod. Two Pods may use the same container name with
  different runtime IDs. Names MUST be unique among a Pod's regular, init, and
  ephemeral containers.
- A selected container's application `image:` MUST be digest-pinned, exactly
  as in 0.x (SPEC §4.4); the shim's protected CRI identity checks are unchanged.
- Only selected containers require Brewlet artifacts, inventory approval,
  numeric identities, read-only roots, and Brewlet mount/process policy.
  Unselected regular, init, sidecar, and ephemeral containers use ordinary OCI
  behavior through the same shim, including image tags and normal image-config
  defaults, subject to cluster policy.
- Container selections, including an absent selection, become immutable when
  the container is declared. A future managed ephemeral name may be reserved
  first by adding its selection annotation, then added through
  `pods/ephemeralcontainers`. A conventional debug container needs no
  reservation and remains ordinary OCI.
- A Pod with no currently selected containers is valid and still uses the
  Brewlet RuntimeClass handler-ready scheduling constraint.

### 8.2 Sandbox assembly (shim `Create`)

1. **Classify the container** from the protected CRI container name and that
   name's Pod selection annotation. An unselected container delegates unchanged
   to runc; it does not resolve an artifact or runtime.
2. **Resolve the artifact** for a selected container exactly as SPEC §6.4 does today (protected CRI metadata,
   content-store digest verification, strict platform match). Recognize a 1.0.0
   artifact by `brewlet.sh/artifact-version=1` and a final `brewlet.sh/layer=launch`
   layer; any other application container image fails with `UnsupportedArtifact`.
3. **Load and validate** `launch.json` (§6.3) and the image-config mirror (§6.4).
4. **Resolve the runtime image** by the protected container name from the Pod
   annotations, carried into the OCI runtime spec by CRI. Check live node-local authorization,
   revocation, and inventory epoch (§7.1); a cached root without current
   authorization cannot be used. If the ID is `available` on this node,
   request installation and wait as described in §7.5 (`RuntimeImageInstalling`
   or `RuntimeImageInstallFailed` on timeout or failure). Fail with
   `RuntimeImageNotInstalled` if the node's profile does not offer the ID at all.
   Enforce any `runtime-generation.brewlet.sh/<runtime-id>` minimum-generation barrier
   (§7.6), then atomically resolve and take a lease on `current` so a concurrent
   rotation cannot substitute an older generation or reclaim the leased root.
5. **Materialize and validate layers** from the content store using the 1.0.0
   per-layer-digest cache contract below.
6. **Assemble the rootfs** with an application-prefix boundary: launch-layer-excluded
   application layers (top to bottom in reverse manifest order)
   over a private opaque `/app` boundary over the runtime root. The boundary
   MUST hide all inherited runtime `/app` content without modifying the approved
   root or shared payload cache. `upperdir`/`workdir` = per-container scratch. The CRI
   selected container root MUST be read-only at execution. Admission defaults
   `readOnlyRootFilesystem: true` and rejects explicit false for selected
   application, init/sidecar and ephemeral containers. The shim independently rejects a
   non-read-only OCI root with `ReadOnlyRootRequired`; missing admission cannot
   restore writable-root behavior. Packaged `/app` is explicitly mounted
   read-only as well. Writable application data uses explicit Kubernetes
   volumes, not writes to a private copy of runtime files. This deliberately
   replaces the earlier writable-root/CRI-flag-preservation contract.
7. **Generate `config.json`** from the CRI-provided spec with the process fields
   of §8.3 and the user of §8.4; resources, mounts, namespaces, and the CNI
   network namespace pass through unchanged. No language-specific arguments are
   injected.
8. **Delegate to runc.** Signals, exit codes, and logs retain normal behavior.
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

- **argv** is the runtime `Entrypoint` followed by the effective `CMD` (§6.3).
  Kubernetes `args` replace the whole effective `CMD`; Kubernetes `command` is
  ignored, and admission emits a warning when a selected container sets it.
- **Working directory** is taken verbatim from the CRI-provided spec, which
  already reflects Kubernetes `workingDir` over the mirrored image config.
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
  absolute `command` paths.
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

Brewlet-managed containers never run as root. The default identity for each
selected container is its approved numeric `RuntimeImage.spec.user`,
independent of account names in the image. Unselected OCI containers retain
normal Kubernetes and image identity behavior, subject to cluster policy.

- The application image config `User` is empty, so CRI derives the process user
  from the pod `securityContext` alone for selected containers.
- Admission defaults each selected container's missing effective `runAsUser`
  and `runAsGroup` independently to that container's `RuntimeImage.spec.user`,
  respecting container-over-pod
  precedence, and sets `runAsNonRoot: true`. A custom UID without a group
  therefore uses the approved non-zero GID, not CRI's implicit GID 0.
- An explicit non-zero `runAsUser` is permitted (for example, platforms that
  assign UIDs per namespace). Files under `/app` are world-readable (§6.2), so any
  non-root UID can run the application.
- For selected containers, admission denies UID or primary GID 0, supplementary GID 0, and
  `runAsNonRoot: false` with
  `RootNotSupported`.
- For selected containers, admission defaults `allowPrivilegeEscalation: false`, drops all Linux
  capabilities, and rejects privileged containers, explicit privilege
  escalation, or added capabilities with `PrivilegeEscalationNotSupported`.
  The shim independently enforces `noNewPrivileges` and empty capability sets
  for initial and exec processes; ordinary runc privilege defaults are not
  sufficient to promise no root.
- **The shim enforces the rule independently for selected containers**: a process UID, primary GID,
  or supplementary GID of 0 in the CRI-provided spec fails `Create` or `Exec`
  with `RootNotSupported`. Because CRI yields
  UID 0 when no user is set, a selected container that bypassed fail-open
  admission fails closed rather than running as root.
- When the pod does not set `HOME`, the shim sets it from the runtime image's
  `/etc/passwd` entry for the process UID, or `/tmp` when the UID has no entry.
  This synthesized fallback is a deliberate Brewlet behavior, also applied to
  exec. A HOME string does not create a directory or promise writable storage.
  Numeric identities do not acquire fabricated passwd/group entries;
  `getpwuid`-based application APIs can fail. Software requiring a named account
  must use an image that provides one for the chosen identity.

## 9. Admission and scheduling

For every `runtimeClassName: brewlet` pod on CREATE, `brewlet-admission`:

- **Validates** every selection annotation against a declared container and
  requires each referenced `RuntimeImage` to exist and be approved. Missing
  selections mean OCI; malformed selections fail closed.
- **Steers** scheduling by ANDing a requirement on each selected runtime's
  capability label `brewlet.sh/runtime.<id>` `In ["ready", "available"]` into **every** existing
  required `nodeAffinity` selector term, creating a term only when no required
  selector exists. It MUST NOT add a separate OR alternative that bypasses the
  author's constraints; an explicitly empty selector MUST remain unsatisfiable.
  Admission preserves `matchFields`, `nodeSelector`, and existing preferred
  rules, and appends a preferred term (weight 100) on `In ["ready"]`, so pods land on nodes that
  already have the runtime installed whenever one fits and otherwise trigger an
  on-demand install (§7.5). Admission validates placement against active,
  approved `NodeProfile` inventory, not only live Node labels. At least one
  valid profile MUST offer every distinct selected ID; profiles that offer
  only disjoint subsets do not jointly authorize a Pod. Eligible profiles
  are non-deleting, pass source/mirror policy, and have no pool ownership
  conflict. It denies with
  `NoCompatibleRuntime` only when no valid profile offers the complete set; an empty pool
  yields an admitted Pending pod so its autoscaler can act. Autoscaler templates
  must publish the runtime selector and capability labels and follow the
  startup-taint/initialization contract in §9.2. Inventory approval does not
  itself prove that a provider's autoscaler template is correctly configured.
  When only profile inventory satisfies placement, admission warns with the
  profile names and leaves scheduling to Kubernetes and the configured scaler.
  This inventory check is not a guarantee that capacity satisfies all pod
  affinity, architecture, taints, resource, or volume constraints.
- **Defaults** only selected containers and denies root for them (§8.4), and
  **overwrites** `brewlet.sh/artifact-*` hints using a selected regular
  container (SPEC §8.3).
- **Does not** read the application artifact from the registry, and therefore
  never defaults per-container runtime selections from it. Artifact-side
  constraints (layer rules and platforms) are enforced by
  the shim and by kubelet's pull, so fail-open admission never weakens them.

The `brewlet.sh/jdk`, `brewlet.sh/jvm-args`, and `brewlet.sh/arch` annotations
are unsupported on 1.0.0 pods and MUST be rejected (`UnsupportedAnnotation`) so
a stale manifest fails loudly instead of silently losing its intent.

*Non-normative:* `brewlet-admission` does not restrict which namespaces may
use an approved ID. Platforms that need this (licensing, compliance tiers,
pilots) can add a standard admission policy; see the recipe in
`docs/runtime-image-namespaces.md`. It restricts use, not presence: runtime
bytes remain on shared nodes, and hard isolation requires separate node pools.

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

This section and §9.2–§9.3 define the 1.0.0 capability-label contract.

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
        runtime-image.brewlet.sh/api: python-3.12
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
Runtime image selection is per container rather than per RuntimeClass
(see §15 for the rejected per-ID RuntimeClass alternative). `overhead.podFixed`
covers only shim and staging overhead.

## 11. What happens to Java

Re-expressed generically:

| 0.x concept | 1.0.0 equivalent |
|---|---|
| JDK `temurin-21` in `NodeProfile.jdks` | `RuntimeImage java-21` built from the same digest-pinned JDK image |
| `jvm.config` | `launch.json` argv generated by the Maven plugin |
| Classpath / module-path layers | Ordinary application files under `/app/lib` or `/app/mods`, referenced from argv |
| Shipped AppCDS archive | An ordinary file in an application layer plus `-XX:SharedArchiveFile=… -Xshare:auto` in argv. Because rotations change the exact JDK build, the archive is best-effort by design. |

### 11.1 The `brewlet` CLI: language-agnostic packaging

The CLI is the producer for every ecosystem and knows none of them. The author
writes `launch.json` (§6.3) by hand and declares which directories become which
layers; the CLI validates and packages them:

```bash
brewlet build registry.example.com/apps/api \
  --platform linux/amd64 \
  --launch ./launch.json \
  --layer ./venv/lib/python3.12/site-packages:/app/site-packages \
  --layer ./src:/app/src
brewlet push registry.example.com/apps/api
```

The CLI MUST:

- validate `launch.json` against the schema and reject unknown fields;
- require explicit `--platform <os/arch[/variant]>` declarations, with no
  host-platform or language-based inference. Repeating this flag produces an
  image index with one manifest per platform. An unprefixed `--layer` asserts
  that its bytes work unchanged on every listed platform; a layer written
  `--layer <os/arch[/variant]>:<src>:<dest>` belongs only to that
  declared platform, and naming an undeclared platform fails with
  `InvalidPlatform`. Each platform receives its applicable `--layer`s in
  command-line order and MUST end up with an `app` layer. The CLI MUST assign
  the `app` role to every supplied payload layer automatically; the author
  supplies only its source, destination, and optional platform. Manifests produced elsewhere MAY be
  combined through an explicitly supplied index descriptor (`brewlet index`);
  no automatic native-file discovery or grouping occurs;
- enforce the layer content rules of §6.2 at build time (paths under `/app`,
  world-readable, no devices, setuid, or escaping links), producing the same
  reason codes the shim would;
- build deterministic layers in the given order, append the launch layer last,
  and mirror `launch.json` into the image config (§6.4);

`brewlet build` writes the result to a local OCI layout (by default
`.brewlet/images/<sanitized-ref>`, or `--store`); it never contacts a registry.
`brewlet push <ref>` publishes that layout,
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
a runtime, or writes workload manifests. What the author wrote is
what ships. `brewlet inspect <image>` prints the effective `launch.json` and
layer table so authors and reviewers can check the result.

`brewlet run <ref> --runtime <id>=<image>` is a local test aid. It applies the
§8.2 assembly to a built application and the runtime image the author names,
with Docker or Podman, honoring `launch.json` process fields. The mapping is
explicit. It MUST NOT publish or tag the merged result; the production
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
- package dependencies as ordinary application files (for example under
  `/app/lib` and `/app/mods`);
- attach a prebuilt AppCDS archive as an ordinary application file and add the matching
  flags to argv.

Its output is an ordinary 1.0.0 artifact that the shim treats exactly like one
produced by the CLI. A developer can always replace the generated `launch.json`
with a hand-written one. The plugin does not generate Kubernetes manifests and
does not write per-container runtime-selection annotations; choosing runtimes for a workload
remains an explicit decision in the reviewed manifest.

## 12. Security model

- **Trust roots.** The only executable bytes outside `/app` are runtime roots
  extracted from administrator-approved, digest-pinned `RuntimeImage` sources,
  apart from explicitly mounted Kubernetes volumes. The application image is
  verified by digest exactly as in 0.x; ordinary-image sidecars cannot bypass
  this contract.
- **Runtime file-path confinement.** §6.2 confines application layers to
  `/app`, and §7.2 isolates and hides the runtime's `/app`. This
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
- New reason codes: `InvalidRuntimeImageID`,
  `UnknownRuntimeImage`, `NoCompatibleRuntime`, `RuntimeImageNotInstalled`,
  `RuntimeImageInstalling`, `RuntimeImageInstallFailed`,
  `UnsupportedArtifact`, `LaunchConfigMismatch`, `LaunchCommandRequired`,
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
| Operator restart writes under the chart admission policy | Allow only one selected `runtime-generation.brewlet.sh/<id>` barrier change on opted-in Brewlet workloads; deny image, command, replica, label, opt-in, and other annotation changes, create/delete, non-opted-in workloads, protected namespaces, and namespaces outside a configured allowlist. |

### 13.2 Language-neutral workload acceptance

The qualification matrix MUST include Python, the single native-language
qualification fixture, and WebAssembly. Java, Node.js, .NET and PHP remain
example fixtures that use the same producer, artifact and explicit launch
contract; each runs the real-node runtime contract weekly against `main` as a
job of one non-gating matrix workflow (Java also through the Maven producer).
Python and the WebAssembly examples are also jobs of that workflow. Those runs are
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
revocation and cleanup rules as the other workloads. Updating application
dependencies requires rebuilding and republishing the application artifact.

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
  which would produce a conventional image containing the runtime and
  application files, rather than leaving the runtime node-provided.

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

A dedicated layer was preferred over a manifest annotation so the
configuration is a first-class, content-addressed blob with its own digest for referrers and tooling, and so the same artifact
shape serves registry-native and kubelet-pulled use. An annotation is equally
covered by the manifest digest, so this is a preference rather than a security
property.

### Digest in the pod annotation

Allowing `runtime-image.brewlet.sh/app: python-3.12@sha256:…` would give
reproducibility at the cost of the central value of the design: runtime
remediation without touching every workload. §7.4's dedicated IDs provide holds
without scattering digests.

### Allow application layers anywhere

Treating application layers like any image layer would support more existing
images, but would let an application shadow runtime files and silently defeat a
rotation. Confinement to `/app` is the property that makes digest-free runtime
references trustworthy.
