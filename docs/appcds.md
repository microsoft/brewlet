# AppCDS (Application Class Data Sharing)

Brewlet can ship and consume an **externally generated** AppCDS archive.
Brewlet does not generate archives: the Maven `brewlet:appcds` goal, CLI
training options, and node-side regeneration are no longer available.
The JDK's own default CDS archive remains available independently.

## Prebuilt archive delivery

Attach an existing archive when publishing:

```bash
brewlet push target/app.jar registry.example.com/team/app:1.0 \
  --appcds-archive target/app.jsa --arch amd64
```

Or attach it with the Maven plugin:

```bash
mvn package brewlet:push \
  -Dbrewlet.image=registry.example.com/team/app:1.0 \
  -Dbrewlet.cdsArchive=target/app.jsa \
  -Dbrewlet.arch=amd64
```

The archive's basename becomes `cds.archive` in the launch config:

```json
{
  "schemaVersion": 1,
  "mainJar": "app.jar",
  "entry": { "mode": "jar" },
  "cds": { "archive": "app.jsa", "mode": "dynamic" }
}
```

Native artifacts carry the archive in an
`application/vnd.brewlet.cds.layer.v1+jsa` layer. Runnable images carry it
alongside the application JAR. Both formats flow through `run`, `bundle`,
and the containerd shim. At runtime the archive is read-only at
`/app/<archive>`, and Brewlet prepends:

```text
-Xshare:auto -XX:SharedArchiveFile=/app/<archive>
```

There is no writable node archive cache, writer election, or automatic refresh.
No generation flag is injected into the JVM.

## Compatibility and timestamps

An archive is tied to the **exact JDK build**, CPU architecture, application
bytes, and classpath/module-path layout used to produce it. Even a JDK patch
or rebuild can invalidate it. Select the artifact's `arch` constraint explicitly
when shipping an architecture-specific archive; bytecode architecture detection
does not inspect `.jsa` files.

HotSpot also validates classpath JAR names, sizes, and modification times.
When an artifact ships an archive, Brewlet pins the staged application JAR and
dependency files to **2000-01-01 00:00:00 UTC** (Unix timestamp `946684800`).
External training must use the same timestamp and the same JAR bytes and
relative classpath/module-path order. Brewlet launches from the directory
holding the application JAR (`/app` in the sandbox).

For layered Spring Boot applications, train against the prepared thin payload
and packaged dependency order, not the original executable Boot JAR. See
[building and publishing](building-and-publishing.md#layered-thin-jar-apps)
and the [Maven plugin contract](https://github.com/microsoft/brewlet/blob/main/maven-plugin/README.md#layered-spring-boot-jars).

With `-Xshare:auto`, an incompatible archive falls back to base CDS rather than
preventing startup. A central JDK patch therefore preserves correctness, but
the application-archive benefit is lost until an externally rebuilt archive
is published. Measure startup with the exact production JDK, layout, and agents;
archive delivery alone does not prove that HotSpot mapped it.

## Migration from archive generation

Remove the old generation settings from build and deployment configuration:

- Maven `brewlet:appcds` lifecycle bindings and `brewlet.appcds.*` properties.
- CLI `push --appcds`, `--appcds-java`, `--appcds-timeout`, and `--appcds-arg`.
- CLI `run`/`bundle --appcds-regenerate` and `BREWLET_CDS_CACHE`.
- `JavaApplication.spec.jvm.cds`, the `brewlet.sh/cds-regenerate` pod annotation,
  and `NodeProfile.spec.appCDS` / chart `appCDS` settings.

The provisioner removes the old authorization sentinel and capability label
when provisioning or cleaning up a node. It does not delete archives left by
older releases or stop an already-running old JVM/shim. Review retained files
with their owners before removing them; follow the
[retained-cache cleanup guidance](uninstallation.md#retained-appcds-cache-files).
