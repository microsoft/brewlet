# Optional Java producer

This independently packaged Maven plugin contains Brewlet's Java project
knowledge. It invokes the language-neutral `brewlet` executable through an argv
array, not a shell. Descriptor, mirror, layer and platform validation are the
same Go implementation used by the CLI and shim, not a second Java parser.
The matching Brewlet 2 CLI must be installed explicitly.

After `mvn package`, run:

```sh
mvn sh.brewlet:brewlet-maven-plugin:2.0.0-SNAPSHOT:push \
  -Dbrewlet.image=registry.example.com/apps/example:v2 \
  -Dbrewlet.platforms=linux/amd64,linux/arm64 \
  -Dbrewlet.mainClass=example.Main
```

Use `-Dbrewlet.store=/absolute/path/to/oci` for local publication,
`-Dbrewlet.executable=/absolute/path/to/brewlet` to select the installed CLI,
or `-Dbrewlet.launch=/path/to/launch.json` for an authoritative hand-written
descriptor. The producer never supplies a platform default.

The producer packages the project JAR as `/app/app.jar`, copies resolved runtime
dependencies byte-for-byte into a separate `/app/lib` layer, and generates
ordinary Java classpath argv. `brewlet.module` selects a JPMS module and uses
`/app/mods`. `javaArgs`, `runtimeImages` and `layerSets` are optional plugin
configuration lists. `javaArgs` are explicit application argv, not runtime
tuning injected by core. No runtime ID is inferred from the Maven JVM.

The plugin does not build Java, install a runtime, generate workload manifests,
choose pod annotations, access Kubernetes, or apply deployments.
Applications still declare their runtime image ID in reviewed Kubernetes YAML.

Build and test with `mvn -B -f maven-plugin/pom.xml verify`.
