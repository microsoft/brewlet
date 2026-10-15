# Brewlet live demo canvas

A project-shared Copilot canvas for presenting ShowMyJVM's **Spring Boot**
application, with a live terminal showing real commands and streamed stdout/stderr.
Ask Copilot to open the `brewlet-demo` canvas. Nothing executes merely by opening it.
The terminal is docked in the lower half of the viewport with an always-dark
background, even when Copilot uses a light theme. The guide and terminal output
scroll independently, keeping live output and command controls visible.
Phases appear in one column, in demo order.
A blue-and-teal presentation theme adds tinted surfaces, phase-specific accent
rails and buttons, and colored status badges. Accent colors adapt to the host's
light/dark mode; the live terminal remains dark in either mode.
Every phase, including prerequisite checks, Docker Desktop preparation and
inspection, has a **Show commands & inputs** button. It expands a read-only
preview of the key commands and JSON configuration without executing anything.
Resolved session paths and image digests are shown when available; values that
will only be known during execution are explicitly marked as placeholders.
Previews remain expanded while live output updates, and the terminal stays visible.

## Presenting the JavaApplication YAML

The deployment input lives in [`javaapplication.yaml`](javaapplication.yaml),
a **JavaApplication custom resource** (not the cluster-wide CRD schema).
Click **Show JavaApplication YAML** to present it in large, readable text inside
the guide while the dark terminal remains docked below. **Source template**
shows the repository file; **Deployment YAML** shows a preview until applied,
then the captured exact stage 4 input with the session namespace and packaged
digest substituted. Before packaging, the unresolved image is explicitly marked
as a preview. The rendered file is
saved to `<session.workspacePath>/brewlet-demo/javaapplication.yaml` and passed
to `kubectl apply -f`. The viewer does not execute commands. Its state survives
live output updates. Edits to the source file are loaded on extension reload
and again when stage 4 runs.

The resource is named `showmyjvm-java`. The Brewlet operator owns its Deployment
and Service; stages 5/6 patch `spec.jvm.version` on the JavaApplication, never
its generated Deployment. They leave the initial YAML on disk at version 25.
Readiness waits for the **current generation**, not a stale Ready condition.
Existing raw-demo `showmyjvm` resources are retained, not silently adopted or
deleted. After updating an old demo, rerun stage 4 to establish managed-resource
proof; the already-built OCI artifact is reused.

**Same artifact, different runtime** compares captured 25 -> 27 -> 25 rollout
evidence: the live JVM, serving ready Pod, application OCI index, platform
manifest, and runtime-reported imageID. Verification follows controller-owner
UIDs from JavaApplication to Deployment to ReplicaSet to Pod, checks all owned ready Pods, and
matches the Service response's hostname to a verified Pod. It hashes local OCI
index/manifest/config content and links each reported imageID to that descriptor
chain. Containerd may report the enclosing import index, rather than the
application index; these digests are labeled separately, not falsely compared
as equal. The application index and selected platform manifest/config must match
the initial verified rollout. The panel displays **Artifact unchanged** only
after verified evidence for both JDKs exists. Older saved evidence remains
unverified until its phase is rerun; inspection never invents historical proof.

After stage 4, click **Open live app** in the always-visible terminal toolbar.
The real ShowMyJVM dashboard opens in a browser tab with automatic refresh every
two seconds. The button requests the operating system's default browser directly,
rather than depending on new-window support inside the canvas iframe.
Keep it open while running stage 5: its capability-protected loopback proxy routes
through the Kubernetes API's **Service proxy** for the `showmyjvm-java` ClusterIP Service.
Every request resolves the Service's ready endpoints; neither Deployment nor
Service `port-forward` is used, since both pin the connection to a single Pod.
The browser URL and Service proxy stay running across the JDK switch.
A brief reconnecting message during replacement is expected.
After extension reload or Stop, use **Reconnect app** to restore Service access
without redeploying. Extension reload changes the proxy address;
reopen the browser tab from the refreshed canvas in that case.

Run the six stages in order:

