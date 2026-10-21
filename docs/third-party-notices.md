# Third-party notices

The [root notices index](https://github.com/microsoft/brewlet/blob/brewlet-2.0/NOTICE.txt)
describes current build outputs, not legacy releases. Brewlet source is MIT
licensed. Dependency notices are artifact-specific and generated from the
dependencies actually selected for redistribution.

## Go binaries and containers

`make build` writes the CLI with `CGO_ENABLED=0`, `LICENSE.txt`, and `NOTICE.txt` to `bin/`.
Keep the two text files with the executable when distributing that directory.
Candidate packaging also writes these resources into the exact-target release
archives. The [prerelease infrastructure](supported-release.md) does not itself
qualify or publish a release.

The Go notice generator uses `go list -mod=readonly -deps -json` for the selected
packages with the requested OS/architecture and `CGO_ENABLED=0`. It includes
the Go runtime license and the selected modules' license, copying, copyright,
author, and notice files. Missing or empty license material fails generation;
unregistered module replacements are rejected. The default target is the
generator's host; explicit target flags support Linux amd64 and arm64 without
executing a cross-built generator.

```sh
mkdir -p bin
go run ./scripts/notice --goos linux --goarch arm64 \
  --output bin/control-notice.txt ./kubernetes/cmd/operator ./kubernetes/cmd/admission
go run ./scripts/notice --goos linux --goarch arm64 \
  --output bin/worker-notice.txt ./cmd/brewlet-node ./shim/cmd/containerd-shim-brewlet-v2
```

Both Dockerfiles generate notices in the builder and copy `/NOTICE.txt` and
`/LICENSE.txt` into the final image. The control image ships both operator and
admission by default; the explicit admission release target ships admission.
That target generates a separate notice set for its admission-only dependencies.
The worker ships brewlet-node and the containerd shim. The obsolete
standalone kubectl/ctr/crictl downloads and their former manifest do not apply.
Base-image license material remains in its original filesystem locations.

## Maven plugin

`mvn -B -f maven-plugin/pom.xml verify` resolves the runtime dependency
classpath, collects the actual JAR license/notice resources, and packages
`META-INF/NOTICE.txt` and `META-INF/LICENSE.txt`. Jackson modules use their own
resolved versions; no common-version assumption is made. Provided Maven APIs,
annotations, and test-only dependencies are not represented as redistributed
runtime software. Missing JAR license text fails the package phase.

## Verification and documentation preview

After Maven verification, run `make legal-check` to generate all three Go notice
sets for both Linux architectures and inspect the packaged plugin JAR.
The contracts job runs this check. Generator regression tests cover missing
licenses and independently versioned runtime JARs.

The documentation preview retains the existing
`.github/workflows/site-pages.yml` build. `site/scripts/build.py` combines the
checked-in browser attribution snapshot with installed documentation dependency
licenses. This change does not publish or switch production Pages.

## Distribution limits

Generated notices are not a legal determination or certification. No
Microsoft-specific container legal notice is currently supplied by this tree.
The release owner must confirm whether additional distribution-policy text is
required before publishing and supply authoritative text if needed. Do not
invent such text, restore obsolete manifests, or infer release approval from
passing packaging checks.
