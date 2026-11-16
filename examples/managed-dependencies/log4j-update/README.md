# Update a managed dependency: Log4j sample

A multi-module Maven project for the
[Update a managed dependency](../../../docs/update-managed-dependency.md)
tutorial. It shows a platform team replacing Log4j in a running Java service
without rebuilding the application or changing the Java runtime.

| Module | Packaging | Role |
| --- | --- | --- |
| [`app`](app/pom.xml) | `jar` | A small HTTP service that logs with Log4j. It imports bundle release 1 and depends on `log4j-api` without a version. The Brewlet Maven plugin packages it as an app-only image: the thin JAR and a launch descriptor, with no Log4j. |
| [`bundle-release-1`](bundle-release-1/pom.xml) | `pom` | `log4j-bundle:1.0.0`, the first release of the dependency bundle. It pins **Log4j 2.14.1**, which has known CVEs. |
| [`bundle-release-2`](bundle-release-2/pom.xml) | `pom` | `log4j-bundle:2.0.0`, the patched release. It pins **Log4j 2.26.1**. |

Each bundle is both a BOM (`dependencyManagement` lists the approved versions)
and the list of members the platform team publishes (`dependencies`). The
`brewlet:layer-set` goal turns a bundle into a signed layer set, and
`brewlet bundle` combines a layer set with the app-only image into a release
image.

The app module never changes between releases. Moving from release 1 to
release 2 swaps only the Log4j layer.

> **Release 1 is vulnerable on purpose.** Log4j 2.14.1 is affected by
> CVE-2021-44228 (Log4Shell), CVE-2021-45046, CVE-2021-45105 and
> CVE-2021-44832, among others. The sample service never exercises them, but
> run it only in the local, disposable cluster from the tutorial. Don't deploy
> release 1 to a shared or production cluster.

## Build

You need JDK 21 or later and Maven 3.9 or later. The `app` module imports the
bundle from a Maven repository, so install the bundles first. This stands in
for the repository your platform team publishes to:

```sh
mvn -B -q install -pl '!app'
```

Build the app-only image and publish each bundle as a layer set with the
[Brewlet Maven plugin](../../../maven-plugin/README.md). The
[tutorial](../../../docs/update-managed-dependency.md) lists the full commands
with their signing keys, trust policy and cluster steps.

## Scan for CVEs

Log4j's advisories are in `log4j-core`, which only the bundles list. Scanning
the bundles, which the platform team owns, covers what the service runs with.
The `app` module alone scans clean, because it depends on `log4j-api` only.

```sh
mvn -B -q org.cyclonedx:cyclonedx-maven-plugin:2.9.1:makeBom -f bundle-release-1/pom.xml -DoutputFormat=json
osv-scanner scan source -L bundle-release-1/target/bom.json
```

Repeat with `bundle-release-2` to confirm the patched release is clean.
