# Optional Java producer

This independently packaged Maven plugin contains Brewlet's Java project
knowledge. It invokes the language-neutral `brewlet` executable through an argv
array, not a shell. Launch descriptors, runtime image IDs and platforms are
validated by an independent Java implementation of the shared artifact contract
before the CLI runs; failures report the same reason codes (for example
`InvalidLaunchConfig: ...`). The CLI still enforces the contract when publishing.
The matching Brewlet 1.0.0 CLI must be installed explicitly.
The candidate version is governed by `release.json`; it is not a claim that
these coordinates have been published. Build/install from matching source
until a qualified prerelease is available. Java 17 or later is required.

The `conformance` goal evaluates the entire published fixture matrix (launch,
ID, platform, compatibility, mirror, layer and composition cases, including
links, permissions and whiteouts) independently in Java. It does not need the
`brewlet` executable:

```sh
mvn sh.brewlet:brewlet-maven-plugin:0.8.0:conformance \
  -Dbrewlet.fixtures=/path/to/conformance/artifact-v1.json \
  -Dbrewlet.conformanceResult=/path/to/results.json
```

The result file has the same shape as `brewlet conformance` output: normalized
process fields and reason codes per case. Any failing case fails Maven without
replacing an existing result. The integration harness compares this result with
the CLI's output, and also publishes the applicable launch, runtime-ID and
platform cases through the actual `push` goal.

Package and publish in the same Maven invocation:

```sh
mvn package sh.brewlet:brewlet-maven-plugin:0.8.0:push \
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

Optional Java conveniences (spec §11.2):

- `brewlet.appCdsArchive=/path/to/app.jsa` copies a prebuilt AppCDS archive
  (regular, non-symlink file; its mtime is preserved) into the application
  layer as `/app/app.jsa` and adds `-XX:SharedArchiveFile=/app/app.jsa
  -Xshare:auto` to the generated argv, after `javaArgs`. Runtime rotations
  change the JDK build, so the archive is best-effort. A hand-written
  `brewlet.launch` still receives the file but is never rewritten; it must
  reference `/app/app.jsa` itself.
- `runtimeImageMappings` maps Java feature releases to inventory IDs with
  entries such as `<runtimeImageMapping>21=java-21</runtimeImageMapping>`. The
  release is `maven.compiler.release` (or a numeric `maven.compiler.target` /
  `maven.compiler.source`). When `runtimeImages` is not configured, the plugin
  proposes every mapped ID whose release is at least the project's, in release
  order; with `--enable-preview` in `javaArgs` only the exact release
  qualifies. If none qualifies the build fails instead of publishing an empty
  allow-list. When `runtimeImages` is configured, the plugin warns about mapped
  IDs that provide an older (or, with preview, different) release than the
  generated argv needs.

The plugin does not build Java, install a runtime, generate workload manifests,
choose pod annotations, access Kubernetes, or apply deployments.
Applications still declare their runtime image ID in reviewed Kubernetes YAML.

## Independent Java layer sets

The standalone `layer-set` goal resolves compile/runtime dependencies without
requiring an application JAR. An Ops-owned Maven project explicitly declares its
dependency membership (including the transitive runtime closure) and supplies
`brewlet.sourceBom=groupId:artifactId:version`. The plugin resolves that exact
BOM as a Maven POM and checks **every resolved runtime member** against its
effective dependency management: group, artifact, version, type and classifier
must match. Merely importing a BOM does not add its entire catalog to the
layer set. Project version overrides outside that catalog fail closed.
There is no guessed BOM, runtime selection, or Kubernetes integration.

```sh
mvn sh.brewlet:brewlet-maven-plugin:0.8.0:layer-set \
  -Dbrewlet.layerSet=registry.example.com/dependencies/approved:2026.10 \
  -Dbrewlet.sourceBom=com.example:approved-bom:2026.10 \
  -Dbrewlet.layerSetName=approved \
  -Dbrewlet.layerSetVersion=2026.10
```

Name and version default to the publishing project's artifact ID and version.
`brewlet.store` selects a local OCI output layout. `brewlet.module` uses
`/app/mods` instead of `/app/lib` on both publication and consumption; it does
not install or choose a Java runtime. `compatibleRuntimeImages` is an optional
configuration list of inventory IDs passed to the generic layer-set config.
No platform is needed for publishing a dependency-only layer set.

The CLI computes all OCI descriptor/diff bindings, generates the mandatory
CycloneDX SBOM, and optionally signs provenance. Set
`brewlet.signingKey=/path/to/p256-private.pem` and
`brewlet.builderIdentity=https://example.com/builders/approved` for signing.
These are separate argv elements, not shell fragments. The Java plugin never
implements OCI assembly or signature verification. Signing parameters apply
only to `layer-set`, not application `push`. The separate generic `compose`
command owns final-composition signing under a distinct release policy.

### App-only release inputs

Set `-Dbrewlet.applicationOnly=true` with an explicit `brewlet.sourceBom` to
publish just the thin application contribution and matching launch metadata.
The producer validates BOM membership and retains the scoped original build
graph as `/app/.brewlet/application-build.json`; it packages no dependency
payload and does not pass `--layer-set` to `push`. Configuring `layerSets` in
this mode is rejected rather than silently composing an image.

Pass that immutable payload and a complete approved set to the generic
`brewlet compose` command in the release pipeline. Replacing the set does not
rewrite the app's build graph or prove compatibility; test the candidate
digest before updating a native workload.

