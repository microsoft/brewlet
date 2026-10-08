# Building & publishing application artifacts

This is the developer's half of Brewlet: turn your Java application into an **OCI
application artifact** and push it to a registry. There is no Dockerfile and no image
build — you ship the application payload (a fat JAR, or dependency + app classpath
layers) plus a small JSON launch config.

- Ops/cluster side: [Installation](installation.md) and [JDK management](jdk-management.md).
- Deploying what you publish here: [Deploying workloads](deploying-workloads.md).

---

## 1. Build your fat JAR (nothing Brewlet-specific)

Build a self-executable (fat/uber) JAR exactly as you do today:

```bash
mvn -q clean package          # → target/app.jar
# or
gradle bootJar                # → build/libs/app.jar
```

Brewlet runs it with the canonical `java -jar app.jar`, so Spring Boot, Quarkus,
Micronaut, JMX, OTel, JFR, and shutdown hooks all behave exactly as they normally
would.

---

## 2. The launch config

Every artifact carries a small JSON **launch config** describing how to run the JAR.
It is deployment-agnostic: the JDK feature and launcher are requested later in the
deployment descriptor. The `brewlet push` CLI generates a minimal config for you, or
you can author it and pass `--config`.

```json
{
  "schemaVersion": 1,
  "mainJar": "app.jar",
  "entry": { "mode": "jar" },
  "enablePreview": true,
  "addOpens": ["java.base/java.lang=ALL-UNNAMED"],
  "systemProperties": { "spring.aot.enabled": "true" },
  "env": []
}
```

| Field | Meaning |
|---|---|
| `mainJar` | The JAR filename inside the artifact (mounted at `/app/<mainJar>`). Defaults to `app.jar` when omitted, regardless of the source JAR's filename. Must be a bare filename: separators, wildcards and parent references are rejected at publish and load time, since the name is resolved against the node's staging directory and bind-mounted from there. |
| `entry.mode` | `jar` → `java -jar` (default); `classpath` → `java -cp <jar> <mainClass>`; `module` → `java -p <modulePath> -m <module>[/<mainClass>]` (JPMS). |
| `entry.mainClass` | Required when `entry.mode == "classpath"`; optional in `module` mode. |
| `entry.classPath` | Optional, ordered `/app`-relative class-path entries (e.g. `["app.jar", "lib/*"]`) used with `entry.mode == "classpath"` for layered deployment. |
| `entry.module` | Required when `entry.mode == "module"`; the root module name for `java -m`. |
| `entry.modulePath` | Optional, ordered `/app`-relative module-path entries (e.g. `["orders.jar", "mods"]`) fed to `java -p` in `module` mode; defaults to `mainJar`. |
| `enablePreview` | Optional app-intrinsic flag for code compiled with preview features; expands to `--enable-preview`. |
| `addModules` | Optional module names; expands to `--add-modules <comma-joined>`. |
| `addOpens` | Optional module/package access tokens such as `java.base/java.lang=ALL-UNNAMED`; expands to repeated `--add-opens`. |
| `addExports` | Optional module/package export tokens; expands to repeated `--add-exports`. |
| `systemProperties` | Optional string map expanded as sorted `-D<key>=<value>` flags. |
| `cds` | Optional AppCDS block. `cds.archive` is a bare `/app`-relative `.jsa` filename shipped as a CDS layer (`brewlet push --appcds-archive`); `cds.mode` (`dynamic`\|`static`) is informational. Launches with `-Xshare:auto -XX:SharedArchiveFile=/app/<archive>`, so a JDK-build mismatch falls back safely to base CDS. The artifact carries only this shipped *seed* archive; node-side regeneration is a deployment choice set via `spec.jvm.cds.regenerate` on the `JavaApplication` CRD (or `brewlet run/bundle --appcds-regenerate`), not a field in the artifact. See [AppCDS](appcds.md). |
| `aot` | Optional JDK AOT cache block. `aot.cache` is a bare `/app`-relative filename shipped as an AOT layer (`brewlet push --aot-cache` or `--aot`). Launches with `-XX:AOTCache=/app/<cache>` on JDK 24+ and is dropped on JDKs older than 24 or when the JDK identity is unreadable. May ship next to `cds` under a different filename: JDK 24+ uses the AOT cache, older JDKs the `.jsa`. See [JDK AOT cache](aot-cache.md). |
| `env` | Environment variables baked into the artifact. |

