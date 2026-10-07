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
possible (entry mode, main class, runtime dependencies) from the project and
the built JAR. JDK selection, launcher, ports and probes belong to deployment
configuration, not the artifact config. The optional `manifest` goal generates
a development starting point for the image built in the current invocation.

- **Coordinates:** `sh.brewlet:brewlet-maven-plugin` on [Maven Central](https://central.sonatype.com/artifact/sh.brewlet/brewlet-maven-plugin)
- **Requires:** Maven 3.10+, JDK 17+ (to run the build). The `appcds`
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

For development, build and publish the application and generate its
`JavaApplication` YAML using the short `brewlet` prefix:

```bash
mvn clean package brewlet:push brewlet:manifest \
  -Dbrewlet.image=registry.example.com/team/app:1.4.2

# or derive the image (<registry>/${artifactId}:${version}) from a registry:
mvn clean package brewlet:push brewlet:manifest -Dbrewlet.registry=registry.example.com/team
```

Declaring the plugin does not bind `push` to the build lifecycle; it only enables
direct goal invocation. `mvn brewlet:push` alone requires an already packaged application.

These commands generate `target/brewlet/javaapplication.yaml` with the actual
published digest; there is no manual YAML or digest substitution step.
Review the runtime, ports and health probes, then deploy separately:

```bash
kubectl apply -f target/brewlet/javaapplication.yaml
```

See [Development manifest](#development-manifest) for configuration options and
the [developer workshop](../docs/workshops/developers.md) for a complete example.
For CI publication only, omit `brewlet:manifest` and hand the published digest
to the separate production deployment stage.

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
    <enablePreview>true</enablePreview>      <!-- artifact correctness flag -->
    <addOpens>
      <addOpen>java.base/java.lang=ALL-UNNAMED</addOpen>
    </addOpens>
    <systemProperties>
      <spring.aot.enabled>true</spring.aot.enabled>
    </systemProperties>
  </configuration>
  <executions>
    <execution>
      <goals><goal>config</goal><goal>push</goal></goals>
    </execution>
  </executions>
</plugin>
```

With that, `mvn deploy` publishes the runnable OCI image, prints its
digest-pinned `deploy image`, and records it in `target/brewlet/push.json`.
The Maven `deploy` lifecycle phase publishes artifacts; it does not deploy a
Kubernetes workload.

### Publish, then deploy separately

The plugin never invokes `kubectl` or needs cluster credentials. CI's
responsibility ends at publication. Publish once and
promote the same immutable image across environments without rebuilding:

```bash
az acr login -n myregistry     # or docker login; credential helpers are supported
mvn package brewlet:push -Dbrewlet.registry=myregistry.azurecr.io
jq -r '.deployImage' target/brewlet/push.json
```

Pass `deployImage` to a separately maintained `JavaApplication` or Deployment
manifest, then apply it with `kubectl`, Helm, or GitOps. The optional CLI command
`brewlet k8s app wait <appName> --namespace <ns>` waits for `Ready`;
`brewlet k8s app status <appName>` explains why an app is not ready.
See [Deploying workloads](../docs/deploying-workloads.md#deploy-a-published-image)
for a complete handoff example. Kubernetes requires `format=image`, not a native
`format=artifact` publication.

### Build/push ordering

`brewlet:build` and `brewlet:push` share one assembled image per project in the
same Maven invocation. The first goal assembles it; subsequent goals revalidate
the inputs and reuse the exact blobs and manifests, including their original
creation timestamp.

| Invocation after `mvn package` | Behavior |
|---|---|
| `brewlet:build brewlet:push brewlet:manifest` | Assemble locally, publish the same bytes, generate YAML for that digest. |
| `brewlet:push brewlet:build brewlet:manifest` | Assemble and publish, write the same bytes locally, generate YAML for that digest. |
| `brewlet:push brewlet:manifest` | Assemble and publish without requiring a prior local build. |
| `brewlet:build brewlet:manifest` | Assemble locally and generate YAML; warns that the image has not been published. |

Configure the same registry-qualified image reference for both goals. Changes
to the application/dependency/CDS contents or effective image configuration
between goals fail explicitly rather than creating a second image or publishing
stale bytes. Use separate Maven invocations for deliberately different builds.
Local writes preserve publication state, and a retry after a failed push reuses
the same assembly. Separate invocations do not reuse saved image bytes.

Both goals apply the same managed-dependency graph and provenance checks.
`build` does not publish, but selecting a remote dependency bundle may require
registry access to read and verify it. Use a local bundle layout for offline builds.

### Dry-run behavior

| Goal with `-Dbrewlet.dryRun=true` | Result |
|---|---|
| `brewlet:build` | Still assembles and writes the local OCI layout; never publishes. |
| `brewlet:push` | Validates and displays configuration; produces no current image result or publication, so it cannot feed `manifest`. |
| `brewlet:manifest` | Logs YAML for this invocation's built image; leaves any existing YAML unchanged. |

A dry-run push preserves existing `target/brewlet/push.json` byte-for-byte,
even if validation fails, and does not create one. That file still describes
the last successful push. A real push clears it after target-registry validation
and before preparing or publishing the application, preventing a failed push
from leaving a stale handoff.

For an offline manifest preview, use
`mvn package brewlet:build brewlet:manifest -Dbrewlet.dryRun=true`
with a registry-qualified build target.

### Development manifest

Explicitly invoke `brewlet:manifest` **after a successful** `brewlet:build` or `brewlet:push`
for the same project in the **same Maven invocation**:

```bash
mvn package brewlet:push brewlet:manifest \
  -Dbrewlet.registry=registry.example.com/team
kubectl apply -f target/brewlet/javaapplication.yaml
brewlet k8s app wait my-app --namespace default --wait-timeout 5m
```

Replace `my-app` with the project's artifact ID (or configured `appName`).
For offline generation, use `brewlet:build brewlet:manifest` instead; that does
not publish, so the exact built OCI image must be made available to the cluster
before applying the YAML. Configure a registry-qualified target on **build/push**
even for a local build. Native `format=artifact` output is not supported.

The manifest pins the actual current build's digest. It has **no `image`,
`registry`, or JAR input of its own** and never reads `push.json` or an old OCI
layout. A standalone `mvn brewlet:manifest` fails. Generation is optional,
not bound to a lifecycle phase, and never publishes, applies, or waits.

`target/brewlet/javaapplication.yaml` is a development starting point, not a
production configuration source of truth. These settings apply **only to manifest
generation**, not to the image:

| Setting | Property / configuration | Default |
|---|---|---|
| Name / namespace / replicas | `brewlet.appName`, `brewlet.namespace`, `brewlet.replicas` | artifact ID / `default` / `1` |
| Runtime | `brewlet.jdkFeature`, `brewlet.jdkDistribution`, `brewlet.launcher` | inferred feature / any distribution / `java` |
| CPU request / limit | `brewlet.resources.cpuRequest`, `brewlet.resources.cpuLimit` | `500m` / `2` |
| Memory request / limit | `brewlet.resources.memoryRequest`, `brewlet.resources.memoryLimit` | `256Mi` / `512Mi` |
| JVM tuning | `<jvmArgs><jvmArg>...</jvmArg></jvmArgs>` | none |
| Ports | `<ports><port><name>http</name><containerPort>8080</containerPort></port></ports>` | `8080/http` for Spring Boot or Quarkus, with a warning; otherwise none |
| Health probes | `<probes>` or `brewlet.readinessPath` / `brewlet.livenessPath` | none; never inferred |

For example, add development defaults directly to the plugin's
`<configuration>` and invoke `brewlet:manifest` after build/push:

```xml
<configuration>
  <ports>
    <port><name>http</name><containerPort>8080</containerPort></port>
  </ports>
  <probes>
    <readiness><path>/healthz</path></readiness>
  </probes>
</configuration>
```

Use only health endpoints the application actually exposes. Each probe uses
HTTP GET when `<path>` is set, exec when `<command><arg>...</arg></command>` is
set, or TCP otherwise. HTTP/TCP probes use `<port>` (a declared name or number),
defaulting to the first configured port. Optional timing fields are
`initialDelaySeconds`, `periodSeconds`, `timeoutSeconds`, and `failureThreshold`.
Ports alone do not establish readiness. `brewlet.skip=true` skips generation.
For output preview, see [Dry-run behavior](#dry-run-behavior).

---

## Goals

| Goal | Default phase | What it does |
|---|---|---|
| `brewlet:config` | `package` | Generate `target/brewlet/jvm-config.json` from POM metadata + the JAR manifest. Input for `build`/`push`. |
| `brewlet:build` | — | Assemble the OCI artifact into a local **OCI image-layout** dir (`target/brewlet/oci`) without pushing. Good for inspection, air-gapped flows, or local-registry tests. |
| `brewlet:push` | `deploy` | Build and push to the registry in `<image>`. By default (`image` format) this pushes a **runnable OCI image** — a standard, kubelet-pullable image (see [Delivery format](#delivery-format-native-artifact-vs-runnable-image)). With `-Dbrewlet.format=artifact` it pushes the native Brewlet artifact instead (JAR layer + launch-config blob + manifest with `artifactType: application/vnd.brewlet.app.v1+json`). |
| `brewlet:appcds` | — | Generate a dynamic AppCDS archive (`target/brewlet/app.jsa`) from the same fat/thin/Boot/module payload used for publication, using a self-terminating run or explicit signal-mode training. Attach it later with `-Dbrewlet.cdsArchive=...`. |
| `brewlet:dependency-bundle` | `package` | Resolve the runtime dependency closure, create a canonical lock and deterministic flat classpath tar, write `target/brewlet/dependency-bundle-oci`, and publish an OCI dependency bundle. |
| `brewlet:inspect` | — | Print the fully-resolved launch config and OCI descriptor that *would* be pushed — a dry run to verify inference. Honors `brewlet.cdsArchive` exactly like `build`/`push` (`cds` block + CDS layer with digest). |
| `brewlet:manifest` | — | Generate a development `JavaApplication` YAML for this invocation's built/pushed image. No independent image input, publication, or cluster access. |
| `brewlet:help` | — | List goals and parameters; use `-Ddetail=true -Dgoal=push` for detailed publishing help. |

Run any goal directly, e.g. `mvn brewlet:inspect`.

---

## Configuration parameters

All parameters are optional unless noted; most have a `-Dbrewlet.*` command-line
property. Values configured in `<configuration>` and CLI properties can be mixed.

### Core

Publishing rejects malformed repository paths and tags before preparing
artifacts, including uppercase repository components, empty tags, and embedded
URL schemes or query strings in `image`. Docker Hub references such as
`docker.io/alpine:3` and `index.docker.io/alpine:3` use the registry API endpoint
`registry-1.docker.io` and repository `library/alpine`.

| Parameter | Property | Default | Notes |
|---|---|---|---|
| `image` | `brewlet.image` | `<registry>/${project.artifactId}:${project.version}` | Target OCI ref, e.g. `registry.example.com/team/app:1.4.2`. `push` rejects refs without a registry host instead of defaulting to Docker Hub (use `docker.io/<user>/app` to target Docker Hub). Publishing requires a mutable tag (implicit `latest` when omitted), not `repo@digest` or `repo:tag@digest`, even in dry-run mode. |
| `registry` | `brewlet.registry` | — | Registry (optionally with a repository prefix, e.g. `registry.example.com/team`) used to derive `image` when it is not set. |
| `format` | `brewlet.format` | `image` | Delivery format for `push`: `image` (runnable, kubelet-pullable OCI image — the default) or `artifact` (native Brewlet OCI artifact). See [Delivery format](#delivery-format-native-artifact-vs-runnable-image). |
| `jarFile` | `brewlet.jarFile` | project's primary artifact | Path to the application JAR to publish. After a separate `mvn package`, standard unclassified `jar`/`maven-plugin` projects can use `${project.build.directory}/${project.build.finalName}.jar`. Custom packaging, classifier, or JAR-plugin output overrides require an explicit `jarFile`; the plugin never searches for a newest or arbitrary JAR. |
| `mainClass` | `brewlet.mainClass` | inferred from `Main-Class` | Main class to launch. Does **not** by itself set the entry mode — the mode is inferred from the JAR's shape (see `entryMode`). Used in `classpath` mode (the class launched via `-cp`) and optionally in `module` mode (selects `<module>/<mainClass>`); ignored in `jar` mode (uses the manifest's `Main-Class`). |
| `entryMode` | `brewlet.entryMode` | inferred from manifest | `jar`, `classpath`, or `module` (auto-detected for modular JARs with a root `module-info.class`). |
| `outputDirectory` | — | `${project.build.directory}/brewlet` | Where generated files land. |
| `skip` | `brewlet.skip` | `false` | Skip all Brewlet goals. |
| `dryRun` | `brewlet.dryRun` | `false` | Goal-specific preview; see [Dry-run behavior](#dry-run-behavior). |
| `layered` | `brewlet.layered` | `false` | **Layered deployment.** Plain thin JARs use the resolved POM runtime dependencies and `entry.classPath=[mainJar, "lib/*"]`; standard Spring Boot executable JARs are unpacked into a thin application JAR plus their exact packaged libraries and an explicitly ordered classpath (see below). Modular JARs use dependency modules at `/app/mods` and `entry.modulePath=[mainJar, "mods"]`. Non-modular layering selects `classpath` mode. Unchanged dependency layers dedup by digest. |
| `splitSnapshotLayers` | `brewlet.splitSnapshotLayers` | `true` | When `layered`, pack released deps and `-SNAPSHOT` deps into separate `deps` / `snapshot-deps` layers (stable→volatile) for finer dedup. |
| `dependencyBundle` | `brewlet.dependencyBundle` | — | For `build` and `push`, a registry reference or local OCI-layout directory containing a managed dependency bundle. The resolved runtime graph must exactly match its lock. Forces thin-JAR classpath launch and requires `mainClass`. |
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

Passwords in the matching Maven `settings.xml` server may be plaintext or
encrypted with Maven's password-encryption tooling. The plugin uses Maven's
settings decrypter and `settings-security.xml` configuration for publishing
and managed dependency-bundle registry access. Decryption errors fail the goal with an
actionable message, without logging credentials or falling back to a different
credential source.

Registry credentials are resolved from, in order: a `settings.xml` `<server>`
whose `<id>` is the registry host and whose username is configured; the Docker
config (`$DOCKER_CONFIG/config.json` or `~/.docker/config.json`); and finally
`BREWLET_REGISTRY_USERNAME` / `BREWLET_REGISTRY_PASSWORD`. If none supplies
credentials, access is anonymous.

Within Docker config, a matching per-registry `credHelpers` entry with a nonempty
helper name is authoritative: inline `auths` and the default `credsStore` are
not tried, even if that helper fails or returns no usable credentials. In that
case, resolution continues with the Brewlet environment variables, then anonymous
access — the helper is not final across all credential sources. Without a
matching nonempty helper entry, the plugin tries inline `auths` (`identitytoken`
before `auth`), then the default `credsStore` if no usable inline credentials are
found. Helpers run as `docker-credential-<name> get` (e.g. `osxkeychain`,
`desktop`, `wincred`).

So after `docker login` or `az acr login` no further setup is needed; identity
tokens are exchanged with an OAuth2 refresh-token grant. The plugin keeps
credentials scoped to the registry you configured:

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
  forwarding your credentials. Token exchanges never follow redirects, even
  from a trusted realm: a redirect could otherwise replay an identity-token
  request body at another origin.

Insecure registry entries match the configured authority; a bare `host` entry
also permits `host:80`. Other ports must be listed explicitly.
Registry redirects also obey this plaintext policy and
never carry registry credentials to a different origin.

| Parameter | Property | Default | Notes |
|---|---|---|---|
| `insecureRegistries` | `brewlet.insecureRegistries` | *(empty)* | Exact registry authorities (`host` or `host:port`) that may be contacted over plain HTTP. Loopback registries are always allowed, so this is only needed for a non-loopback HTTP registry such as an in-cluster mirror. Configure as `<insecureRegistries><insecureRegistry>registry.internal:5000</insecureRegistry></insecureRegistries>` or `-Dbrewlet.insecureRegistries=registry.internal:5000`. |
| `allowedTokenRealms` | `brewlet.allowedTokenRealms` | *(empty)* | Exact authorities (`host` or `host:port`) trusted to receive this build's registry credentials when the authentication challenge realm is **not** the registry's own origin. Docker Hub (`auth.docker.io`) is trusted automatically; add an entry only when you trust that host with your registry credentials. |

### Managed-dependency JDK compatibility

This build-time setting checks the application's JDK feature against a managed
dependency bundle's declared compatibility. The optional development manifest
also uses it for `spec.jvm.version`. It is **not** serialized into
`target/brewlet/jvm-config.json`.

| Parameter | Property | Default | Notes |
|---|---|---|---|
| `jdkFeature` | `brewlet.jdkFeature` | inferred from the main compiler configuration or toolchain | Positive application JDK feature used when the dependency bundle declares compatible JDKs; an explicit value overrides inference. See the precedence below. |

### JDK inference

The plugin resolves an **application JDK feature for compatibility checks**, not a measurement of
the running Pods or a proof of the application's minimum compatible JVM:

1. A positive explicit `<jdkFeature>` / `-Dbrewlet.jdkFeature` wins.
2. Effective main compiler settings use `release`, then `target`, then `source`.
   Inherited settings, active profiles, and main compile executions participate.
   Literal compiler XML takes precedence over its property counterpart;
   `${...}` expressions are evaluated with Maven's property semantics.
   Disabled executions and test-only compiler settings do not determine the
   application request.
3. Without a declared level, use the compiler's `jdkToolchain`, then a suitable
   session-selected toolchain, then main-bound `maven-toolchains-plugin:toolchain`
   requirements. Configured matching follows Maven's first-match order,
   including resolvable version ranges; it does not choose the highest or lowest
   installed JDK. The selected feature is checked against the JDK's `release`
   metadata.
4. Only when no other compiler authority is configured, use the in-process
   Maven JDK as a fallback. Implicit defaults from every compiler-plugin version
   are not emulated.

For example, a main compiler `<release>17</release>` resolves to JDK 17 even when
Maven or its compiler toolchain runs on JDK 21. A literal `<release>17</release>`
also remains authoritative over an unrelated `maven.compiler.release=21`
property; reference that property in the XML if it is intended to control the
build.

Inference fails with explicit-override guidance for unresolved or malformed
values, differing main compilation levels, unavailable toolchains, unsupported
compiler/executable choices, or opaque arguments that could change the target.
Unknown or malformed selected-JDK versions never default to Java 17 or the
Maven JVM. `jdkFeature` uses only a major (feature) version, such as
`17` or `21`, and does not accept patch versions,
build numbers, or early-access/vendor suffixes. Full versions read from external
JDK metadata are used only to infer the major version: for example,
`21.0.8+9-LTS` becomes `21` and the historical Java 8 version string `1.8.0_391`
becomes `8`. User-configured feature versions remain integers: Java 25 is `25`,
not `1.25`.
Standalone automatic toolchain discovery and a selection that might belong
only to test or later build phases are not guessed. Set `brewlet.jdkFeature`
after reviewing those builds rather than relying on the Maven JVM by accident.
An execution bound to `compile` can still run after the main compiler in that
same phase; selections whose ordering cannot establish main-compiler authority
also require an explicit request.
Failures occur before publishing an incompatible image, and logs identify the
compiler setting or fallback toolchain used.

This resolution is used when managed-dependency assembly checks
the bundle's compatible JDK features. It remains separate from the artifact's
launch configuration and does not install or upgrade a node JDK.

### Runtime shape

| Parameter | Notes |
|---|---|
| `enablePreview` (`brewlet.enablePreview`) | App-intrinsic artifact knob; writes `enablePreview` and expands to `--enable-preview`. |
| `addModules` / `addOpens` / `addExports` | App-intrinsic artifact lists for JPMS/module access; configure with `<addModule>`, `<addOpen>`, and `<addExport>` entries. |
| `systemProperties` | App-intrinsic artifact map expanded as sorted `-D<key>=<value>` flags. |
| `env` | `<envVar>` entries (`name`, `value`). |

Ports, probes and JVM resource tuning are not artifact fields. Development
defaults can be supplied to `manifest`; production values belong in maintained
deployment configuration. Process UID/GID is also excluded: Kubernetes
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

The destination `dependencyBundleImage` (or fallback `image`) must use a mutable
tag, with implicit `latest` when omitted. Digest-pinned destinations, including
`repo:tag@digest`, are rejected before writing the layout, even in dry-run mode.

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

The source `dependencyBundle` accepts a tag, a digest-pinned registry reference
(`repo@sha256:...` or `repo:tag@sha256:...`), or a local OCI-layout directory. The plugin
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
If the destination already has the blob, no mount or upload is needed. A
successful mount (`201`) transfers no layer body; a declined mount (`202`)
completes the upload session at the registry's returned `Location`, preserving
its query parameters. Registries that reject the mount operation with `400`,
`404`, or `405` use a fresh blob upload instead. Authentication and server
errors abort publication rather than being treated as missing blobs or
successful mounts.
Native artifacts use the custom `application/vnd.brewlet.classpath.layer.v1+tar`
media type for dependency tars in current local OCI-layout, CLI, and
`prepare-bundle` workflows. Brewlet unpacks these tars during sandbox assembly;
container runtimes cannot unpack this custom media type as a runnable image
layer. Managed dependency bundles and runnable images instead use the standard
gzip layer described above.

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

---

## Delivery format: native artifact vs runnable image

`brewlet:push` can publish in two current formats, selected by `format` /
`-Dbrewlet.format`. Both share the current launch contract and sandbox assembly.

| Format | What it publishes | When a pod names it as `image:` |
|---|---|---|
| `image` (default) | A **runnable OCI image**: your JAR (+ dependency/module/CDS payload) packaged as standard `tar+gzip` layers with a real OCI image config and a multi-arch index. The Brewlet launch contract rides in the `brewlet.sh/jvm-config` manifest annotation. | kubelet/containerd pull and unpack it like any image. A `runtimeClassName: brewlet` pod uses the digest-pinned reference printed by `brewlet:push`; tag-only execution is rejected. |
| `artifact` | The **native artifact**: your raw JAR, optional dependency/module/CDS layers, and a launch-config blob under Brewlet [media types](https://github.com/microsoft/brewlet/blob/main/docs/reference.md#oci-media-types) (`artifactType: application/vnd.brewlet.app.v1+json`). Used by local OCI-layout, CLI, and `prepare-bundle` workflows. | kubelet/containerd **cannot** unpack the custom layer media types, so a pod `image:` reference fails with `ImagePullBackOff`. Kubernetes execution rejects native artifacts even if the blobs are already on the node; use a runnable image for Kubernetes workloads. |

```bash
# The default push already produces a runnable, kubelet-pullable image:
mvn clean package brewlet:push \
  -Dbrewlet.image=registry.example.com/team/orders:1.4.2

# Publish a native artifact for local OCI-layout / CLI / prepare-bundle workflows:
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
- `target/brewlet/push.json` — the last successful push (`image`, `digest`,
  `deployImage`, `format`), for downstream deployment tooling.
- `target/brewlet/javaapplication.yaml` — optional development manifest for the
  current build (`brewlet:manifest`); never automatically applied.

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
source files.

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
publishing. For the CLI equivalent, set `RELEASE_TAG` to the existing release
tag you intend to validate:

```bash
gh workflow run maven-central.yml --repo microsoft/brewlet \
  --ref main -f tag="${RELEASE_TAG:?Set RELEASE_TAG to the release tag to validate}" \
  -f publish=false
```

The tag must exist and its specification version must match, but it does not
need to contain the publishing workflow or profile. Do not move an existing
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
