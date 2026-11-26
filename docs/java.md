# Java workloads

This guide explains how to adapt an existing Java application, such as a Spring
Boot or Quarkus service built with Maven or Gradle or a WAR or EAR deployed to
an application server, so that it runs on a platform-managed Java runtime. It
is a reference, not a script to run from top to bottom. Copy the snippets into
your project and change the values marked in each section.

## How it works

A container image normally bundles the JDK, the application and its libraries.
Brewlet splits ownership along a different line:

| Concern | Owner | Where it lives |
| --- | --- | --- |
| Java runtime (JDK/JRE or custom `jlink` image, CA certificates, OS libraries) and, optionally, an application server such as Tomcat | Platform team | An approved [runtime image](runtime-images.md), selected by ID |
| Shared libraries the platform chooses to standardize (optional) | Platform team | The same runtime image, as linked modules or JARs |
| Application files in whatever form the launcher expects (JARs, WARs, EARs, class directories, resources) and every other library | Application team | Application artifact, mounted read-only at `/app` |
| How the JVM starts (main class, `-jar`, JVM options, arguments) | Application team | `launch.json` in the application artifact |
| Replicas, resources, probes, config, Secrets, writable volumes | Application team | Kubernetes manifests |

Three consequences shape everything else in this guide:

- **Maven or Gradle still builds the application.** Brewlet only packages the
  files your build produced, plus an explicit launch descriptor. It does not
  resolve dependencies, read `pom.xml`/`build.gradle`, discover a main class,
  unpack nested JARs or interpret WARs. The application artifact holds ordinary
  files, so it is not limited to a JAR, let alone a fat JAR.
