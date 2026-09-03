# Runnable-image delivery (kubelet-pullable, the WASI-style pull path)

`brewlet push --format=image` publishes a kubelet-pullable OCI image that the Brewlet
shim runs with the node-resident JDK. This page documents the delivery contract referenced by
> [SPECIFICATION §4.4](https://github.com/microsoft/brewlet/blob/main/specs/SPECIFICATION.md#4-the-oci-application-artifact). It answers
> the question "how does a `runtimeClassName: brewlet` pod name the app as its `image:`
> and let kubelet pull it, exactly like a `runtimeClassName: wasmtime` pod names a
> Wasm module?"

---

## 1. TL;DR

- **The native Brewlet artifact is registry-native but not runnable by containerd.**
  Its custom layer media types (`application/vnd.brewlet.jar.layer.v1+jar`,
  `…classpath.layer.v1+tar`, `…modulepath.layer.v1+tar`) are not among the media types
  containerd's CRI differ can unpack (`tar`, `tar+gzip`, `tar+zstd`). A pod that sets
  `image: <native-artifact-ref>` therefore **fails to pull** (`ImagePullBackOff`); the
  payload has to reach the node **out of band**, such as with `ctr images import`.
- **Runnable-image mode fixes this without changing the native format.** `brewlet push
  --format=image` publishes the *same* JAR as a **standard, kubelet-pullable OCI
  image**. containerd/kubelet pull + unpack it with no special configuration; the shim
  recognizes it and runs it on the node-resident JDK.
- **The developer experience becomes the WASI/SpinKube one:** `image: <ref>` +
  `runtimeClassName: brewlet` and nothing else. kubelet pulls, containerd unpacks, the
  shim launches `java -jar` under the pod's cgroup.
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

That remains useful for explicit local OCI-layout / CLI workflows, but a Pod
cannot *name* the native artifact as its image. Kubernetes workloads use the
runnable-image format so `runtimeClassName: brewlet` pods name the actual runnable image
in `image:`.

## 3. What `--format=image` publishes

```bash
# Same JAR, same launch contract — published as a runnable OCI image.
brewlet push ./target/app.jar registry.example.com/team/app:1.4.2 --format=image
```

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

- **app layer** — a flat tar containing the main JAR (named per `mainJar`) plus an
  optional AppCDS `.jsa`.
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
3. gunzips the app layer to recover the JAR (and any `.jsa`), and gunzips each
   classpath/modulepath layer to a temporary tar;
4. feeds those tars to the **existing** `StageClasspathLayers` / `StageModulepathLayers`
   bundle-assembly path — so runnable images and native artifacts converge on the same
   `java -jar` / `-cp` / `-p -m` sandbox on the node-resident JDK, under the pod's
   cgroup limits.

Nothing about JVM launch, cgroup-awareness, JDK/launcher selection, or Brewlet's
overlay rootfs (shared read-only JDK lower + per-container upper) changes.

## 5. Operator & webhook: no change required

- The `JavaApplication` controller already sets the Deployment's container
  `image:` to `spec.artifact.image`. With a runnable image that ref is now **pullable**,
  so the happy path just works.
- The admission webhook overwrites `brewlet.sh/artifact-ref` + `brewlet.sh/artifact-digest`
  as compatibility hints from the selected Pod image; for a runnable image the digest
  is the **image-index** digest, and the shim resolves the executable image target
  digest through CRI's immutable image-config record. For digest-pinned Pods it
  also requires containerd's protected `io.kubernetes.cri.image-name` annotation
  to name that same target, so conflicting tenant hints do not change what runs.

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
Deployment whose container **`image:` is the brewlet ref itself** (no placeholder,
`imagePullPolicy: Never`) and asserts the pod is Ready with that image, serves a `200`
from `/hello`, and that the JVM is cgroup-aware (`availableProcessors == 1`, bounded
`maxMemory`).