1. Build the pinned [ShowMyJVM revision](https://github.com/brunoborges/showmyjvm/tree/3bab16bf108bb9d2cdf0268fd22213870ae37408)
   using local **Temurin 25**, Maven's `-pl spring-boot -am`, and `-Djib.skip=true`.
   Run the executable Spring Boot JAR on a dynamically assigned loopback port.
2. **Publish bundle & compose OCI image**: resolve the pinned sample's actual
   Maven runtime graph with `maven-dependency-plugin:3.8.1:list`, preserving
   coordinates, classifiers, effective scopes and SHA-256 file hashes in a lock.
   Create a thin JAR from the Spring Boot module's `target/classes` (the local
   demo still uses its executable fat JAR). Create a flat dependency tar, then
   publish it locally with `brewlet dependency-bundle`, using the imported
   `org.springframework.boot:spring-boot-dependencies:4.1.1` BOM as its source.
   Compose the runnable image with `brewlet push --dependency-bundle ...
   --dependency-lock ... --format image`. Brewlet verifies the exact graph,
   rejects fat JARs in managed mode, and reuses the bundle's classpath layer.
   The launch contract is `classpath` mode with the application main class and
   `app.jar:lib/*`. No bundled JDK, registry upload, or runtime Maven resolution.
   The demo acts as both publisher and consumer of an **unsigned local bundle**;
   it does not claim signed approval or a production governance policy.
3. **Inspect the OCI artifact** (`artifact`): run `brewlet inspect`, verify the
   index, platform manifests, configs and layer SHA-256 digests, then list the
   contents of every unique layer with `tar -tvf`. The phase card presents
   an interactive artifact explorer: platform cards, a selectable card for each
   actual layer with size bars and shared-layer badges, and a searchable archive
   file browser. Select a platform to see its layer order; select a layer to
   browse its paths, inspect full digests, or expand the complete raw listing.
   No decorative cards imply additional layers: an artifact with one shared
   layer has just one selectable layer. Unique compressed sizes count shared
   blobs once. The file browser shows up to
   200 matching entries at a time with an explicit count; filter to narrow, or
   use the complete raw listing. Selection, filters and open details survive live
   updates. Older inspection results retain their raw listing; rerun phase 3 to
   populate the file browser. Separate purple controls expose the JVM launch
   annotation and OCI config JSON, neither of which is a filesystem layer.
   **Managed dependency bundle** shows the source BOM, bundle/layer/lock/SBOM
   digests, and searchable locked Maven coordinates and file hashes. The
   application now has real, separately selectable `app` and `classpath` layers.
   Inspection verifies the bundle config/lock blob hashes and checks the
   managed evidence points at the reused dependency layer. Changing JDK versions
   changes neither layer. Updating a bundle requires recomposing the final
   application image; Kubernetes still references only that final image.
   Legacy fat-JAR artifacts instead show **Spring Boot dependencies**, listing
   JARs in `app.jar!/BOOT-INF/lib/` rather than inventing separate layers.
   Before reading this nested listing, inspection streams the packaged `app.jar`
   to SHA-256 and requires the local build JAR to match exactly. It then uses
   the JDK's `jar --list` command without unpacking files. A changed local JAR
   fails explicitly and requires repackaging. Rerun phase 2, then phase 3, to
   migrate an older demo to managed bundles. Repackaging invalidates deployment
   and runtime-comparison badges, so rerun phases 4-6 for the new artifact.
   Nothing is extracted, rebuilt or deployed. This read-only phase
   requires packaging but is optional before deployment; rebuilding/repackaging
   clears its saved results.
4. Import the OCI archive through ready Brewlet provisioners into compatible
   workers' `k8s.io` containerd namespaces. Apply `javaapplication.yaml` with a
   digest-pinned artifact, `pullPolicy: Never`, `jvm.distribution: temurin`, and
   `jvm.version: 25`. The operator creates the Deployment and Service.
5. Merge-patch **JavaApplication.spec.jvm.version** to `27`,
   wait for rollout through the same Service, and verify both the actual JVM
   vendor/version and the unchanged application-image digest. No rebuild.
