# Third-party notices

Brewlet is MIT licensed. Each released artifact carries the Brewlet
`LICENSE.txt` and a `NOTICE.txt` generated from the third-party dependencies
actually redistributed in that artifact. The
[root notices index](https://github.com/microsoft/brewlet/blob/main/NOTICE.txt)
describes the current release outputs.

## Where to find the notices

| Artifact | License and notices |
| --- | --- |
| CLI archive `brewlet_VERSION_OS_ARCH.tar.gz` | `LICENSE.txt` and `NOTICE.txt` next to the `brewlet` executable. Keep them with the executable if you redistribute it. |
| `ghcr.io/microsoft/brewlet-operator` | `/LICENSE.txt` and `/NOTICE.txt` in the image, covering the operator and admission binaries. |
| `ghcr.io/microsoft/brewlet-admission` | `/LICENSE.txt` and `/NOTICE.txt` in the image, covering the admission binary only. |
| `ghcr.io/microsoft/brewlet-node-provisioner` | `/LICENSE.txt` and `/NOTICE.txt` in the image, covering the node worker and the containerd shim. |
| Helm chart `oci://ghcr.io/microsoft/charts/brewlet` | `LICENSE.txt` and `NOTICE.txt` in the chart package. |
| Maven plugin `sh.brewlet:brewlet-maven-plugin` | `META-INF/LICENSE.txt` and `META-INF/NOTICE.txt` in the binary, sources and Javadoc JARs. |
| Website | [`NOTICE.txt`](https://brewlet.sh/NOTICE.txt) for the browser and documentation dependencies. |

Container images also keep their base image's license material in its original
filesystem locations.

For example, to read the notices of a downloaded CLI archive:

```sh
tar -xzf brewlet_0.8.1_linux_amd64.tar.gz NOTICE.txt LICENSE.txt
```

## How notices are generated

- **Go binaries and images:** notices come from the Go packages actually
  compiled for the target OS and architecture, including the Go runtime
  license and each module's license, copying, copyright, author and notice
  files. Missing or empty license material fails the build.
- **Maven plugin:** notices come from the resolved runtime dependency
  classpath, using each JAR's own license and notice resources. Provided Maven
  APIs, annotations and test-only dependencies are not redistributed and are
  not listed. Missing JAR license text fails packaging.
- **Website:** notices come from the locked browser and documentation
  dependencies, including transitive packages, plus small checked-in
  supplements for attribution missing from npm packages.

Generated notices are not a legal determination or certification.

## Maintainers: regenerating notices

Regenerate notices after changing dependencies. With a development checkout set
up as described in [CONTRIBUTING.md](../CONTRIBUTING.md) (Go, Maven, a JDK,
Python and Node.js/npm), install the website notice inputs in an isolated
environment and run:

```sh
python3 -m venv site/.venv
site/.venv/bin/python -m pip install -r site/requirements.txt
npm ci --ignore-scripts --prefix site/notices
make notices SITE_PYTHON=site/.venv/bin/python
```

`make notices` writes target-specific Go notices to `bin/notices/`, packages
the Maven plugin with its legal resources and regenerates `site/NOTICE.txt`.
Use `make go-notices`, `make maven-notices` or `make site-notices` to update a
single surface. Only `site/NOTICE.txt` is checked in; commit it together with
the dependency change.

`make legal-check` generates every Go notice set and checks them, and
`make site-notices-check SITE_PYTHON=site/.venv/bin/python` fails on a stale
website notice without rewriting it. Both run in CI. When upgrading the
documentation theme, also review its bundled browser dependencies and update
`MATERIAL_VERSION` in `site/scripts/generate-notice.py`.
