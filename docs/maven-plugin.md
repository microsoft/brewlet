# Maven plugin reference

The optional `sh.brewlet:brewlet-maven-plugin` packages a Java project's
application JAR and resolved dependencies as Brewlet application layers. It
invokes the language-neutral Brewlet CLI; it does not install a runtime,
select a Pod runtime image, generate Kubernetes manifests or deploy workloads.
It handles JAR projects only; other Java artifacts, such as a WAR or EAR for a
platform-provided application server, are packaged with the generic CLI. For a
walkthrough and framework-specific alternatives, see
[Java applications](java.md).

## Requirements

- Java 17 or later to run Maven and the plugin.
- A Brewlet CLI matching the plugin version, available on `PATH` or specified
  with `brewlet.executable` for the `build` and `push` goals.
- A packaged project JAR and explicit image reference and platforms for `build`.
- Registry credentials, when the registry requires them, in the environment
  (see [Registry access](#registry-access)).

The version in `release.json` does not by itself establish that the plugin
has been published. If a matching release is not available, build and install
the plugin from matching source:

```sh
mvn -f maven-plugin/pom.xml install
```

Use the plugin version matching the CLI; the examples below use `0.13.1`,
which includes the `build` goal introduced in `0.12.0`.

## Goals

| Goal | Purpose |
| --- | --- |
| `build` | Prepare the Java layers and launch descriptor and run `brewlet build` to write a local OCI layout. Never contacts a registry. Requires runtime dependency resolution; its default lifecycle phase is `package` when configured in an execution. |
| `push` | Run `brewlet push` to upload the layout written by `build`, byte for byte. Does not rebuild; fails if no build exists. Its default lifecycle phase is `deploy` when configured in an execution. |
| `conformance` | Evaluate shared artifact fixtures with the plugin's independent Java implementation. Does not require the Brewlet CLI. |

The `build` and `push` goals mirror the CLI's
[`build` and `push`](cli-reference.md#build-and-publish) commands. Package,
build and publish in one Maven command:

```sh
mvn package sh.brewlet:brewlet-maven-plugin:0.13.1:build \
  sh.brewlet:brewlet-maven-plugin:0.13.1:push \
  -Dbrewlet.image=registry.example.com/apps/orders:v2 \
  -Dbrewlet.platforms=linux/amd64,linux/arm64 \
  -Dbrewlet.mainClass=com.example.Main
```

Omit `push` to build only. Both goals use the OCI layout
`target/brewlet/oci` unless `store` names another one, so `mvn clean` removes
it. Pass `--store target/brewlet/oci` when you use that build with the CLI,
for example with `brewlet run` or `brewlet inspect`.

### `build`

Package the project before invoking the goal in the same Maven command:

```sh
mvn package sh.brewlet:brewlet-maven-plugin:0.13.1:build \
  -Dbrewlet.image=registry.example.com/apps/orders:v2 \
  -Dbrewlet.platforms=linux/amd64,linux/arm64 \
  -Dbrewlet.mainClass=com.example.Main
```

The goal places the project JAR at `/app/app.jar` and resolved compile and
runtime dependencies in a separate layer at `/app/lib`. With `module`, the
dependency directory is `/app/mods` instead. Inputs must be regular files,
not symlinks; duplicate dependency filenames and unresolved dependencies fail
the build.

Unless `launch` is supplied, the plugin generates a descriptor with `java`
as the `command` and `/app` as the working directory. It uses `mainClass`
or, if omitted, the JAR's `Main-Class` manifest attribute. For a classpath
application the arguments include `--class-path /app/app.jar:/app/lib/*`
followed by the main class. With `module`, they instead include
`--module-path /app/app.jar:/app/mods --module MODULE[/MAINCLASS]`.
At least a main class, a module or an explicit launch descriptor is required.

#### Configuration

Set scalar options in the POM or with the listed Maven properties. The list
option `javaArgs` is POM configuration, not a `-D` property.

| POM configuration | Maven property | Optional | Default | Meaning |
| --- | --- | --- | --- | --- |
| `image` | `brewlet.image` | No | None | Image reference. |
| `platforms` | `brewlet.platforms` | No | None | Explicit platform list, such as `linux/amd64,linux/arm64`. |
| `executable` | `brewlet.executable` | Yes | `brewlet` | CLI executable or path. |
| `store` | `brewlet.store` | Yes | `target/brewlet/oci` | Output OCI layout directory. |
| `launch` | `brewlet.launch` | Yes | Generated descriptor | Path to an authoritative version-1 launch descriptor. The plugin validates it but does not rewrite it. |
| `mainClass` | `brewlet.mainClass` | Yes | JAR's `Main-Class`, if present | Main class for the generated descriptor. |
| `module` | `brewlet.module` | Yes | None | JPMS module name; also places dependencies under `/app/mods`. |
| `javaArgs` | — | Yes | Empty list | Ordered list of JVM arguments placed before the generated classpath or module arguments. |
| `appCdsArchive` | `brewlet.appCdsArchive` | Yes | None | Path to a prebuilt AppCDS archive to package as `/app/app.jsa`. |

For example, configure lists and the compiler target in the project POM:

```xml
<properties>
  <maven.compiler.release>21</maven.compiler.release>
</properties>
<build>
  <plugins>
    <plugin>
      <groupId>sh.brewlet</groupId>
      <artifactId>brewlet-maven-plugin</artifactId>
      <version>0.13.1</version>
      <configuration>
        <javaArgs>
          <javaArg>-Xmx512m</javaArg>
        </javaArgs>
      </configuration>
    </plugin>
  </plugins>
</build>
```

An explicit `launch` overrides generated launch configuration, including
`mainClass` and `javaArgs`. The `module` option still
determines the dependency directory; make the descriptor match that layout.
The Pod must select an approved, compatible runtime ID.

For AppCDS, the plugin preserves the archive's modification time and adds
`-XX:SharedArchiveFile=/app/app.jsa -Xshare:auto` after `javaArgs` in the
generated descriptor. A supplied `launch` must reference the archive itself.
Runtime rotation can change the JDK build, so archive use is best-effort.

### `push`

```sh
mvn sh.brewlet:brewlet-maven-plugin:0.13.1:push \
  -Dbrewlet.image=registry.example.com/apps/orders:v2
```

| POM configuration | Maven property | Optional | Default | Meaning |
| --- | --- | --- | --- | --- |
| `image` | `brewlet.image` | No | None | Image reference to upload, which must be the one `build` used. |
| `executable` | `brewlet.executable` | Yes | `brewlet` | CLI executable or path. |
| `store` | `brewlet.store` | Yes | `target/brewlet/oci` | Source OCI layout directory, normally the one `build` wrote. |

`push` verifies the layout and copies its bytes unchanged, so the pushed
digest equals the built digest. It never rebuilds, and it fails when the
layout holds no build for `image` rather than uploading another reference's
build under that tag.

#### Registry access

Only the `push` goal contacts a registry. The plugin has no credential or TLS
options, because `push` runs `brewlet push` as a child process that inherits
Maven's environment, so you configure registry access with the same environment variables as the
[CLI](cli-reference.md#registry-access):

| Variable | Meaning |
| --- | --- |
| `BREWLET_REGISTRY_USERNAME` | Username for registry basic or token authentication. |
| `BREWLET_REGISTRY_PASSWORD` | Password or access token. Set it together with the username. |
| `SSL_CERT_FILE` | PEM file of additional CA certificates trusted for the registry, added to the system trust store. Use it for a registry with a private or self-signed CA. |

Set them in the shell or CI step that runs Maven:

```sh
export BREWLET_REGISTRY_USERNAME=ci-bot
export BREWLET_REGISTRY_PASSWORD="$REGISTRY_TOKEN"
export SSL_CERT_FILE=/etc/ssl/private-registry-ca.pem   # only for a private CA
mvn package sh.brewlet:brewlet-maven-plugin:0.13.1:build \
  sh.brewlet:brewlet-maven-plugin:0.13.1:push \
  -Dbrewlet.image=registry.example.com/apps/orders:v2 \
  -Dbrewlet.platforms=linux/amd64,linux/arm64 \
  -Dbrewlet.mainClass=com.example.Main
```

Credentials are not read from the POM, Maven `settings.xml` server entries or
Docker credential helpers. Supply them from your CI secret store and keep
them out of source control; see
[Registry access and certificate trust](registry-trust.md#producer-access).

Registries on `localhost` or `127.0.0.1` are reached over plain HTTP; all
other registries use HTTPS. The plugin cannot pass the CLI's `--insecure` or
`--registry-scheme` flags. To push to a plain-HTTP registry on another host,
or to a loopback registry over TLS, run only `build` and then
`brewlet push --store target/brewlet/oci` with those flags yourself.

A rejected login or an untrusted certificate fails the Maven build, and no
tag is written. The `build` goal does not contact a registry and needs none
of these variables.

### `conformance`

```sh
mvn sh.brewlet:brewlet-maven-plugin:0.13.1:conformance \
  -Dbrewlet.fixtures=/path/to/conformance/artifact-v1.json \
  -Dbrewlet.conformanceResult=/path/to/results.json
```

| POM configuration | Maven property | Optional | Default | Meaning |
| --- | --- | --- | --- | --- |
| `fixtures` | `brewlet.fixtures` | No | None | Path to the shared fixture file. |
| `result` | `brewlet.conformanceResult` | No | None | Output path for normalized results. |

Every fixture must pass before the result is written. A failing case fails
Maven without replacing an existing result file.
