# Brewlet live demo canvas

A [GitHub Copilot app canvas extension](https://docs.github.com/copilot) that runs
the "One app. Two JDKs. No image rebuild." demonstration against Brewlet 1.0
built from this checkout. It is a contributor/presenter tool, not part of the
product.

The application travels as an OCI image containing only ShowMyJVM's dependency
JARs, its application JAR and `launch.json`. The Java runtime is node
infrastructure: an approved RuntimeImage offered by a NodeProfile. Switching the
`brewlet.sh/runtime-image` annotation on an ordinary Deployment rolls Pods onto
Temurin 27 and back to Temurin 25 with the exact same application digest.

## Stages

| Stage | What runs |
| --- | --- |
| Check prerequisites | Read-only: Docker (Linux engine), kind `v0.33.0`, kubectl, Helm, Go, Git, Maven and a local JDK 25. |
| Prepare private cluster | Builds the control-plane and node-provisioner images from `git archive HEAD`, imports the pinned Temurin 25/27 roots, creates a session-owned kind cluster with a private kubeconfig, installs the chart, and waits for RuntimeImages `Approved` and NodeProfile `Ready`. |
| 1. Build & run locally | Clones [ShowMyJVM](https://github.com/brunoborges/showmyjvm) at the latest commit of its default branch (fetched on every run; the resolved commit is recorded), builds the Spring Boot module and serves it on loopback. |
| 2. Package app-only OCI image | `java -Djarmode=tools … extract`, then [`producer.Dockerfile`](producer.Dockerfile) builds `brewlet` from this checkout and runs `brewlet build` with `dependency`, `app` and `launch` layers. |
| 3. Inspect the OCI artifact | Hash-verifies manifest, config and layers, lists every archive entry, and fails if any layer contains `bin/java` or `libjvm.so`. |
| 4. Deploy on temurin-25 | Imports the layout into the node's containerd, applies [`deployment.yaml`](deployment.yaml), and verifies the live JVM, Pod ownership, imageID digest and the node's `brewlet.sh/runtime-bindings` observation. |
| 5. Switch to temurin-27 | JSON-patches only the runtime-image annotation (guarded by `test` operations) and re-verifies. |
| 6. Switch back to temurin-25 | Patches the annotation back and re-verifies. |
| Inspect deployment | Shows workload, RuntimeImage and NodeProfile state, logs, and re-verifies the binding. |
| Remove private cluster | Deletes the namespace, NodeProfile (waiting for host cleanup), RuntimeImages and Helm release, then the kind cluster and component images. |

The runtime pins live in [`runtimes.json`](runtimes.json). Update the index and
per-architecture platform-manifest digests together.

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
