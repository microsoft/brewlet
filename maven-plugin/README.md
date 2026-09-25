# Brewlet Maven Plugin

[![Maven plugin CI](https://github.com/microsoft/brewlet/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/microsoft/brewlet/actions/workflows/ci.yml)
[![Maven plugin release](https://github.com/microsoft/brewlet/actions/workflows/release.yml/badge.svg)](https://github.com/microsoft/brewlet/actions/workflows/release.yml)

Build and publish a [Brewlet](https://github.com/microsoft/brewlet) OCI app straight from your Maven build —
no Dockerfile, no base image, no container build. A single `mvn brewlet:push`
serializes a launch config, packages your fat JAR, and publishes it to any OCI 1.1+
registry. By default it publishes a **runnable, kubelet-pullable OCI image** (a
`runtimeClassName: brewlet` pod can name it as `image: <ref>`); pass
`-Dbrewlet.format=artifact` for the native Brewlet artifact instead (see
[Delivery format](#delivery-format-native-artifact-vs-runnable-image)).

The plugin wraps steps 2–3 of the [build & publish flow](../specs/SPECIFICATION.md#43-build--publish-flow-developer-experience)
so you never touch `oras` or hand-author a `jvm-config.json`. It infers as much as
possible (main class, framework, ports) from the project and the built JAR's
manifest. JDK feature and launcher requests belong to the generated deployment
descriptor, not the artifact config.

- **Coordinates:** `sh.brewlet:brewlet-maven-plugin` on [Maven Central](https://central.sonatype.com/artifact/sh.brewlet/brewlet-maven-plugin)
- **Requires:** Maven 3.9+, JDK 17+ (to run the build). The `appcds`
  training goal requires a JDK 21+ training runtime.

---

## Quick start

The plugin is available from Maven Central starting with 0.5.1. Maven downloads
it automatically; no manual installation, GitHub credentials, or custom
repository is needed.

Use the concrete `BREWLET_VERSION` supplied by your platform team. Otherwise,
resolve the latest Brewlet release once:

```bash
release_url="$(curl -fsSL -o /dev/null -w '%{url_effective}' \
  https://github.com/microsoft/brewlet/releases/latest)" &&
BREWLET_VERSION="${release_url##*/}" &&
BREWLET_VERSION="${BREWLET_VERSION#v}" &&
export BREWLET_VERSION
```

Add the plugin to your application's `<build><plugins>` section. The version
comes from the exported environment variable, not a moving `latest` alias.
For reproducible builds, pin the resolved version in your POM or CI environment:

```xml
<plugin>
  <groupId>sh.brewlet</groupId>
  <artifactId>brewlet-maven-plugin</artifactId>
  <version>${env.BREWLET_VERSION}</version>
</plugin>
```

Build the application and publish it using the short `brewlet` prefix:

```bash
mvn clean package brewlet:push \
  -Dbrewlet.image=registry.example.com/team/app:1.4.2
```

Declaring the plugin does not bind `push` to the build lifecycle; it only enables
direct goal invocation. `mvn brewlet:push` alone requires an already packaged application.

For a one-off invocation without editing the application's POM, use the full
coordinates instead. Maven still resolves the plugin from Central:

```bash
mvn clean package "sh.brewlet:brewlet-maven-plugin:${BREWLET_VERSION}:push" \
  -Dbrewlet.image=registry.example.com/team/app:1.4.2
```

If a newly tagged release has not reached Central yet, wait for its publishing
job and repository propagation. Do not switch to a different plugin version.

### Optional lifecycle binding

To configure publishing once in `pom.xml` and bind `push` to `mvn deploy`, extend
the plugin declaration:

```xml
<plugin>
  <groupId>sh.brewlet</groupId>
  <artifactId>brewlet-maven-plugin</artifactId>
  <version>${env.BREWLET_VERSION}</version>
  <configuration>
    <image>registry.example.com/team/app:${project.version}</image>
    <jdkFeature>21</jdkFeature>       <!-- written to brewlet:manifest descriptors -->
    <launcher>java</launcher>         <!-- omit for vanilla java; use jaz if installed -->
    <ports>                             <!-- descriptor spec.ports (manifest goal) -->
      <port><name>http</name><containerPort>8080</containerPort></port>
    </ports>
    <enablePreview>true</enablePreview>      <!-- artifact correctness flag -->
    <addOpens>
      <addOpen>java.base/java.lang=ALL-UNNAMED</addOpen>
    </addOpens>
    <systemProperties>
      <spring.aot.enabled>true</spring.aot.enabled>
    </systemProperties>
    <jvmArgs>                               <!-- deployment tuning; spec.jvm.args -->
      <jvmArg>-XX:MaxRAMPercentage=75.0</jvmArg>
    </jvmArgs>
  </configuration>
  <executions>
    <execution>
      <goals><goal>config</goal><goal>push</goal></goals>
    </execution>
  </executions>
</plugin>
```

With that, `mvn deploy` publishes the runnable OCI image and prints its
digest-pinned `deploy image`. Pass that exact reference to the manifest goal:

```bash
mvn brewlet:manifest \
  -Dbrewlet.image=registry.example.com/team/app@sha256:REPLACE_WITH_IMAGE_DIGEST
```

---

## Goals

| Goal | Default phase | What it does |
|---|---|---|
| `brewlet:config` | `package` | Generate `target/brewlet/jvm-config.json` from POM metadata + the JAR manifest. Input for `build`/`push`. |
| `brewlet:build` | — | Assemble the OCI artifact into a local **OCI image-layout** dir (`target/brewlet/oci`) without pushing. Good for inspection, air-gapped flows, or local-registry tests. |
| `brewlet:push` | `deploy` | Build and push to the registry in `<image>`. By default (`image` format) this pushes a **runnable OCI image** — a standard, kubelet-pullable image (see [Delivery format](#delivery-format-native-artifact-vs-runnable-image)). With `-Dbrewlet.format=artifact` it pushes the native Brewlet artifact instead (JAR layer + launch-config blob + manifest with `artifactType: application/vnd.brewlet.app.v1+json`). |
| `brewlet:appcds` | — | Generate a dynamic AppCDS archive (`target/brewlet/app.jsa`) from the same fat/thin/Boot/module payload used for publication, using a self-terminating run or explicit signal-mode training. Attach it later with `-Dbrewlet.cdsArchive=...`. |
| `brewlet:dependency-bundle` | `package` | Resolve the runtime dependency closure, create a canonical lock and deterministic flat classpath tar, write `target/brewlet/dependency-bundle-oci`, and publish an OCI dependency bundle. |
| `brewlet:manifest` | — | Emit a `JavaApplication` CR YAML compatible with the [Brewlet Kubernetes components](../kubernetes) to `target/brewlet/` for `kubectl apply`, including `spec.jvm.version` / `spec.jvm.launcher`. Pass the digest-pinned image reference printed by `brewlet:push`. Health probes must be configured explicitly in the generated manifest. |
| `brewlet:inspect` | — | Print the fully-resolved launch config and OCI descriptor that *would* be pushed — a dry run to verify inference. |

Run any goal directly, e.g. `mvn brewlet:inspect`.

---

## Configuration parameters

All parameters are optional unless noted; most have a `-Dbrewlet.*` command-line
property. Values configured in `<configuration>` and CLI properties can be mixed.

### Core

| Parameter | Property | Default | Notes |
|---|---|---|---|
| `image` | `brewlet.image` | — | Target OCI ref, e.g. `registry.example.com/team/app:1.4.2` for `build`/`push` or `registry.example.com/team/app@sha256:…` for `manifest`. **Required for `build`, `push`, and `manifest`; Kubernetes manifests must use the digest-pinned form.** |
| `format` | `brewlet.format` | `image` | Delivery format for `push`: `image` (runnable, kubelet-pullable OCI image — the default) or `artifact` (native Brewlet OCI artifact). See [Delivery format](#delivery-format-native-artifact-vs-runnable-image). |
| `jarFile` | `brewlet.jarFile` | project's primary artifact | Path to the application JAR to publish. After a separate `mvn package`, standard unclassified `jar`/`maven-plugin` projects can use `${project.build.directory}/${project.build.finalName}.jar`. Custom packaging, classifier, or JAR-plugin output overrides require an explicit `jarFile`; the plugin never searches for a newest or arbitrary JAR. |
| `mainClass` | `brewlet.mainClass` | inferred from `Main-Class` | Main class to launch. Does **not** by itself set the entry mode — the mode is inferred from the JAR's shape (see `entryMode`). Used in `classpath` mode (the class launched via `-cp`) and optionally in `module` mode (selects `<module>/<mainClass>`); ignored in `jar` mode (uses the manifest's `Main-Class`). |
| `entryMode` | `brewlet.entryMode` | inferred from manifest | `jar`, `classpath`, or `module` (auto-detected for modular JARs with a root `module-info.class`). |
| `outputDirectory` | — | `${project.build.directory}/brewlet` | Where generated files land. |
| `skip` | `brewlet.skip` | `false` | Skip all Brewlet goals. |
| `dryRun` | `brewlet.dryRun` | `false` | Generate + display the config but do not push. |
| `layered` | `brewlet.layered` | `false` | **Layered deployment.** Plain thin JARs use the resolved POM runtime dependencies and `entry.classPath=[mainJar, "lib/*"]`; standard Spring Boot executable JARs are unpacked into a thin application JAR plus their exact packaged libraries and an explicitly ordered classpath (see below). Modular JARs use dependency modules at `/app/mods` and `entry.modulePath=[mainJar, "mods"]`. Non-modular layering selects `classpath` mode. Unchanged dependency layers dedup by digest. |
| `splitSnapshotLayers` | `brewlet.splitSnapshotLayers` | `true` | When `layered`, pack released deps and `-SNAPSHOT` deps into separate `deps` / `snapshot-deps` layers (stable→volatile) for finer dedup. |
| `dependencyBundle` | `brewlet.dependencyBundle` | — | For `push`, a registry reference or local OCI-layout directory containing a managed dependency bundle. The resolved runtime graph must exactly match its lock. Forces thin-JAR classpath launch and requires `mainClass`. |
| `signingKey` | `brewlet.signingKey` | — | Optional PKCS#8 PEM ECDSA P-256 private key. When present, bundle or final-image provenance is published and must be paired with the corresponding identity. |
| `trustedPublicKey` | `brewlet.trustedPublicKey` | — | SubjectPublicKeyInfo PEM ECDSA P-256 public key trusted to verify a managed bundle. Required when the selected bundle has provenance. |
| `signerIdentity` | `brewlet.signerIdentity` | — | Bundle-publisher identity used only by `dependency-bundle`; required when that goal uses `signingKey`. |
| `trustedSignerIdentity` | `brewlet.trustedSignerIdentity` | — | Expected identity in signed bundle provenance. Required when the selected bundle has provenance. |
| `builderIdentity` | `brewlet.builderIdentity` | — | Application publisher identity asserted in optional final-image provenance. Required with `signingKey` when pushing a managed application. |
| `cdsArchive` | `brewlet.cdsArchive` | — | Optional prebuilt AppCDS `.jsa` archive to append as a `application/vnd.brewlet.cds.layer.v1+jsa` layer after dependency layers. The archive basename becomes `cds.archive`, is mounted at `/app/<name>`, and launches with `-Xshare:auto -XX:SharedArchiveFile=/app/<name>` as best-effort acceleration. See [AppCDS §4.1](https://github.com/microsoft/brewlet/blob/main/docs/appcds.md#41-build-time-archive-layer-recommended-primary). |

### Layered Spring Boot JARs

For a standard Spring Boot executable JAR using `JarLauncher` or
`launch.JarLauncher`, `layered=true` prepares the **bytes**, not just a different
launch command:

- `BOOT-INF/classes/` becomes the root of a deterministic thin application JAR,
  including application resources.
- Flat `BOOT-INF/lib/*.jar` entries supply the exact dependency bytes. Maven's
  resolved dependency graph does not replace those packaged libraries.
- If `BOOT-INF/classpath.idx` exists, it must list every packaged library
  exactly once. Otherwise ZIP library order is preserved. `entry.classPath`
  lists the app and each `lib/<name>.jar` explicitly, so layer grouping does not
  alter class resolution.

Prepared files live under `target/brewlet/prepared/` by default. Build, push,
config, inspect, and AppCDS training use the same prepared application. The input
JAR is unchanged, and the plugin does not guess a sibling `.original` file.
Without `layered=true`, ordinary Boot fat-JAR launching is unchanged.

Valid signed Boot containers produce a new **unsigned application JAR** after
content verification; signatures invalidated by repackaging are removed.
Packaged dependency JARs remain byte-for-byte intact, including their signatures.
WAR/custom-loader/PropertiesLauncher layouts, custom Boot paths, `requiresUnpack`,
ZIP64, and shell-prefixed executable archives are rejected for layered
preparation, as are unsafe or ambiguous ZIP entries. Supply a supported standard
Boot JAR or an explicitly prepared thin JAR rather than relying on silent
layout conversion. `layers.idx` grouping is not interpreted.

### Registry transport and credential safety

Registry credentials are resolved from `settings.xml`, `~/.docker/config.json`,
or `BREWLET_REGISTRY_USERNAME` / `BREWLET_REGISTRY_PASSWORD`, and the plugin
keeps them scoped to the registry you configured:

- **HTTPS is required** for every registry except exact loopback authorities
  (`localhost`, `127.0.0.0/8`, `::1`, with or without a port). Matching is exact,
  so a lookalike host such as `localhost.attacker.example` or `127.example.com`
  stays on HTTPS instead of leaking Basic credentials over plaintext.
- **Credentials never leave the registry origin.** The `Authorization` header is
  attached only to requests whose scheme, host, and port match the configured
  registry. Registry-supplied URLs — blob upload `Location` values, pagination
  links, storage redirects — are still followed, but without credentials.
- **Token realms are validated.** A `WWW-Authenticate: Bearer` challenge realm
  must be an absolute `https` URL (plain `http` only for insecure-eligible
  authorities) with no embedded credentials. Credentials are exchanged only with
  a same-origin realm, Docker Hub's built-in `auth.docker.io` realm, or a realm
  you explicitly allowlisted; any other realm fails the build rather than
  forwarding your credentials.

| Parameter | Property | Default | Notes |
|---|---|---|---|
| `insecureRegistries` | `brewlet.insecureRegistries` | *(empty)* | Exact registry authorities (`host` or `host:port`) that may be contacted over plain HTTP. Loopback registries are always allowed, so this is only needed for a non-loopback HTTP registry such as an in-cluster mirror. Configure as `<insecureRegistries><insecureRegistry>registry.internal:5000</insecureRegistry></insecureRegistries>` or `-Dbrewlet.insecureRegistries=registry.internal:5000`. |
| `allowedTokenRealms` | `brewlet.allowedTokenRealms` | *(empty)* | Exact authorities (`host` or `host:port`) trusted to receive this build's registry credentials when the authentication challenge realm is **not** the registry's own origin. Docker Hub (`auth.docker.io`) is trusted automatically; add an entry only when you trust that host with your registry credentials. |

### Descriptor JDK / launcher requests

These parameters feed `brewlet:manifest` and are written to the deployment
descriptor. They are **not** serialized into `target/brewlet/jvm-config.json`.

| Parameter | Property | Default | Notes |
|---|---|---|---|
| `jdkFeature` | `brewlet.jdkFeature` | inferred from the main compiler configuration or toolchain | Positive JDK feature request written as `spec.jvm.version`; an explicit value overrides inference. See the precedence below. |
| `jdkDistribution` | `brewlet.jdkDistribution` | *(none — any distribution)* | Optional JDK distribution (`temurin`, `microsoft`) written as `spec.jvm.distribution`. With `jdkFeature` it pins an exact `<distribution>-<feature>` node JDK; omit to accept any distribution of that feature. |
| `launcher` | `brewlet.launcher` | `java` | Launcher written as `spec.jvm.launcher`; use `jaz` for the auto-tuning launcher. |

### JDK inference

The plugin selects a **requested deployment JDK feature**, not a measurement of
the running Pods or a proof of the application's minimum compatible JVM:

1. A positive explicit `<jdkFeature>` / `-Dbrewlet.jdkFeature` wins.
2. Effective main compiler settings use `release`, then `target`, then `source`.
   Inherited settings, active profiles, and main compile executions participate.
   Literal compiler XML takes precedence over its property counterpart;
   `${...}` expressions are evaluated with Maven's property semantics.
   Disabled executions and test-only compiler settings do not determine the
   application request.
3. Without a declared level, use the compiler's `jdkToolchain`, then a suitable
   session-selected toolchain, then main-bound legacy toolchains-plugin
   requirements. Configured matching follows Maven's first-match order,
   including resolvable version ranges; it does not choose the highest or lowest
   installed JDK. The selected feature is checked against the JDK's `release`
   metadata.
4. Only when no other compiler authority is configured, use the in-process
   Maven JDK as a fallback. Implicit defaults from every compiler-plugin version
   are not emulated.

For example, a main compiler `<release>17</release>` requests JDK 17 even when
Maven or its compiler toolchain runs on JDK 21. A literal `<release>17</release>`
also remains authoritative over an unrelated `maven.compiler.release=21`
property; reference that property in the XML if it is intended to control the
build.

Inference fails with explicit-override guidance for unresolved or malformed
values, differing main compilation levels, unavailable toolchains, unsupported
compiler/executable choices, or opaque arguments that could change the target.
Standalone automatic toolchain discovery and a selection that might belong
only to test or later build phases are not guessed. Set `brewlet.jdkFeature`
after reviewing those builds rather than relying on the Maven JVM by accident.
An execution bound to `compile` can still run after the main compiler in that
same phase; selections whose ordering cannot establish main-compiler authority
also require an explicit request.
Failures occur before writing a guessed manifest, and logs identify the
compiler setting or fallback toolchain used.

The same request resolution is used when managed-dependency publication checks
the bundle's compatible JDK features. It remains separate from the artifact's
launch configuration and does not install or upgrade a node JDK.

### Runtime shape

| Parameter | Notes |
|---|---|
| `ports` | Descriptor `spec.ports` (manifest goal): `<port>` entries (`name`, `containerPort`, `protocol`). Defaults to `8080/http` with a warning for Spring Boot / Quarkus. Ports enable Service generation but never imply health probes. Not part of the artifact. |
| `enablePreview` (`brewlet.enablePreview`) | App-intrinsic artifact knob; writes `enablePreview` and expands to `--enable-preview`. |
| `addModules` / `addOpens` / `addExports` | App-intrinsic artifact lists for JPMS/module access; configure with `<addModule>`, `<addOpen>`, and `<addExport>` entries. |
| `systemProperties` | App-intrinsic artifact map expanded as sorted `-D<key>=<value>` flags. |
| `jvmArgs` (`brewlet.jvmArgs`) | Deployment tuning/escape-hatch args written directly to descriptor `spec.jvm.args`; use for heap, GC, agents, and `-XX` flags. |
| `env` | `<envVar>` entries (`name`, `value`). |

Framework auto-detection is still used for port inference, but framework labels are
not written into the artifact. Process UID/GID is also excluded: Kubernetes
deployments set it through Pod `securityContext`, and artifact configs containing
`user` are rejected.

---

## Managed dependency bundles

Publish a reusable bundle from a Maven project:

```bash
mvn package brewlet:dependency-bundle \
  -Dbrewlet.dependencyBundleImage=registry.example.com/team/java-platform:1 \
  -Dbrewlet.sourceBom=com.acme:platform-bom:1 \
  -Dbrewlet.signingKey=builder-private.pem \
  -Dbrewlet.signerIdentity=https://ci.example.com/builders/platform
```

`sourceBom` is required and must be `G:A:V`. `compatibleJdks` can be configured
as an integer list. The goal always writes a local OCI layout
to `target/brewlet/dependency-bundle-oci`; `-Dbrewlet.dryRun=true` skips registry
publication.

The bundle layout and registry repository always receive a CycloneDX 1.5 SBOM
referrer. Supplying `signingKey` and `signerIdentity` additionally publishes a
DSSE-signed in-toto bundle-provenance referrer; omitting both publishes an
unsigned bundle.

The artifact uses:

- artifact type `application/vnd.brewlet.dependencies.v1+json`
- config `application/vnd.brewlet.dependencies.config.v1+json`
- dependency lock `application/vnd.brewlet.dependencies.lock.v1+json`
- reusable classpath layer `application/vnd.oci.image.layer.v1.tar+gzip` with
  `brewlet.sh/layer=classpath`

Consume it while publishing an application:

```bash
mvn package brewlet:push \
  -Dbrewlet.image=registry.example.com/team/orders:1 \
  -Dbrewlet.dependencyBundle=registry.example.com/team/java-platform:1 \
  -Dbrewlet.mainClass=com.acme.orders.Main \
  -Dbrewlet.signingKey=app-private.pem \
  -Dbrewlet.trustedPublicKey=builder-public.pem \
  -Dbrewlet.trustedSignerIdentity=https://ci.example.com/builders/platform \
  -Dbrewlet.builderIdentity=https://ci.example.com/builders/apps
```

`dependencyBundle` may instead name the local OCI-layout directory. The plugin
verifies artifact/config/lock/layer media types, all descriptor sizes and
SHA-256 digests, and exact GAV/type/classifier/scope/filename/file-hash agreement
with the current resolved runtime graph. It always requires and validates the
SBOM. When bundle provenance exists, it requires trust credentials and verifies
the subject, predicate, identity, and all digest bindings; invalid provenance
never falls back to unsigned treatment or project-built
layers after a bundle error. Managed mode rejects every application JAR with an
embedded `.jar` (including `BOOT-INF/lib` and `WEB-INF/lib`), forces
`entry.mode=classpath`, and composes the verified bundle layer into a runnable
image.

Dependency scope is part of the version 1 lock contract and comparison. A
`compile`/`runtime` scope difference is rejected even when the artifact bytes are
identical; publish the bundle from the same runtime dependency model used by the
applications.

The bundle config records both `layerDigest` (the compressed blob digest) and
`layerDiffId` (the uncompressed tar digest). Runnable images reuse the standard gzip
blob and descriptor unchanged and append that exact `diffId` to
`rootfs.diff_ids`, enabling registry deduplication and cross-repository mounting.
The custom `application/vnd.brewlet.classpath.layer.v1+tar` media type remains
only for native/legacy Brewlet artifacts because container runtimes cannot
unpack it as a runnable image layer.

When `signingKey` and `builderIdentity` are supplied, the final image index
receives a signed DSSE managed-dependency attestation. Otherwise the application
image is published unsigned.
It also carries
canonical `brewlet.sh/managed-dependency-evidence` JSON: schema version,
thin-JAR verdict, application JAR digest, bundle/layer/lock/SBOM digests, source
BOM, and builder identity. The annotation remains informational and makes no
claim about signature status;
the OCI referrer is the cryptographic evidence.

---

## AppCDS (`brewlet:appcds`)

AppCDS is Brewlet's optional startup accelerator from
[AppCDS §4.2](https://github.com/microsoft/brewlet/blob/main/docs/appcds.md#42-turnkey-generation-in-the-maven-plugin--cli):
run the app once with `-XX:ArchiveClassesAtExit`, produce `app.jsa`, then ship it
as an extra artifact layer.

```bash
mvn package brewlet:appcds \
  -Dbrewlet.appcds.trainingArgs=--warmup \
  -Dbrewlet.appcds.timeoutSeconds=180

mvn brewlet:push \
  -Dbrewlet.image=registry.example.com/team/app:1.4.2 \
  -Dbrewlet.cdsArchive=target/brewlet/app.jsa
```

You can also bind `appcds` to `pre-integration-test` when your application has a
short startup/warmup mode that exits on its own.

### Long-running servers — `signal` mode

Most Brewlet workloads are long-running servers that never exit on their own.
Dynamic CDS only writes the archive when the JVM exits cleanly, so for these use
`-Dbrewlet.appcds.mode=signal`: the goal starts the app, waits for a readiness
signal, then sends `SIGTERM` so the shutdown hook runs and the archive flushes.

```bash
# Spring Boot: wait for the "Started … in … seconds" line, then SIGTERM
mvn package brewlet:appcds \
  -Dbrewlet.appcds.mode=signal \
  -Dbrewlet.appcds.readyLog='Started .* in .* seconds' \
  -Dbrewlet.appcds.timeoutSeconds=180

# …or poll a health endpoint until it answers 2xx/3xx
mvn package brewlet:appcds \
  -Dbrewlet.appcds.mode=signal \
  -Dbrewlet.appcds.readyHttp=http://localhost:8080/actuator/health

# …or just warm up for a fixed number of seconds
mvn package brewlet:appcds \
  -Dbrewlet.appcds.mode=signal \
  -Dbrewlet.appcds.readyDelaySeconds=20
```

`signal` mode is Unix-oriented: it relies on a graceful `SIGTERM` shutdown to flush
the archive (Brewlet nodes are Linux). The app **must** shut down cleanly on
`SIGTERM` within `shutdownGraceSeconds`, or the archive won't be written.

Parameters:

| Parameter | Property | Default | Notes |
|---|---|---|---|
| `cdsArchiveOutput` | — | `${project.build.directory}/brewlet/app.jsa` | Where `brewlet:appcds` writes the generated dynamic-CDS archive. |
| `trainingArgs` | `brewlet.appcds.trainingArgs` | — | Extra program args passed after `-jar <mainJar>` during training. Use these to exercise startup paths (and, in `exit` mode, to make the app terminate). |
| `timeoutSeconds` | `brewlet.appcds.timeoutSeconds` | `120` | Execution budget in `exit` mode; combined readiness and settling budget in `signal` mode. Shutdown grace and failure cleanup are separate bounded waits. |
| `trainingJavaHome` | `brewlet.appcds.javaHome` | Maven's `java.home` | JDK used for training. Must be JDK 21+ per [AppCDS §2.2](https://github.com/microsoft/brewlet/blob/main/docs/appcds.md#22-minimum-jdk-version). |
| `mode` | `brewlet.appcds.mode` | `exit` | `exit` (self-terminating app) or `signal` (start → wait for readiness → `SIGTERM`). |
| `readyLog` | `brewlet.appcds.readyLog` | — | `signal` mode: regex matched line-by-line against the app's stdout/stderr; readiness fires on first match. |
| `readyHttp` | `brewlet.appcds.readyHttp` | — | `signal` mode: HTTP(S) URL polled until it returns 2xx/3xx. |
| `readyDelaySeconds` | `brewlet.appcds.readyDelaySeconds` | `0` | `signal` mode: fixed warmup delay before `SIGTERM` (used alone, or as a settle time after `readyLog`/`readyHttp`). |
| `shutdownGraceSeconds` | `brewlet.appcds.shutdownGraceSeconds` | `30` | `signal` mode: time allowed after `SIGTERM` for a graceful shutdown that flushes the archive. |
| `readyPollMillis` | `brewlet.appcds.readyPollMillis` | `500` | `signal` mode: poll interval for `readyHttp`. |

Every training attempt owns its JVM and output reader through completion.
Readiness failure, timeout, or interruption triggers bounded termination and
reaping rather than leaving a server behind. Failure cleanup allows up to five
seconds for graceful termination and another five for forced reaping, with
bounded output draining; interruption is preserved. Output-reader errors fail
the attempt, although a process wait may reach its timeout before reporting them.

Outside dry-run, the configured output archive is cleared before training and
configuration validation (after protecting the source JAR). Failed attempts
also remove partial output: an older archive must not masquerade as newly
trained output. Preserve a prebuilt archive elsewhere if it must survive a
failed attempt. Dry-run leaves archives untouched.

Layered class-path and JPMS module apps are supported: run `brewlet:appcds` with
the **same** `-Dbrewlet.layered=true` (and module shape) you push with. Plain thin
JARs use POM runtime dependencies in `lib/`; prepared Boot JARs use the exact
packaged libraries and explicit classpath order described above. Modular apps
stage dependencies into `mods/`. Training and publication share the prepared
payload and its hashes, matching the shim's `/app/lib` or `/app/mods` layout.
If you push a fat JAR, train a fat JAR; the training layout must match runtime.

Pairing caveat: HotSpot validates dynamic-CDS archives against each app-classpath
JAR's basename, size, and mtime, and Brewlet nodes pin the app JAR mtime to a
canonical timestamp. `brewlet:appcds` trains against a copy with that same mtime,
but the archive is still tied to the exact JDK build and classpath layout. With
`-Xshare:auto`, a mismatch safely falls back to base CDS (no correctness risk, just
no AppCDS benefit); see [AppCDS §7](https://github.com/microsoft/brewlet/blob/main/docs/appcds.md#7-the-jdk-coupling-problem-design-core).

### `brewlet:manifest` extras

The generated manifest deliberately omits `spec.probes`. A configured or inferred
port does not prove that HTTP `/` exists, or that it is suitable for liveness.
An API-only application returning 404 at `/` must not be restarted because of a
guessed probe.

Add probes to the generated YAML using the application's actual health contract.
For example, **only if these endpoints are implemented and enabled**:

```yaml
spec:
  probes:
    readiness:
      httpGet: { path: /actuator/health/readiness, port: 8080 }
    liveness:
      httpGet: { path: /actuator/health/liveness, port: 8080 }
```

TCP or exec probes can be appropriate for non-HTTP services. Without an explicit
readiness probe, Kubernetes does not wait for application-specific readiness;
omitting probes is a safe generation default, not a production health policy.
Keep reviewed deployment YAML in source control: regenerating the manifest
overwrites local edits.

| Parameter | Property | Default |
|---|---|---|
| `namespace` | `brewlet.namespace` | `default` |
| `appName` | `brewlet.appName` | `${project.artifactId}` |
| `replicas` | `brewlet.replicas` | `1` |
| CPU/memory requests & limits | `brewlet.resources.*` | `500m` / `256Mi` req, `2` / `512Mi` limit |

---

---

## Delivery format: native artifact vs runnable image

`brewlet:push` can publish in two formats, selected by `format` / `-Dbrewlet.format`:

| Format | What it publishes | When a pod names it as `image:` |
|---|---|---|
| `image` (default) | A **runnable OCI image**: your JAR (+ dependency/module/CDS payload) packaged as standard `tar+gzip` layers with a real OCI image config and a multi-arch index. The Brewlet launch contract rides in the `brewlet.sh/jvm-config` manifest annotation. | kubelet/containerd pull and unpack it like any image. A `runtimeClassName: brewlet` pod uses the digest-pinned reference printed by `brewlet:push`; tag-only execution is rejected. |
| `artifact` | The **native Brewlet OCI artifact**: your JAR plus a launch-config blob under Brewlet [media types](https://github.com/microsoft/brewlet/blob/main/docs/reference.md#oci-media-types) (`artifactType: application/vnd.brewlet.app.v1+json`). Compact and the canonical Brewlet shape. | kubelet/containerd **cannot** unpack the custom layer media types, so a bare pod `image:` reference `ImagePullBackOff`s. It is resolved by the shim out-of-band via the `brewlet.sh/artifact-*` annotations the admission webhook stamps. |

```bash
# The default push already produces a runnable, kubelet-pullable image:
mvn clean package brewlet:push \
  -Dbrewlet.image=registry.example.com/team/orders:1.4.2

# Opt into the native artifact instead (registry-native / pre-puller flows):
mvn brewlet:push \
  -Dbrewlet.image=registry.example.com/team/orders:1.4.2 \
  -Dbrewlet.format=artifact
```

Or pin the format in `pom.xml` (only needed to opt out of the `image` default):

```xml
<configuration>
  <image>registry.example.com/team/orders:1.4.2</image>
  <format>artifact</format>
</configuration>
```

The runnable image is **byte-compatible** with `brewlet push` from the Go CLI:
standard layers, `rootfs.diff_ids` computed over the *uncompressed* tars, and a
multi-arch index (a portable JAR publishes `amd64`+`arm64`; a JAR pinned to native
architectures via `arch` publishes just those). `brewlet:inspect` reports the runnable
shape (media types, `jvm-config` annotation, platforms). See
[runnable image documentation](https://github.com/microsoft/brewlet/blob/main/docs/runnable-image.md) for the full design.

---

## Output

- `target/brewlet/jvm-config.json` — the generated launch config
  (`application/vnd.brewlet.jvm.config.v1+json`).
- `target/brewlet/app.jsa` — optional AppCDS archive from `brewlet:appcds`.
- `target/brewlet/oci/` — a local OCI image-layout (from `brewlet:build`); readable
  with `brewlet inspect` or pushable with `oras push`.
- `target/brewlet/javaapplication.yaml` — the CR/Deployment manifest (from
  `brewlet:manifest`).

The published artifact uses the Brewlet [media types](https://github.com/microsoft/brewlet/blob/main/docs/reference.md#oci-media-types),
which mark it as an OCI artifact rather than a container image.

---

## Relationship to the `brewlet` CLI

The plugin mirrors the Go [`brewlet` CLI](https://github.com/microsoft/brewlet/blob/main/docs/cli-reference.md): `brewlet:push`
≈ `brewlet push`, `brewlet:inspect` ≈ `brewlet inspect`, and `brewlet:build`
produces the same OCI layout as `brewlet bundle`/`--store`. Use whichever fits your
pipeline — the resulting artifact is identical.

## Building the plugin

From the monorepo root:

```bash
mvn -f maven-plugin/pom.xml install
```

## Publishing the plugin to Maven Central

The tag release workflow calls
[Publish Maven Central](../.github/workflows/maven-central.yml) after creating the
GitHub release, explicitly enabling automatic publication. The publishing job
checks out the publishing configuration at the workflow's commit and the source
at the requested release tag into separate directories. It adds missing
developer/SCM metadata and applies the `central-release` profile to the tagged
POM, without replacing the tag's dependencies, ordinary build configuration, or
source files. This also supports older tags that predate Central publishing.

The release caller must set `secrets: inherit` on the reusable-workflow job.
The called job still selects the `maven-central` environment; without inheritance,
its environment secrets can resolve to empty values even when they are configured.
Keep the credentials in that environment rather than copying them to repository
secrets.

The job checks the specification version, runs the tagged plugin's tests and
notice checks, and sets the Maven version from the tag without its `v` prefix.
The release profile attaches sources and Javadoc and signs the JARs and POM with
PGP. Manual runs default to **validation only**: the Sonatype publisher uploads
the bundle, waits for **validated**, and leaves it unpublished in the Portal.
Only `publish=true` automatically publishes and waits for **published**. Either
mode fails on validation errors. Normal builds do not activate this profile.

### One-time maintainer setup

1. Verify the `sh.brewlet` namespace in the
   [Central Publisher Portal](https://central.sonatype.com/) and ensure the
   publishing account is authorized for it.
2. Create a passphrase-protected PGP signing key, or select an existing release
   signing key. Publish its **public** key to a
   [keyserver supported by Central](https://central.sonatype.org/publish/requirements/gpg/#distributing-your-public-key).
   Keep the private key and passphrase out of source control.
3. Create the GitHub environment **`maven-central`** in this repository.
   Configure required reviewers and restrict deployment refs to trusted release
   tags matching `v*` and trusted workflow branches used for manual backfills
   (for example `main`). The environment checks the workflow ref, not the tag
   passed as an input. Store these environment secrets:

   | Secret | Value |
   | --- | --- |
   | `MAVEN_CENTRAL_USERNAME` | The **User** from the Portal's generated publishing token, not your account login |
   | `MAVEN_CENTRAL_PASSWORD` | The **Password** from that same token |
   | `MAVEN_GPG_KEY` | The ASCII-armored exported private signing key, including its header and footer |
   | `MAVEN_GPG_PASSPHRASE` | The signing key's passphrase |

The Portal's `Username:Password (base64)` value is not needed; the publisher
constructs authentication from the two token fields. Do not paste credentials
into workflow files, POMs, issues, or logs.

The GitHub CLI can prompt for the token fields and passphrase without putting
their values in command history:

```bash
gh secret set MAVEN_CENTRAL_USERNAME --repo microsoft/brewlet --env maven-central
gh secret set MAVEN_CENTRAL_PASSWORD --repo microsoft/brewlet --env maven-central
gh secret set MAVEN_GPG_PASSPHRASE --repo microsoft/brewlet --env maven-central
```

Export only the intended signing key directly into the environment secret,
replacing `YOUR_SIGNING_KEY_FINGERPRINT` with its full fingerprint:

```bash
gpg --armor --export-secret-keys YOUR_SIGNING_KEY_FINGERPRINT |
  gh secret set MAVEN_GPG_KEY --repo microsoft/brewlet --env maven-central
```

The workflow uses the Maven GPG plugin's in-memory Java signer; it does not
import the private key into the runner's GnuPG keyring. The Central token and
signing secrets are exposed only to the signing/publishing step.

### Release and retry

Commit the publishing configuration and the matching specification version
before creating a new release tag. Tag names must be `vMAJOR.MINOR.PATCH`,
optionally followed by a prerelease suffix such as `-rc.1`. SNAPSHOT versions
and snapshot dependencies are rejected. Approve the `maven-central` deployment
when GitHub requests it.

For a Central-only run, use **Publish Maven Central** from the Actions UI.
Select a trusted ref containing the publishing workflow in **Use workflow from**,
enter the release tag in `tag`, and leave `publish` unchecked to validate without
publishing. After the workflow is on `main`, the CLI equivalent for `0.5.1` is:

```bash
gh workflow run maven-central.yml --repo microsoft/brewlet \
  --ref main -f tag=v0.5.1 -f publish=false
```

The tag must exist and its specification version must match, but it does not
need to contain the new workflow or publishing profile. Do not move an existing
tag or publish current development sources under an old version.

Open the deployment in the Central Publisher Portal after validation. It can
be published manually without rebuilding or deleted if this was only a test.
To automatically publish a new deployment instead, explicitly pass
`-f publish=true` to the dedicated workflow.

The publishing workflow must first be present on the default branch for GitHub
to allow manual dispatch. The general Release workflow's version-only manual
dispatch retains its existing behavior and does not submit to Central.

Central versions are immutable. If a job times out, inspect the deployment in
the Portal before retrying: it may already be published or still processing.
Never retry a published version or move its tag; use a new version for changed
artifacts. Older GitHub releases are not automatically backfilled into Central.

If the signing step fails its required-secret checks before Maven runs, no
Central submission was attempted. Verify the environment secrets and the caller's
`secrets: inherit`, then use the Central-only workflow above with the existing tag.
Rerunning the original tag workflow does not pick up fixes made after that tag.

The `central-release` profile itself defaults to `central.autoPublish=false`
and `central.waitUntil=validated`; the automated tag-release path explicitly
overrides both properties. Never use `publish=true` for a validation-only test.

CI tests the tagged-POM preparation, runs
`verify -Pcentral-release -Dgpg.skip=true`, checks the plugin, sources, and Javadoc
JARs and the `brewlet` prefix, and confirms that SNAPSHOT versions are rejected.
It never invokes the publishing phase and requires no secrets.

## License

[MIT](../LICENSE.txt). The published JAR also includes the applicable dependency
attributions at `META-INF/NOTICE.txt`.
