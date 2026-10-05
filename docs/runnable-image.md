# Runnable-image delivery (kubelet-pullable, the WASI-style pull path)

`brewlet push --format=image` publishes a kubelet-pullable OCI image that the Brewlet
shim runs with the node-resident JDK. This page documents the delivery contract referenced by
> [SPECIFICATION §4.4](https://github.com/microsoft/brewlet/blob/main/specs/SPECIFICATION.md#4-the-oci-application-artifact). It answers
> the question "how does a `runtimeClassName: brewlet` pod name the app as its `image:`
> and let kubelet pull it, exactly like a `runtimeClassName: wasmtime` pod names a
> Wasm module?"

---

## 1. TL;DR

Brewlet supports two current formats with the same launch contract and shared
sandbox assembly: native artifacts for local OCI-layout / CLI / `prepare-bundle`
workflows, and runnable images for Kubernetes container-runtime workloads.

- **The native artifact is registry-native but not unpackable by containerd.**
  Its custom layer media types (`application/vnd.brewlet.jar.layer.v1+jar`,
  `…classpath.layer.v1+tar`, `…modulepath.layer.v1+tar`) are not among the media types
  containerd's CRI differ can unpack (`tar`, `tar+gzip`, `tar+zstd`). A pod that sets
  `image: <native-artifact-ref>` therefore **fails to pull** (`ImagePullBackOff`).
  Kubernetes execution also rejects native artifacts if their blobs are delivered
  out of band. Brewlet resolves their raw blobs for local workflows, unpacking
  dependency/module tars during sandbox assembly rather than through containerd.
- **Runnable-image mode fixes this without changing the native format.** `brewlet push
  --format=image` publishes the *same* JAR as a **standard, kubelet-pullable OCI
  image**. containerd/kubelet pull + unpack it through the standard OCI path; the shim
  recognizes it and runs it on the node-resident JDK. Keep packed layers available
  until verified staging completes, as described below.
- **The developer experience becomes the WASI/SpinKube one:** `image: <ref>` +
  `runtimeClassName: brewlet` and nothing else. kubelet pulls, containerd unpacks, the
  shim launches `java -jar` under the pod's cgroup.
- **The Kubernetes reference must be digest-pinned.** The immutable
  `repo@sha256:…` target covers the manifest-level Brewlet launch configuration;
  tag-only requests are rejected before launch.
- **Runnable-image mode is now the default.** `brewlet push` (and `mvn brewlet:push`)
  produce a runnable image unless you opt into `--format=artifact`
  (`-Dbrewlet.format=artifact`). It is the delivery path that fulfils the WASI/SpinKube
  parity goal, so it is the out-of-the-box behaviour; the native artifact remains an
  opt-in choice for explicit local OCI-layout / CLI workflows.

---

## 2. Why the native artifact can't be a pod `image:`

containerd's CRI `PullImage` **unpacks** every layer into the snapshotter *before* the
runtime shim's `Create` runs. Unpacking dispatches on the layer's media type, and the
differ only understands `tar`, `tar+gzip`, and `tar+zstd`. Brewlet's native layers use
bespoke `+jar`/`+tar` media types so the artifact stays self-describing and
registry-native — but that is precisely what makes `crictl`/kubelet unable to unpack
them. The pull fails long before the shim is ever consulted.

Native artifacts are used for local OCI-layout / CLI / `prepare-bundle` workflows.
A Pod cannot *name* the native artifact as its image. Kubernetes workloads use the
runnable-image format so `runtimeClassName: brewlet` pods name the actual runnable image
in `image:`.

## 3. What `--format=image` publishes

```bash
# Same JAR and launch contract, packaged into a local runnable-image layout.
brewlet push ./target/app.jar demo/app:1.4.2 --store ./oci --format=image
```

The Go CLI writes the local OCI layout; it does not upload to a registry.
Use the Maven plugin's `push` goal for registry publication, as described in
[Building and publishing](building-and-publishing.md).

The result is an ordinary OCI **image index** (multi-arch) whose per-arch manifests are
plain OCI images:

| Piece | What it is |
|-------|-----------|
| Manifest media type | `application/vnd.oci.image.manifest.v1+json` |
| Index media type | `application/vnd.oci.image.index.v1+json` (multi-arch) |
| Config | `application/vnd.oci.image.config.v1+json` — a **real** image config whose `rootfs.diff_ids` are the sha256 of the **uncompressed** layer tars |
| Layers | `application/vnd.oci.image.layer.v1.tar+gzip` — standard, unpackable |
| Launch config | the §4.2 launch descriptor, carried verbatim in the manifest annotation `brewlet.sh/jvm-config` |
| Layer roles | each layer tagged with `brewlet.sh/layer` = `app` \| `classpath` \| `modulepath` |

Layer layout:

- **app layer** — a flat tar containing the main JAR (named per `mainJar`, which must
  be a bare filename and defaults to `app.jar` when omitted) plus an optional
  AppCDS `.jsa`.
- **classpath / modulepath layers** — the *same* flat-JAR tars a native artifact would
  ship for [layered classpath](layered-classpath-deployment.md) / [JPMS](jpms-support.md)
  deployments, just gzip-compressed and role-tagged.

**Multi-arch by default.** A portable bytecode JAR is published for `amd64` + `arm64`
(identical layers, per-arch config differing only in `architecture`) so any provisioned
node matches. A JAR carrying native libraries narrows this with `--arch amd64,arm64`
(see [multi-arch.md](multi-arch.md)).

> **OCI correctness note.** A layer descriptor's `digest` is the sha256 of the
> **gzipped** blob, but the image config's `rootfs.diff_ids[i]` must be the sha256 of
> the **uncompressed** tar. Getting this wrong makes containerd reject the image on
> unpack. The writer computes both; a unit test asserts the diff-ids equal the
> uncompressed digests.

## 4. How the shim runs it

On the node the shim distinguishes the two formats by the manifest: the presence of the
`brewlet.sh/jvm-config` annotation ⇒ runnable image (the Kubernetes workload path);
otherwise ⇒ native artifact, which remains the raw-blob path for local OCI-layout /
CLI / prepare-bundle workflows only. For a runnable image the shim:

1. follows the image index to the node's **platform** manifest (by `GOARCH`);
2. decodes the launch config from `brewlet.sh/jvm-config`;
3. recovers the JAR (and any `.jsa`) and classpath/modulepath tars into a
   manifest-specific immutable stage; extraction is published atomically only
   after it completes, and later resolutions reuse it without rewriting files
   already mounted by running workloads;
4. feeds those tars to the **existing** `StageClasspathLayers` / `StageModulepathLayers`
   bundle-assembly path — so runnable images and native artifacts converge on the same
   `java -jar` / `-cp` / `-p -m` sandbox on the node-resident JDK, under the pod's
   cgroup limits.

Nothing about JVM launch, cgroup-awareness, JDK/launcher selection, or Brewlet's
overlay rootfs (shared read-only JDK lower + per-container upper) changes.

Shims publish stages under an `immutable-v2` subdirectory of
`BREWLET_RUNNABLE_STAGE` (or the default temporary staging root). They leave unmanaged
stage layouts untouched because workloads may still mount those files. Allow
extra disk capacity: unmanaged layouts and pending directories left
by abruptly terminated processes are not removed by the stage reaper.
Do not remove staging trees while workloads still reference them.

The stage retains descriptor-verified packed layers as well as extracted
payloads. Every cache reuse verifies those retained bytes against the manifest
digests. This permits later replicas to start after containerd garbage-collects
its packed layers (`discard_unpacked_layers=true`) without bypassing blob
verification. A corrupt source blob that remains present still causes failure.
Missing or corrupt retained evidence fails closed. A cold launch whose packed
layers are already missing, or a node with only an unmanaged stage layout, requires a
verified image re-pull; this does not repair missing content from an unpacked
snapshot. Allow disk capacity for the retained compressed bytes per image.

### Reclaiming unused stages

Run the Linux node command as root, in the host PID, mount, and initial user
namespaces, with complete visibility of the host's `/proc` and access to the
containerd socket. On nodes installed by the provisioner, use its host helper:

```sh
sudo /usr/local/bin/brewlet-stage-gc stage-gc --dry-run
```

If you installed the standalone `brewlet` CLI on the host, use:

```sh
sudo brewlet stage-gc --dry-run
# Only on a safely established installation with no unguarded consumers:
sudo brewlet stage-gc --min-age 24h
# Non-default locations (use the same stage root as the shim):
sudo brewlet stage-gc --stage-root /var/lib/brewlet/runnable \
  --address /run/containerd/containerd.sock --min-age 48h
```

Manual commands do not enforce `stageGC.enabled`, node ownership, or the
provisioner's installation safety record. The reaper's
reference, mount, and locking checks still apply, but they cannot make
unguarded consumers safe. Retire unguarded launchers and exported bundles that
reference stage paths before cleanup. Do not use manual deletion or an additional
timer to bypass blocked automatic activation.

`--stage-root` defaults to `BREWLET_RUNNABLE_STAGE`, then
`/tmp/brewlet-runnable` on Linux (independent of `TMPDIR`, so it matches the
shim even when containerd sets `TMPDIR`) or `os.TempDir()/brewlet-runnable`
elsewhere. `--min-age` must be positive and defaults to
24 hours. Keep the stage root administrator-owned and do not rename or replace
it while launchers or cleanup are running. Only published
`immutable-v2/<manifest-hex>` directories older than
that floor are eligible. The command preserves stages whose manifest remains
in containerd image or content metadata in **any namespace**, including platform
manifests referenced by image indexes. Removing an image alone may not free its
stage until containerd also garbage-collects the manifest content.

The reaper checks live references across process mount namespaces, including
read-only bind mounts of individual staged files. It fails closed if it cannot
establish that reference information is complete. A shared staging guard covers
resolution and workload mount creation; cleanup takes the exclusive guard and
fails if a launch is in progress. Retry on the next scheduled run rather than
removing trees manually. Running workloads do not need to stop.

The Helm chart enables periodic cleanup by default on fresh nodes through the
existing provisioner, independently of `metrics.enabled`. The global settings
apply to every operator-managed NodeProfile, including externally managed CRs:

```yaml
stageGC:
  enabled: true
  interval: 5m
  minAge: 24h
```

The provisioner enters the host PID and mount namespaces for each sweep and
rechecks node/profile ownership before it runs. If the Kubernetes API cannot be
read, that sweep is skipped and retried; the worker exits only when a successful
read shows it no longer owns the node. Sweeps start after successful
provisioning, do not overlap, and wait the configured interval plus up to 10%
jitter between attempts. Each attempt has a five-minute context deadline;
filesystem operations already in progress may take longer to return. Lock contention
or inspection failures are logged and retried on the next interval; shutdown
signals cancel the active reaper. Logs include removed stage counts, logical
bytes, process-local success/failure counters, and the last successful sweep.
GC never runs in the provisioner's teardown mode. Disable it with
`--set stageGC.enabled=false`.

This follows kubelet's image-retention decisions rather than competing with
them: kubelet removes unused image records via CRI, containerd eventually
releases unreferenced content, and Brewlet reclaims the orphaned stages. Brewlet
does not delete containerd images, read or modify kubelet configuration, or apply
another set of disk-pressure thresholds. The `minAge` floor measures stage
directory age, **not** time since last use or since becoming unreferenced, and
is not kubelet's `imageMinimumGCAge` or `imageMaximumGCAge`. This is eventual
orphan reclamation, not guaranteed immediate disk-pressure relief.

### Existing installations and unguarded consumers

Pre-GA release changes require [safe teardown/reinstallation](installation.md#upgrading);
GC does not provide an in-place upgrade exception. Existing installations without
a matching safety record stay provisioned but log `stage GC blocked` instead of
deleting stages. There is no acknowledgment override. Installing a new shim does
not retire running unguarded shim processes or stage-dependent exported bundles.
Current `brewlet bundle` outputs retain their own payloads.

A fresh installation has no installed shim copies or safety record and an absent
or empty staging root. It establishes a root-owned
`/opt/brewlet/.stage-gc-compatible` record tied to both installed shim copies and
the staging path, even when GC is disabled. The name is retained, but the record
is installation safety evidence, not a cross-release compatibility promise.
It is invalidated before binary replacement and renewed only when safety was
established. A changed installed shim identity/path, missing record, or interrupted
installation blocks activation on the next provisioner startup.

Recover blocked nodes through safe teardown using the installed release's cleanup
path: retire unguarded consumers, finish their launches, and retire or regenerate
stage-dependent exported bundles. Review retained host files before reinstalling;
uninstall alone does not empty the staging root or prove those consumers are gone.
Use a replacement node if safety cannot be established. Do not forge a record,
rename staging roots, or delete in-use files to make an installation appear fresh.
Disable GC before introducing any consumer that does not participate in the guard;
that does not make mixed-version operation or rollback supported.
Earlier Linux shims derived their default stage root from `TMPDIR`. If
containerd set a non-default `TMPDIR`, stages created before the upgrade remain
under that old location; new launches use `/tmp/brewlet-runnable`. Automatic GC
does not sweep the old location. Retain it until reviewed host cleanup establishes
that no process, mount, pending launch, or exported bundle still depends on it.
For complete Helm commands and per-node activation checks, see
[Activating runnable-stage GC](installation.md#activating-runnable-stage-gc).

Standalone shim installation still does not schedule GC. Outside the provisioner,
use a host timer or root cron job, such as an hourly invocation of
`/usr/local/bin/brewlet stage-gc --min-age 24h`, only after establishing the same
consumer-safety prerequisites. Unmanaged layouts and abandoned pending trees
remain outside automated eviction.
External consumers that read stages without mounts must participate in the
staging guard or be stopped during cleanup; open file descriptors alone are
not tracked.

### Observing stage usage and cleanup

The metrics exporter exposes `brewlet_runnable_stage_bytes`, the logical size
of regular files remaining under its stage root (including unmanaged and pending
trees, without following symlinks). This is not filesystem-allocated space or
free disk space. It is refreshed on each scrape; inspection errors fail the
scrape rather than reporting a misleading zero. The operator's exporter mounts
the default host stage root read-only. For a custom staging location, configure
the exporter's `--stage-root` and its read-only host mount to point to that same
location. Alert on sustained growth and monitor node filesystem free space too.
Sweep success/failure counters and last-success timestamps are currently
provisioner logs, not Prometheus metrics. See the
[metric catalog](runtime-metrics.md#runtime-and-node-metrics).

## 5. Operator & webhook

- The `JavaApplication` controller sets the Deployment's container `image:` to
  `spec.artifact.image`. Supply a digest-pinned runnable-image reference so it is
  both pullable and immutable.
- The admission webhook overwrites `brewlet.sh/artifact-ref` + `brewlet.sh/artifact-digest`
  as compatibility hints from the selected Pod image; for a runnable image the digest
  is the **image-index** digest. The shim takes that exact target from
  containerd's protected CRI requested-image metadata, requires
  `io.kubernetes.cri.image-name` to name the same target, resolves the target
  directly from the content store, selects the platform manifest with
  containerd's strict OS/architecture/variant matcher (with no unmatched
  fallback), and verifies it against CRI's image-config digest. It never looks
  up a mutable image record by config digest, so manifests that share a config
  cannot substitute different launch metadata. This contract requires
  containerd 2.0 or newer; the provisioner rejects older servers before
  advertising node readiness.

## 6. When to use which

| | Runnable image (default) | Native artifact (`--format=artifact`) |
|--|--------------------------|----------------------------------------|
| Media types | standard OCI `tar+gzip` | custom `+jar` / `+tar` |
| Pod `image: <ref>` pulls via kubelet | ✓ | ✗ (rejected by the Kubernetes runtime path) |
| Registry-native / smallest | slightly larger (OS-image framing) | ✓ |
| Delivery | kubelet `PullImage`, like any image | local OCI layout / explicit tooling |
| Developer UX | pure WASI-style `image: <ref>` | local CLI / bundle workflow |

The default runnable image gives the pure `image: <ref>` experience end to end — the
WASI/SpinKube parity goal. Opt into `--format=artifact` only for local OCI-layout /
CLI / bundle workflows; native artifacts stay out of the Kubernetes pod path.

## 7. End-to-end behavior

The runnable-image tier of the [e2e suite](https://github.com/microsoft/brewlet/blob/main/integration-tests/README.md) provisions a real `kind`/CI node,
`brewlet push --format=image`s the demo JAR, imports it into the node's `k8s.io`
content store, and asserts **`ctr images unpack` SUCCEEDS** — the exact operation that
`ImagePullBackOff`s for a native artifact. It then runs a `runtimeClassName: brewlet`
Deployment whose container **`image:` is the digest-pinned Brewlet ref itself**
(`imagePullPolicy: Never`) and asserts the pod is Ready with that image, serves a
`200` from `/hello`, and that the JVM is cgroup-aware
(`availableProcessors == 1`, bounded `maxMemory`).