### Managed consumption

Configure the same explicit `sourceBom` and one approved layer set on the
application's plugin:

```xml
<configuration>
  <sourceBom>com.example:approved-bom:2026.10</sourceBom>
  <layerSets>
    <layerSet>registry.example.com/dependencies/approved:2026.10</layerSet>
  </layerSets>
  <trustKeys>
    <trustKey>/path/to/trusted-builder.pem</trustKey>
    <trustKey>/path/to/rotated-builder.pem</trustKey>
  </trustKeys>
  <signerIdentity>https://example.com/builders/approved</signerIdentity>
  <requireProvenance>true</requireProvenance>
</configuration>
```

Run the ordinary `package ...:push` command above. `signerIdentity` and
`requireProvenance` also have `brewlet.signerIdentity` and
`brewlet.requireProvenance` properties. Trust keys are repeated `--trust-key`
arguments to **both** CLI inspection and application publication. Signed sets
require matching trust credentials even when provenance is not required.
The CLI owns these evidence/trust rules, including failure handling.

Before publication, the plugin:

1. rejects an application JAR with any nested `.jar` file (including Boot and
   WAR-style layouts); it does not unpack/rewrite a fat JAR;
2. resolves and validates exact source-BOM membership;
3. asks the CLI to inspect and verify the layer set and its SBOM/provenance;
4. compares every resolved JAR's path, SHA-256 and exact Maven identity against
   the verified neutral lock, rejecting extra, missing or substituted entries;
5. compares the canonical scoped Maven graph file's hash, so changed scopes,
   source BOM, types and classifiers cannot be hidden by matching JAR bytes; and
6. supplies only the thin application layer and the inspected, digest-pinned
   layer-set reference to `push`. It never bundles those dependencies again.

A hand-written `brewlet.launch` remains authoritative: main class, module,
Java args and runtime IDs never rewrite its bytes. That authority does not
bypass the managed dependency or thin-JAR checks. The nested-JAR check is not
a detector for arbitrary shaded/relocated classes; publish a genuinely thin
application when using managed dependencies.

Local sets may be configured as `/absolute/path/to/layout`, or as
`/absolute/path/to/layout@sha256:<manifest digest>`. Initial inspection uses
`brewlet inspect LAYOUT`; subsequent consumption uses
`--layer-set LAYOUT@sha256:<inspected manifest digest>`, independent of index
rotation. Remote tags likewise become `repository@sha256:<manifest digest>`.
The CLI re-verifies pinned artifacts at publication, so mutation of content
addressed blobs fails rather than changing what is consumed.

One complete managed graph per application is currently supported. Multiple
sets are rejected explicitly rather than ambiguously merging competing Maven
graph files. The non-managed, bundled dependency path remains available and
does not require a source BOM.

### Java graph contract (producer-owned)

The dependency layer contains JARs under `/app/lib` (or `/app/mods`) and the
ordinary file `/app/.brewlet/maven-lock.json`. The latter is compact UTF-8 JSON
with sorted object keys and artifacts sorted lexicographically by
`groupId:artifactId:type:classifier:version`:

```json
{"artifacts":[{"artifactId":"library","groupId":"com.example","path":"/app/lib/library-1.0.jar","scope":"runtime","sha256":"<64 lowercase hex>","type":"jar","version":"1.0"}],"schemaVersion":1,"sourceBom":"com.example:approved-bom:2026.10"}
```

`classifier` is omitted when empty. Scope is exactly Maven's resolved `compile`
or `runtime` scope, not normalized to a common value. Test/provided/system
dependencies are not members. Duplicate Maven coordinates or dependency
filenames, symlink inputs, unresolved artifacts and non-JAR runtime inputs
are rejected. Source JAR modes and mtimes are preserved; generated graph
metadata and staging directory mtimes are zero.

Each JAR is also represented independently in the neutral lock with
`ecosystem: java`, artifact ID as `name`, group ID as `group`, exact `version`,
Maven `purl` (including non-default type/classifier qualifiers), `/app/...`
`path`, and bare SHA-256. The graph file has an ordinary neutral entry with
`ecosystem: java`, `name: maven-lock`, `version: "1"`, its path, and its
SHA-256. Metadata-only hash comparison is deliberately insufficient: all
individual JAR entries must match too. Maven/BOM/scope data never enters core
structs. The generic CLI binds these file hashes to verified layer bytes,
manifest descriptors, the SBOM and optional signed provenance.

## Validation

Build and test with `mvn -B -q -f maven-plugin/pom.xml verify`.
`MavenGraphTest`, `JavaProducerTest`, `RuntimeImageMappingTest` and
`MojoContractTest` cover canonical BOM/scoped graph locks, membership/hash/scope
drift, duplicate filenames, thin-JAR rejection, descriptor authority, AppCDS
attachment, runtime ID proposals, local/remote pinning, trust argv boundaries,
and full goal execution with a recording CLI. `ConformanceTest` requires every
case in `../conformance/artifact-v1.json` to pass the Java evaluator.

For real CLI integration, install the matching plugin, publish an Ops project
with the `layer-set` command, and consume its OCI layout via the application
configuration above and `package ...:push`. The Brewlet executable is always
selectable with `-Dbrewlet.executable=/absolute/path/to/cli-wrapper`; on hosts
that attach unremovable filesystem xattrs, package/consume inside the existing
Linux integration environment rather than stripping metadata or weakening the
CLI's file policy.
