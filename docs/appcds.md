# AppCDS (Application Class Data Sharing)

Brewlet packages and consumes **externally prebuilt** AppCDS archives. Attach a
`.jsa` file with CLI `--appcds-archive` or Maven `brewlet.cdsArchive`; Brewlet
delivers it read-only and launches with `-Xshare:auto`. Archive creation and
refresh belong to your own build tooling, outside Brewlet.

## 1. What is shared

The JDK's default CDS archive (`lib/server/classes.jsa`) contains JDK classes.
Brewlet shares the node JDK root read-only, so compatible workloads can reuse
that base archive without shipping an application archive.

Application CDS extends sharing to application and library classes. Static and
dynamic AppCDS archives can both be delivered as an optional
`application/vnd.brewlet.cds.layer.v1+jsa` layer. The launch-config `cds.mode`
value (`static` or `dynamic`) is informational; it does not request training or
change how the archive is consumed.

## 2. Compatibility requirements

An archive is a **best-effort startup accelerator**, not an application
correctness requirement. Before packaging, prepare and validate it externally
against the runtime and payload you intend to deploy:

- **Exact JDK build:** HotSpot validates its full build identity, not only the
  feature or patch version. A different vendor build, rebuild, or JDK patch may
  invalidate the archive.
- **CPU architecture:** a `.jsa` is architecture-specific. Select an appropriate
  artifact `arch` constraint for an archive intended for one architecture; see
  [multi-arch fleets](multi-arch.md#appcds-archives).
- **Application bytes and launch layout:** use the same application JAR and
  dependency/module JARs, filenames, entry mode, classpath/module-path order,
  and relevant JVM options as the published application. Layered Spring Boot
  publication transforms the executable JAR into a thin payload, so an archive
  prepared against the original executable JAR is not a substitute.
- **JAR timestamps:** HotSpot validates classpath-entry size and mtime as well
  as identity. See [payload timestamps](#5-payload-timestamps-and-layout).
- **JVM support:** use a JDK that supports the archive format and options used by
  your external tooling. There is no separate Brewlet training-runtime requirement.

With `-Xshare:auto`, an incompatible archive is ignored and the JVM starts
without application sharing, using base CDS where available. Do not replace it
with `-Xshare:on` in production: that makes an unusable archive fatal.

## 3. Package a prebuilt archive with the CLI

Given an existing `app.jsa` prepared for your published payload:

```bash
brewlet push ./target/app.jar registry.example.com/team/app:1.4.2 \
  --appcds-archive ./target/app.jsa
```

For a local native-artifact workflow:

```bash
brewlet push ./target/app.jar demo/app:1.4.2 \
  --store ./oci --format artifact --appcds-archive ./target/app.jsa
brewlet inspect demo/app:1.4.2 --store ./oci
```

The archive basename becomes `cds.archive`. If you supply `--config`, its archive
name must match the attached file. Dependency and module layers can accompany a
prebuilt archive, provided the archive matches their final launch layout.
Managed dependency-bundle composition does not support attaching AppCDS.

Use runnable-image format (the default) for Kubernetes; native artifacts serve
local CLI and bundle workflows. The archive layer and launch contract are
preserved in either format.

## 4. Package a prebuilt archive with Maven

First [configure the Maven plugin](building-and-publishing.md#option-c-maven-plugin)
in the application POM. Point `brewlet.cdsArchive` at an existing file:

```bash
mvn package brewlet:inspect -Dbrewlet.cdsArchive=./archives/app.jsa
mvn package brewlet:push \
  -Dbrewlet.image=registry.example.com/team/app:1.4.2 \
  -Dbrewlet.cdsArchive=./archives/app.jsa
```

The `config`, `inspect`, `build`, and `push` goals honor this setting. `inspect`
shows the `cds` launch-config block and archive layer descriptor before
publication. You can also configure `<cdsArchive>` in the plugin's
`<configuration>`. Keep the archive paired with the exact prepared application
bytes; rebuilding or changing dependencies after external preparation can
invalidate it.

## 5. Payload timestamps and layout

When an archive is attached, Brewlet normalizes application and dependency JAR
mtimes to **2000-01-01T00:00:00Z**, Unix second **946684800**, nanoseconds zero.
External archive preparation must use that same timestamp for every applicable
JAR. Otherwise an apparently correct archive can be ignored at launch.

The runtime stages copies rather than modifying shared read-only content-store
blobs. The application lives under `/app`, dependencies under `/app/lib`, and
module dependencies under `/app/mods`. The working directory is `/app`; relative
classpath entries and their order must agree with the archived layout. For
layered Boot apps, use the prepared thin application and exact packaged
libraries, not a separately resolved dependency graph.

Inspect the effective launch config and validate archive mapping with your target
JDK and prepared payload before relying on startup gains. An archive descriptor
in the image proves delivery, not that HotSpot accepted or mapped the archive.

## 6. Runtime consumption and JDK updates

The config references the archive by a bare `/app`-relative filename:

```json
{
  "schemaVersion": 1,
  "mainJar": "app.jar",
  "entry": { "mode": "jar" },
  "cds": { "archive": "app.jsa", "mode": "dynamic" }
}
```

The shim mounts it read-only at `/app/app.jsa` and emits:

```bash
java -Xshare:auto -XX:SharedArchiveFile=/app/app.jsa -jar /app/app.jar
```

Local `run`, OCI `bundle`, and production shim launches consume the shipped
archive. No writable node archive cache or NodeProfile capability is required.

Central JDK patching remains independent of application publication. Existing
JVMs keep their original runtime until they stop; after a restart onto a different
JDK build, a shipped archive may no longer provide acceleration. If startup
measurements justify it, prepare a matching archive externally and publish a new
application image. Brewlet does not refresh the archive on the node. Do not make
archive compatibility a hard exact-build scheduling dependency.

Agents that alter the bootstrap classpath can reduce sharing benefits; see
[OpenTelemetry interaction](opentelemetry.md#appcds-interaction). Measure with
your production instrumentation and representative workload.

## 7. References

- [JEP 310: Application Class-Data Sharing](https://openjdk.org/jeps/310)
- [JEP 350: Dynamic CDS Archives](https://openjdk.org/jeps/350)
- [Building and publishing](building-and-publishing.md)
- [Layered classpath deployment](layered-classpath-deployment.md)
- [Specification §13: Performance and startup](https://github.com/microsoft/brewlet/blob/main/specs/SPECIFICATION.md#13-performance--startup)
