# Third-party notices

Brewlet includes the license and attribution text for redistributed open source
inside each applicable release artifact. The notices are artifact-specific so
build-only and test-only dependencies are not presented as shipped software.

| Release surface | Notice location |
|---|---|
| Repository source | [`NOTICE.txt`](https://github.com/microsoft/brewlet/blob/main/NOTICE.txt) |
| `brewlet` CLI archive | `NOTICE.txt` beside the executable |
| Operator and admission images | `/NOTICE.txt` |
| Node provisioner image | `/NOTICE.txt` |
| Maven plugin JAR | `META-INF/NOTICE.txt` |
| `brewlet.sh` and generated documentation | `/NOTICE.txt` |
| Helm chart | No third-party code is embedded in the chart package |

Container images also include the Microsoft container legal notice at
`/CONTAINER-LEGAL-NOTICE.txt`. Base-image package copyright files remain in
their original filesystem locations.

The Maven plugin JAR also includes the project's MIT license at
`META-INF/LICENSE.txt`. CLI archives include `LICENSE.txt`, and container images
include it at `/LICENSE.txt`.

## Maintaining the notices

The release and CI workflows generate Go notices from the packages actually
linked for the target operating system and architecture. Generation fails when
a linked module has no discoverable license or attribution file.

The Maven notice is generated from the license and notice files inside the
resolved Jackson JARs:

```bash
maven-plugin/scripts/generate-notice.sh --update
```

The website build is pinned by `site/requirements.txt` and
`site/notices/package-lock.json`. Regenerate its notice after updating either
set of inputs:

```bash
npm ci --ignore-scripts --prefix site/notices
python3 -m pip install -r site/requirements.txt
python3 site/scripts/generate-notice.py
```

The provisioner's directly downloaded `kubectl`, `ctr`, and `crictl` binaries
are registered in `provisioner/cgmanifest.json` because they are not represented
by a package-manager manifest in this repository.
