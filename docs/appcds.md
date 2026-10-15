# AppCDS (Application Class Data Sharing)

Brewlet can ship and consume a **prebuilt** Application Class Data Sharing
archive (`.jsa`). It does not generate archives, run training JVMs, or maintain
a node-side archive cache. The default CDS archive supplied by the node JDK
continues to work independently of this optional application archive.

## Shipping a prebuilt archive

Supply an archive prepared outside Brewlet with the CLI:

```bash
brewlet push target/app.jar registry.example.com/team/app:1.0 \
  --appcds-archive target/app.jsa
```

Or use the Maven plugin's existing archive parameter:

```bash
mvn brewlet:push \
  -Dbrewlet.image=registry.example.com/team/app:1.0 \
  -Dbrewlet.cdsArchive=target/app.jsa
```

The archive is carried as an optional
`application/vnd.brewlet.cds.layer.v1+jsa` layer in the native artifact format,
or as application content in a runnable image. The launch config pairs it with
an optional hint:

```json
{
  "schemaVersion": 1,
  "mainJar": "app.jar",
  "entry": { "mode": "jar" },
  "cds": { "archive": "app.jsa", "mode": "dynamic" }
}
```

`cds.archive` must be a bare filename matching the supplied archive.
`cds.mode` accepts `dynamic` or `static` and is informational. The CLI and Maven
plugin default the archive name from the supplied file.

The shim and standalone bundle path mount the archive **read-only** at
`/app/<archive>`. Local `brewlet run` copies it beside the application JAR.
Launch prepends `-Xshare:auto -XX:SharedArchiveFile=<archive-path>`; there are
no archive-generation flags or writable archive-cache mounts.

## Compatibility requirements

An archive is a best-effort startup accelerator, not a correctness requirement:

- Use the **exact JDK build** and CPU architecture that will consume the archive.
  Even a patch update or rebuild of the same JDK release can invalidate it.
- Match the published application and dependency bytes, JAR filenames, entry
  mode, classpath/module-path ordering, working-directory-relative paths, and
  app-intrinsic JVM flags.
- Prepare archive inputs with the canonical modification time
  **2000-01-01T00:00:00Z** (Unix `946684800`, zero nanoseconds). Brewlet pins
  staged application and dependency JARs to that timestamp when consuming an
  archive. The Maven plugin's prepared payload uses the same timestamp.
- For layered Spring Boot applications, use the
  [prepared thin JAR and exact packaged libraries](building-and-publishing.md#layered-thin-jar-apps),
  not the original executable Boot JAR or a separately resolved dependency set.
- For architecture-specific archives, set the artifact's
  [architecture constraint](multi-arch.md#appcds-archives) explicitly.

With `-Xshare:auto`, a JDK-build or classpath mismatch falls back to base CDS
instead of preventing application startup. Brewlet does not refresh a stale
archive after a node JDK update: prepare and publish a replacement externally,
or omit the application archive. Measure startup for the actual workload and
JDK rather than assuming a benefit.

## Migrating from archive generation

Remove the former CLI training flags (`--appcds`, `--appcds-java`,
`--appcds-timeout`, `--appcds-arg`) and `run`/`bundle --appcds-regenerate`.
Remove Maven `brewlet:appcds` executions and their `brewlet.appcds.*`
parameters. Continue using `--appcds-archive` or `brewlet.cdsArchive` only when
you have a compatible prebuilt archive.

Remove `JavaApplication.spec.jvm.cds`, the `brewlet.sh/cds-regenerate` pod
annotation, `NodeProfile.spec.appCDS`, and the corresponding Helm
`provisioner.appCDS`/`profiles[].appCDS` values. The provisioner no longer accepts
`BREWLET_APP_CDS_REGENERATION_ENABLED`, and runtimes no longer use
`BREWLET_CDS_CACHE`. The regeneration decision metric and its dashboard panel
have also been removed.

Existing writer workloads must be stopped or replaced before retiring older
shims. Legacy cache files and policy sentinels are not automatically deleted;
review them with the node owner as described in
[uninstallation](uninstallation.md).