Ports are **not** an artifact field — they are a deployment concern
(`spec.ports` in the deployment descriptor).
Process UID/GID is also a deployment concern: use Pod `securityContext` on
Kubernetes or `brewlet bundle --uid/--gid` for a standalone OCI bundle. A
launch config containing `user` is rejected.

Artifact launch knobs are app-intrinsic correctness flags. They expand before
descriptor `jvm.args`, which is where deployment tuning and escape-hatch JVM args
belong.

The JDK and launcher are set once in the deployment descriptor: `spec.jvm.version`
and `spec.jvm.launcher` for `JavaApplication`, or pod annotations
`brewlet.sh/jdk` and `brewlet.sh/launcher` for raw Deployments.

> **Tuning is yours, not Brewlet's.** The container-aware JVM reads the sandbox
> cgroup limits directly; set heap/GC in descriptor `jvm.args` (or let `jaz` derive
> them). The artifact only carries app-intrinsic launch knobs. See [Resource requests, limits & JVM tuning](resource-tuning.md).

### Class-path / non-`-jar` entry

For an app launched via a main class instead of `-jar`:

```json
{ "schemaVersion": 1, "mainJar": "app.jar",
  "entry": { "mode": "classpath", "mainClass": "com.example.Main" } }
```

This produces `java -cp /app/app.jar com.example.Main`.

### Modular (JPMS) apps

A modular JAR — an ordinary JAR with a root `module-info.class` — runs on the
**module path** (`java -p … -m …`) on the node's shared JDK installation, exactly like a fat
JAR runs on the class path. Set `entry.mode: "module"` with the module name:

```json
{ "schemaVersion": 1, "mainJar": "orders.jar",
  "entry": { "mode": "module", "module": "com.acme.orders" } }
```

This produces `java -p /app/orders.jar -m com.acme.orders` (the module's declared
`Main-Class` is used). Add `entry.mainClass` to launch a specific class
(`-m com.acme.orders/com.acme.orders.Main`).

**Auto-detection.** `brewlet push` and the Maven plugin detect a modular JAR
automatically and default to `entry.mode: module`, reading the module name (and
declared main class) from the descriptor — no config needed for a single modular
JAR.

**Multi-JAR module path.** Ship the app module plus its library modules as a
`modulepath.layer.v1+tar` layer (unpacked to `/app/mods`) and list the module
path explicitly:

```json
{ "schemaVersion": 1, "mainJar": "orders.jar",
  "entry": { "mode": "module", "module": "com.acme.orders", "modulePath": ["orders.jar", "mods"] } }
```

```bash
brewlet push ./target/orders.jar demo/orders:1.0 --module-layer mods.tar
```

produces `java -p /app/orders.jar:/app/mods -m com.acme.orders`.

