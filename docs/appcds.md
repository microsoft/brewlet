# AppCDS archives

Brewlet can ship and consume a prebuilt Application Class-Data Sharing (AppCDS)
archive as an optional startup accelerator. Brewlet does not generate archives:
create the `.jsa` with your own build tooling, then attach it when publishing
the application.

## Ship a prebuilt archive

With the CLI, pass the archive to `brewlet push`:

```bash
brewlet push ./target/app.jar registry.example.com/team/app:1.0.0 \
  --appcds-archive ./target/app.jsa
```

The archive is included as a dedicated OCI layer, and its basename becomes the
`cds.archive` launch-config value. With the Maven plugin, set the existing
`brewlet.cdsArchive` parameter/property to the prebuilt archive path when
building or publishing:

```bash
mvn package brewlet:push \
  -Dbrewlet.image=registry.example.com/team/app:1.0.0 \
  -Dbrewlet.cdsArchive=target/app.jsa
```

The archive must match the application classpath and JDK build used at runtime.
Use `-Xshare:auto`, which Brewlet configures for the shipped archive, so an
incompatible archive safely falls back to the JVM's default shared archive
behavior. The application remains functional without an AppCDS archive.

AppCDS archives are supported for existing artifact, runnable-image, local
bundle, and shim launch workflows. The Maven `brewlet:appcds` goal, CLI archive
training switches, and node-side archive regeneration are not available.
