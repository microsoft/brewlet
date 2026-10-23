# Third-party notices

The [root notices index](https://github.com/microsoft/brewlet/blob/main/NOTICE.txt)
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

## Regenerating all notices

Install the website notice inputs in an isolated environment, using the same
Python requirements as the Pages workflow:

```sh
python3 -m venv site/.venv
site/.venv/bin/python -m pip install -r site/requirements.txt
npm ci --ignore-scripts --prefix site/notices
make notices SITE_PYTHON=site/.venv/bin/python
```

This requires Go, Maven, a JDK, Python, and Node.js/npm. The command writes
target-specific Go notices to `bin/notices/` for the CLI, combined control
image, admission-only image, and worker image on Linux amd64 and arm64. It
packages the Maven plugin with generated legal resources and regenerates
`site/NOTICE.txt`. Existing binary, container, and release builds continue to
generate and package their own artifact-specific notices.

Use `make go-notices`, `make maven-notices`, or `make site-notices` to update
one surface. Only the website notice is checked in; the Go and Maven notices
live in ignored build directories. Regenerate after changing dependencies,
and commit website notice changes with their inputs.

The website generator restores the pinned browser dependency inventory from
the Java-era branch. Its separate npm project is for attribution inputs, not
the Playwright test tooling in `site/package.json`. It collects actual license
and notice files for every locked package, including transitive dependencies,
and validates installed versions against the lockfile. The small checked-in
license supplements preserve upstream attribution absent from npm tarballs.
Version-specific source links and modification statements must also be updated
when their corresponding dependency pins change. Material upgrades require
reviewing its bundled browser dependencies and updating the generator's
`MATERIAL_VERSION` guard alongside the requirements and npm inventory.

## Verification and documentation preview

After Maven verification, run `make legal-check` to generate all four Go notice
sets for both Linux architectures and inspect the packaged plugin JAR.
The contracts job runs this check. Generator regression tests cover missing
licenses and independently versioned runtime JARs.

The documentation preview retains the existing
`.github/workflows/site-pages.yml` build. `site/scripts/build.py` combines the
checked-in browser attribution snapshot with installed documentation dependency
licenses. Before building, the workflow installs the locked browser notice inputs
and runs `make site-notices-check`, which fails on stale or missing notices without
rewriting the snapshot. Reproduce this check with
`make site-notices-check SITE_PYTHON=site/.venv/bin/python`.
This change does not publish or switch production Pages.

## Distribution limits

Generated notices are not a legal determination or certification. No
Microsoft-specific container legal notice is currently supplied by this tree.
The release owner must confirm whether additional distribution-policy text is
required before publishing and supply authoritative text if needed. Do not
invent such text, restore obsolete manifests, or infer release approval from
passing packaging checks.
