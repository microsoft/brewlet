# Brewlet live demo canvas

A project-shared Copilot canvas for presenting ShowMyJVM's **Spring Boot**
application, with a live terminal showing real commands and streamed stdout/stderr.
Ask Copilot to open the `brewlet-demo` canvas. Nothing executes merely by opening it.
The terminal is docked in the lower half of the viewport with an always-dark
background, even when Copilot uses a light theme. The guide and terminal output
scroll independently, keeping live output and command controls visible.
Every phase, including prerequisite checks, Docker Desktop preparation and
inspection, has a **Show commands & inputs** button. It expands a read-only
preview of the key commands and JSON configuration without executing anything.
Resolved session paths and image digests are shown when available; values that
will only be known during execution are explicitly marked as placeholders.
Previews remain expanded while live output updates, and the terminal stays visible.

**Same artifact, different runtime** compares captured 25 -> 27 -> 25 rollout
evidence: the live JVM, serving ready Pod, application OCI index, platform
manifest, and runtime-reported imageID. Verification follows controller-owner
UIDs from Deployment to ReplicaSet to Pod, checks all owned ready Pods, and
matches the Service response's hostname to a verified Pod. It hashes local OCI
index/manifest/config content and links each reported imageID to that descriptor
chain. Containerd may report the enclosing import index, rather than the
application index; these digests are labeled separately, not falsely compared
as equal. The application index and selected platform manifest/config must match
the initial verified rollout. The panel displays **Artifact unchanged** only
after verified evidence for both JDKs exists. Older saved evidence remains
unverified until its phase is rerun; inspection never invents historical proof.

After stage 3, click **Open live app** in the always-visible terminal toolbar.
The real ShowMyJVM dashboard opens in a browser tab with automatic refresh every
two seconds. The button requests the operating system's default browser directly,
rather than depending on new-window support inside the canvas iframe.
Keep it open while running stage 4: its capability-protected loopback proxy routes
through the Kubernetes API's **Service proxy** for the `showmyjvm` ClusterIP Service.
Every request resolves the Service's ready endpoints; neither Deployment nor
Service `port-forward` is used, since both pin the connection to a single Pod.
The browser URL and Service proxy stay running across the JDK switch.
A brief reconnecting message during replacement is expected.
After extension reload or Stop, use **Reconnect app** to restore Service access
without redeploying. Extension reload changes the proxy address;
reopen the browser tab from the refreshed canvas in that case.

Run the five stages in order:

1. Build the pinned [ShowMyJVM revision](https://github.com/brunoborges/showmyjvm/tree/3bab16bf108bb9d2cdf0268fd22213870ae37408)
   using local **Temurin 25**, Maven's `-pl spring-boot -am`, and `-Djib.skip=true`.
   Run the executable Spring Boot JAR on a dynamically assigned loopback port.
2. Use `brewlet push --format image --store ...` to package the JAR as a
   Kubernetes-runnable OCI image, without a bundled JDK or registry upload.
3. Import the OCI archive through the existing Brewlet provisioner's `ctr` into
   the selected Docker Desktop worker's `k8s.io` containerd namespace. Deploy a
   raw Deployment with `runtimeClassName: brewlet`, a digest-pinned image,
   `imagePullPolicy: Never`, and `brewlet.sh/jdk: temurin-25`.
4. Merge-patch the Deployment's **pod-template annotation** to `temurin-27`,
   wait for rollout through the same Service, and verify both the actual JVM
   vendor/version and the unchanged application-image digest. No rebuild.
5. **Switch back to JDK 25** (`rollback`): merge-patch the same annotation to
   `temurin-25`, wait for rollout, and verify Temurin 25 and the unchanged digest.
   Keep the browser open: the same Service URL automatically refreshes through
   both switches. Run stage 4 again to repeat the 25-to-27-to-25 cycle.

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
