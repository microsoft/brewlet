# Brewlet — The JVM analogue to SpinKube

**Version:** 0.1

**Audience:** Platform engineers, Kubernetes operators, JVM platform owners

**Comparison point:** [SpinKube](https://www.spinkube.dev/), including
[Spin Operator](https://github.com/spinframework/spin-operator),
[Runtime Class Manager](https://github.com/spinframework/runtime-class-manager),
and
[containerd-shim-spin](https://github.com/spinframework/containerd-shim-spin) on
[runwasi](https://github.com/containerd/runwasi)

---

## 1. Vision & Problem Statement

Today, shipping a Java service to Kubernetes forces developers to become container
authors: pick a base image, write a `Dockerfile`, manage CVEs in the OS layer,
build and push a full container image, and keep the JVM inside that image patched.
The actual artifact they care about — a single self-executable (fat/uber) JAR — is
buried inside hundreds of megabytes of OS and JVM packaging that they did not write
and do not want to maintain.

SpinKube solves the analogous problem for Spin-compatible WebAssembly
applications. Developers deliver a Wasm application through OCI; Spin Operator
manages the workload, Runtime Class Manager manages the node-side runtime
integration, and containerd-shim-spin/runwasi executes it with a Wasm sandbox and
capability model.

> **Brewlet brings the same model to Java.**
> Developers push **their Java application** to their OCI registry — most commonly a
> single `app.jar`, but equally a layered classpath app or a JPMS module. A
> node-resident JVM runtime executes it directly (e.g. `java -jar app.jar`), inside an
> isolated sandbox with CPU/memory limits taken from the deployment descriptor. No
> Dockerfile, no base image, no container build step.

### 1.1 The SpinKube parallel

| Concern | SpinKube (Wasm) | Brewlet (JVM) |
|---|---|---|
| Developer workload | Spin-compatible Wasm application delivered through OCI | Existing JAR, layered classpath, or JPMS content delivered through OCI |
| Runtime location | Wasm runtime on the node | JDK/JVM distribution on the node |
| Node/shim lifecycle | Runtime Class Manager installs and configures containerd-shim-spin/runwasi | Brewlet operator and provisioner install the Brewlet shim and JDK |
| Workload control plane | Spin Operator reconciles Spin applications | Brewlet operator reconciles `JavaApplication` resources |
| Execution routing | `RuntimeClass` → containerd-shim-spin/runwasi | `RuntimeClass` → containerd-shim-brewlet-v2 → runc |
| Isolation model | Wasm sandbox and capability model | Linux container isolation delegated to runc |
| Primary tradeoff | Smaller footprint, fast startup, and low idle resource usage | Compatibility with existing JVM applications and tooling |

---

## 2. Goals & Non-Goals

### 2.1 Goals
- **G1 — Zero-Dockerfile delivery.** Developer publishes their Java application —
  a single self-executable JAR, a layered classpath app, or a JPMS module — as an
  OCI artifact and nothing else.
- **G2 — Native execution.** The JVM runs the application with a canonical
  invocation (e.g. `java -jar app.jar`); no wrapper semantics change.
- **G3 — Declarative resources.** CPU/memory limits, replicas, ports, and JVM
  options come from a Kubernetes deployment descriptor and are enforced via cgroups.
- **G4 — Declarative Kubernetes ergonomics.** Enabling the runtime is a single
  Helm install; deploying a workload is a single `JavaApplication` manifest or
  a standard pod manifest with `runtimeClassName: brewlet`.
- **G5 — Shared, patchable JVM.** The JDK is owned by the platform, lives on the
  node, is shared across workloads, and is upgraded independently of app artifacts.
- **G6 — First-class Kubernetes citizen.** Services, Ingress, probes, HPA, logs,
  and metrics all work unchanged.

### 2.2 Non-Goals (for v1)
- Replacing OCI *images* for apps that legitimately need OS packages/native deps.
- Building JARs (CI's job). Brewlet only *runs* the application.
- Multi-language polyglot runtimes (Python, Node). JVM-only for v1.
- Live migration of running JVMs between nodes.

---

## 3. High-Level Architecture

```
 Developer / CI             Control Plane                      Worker Node (provisioned)
 ──────────────             ─────────────                      ─────────────────────────

  mvn package               ┌────────────────────────┐         ┌────────────────────────────┐
  oras push ──┐             │ brewlet-operator       │ watches │ Node                       │
              │             │ - node provisioning    │──────►  │  ┌──────────────────────┐  │
              ▼             │ - RuntimeClass mgmt    │annotates│  │ containerd           │  │
       ┌──────────────┐     │ - JavaApplication CRD  │         │  │ + shim:              │  │
       │ OCI Registry │     └───────────┬────────────┘         │  │ containerd-shim-     │  │
       └──────┬───────┘                 │ generates            │  │ brewlet              │  │
              │                         ▼                      │  └──────────┬───────────┘  │
              │               ┌─────────┬──────────┐ scheduled │             │ runc         │
              │               │ Deployment / Pod   │ ────────► │             ▼              │
              │               │ runtimeClassName:  │           │  ┌──────────┬───────────┐  │
              │               │   brewlet          │           │  │ Sandbox              │  │
              │               └────────────────────┘           │  │ (cgroup + netns)     │  │
              │                                                │  │ java -jar            │  │
              └─────── shim pulls pod image ──────────────►     │  │ /app/app.jar         │  │
                                                               │  │ (node JDK RO)        │  │
                                                               │  └──────────────────────┘  │
                                                               └────────────────────────────┘
```

### 3.1 Component inventory

1. **OCI Application Artifact format** (§4) — how a Java application (a JAR, or an
   app split into classpath layers) is packaged/pushed/pulled.
2. **`brewlet-node-provisioner`** (§5) — privileged DaemonSet/Job that installs the
   shim binary and one or more JDK distributions onto nodes, then registers the
   containerd runtime.
3. **`containerd-shim-brewlet-v2`** (§6) — containerd Runtime v2 shim that assembles
   a sandbox from the node JDK + the JAR layer and launches `java -jar`.
4. **`RuntimeClass/brewlet`** (§7) — routes pods to the shim handler.
5. **`brewlet-operator`** (§8) — combines the node/shim lifecycle role analogous
   to Runtime Class Manager with reconciliation of the higher-level
   `JavaApplication` CRD into Deployments.
6. **`JavaApplication` CRD** (§9) — the developer-facing deployment descriptor.

---

## 4. The OCI Application Artifact

The Java application ships in one of two OCI formats (OCI Image Spec ≥ 1.1), and
in **neither** is there an OS layer or a JVM inside it — only the application
payload (a fat JAR, or dependency/module layers) plus a small JSON config
describing how to launch it:

- a **runnable image** (§4.4) — a standard, kubelet-pullable OCI image. This is
  the **default** for `brewlet push` and the Maven plugin, and the format
  Kubernetes workloads use.
- a **native artifact** (this section) — registry-native custom media types, *not*
  runnable by containerd, retained for local OCI-layout / CLI / `prepare-bundle`
  workflows.

The launch contract below is shared by both; §4.4 describes how a runnable image
carries it.

### 4.1 Media types

| Component        | Media type                                          | Contents                          |
|------------------|-----------------------------------------------------|-----------------------------------|
| Artifact type    | `application/vnd.brewlet.app.v1+json`               | Manifest `artifactType`           |
| Config blob      | `application/vnd.brewlet.jvm.config.v1+json`        | Launch descriptor (below)         |
| Payload layer    | `application/vnd.brewlet.jar.layer.v1+jar`          | The raw self-executable JAR       |
| (optional) layer | `application/vnd.brewlet.classpath.layer.v1+tar`    | Dependency JARs; unpacked to `/app/lib` for layered class-path deployment ([docs](https://github.com/microsoft/brewlet/blob/main/docs/layered-classpath-deployment.md)) |
| (optional) layer | `application/vnd.brewlet.modulepath.layer.v1+tar`   | Library modules for a modular (JPMS) app; unpacked to `/app/mods` and fed to `--module-path` ([docs](https://github.com/microsoft/brewlet/blob/main/docs/jpms-support.md)) |
| (optional) layer | `application/vnd.brewlet.cds.layer.v1+jsa`          | AppCDS class-data archive paired with the config's `cds.archive`; mounted read-only at `/app/<archive>` and consumed with `-Xshare:auto` (§13, [docs](https://github.com/microsoft/brewlet/blob/main/docs/appcds.md)) |

**Managed dependency bundles (§4.5)** add a second artifact family:

| Component        | Media type                                            | Contents                        |
|------------------|-------------------------------------------------------|---------------------------------|
| Artifact type    | `application/vnd.brewlet.dependencies.v1+json`        | Manifest `artifactType`         |
| Config blob      | `application/vnd.brewlet.dependencies.config.v1+json` | Bundle identity + layer binding |
| Lock blob        | `application/vnd.brewlet.dependencies.lock.v1+json`   | Canonical approved inventory    |

**Well-known annotations and labels on published artifacts:**

| Key | Where | Meaning |
|---|---|---|
| `brewlet.sh/managed-dependency-evidence` | Application manifest annotation | JSON evidence binding an application to the approved dependency bundle it consumed (§4.5) |
| `sh.brewlet.runnable` | Runnable **image** config label, value `"true"` | Marks a §4.4 runnable image. Informational: the shim derives its authoritative target from CRI/containerd metadata and never trusts this label for admission or execution |

### 4.2 Launch config (config blob) schema

```json
{
  "schemaVersion": 1,
  "mainJar": "app.jar",
  "entry": {
    "mode": "jar",                       // "jar" => java -jar; "classpath" => java -cp ... Main; "module" => java -p ... -m module
    // classpath mode adds: "mainClass" (required) and optional "classPath"
    // (e.g. ["app.jar","lib/*"]) for layered classpath deployment.
    // module mode adds: "module" (required) and optional "mainClass" + "modulePath"
    // (e.g. ["orders.jar","mods"]) for a JPMS module path — see the JPMS support docs.
    // jar mode must NOT carry mainClass/classPath (the manifest Main-Class wins).
  },
  "enablePreview": true,
  "addOpens": ["java.base/java.lang=ALL-UNNAMED"],
  "systemProperties": { "spring.aot.enabled": "true" },
  "env": []
}
```

- The launch config records the artifact's launch contract only. JDK feature +
  distribution and launcher are specified once in the deployment descriptor
  (`spec.jvm.version` / `spec.jvm.distribution` / `spec.jvm.launcher` on
  `JavaApplication`, or `brewlet.sh/jdk` / `brewlet.sh/launcher` pod annotations
  for raw Deployments). The artifact is deployment-agnostic.
- The artifact field set is exactly `schemaVersion`, `mainJar`, `entry`,
  `enablePreview`, `addModules`, `addOpens`, `addExports`, `systemProperties`,
  `env`, and the two optional constraints/hints `arch` (an architecture
  constraint for non-portable/JNI JARs, steering `kubernetes.io/arch`
  nodeAffinity — §14) and `cds` (an AppCDS archive hint pairing the artifact with
  its `cds.layer.v1+jsa` layer — §13). Ports and process credentials are
  deployment concerns, not part of the artifact. Consumers MUST reject a config
  containing a `user` field.
- The optional `cds` object has exactly two fields. `cds.archive` (required when
  `cds` is present) is the bare filename the paired `cds.layer.v1+jsa` layer is
  materialized as under `/app`. `cds.mode` is optional and records how the
  archive was produced — `"dynamic"` (`-XX:ArchiveClassesAtExit`) or `"static"`
  (`-Xshare:dump`). `cds.mode` is **informational only**: consumption is
  identical either way, so an omitted value is not a defect. Any other value is
  rejected, like every other unknown field or mode.
- Non-empty `mainJar` values and `cds.archive` MUST be bare filenames (no path separator, no
  wildcard, no `..`). Both name files that a node materializes under a per-image
  staging directory and then bind-mounts read-only into the sandbox, so
  producers MUST reject a non-bare value at publish time and consumers MUST
  reject it at load time and confirm the resolved path is contained by the
  staging directory.
- An omitted or empty `mainJar` means `app.jar` for both publication and
  consumption, independent of the source JAR's filename. Producers that choose
  another name MUST record it explicitly in the launch config.
- The descriptor's launcher selects the JVM launcher that fronts the entrypoint. It is **generic
  and OpenJDK-neutral**: omitted (or `"java"`) means the stock `java` launcher from
  the selected JDK. Brewlet injects **no JVM tuning flags** in either case — the
  container-aware JDK reads the sandbox cgroup limits and the user supplies any
  tuning via descriptor `jvm.args`. Any other name (e.g. `jaz`, the [Azure
  Command Launcher for Java](https://learn.microsoft.com/java/jaz/overview)) is a
  **node-installed, drop-in java-compatible launcher** that additionally auto-tunes
  the JVM from the cgroup limits on the user's behalf. A launcher is a separate node
  package (not part of any JDK), so it composes over any OpenJDK distribution. If the
  requested launcher is not installed on the node, the pod fails to admit (event
  `NoCompatibleLauncher`).
- Artifact launch knobs (`enablePreview`, `addModules`, `addOpens`, `addExports`,
  `systemProperties`) carry app-intrinsic correctness flags. Deployment tuning
  (heap, GC, agents, container flags) belongs in descriptor `jvm.args`, which is
  applied after the artifact knobs and before the entrypoint. Because the JVM
  resolves conflicting options last-wins, deployment tuning therefore **overrides**
  an app-embedded flag — a platform team can always win.
- Launch expansion order is: `-Xshare:auto` + `-XX:SharedArchiveFile` (only when
  the artifact ships an AppCDS archive and the deployment has not opted into
  node-side regeneration — §13), `--enable-preview`, `--add-modules`,
  `--add-opens`, `--add-exports`, sorted `-D` system properties, descriptor
  `jvm.args`, then the entrypoint (`-jar`, `-cp … <MainClass>`, or `-p … -m …`).
- Descriptor `jvm.args` reach the JVM as **argv**, not as an options environment
  variable. They ride the pod on `brewlet.sh/jvm-args` (§8.2) and the shim appends
  them at the position above. Two consequences are contractual:
  - Argument boundaries are preserved, so a flag whose value contains a space
    (e.g. `-XX:OnOutOfMemoryError=kill -9 %p`) is delivered as one argument.
  - `jvm.args` MUST NOT select the entrypoint. `-jar`, `-cp`/`-classpath`/
    `--class-path`, `-p`/`--module-path`, `-m`/`--module` (including their
    `--flag=value` forms) and `@argfile` are rejected, because `java` stops
    parsing options at the first entrypoint selector and one injected here would
    silently take over the launch. The artifact owns the entrypoint. This is a
    well-definedness rule, not a privilege boundary: the pod author already
    controls the image contents.
- **Mode owns its fields.** Each `entry.mode` uses a fixed set of fields and
  fields foreign to the selected mode are rejected rather than silently ignored:
  `jar` mode must not carry `mainClass`/`classPath` (the manifest `Main-Class`
  wins), `classpath` mode requires `mainClass`, and `module` mode requires
  `module` and additionally permits `classPath` (the **mixed form**: a
  supplementary `-cp` alongside the module path). Unknown modes and foreign-mode
  fields are rejected by the Maven plugin at build time and by the launch core at
  publish and launch time; unknown JSON *fields* are additionally rejected by the
  launch core, which parses configs with strict field checking (publish and launch
  time). Because unknown fields are rejected, old artifacts whose `jvm-config.json`
  still contains `jdk`, `launcher`, `labels`, or legacy free-form JVM args must be
  re-pushed. The Maven plugin
  generates the config from typed models, so it validates mode/field consistency
  rather than parsing arbitrary JSON.

### 4.3 Build & publish flow (developer experience)

```bash
# 1. Build the fat JAR as usual — nothing Brewlet-specific.
mvn -q clean package            # → target/app.jar

# 2. Author (or let the CLI generate) the launch config.
cat > jvm-config.json <<'EOF'
{ "schemaVersion": 1, "mainJar": "app.jar",
  "entry": { "mode": "jar" } }
EOF

# 3. Push as an OCI artifact with ORAS (no docker build!).
oras push registry.example.com/team/app:1.4.2 \
  --artifact-type application/vnd.brewlet.app.v1+json \
  --config   jvm-config.json:application/vnd.brewlet.jvm.config.v1+json \
  target/app.jar:application/vnd.brewlet.jar.layer.v1+jar
```

The [Brewlet Maven plugin](../maven-plugin) (`mvn brewlet:push`) wraps steps 2–3
so developers never touch ORAS directly, **including registry publication**. The
`brewlet` CLI (`brewlet push ./target/app.jar <ref> --store ./oci`) wraps step 2
and writes the result to a local **OCI layout**; it has no registry client, so
publishing to a registry is done by the Maven plugin or ORAS. Registry
publication from the Go CLI is [roadmap](../ROADMAP.md) work.

### 4.4 Runnable-image delivery mode (kubelet-pullable, the SpinKube-style pull path)

The native artifact above is **registry-native but not runnable by containerd**: its
custom layer media types (`…+jar`, `.classpath+tar`, `.modulepath+tar`) are not
`tar`/`tar+gzip`/`tar+zstd`, so containerd's CRI differ cannot unpack them. A pod that
names such an artifact as its `image:` therefore fails to pull (`ImagePullBackOff`),
and the Kubernetes runtime path rejects native artifacts even if their blobs were
delivered out of band. Native artifacts remain available to explicit local
OCI-layout / CLI / `prepare-bundle` workflows. Kubernetes workloads must use a
runnable image so a `runtimeClassName: brewlet` pod can simply set `image: <ref>`
and let kubelet pull it, as SpinKube does for a Spin-compatible Wasm application
routed to containerd-shim-spin.

**Runnable-image mode** closes this gap without changing the native format. `brewlet
push --format=image` (and the Maven plugin's `mvn brewlet:push -Dbrewlet.format=image`)
publishes the *same* JAR as a **standard, kubelet-pullable OCI image**:

- a real `application/vnd.oci.image.config.v1+json` config (with `rootfs.diff_ids`
  over the **uncompressed** layer tars, as the OCI image spec requires);
- **`application/vnd.oci.image.layer.v1.tar+gzip`** layers — the app JAR (plus an
  optional AppCDS `.jsa`) in one layer, and the same classpath/modulepath tars a
  native artifact would ship as additional layers, each tagged with its role via a
  `brewlet.sh/layer` annotation (`app` / `classpath` / `modulepath`);
- the launch config (§4.2) carried verbatim in the manifest annotation
  **`brewlet.sh/jvm-config`** rather than as a config blob;
- published as a **multi-arch OCI image index** (default `amd64` + `arm64` for a
  portable bytecode JAR, so any provisioned node matches; narrowed to `--arch` for a
  JAR carrying native libraries).

containerd/kubelet pull and unpack this image with **no special configuration**.
Kubernetes execution requires a digest-pinned request. The shim takes that exact
manifest/index target from protected CRI requested-image metadata, requires the
containerd-owned `io.kubernetes.cri.image-name` OCI annotation to name the same
target, resolves it directly from the content store, and verifies that the
platform manifest selected by containerd's strict OS/architecture/variant
matcher (without an unmatched fallback) has a config digest equal to CRI's
recorded image-config identity. It then recognizes a runnable image (the presence of
`brewlet.sh/jvm-config` ⇒ runnable image), reads the launch config from the
annotation, and assembles the same `java -jar`/`-cp`/`-p -m` sandbox on the
node-resident JDK it would for a native artifact — the layers are gunzipped and
fed to the existing bundle-assembly path unchanged. The admission webhook overwrites
`brewlet.sh/artifact-ref` and `brewlet.sh/artifact-digest` compatibility hints from
the selected Pod image (here the image-index digest), while the shim independently
derives the authoritative target from containerd metadata.

Runnable-image mode is the **default** delivery format for `brewlet push` and the
Maven plugin, since it fulfils the pure `image: <ref>` promise end to end. Native
artifact mode (`--format=artifact` / `-Dbrewlet.format=artifact`) remains available
for local OCI-layout / CLI workflows that want the registry-native, smallest,
no-OS-image framing and its self-describing media types. See
[`docs/runnable-image.md`](https://github.com/microsoft/brewlet/blob/main/docs/runnable-image.md)
for the full contract. The kubelet-pull → unpack → shim-run path is covered on a
live node by the end-to-end test suite.

### 4.5 Managed dependency bundles

Managed dependency bundles let a platform team publish an approved classpath
independently from application code while retaining a self-contained,
kubelet-pullable final application image. The Maven BOM remains a development
and publication input; Kubernetes never resolves or references it.

An Ops-owned bundle project imports the approved BOM, explicitly declares bundle
membership, and publishes:

| Component | Media type | Purpose |
|---|---|---|
| Artifact | `application/vnd.brewlet.dependencies.v1+json` | Identifies the managed dependency-bundle contract. |
| Config | `application/vnd.brewlet.dependencies.config.v1+json` | Binds name/version/source BOM, lock digest, compressed layer digest, uncompressed diff ID, and compatible JDKs. |
| Layer | `application/vnd.oci.image.layer.v1.tar+gzip` with `brewlet.sh/layer=classpath` | Deterministic flat dependency-JAR tar, directly reusable by a runnable image. |
| Lock | `application/vnd.brewlet.dependencies.lock.v1+json` | Canonical ordered GAV, filename, scope, classifier, and SHA-256 inventory. |

#### Version 1 wire contract

The requirements in this subsection are normative. A version 1 bundle manifest
MUST be an OCI image manifest with `schemaVersion: 2`,
`mediaType: application/vnd.oci.image.manifest.v1+json`, and
`artifactType: application/vnd.brewlet.dependencies.v1+json`. It MUST contain:

1. one config descriptor with media type
   `application/vnd.brewlet.dependencies.config.v1+json`;
2. one dependency layer descriptor with media type
   `application/vnd.oci.image.layer.v1.tar+gzip` and annotation
   `brewlet.sh/layer=classpath`; and
3. one lock descriptor with media type
   `application/vnd.brewlet.dependencies.lock.v1+json`.

Every descriptor digest and size MUST match the referenced bytes. Unknown JSON
fields, duplicate dependency coordinates, duplicate filenames, additional
classpath layers, and additional lock documents MUST be rejected.

The config document has this exact field contract:

```json
{
  "schemaVersion": 1,
  "name": "approved",
  "version": "2026.08",
  "sourceBom": "com.example.platform:approved-spring-boot-bom:2026.08",
  "lockDigest": "sha256:<64 lowercase hex characters>",
  "layerDigest": "sha256:<64 lowercase hex characters>",
  "layerDiffId": "sha256:<64 lowercase hex characters>",
  "compatibleJdks": [21, 25]
}
```

`name`, `version`, and the `G:A:V` `sourceBom` are required.
`compatibleJdks` is optional; when present it MUST contain unique positive
integers. Publishers MUST serialize it in ascending order. `lockDigest`
identifies the lock document,
`layerDigest` identifies the compressed layer, and `layerDiffId` identifies the
uncompressed tar stream.

The lock document has this exact field contract:

```json
{
  "schemaVersion": 1,
  "artifacts": [{
    "groupId": "org.example",
    "artifactId": "library",
    "version": "1.2.3",
    "type": "jar",
    "classifier": "tests",
    "scope": "runtime",
    "fileName": "library-1.2.3-tests.jar",
    "sha256": "<64 lowercase hex characters>"
  }]
}
```

`classifier` is omitted when empty. All other artifact fields are required.
`fileName` MUST be a flat `.jar` filename without path components. Publishers
MUST order entries lexicographically by
`groupId:artifactId:type:classifier:version`; consumers MAY normalize this order
before comparison. Coordinates and filenames MUST each be unique. `sha256` is
the digest of the individual JAR bytes and does not include the `sha256:`
prefix. Scope is part of the version 1 graph identity: consumers MUST reject a
scope difference even when the artifact coordinate and bytes otherwise match.

The uncompressed dependency layer MUST be a flat USTAR archive containing
exactly one regular file for every lock entry and no other entries. Directory,
link, device, and path-traversal entries are forbidden. Each file's bytes MUST
match its lock digest. Canonical publishers order files by `fileName`, use mode
`0644`, UID/GID `0`, mtime `0`, and omit build-time timestamps from gzip.
Consumers MUST validate the safe flat-file shape and contents, but MUST NOT
depend on tar metadata or compressed-byte identity beyond the digests declared
by that bundle.

The managed layer deliberately uses the standard gzip OCI layer type rather than
the native artifact's uncompressed
`application/vnd.brewlet.classpath.layer.v1+tar`. Runnable images require
standard compressed layers; using that representation in the bundle lets the
publisher cross-repository-mount or upload the exact blob and lets containerd
deduplicate it by digest.

At application publication, Brewlet:

1. resolves and validates the bundle;
2. verifies the classpath tar against its dependency lock;
3. compares the application's resolved runtime dependency lock exactly with the
   bundle lock;
4. rejects an application JAR containing nested dependency JARs;
5. composes the thin application layer and existing managed classpath descriptor
   into a runnable image; and
6. records canonical managed-dependency evidence binding the final image to the
   application JAR, source BOM, bundle, classpath layer, and lock digests.

The launch config uses `entry.mode=classpath` and
`classPath=[mainJar, "lib/*"]`; the existing shim path extracts the managed layer
under `/app/lib`. `JavaApplication` remains unchanged and references only the
complete application image digest.

#### Supply-chain referrers

Bundle publication always creates an SBOM referrer and may create a provenance
referrer whose subject is the immutable dependency-bundle manifest:

| Referrer | Artifact/layer media type | Contents |
|---|---|---|
| SBOM | `application/vnd.cyclonedx+json` | CycloneDX 1.5 document derived from the dependency lock. |
| Provenance | `application/vnd.brewlet.attestation.v1+json` / `application/vnd.dsse.envelope.v1+json` | Signed DSSE envelope containing an in-toto Statement v1 with predicate type `https://brewlet.sh/attestations/dependency-bundle/v1`. |

Exactly one discovered and valid SBOM referrer is required. Bundle provenance is optional. If no
provenance referrer exists, Brewlet treats the bundle as unsigned. If one or
more provenance referrers exist, consumers MUST require trust credentials and
MUST validate at least one complete signature, identity, subject, and predicate
contract; they MUST NOT silently treat invalid signed provenance as unsigned.

**Why SBOM is stricter than provenance.** The asymmetry is deliberate.
Provenance is an *assertion about* the bundle, and several are legitimate —
this contract explicitly allows adding signatures during signer rotation — so
one fully valid candidate is proof. An SBOM is the *inventory of* the bundle,
and the bundle manifest is immutable and content-addressed, so exactly one SBOM
is correct for it. Two are contradictory claims about the same content:
selecting whichever one happens to validate would let anyone able to push a
referrer steer which inventory a consumer reads, and would conceal that the
registry holds a conflicting claim. A stale or malformed *additional* SBOM
referrer therefore fails the bundle closed. The remedy is under the publisher's
control: delete the extra referrer.
CycloneDX is a wire format generated directly from the lock; Brewlet does not
require a CycloneDX service, SDK, or runtime library. The SBOM MUST have
`bomFormat: CycloneDX`,
`specVersion: 1.5`, and `version: 1`. Every lock entry MUST have exactly one
component with matching group, name, version, SHA-256 hash, and Maven package
URL. Its package URL is:

```text
pkg:maven/<percent-encoded-groupId>/<percent-encoded-artifactId>@<percent-encoded-version>[?type=<percent-encoded-type>[&classifier=<percent-encoded-classifier>]]
```

The `type` qualifier is omitted for `jar`; the `classifier` qualifier is omitted
when empty. Spaces are encoded as `%20`. Consumers compare these semantic fields
rather than requiring byte-identical JSON.

Ops admission policy determines whether unsigned bundles are acceptable in a
deployment environment. A deployable example of cluster-side enforcement — a
Ratify external verifier plugin plus Gatekeeper policy that admits Brewlet
runtime pods only when their image carries a valid managed-dependency
attestation — is provided in [`admission/`](../admission/).

Each referrer MUST be an OCI image manifest with:

- `schemaVersion: 2` and
  `mediaType: application/vnd.oci.image.manifest.v1+json`;
- a `subject` descriptor exactly matching the subject's media type, digest, and
  size;
- one config descriptor for the exact bytes `{}` using media type
  `application/vnd.oci.empty.v1+json`; and
- exactly one document layer using the media type from the table above.

Provenance referrer manifests MUST carry
`brewlet.sh/predicate-type=<predicate type>`. Local OCI indexes additionally use
`brewlet.sh/referrer-subject=<subject digest>` for discovery. These annotations
are discovery hints and are never trust evidence.

The dependency-bundle predicate has this exact schema:

```json
{
  "schemaVersion": 1,
  "dependencyBundleDigest": "sha256:<hex>",
  "dependencyLayerDigest": "sha256:<hex>",
  "dependencyLockDigest": "sha256:<hex>",
  "sbomDigest": "sha256:<hex>",
  "sourceBom": "group:artifact:version",
  "builderIdentity": "<bundle publisher identity>"
}
```

The bundle predicate binds the bundle manifest, dependency layer, lock, SBOM,
source BOM, and builder identity. When provenance exists, managed application
publication must verify the ECDSA P-256 signature against a configured public
key, match the expected signer identity, and compare every binding with the
resolved bundle before composing an image. CycloneDX verification is semantic:
component coordinates and SHA-256 hashes must exactly match the lock, while
serialization order and non-contract metadata may differ between publishers.
When multiple provenance referrers exist, verification succeeds only if at
least one candidate satisfies the complete trusted-key, identity, subject, and
predicate contract.
`sbomDigest` is the SHA-256 digest of the CycloneDX document blob, not the
digest of its enclosing OCI referrer manifest.

After publishing the final image index and obtaining its immutable digest, the
publisher may attach a signed in-toto statement using predicate type
`https://brewlet.sh/attestations/managed-dependencies/v1`. When present, its
subject is the final image index and its predicate binds:

- final application image digest;
- trusted-builder `thinJar` verdict and application JAR digest;
- dependency-bundle manifest and classpath-layer digests;
- dependency-lock and CycloneDX SBOM digests;
- source Maven BOM coordinates; and
- application builder identity and schema version.

The managed-dependency predicate has this exact schema:

```json
{
  "schemaVersion": 1,
  "finalImageDigest": "sha256:<hex>",
  "thinJar": true,
  "applicationJarDigest": "sha256:<hex>",
  "dependencyBundleDigest": "sha256:<hex>",
  "dependencyLayerDigest": "sha256:<hex>",
  "dependencyLockDigest": "sha256:<hex>",
  "sbomDigest": "sha256:<hex>",
  "sourceBom": "group:artifact:version",
  "builderIdentity": "<application publisher identity>"
}
```

`thinJar` MUST be `true`. The in-toto statement for either predicate MUST use
`_type: https://in-toto.io/Statement/v1`, contain exactly one subject whose
`digest.sha256` value omits the `sha256:` prefix, and use the predicate type
specified above.

DSSE pre-authentication encoding follows:

```text
DSSEv1 <len(payloadType)> <payloadType> <len(payload)> <payload>
```

The payload type is `application/vnd.in-toto+json`. Signatures use ECDSA P-256
with SHA-256; keys are PKCS#8 private PEM and SubjectPublicKeyInfo public PEM,
and the envelope MUST contain exactly one ASN.1 DER signature. `keyid` is
`sha256:` followed by the lowercase SHA-256 digest of the public-key DER.
Informational OCI
annotations mirror evidence for discovery but are never accepted as proof.
The bundle signer identity and final-image builder identity are separate trust
inputs; deployments must not assume that the platform bundle publisher also
builds each application.

Local OCI layouts index referrers with
`brewlet.sh/referrer-subject=<subject digest>`. Registry publication uses the
OCI Referrers API and deterministic per-referrer digest tags when the registry
does not advertise native subject/referrer support. Per-referrer tags preserve
multiple signatures during signer rotation instead of overwriting an existing
candidate. Implementations MUST follow `Link: rel="next"` pagination for native
referrers and fallback tag listing. The fallback tag is:

```text
sha256-<subject hex>.<first 12 hex of SHA-256(artifactType UTF-8)>.<first 24 hex of referrer-manifest digest>
```

Consumers MUST validate descriptor media types, sizes, and digests; the complete
subject binding; empty config; document-layer type; DSSE signature and key ID;
expected signer identity; statement and predicate types; schema version; and
every predicate digest whenever provenance exists.
Malformed or untrusted candidates do not invalidate a
different fully trusted candidate. Consumption fails closed unless exactly one
SBOM referrer is discovered and validated. Absence of provenance means the
artifact is unsigned; presence of provenance means at least one candidate must
satisfy the complete trust contract. Brewlet permits both states. Production
admission policy decides whether unsigned dependency bundles or application
images may run.

This is a key-based Sigstore/in-toto-compatible signing profile. Fulcio keyless
identity issuance and Rekor transparency-log inclusion are not required by this
version of the Brewlet contract and can be added without changing its predicates.
A deployable cluster-side enforcement example that verifies this evidence at
admission — reusing Brewlet's own DSSE/predicate verification through a Ratify
external verifier plugin and Gatekeeper policy — is provided in
[`admission/`](../admission/); it requires a registry that exposes the OCI 1.1
Referrers API.

---

## 5. Node Provisioning (`brewlet-node-provisioner`)

Mirrors Runtime Class Manager's node/shim lifecycle: a privileged DaemonSet —
placed by a `NodeProfile` pool (§5.6), or by a `brewlet.sh/provision` node label
on the standalone no-operator path (§5.5) — installs the runtime onto the host
and wires it into containerd. Nodes MUST run containerd 2.0 or newer because
the authoritative image-identity contract requires protected CRI
requested-image metadata. This server requirement is independent of the
containerd TOML format: config `version = 2` and `version = 3` are both
supported on containerd 2.

### 5.1 Activation
```bash
helm repo add brewlet https://charts.brewlet.sh
helm install -n brewlet --create-namespace brewlet microsoft/brewlet-operator
```

The chart renders a **default `NodeProfile`** (§5.6) that provisions the node
pools named in `provisioner.pools` — pool-level activation, with no per-node
opt-in step to manage. `provisioner.pools` is **required**: because the
provisioner is privileged and mutates the host, the chart fails to render rather
than default to the whole cluster. To author profiles yourself instead, disable
the default profile (`defaultProfile.enabled=false`, §5.6). The legacy per-node opt-in — a `brewlet.sh/provision=true` node **label**
(not an annotation; it drives `nodeAffinity`) consumed by the standalone
the [`deploy/node-provisioner.yaml`](../kubernetes/deploy/node-provisioner.yaml)
DaemonSet — remains for the no-operator path (§5.5).

### 5.2 What the provisioner does on each opted-in node
Before step 1, the provisioner validates every indexed JDK and launcher source.
Every source image is mandatory and must be a canonical, fully qualified,
tagless `repository@sha256:<64 lowercase hex>` reference; every Java home and
launcher path must be a clean absolute path below `/`. Missing fields, malformed
or duplicate entries, mutable references, reserved names, or invalid mirrors
cause provisioning to fail with no shim installation, source pull/mount/copy,
or readiness advertisement. Brewlet has no built-in runtime catalog.

1. Installs the shim binary `containerd-shim-brewlet-v2` into `$PATH` (e.g. `/opt/brewlet/bin`).
2. Installs one or more **JDK runtime roots** under `/opt/brewlet/jdks/<dist>-<feature>/`
   (a minimal, read-only Linux userland + JDK installation; shared by all workloads). See §5.3.
2b. Optionally installs one or more **launcher layers** under
   `/opt/brewlet/launchers/<name>/` (e.g. `jaz`) — read-only, drop-in
   java-compatible launchers overlaid into the sandbox and prepended to `PATH`.
   These are independent of the JDK roots, so any launcher composes over any JDK.
   See §5.4.
3. Registers a runtime entry through
   `/etc/containerd/config.toml.d/99-brewlet.toml` when the host's primary
   configuration imports that drop-in directory. Hosts without an enabled
   import use an in-place append to `/etc/containerd/config.toml` and retain the
   original as `config.toml.brewlet.bak`:
   ```toml
   [plugins."io.containerd.grpc.v1.cri".containerd.runtimes.brewlet]
     runtime_type = "io.containerd.brewlet.v2"
     # Forward the deployment-descriptor annotations the admission webhook stamps.
     # The shim resolves executable identity from containerd-owned CRI metadata,
     # verifies the selected platform manifest from the content store, and does
     # not trust artifact annotations as execution or AppCDS cache identity.
     pod_annotations = ["brewlet.sh/*"]
     [plugins."io.containerd.grpc.v1.cri".containerd.runtimes.brewlet.options]
       SystemdCgroup = true  # mirror the node's runc cgroup driver
   ```
   Containerd config `version = 2` uses the legacy
   `io.containerd.grpc.v1.cri` namespace shown above. Config `version = 3` uses
   `io.containerd.cri.v1.runtime`; the provisioner selects the namespace from
   the config format rather than the containerd server version.
   Because `brewlet` is not a built-in `runc` handler, CRI hands the shim a
   generic `runtimeoptions.Options` carrying this `options` block verbatim; the
   shim translates it back into `runc` options (preserving `SystemdCgroup`) and,
   for the pod's pause/sandbox container, delegates to the embedded `runc` task
   service unchanged rather than rewriting it into a JVM launch.
4. Requires a containerd 2.0-or-newer server before any readiness can be
   advertised, then applies the readiness smoke gate and configured containerd
   lifecycle. Unless validation is disabled, it runs `java -version` inside
   every configured JDK
   root and verifies that each staged launcher is an executable regular file.
   Arbitrary launchers are not executed because the contract has no universal
   safe probe. In the default `validated` mode, these checks run before
   `containerd config dump` validates the
   effective host configuration and requires both a successful parse and the
   exact `brewlet` runtime handler before restarting containerd through the host
   service manager. An invalid new render is restored or removed, leaves the
   node unready, and is reported through
   `brewlet.sh/provision-error`. After restart, it verifies the containerd socket
   and live `brewlet` runtime handler, and automatically restores the prior
   primary config (or drop-in) before restarting and verifying recovery after a
   failure. The node remains unready and `brewlet.sh/provision-error`
   distinguishes restart, health-check, runtime-handler, rollback, and bounded
   component-specific failures. The explicit `sighup` mode retains the legacy
   in-place reload path without the config-dump gate; `none` leaves containerd
   configuration untouched. Both still apply the smoke gate before readiness is
   advertised. Unchanged valid configuration is config-dump validated and
   health-checked without another restart.
5. Verifies the shim responds.
6. Applies the profile's AppCDS policy. When
   `spec.appCDS.regenerationEnabled=true`, atomically installs the root-owned,
   read-only `/opt/brewlet/policy/appcds-regeneration-enabled` sentinel. When
   disabled, during cleanup, or after any provisioning failure, removes the
   sentinel and its capability advertisement.
7. For an operator-managed profile, re-reads the cluster-scoped `NodeProfile`
   and requires its UID and generation to match the provisioner pod and its
   deletion timestamp to remain empty. The same fence is checked immediately
   before publishing the final readiness label. Standalone provisioners omit
   the UID and skip this profile identity check.
8. On success, labels the node `brewlet.sh/runtime=ready` and annotates with the
   available JDKs, e.g. `brewlet.sh/jdks=temurin-17,temurin-21,temurin-25`, and
   any installed launchers, e.g. `brewlet.sh/launchers=java,jaz`. It also emits
   per-capability **scheduling labels** the admission webhook matches nodeAffinity
   against (annotations can't drive nodeAffinity): `brewlet.sh/jdk.<dist>-<feature>`,
   `brewlet.sh/jdk-feature.<feature>`, `brewlet.sh/launcher.<name>`, and, only
   when policy authorizes it, `brewlet.sh/appcds-regeneration` (§8/§14).
   Their exact keys, token grammar, presence semantics, compatibility guarantees,
   and autoscaler integration are defined by the public
   [capability-label contract](CAPABILITY_LABELS.md).
9. Publishes the container-local `/tmp/brewlet-complete` marker and idles.
   Provisioner and cleanup containers have an exec readiness probe for that
   marker. Each entrypoint invocation removes stale completion state before
   doing any work; failures do not publish completion. This gate is independent
   of the optional runtime smoke checks and has no startup deadline that could
   kill a legitimately slow installation.

The `brewlet.sh/runtime=ready` label is used by the `RuntimeClass` `nodeSelector`
so workloads only schedule onto provisioned nodes.

### 5.3 Installing JDK runtime roots on nodes

Which JDKs a node offers is **declarative**: the platform team lists them in the
provisioner (Helm value / DaemonSet env), and the DaemonSet materializes them on
every opted-in node. Nothing is baked into application artifacts, and no
distribution name implies an image. Every entry declares its source.

```yaml
# values.yaml (brewlet-operator Helm chart)
jdks:
  - distribution: temurin
    feature: 21
    source:
      image: docker.io/library/eclipse-temurin@sha256:85f00967bcc624fc19fa9c2cf124ea426a5363898e267141726f31f358c2e14b
      javaHome: /opt/java/openjdk
  - distribution: microsoft
    feature: 25
    source:
      image: mcr.microsoft.com/openjdk/jdk@sha256:bfde2ed613f4c67c112d1592452575d3a1dc9ce5f7d75821bb7752aa786fa575
      javaHome: /usr/lib/jvm/msopenjdk-25
  - distribution: zulu
    feature: 21
    source:
      image: docker.io/library/azul-zulu@sha256:2e230d906cffcc7bb7360ce82836f2ff0e0be74a1d5ebaf929e4e6ac99d61bf2
      javaHome: /usr/lib/jvm/zulu21
```

On each node the provisioner installs every listed JDK under
`/opt/brewlet/jdks/<distribution>-<feature>/` as a **read-only, shared** root, via
**copy-from-image** — the sole acquisition mechanism. The verified,
digest-pinned image is pulled through the host containerd and its complete root
filesystem is mounted and copied onto the host `hostPath`, so no package manager
touches the host:

```bash
# inside the provisioner, per JDK, for the node's arch:
ref=mcr.microsoft.com/openjdk/jdk@sha256:bfde2ed613f4c67c112d1592452575d3a1dc9ce5f7d75821bb7752aa786fa575
ctr -n k8s.io image pull "$ref"
ctr -n k8s.io images mount "$ref" /tmp/jdk-root
cp -a /tmp/jdk-root/. /opt/brewlet/jdks/microsoft-25/
ctr -n k8s.io images unmount --rm /tmp/jdk-root
```

The result contains the source image's userland, a `.brewlet-java-home` file
recording the JDK or jlink runtime location, and a `.brewlet-source` file
recording the exact resolved image. An existing root is not executed until both
metadata values match the newly verified request. A changed digest is staged and
atomically activated. The shim uses the complete root as the sandbox lower layer
and exposes that Java home at `/opt/jdk` (§6.1).
This permits both full vendor JDKs and centrally managed jlink runtimes with an
administrator-approved module set. The latter is installed once per node pool;
it is not carried in each application artifact. Roots are **versioned and
additive**: patching means dropping in a new root and retiring old ones; running
pods are unaffected until they restart. Install one root per node architecture
(amd64/arm64); the JAR is arch-neutral so the same artifact runs on either.

> **Configuration interface.** The operator renders `JDK_SOURCE_COUNT` and
> indexed `JDK_SOURCE_<n>_{TOKEN,IMAGE,JAVA_HOME}` variables for every profile
> entry. Optional launchers use `LAUNCHER_SOURCE_COUNT` and indexed
> `LAUNCHER_SOURCE_<n>_{NAME,IMAGE,PATH}` variables. The provisioner validates
> the complete set, then derives internal `JDKS` and `LAUNCHERS` inventories;
> those inventories are never accepted as independent inputs. Full reference is
> in
> [`provisioner/README.md`](https://github.com/microsoft/brewlet/blob/main/provisioner/README.md).

The source-policy validator rejects duplicate tokens, mutable or non-canonical
references, unsupported digest algorithms, malformed paths, invalid registry
hosts, and malformed mirror targets before any host mutation.

For air-gapped clusters, `spec.registry.mirrors` rewrites only the
registry/repository prefix and preserves the administrator-selected digest. The
destination host, including any explicit port, must exactly match the external
operator/admission `--allowed-source-mirror-hosts` policy and the provisioner's
`SOURCE_ALLOWED_MIRROR_HOSTS`; an empty allowlist disables mirrors. Schemes,
whitespace, malformed or duplicate hosts, self-mappings, and unapproved
destinations fail before source operations. Repository path prefixes are
allowed, but the mirror must preserve the referenced manifest/index bytes and
digest.

> **Licensing:** ship only OpenJDK builds whose license you accept. Brewlet is
> distribution-neutral; the platform team chooses every digest-pinned build.

### 5.4 Installing a launcher on nodes (e.g. `jaz`)

A launcher is installed the same declarative way, independently of the JDKs:

```yaml
# values.yaml
launchers:
  - name: jaz
    source:
      image: mcr.microsoft.com/openjdk/jdk@sha256:bfde2ed613f4c67c112d1592452575d3a1dc9ce5f7d75821bb7752aa786fa575
      path: /usr/bin/jaz
```

The provisioner stages each declared binary into
`/opt/brewlet/launchers/<name>/bin/<name>` so it can be overlaid into the
sandbox and put on `PATH` (§6). The provisioner pulls and mounts the explicit
digest-pinned source image, rejects a missing source file or a symlink in any
source-path component, copies the binary from the mounted filesystem, and
atomically activates the launcher layer:

```bash
# conceptual — the provisioner handles this automatically
ref=mcr.microsoft.com/openjdk/jdk@sha256:bfde2ed613f4c67c112d1592452575d3a1dc9ce5f7d75821bb7752aa786fa575
ctr -n k8s.io image pull "$ref"
ctr -n k8s.io images mount "$ref" /opt/brewlet/.jaz-image
install -m 0755 /opt/brewlet/.jaz-image/usr/bin/jaz \
  /opt/brewlet/launchers/jaz/bin/jaz
ctr -n k8s.io images unmount --rm /opt/brewlet/.jaz-image
```

The source image is never executed, receives no host networking, and receives
no writable host bind mount. `.brewlet-source` records the exact image and
source path; matching metadata is required before an existing launcher layer is
reused. The provisioner atomically rewrites
`/opt/brewlet/launchers/.brewlet-active`; the shim MUST reject a launcher root
that is not listed in that inventory, and MUST fail closed when the inventory is
absent. The shim MUST also reject a requested launcher name (and JDK
distribution) that is not a lowercase DNS-1123 token, and MUST verify after
symlink resolution that the selected root is a direct child of the configured
runtime roots directory before using it as an overlay lower layer or bind-mount
source. The same requirement applies to `/opt/brewlet/jdks/.brewlet-active`.

If a bundled/copied launcher needs shared libraries not present in the JDK root,
include them under the launcher root (e.g. `lib/`); the layer is mounted read-only
alongside the JDK. Because `jaz` locates the JVM via `JAVA_HOME` (which Brewlet
pins to the selected JDK), the **same** launcher layer works with any installed
OpenJDK. The node advertises what it installed via `brewlet.sh/launchers=…`, and
a descriptor requesting a launcher the node lacks fails admission with
`NoCompatibleLauncher` (§14).

Before launcher capabilities are advertised, the provisioner verifies each
staged launcher is an executable regular file. It does not execute arbitrary
administrator-provided launchers because there is no universal safe version
probe. Validation failure leaves the node unready and reports a concise
launcher-specific provisioning error. Setting `BREWLET_VALIDATE=false` skips
this check together with the JDK smoke tests.

> **Security note (shared with Runtime Class Manager):** node provisioning is privileged and
> modifies the host. It must be scoped to nodes the platform team controls, and is
> not recommended for hostile multi-tenant nodes without further isolation (§11).

### 5.5 Provisioner

The provisioner is a container image built from the
[`provisioner/`](https://github.com/microsoft/brewlet/tree/main/provisioner)
directory (`Dockerfile` + `entrypoint.sh`) and deployed by
[`deploy/node-provisioner.yaml`](../kubernetes/deploy/node-provisioner.yaml):

- **Image** — a multi-stage build that compiles
  `containerd-shim-brewlet-v2` and `brewlet-source-policy` from the
  [core runtime](https://github.com/microsoft/brewlet) for the target architecture
  (so installed binaries always match the node arch), then assembles a small
  Debian-based runtime carrying the entrypoint, source-policy validator, and
  `bash`/`curl`/`kubectl`. Build with `make provisioner-image`
  (single arch) or `make provisioner-image-push` (multi-arch via buildx).
- **Entrypoint** — an idempotent script that performs all §5.2 steps:
  validates all indexed JDK/launcher sources before host mutation; installs
  the shim to `/opt/brewlet/bin` and the host `/usr/local/bin` (containerd's
  PATH); materializes each declared JDK root under `/opt/brewlet/jdks/<dist>-<feature>/`
  via digest-pinned **copy-from-image** (`ctr` against the host containerd); stages launcher layers
  (e.g. `jaz`) under `/opt/brewlet/launchers/`; appends the
  `runtimes.brewlet` block to `/etc/containerd/config.toml` and reloads containerd
  (SIGHUP via `hostPID`) — gated by post-install JDK smoke tests and launcher
  executable checks and
  configurable per the restart policy in §5.6 (`validate` /
  `containerdRestart`); then labels the
  node `brewlet.sh/runtime=ready`,
  annotates the installed JDKs/launchers, and emits the per-capability scheduling
  labels the admission webhook uses (`brewlet.sh/jdk.*`, `brewlet.sh/jdk-feature.*`,
  `brewlet.sh/launcher.*`). The manifest ships the `ServiceAccount` + `ClusterRole`/binding
  (`get`/`patch` on nodes) the labelling step needs.

Node provisioning is driven by the **operator** (§8.1) and admission is handled
by the **pod webhook** (§8.3). The whole set is packaged by
the [`charts/brewlet`](../kubernetes/charts/brewlet)
Helm chart.

Operator reference for the provisioner (env-var interface, copy-from-image
mechanics, source policy, deployment): see
[`provisioner/README.md`](https://github.com/microsoft/brewlet/blob/main/provisioner/README.md).

### 5.6 Node profiles (per-pool preparation)

Provisioning every node identically — whether via a cluster-wide default profile
or the legacy `brewlet.sh/provision` label (§5.1/§5.5) — ignores that real
clusters are heterogeneous: a batch pool wants a different JDK than the web pool,
an air-gapped pool needs a registry mirror, some pools must never have containerd
restarted. The cluster-scoped **`NodeProfile`** CRD (`node.brewlet.sh/v1alpha1`)
binds a **node pool** to a **JDK/launcher inventory**, AppCDS policy, and rollout
policy.
*Selecting a pool is the opt-in* — every node in the pool, present and future, is
provisioned; there is no per-node label to manage.

Mirror authority is configured separately on the operator and admission
components through `--allowed-source-mirror-hosts` (Helm
`security.allowedSourceMirrorHosts`). A profile can select mappings only to those
exact destination hosts; an empty allowlist disables mappings.

```yaml
apiVersion: node.brewlet.sh/v1alpha1
kind: NodeProfile
metadata: { name: batch }
spec:
  nodePool:
    names: ["batch"]        # matched on the resolved pool key
    # key: agentpool        # optional; auto-detected when omitted
    # includeControlPlane: true   # opt in to control-plane nodes (default false)
  tolerations:              # optional; nothing is tolerated implicitly
    - key: workload
      operator: Equal
      value: java
      effect: NoSchedule
  jdks:
    - distribution: microsoft
      feature: 25
      source:
        image: mcr.microsoft.com/openjdk/jdk@sha256:bfde2ed613f4c67c112d1592452575d3a1dc9ce5f7d75821bb7752aa786fa575
        javaHome: /usr/lib/jvm/msopenjdk-25
  launchers:
    - name: jaz
      source:
        image: mcr.microsoft.com/openjdk/jdk@sha256:bfde2ed613f4c67c112d1592452575d3a1dc9ce5f7d75821bb7752aa786fa575
        path: /usr/bin/jaz
  appCDS:
    regenerationEnabled: true  # default false; authorizes node cache writers
  registry:                  # optional, air-gapped pulls (see below)
    mirrors: { "mcr.microsoft.com": "mirror.internal/mcr" }
  rollout:
    maxUnavailable: 1
    validate: true           # JDK probes plus launcher executable checks
    containerdRestart: validated   # validated | sighup | none
```

- **Pool key resolution.** `spec.nodePool.key` pins the node label carrying the
  pool name; when empty the operator auto-detects the provider key by probing the
  fleet for the well-known keys (`cloud.google.com/gke-nodepool`, `agentpool`,
  `eks.amazonaws.com/nodegroup`, `karpenter.sh/nodepool`). Bare-metal/kubeadm
  clusters with no such label must set an explicit key for named pools. A named
  pool with no resolved key selects no nodes; it never falls back to every node.
- **The default profile.** A profile with no `nodePool.names` is the **catch-all
  default**: it targets nodes not selected by any named-pool profile. Only one
  catch-all is allowed. Named profiles must use the same pool-key configuration
  (all the same explicit key, or all auto-detected) and disjoint pool names.
  Different named-profile keys, including explicit/auto mixtures, are rejected
  because a future node could satisfy both selectors. The Helm
  chart renders one from `provisioner.jdks/launchers`, scoped to the pools named
  in the required `provisioner.pools`; the catch-all form remains available to
  administrators who author a `NodeProfile` directly. Additional chart profiles
  also require nonempty named pools; omitted pools never create a catch-all.
- **Control-plane nodes are excluded from provisioning.** Every profile's
  provisioning DaemonSet carries
  required `node-role.kubernetes.io/control-plane` and
  `node-role.kubernetes.io/master` `DoesNotExist` expressions, so a privileged
  provisioner never lands on a control-plane node. This is deliberately a
  *label* rule rather than a taint rule: kind and Docker Desktop label their
  single node as the control plane without tainting it, so the taint alone would
  not have held. `spec.nodePool.includeControlPlane: true` is the only way in.
  The same rule governs desired membership and normal readiness. Cleanup instead
  follows recorded node identities: it must still reverse an already-authorized
  node whose pool or role labels changed after provisioning.
- **Tolerations are explicit.** The DaemonSet tolerates exactly what
  `spec.tolerations` declares — there is no blanket `operator: Exists` entry —
  plus the node-condition tolerations Kubernetes injects into every DaemonSet.
  Each entry must name a `key`, so "tolerate everything" is not expressible.
- **One DaemonSet per profile.** The operator's `NodeProfileReconciler` (§8.1)
  reconciles each profile into its own `brewlet-node-provisioner-<profile>`
  DaemonSet whose pod affinity requires a recorded node name and UID-bound
  ownership labels, plus applicable provisioning pool/role constraints. Every JDK is rendered
  as indexed `JDK_SOURCE_*` variables; every optional launcher is rendered as
  indexed `LAUNCHER_SOURCE_*` variables.
  `BREWLET_APP_CDS_REGENERATION_ENABLED`, `MIRRORS`,
  `SOURCE_ALLOWED_MIRROR_HOSTS`, and `BREWLET_CONTAINERD_RESTART` come from the
  spec and external policy. `JDKS` and `LAUNCHERS` are derived inside the
  provisioner, preventing source/inventory mismatches.
- **Exclusive ownership.** Before granting scheduling authority, the operator
  durably records each target's node name and UID. Optimistic node updates claim
  `brewlet.sh/owner-uid` (profile UID), `brewlet.sh/owner-node-uid` (node UID),
  and the `brewlet.sh/owner-name` annotation. A conflicting owner is not
  overwritten. Claims remain reserved until host cleanup and worker teardown
  finish, including handoff from a catch-all to a named profile. Admission uses
  fresh profile reads, but node claims remain authoritative when admissions race.
  Managed containers set `BREWLET_REQUIRE_NODE_CLAIM=true` and check both the
  node identity and persisted provisioning/retirement authority before host
  mutation or readiness publication. A failed initial ownership fence does not
  run host-mutating or node-advertisement failure handlers.
- **Retargeting.** Selector edits and node pool/role changes retire departing
  recorded targets. The operator stops provisioning, freezes the retirement
  episode, cleans only those departing nodes, then waits for cleanup workers to
  terminate before releasing their claims and reconciling the latest desired
  targets. Retained nodes' runtime roots are not cleaned. A later spec edit,
  deletion request, or controller restart cannot discard an in-flight retirement.
  Cleanup uses per-node recorded containerd restart policy: prior Brewlet config
  mutations remain reversible after a later `none` rollout, without removing
  pre-existing config from newly added `none` nodes. Previously authorized and
  newly explicit tolerations may be combined to reach a newly tainted old target;
  no blanket toleration is introduced.
- **Registry mirrors (air-gap).** `spec.registry.mirrors` maps an upstream
  source host to an approved internal mirror repository prefix. The destination host
  must exactly match the external allowlist, and the provisioner preserves the
  source `@sha256:` digest while rewriting every copy-from-image pull. Admission,
  reconciliation, and the provisioner all enforce the policy.
- **Fail-closed reconciliation.** The reconciler repeats source, mirror, and pool
  conflict validation before creating/updating the privileged DaemonSet. An
  invalid stored or updated profile has its provisioner DaemonSet withheld or
  deleted, loses runtime/JDK/feature/launcher advertisements only on nodes owned
  by that profile, and reports `Ready=False`, reason `InvalidProfile`, with an
  actionable message. Admission remains an early-feedback layer, not the
  security boundary.
- **Reversal.** Deleting a NodeProfile does not silently strip nodes. A finalizer
  (`node.brewlet.sh/cleanup`) holds the object while the operator first stops the
  managed provisioner and waits for its pods, including terminating pods, to be
  gone. Only then does it run a
  short-lived `brewlet-cleanup-<profile>` DaemonSet (`BREWLET_MODE=cleanup`) that
  restores the containerd config backup, removes the AppCDS authorization
  sentinel, the shim, and the profile's installed JDK/launcher roots (unless
  `BREWLET_CLEANUP_RUNTIME_ROOTS=false`, for nodes whose roots are baked into an
  immutable image), and drops the runtime + capability labels; only once every
  recorded target is cleaned does the operator record completion and tear down
  cleanup. The finalizer remains until the cleanup DaemonSet and its pods are
  gone, so a replacement profile cannot race an old cleanup process. Recorded
  completion survives a controller restart; teardown must not create another
  cleanup DaemonSet. If the current source/mirror/pool policy is invalid,
  deletion stops provisioning and cleanup workers and withdraws advertisements,
  but MUST retain both finalizer and claims while recorded or claimed host state
  may remain. `Ready=False/CleanupBlocked` requires repairing the spec, external
  source policy, or pool conflict; repairing a deleting profile resumes cleanup.
  Only proven-unprovisioned invalid profiles may finalize without host cleanup,
  after direct node/writer checks. Missing ownership records are not proof of
  successful cleanup.

The operator-owned status ledger contains `targets[]` entries with `name`, `uid`,
`claimed`, and per-node `containerdRestart`; `ownershipInitialized`, `migrating`,
and `migrationDaemonSetUIDs` track migration. `provisioningGeneration` and
`provisioningSpec` retain provisioning policy, while `retirement` freezes
`targets`, `generation`, `spec`, and `phase` (`Cleaning` or `Teardown`). Target and
retirement checkpoints MUST survive a fresh API read before dependent actions.
An older CRD that silently prunes these fields fails closed: apply the updated
NodeProfile CRD before upgrading operator/provisioner images, not merely the
Helm values.

Migration drains legacy unfenced workers before activating claims and records
discoverable potential targets, bound and pending pods, advertisements even
without a surviving DaemonSet, and exact legacy DaemonSet UIDs. Pending pod
`metadata.name` affinity contributes possible targets. Observed old-pod restart
policy is retained per node rather than replaced by a newer template's `none`.
Migration first fences replacement scheduling with `OnDelete` and the
`node.brewlet.sh/migration` Pod scheduling gate, confirms the fence and observed
generation, and durably inventories evidence before draining workers. It
requires scheduling-gate support; gates must not be removed manually.
Unverified UID/revision/policy evidence remains durably blocked and cannot
become safe no-host finalization after advertisements are withdrawn. It cannot
reconstruct vanished historical hosts with no
surviving Kubernetes evidence; those require administrator verification.
Ownership conflicts, missing/reused node identities, and unavailable cleanup
targets are reported rather than treated as completed reversal.
Retirement is profile-wide serialized: before successful cleanup is durably
recorded, an inaccessible/missing/reused departing node pauses all profile
provisioner and metrics workers and retained/new-node provisioning, while
preserving retained hosts' runtime roots and advertisements. Recovery is
supported for a disconnected original Node with its UID intact; missing/reused
UIDs have no supported in-place recovery. A durable `Teardown` checkpoint is
different: cleanup is already proven, so later Node disappearance need not block
release after workers disappear. Planned shrink MUST exclude departing nodes
from every remaining profile, including catch-alls, and finish cleanup/teardown
and claim release before deleting the Node/VM. Automatic autoscaler
scale-in/consolidation requires external coordination; it is not supplied here.

During normal reversal, `CleanupComplete=True` with reason `CleanupSucceeded`
records completed host work for the current profile UID and
`observedGeneration`. `Ready=False`, reason `CleanupTeardown`, then denotes
waiting for the cleanup DaemonSet and all profile pods to disappear. A stale
checkpoint is not reused for another generation or profile incarnation;
reappearing provisioners invalidate it before being stopped. Failure to persist
the checkpoint prevents teardown. `CleanupComplete` is not permission to skip
the finalizer or start a replacement profile early.
Partial retirement uses its own frozen phase, never the full-deletion
`CleanupComplete` checkpoint.

**Helm uninstall.** A `pre-delete` Job runs the operator image in a separate,
unprivileged cleanup-coordinator mode. It MUST complete before Helm deletes the
operator or its RBAC. Release ownership requires both
`meta.helm.sh/release-name` and `meta.helm.sh/release-namespace`, plus
`app.kubernetes.io/managed-by=Helm`; instance labels or names alone are not
authority. The coordinator refuses uninstall while any other NodeProfiles
exist, requests owned-profile deletion with UID/resource-version preconditions,
and inventories provisioning/cleanup workers across namespaces. Known workers
outside the operator namespace and standalone writers must be deprovisioned
separately; a namespace change cannot hide them. Success requires profiles and
their workers to disappear. It does
not remove finalizers or delete workers directly. API errors and bounded
timeouts fail the hook, retaining the normal control-plane resources so cleanup
can finish or be repaired. The dedicated hook RBAC permits profile reads/deletes
and cluster-wide worker lists, not worker mutation, host access, or finalizer
writes. The operator also has read-only global writer inventory; its DaemonSet
mutation permissions remain restricted to its configured namespace.

Administrators MUST drain affected workloads and quiesce profile/GitOps writers
before uninstall. The coordinator rechecks state while waiting; it does not
atomically lock cluster-wide profile creation. `uninstall.timeoutSeconds`
defaults to 240 (whole seconds, 1-86400); Helm's timeout must exceed this plus 30
seconds for Job startup/termination. Failed cleanup Jobs remain for diagnosis
until retry. Helm may remove earlier successful hook resources on failure;
retry recreates the dedicated hook RBAC. Successful hook resources are removed.
The component namespace is retained to protect unrelated objects; CRDs and the
operator-created shared RuntimeClass are not automatically removed. Existing
releases without this hook require staged profile deletion before control-plane
removal.

Sample manifests:
[`deploy/sample-nodeprofile.yaml`](../kubernetes/deploy/sample-nodeprofile.yaml);
design detail in [`proposals/0001-node-profiles.md`](proposals/0001-node-profiles.md).

---

## 6. The containerd Shim (`containerd-shim-brewlet-v2`)

Implements the **containerd Runtime v2 (TTRPC) shim API**, the same integration
seam used by SpinKube through containerd-shim-spin/runwasi. Brewlet takes the
**runc-backed** approach: rather
than re-implement namespaces, cgroups, and CNI, the shim *assembles an OCI runtime
bundle and delegates isolation to runc*. This maximizes correctness and reuse.

### 6.1 Per-container lifecycle (`Create`)

1. **Resolve artifact.** Read the image the kubelet handed us, verify the
   resolved platform-manifest digest against its bytes (and descriptor size when
   present), and separate the `jvm.config.v1+json` blob from the
   `jar.layer.v1+jar` blob (containerd content store already cached them).
2. **Select JDK/launcher.** Read `brewlet.sh/jdk` and `brewlet.sh/launcher` from
   the OCI runtime spec annotations that originated on the pod. The JDK annotation
   selects the node-resident JDK root (a bare feature or empty request picks the
   lexically-first installed distribution for that feature — feature 21 when the
   annotation is absent — while `<dist>-<feature>` pins an exact root), and
   the launcher annotation selects `java` or a node-installed launcher such as `jaz`.
   Fail fast with a clear event if the requested runtime is unavailable.
3. **Assemble rootfs (overlayfs):**
   - `lowerdir` = the selected read-only JDK runtime root (shared across all pods).
   - `upperdir`/`workdir` = per-container writable scratch.
   - The JAR layer is mounted read-only at `/app/`.
4. **Generate `config.json` (OCI runtime spec):**
   - `process.args = ["java", <merged JVM args>, "-jar", "/app/app.jar"]`
     (or `-cp ... <mainClass>` in classpath mode).
   - `process.user` from config / pod `securityContext`.
   - `linux.resources` populated from the **pod container resource limits**
     (CPU shares/quota, memory limit) — see §10.
   - Preserve the CRI root read-only flag when selecting the Brewlet overlay:
     `readOnlyRootFilesystem: true` remains enforced by runc. Explicit writable
     volumes remain writable; the shared JDK and application mounts stay
     read-only independently of the root setting.
   - Standard pod mounts, env, hostname, and the **CNI-provided network namespace**
     injected by the kubelet/containerd (so the pod gets a normal pod IP).
   - For `brewlet.sh/cds-regenerate: "true"`, require the root-owned AppCDS
     policy sentinel, derive cache identity from the trusted CRI sandbox
     namespace + verified resolved manifest digest + exact JDK build + trusted
     CRI process UID, and mount only `<cache>/<key>` at `/run/brewlet/cds`. The
     elected writer receives a UID/GID-owned read-write directory; consumers
     with the same UID receive the same
     namespace-scoped entry read-only. The cache root and `<key>.writer` marker
     are never mounted. The shim refreshes the writer marker for the task
     lifetime and releases it when the task is deleted; an unrefreshed marker
     becomes reclaimable after the writer TTL. Marker state transitions are
     serialized through a host-only cache-root lock.
5. **Delegate to runc** to create/start the container. stdout/stderr flow back
   through containerd exactly like any container → `kubectl logs` just works.

### 6.2 Signals & lifecycle
- `Kill`/SIGTERM is forwarded to the JVM PID 1 → JVM shutdown hooks run; honors
  `terminationGracePeriodSeconds` and `preStop`.
- Exit code of the `java` process is the container exit code (drives restarts).

### 6.3 Why runc-backed (vs. a from-scratch JVM launcher)
- Reuses battle-tested cgroup v2, namespace, seccomp/AppArmor, and CNI plumbing.
- Probes (`exec`, `httpGet`, `tcpSocket`), `kubectl exec`, and metrics-server use
  the normal containerd/runc mechanisms.
- Ordinary-image ephemeral debug containers are not supported by the current
  Brewlet handler: only the CRI sandbox bypasses JVM-image assembly. A debug
  container in a Brewlet Pod still inherits that handler. Use `kubectl exec`
  with tools present in the selected runtime, or debug from a separate
  ordinary-runtime Pod; do not treat tenant annotations as authority to bypass
  runnable-image validation.
- The novel part stays small: artifact disassembly + rootfs assembly + arg building.

### 6.4 Runtime shim

The shim lives in the
[`core/shim/cmd/containerd-shim-brewlet-v2`](https://github.com/microsoft/brewlet/tree/main/core/shim/cmd/containerd-shim-brewlet-v2)
package
and builds/runs on Linux:

- It embeds containerd's `runtime/v2/shim` framework and reuses containerd's
  runc-backed Task service for the full lifecycle (`Create`/`Start`/`Kill`/
  `Delete`/`Exec`/`Wait`) (`main_linux.go`, `service_linux.go`).
- The Brewlet-specific work is a `Create()` decorator that performs §6.1
  steps 1–4: it resolves the workload image, selects the node JDK/launcher,
  assembles the **overlay rootfs** (shared RO JDK lower + per-container
  upper/work, JAR at `/app`), and rewrites the OCI spec's args/env/mounts — while
  preserving the CRI-provided namespaces (incl. the CNI netns) and cgroup
  resources. runc then does the real create/start.
- Artifact blobs are read through a pluggable resolver (`resolver.go`): a
  `containerd` backend reads the manifest + config + JAR straight from
  containerd's on-disk content store by digest (production), and a `layout`
  backend reads a Brewlet-local OCI layout (the PoC/e2e harness path). Both
  backends resolve paths only through `artifact.BlobPathIn`, which requires a
  canonical `sha256:<64 lowercase hex>` digest and independently confirms the
  result stays under the store's `blobs/sha256` directory. Blob bytes are hashed
  and checked against the declaring descriptor before they are read, staged, or
  bind-mounted, so a descriptor inside a tenant-authored manifest can neither
  escape the content store nor substitute unrelated content.
- Runnable-image staging is immutable after publication. Extraction completes in
  a private directory before the complete stage becomes visible atomically.
  Repeated and concurrent resolutions, including separate shim processes, reuse
  the published stage without truncating or replacing files held by existing
  workloads. Failed extraction never publishes an incomplete stage.

The workload image reference and manifest digest hints are managed **cluster-side,
not in the shim**: the `brewlet-admission` webhook (§8.3) overwrites the
`brewlet.sh/artifact-container`, `brewlet.sh/artifact-ref`, and
`brewlet.sh/artifact-digest` compatibility hints onto
`runtimeClassName: brewlet` pods, mirrored from the selected Pod image. The shim
requires the protected CRI requested image to be digest-pinned, resolves that
exact target directly from containerd's content store, cross-checks
containerd's protected `io.kubernetes.cri.image-name` OCI annotation, and
verifies the selected platform manifest's config digest against CRI's recorded
config identity. It never looks up a target through the config digest because
multiple manifests can share that config while carrying different launch
metadata. Brewlet hints do not select executable content. For AppCDS, the shim
uses the verified selected platform-manifest digest—not a Pod annotation—as
cache identity. On non-Linux dev hosts
only the portable bundle-assembly core builds locally;
[integration-test tier 3](../integration-tests/e2e/tier3-runc.sh)
exercises the real Linux/runc path against the monorepo's core and Kubernetes
sources.

---

## 7. RuntimeClass

```yaml
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata:
  name: brewlet
handler: brewlet                 # matches the containerd runtime name (§5.2)
scheduling:
  nodeSelector:
    brewlet.sh/runtime: "ready"  # only land on provisioned nodes
overhead:
  podFixed:
    memory: "64Mi"                # JVM/runtime baseline overhead accounting
    cpu: "50m"
```

A raw Pod/Deployment is enough to use Brewlet — set `runtimeClassName: brewlet`
and reference the runnable OCI image as the container `image`. The CRD (§9) is a
higher-level convenience on top of this.

---

## 8. The Operator (`brewlet-operator`)

Two responsibilities: node/shim lifecycle analogous to SpinKube's Runtime Class
Manager, and workload reconciliation analogous to Spin Operator:

### 8.1 Node lifecycle controller
- Watches `Node` objects and reflects each provisioned node's state.
- `NodeProfileReconciler` reconciles each `NodeProfile` (§5.6) into a per-profile
  provisioner DaemonSet + the shared `RuntimeClass`, with finalizer-driven
  cleanup. Source pins, mirrors, and pool conflicts are validated before the
  privileged DaemonSet is created or updated.
- Surfaces node readiness/health and JDK inventory as conditions/events.

> Built as a controller-runtime operator in
> the [`kubernetes/`](../kubernetes)
> module (separate from the shim, to isolate client-go /
> controller-runtime from the containerd-pinned deps). Two controllers split the
> old monolith:
>
> - **`NodeProfileReconciler`** owns the provisioning mechanism (§5.6). For each
>   `NodeProfile` it validates the complete profile policy, resolves the pool
>   key, and builds the per-profile
>   `brewlet-node-provisioner-<profile>` DaemonSet (pool `nodeAffinity`, indexed
>   `JDK_SOURCE_*`/`LAUNCHER_SOURCE_*`, `MIRRORS`,
>   `SOURCE_ALLOWED_MIRROR_HOSTS`, and `BREWLET_CONTAINERD_RESTART` env from the
>   spec and operator policy),
>   ensures the `brewlet` RuntimeClass, and reports `assignedNodes` / `readyNodes`
>   and a `Ready` condition on status (reasons: `AllNodesProvisioned`,
>   `Provisioning`, `EmptyPool`, `NodeFailure`, `InvalidProfile`,
>   `CleanupPending` — §14.3). A
>   validation failure reports `Ready=False`, reason `InvalidProfile`, deletes
>   or withholds the profile DaemonSet, and removes runtime/JDK/feature/launcher
>   labels and inventory annotations only from nodes whose profile annotation
>   identifies that profile. A
>   `node.brewlet.sh/cleanup` finalizer holds a deleted profile while a
>   `brewlet-cleanup-<profile>` DaemonSet reverses host state; the object is only
>   GC'd once host cleanup and worker teardown complete.
> - **`NodeReconciler`** is now a per-node *state mirror*: it watches provisioned
>   nodes (pool membership + the legacy `brewlet.sh/provision` label, gated on the
>   runtime-ready label) and reflects state via the `brewlet.sh/provision-state`
>   annotation plus `Provisioning` / `NodeReady` / `ProvisionFailed` events (§14),
>   reading the `brewlet.sh/provision-error` annotation the provisioner writes on
>   failure. DaemonSet/RuntimeClass ownership moved to `NodeProfileReconciler`.
>
> RBAC + Deployment ship in
> [`config/operator.yaml`](../kubernetes/config/operator.yaml)
> and the [`charts/brewlet`](../kubernetes/charts/brewlet)
> Helm chart. The `NoCompatibleJDK` / `NoCompatibleLauncher`
> events are owned by the pod admission/scheduling webhook (§8.3).

### 8.2 `JavaApplication` controller (developer ergonomics)
- Watches the `JavaApplication` CRD (§9) and reconciles it into a managed
  `Deployment` + `Service` + (optional) `HPA`, with:
  - `runtimeClassName: brewlet`,
  - the container `image` = the runnable OCI image ref,
  - `resources` copied from the descriptor (enforced as the sandbox cgroup),
  - user-supplied `jvm.args` delivered as argv via the `brewlet.sh/jvm-args` pod
    annotation, and `env` wired through verbatim (Brewlet injects no tuning of its own),
  - probes, ports, and env wired through.
- Owns and continuously reconciles the generated objects (GC via owner refs).
- Removing a Service (disabled or no ports) or disabling autoscaling deletes only
  a resource controlled by the current `JavaApplication` UID. Independently
  managed resources, including those referencing an older same-name application,
  are preserved. Deletion checks both UID and resource version so concurrent
  replacement or ownership changes cause a retry, not deletion of the changed object.
- `env[].valueFrom` preserves Kubernetes Secret, ConfigMap, pod-field, and
  container-resource references, including selector options. The CRD also
  preserves `fileKeyRef`; using it requires the corresponding Kubernetes
  feature support and a volume supplied by the platform. Brewlet does not resolve
  referenced values or copy secrets into the descriptor.

This is what the prompt calls *“provisioning a container with CPU and Memory limits
as per deployment descriptor.”* The descriptor is the `JavaApplication`.

> Built as `JavaApplicationReconciler` in the operator
> module (`internal/controller/javaapplication_controller.go`, with pure,
> unit-tested builders in `javaapplication_resources.go`). It reconciles the
> Deployment/Service/HPA, owns them via controller references, stamps the
> `brewlet.sh/jdk` / `brewlet.sh/launcher` pod annotations the admission webhook
> (§8.3) consumes, delivers `jvm.args` as argv via the
> `brewlet.sh/jvm-args` pod annotation, and reports
> `readyReplicas` / `selectedJdk` / a `Ready` condition on status. The API types
> live in `api/v1alpha1`; RBAC ships in `config/operator.yaml` and the
> [`charts/brewlet`](../kubernetes/charts/brewlet)
> Helm chart (which also installs the CRD).
>
> **`jvm.args` delivery (`brewlet.sh/jvm-args`) — public contract.** The value is
> a **JSON array of strings**, e.g. `["-XX:MaxRAMPercentage=75.0","-XX:+UseZGC"]`.
> The operator stamps it from `spec.jvm.args`; a user may set it directly on a raw
> pod (§9.2). The shim decodes it and appends the elements to the launcher argv
> immediately before the entrypoint (§4.2). The containerd runtime configuration
> forwards `brewlet.sh/*` pod annotations onto the OCI spec (§5.2), so no node
> reconfiguration is required to adopt it.
>
> Brewlet does **not** set `JDK_JAVA_OPTIONS` (or `JAVA_TOOL_OPTIONS`). The `java`
> launcher *prepends* those variables to the command line, which would place
> deployment tuning **before** the artifact's own flags and let the artifact win —
> the inverse of the §4.2 contract — and joining the args with whitespace would
> corrupt any argument containing a space. A user-supplied
> `JDK_JAVA_OPTIONS`/`JAVA_TOOL_OPTIONS` in `spec.env` is still passed through
> untouched and still honored by the JVM; because the launcher applies it before
> argv, `jvm.args` win on conflict. That overlap is reported as a
> `JVMArgsApplied=True` condition with reason `EnvOptionsOverlap` plus a warning
> event. Both are applied — neither side is dropped. Args that would select the
> entrypoint (§4.2) fail validation with `Ready=False`, reason `ReconcileError`,
> and are independently rejected by the shim.
>
> **Autoscaling (HPA).** When `spec.autoscaling.enabled` is `true`, the controller
> renders an `autoscaling/v1` `HorizontalPodAutoscaler` targeting the managed
> Deployment (`minReplicas` / `maxReplicas` /
> `targetCPUUtilizationPercentage`), and does not reconcile the Deployment's
> `replicas` so the HPA owns scaling. Disabling autoscaling deletes the managed HPA and
> restores `spec.replicas` (default `1`). The HPA is owned via a controller
> reference and garbage-collected with the `JavaApplication`.

### 8.3 Pod admission/scheduling webhook

A mutating+validating admission webhook (`brewlet-admission`) closes the loop
between a brewlet pod and the ready fleet. For every pod on CREATE with
`runtimeClassName: brewlet` it:

- **Overwrites** `brewlet.sh/artifact-container` with the selected regular
  container name, `brewlet.sh/artifact-ref` with that container's `image`, and,
  when the ref is digest-pinned (`repo@sha256:…`),
  `brewlet.sh/artifact-digest` as Pod-wide compatibility hints. Other tasks in a
  multi-container Pod ignore those shared hints. The shim resolves each
  executable image target digest from containerd-owned CRI requested-image
  metadata, requires it to be digest-pinned, requires containerd's protected
  `io.kubernetes.cri.image-name` annotation to agree, verifies the selected
  platform manifest against CRI's image-config digest, and reads the JAR from
  containerd's content store by digest (§6.4); malformed or conflicting hints
  are rejected, but hints never select executable content.
- **Matches** any explicitly requested JDK/launcher/architecture/AppCDS policy
  (pod annotations
  `brewlet.sh/jdk` = `<dist>-<feature>` or a bare feature such as `21`, and
  `brewlet.sh/launcher`, `brewlet.sh/arch` for non-portable artifacts,
  and `brewlet.sh/cds-regenerate`) against the same ready node. If no ready node
  is compatible, admission is denied with `NoCompatibleJDK`,
  `NoCompatibleLauncher`, `NoCompatibleArch`, or
  `AppCDSRegenerationDisabled` (§14).
- **Steers** scheduling by injecting `nodeAffinity` onto the provisioner's
  per-capability labels (`brewlet.sh/jdk.<d-f>`, `brewlet.sh/jdk-feature.<f>`,
  `brewlet.sh/launcher.<n>`, `brewlet.sh/appcds-regeneration`) and the standard
  `kubernetes.io/arch` label, using the operators defined by the public
  [capability-label contract](CAPABILITY_LABELS.md), so the scheduler skips
  incompatible nodes rather than failing at runtime.

Non-brewlet pods pass through untouched; a pod with no explicit JDK/launcher or
regeneration request is admitted with just the compatibility hints overwritten,
and the shim defaults the JDK to feature 21 (lexically-first installed
distribution) and the launcher to `java`. `failurePolicy: Ignore` ensures a
webhook outage never blocks workloads. For AppCDS, the shim's root-owned sentinel
check remains authoritative, so fail-open admission cannot authorize
regeneration. Runtime identity resolution also fails closed if the shim cannot
determine the containerd-resolved image.

> Built as a second binary in the operator module
> ([`cmd/admission`](../kubernetes/cmd/admission)
> + pure, unit-tested logic in
> `internal/admission`). Deployed — with a self-signed serving cert — by the
> [`charts/brewlet`](../kubernetes/charts/brewlet)
> Helm chart (`admission.enabled=true`).
>
> **NodeProfile validation.** The same binary also serves a *validating* webhook
> at `/validate-nodeprofiles` (`NodeProfileValidator`): on `NodeProfile`
> CREATE/UPDATE it rejects an empty JDK list, any JDK or launcher missing its
> source, any source not using a canonical SHA-256 digest ref, invalid names or
> paths, malformed mirror mappings, destinations outside
> `--allowed-source-mirror-hosts`, an invalid
> `containerdRestart`, and — after freshly listing existing profiles — overlapping
> named pools, incompatible explicit/auto pool keys, or multiple catch-alls
> (`PoolConflict`). Profile-list errors fail closed. Unlike the pod webhook it is
> configurable with `admission.nodeProfileFailurePolicy`. It defaults to
> `Ignore` so certificate bootstrap cannot block profile creation; operators can
> select `Fail` for synchronous transport-failure rejection once webhook
> availability is guaranteed. The reconciler repeats the same policy, so
> admission is not the privileged security boundary (§5.6).

---

## 9. `JavaApplication` CRD (the deployment descriptor)

`apiVersion: apps.brewlet.sh/v1alpha1`

```yaml
apiVersion: apps.brewlet.sh/v1alpha1
kind: JavaApplication
metadata:
  name: orders-api
  namespace: payments
spec:
  artifact:
    image: registry.example.com/team/orders@sha256:REPLACE_WITH_IMAGE_DIGEST
    pullPolicy: IfNotPresent
    pullSecrets: [regcred]
  replicas: 3
  resources:
    requests: { cpu: "500m", memory: "512Mi" }
    limits:   { cpu: "2",    memory: "1Gi" }
  jvm:
    version: 21                  # JDK feature version; authoritative scheduling/runtime request
    distribution: temurin        # optional; pins <distribution>-<feature>. Omit to accept any distribution of this feature
    launcher: java               # vanilla OpenJDK launcher (default/omittable); tune the JVM yourself via args
    # Brewlet injects no -XX flags of its own; the container-aware JDK reads the
    # cgroup memory/cpu limits directly. Set any tuning explicitly here:
    args: ["-XX:MaxRAMPercentage=75.0", "-XX:+UseZGC", "-XX:+ExitOnOutOfMemoryError"]
  env:
    - name: SPRING_PROFILES_ACTIVE
      value: prod
  ports:
    - name: http
      containerPort: 8080
  service:
    enabled: true
    type: ClusterIP
  probes:
    readiness: { httpGet: { path: /actuator/health/readiness, port: 8080 } }
    liveness:  { httpGet: { path: /actuator/health/liveness,  port: 8080 } }
  autoscaling:
    enabled: true
    minReplicas: 3
    maxReplicas: 10
    targetCPUUtilizationPercentage: 70
status:
  observedGeneration: 4
  readyReplicas: 3
  selectedJdk: "temurin-21"      # mirrors the jvm request: "<distribution>-<feature>", or a bare feature when no distribution was pinned
  conditions:
    - type: Ready
      status: "True"
```

### 9.1 Minimal example (the SpinKube-style declarative workload)

```yaml
apiVersion: apps.brewlet.sh/v1alpha1
kind: JavaApplication
metadata: { name: hello }
spec:
  artifact: { image: registry.example.com/demo/hello@sha256:REPLACE_WITH_IMAGE_DIGEST }
  resources:
    limits: { cpu: "1", memory: "512Mi" }
  ports: [{ name: http, containerPort: 8080 }]
```

### 9.2 Raw equivalent (no CRD, just RuntimeClass)

```yaml
apiVersion: apps/v1
kind: Deployment
metadata: { name: hello }
spec:
  replicas: 1
  selector: { matchLabels: { app: hello } }
  template:
    metadata:
      labels: { app: hello }
      annotations:
        brewlet.sh/jdk: "21"
        brewlet.sh/launcher: java
        # The raw-pod equivalent of spec.jvm.args (§8.2). A JSON array of
        # strings, appended to the launcher argv before the entrypoint (§4.2).
        brewlet.sh/jvm-args: '["-XX:MaxRAMPercentage=75.0","-XX:+UseZGC"]'
    spec:
      runtimeClassName: brewlet
      containers:
        - name: hello
          image: registry.example.com/demo/hello@sha256:REPLACE_WITH_IMAGE_DIGEST
          resources: { limits: { cpu: "1", memory: "512Mi" } }
          ports: [{ containerPort: 8080 }]
```

Raw pods have no controller to validate the annotation, so the shim is the
enforcement point: a value that is not a JSON array of strings, or that contains
an entrypoint-selecting flag (§4.2), fails task creation rather than launching
with the tuning silently dropped.

### 9.3 Choosing a launcher: vanilla `java` vs. `jaz`

The two launchers represent two philosophies of JVM tuning. Brewlet itself injects
**no** `-XX` flags in either case — the difference is who does the tuning.

**Vanilla `java` (default) — you tune it.** The container-aware JDK reads the
cgroup limits; you set heap/GC/etc. explicitly:

```yaml
  jvm:
    version: 21
    launcher: java                 # default; may be omitted
    args:                          # tuning is YOUR responsibility
      - "-XX:MaxRAMPercentage=75.0"
      - "-XX:+UseZGC"
      - "-XX:+ExitOnOutOfMemoryError"
```

**`jaz` — it tunes for you.** The [Azure Command Launcher for
Java](https://learn.microsoft.com/java/jaz/overview) inspects the container's
resources and picks sensible JVM ergonomics automatically, so you typically pass
**no** manual tuning flags. Do not restate what `jaz` derives (e.g.
`MaxRAMPercentage`); reserve `args` for genuinely app-specific flags only:

```yaml
  jvm:
    version: 21
    launcher: jaz                  # auto-tunes heap/GC/CPU from the cgroup limits
    # no MaxRAMPercentage / GC selection needed — jaz derives them
    # args: ["-Dfoo=bar"]          # only truly app-specific flags, if any
```

> The node must have the requested launcher installed (§5.4); otherwise the pod
> fails admission with `NoCompatibleLauncher`.

---

## 10. Resource Limits ↔ JVM Mapping

The deployment descriptor's CPU/memory drive the **sandbox cgroup only**. Brewlet
injects **no JVM tuning flags**: modern JDKs are container-aware (`cgroup v2`) and
read the sandbox limits directly. Artifact launch knobs carry app-intrinsic
correctness flags only; JVM tuning is the **user's** responsibility, set via the
descriptor's `jvm.args`.

**Standalone bundle inputs.** `brewlet bundle` and the shim's local
`prepare-bundle` path reject malformed, nonpositive, or overflowing limits before
writing a bundle. Empty/omitted limits mean unlimited. CPU limits use a 100ms
period and must be at least `10m` (`0.01` CPU), the minimum supported Linux quota
for that period. This validation does not alter production CRI resource
forwarding or Kubernetes resource semantics.

| Descriptor field          | Cgroup effect (via runc)        | JVM effect                                             |
|---------------------------|---------------------------------|-------------------------------------------------------|
| `resources.limits.memory` | `memory.max`                    | `-XX:+UseContainerSupport` (default on) reads the cgroup and sizes the heap |
| `resources.limits.cpu`    | `cpu.max` (quota/period)        | cgroup-aware JDK auto-detects available processors from the quota; GC/JIT thread counts scale |

**Recommended user-set tuning (not injected by Brewlet):**
- Container memory must leave headroom for **non-heap** (Metaspace, thread stacks,
  code cache, direct/`mmap` buffers, GC structures). Users typically set
  `-XX:MaxRAMPercentage` (e.g. `75.0`, reserving ~25%) via `jvm.args`.
- `-XX:+ExitOnOutOfMemoryError` recommended so OOM → clean restart by the kubelet.
- Modern JDKs are cgroup-v2 aware; Brewlet **requires cgroup v2** on nodes.
- A custom launcher (e.g. `jaz`) can auto-tune these from the cgroup limits on the
  user's behalf.

**Requests vs. limits.** Only *limits* become the sandbox cgroup and therefore only
limits are visible to the container-aware JDK; *requests* are a scheduling concern.
Brewlet copies `spec.resources` verbatim and never defaults one side from the
other, so each shape behaves as plain Kubernetes does:

| Descriptor shape             | Cgroup / JVM consequence                                                                 |
|------------------------------|------------------------------------------------------------------------------------------|
| request < limit (Burstable)  | Cgroup uses the **limit**; the JDK sizes the heap from the limit, not the request           |
| limit, no request            | Kubernetes defaults the request from the limit at admission; the cgroup is the limit         |
| request, no limit            | **No cgroup ceiling** — the JDK sees host-visible memory/CPU. Set a limit or an explicit `-Xmx` |
| neither                      | Unbounded (BestEffort); the JDK sees host-visible memory/CPU                                 |

---

## 11. Security Model

- **Isolation parity with containers.** Because execution is runc-backed, workloads
  get the same namespace/cgroup/seccomp/AppArmor isolation as ordinary pods. The JAR
  is treated as untrusted code.
- **Deployment-authoritative process identity.** `JavaApplication` workloads and
  standalone bundles default to UID/GID `65532:65532`. Raw Pods select identity
  through `securityContext`, and the shim preserves the CRI-populated OCI user
  unchanged. Generated workloads also use `RuntimeDefault` seccomp, disable
  privilege escalation, and drop all Linux capabilities. Artifact metadata
  cannot carry process credentials and a config containing `user` is rejected.
  Root is possible only through an explicit, trusted deployment/runtime choice.
- **Artifact identity.** Digest-pinned references are mandatory for Kubernetes
  execution. The shim resolves the exact protected CRI target from the content
  store and verifies its selected platform-manifest config against CRI metadata;
  tag-only requests fail closed.
- **Publishing credentials stay on the registry origin.** A publishing client
  (§4.3) holds developer or CI registry credentials, so it MUST use HTTPS for
  every registry except an exact loopback authority or one an operator has
  explicitly marked insecure, and it MUST attach credentials only to requests
  whose scheme, host, and port match the configured registry. Registry-supplied
  URLs — bearer token realms, blob upload `Location` values, pagination links —
  are attacker-influenced input: they may be followed, but a cross-origin target
  MUST NOT receive credentials unless the operator allow-listed it, and an
  unusable authenticated exchange MUST fail closed rather than fall back.
- **Privileged provisioning is the sharp edge.** As with SpinKube's Runtime Class
  Manager, the node provisioner is privileged and mutates the host. Scope it to
  platform-owned node pools; document the blast radius, and do not use it on
  hostile multi-tenant nodes.
- **JDK CVE management is centralized.** Patching the node JDK patches *all*
  workloads at once — a major advantage over per-image JVMs.

---

## 12. Networking, Observability, Day-2

- **Networking:** normal pod IP via CNI (runc owns the netns). Services/Ingress/
  NetworkPolicy unchanged.
- **Logs:** JVM stdout/stderr → containerd → `kubectl logs`.
- **Metrics/Tracing:** JMX/Micrometer/OTel work as usual; JFR can be enabled via
  `jvm.args`.
- **Brewlet runtime metrics:** runtime metrics are opt-in and disabled by
  default. When enabled, every long-lived provisioner pod also runs a node-local
  Prometheus exporter. The Runtime v2 shim emits best-effort,
  bounded-cardinality events over a Unix datagram socket under `/opt/brewlet`;
  exporter loss never changes launch behavior. The endpoint reports launch phase
  latency/outcomes, artifact-resolution behavior, AppCDS regeneration decisions,
  and installed JDK/launcher inventory. `overlay_setup` measures preparation of
  the overlay mount configuration; `runc_create` includes applying that mount and
  creating the sandbox. The operator and admission webhook add
  NodeProfile readiness/provisioning metrics and admission outcomes (including
  `NoCompatibleJDK`, `NoCompatibleLauncher`, and `NoCompatibleArch`) to their
  controller-runtime endpoints.
- **Metric discovery:** with `metrics.enabled=true`, the Helm chart creates
  Services for node, operator, and admission metrics.
  `metrics.serviceMonitor.enabled` and
  `metrics.grafanaDashboard.enabled` add optional Prometheus Operator and Grafana
  resources.
- **Scope:** the shim cannot reliably determine whether kubelet/containerd
  satisfied an earlier image pull from cache, so Brewlet reports resolution
  duration/backend/format rather than a false cache-hit signal. JDK metrics expose
  exact build/source and node installation time; installation age is not presented
  as the upstream patch release age.
- **Probes & exec:** `kubectl exec` and all probe types use containerd/runc.
  Ordinary-image ephemeral debug containers remain unsupported by the Brewlet
  handler (§6.3).
- **Upgrades:** JDK roots are versioned and additive on nodes. Rotating a root
  renames the previous one aside (`<root>.retired.<epoch>.<pid>`) rather than
  deleting it, because overlayfs resolves `lowerdir` at mount time — so running
  pods keep the root they started with. A later provisioning pass reclaims a
  rotated-out root once no live mount references it *and* it has aged past a
  grace period (`BREWLET_RETIRED_GRACE_SECONDS`, default 1h); if the mount table
  cannot be read the root is retained. Deleting a `NodeProfile` removes the
  profile's installed roots during cleanup unless
  `BREWLET_CLEANUP_RUNTIME_ROOTS=false`. Roots that merely drop out of a
  profile's inventory while the profile still exists are not yet reclaimed —
  reference-counted GC of de-inventoried roots is [roadmap](../ROADMAP.md) work.
- **Multi-arch:** JDK roots installed per node architecture (amd64/arm64); the JAR
  artifact is arch-independent, so the *same* artifact runs on any provisioned arch
  (see [multi-arch](https://github.com/microsoft/brewlet/blob/main/docs/multi-arch.md)).

---

## 13. Performance & Startup

Compared with SpinKube's smaller Wasm footprint, fast startup, and low idle
resource usage, Brewlet prioritizes compatibility with existing JVM applications.
It offsets the JVM's heavier startup and memory profile with node-resident caching
and JVM features:

- **Shared, pre-warmed JDK** on the node → no per-pod JDK pull/unpack.
- **Artifact caching:** containerd content store caches the JAR layer; only the
  (small) JAR moves over the network, not a full image.
- **AppCDS / dynamic CDS:** optionally ship a class-data archive as a
  `cds.layer.v1+jsa` layer (`brewlet push --appcds-archive`) to cut startup; it is
  mounted at `/app/<archive>` and consumed with `-Xshare:auto` so a JDK-build
  mismatch falls back safely to base CDS. Alternatively opt into **node-side
  regeneration** at the deployment level (`spec.jvm.cds.regenerate` on the
  `JavaApplication` CRD → `brewlet.sh/cds-regenerate` pod annotation; `brewlet
  run/bundle --appcds-regenerate` locally). Kubernetes regeneration additionally
  requires `NodeProfile.spec.appCDS.regenerationEnabled=true`. The node maintains
  a private per-`(namespace, verified-platform-manifest, JDK-build, process-UID)`
  archive cache; the platform manifest is derived from the CRI/containerd-
  authoritative image target. The cache is driven by
  `-XX:+AutoCreateSharedArchive` (JDK 19+) and self-heals on every central JDK
  patch. Workloads receive only their single entry directory, never the
  node-shared cache root. See the
  [AppCDS note](https://github.com/microsoft/brewlet/blob/main/docs/appcds.md).

---

## 14. Failure Modes & Edge Cases

| Scenario                                   | Behavior                                                            |
|--------------------------------------------|--------------------------------------------------------------------|
| No compatible JDK on any ready node        | Pod **rejected at admission**, `403` with `reason: NoCompatibleJDK`   |
| Requested launcher not installed on node   | Pod **rejected at admission**, `403` with `reason: NoCompatibleLauncher` |
| Non-portable JAR needs an arch with no ready node | Pod **rejected at admission**, `403` with `reason: NoCompatibleArch` |
| Requested capability exists but its nodes are momentarily unschedulable | Pod admitted with `nodeAffinity`; stays `Pending` with a scheduler `FailedScheduling` event |
| AppCDS regeneration requested with no authorized compatible node | Admission denies with `AppCDSRegenerationDisabled`; shim also rejects task creation when the host sentinel is absent or unsafe |
| OCI artifact missing/unauthorized          | `ImagePull`-style failure surfaced on the pod                       |
| JVM OOM                                     | `ExitOnOutOfMemoryError` → exit → kubelet restart per `restartPolicy`|
| Node provisioning fails                     | Node not labeled `ready`; operator event `ProvisionFailed`          |
| Runtime source or mirror preflight fails       | Provisioner performs no pull/mount/copy, publishes no readiness, and records the source-policy error |
| NodeProfile names a pool with no matching nodes | Profile `Ready=False` reason `EmptyPool`; DaemonSet lands nowhere |
| NodeProfile has a mutable source, unauthorized mirror, or pool conflict | Rejected at admission when available; active reconciliation reports `InvalidProfile` and withdraws profile-owned advertisements. Deleting profiles with possible host state retain finalizers/claims with `CleanupBlocked` until repaired |
| NodeProfile targets change | Retire recorded departing identities; keep their claims until host cleanup and worker teardown finish, then reconcile the latest desired targets |
| NodeProfile deleted                         | Held by `node.brewlet.sh/cleanup` finalizer until recorded-target cleanup and worker teardown finish; missing identity or ownership evidence blocks completion |
| Shim crash                                  | containerd reports task failure; pod restarts                       |
| cgroup v1-only node                         | Provisioner refuses; node not marked ready (cgroup v2 required)     |
| containerd 1.x node                         | Provisioner refuses; node not marked ready (protected CRI requested-image metadata requires containerd 2.0+) |

> The `NoCompatibleJDK` / `NoCompatibleLauncher` / `NoCompatibleArch` /
> `AppCDSRegenerationDisabled` rows are enforced by the pod admission webhook
> (§8.3): an incompatible explicit request is denied at admission with that
> reason, and compatible pods get nodeAffinity so the scheduler skips nodes
> lacking the requested capability.
>
> **These are admission denials, not events.** The webhook returns them as the
> `reason` of a `403` `metav1.Status`; it has no event recorder, and the pod
> object is never created, so there is nothing to attach an event to. For a
> workload owned by a Deployment/ReplicaSet the reason still surfaces on the
> owner as a `FailedCreate` warning event and on the ReplicaSet's
> `ReplicaFailure` condition; for a bare `kubectl create` it is the CLI error.
> A pod that requests nothing imposes no constraint and is admitted. The
> `arch` constraint (mapped to the kubelet-provided `kubernetes.io/arch` label) is
> optional and only needed for non-portable JARs that bundle JNI native libraries;
> arch-neutral bytecode artifacts leave it unset and run on any provisioned arch.
> AppCDS admission remains a scheduling/early-denial aid; the root-owned host
> sentinel is the authoritative authorization because webhook failure policy is
> `Ignore`.

---

### 14.1 Observable contract (stable surfaces)

Everything below is a **public contract**: consumers may parse it, alert on it,
and branch on it. Free-form human text is always carried in a companion field
that is explicitly *not* a contract.

#### Node annotations the provisioner publishes

| Annotation | Value | Contract |
|---|---|---|
| `brewlet.sh/jdks` | Comma-separated `<distribution>-<feature>` tokens, e.g. `temurin-21,microsoft-25` | Installed JDK inventory |
| `brewlet.sh/jdks-info` | JSON array, one object per root: `{"distribution","vendor","feature","version","arch"}` | Diagnostic inventory. `vendor`/`version`/`arch` are read from the installed JDK itself, so they describe what is really on the node. A root whose `java` cannot be executed is omitted |
| `brewlet.sh/launchers` | Comma-separated launcher names, always including the implicit `java` | Installed launcher inventory |
| `brewlet.sh/profile` | The owning `NodeProfile`'s `metadata.name` | Which profile last provisioned this node |
| `brewlet.sh/profile-generation` | Decimal `metadata.generation` of that profile | Distinguishes an up-to-date node from a stale one |
| `brewlet.sh/provision-error` | A reason **code** from §14.2 | Machine-readable failure cause |
| `brewlet.sh/provision-error-message` | Free-form text | **Not a contract.** Human detail only; wording may change at any time |
| `brewlet.sh/provision-state` | `Provisioning` \| `Ready` \| `Failed` | The *operator's* view of the lifecycle. Distinct from the `brewlet.sh/runtime=ready` label, which the *provisioner* owns and which drives scheduling |

None of these drive scheduling; the per-capability **labels** do
([`CAPABILITY_LABELS.md`](CAPABILITY_LABELS.md)). Annotations cannot back a
`nodeAffinity`.

### 14.2 `provision-error` reason codes

The provisioner publishes exactly one of these tokens. Each is lowercase
alphanumerics and `-`, at most 63 characters, and is normalized before
publication so an unexpected value can never become prose or an unbounded
string. New codes may be added in a minor release; existing codes are not
repurposed.

| Code | Meaning |
|---|---|
| `invalid-source-ref` | A configured source image is not a fully qualified, tagless, digest-pinned reference |
| `invalid-source-path` | A configured `javaHome`/launcher path failed path-policy validation |
| `invalid-mirror-config` | `registry.mirrors` / the mirror allowlist is malformed, self-referential, duplicated, or names an unapproved destination |
| `source-policy-validator-missing` | The source-policy validator binary is absent or not executable in the provisioner image |
| `invalid-jdk-source` | A JDK source entry is missing fields, has a malformed `<distribution>-<feature>` token, or is duplicated |
| `invalid-launcher-source` | A launcher source entry is missing fields, has a malformed or reserved name, or is duplicated |
| `invalid-restart-mode` | `rollout.containerdRestart` is not `validated` / `sighup` / `none` |
| `unsupported-architecture` | The node's architecture is not supported |
| `host-tooling-missing` | A required host tool (`nsenter`, the host `ctr` helper) is unavailable |
| `completion-state-failed` | The container-local readiness marker could not be reset or published; the operation exits unsuccessfully |
| `ownership-fence-failed` | Node/profile identity or durable writer authority is missing or stale; an initial fence failure leaves host state and node advertisements untouched and is reported in container logs |
| `containerd-version-unavailable` | The host containerd server version could not be queried |
| `containerd-version-invalid` | The reported containerd server version could not be parsed |
| `unsupported-containerd-version` | containerd is older than 2.0 (protected CRI requested-image metadata is required) |
| `cgroup-v2-required` | The node is cgroup-v1 only |
| `shim-image-incomplete` | The provisioner image is missing the shim / `ctr` / `crictl` binary |
| `shim-install-failed` | The shim was not present after installation |
| `jdk-source-missing` | No configured source exists for a requested JDK token (there is no built-in catalog — §5.2) |
| `jdk-install-failed` | Mounting, copying, activating, or retaining a JDK root failed |
| `jdk-validation-failed` | An installed JDK failed its `java -version` smoke test |
| `launcher-source-missing` | No configured source exists for a requested launcher |
| `launcher-install-failed` | Mounting, copying, activating, or retaining a launcher failed |
| `launcher-<name>-missing` | The named launcher's binary is absent. `<name>` is sanitized and truncated to 39 characters so the whole code stays within the 63-character limit |
| `launcher-<name>-not-executable` | The named launcher's binary is present but not executable |
| `containerd-config-missing` | The host containerd configuration file was not found |
| `containerd-config-invalid` | The rendered containerd configuration failed validation |
| `restart-failed` | The containerd service could not be restarted |
| `containerd-health-check-failed` | containerd was not operational after a restart |
| `runtime-handler-health-check-failed` | The `brewlet` runtime handler was not available after a restart |
| `rollback-failed` | Brewlet could not restore the previous containerd configuration — the node needs manual attention |
| `stale-mount-cleanup-failed` | A stale source mount from an earlier attempt could not be removed |
| `profile-changed` | The owning `NodeProfile` was deleted or changed identity mid-provision |
| `cleanup-failed` | Profile cleanup could not reverse host state |
| `policy-apply-failed` | The AppCDS regeneration sentinel could not be applied or removed |
| `node-advertisement-failed` | Node readiness/inventory could not be published or withdrawn |
| `internal-error` | A code was empty or unrecognized after normalization |

When a rollback succeeds, the code published is the **original** reason, not
`rollback-failed`, so the cause is preserved. The operator's `NodeReconciler`
surfaces `<code>: <message>` on the `ProvisionFailed` event (§8.1) and
`NodeProfileReconciler` on the owning profile's `Ready=False` / `NodeFailure`
reason (§5.5).

### 14.3 Condition reasons and denial reasons

| Object | Condition | Reasons |
|---|---|---|
| `JavaApplication` | `Ready` | `Reconciled`, `Progressing`, `ReconcileError` |
| `JavaApplication` | `JVMArgsApplied` | `ArgsDelivered`, `EnvOptionsOverlap` (§8.2) |
| `NodeProfile` | `Ready` | `AllNodesProvisioned`, `Provisioning`, `EmptyPool`, `NodeFailure`, `InvalidProfile`, `OwnershipConflict`, `OwnershipMigration`, `Retargeting`, `CleanupBlocked`, `CleanupPending`, `CleanupTeardown` |
| `NodeProfile` | `CleanupComplete` | `CleanupSucceeded` (current-generation host cleanup finished; worker teardown may still be pending) |

Event reasons: `Provisioning`, `NodeReady`, `ProvisionFailed`, `NodeUnmatched`
(node lifecycle, §8.1); `ReconcileError`, `EnvOptionsOverlap` (`JavaApplication`,
§8.2). Admission **denial** reasons (§8.3, returned as a `403` `metav1.Status`
reason — not events): `NoCompatibleJDK`, `NoCompatibleLauncher`,
`NoCompatibleArch`, `AppCDSRegenerationDisabled`, `InvalidNodeProfile`,
`PoolConflict`.

### 14.4 Metrics

Metric **names** are a contract; label sets may gain labels in a minor release.
All are opt-in (`metrics.enabled=true`) and bounded-cardinality.

| Metric | Emitted by |
|---|---|
| `brewlet_sandbox_launches_total`, `brewlet_sandbox_launch_duration_seconds` | Shim → node exporter |
| `brewlet_artifact_resolution_duration_seconds` | Shim → node exporter |
| `brewlet_cds_regeneration_decisions_total`, `brewlet_cds_archive_mapped` | Shim → node exporter |
| `brewlet_jdk_info`, `brewlet_jdk_installed_timestamp_seconds`, `brewlet_launcher_info` | Node exporter |
| `brewlet_telemetry_events_invalid_total` | Node exporter |
| `brewlet_node_provision_transitions_total` | Operator |
| `brewlet_nodeprofile_condition`, `brewlet_nodeprofile_nodes` | Operator |
| `brewlet_admission_requests_total` | Admission webhook |

### 14.5 Host path layout (`/opt/brewlet`)

The provisioner root, overridable with `BREWLET_PREFIX`. It is platform-owned
host state, not a workload interface: a workload only ever receives the specific
read-only mounts §6.1 describes.

| Path | Contents |
|---|---|
| `bin/containerd-shim-brewlet-v2` | The installed shim binary |
| `jdks/<distribution>-<feature>/` | An installed JDK runtime root. `.brewlet-java-home` records the JDK home inside it and `.brewlet-source` its source ref |
| `jdks/.brewlet-active` | Newline-separated list of the currently inventoried JDK tokens |
| `launchers/<name>/bin/<name>` | An installed launcher, bind-mounted at `/opt/brewlet/launcher` in the sandbox |
| `launchers/.brewlet-active` | Newline-separated list of the currently inventoried launchers |
| `policy/appcds-regeneration-enabled` | Root-owned, mode `0444` sentinel authorizing node-side AppCDS regeneration (§13). The authoritative check — the node label is only a scheduling hint |
| `cds/` | Private per-`(namespace, manifest, JDK build, UID)` AppCDS cache. Overridable with `BREWLET_CDS_CACHE`. A workload receives only its own entry directory |
| `metrics/telemetry.sock` | Unix datagram socket the shim writes best-effort telemetry to |
| `<root>.retired.<epoch>.<pid>/` | A rotated-out JDK/launcher root, retained while live mounts may reference it (§12) |
| `.image-mount-*` | Transient source-image mount points; reclaimed on the next pass |

---

## 15. Glossary

- **OCI Artifact** — non-image content stored/distributed via an OCI registry using
  custom media types (OCI Image Spec ≥ 1.1).
- **containerd Runtime v2 shim** — pluggable per-runtime process that containerd
  talks to (TTRPC) to manage a container/task; the integration seam used by
  SpinKube through containerd-shim-spin/runwasi.
- **RuntimeClass** — Kubernetes object selecting which node runtime/handler executes
  a pod.
- **JDK runtime root** — a minimal, read-only Linux userland + JDK installed on the
  node and shared (overlayed) into every JVM sandbox.