6. **Switch back to JDK 25** (`rollback`): merge-patch the same field to
   `25`, wait for rollout, and verify Temurin 25 and the unchanged digest.
   Keep the browser open: the same Service URL automatically refreshes through
   both switches. Run stage 5 again to repeat the 25-to-27-to-25 cycle.

The runtime proof uses `/jvm/inspect.json`, but retains only JVM identity fields:
the application's environment/system-properties dump is not stored. The local
JVM gets a minimal environment and binds only to `127.0.0.1`. The terminal command
line accepts guided stage names, not arbitrary shell commands.
Both local and OCI launch configurations open the management package needed by
ShowMyJVM's flag inspector; its per-flag INFO logging is suppressed so the live
terminal stays focused on the demo.

## Prerequisites and explicit preparation

Use macOS or Linux with Node.js (provided by Copilot), Git, Maven, Temurin 25,
the released Brewlet CLI, Docker with Buildx, Helm, `kubectl`, and `tar`. On macOS,
the runner finds JDK 25 through `/usr/libexec/java_home`; elsewhere set `JAVA_HOME`
to Temurin 25 **before loading the extension**. Tool executables must be on the
Copilot process's `PATH`.

Docker Desktop must expose the `docker-desktop` Kubernetes context, using a local
API endpoint and a Ready, schedulable **worker** (the kind engine). The canvas
never switches the user's context and never provisions control-plane nodes.
Every demo requires a worker providing **both** `temurin-25` and `temurin-27`.
Readiness uses the `brewlet.sh/runtime` node label and the compact
`brewlet.sh/jdks` **annotation**, matching Brewlet's published inventory contract.
JDK 27 availability is checked against the real Temurin image; there is no
silent substitution of JDK 26, another vendor, or another runtime.

**Check prerequisites** is read-only. **Prepare Docker Desktop** is a separate,
confirmed opt-in: if there is no Helm release in the `brewlet` namespace, it
installs the chart matching the released Brewlet CLI, with digest-resolved
Temurin 25/27 source images, restricted to one worker's hostname. Provisioning is
privileged and may restart that worker's containerd. Use a development cluster.
Release discovery uses explicit status filters compatible with Helm 3 and 4,
including failed, pending, and uninstalling releases (not the removed `--all` flag).
If a release exists, it is retained without upgrade; provision both JDKs through
your normal administration process if needed. Network access to GitHub, Maven
Central, Docker Hub and GHCR is needed; normal local Maven caches are used.

## Lifetime and cleanup

Commands, OCI layout/archive, manifests, terminal log, and state are stored under
`<session.workspacePath>/brewlet-demo/`, not in this repository. State is scoped
to the session, not the panel ID, and survives panel/extension reloads. Interrupted
steps are marked interrupted; local processes are not silently restarted.
Rebuilding/repackaging invalidates downstream completion badges.

**Stop processes** cancels the current command and stops only this demo's Java
process and loopback Service proxy. Closing a panel leaves them running; unloading the
extension stops them. The Kubernetes workload keeps running. There is no
automatic deletion, Helm uninstall, cluster reset, or global Docker cleanup.

The session-specific namespace is displayed in the footer. To remove its
workload when finished, inspect that exact namespace and its
`brewlet-demo-owner` label, then explicitly delete it:

```sh
kubectl --context docker-desktop get namespace <displayed-namespace> --show-labels
kubectl --context docker-desktop delete namespace <displayed-namespace>
```

The Brewlet installation and shared JDKs are intentionally retained. Follow
[Brewlet's uninstall guidance](../../../docs/installation.md) separately if you
want to remove the runtime; do not delete node runtime files manually.

## Development

No npm dependencies, CDN assets, SDK package install, or unrestricted shell.
The server binds to loopback, uses per-panel capability tokens, checks Host and
Origin for mutations, limits request bodies, and renders log data with text nodes.

```sh
node --test .github/extensions/brewlet-demo/*.test.mjs
```

Set `BREWLET_DEMO_CHROME` to an installed Chrome/Chromium executable to also run
the headless layout regression test across desktop, narrow and short viewports,
in both light and dark themes.

After editing, reload extensions and inspect `brewlet-demo` before reopening.
