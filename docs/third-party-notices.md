# Third-party notices

Brewlet includes the license and attribution text for redistributed open source
inside each applicable release artifact. The notices are artifact-specific so
build-only and test-only dependencies are not presented as shipped software.
There is no single aggregated notice file: the repository-level
[`NOTICE.txt`](https://github.com/microsoft/brewlet/blob/main/NOTICE.txt) is an
index that points at the files below.

Some notices are checked into the repository. The rest are generated during the
build from the dependencies actually linked into the artifact, so they exist
only inside a built artifact and cannot be browsed in the source tree.

## Notices checked into the repository

| Notice | Covers | Ships as |
|---|---|---|
| [`site/NOTICE.txt`](https://github.com/microsoft/brewlet/blob/main/site/NOTICE.txt) | Website and generated documentation (Python and npm dependencies) | [`/NOTICE.txt`](https://brewlet.sh/NOTICE.txt) on `brewlet.sh` |
| [`maven-plugin/src/main/resources/META-INF/NOTICE.txt`](https://github.com/microsoft/brewlet/blob/main/maven-plugin/src/main/resources/META-INF/NOTICE.txt) | Maven plugin (Jackson) | `META-INF/NOTICE.txt` in the plugin JAR |
| [`legal/CONTAINER-LEGAL-NOTICE.txt`](https://github.com/microsoft/brewlet/blob/main/legal/CONTAINER-LEGAL-NOTICE.txt) | Microsoft container legal notice | `/CONTAINER-LEGAL-NOTICE.txt` in every image |
| [`provisioner/cgmanifest.json`](https://github.com/microsoft/brewlet/blob/main/provisioner/cgmanifest.json) | `kubectl`, `ctr`, and `crictl` binaries the provisioner downloads directly | Registration only; the licenses ship in the provisioner image notice |

## Notices generated at build time

| Release surface | Notice location | Reproduce locally |
|---|---|---|
| `brewlet` CLI archive | `NOTICE.txt` beside the executable | `./scripts/generate-go-notice.sh core /tmp/cli-notice.txt ./cmd/brewlet` |
| Operator image | `/NOTICE.txt` | `./scripts/generate-go-notice.sh kubernetes /tmp/operator-notice.txt ./cmd/manager` |
| Admission image | `/NOTICE.txt` | `./scripts/generate-go-notice.sh kubernetes /tmp/admission-notice.txt ./cmd/admission` |
| Node provisioner image | `/NOTICE.txt` | `./scripts/generate-go-notice.sh core /tmp/provisioner-notice.txt ./shim/cmd/containerd-shim-brewlet-v2 ./cmd/brewlet-metrics-exporter ./cmd/brewlet-source-policy` |
| Helm chart | No third-party code is embedded in the chart package | — |

The provisioner image build appends the `kubectl`, `ctr`, and `crictl` licenses
to the generated notice, so the image copy is longer than the script output
alone.

Base-image package copyright files remain in their original filesystem
locations inside the image and are not merged into `/NOTICE.txt`; see the
[Linux legal metadata instructions](https://aka.ms/mcr/osslinuxmetadata).

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
