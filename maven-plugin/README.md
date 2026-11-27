# Optional Java producer

This independently packaged Maven plugin contains Brewlet's Java project
knowledge. It invokes the language-neutral `brewlet` executable through an argv
array, not a shell. Launch descriptors, runtime image IDs and platforms are
validated independently before the CLI runs; failures report the same reason
codes (for example `InvalidLaunchConfig: ...`). The matching Brewlet CLI must
be installed explicitly. The candidate version is governed by `release.json`;
it is not a claim that these coordinates have been published. Build/install
from matching source until a qualified prerelease is available. Java 17 or
later is required.

The `conformance` goal evaluates the published fixture matrix (launch, ID,
platform, compatibility, mirror, layer and composition cases, including
links, permissions and whiteouts) independently in Java. It does not need the
`brewlet` executable:

```sh
mvn sh.brewlet:brewlet-maven-plugin:0.13.1:conformance \
  -Dbrewlet.fixtures=/path/to/conformance/artifact-v1.json \
  -Dbrewlet.conformanceResult=/path/to/results.json
```

The result file contains normalized process fields and reason codes per case.
Any failing case fails Maven without replacing an existing result. The
integration harness checks every fixture and also publishes the applicable
launch, runtime-ID and platform cases through the `build` goal and the actual CLI.

The `build` and `push` goals mirror `brewlet build` and `brewlet push`.
`build` writes an OCI layout (default `target/brewlet/oci`) and never
contacts a registry; `push` uploads that layout unchanged. Package, build and
publish in the same Maven invocation:

```sh
mvn package sh.brewlet:brewlet-maven-plugin:0.13.1:build \
  sh.brewlet:brewlet-maven-plugin:0.13.1:push \
  -Dbrewlet.image=registry.example.com/apps/example:v2 \
  -Dbrewlet.platforms=linux/amd64,linux/arm64 \
  -Dbrewlet.mainClass=example.Main
```

Omit `push` to build locally only. Use `-Dbrewlet.store=/absolute/path/to/oci`
to select another layout for both goals,
`-Dbrewlet.executable=/absolute/path/to/brewlet` to select the installed CLI,
or `-Dbrewlet.launch=/path/to/launch.json` for an authoritative hand-written
descriptor. The producer never supplies a platform default.

The producer packages the project JAR as `/app/app.jar`, copies resolved
compile/runtime dependencies byte-for-byte into `/app/lib`, and generates
ordinary Java classpath argv. Both libraries and the application are ordinary
app payload, staged in separate app layers to allow independent caching.
`brewlet.module` selects a JPMS module and uses `/app/mods` for libraries.
`javaArgs` is an optional plugin configuration list.
`javaArgs` are explicit application argv, not runtime tuning injected by
core. No runtime ID is inferred from the Maven JVM. Duplicate library filenames,
unresolved artifacts and symlink producer inputs are rejected.

Optional Java conveniences:

- `brewlet.appCdsArchive=/path/to/app.jsa` copies a prebuilt AppCDS archive
  (regular, non-symlink file; its mtime is preserved) into the application
  layer as `/app/app.jsa` and adds `-XX:SharedArchiveFile=/app/app.jsa
  -Xshare:auto` to the generated argv, after `javaArgs`. Runtime rotations
  change the JDK build, so the archive is best-effort. A hand-written
  `brewlet.launch` still receives the file but is never rewritten; it must
  reference `/app/app.jsa` itself.
The plugin does not build Java, install a runtime, generate workload manifests,
choose pod annotations, access Kubernetes, or apply deployments.
Applications still declare their runtime image ID in reviewed Kubernetes YAML.

## Validation

Build and test with `mvn -B -q -f maven-plugin/pom.xml verify`.
`JavaProducerTest` and `MojoContractTest` cover split app layers, duplicate
filenames, descriptor authority, AppCDS and goal execution with a recording CLI.
`ConformanceTest` requires every case in `../conformance/artifact-v1.json`
to pass the Java evaluator.

For real CLI integration, install the matching plugin and use `package ...:build`
as above. The executable is selectable with
`-Dbrewlet.executable=/absolute/path/to/cli-wrapper`; on hosts that attach
unremovable filesystem xattrs, package inside the existing Linux integration
environment rather than stripping metadata or weakening the CLI's file policy.
