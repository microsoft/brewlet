# Contributing to Brewlet

Use [GitHub Issues](https://github.com/microsoft/brewlet/issues) for bugs and
feature requests. Include the source revision, relevant configuration, and
reproduction steps without credentials or application secrets.

For implementation changes, follow the
[specification](specs/SPECIFICATION.md), keep directly related documentation
current, and distinguish portable contract results from real-node evidence.
See the [security guidance](docs/security.md) for trust boundaries and
[troubleshooting](docs/troubleshooting.md) for operational diagnostics.

## Prerequisites

Development requires Go 1.26 or newer. The optional Maven producer requires
Maven and JDK 17+; Python 3.12+ is used by the validation tooling.

## Build the CLI from source

```bash
git clone https://github.com/microsoft/brewlet.git
cd brewlet
make build
export PATH="$PWD/bin:$PATH"
brewlet version
```

The build places the CLI, license and generated third-party notices in `bin/`.
Unstamped source builds report `dev`/`unknown`, not a release.
See the [CLI reference](docs/cli-reference.md) for commands and artifact options.

## Build component images from source

Releases publish matching operator/admission and provisioner images; see
[installation](docs/installation.md). To build them from a checkout instead,
use a clean checkout without credentials or private work directories in the
Docker build context. The host must supply containerd 2.x, systemd/systemctl,
crictl, cgroup v2 and the supported overlayfs layout. The default containerd
socket is currently required.

```sh
docker build -f kubernetes/Dockerfile \
  -t registry.example.com/brewlet/control:<version> .
docker build -f provisioner/Dockerfile \
  -t registry.example.com/brewlet/node:<version> .
docker push registry.example.com/brewlet/control:<version>
docker push registry.example.com/brewlet/node:<version>
```

Record the resulting registry digests and use them in reviewed values:

```yaml
operator:
  image: registry.example.com/brewlet/control@sha256:<control-digest>
admission:
  image: registry.example.com/brewlet/control@sha256:<control-digest>
provisioner:
  image: registry.example.com/brewlet/node@sha256:<node-digest>
  profiles: []
```

Replace every placeholder. The empty profile list intentionally provisions no
pool; it is not an accidental catch-all. Configure component image access
through your platform's namespace/service-account policy.

## Install the chart from source

Apply the current CRDs before every chart install/upgrade, then install the
chart from the checkout:

```sh
kubectl --context "$CONTEXT" apply -f kubernetes/deploy/crds/
helm --kube-context "$CONTEXT" upgrade --install brewlet kubernetes/charts/brewlet \
  --namespace brewlet --create-namespace --values reviewed-values.yaml \
  --wait --timeout 5m
```

Helm installation does not by itself mean a node runtime is provisioned.
Review and apply your runtime/profile definitions separately, or supply named
profiles through reviewed Helm values. Continue with
[runtime images](docs/runtime-images.md) and the
[installation guide](docs/installation.md#4-verify-the-deployment).

## Build and test

Run the portable unit and contract checks, Maven verification, and ledger
structure checks:

```bash
make test
```

Build the CLI and its redistribution notices:

```bash
make build
```

Strict promotion qualification is separate from routine checks:

```bash
make acceptance
```

`make acceptance` is deliberately a strict release gate, not a smoke-test alias.
It fails when required behavior lacks verified evidence. Portable tests alone
do not prove Linux mounts, real scheduling or provider behavior. See the
[acceptance guide](acceptance/README.md) and
[operations and verification](docs/README.md) for scope and prerequisites.

Build the static website and searchable documentation with `make site-build`.
The [website workflow](.github/workflows/site-pages.yml) uploads a checked preview
artifact; the separate [Pages deployment workflow](.github/workflows/site-deploy.yml)
publishes that artifact to `brewlet.sh` after website/docs changes reach `main`,
or on manual dispatch from `main`. Pull requests cannot deploy, and the site
does not advertise the old release installer. See the [website guide](site/README.md).

CodeQL scans Actions, Go, Java, C#, JavaScript/TypeScript and Python on pull
requests, pushes to `main`, and a weekly schedule. Dependabot
checks Actions, Go, Maven, npm, pip and NuGet dependencies weekly using the
current repository and fixture manifests.