> Do not publish a `jlink` runtime or `.jmod` files in the application artifact.
> Platform administrators can provide a shared jlink runtime and approved module
> set through `NodeProfile`; see [JDK management](jdk-management.md#shared-jlink-runtimes)
> and the [JPMS support note](jpms-support.md).

### Layered (thin JAR) apps

Instead of one fat JAR, you can ship a **thin application JAR** plus one or more
**dependency layers** so registries and nodes dedup the unchanged dependencies and
only the small app layer moves per build. Set `entry.classPath` (ordered,
`/app`-relative) and attach the dependency JARs as a tar layer:

For a governed variant, platform teams can publish an approved immutable
classpath as a **managed dependency bundle**. Application publication verifies
the Maven runtime graph against that bundle and reuses its exact layer in the
final thin-JAR image. See
[Managed dependency bundles](managed-dependency-bundles.md).

```json
{ "schemaVersion": 1, "mainJar": "app.jar",
  "entry": { "mode": "classpath", "mainClass": "com.example.Main", "classPath": ["app.jar", "lib/*"] } }
```

**Maven plugin (recommended).** Enable layering to publish a thin app JAR and
reproducible dependency layers with matching `entry.mode: classpath` /
`entry.classPath`. Plain thin JARs use the resolved POM dependency tree. Standard
Spring Boot executable JARs are actually unpacked: application classes/resources
move from `BOOT-INF/classes/` to the app JAR root, and dependencies come from the
exact packaged `BOOT-INF/lib/*.jar` entries, not a second dependency resolution:

First [select and configure the released plugin](#option-c-maven-plugin) below.
Keep its exported `BREWLET_VERSION` for these commands and Maven configuration.

```bash
# Enable layering on the command line
mvn clean package brewlet:push \
  -Dbrewlet.image=registry.example.com/team/app:1.4.2 \
  -Dbrewlet.layered=true
```

```xml
<plugin>
  <groupId>sh.brewlet</groupId>
  <artifactId>brewlet-maven-plugin</artifactId>
  <version>${env.BREWLET_VERSION}</version>
  <configuration>
    <image>registry.example.com/team/app:${project.version}</image>
    <layered>true</layered>                         <!-- thin JAR + dependency layers -->
    <splitSnapshotLayers>true</splitSnapshotLayers>  <!-- deps vs. snapshot-deps (default) -->
  </configuration>
</plugin>
```

By default (`brewlet.splitSnapshotLayers=true`) released dependencies and internal
`-SNAPSHOT` dependencies land in separate `deps` / `snapshot-deps` layers (stable →
volatile) for finer dedup; set it to `false` to pack all dependencies into one layer.
See the [plugin README](https://github.com/microsoft/brewlet/blob/main/maven-plugin/README.md#configuration-parameters)
for the full option reference.

For Boot JARs, `BOOT-INF/classpath.idx` defines library order when present and
must name every packaged library exactly once; otherwise archive order is used.
The generated classpath lists dependencies explicitly rather than relying on
wildcard ordering. Build, push, inspect/config, and AppCDS share the same prepared
bytes, while the source JAR is left unchanged. See the
[supported layouts and signature handling](https://github.com/microsoft/brewlet/blob/main/maven-plugin/README.md#layered-spring-boot-jars).

**CLI.** Pre-build the dependency tar(s) yourself and attach them with
`--classpath-layer` (repeatable):

```bash
# lib/ holds your runtime dependency JARs (e.g. mvn dependency:copy-dependencies)
tar -cf deps.tar -C lib .
brewlet push ./target/app.jar demo/app:1.4.2 --config cfg.json --classpath-layer deps.tar
```

**ORAS.** The multi-layer form is a plain multi-layer OCI push — one
`classpath.layer.v1+tar` per dependency tar, in stable → volatile order:

```bash
oras push registry.example.com/team/app:1.4.2 \
  --artifact-type application/vnd.brewlet.app.v1+json \
  --config   jvm-config.json:application/vnd.brewlet.jvm.config.v1+json \
  target/app.jar:application/vnd.brewlet.jar.layer.v1+jar \
  deps.tar:application/vnd.brewlet.classpath.layer.v1+tar \
  snapshot-deps.tar:application/vnd.brewlet.classpath.layer.v1+tar
```

**The resulting artifact** carries a thin `app.jar` as the
`application/vnd.brewlet.jar.layer.v1+jar` layer plus one or more
`application/vnd.brewlet.classpath.layer.v1+tar` layers. Each dependency tar is
unpacked to `/app/lib` in the sandbox. The generic thin-JAR example launches with
`java -cp /app/app.jar:/app/lib/* com.example.Main` (the JVM expands the `lib/*`
wildcard); prepared Boot images instead name each dependency in order.
`brewlet inspect` lists every layer with its media type and digest so you
can see the split:

```bash
brewlet inspect registry.example.com/team/app:1.4.2
# == manifest ==
#   layers:
#     application/vnd.brewlet.jar.layer.v1+jar        sha256:…  (thin app.jar)
#     application/vnd.brewlet.classpath.layer.v1+tar  sha256:…  (deps → /app/lib)
#     application/vnd.brewlet.classpath.layer.v1+tar  sha256:…  (snapshot-deps → /app/lib)
# == jvm config ==  entry.mode=classpath, entry.classPath=["app.jar","lib/*"]
```

Because each layer has its own digest, a code-only rebuild changes **only the small
`app.jar` layer** — the dependency layers are already present in the registry and the
node content store, so only the app layer moves over the wire (and identical
dependency layers dedup across apps). See the
[layered classpath deployment note](layered-classpath-deployment.md) for the design,
cache behavior, and layer-ordering strategy.

> **Already have a framework that emits layered output?** If your framework can
> produce an exploded classes directory plus a directory of dependency JARs (Spring
> Boot's `layertools`, `mvn dependency:copy-dependencies`, Gradle `bootJar`, …), you
> can map that straight onto Brewlet's *generic* classpath layers with a few
> structural steps — **Brewlet never parses `layers.idx` or any framework-specific
> layering manifest**. The Spring PetClinic walkthrough works this through end to
> end: [mapping a framework's layered output](spring-petclinic.md#layered-classpath-delivery).

---

## 3. Publish the artifact

### Publish with a managed dependency bundle

Platform teams can publish an approved dependency bundle from a Maven BOM with
`mvn package brewlet:dependency-bundle`; application teams then select it with
`mvn package brewlet:push -Dbrewlet.dependencyBundle=...`. Brewlet verifies the
application graph, rejects fat JARs, and never falls back to application-built
dependency layers after a managed-bundle error.

See [Managed dependency bundles](managed-dependency-bundles.md) for the Ops and
developer workflows, signing options, trust roles, and Go CLI boundary.

### Option A — the `brewlet` CLI

```bash
az acr login --name myacr          # or: docker login registry.example.com
brewlet push ./target/app.jar myacr.azurecr.io/team/app:1.4.2 --push-result push.json
# pushed myacr.azurecr.io/team/app:1.4.2 (runnable OCI image — kubelet-pullable)
#   deploy image: myacr.azurecr.io/team/app@sha256:...
```

- Ships **only the JAR** — no Dockerfile, no base image, no OS or JVM layers.
- Defaults to `--format image`: a standard, kubelet-pullable OCI image index, so a
  `runtimeClassName: brewlet` pod can just set `image: <ref>`. Use
  `--format artifact` for the registry-native Brewlet media types.
- Generates a minimal launch config, or embeds one you pass with `--config jvm-config.json`.
- Attach a prebuilt AppCDS archive with `--appcds-archive ./target/app.jsa` to speed up
  startup (mounted at `/app/app.jsa`, launched with `-Xshare:auto`). See [AppCDS](appcds.md).
- Full flags: [CLI reference](cli-reference.md#brewlet-push).

- A `<ref>` that names a registry host is uploaded directly to that registry using
  your Docker credentials (`credHelpers`, `auths`, `credsStore`) or
  `BREWLET_REGISTRY_USERNAME`/`BREWLET_REGISTRY_PASSWORD` — the same chain and
  trust policy as the Maven plugin, minus `settings.xml`. See the
  [CLI reference](cli-reference.md#brewlet-push) for Docker helper selection and
  fallback rules. A ref without a host never defaults to Docker Hub.
- `--push-result FILE` writes the same `push.json` handoff as the Maven plugin.

To build into a local **OCI layout** instead (for `brewlet inspect`, `run`, or
`bundle`), pass `--store` explicitly; the ref then names the image *within* that
layout:

```bash
brewlet push ./target/app.jar registry.example.com/team/app:1.4.2 --store ./oci
```

Inspect what you built:

```bash
brewlet inspect registry.example.com/team/app:1.4.2 --store ./oci
# == manifest ==   (OCI image index by default; brewlet media types with --format artifact)
# == jvm config == (the launch config above)
```

### Option B — ORAS (a real registry, today)

The artifact is a standard OCI artifact, so `oras` pushes it to any OCI 1.1+
registry:

```bash
cat > jvm-config.json <<'EOF'
{ "schemaVersion": 1, "mainJar": "app.jar",
  "entry": { "mode": "jar" } }
EOF

oras push registry.example.com/team/app:1.4.2 \
  --artifact-type application/vnd.brewlet.app.v1+json \
  --config   jvm-config.json:application/vnd.brewlet.jvm.config.v1+json \
  target/app.jar:application/vnd.brewlet.jar.layer.v1+jar
```

The [media types](reference.md#oci-media-types) are what mark this as a Brewlet JAR
artifact rather than a container image.

### Option C — Maven plugin

The [Brewlet Maven plugin](https://github.com/microsoft/brewlet/tree/main/maven-plugin/) wraps steps 2–3 so developers
never touch ORAS or hand-author the launch config. It infers the entry point and
framework from the project and JAR manifest; its optional `manifest` goal writes the
descriptor's JDK feature request and infers the container `ports`.

JDK requests come from an explicit `brewlet.jdkFeature` override or the effective
main compiler `release`/`target`/`source` configuration. A selected compiler
toolchain is a fallback when no level is declared, ahead of the in-process Maven
JDK. Test-only settings are ignored, and ambiguous or unresolved authority fails
with override guidance instead of silently selecting the build machine's JDK.
See the [complete inference precedence](https://github.com/microsoft/brewlet/blob/main/maven-plugin/README.md#jdk-inference).

Resolve the [latest GitHub release](https://github.com/microsoft/brewlet/releases/latest)
once. If your platform team has already supplied a concrete `BREWLET_VERSION`,
keep that version and skip this lookup so the plugin matches the platform:

```bash
release_url="$(curl -fsSL -o /dev/null -w '%{url_effective}' \
  https://github.com/microsoft/brewlet/releases/latest)" &&
BREWLET_VERSION="${release_url##*/}" &&
BREWLET_VERSION="${BREWLET_VERSION#v}"
```

The plugin is published to
[Maven Central](https://central.sonatype.com/artifact/sh.brewlet/brewlet-maven-plugin)
starting with 0.5.1. Maven downloads it automatically: no manual JAR download,
`install-file`, custom repository, or GitHub credentials are needed.

```bash
export BREWLET_VERSION
```

Add this declaration to your application's `<build><plugins>` in `pom.xml`.
`${env.BREWLET_VERSION}` reads the concrete version exported above, not a moving
`latest` alias. For reproducible project builds, pin that resolved version in
your POM or CI environment instead of resolving latest on every build. The
declaration enables the short `brewlet` prefix without changing the build
lifecycle:

```xml
<plugin>
  <groupId>sh.brewlet</groupId>
  <artifactId>brewlet-maven-plugin</artifactId>
  <version>${env.BREWLET_VERSION}</version>
  <configuration>
    <image>registry.example.com/team/app:${project.version}</image>
    <jdkFeature>21</jdkFeature>
    <ports><port><name>http</name><containerPort>8080</containerPort></port></ports>
  </configuration>
</plugin>
```

```bash
# Build the fat JAR, publish its OCI image, and generate development YAML:
mvn clean package brewlet:push brewlet:manifest \
  -Dbrewlet.image=registry.example.com/team/app:1.4.2
```

This generates `target/brewlet/javaapplication.yaml` with the actual
published digest. Review its runtime, ports and probes, then apply it separately:

```bash
kubectl apply -f target/brewlet/javaapplication.yaml
```

You do not need to copy a digest or write YAML by hand. For a complete
development loop with explicit health probes and resource limits, follow the
[developer workshop](workshops/developers.md).

To bind publishing to `mvn deploy`, optionally add this inside the plugin
declaration:

```xml
<executions>
  <execution><goals><goal>push</goal></goals></execution>
</executions>
```

If a newly tagged version is not yet available on Central, wait for its Maven
Central publishing job and repository propagation; do not silently select a
different plugin version. Select a release available on Central.

For CI publication without development YAML, omit `brewlet:manifest`.
`brewlet:push` prints a digest-pinned `deploy image` and records it in
`target/brewlet/push.json` for a separate production deployment stage:

```bash
mvn package brewlet:push -Dbrewlet.registry=registry.example.com/team
jq -r '.deployImage' target/brewlet/push.json
```

The CI build is complete when publication succeeds. Keep production deployment
manifests and cluster credentials in the release stage: use the saved `deployImage` with
`kubectl`, Helm or GitOps, and optionally `brewlet k8s app wait` for readiness.
See [Deploy a published image](deploying-workloads.md#deploy-a-published-image).

For offline generation and manifest inputs, see the
[development manifest reference](https://github.com/microsoft/brewlet/blob/main/maven-plugin/README.md#development-manifest).
The plugin reference also explains
[build/push ordering](https://github.com/microsoft/brewlet/blob/main/maven-plugin/README.md#buildpush-ordering)
and [dry-run behavior](https://github.com/microsoft/brewlet/blob/main/maven-plugin/README.md#dry-run-behavior).

`<registry>` derives the image as `<registry>/${project.artifactId}:${project.version}`;
a full `<image>` still works. References without a registry host (for example
`app:1.0`) are rejected instead of silently targeting Docker Hub. Publishing
requires a tag, not a digest-pinned destination; use the resulting digest for
deployment and promotion without republishing.

Follow-up goals work in a fresh Maven invocation after `mvn package`: for standard
unclassified JAR projects, the plugin finds
`${project.build.directory}/${project.build.finalName}.jar` when Maven has not
associated the packaged artifact with the new session. An explicit
`-Dbrewlet.jarFile=/path/to/app.jar` takes precedence and is required for custom
packaging, classifiers, or JAR-plugin output overrides. The plugin does not guess
among files in `target`. `manifest` is the exception: always invoke it together
with `build` or `push`, not on its own in a later invocation.

Goals: `brewlet:config` (generate the launch config), `brewlet:build` (assemble a
local OCI layout), `brewlet:push` (publish to a registry),
`brewlet:manifest` (generate development YAML for the current build),
`brewlet:inspect` (dry-run preview), `brewlet:dependency-bundle` (publish shared
runtime dependencies), `brewlet:help` (goal and parameter reference),
and `brewlet:appcds` (generate an AppCDS startup archive — see [AppCDS](appcds.md)).
See the [plugin README](https://github.com/microsoft/brewlet/blob/main/maven-plugin/README.md) for the full goal and
parameter reference.

`brewlet:push` publishes over HTTPS and keeps your registry credentials on the
registry's own origin. Plain HTTP is allowed only for exact loopback registries
such as `localhost:5000`, so publishing to an internal plaintext registry
(`registry.internal:5000`) fails until you list its authority in
`insecureRegistries`; a registry that answers with a cross-origin bearer token
realm likewise fails until you list that realm in `allowedTokenRealms`.
Credentials stored by `docker login` or `az acr login` — including Docker
credential helpers (`credsStore`/`credHelpers`) and identity tokens — are used
automatically. See
[publish-time registry credentials](security.md#publish-time-registry-credentials).

---

## 4. Pin to a digest

Kubernetes workloads using `runtimeClassName: brewlet` must use a
digest-pinned reference:

```
registry.example.com/team/app@sha256:<digest>
```

The shim extracts this immutable target from containerd's protected CRI
metadata, resolves it directly from the content store, and verifies that the
selected platform manifest has the config digest CRI recorded for the
container. Tag-only requests are rejected before launch. The admission webhook
mirrors the target digest in `brewlet.sh/artifact-digest` only as a
cross-checked compatibility hint. The shipped managed-dependency admission
example verifies Brewlet DSSE attestations against this immutable image identity.
General cosign or standard SLSA admission for application images remains roadmap
work; component-release build provenance is a separate shipped capability.
See [Security](security.md).

---

## What you did *not* have to do

- No `Dockerfile`.
- No base image to pick, pin, or patch.
- No JVM copied into an image.
- No OS or JDK layers in the application payload. Transfer size still depends
  on application dependencies, optional CDS archives, and existing layer caches.

## Next steps

- **[Deploying workloads](deploying-workloads.md)** — run the artifact on a cluster.
- **[Launchers](launchers.md)** — pick `java` vs `jaz`.
- **[Resource requests, limits & JVM tuning](resource-tuning.md)** — set the right JVM flags.
