# Brewlet live demo canvas

A [GitHub Copilot app canvas extension](https://docs.github.com/copilot) that runs
the "One app. One runtime ID. No image rebuild." demonstration against the latest
Brewlet release, or optionally against Brewlet built from this checkout. It is a
contributor/presenter tool, not part of the product.

The application travels as an OCI image containing only ShowMyJVM's dependency
JARs, its application JAR and `launch.json`. The Java runtime is node
infrastructure: the approved `temurin-25` RuntimeImage offered by a NodeProfile.
The demo rotates `spec.source.image` on that same RuntimeImage from Temurin
**25.0.4.1+1 to 25.0.5+8 and back**; the Deployment keeps selecting
`temurin-25` throughout, and the application digest stays the same.
The canvas captures the bundled `cacerts` from ShowMyJVM's live
`/jvm/inspect.json` response alongside the JVM version and Pod identity.
**emSign ECC Root CA - G3, emSign Root CA - C1 and emSign ECC Root CA - C3**
are present before the update, absent afterward, and return on rollback.
**emSign Root CA - G1 remains present throughout.** This is a root-store
comparison, not a TLS exploit test or a claim that every emSign root was removed.
The older runtime and rollback are for demonstration only.
The chart enables Brewlet's rotation-restart controller, and the Deployment
opts in with `brewlet.sh/restart-on-rotation: "true"`. Each rollout follows node
convergence of the new runtime generation.

The removal landed in [adoptium/temurin-build#4587](https://github.com/adoptium/temurin-build/pull/4587)
on October 21, 2026. The explicit **Platform team: build the golden JDK image**
stage (`golden`) shows a hypothetical platform team's build pipeline independently
of application packaging. The canvas displays pinned inputs, the actual
[`runtime.Dockerfile`](runtime.Dockerfile), verification source, commands,
build-time root evidence and the resulting immutable runtime digest.
It builds a **private demo golden runtime**, not
an official Temurin container: it keeps the pinned older container's Alpine OS,
replaces `/opt/java/openjdk` entirely with the official full JDK 25.0.5+8 musl archive,
and preserves that archive's bundled `cacerts` unchanged. The archive SHA-256
is pinned per architecture in `runtimes.json` and checked before building.
The Dockerfile runs [`VerifyRuntime.java`](VerifyRuntime.java) against the new
JDK. It fails unless the exact vendor/build and JDK compiler are present,
G3/C1/C3 are absent from bundled `cacerts`, and G1 remains present. The updated
archive already omits the distrusted roots; no manual certificate deletion is
performed. Buildx exports the measured evidence from the same runtime stage.
The runner validates it before recording a successful golden build.

Buildx exports an OCI layout; the runner verifies its manifest/config hashes
and platform, then imports the golden digest into the private node for rotation.
Nothing is uploaded to a registry. Do not compare the two builds by major
version alone: both are Java 25. In a real platform pipeline, the team would
publish, sign, qualify and approve its runtime digest in its registry. This demo
does not perform those production steps or claim that the image is hardened.
Cluster preparation requires the golden build and consumes its exact digest
without rebuilding it. Rebuilding is refused while the demo cluster exists.

## Brewlet source

The preparation panel (or the `set_brewlet_source` canvas action) chooses where
Brewlet comes from. Switch only while no demo cluster exists; switching resets
prerequisites, preparation and packaging.

| Source | Cluster | `brewlet` CLI used for packaging |
| --- | --- | --- |
| Latest release (default) | `helm upgrade --install brewlet oci://ghcr.io/microsoft/charts/brewlet --version X.Y.Z` with rotation restarts enabled; the node pulls the initial Temurin runtime, and the private updated digest is pre-imported for same-ID rotation. | `brewlet_X.Y.Z_linux_<arch>.tar.gz` from the GitHub release, verified against `checksums.txt`. |
| This checkout | Control-plane and node-provisioner images built from `git archive HEAD`, both runtime layouts pre-imported, local chart and CRDs, with rotation restarts enabled. Requires Go. | Built from `git archive HEAD` inside the producer. |

"Latest" is resolved from `https://github.com/microsoft/brewlet/releases/latest`
when prerequisites are checked without a cluster, then kept for the session so
the cluster and CLI match. Set `BREWLET_DEMO_VERSION=X.Y.Z` to pin a release.

## Stages

| Stage | What runs |
| --- | --- |
| Check prerequisites | Read-only: Docker (Linux engine) with Buildx, kind `v0.33.0`, kubectl, Helm, Git, Maven, a local JDK 25, and Go (checkout source only). Resolves the release to install. |
| Platform team: build the golden JDK image | Downloads and checksum-verifies the official full JDK archive, builds and checks the golden runtime, exports OCI layout and measured trust-store evidence, and records the image digest. Independent of application packaging and Brewlet source. |
| Prepare private cluster | Requires the golden build; makes both runtime digests available on the node, creates a session-owned kind cluster with a private kubeconfig, installs Brewlet with rotation restarts enabled, applies the initial `temurin-25` RuntimeImage and a NodeProfile offering that one ID, and waits for `Approved` and `Ready`. |
| 1. Build & run locally | Clones [ShowMyJVM](https://github.com/brunoborges/showmyjvm) at the latest commit of its default branch (fetched on every run; the resolved commit is recorded), builds the Spring Boot module and serves it on loopback. |
| 2. Package app-only OCI image | `java -Djarmode=tools … extract`, then [`producer.Dockerfile`](producer.Dockerfile) obtains the released `brewlet` CLI (or builds it from this checkout) and runs `brewlet build` with `dependency`, `app` and `launch` layers. |
| 3. Inspect the OCI artifact | Hash-verifies manifest, config and layers, lists every archive entry, and fails if any layer contains `bin/java` or `libjvm.so`. |
| 4. Deploy on Temurin 25.0.4.1+1 (`temurin-25`) | Imports the app layout into the node's containerd, applies [`deployment.yaml`](deployment.yaml), and verifies the exact live JVM build, Pod ownership, imageID digest, node binding and four bundled emSign roots with SHA-256 fingerprints. |
| 5. Rotate `temurin-25` to Temurin 25.0.5+8 | JSON-patches `spec.source.image` on the existing RuntimeImage (guarded by UID/source tests); waits for `RotationComplete` and the operator's generation barrier to roll the opted-in Deployment; verifies the same app digest, exact JVM build and measured roots. |
| 6. Roll back `temurin-25` to Temurin 25.0.4.1+1 | Patches the same RuntimeImage back to its original digest, producing another generation and normal workload rollout; captures the roots returning with the older runtime. |
| Inspect deployment | Shows workload, RuntimeImage and NodeProfile state, logs, and re-verifies the binding. |
| Remove private cluster | Deletes the namespace, NodeProfile (waiting for host cleanup), RuntimeImages and Helm release, then the kind cluster (and, for the checkout source, the component images). |

The runtime build pins live in [`runtimes.json`](runtimes.json); they are two source
digests for one runtime ID, not two RuntimeImages. Keep the older runtime's
index and per-architecture platform-manifest digests together; update the private
runtime's official archive URL, SHA-256, Dockerfile extraction path, exact JVM
version and expected roots together. Missing/empty certificate data, a wrong
patch build or unexpected root presence fails the rollout stage rather than
displaying an inferred success. Existing 25-to-27 demo clusters must be removed
and prepared again; their historical rollout evidence is invalidated on reload.
Clusters from the former two-runtime-ID flow must also be removed before running
this same-ID rotation demo.

## Safety

- All cluster commands use `--kubeconfig <session>/brewlet-demo/kubeconfig`
  and the `kind-brewlet-demo-<session>` context. Your current context is never
  read or changed.
- Setup refuses to adopt a pre-existing cluster with the demo's name, and node
  commands verify the kind node container ID, cluster label and Node UID.
- Cleanup is explicit and confirmed in the UI. It never touches other clusters.
- The panel server binds to `127.0.0.1` with a per-panel token, enforces
  same-origin JSON mutations and serves a strict CSP. The guided command box is
  not a shell.
- State, terminal output and artifacts are kept in the session workspace under
  `brewlet-demo/`. Progress from the 0.x JavaApplication demo is discarded.

Set `BREWLET_DEMO_JAVA_HOME` to choose the local JDK 25. Otherwise the runner
uses `JAVA_HOME`, `/usr/libexec/java_home -v 25` on macOS, then SDKMAN,
preferring Temurin.

## Tests

```bash
node --test .github/extensions/brewlet-demo/*.test.mjs
```
