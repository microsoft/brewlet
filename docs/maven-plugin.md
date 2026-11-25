# Maven plugin reference

The optional `sh.brewlet:brewlet-maven-plugin` packages a Java project's
application JAR and resolved dependencies as Brewlet application layers. It
invokes the language-neutral Brewlet CLI; it does not install a runtime,
select a Pod runtime image, generate Kubernetes manifests or deploy workloads.
For a walkthrough and framework-specific alternatives, see
[Java applications](java.md).

## Requirements

- Java 17 or later to run Maven and the plugin.
- A Brewlet CLI matching the plugin version, available on `PATH` or specified
  with `brewlet.executable` for the `push` goal.
- A packaged project JAR and explicit image reference and platforms for `push`.
- Registry credentials, when the registry requires them, in the environment
  (see [Registry access](#registry-access)).

The version in `release.json` is a candidate, not a claim that the plugin
has been published. Until a qualified prerelease is available, build and
install the plugin from matching source:

```sh
mvn -f maven-plugin/pom.xml install
```

Use the plugin version matching the CLI. The examples below use `0.11.0`.

## Goals

| Goal | Purpose |
| --- | --- |
| `push` | Prepare the Java layers and launch descriptor, build an OCI layout and, unless `store` is set, push it to a registry. Requires runtime dependency resolution; its default lifecycle phase is `package` when configured in an execution. |
| `conformance` | Evaluate shared artifact fixtures with the plugin's independent Java implementation. Does not require the Brewlet CLI. |

### `push`

Package the project before invoking the goal in the same Maven command:

```sh
mvn package sh.brewlet:brewlet-maven-plugin:0.11.0:push \
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
| `store` | `brewlet.store` | Yes | Generated layout in the project build directory; push to registry | OCI layout directory. When set, build locally without pushing. |
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
      <version>0.11.0</version>
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

#### Registry access

The plugin has no credential or TLS options. When it pushes, it runs
`brewlet push` as a child process that inherits Maven's environment, so you
configure registry access with the same environment variables as the
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
mvn package sh.brewlet:brewlet-maven-plugin:0.11.0:push \
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
or to a loopback registry over TLS, build with `store` and run
`brewlet push` with those flags yourself.

A rejected login or an untrusted certificate fails the Maven build, and no
tag is written. With `store` set, the plugin does not contact a registry and
needs none of these variables.

### `conformance`

```sh
mvn sh.brewlet:brewlet-maven-plugin:0.11.0:conformance \
  -Dbrewlet.fixtures=/path/to/conformance/artifact-v1.json \
  -Dbrewlet.conformanceResult=/path/to/results.json
```

| POM configuration | Maven property | Optional | Default | Meaning |
| --- | --- | --- | --- | --- |
| `fixtures` | `brewlet.fixtures` | No | None | Path to the shared fixture file. |
| `result` | `brewlet.conformanceResult` | No | None | Output path for normalized results. |

Every fixture must pass before the result is written. A failing case fails
Maven without replacing an existing result file.