- **Each library lives on one side of the line.** By default the application
  ships its libraries in `/app`. A platform team can instead move selected
  libraries into the runtime image, for example as JPMS modules linked into a
  custom `jlink` runtime. A runtime rotation then updates those libraries with
  the JDK, without rebuilding applications. Libraries left in `/app` are
  updated only by rebuilding and republishing the application. See
  [libraries provided by the runtime image](#libraries-provided-by-the-runtime-image).
- **Runtime updates start new JVMs.** A runtime rotation never patches a running
  process. New Pods start your unchanged artifact on the newer runtime image,
  so the application must stay compatible with the Java feature release the
  platform promises for that runtime ID.

See the [architecture overview](concepts.md) and the
[launch contract](reference.md#launch-descriptor) for the complete model.

## Before you start

Ask your platform team for:

- **Runtime ID**, for example `java-21`. IDs are chosen by administrators;
  Brewlet has no built-in catalog.
- **Java feature release** the ID guarantees (17, 21, 25, ...). Compile with
  `--release` at or below it.
- **Libraries the runtime image provides**, if any, with their versions and
  whether they are linked modules or classpath JARs.
- **Absolute path of the `java` executable** in that runtime image. Distroless
  images commonly use `/usr/bin/java`; Eclipse Temurin images use
  `/opt/java/openjdk/bin/java`. The examples below use `/usr/bin/java`.
- **Runtime image `ENTRYPOINT` and `CMD`**. Brewlet composes them with your
  descriptor like a Dockerfile built `FROM` the runtime image: argv is the
  runtime `ENTRYPOINT` followed by your `command` and `args`. The examples
  below assume no `ENTRYPOINT`. Distroless Java's `ENTRYPOINT` is
  `/usr/bin/java -jar`; with it, omit `command`, use `"args": ["/app/app.jar"]`
  and pass JVM options through `JDK_JAVA_OPTIONS`.
- **Application server**, if the runtime image provides one (for example
  Tomcat), and its installation path.
- **Supported platforms** (`linux/amd64`, `linux/arm64`).
- **Approved mount locations** outside `/app`, such as `/tmp`, if any (see
  [writable files](#writable-files)).

You also need the `brewlet` CLI. Spring Boot 3 and Quarkus 3 both require
Java 17 or later.

## Choose a packaging strategy

The key decision is which files go into `/app` and which `java` command starts
them. Brewlet does not require a particular Java archive format: an executable
JAR, a thin JAR with a dependency directory, a framework's own directory layout
and a WAR served by an application server in the runtime image all package the
same way. Framework build plugins already produce a runnable layout; you
package that layout unchanged and write a launch descriptor that matches it.

| Application format | What you package | Launch command |
| --- | --- | --- |
| [Spring Boot executable JAR](#spring-boot) | The single repackaged JAR | `java -jar /app/app.jar` |
| [Spring Boot extracted JAR](#extracted-layout-recommended-for-production) | `app.jar` plus its `lib/` directory | `java -jar /app/app.jar` |
| [Quarkus fast-jar](#quarkus) (default) | The **whole** `quarkus-app/` directory | `java -jar /app/quarkus-run.jar` |
| Quarkus uber-jar | The single `*-runner.jar` | `java -jar /app/app.jar` |
| [Plain Java thin JAR](#plain-java-applications-and-the-maven-plugin) | Application JAR plus dependency JARs | `java --class-path '/app/app.jar:/app/lib/*' com.example.Main` |
| [WAR or EAR on a platform-provided application server](#war-and-ear-applications-on-a-platform-provided-application-server) | The `.war` or `.ear`, plus any server configuration the platform delegates to the application | The server's own bootstrap class from the runtime image, for example `java -classpath /usr/local/tomcat/bin/bootstrap.jar:... org.apache.catalina.startup.Bootstrap start` |

Two mistakes cause most first-run failures:

- **Copying only `quarkus-run.jar`.** It is a small bootstrap JAR whose
  manifest points at `lib/`, `app/` and `quarkus/` next to it. Without those
  directories the JVM fails with `ClassNotFoundException` or
  `NoSuchFileException`.
- **Putting a Spring Boot fat JAR on a classpath.** A repackaged Spring Boot
  JAR keeps your classes in `BOOT-INF/classes` and libraries in
  `BOOT-INF/lib`. Only Spring Boot's own launcher understands that format, so
  start it with `-jar` (or extract it first). `--class-path app.jar
  com.example.Application` fails with `ClassNotFoundException`.

Splitting files into more than one layer is optional. Libraries change less
often than your code, so publishing them as a separate layer lets registries
and nodes reuse them between releases. Start with one layer and split once
the basic deployment works.

Do not package a JDK, a `jlink` image or a container base image in `/app`.
A custom Java runtime, including one with linked libraries, belongs in the
runtime image (see
[libraries provided by the runtime image](#libraries-provided-by-the-runtime-image)).

## Spring Boot

### Executable JAR

The Spring Boot build plugins produce an executable JAR that already contains
every dependency. Give it a stable file name so the launch descriptor does not
change with each version.

=== "Maven"

    ```xml
    <!-- pom.xml -->
    <properties>
      <java.version>21</java.version> <!-- not above the runtime's release -->
    </properties>

    <build>
      <finalName>app</finalName> <!-- produces target/app.jar -->
      <plugins>
        <plugin>
          <groupId>org.springframework.boot</groupId>
          <artifactId>spring-boot-maven-plugin</artifactId>
        </plugin>
      </plugins>
    </build>
    ```

    ```sh
    ./mvnw -B package
    # target/app.jar
    ```

=== "Gradle (Kotlin DSL)"

    ```kotlin
    // build.gradle.kts
    java {
        toolchain { languageVersion = JavaLanguageVersion.of(21) }
    }

    tasks.bootJar {
        archiveFileName = "app.jar" // produces build/libs/app.jar
    }

    // Optional: stop Gradle from also producing app-plain.jar.
    tasks.jar {
        enabled = false
    }
    ```

    ```sh
    ./gradlew bootJar
    # build/libs/app.jar
    ```

Stage the JAR in a directory that will become `/app`. The launch descriptor
is a separate file: it tells Brewlet exactly which executable and arguments
to run when a Pod starts.

```sh
mkdir -p brewlet/application
install -m 0644 target/app.jar brewlet/application/app.jar   # Gradle: build/libs/app.jar
```

Save the following as `brewlet/launch.json`. Ask the platform team to confirm
that the selected runtime provides Java 21 at `/usr/bin/java` and has no
`ENTRYPOINT` (see the runtime checklist above).

```json
{
  "schemaVersion": 1,
  "process": {
    "command": ["/usr/bin/java"],
    "args": [
      "-XX:MaxRAMPercentage=75",
      "-jar",
      "/app/app.jar"
    ],
    "workingDir": "/app"
  }
}
```

The fields describe a process invocation, not shell text:

- `schemaVersion` selects Brewlet's version-1 launch descriptor format.
- `command` is the absolute Java executable path in the runtime image. Brewlet
  runs it directly, after any runtime image `ENTRYPOINT`, without starting a
  shell or needing a `PATH` lookup. For a runtime whose `ENTRYPOINT` is already
  `java` (such as distroless Java), omit `command`.
- `args` is an ordered array of separate arguments. `-XX:MaxRAMPercentage=75`
  is an example JVM option used to size the maximum heap as a percentage of
  available memory. With a container-aware JVM and a memory limit, that limit
  informs the calculation. It is not a Brewlet default; tune it for the
  application's non-heap memory needs. `-jar /app/app.jar` then tells Java to
  run the Spring Boot executable JAR.
- `workingDir` sets the process working directory. `/app` is also the default
  when it is omitted.

Change the executable path, runtime IDs, JVM options and JAR path to match
your platform and build output. See the
[launch descriptor contract](reference.md#launch-descriptor) for all
supported fields and validation rules.

Build the artifact by mapping the staged application directory to `/app`.
The explicit platform flags declare which architectures this artifact
supports; they do not choose where Kubernetes schedules a Pod.

```sh
brewlet build registry.example.com/apps/orders:1.4.0 \
  --platform linux/amd64 --platform linux/arm64 \
  --launch brewlet/launch.json \
  --layer brewlet/application:/app
```

A pure JVM application is platform-neutral, so one unprefixed layer can serve
every declared platform. Declare only platforms you have tested (see
[native libraries](#native-libraries-and-other-platform-specific-files)).

### Extracted layout (recommended for production)

Spring Boot can extract an executable JAR into `app.jar` plus a `lib/`
directory. The extracted form starts faster, works with class data sharing,
and lets you publish dependencies as their own layer. On Spring Boot 3.3 and
later:

```sh
java -Djarmode=tools -jar target/app.jar extract --destination target/extracted
# target/extracted/app.jar   (your classes; manifest Class-Path points at lib/)
# target/extracted/lib/*.jar (dependencies)

mkdir -p brewlet/application brewlet/dependencies
install -m 0644 target/extracted/app.jar brewlet/application/app.jar
cp -R target/extracted/lib/. brewlet/dependencies/
chmod -R a+rX brewlet
```

Spring Boot 2.3 to 3.2 use `-Djarmode=layertools ... extract` instead, which
produces a different layout started with `JarLauncher`; check the reference
documentation for your Spring Boot version.

The launch descriptor is the same as for the executable JAR, because
`app.jar` finds `lib/` relative to its own location. Package two layers:

```sh
brewlet build registry.example.com/apps/orders:1.4.0 \
  --platform linux/amd64 --platform linux/arm64 \
  --launch brewlet/launch.json \
  --layer brewlet/dependencies:/app/lib \
  --layer brewlet/application:/app
```

Add the extraction and staging commands to your CI pipeline after
`./mvnw package` or `./gradlew bootJar`; neither framework plugin nor Brewlet
runs them for you.

### Spring Boot settings that matter on Brewlet

- **Writable temporary directory.** Embedded Tomcat, Jetty and multipart
  uploads write to `java.io.tmpdir`. On a read-only root filesystem startup
  fails with `Unable to create tempDir`. Mount a volume (see
  [writable files](#writable-files)).
- **Health probes.** With Spring Boot Actuator,
  `/actuator/health/liveness` and `/actuator/health/readiness` are enabled
  automatically on Kubernetes. Set
  `management.endpoint.health.probes.enabled=true` if you test outside
  Kubernetes.
- **Graceful shutdown.** Set `server.shutdown=graceful` (the default since
  Spring Boot 3.4) and keep `spring.lifecycle.timeout-per-shutdown-phase`
  below the Pod's `terminationGracePeriodSeconds`.
- **Profiles and external configuration.** See
  [configure the application](#configure-the-application).

## Quarkus

### Fast-jar (default)

Quarkus builds a *fast-jar* directory by default. Package the whole directory
unchanged.

=== "Maven"

    ```xml
    <!-- pom.xml -->
    <properties>
      <maven.compiler.release>21</maven.compiler.release>
    </properties>
    ```

    ```sh
    ./mvnw -B package
    # target/quarkus-app/
    #   quarkus-run.jar   bootstrap JAR
    #   app/              your application JAR
    #   lib/boot/, lib/main/  dependencies
    #   quarkus/          build-time generated classes and metadata
    ```

=== "Gradle (Kotlin DSL)"

    ```kotlin
    // build.gradle.kts
    java {
        toolchain { languageVersion = JavaLanguageVersion.of(21) }
    }
    ```

    ```sh
    ./gradlew build
    # build/quarkus-app/  (same layout as Maven)
    ```

Create `brewlet/` if needed and save this Quarkus-specific descriptor as
`brewlet/launch.json`. It uses the same [descriptor fields](#executable-jar)
as the Spring Boot example, but starts the Quarkus bootstrap JAR:

```json
{
  "schemaVersion": 1,
  "process": {
    "command": ["/usr/bin/java"],
    "args": [
      "-XX:MaxRAMPercentage=75",
      "-Djava.util.logging.manager=org.jboss.logmanager.LogManager",
      "-jar",
      "/app/quarkus-run.jar"
    ],
    "workingDir": "/app"
  }
}
```

`schemaVersion` identifies the descriptor format. Replace `/usr/bin/java` with
the executable path supplied by your platform team.

The `args` array puts JVM options before `-jar`. `MaxRAMPercentage` is an
example, not a Brewlet default. The `java.util.logging.manager` property is
used by Quarkus' container templates so early JDK logging goes through
Quarkus. `/app/quarkus-run.jar` is the bootstrap JAR in the staged
`quarkus-app` directory, not a standalone application: it needs the
neighboring `app/`, `lib/` and `quarkus/` directories. `workingDir` sets
the process's working directory; `/app` is also the default.

The simplest packaging maps that entire directory to `/app`:

```sh
chmod -R a+rX target/quarkus-app        # Gradle: build/quarkus-app
brewlet build registry.example.com/apps/inventory:2.0.0 \
  --platform linux/amd64 --platform linux/arm64 \
  --launch brewlet/launch.json \
  --layer target/quarkus-app:/app
```

To publish dependencies as a separate layer, split `lib/` from the rest:

```sh
mkdir -p brewlet/application brewlet/dependencies
cp -R target/quarkus-app/lib/. brewlet/dependencies/
for entry in quarkus-run.jar app quarkus; do
  cp -R "target/quarkus-app/$entry" brewlet/application/
done
chmod -R a+rX brewlet

brewlet build registry.example.com/apps/inventory:2.0.0 \
  --platform linux/amd64 --platform linux/arm64 \
  --launch brewlet/launch.json \
  --layer brewlet/dependencies:/app/lib \
  --layer brewlet/application:/app
```

Copy any other entries your Quarkus version writes into `quarkus-app/`. Keep
the paths below `/app` exactly as Quarkus produced them.

### Uber-jar

If you prefer a single file, set `quarkus.package.jar.type=uber-jar` in
`application.properties` (Quarkus 3.x; older versions used
`quarkus.package.type`). The build produces `*-runner.jar`; stage it as
`app.jar` and launch it with `-jar /app/app.jar`. Fast-jar starts faster and
layers better, so prefer it unless your tooling needs one file.

### Quarkus settings that matter on Brewlet

- **Build-time versus runtime configuration.** Quarkus fixes build-time
  properties (installed extensions, `quarkus.datasource.db-kind`, the package
  type and many others) during `package`. Changing them in Kubernetes has no
  effect; rebuild instead. Runtime properties such as `quarkus.http.port`, URLs
  and credentials can be overridden at deployment time.
- **Writable temporary directory.** Vert.x caches files under
  `java.io.tmpdir`. Mount a volume (see [writable files](#writable-files)).
- **Health probes.** Add the `quarkus-smallrye-health` extension and probe
  `/q/health/live` and `/q/health/ready`.
- **Graceful shutdown.** Set `quarkus.shutdown.timeout` below the Pod's
  `terminationGracePeriodSeconds`.

## Plain Java applications and the Maven plugin

For an application that is a thin JAR plus ordinary dependency JARs, create
`brewlet/` if needed and save a descriptor such as this as
`brewlet/launch.json`. Unlike the framework examples, it names the classpath
and main class explicitly:

```json
{
  "schemaVersion": 1,
  "process": {
    "command": ["/usr/bin/java"],
    "args": [
      "--class-path",
      "/app/app.jar:/app/lib/*",
      "com.example.Main"
    ],
    "workingDir": "/app"
  }
}
```

`schemaVersion`, `command` and `workingDir` have the same meanings as in
the [Spring Boot descriptor](#executable-jar). Change `/usr/bin/java` to the
executable path in your chosen runtime. The Pod must select an approved,
compatible runtime.

`--class-path` tells Java where to find classes: `/app/app.jar` is the
application JAR, and `/app/lib/*` includes JARs directly inside `/app/lib`.
The colon separates classpath entries on Linux. `com.example.Main` is the
application's fully qualified main class; replace it with your own. The
wildcard is expanded by Java, not a shell. Package the application JAR and
its required dependency JARs at those paths, or adjust the descriptor to
match your layout.

The optional [Maven producer](applications.md#optional-maven-producer)
automates exactly this case. It copies the project JAR to `/app/app.jar`,
resolved dependencies to `/app/lib`, and generates classpath (or JPMS module)
arguments:

```sh
mvn package sh.brewlet:brewlet-maven-plugin:0.11.0:push \
  -Dbrewlet.image=registry.example.com/apps/orders:v2 \
  -Dbrewlet.platforms=linux/amd64,linux/arm64 \
  -Dbrewlet.mainClass=com.example.Main
```

Use the plugin version matching your `brewlet` CLI; see the
[Maven plugin reference](maven-plugin.md) for all goals and options. The
plugin does not understand framework packaging formats:

- For **Quarkus**, the project JAR is not runnable on its own; use the
  fast-jar recipe above.
- For **Spring Boot**, the repackaged JAR is not a classpath JAR. Use the
  Spring Boot recipes above rather than the plugin.
- For **WAR** and **EAR** projects, package the archive with the generic CLI
  as shown in
  [WAR and EAR applications](#war-and-ear-applications-on-a-platform-provided-application-server);
  the plugin does not handle these packaging types.

There is no Gradle plugin. Gradle builds call the same `brewlet build` and
`brewlet push` commands, for example from an `Exec` task or a CI step after
`./gradlew build`.

## WAR and EAR applications on a platform-provided application server

An application server can move into infrastructure along with the JVM. The
platform team builds a runtime image containing the JDK and, for example,
Apache Tomcat, while the application artifact carries only the WAR, any
server configuration the platform leaves to the application, and
`launch.json`. Brewlet treats the WAR/EAR as an ordinary file and needs no
WAR/EAR-specific packaging logic. A servlet container such as Tomcat runs
WARs, while an EAR needs a full Jakarta EE server such as WildFly or Open
Liberty in the runtime image; the pattern below is the same for both.

The descriptor starts the server's bootstrap class directly from the runtime
image instead of a wrapper script such as `catalina.sh`. For Tomcat, point
`catalina.home` at the server installation in the runtime image and
`catalina.base` at configuration under `/app`:

```json
{
  "schemaVersion": 1,
  "process": {
    "command": ["/opt/java/openjdk/bin/java"],
    "args": [
      "-Dcatalina.home=/usr/local/tomcat",
      "-Dcatalina.base=/app/tomcat",
      "-Djava.io.tmpdir=/tmp",
      "-classpath",
      "/usr/local/tomcat/bin/bootstrap.jar:/usr/local/tomcat/bin/tomcat-juli.jar",
      "org.apache.catalina.startup.Bootstrap",
      "start"
    ],
    "workingDir": "/app"
  }
}
```

Package the WAR and configuration as separate layers:

```sh
brewlet build registry.example.com/apps/shop:v1 \
  --platform linux/amd64 --platform linux/arm64 \
  --launch launch.json \
  --layer build/webapps:/app/webapps \
  --layer conf:/app/tomcat/conf
```

Because `/app` is read-only, configure the server so it does not write beside
the application: keep the WAR packed (`unpackWARs="false"`), disable
background redeployment (`autoDeploy="false"`) and send JSP compilation and
temporary files to an approved writable mount such as `/tmp` (see
[writable files](#writable-files)).

The [Tomcat example](https://github.com/microsoft/brewlet/tree/main/examples/tomcat)
runs an unchanged `ROOT.war` with JSP compilation on a platform-owned
JDK and Tomcat image, locally and on a cluster. The
[hardened Tomcat example](https://github.com/microsoft/brewlet/tree/main/examples/tomcat-hardened)
goes further: the platform owns Catalina's configuration and launcher in the
runtime image, and the application team supplies only the WAR. Both are
experiments rather than release-gating tests.

## Libraries provided by the runtime image

A runtime image does not have to be a stock JDK. A platform team can build a
custom runtime that already contains libraries it wants to standardize and
patch centrally, for example a JDBC driver, a security provider, a telemetry
library or an internal client library. Applications then stop shipping those
libraries, and a runtime rotation updates them across every application
without rebuilding any of them.

Brewlet does not inspect or resolve these libraries. To Brewlet they are
ordinary runtime files. The agreement about which libraries a runtime ID
provides, in which versions, is a contract between the platform team and
application teams, just like the Java feature release.

### Platform side: build the runtime

There are two common ways to add libraries to a Java runtime.

**Link JPMS modules with `jlink`.** Modular JARs, that is JARs with a
`module-info.class`, can be linked into a custom runtime together with the JDK
modules you need:

```sh
jlink \
  --module-path platform-modules \
  --add-modules java.base,java.logging,java.naming,java.net.http,java.sql,jdk.crypto.ec,com.example.telemetry \
  --strip-debug --no-header-files --no-man-pages \
  --output /opt/java
```

`platform-modules` holds the modular library JARs and their modular
dependencies; add `$JAVA_HOME/jmods` if your JDK ships it. Linked modules
become part of the runtime's system image. When an application runs on the
class path, the JVM resolves system modules that export packages by default,
so ordinary class-path code and `-jar` applications, including Spring Boot
executable JARs, can load their classes without extra options. `java
--list-modules` on the runtime image shows what was linked. `jlink` accepts
only explicit modules. JARs without `module-info.class`, including automatic
modules that only declare `Automatic-Module-Name` (many popular libraries and
JDBC drivers), cannot be linked; use a JAR directory for them.

**Add a directory of JARs.** Non-modular JARs can be placed in a documented
directory of the runtime image, for example `/opt/platform/lib`. They are not
loaded automatically; each application adds them to its class path (see
below).

Either way, publish the result as a [runtime image](runtime-images.md) under a
runtime ID, and document the provided libraries and their versions. Treat the
library set as part of that ID's contract: patch versions can roll out by
rotation, but a change that can break applications (a new major version, a
removed library, a different module name) needs a new runtime ID that
applications opt into, as with a new Java feature release.

### Application side: stop shipping those libraries

Compile against the platform library but leave it out of the package:

=== "Maven"

    ```xml
    <dependency>
      <groupId>com.example.platform</groupId>
      <artifactId>telemetry</artifactId>
      <version>3.2.1</version> <!-- match the runtime's version -->
      <scope>provided</scope>
    </dependency>
    ```

=== "Gradle (Kotlin DSL)"

    ```kotlin
    dependencies {
        compileOnly("com.example.platform:telemetry:3.2.1") // match the runtime's version
        testImplementation("com.example.platform:telemetry:3.2.1")
    }
    ```

Spring Boot's repackaged JAR, Quarkus' `lib/` directory and the
[Brewlet Maven plugin](#plain-java-applications-and-the-maven-plugin) do not
include `provided`/`compileOnly` dependencies. Check the packaged files with
`brewlet inspect`, and exclude transitive copies too: two copies of the same
library on different loaders cause `LinkageError`, split-package errors or
classes silently loaded from the wrong version.

How the application reaches the libraries depends on how the platform added
them:

- **Linked modules** need no launch changes for class-path applications. A
  modular application adds `requires com.example.telemetry;` (the module name
  the platform documents) to its `module-info.java`.
- **A JAR directory** must be on the class path. For a thin-JAR application,
  replace the descriptor's `process.args` array with the following fragment;
  it is not a complete `launch.json`. `java -jar` ignores `--class-path`, so
  this example starts an explicit main class instead:

    ```json
    "args": [
      "--class-path",
      "/app/app.jar:/app/lib/*:/opt/platform/lib/*",
      "com.example.Application"
    ]
    ```

    `/app/app.jar` and `/app/lib/*` refer to application-owned files;
    `/opt/platform/lib/*` refers to JARs supplied by the selected runtime.
    Replace the paths and `com.example.Application` with your own. The
    wildcards are Java classpath wildcards, not shell expansions. This
    thin-JAR invocation is not a substitute for the launchers used by Spring
    Boot executable JARs or Quarkus fast-jars; use a framework-supported
    approach and test runtime-provided libraries with your application.

If the runtime directory lies outside `/app`, it is part of the runtime image,
not a volume, so it needs no `volumeMountPaths` approval.

### What to move, and what to keep

Good candidates are libraries with a stable API that many applications share
and that the platform wants to patch quickly: JDBC drivers, cryptography and
security providers, observability libraries and agents, and internal platform
clients. Framework libraries usually stay in the application. Spring Boot
manages a large, tightly versioned dependency set, and Quarkus resolves and
augments its extensions at build time, so pulling individual framework JARs
into the runtime tends to break version alignment. Test the runtime selected
by the Deployment to ensure it supplies the libraries you compiled against.

## Configure the application

### JVM options versus application arguments

Arguments in `process.args` before `-jar` (or the main class) are Java
launcher options and their values; everything after the JAR or main class is
passed to your `main` method. This fragment replaces the `args` array inside a
descriptor's `process`; it is not a complete `launch.json`:

```json
"args": [
  "-XX:MaxRAMPercentage=75",
  "-Dfile.encoding=UTF-8",
  "-jar",
  "/app/app.jar",
  "--spring.profiles.active=prod"
]
```

Here `-XX:MaxRAMPercentage=75` and `-Dfile.encoding=UTF-8` configure the JVM;
`-jar /app/app.jar` selects the application; and
`--spring.profiles.active=prod` is an application argument interpreted by
Spring Boot. All are examples to adapt to your application.

Brewlet injects no JVM options. Put options that define how the application
must run (module flags, `--enable-preview`, agents you ship in `/app`) in the
descriptor. Put environment-specific tuning in the Deployment instead, using
`JDK_JAVA_OPTIONS`, which the `java` launcher reads on Java 9 and later:

```yaml
env:
  - name: JDK_JAVA_OPTIONS
    value: "-XX:MaxRAMPercentage=70 -Xss512k"
```

`JDK_JAVA_OPTIONS` is prepended to the Java command line. Avoid setting the
same option here and in the descriptor: for example, the descriptor's
`-XX:MaxRAMPercentage=75` can override the `70` shown above. Remove it from
the descriptor if the Deployment should control that setting.

Do not replace the descriptor arguments with Kubernetes `command` or `args`
just to add an option. Pod `args` replaces the descriptor's args while
preserving its command; Pod `command` replaces the descriptor's command and
launch args, while Pod `args` supplies replacement args when set. Neither field
appends to the descriptor. If overriding `args`, supply the complete desired
argument list. If overriding `command`, also set `args` when the Java invocation
needs arguments such as `-jar /app/app.jar`.

### Profiles, properties and environment variables

Both frameworks map environment variables to configuration properties, which
keeps one artifact usable in every environment:

| Purpose | Spring Boot | Quarkus |
| --- | --- | --- |
| Select a profile | `SPRING_PROFILES_ACTIVE=prod` | `QUARKUS_PROFILE=prod` |
| HTTP port | `SERVER_PORT=8080` | `QUARKUS_HTTP_PORT=8080` |
| Any property | `SPRING_DATASOURCE_URL=...` | `QUARKUS_DATASOURCE_JDBC_URL=...` |

Values in `launch.json` `process.env` act as defaults; Kubernetes `env`
overrides them. `PATH` is owned by the runtime and cannot be set.

### Configuration files

`/app` is read-only, so mount configuration files instead of editing them.
Both frameworks read `config/application.properties` (and `.yaml` for Spring
Boot) relative to the working directory. With `workingDir` `/app`, mount a
ConfigMap at `/app/config`:

```yaml
volumeMounts:
  - name: app-config
    mountPath: /app/config
    readOnly: true
volumes:
  - name: app-config
    configMap:
      name: orders-config
```

Mounts beneath `/app` need no platform approval. Files in `/app/config`
override the `application.properties` packaged in your JAR.

### Secrets

Never put credentials in `/app` or `launch.json`; the artifact is world-readable
and cached on nodes. Inject them as environment variables from a Secret, or
mount a Secret volume and import it, for example with Spring Boot's
`spring.config.import=optional:configtree:/app/secrets/` or Quarkus'
`quarkus.config.source.file-system` support.

### Writable files

The entire container root, including `/app`, is read-only. Anything the JVM or
framework writes needs an explicit volume:

```yaml
env:
  - name: JDK_JAVA_OPTIONS
    value: "-Djava.io.tmpdir=/app/tmp"
volumeMounts:
  - name: tmp
    mountPath: /app/tmp
volumes:
  - name: tmp
    emptyDir:
      sizeLimit: 256Mi
```

If your platform approved `/tmp` in the runtime image's
[`volumeMountPaths`](runtime-images.md#declare-data-and-configuration-mount-locations),
mount the `emptyDir` at `/tmp` instead and keep the JVM default. Heap dumps
(`-XX:HeapDumpPath`), file-based logs and local caches need the same treatment.
Disk-backed `emptyDir` counts against ephemeral storage; use a PVC for data
that must survive Pod replacement. See
[resources](resources.md#probes-exec-files-and-shutdown).

### Memory

Kubernetes limits are not converted into heap settings. Modern JVMs size the
heap from the container limit; `-XX:MaxRAMPercentage` controls the share. Leave
headroom for metaspace, thread stacks, direct buffers (Netty uses them
heavily), code cache and any memory-backed volumes. A limit of 512Mi with
`MaxRAMPercentage=75` is a reasonable starting point for a small service;
measure and adjust.

## Deploy

Push the artifact and deploy it by digest:

```sh
brewlet push registry.example.com/apps/orders:1.4.0 --push-result push.json
```

This annotated template combines the settings above. Replace the values in
angle brackets and remove what your application does not need.

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: orders
  namespace: apps
spec:
  replicas: 2
  selector:
    matchLabels: {app: orders}
  template:
    metadata:
      labels: {app: orders}
      annotations:
        # Runtime ID for the container named "app".
        runtime-image.brewlet.sh/app: java-21
    spec:
      runtimeClassName: brewlet
      terminationGracePeriodSeconds: 45
      containers:
        - name: app
          image: registry.example.com/apps/orders@sha256:<application-digest>
          ports:
            - containerPort: 8080
          env:
            - name: SPRING_PROFILES_ACTIVE      # Quarkus: QUARKUS_PROFILE
              value: prod
            - name: JDK_JAVA_OPTIONS
              value: "-Djava.io.tmpdir=/app/tmp"
            - name: SPRING_DATASOURCE_PASSWORD  # Quarkus: QUARKUS_DATASOURCE_PASSWORD
              valueFrom:
                secretKeyRef: {name: orders-db, key: password}
          securityContext:
            readOnlyRootFilesystem: true
          resources:
            requests: {cpu: 250m, memory: 512Mi}
            limits: {memory: 512Mi}
          startupProbe:
            httpGet: {path: /actuator/health/liveness, port: 8080}   # Quarkus: /q/health/live
            failureThreshold: 30
            periodSeconds: 2
          livenessProbe:
            httpGet: {path: /actuator/health/liveness, port: 8080}   # Quarkus: /q/health/live
          readinessProbe:
            httpGet: {path: /actuator/health/readiness, port: 8080}  # Quarkus: /q/health/ready
          volumeMounts:
            - {name: tmp, mountPath: /app/tmp}
            - {name: app-config, mountPath: /app/config, readOnly: true}
      volumes:
        - name: tmp
          emptyDir: {sizeLimit: 256Mi}
        - name: app-config
          configMap: {name: orders-config}
```

Registry authentication, the default non-root identity, capabilities and
startup tolerations follow the
[common workload rules](applications.md#native-workload-example). Add a
`nodeSelector` for `kubernetes.io/arch` if you published fewer platforms than
your cluster runs.

To check what you published and what runs:

```sh
brewlet inspect registry.example.com/apps/orders:1.4.0   # effective launch.json and layers
kubectl -n apps rollout status deployment/orders --timeout=5m
```

Rollout success does not show which runtime image generation a Pod uses; see
[actual runtime bindings](observability.md#logs-and-actual-bindings).

## Java versions and runtime compatibility

Compile for a Java release no newer than the runtime promises: Maven
`maven.compiler.release` (or Spring Boot's `java.version`), Gradle
`java.toolchain` or `options.release`. Running newer bytecode on an older
runtime fails at startup with `UnsupportedClassVersionError`.

The Pod annotation selects the runtime. Test the application against the
selected runtime before deployment, and test again when switching to a new
Java release.

## Advanced options

### Class data sharing and AOT caches

Spring Boot's extracted layout and Quarkus' fast-jar both work with AppCDS
(`-XX:SharedArchiveFile=/app/app.jsa -Xshare:auto`), and Java 24+ offers AOT
caches. An archive is only valid for the exact JDK build that created it, so
generate it with the platform's runtime image and expect it to be ignored after
a runtime rotation. Keep `-Xshare:auto` so a stale archive degrades to a
slower start rather than a failure. The Maven plugin's
`brewlet.appCdsArchive` option packages a prebuilt archive for thin JARs.

### Java agents

Either side can own an agent. An application ships its own agent in `/app`
(for example `/app/agents/opentelemetry-javaagent.jar`) and adds
`-javaagent:/app/agents/opentelemetry-javaagent.jar` before `-jar`; it updates
the agent by republishing. A platform team can instead put a standard agent in
the runtime image, so a runtime rotation updates it; the application still
names it explicitly with `-javaagent:` at the path the platform documents,
because Brewlet adds no JVM options. See [instrumentation](instrumentation.md).

### Native libraries and other platform-specific files

Netty native transports, database drivers and compression libraries sometimes
bundle JNI libraries per architecture. Test every platform you declare. If the
files differ per platform, use platform-prefixed layers such as
`--layer linux/arm64:brewlet/native-arm64:/app/native`.

### Native executables

Spring Boot and Quarkus native images (GraalVM Native Image) do not need a JVM.
Package them as [native workloads](native.md), not with this guide.

## Troubleshooting

| Symptom | Likely cause | Fix |
| --- | --- | --- |
| `ClassNotFoundException` for a framework class with Quarkus | Only `quarkus-run.jar` was packaged, or `lib/` was mounted elsewhere | Package the whole `quarkus-app/` directory with paths unchanged |
| `ClassNotFoundException` for your `Application` class with Spring Boot | Fat JAR started with `--class-path` | Start it with `-jar`, or extract it |
| `Error: Unable to access jarfile` | Path in `args` does not match the staged file name | Check the layout with `brewlet inspect` |
| `UnsupportedClassVersionError` | Compiled for a newer Java than the runtime | Lower `--release` or select a newer runtime ID |
| `Unable to create tempDir` or `Read-only file system` | JVM or framework writes to the read-only root | Mount an `emptyDir` and set `java.io.tmpdir` |
| Deployment rejected before start | Runtime ID not approved or offered | Align the annotation and the platform's inventory |
| Pod restarts during startup | Liveness probe fires before the framework is ready | Add a `startupProbe` |
| `OOMKilled` with a small heap | Non-heap memory exceeds the limit | Lower `MaxRAMPercentage` or raise the limit |
| `NoClassDefFoundError` for a library the platform provides | Selected runtime ID lacks it, or a JAR directory is missing from the class path | Check `java --list-modules` or the documented directory, and test the selected runtime |
| `LinkageError` or split-package error | The library is both in the runtime and in `/app` | Use `provided`/`compileOnly` and remove transitive copies |
| Override has no effect on Quarkus | Property is fixed at build time | Rebuild with the new value |
| Extra Kubernetes `args` dropped `-jar` | `args` replaces descriptor arguments | Repeat the complete argument list |

See [troubleshooting](troubleshooting.md) for Brewlet-level failures such as
missing runtime approvals, admission rejections and registry access.

## Tested runtimes

Brewlet's tests run Java on unmodified distroless Java 21 images (arm64, using
`/usr/bin/java`) and on Eclipse Temurin 21 images. The exact images are listed
in the [upstream runtime list](https://github.com/microsoft/brewlet/blob/main/integration-tests/fixtures/runtimes/upstream.json) and
[rotation runtime list](https://github.com/microsoft/brewlet/blob/main/integration-tests/fixtures/runtimes/rotation.json). They are test inputs, not a security
recommendation; use the runtime images your platform team approves.

The framework recipes in this guide are illustrative and are not run by
Brewlet's tests; check them against your framework version. A runnable thin-JAR
example built with the Maven producer lives in the
[Java fixture](https://github.com/microsoft/brewlet/tree/main/integration-tests/fixtures/java)
and runs weekly against `main` in the
[language example workflow](https://github.com/microsoft/brewlet/blob/main/.github/workflows/language-e2e.yml).
It does not gate pull requests or releases; see [verification](verification.md).
The [Tomcat example](https://github.com/microsoft/brewlet/tree/main/examples/tomcat)
records a manual experiment with a WAR on a platform-provided servlet container.
