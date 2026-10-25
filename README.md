# Brewlet

[![Full validation](https://github.com/microsoft/brewlet/actions/workflows/full-validation.yml/badge.svg?branch=main)](https://github.com/microsoft/brewlet/actions/workflows/full-validation.yml)
[![Go version](https://img.shields.io/github/go-mod/go-version/microsoft/brewlet)](go.mod)
[![License](https://img.shields.io/github/license/microsoft/brewlet)](LICENSE.txt)

**Run applications from OCI artifacts using runtimes managed once on each
Kubernetes node.**

Brewlet moves language runtimes into infrastructure, alongside the operating
system and Kubernetes. Developers publish application code, dependencies and
explicit launch metadata as standard OCI artifacts. Platform teams approve
digest-pinned runtime images and control which node pools provide them.
Kubernetes schedules each workload onto a compatible node, and containerd starts
it through the Brewlet runtime shim.

This separates application delivery from runtime maintenance. Platform teams can
rotate approved runtimes without rebuilding application artifacts, while
developers retain explicit control over their application's command and
dependencies. Java, Python, Node.js and .NET use the same language-neutral
contract; Java project intelligence belongs only to the optional Maven producer.

This repository contains the complete Brewlet 1.0.0 implementation: the CLI and
runtime, Kubernetes platform, node provisioner, Maven plugin, specifications,
integration tests, website, and user-facing documentation.

## Project status

> [!IMPORTANT]
> **Brewlet 0.8.0 is a pivot.** Brewlet is now a language-agnostic runtime
> governance solution. It still does most of what the Java-specific 0.7.x line
> did, now tracked in the
> [`brewlet-0.x-java`](https://github.com/microsoft/brewlet/tree/brewlet-0.x-java)
> branch, but it is no longer specific to Java. See
> [Brewlet 0.x vs. 1.0.0](https://brewlet.sh/docs/legacy-comparison/).
>
> **All 0.x releases are betas and are not stable.** They can break, change
> incompatibly or pivot again between versions. General availability (GA)
> will be 1.0.0.

Brewlet 1.0.0 is a clean-slate, pre-GA implementation under active development.
There is no published stable 1.0.0 installer in this workflow. Evaluate matching
source-built components in a disposable environment, not a production or shared
cluster. Do not substitute the 0.x CLI, chart, artifact format or runbooks.

The [acceptance ledger](acceptance/requirements.json) records scoped successful
local evidence for all 111 requirements. This is not cloud-provider autoscaling
certification, physical alternate-architecture hardware qualification, or proof
of a published stable release. Unit and cross-compilation results remain
distinct from real-node evidence. See the
[exact sources, commands and results](acceptance/RESULTS.md) and
[verification boundaries](docs/verification.md).

- [Documentation](docs/index.md)
- [Getting started](docs/local-kubernetes.md)
- [Ops workshop](docs/workshops/platform.md)
- [Developer workshop](docs/workshops/developer.md)
- [Prerelease distribution and recovery](docs/supported-release.md)
- [Specification](specs/SPECIFICATION.md)
- [Reference provenance](REFERENCE.md)

## How it works

```text
 Platform operator                   Developer
 -----------------                   ---------
 RuntimeImage: approved runtime      Application + dependencies
 source pinned to an OCI digest      + explicit launch.json
 NodeProfile: placement and policy   packaged as an OCI artifact
               |                                   |
               v                                   v
 +----------------------------+      +----------------------------+
 | Node provisioner           |      | OCI registry               |
 | installs the shim and      |      | application layers under   |
 | approved runtime roots     |      | /app and launch metadata   |
 +-------------+--------------+      +-------------+--------------+
               |                                   |
               |                                   v
               |                     +----------------------------+
               |                     | Admission webhook          |
               |                     | validates the explicit     |
               |                     | runtime ID and placement   |
               |                     +-------------+--------------+
               |                                   |
               |                       runtimeClassName: brewlet
               |                       brewlet.sh/runtime-image: ID
               |                                   |
               v                                   v
 +----------------------------------------------------------------+
 | Brewlet-enabled node                                           |
 | containerd -> Brewlet shim -> approved node-resident runtime     |
 +--------------------------------+-------------------------------+
                                  v
                             Application
```

1. The platform operator approves `RuntimeImage` inventory with digest-pinned
   sources and defines placement and installation policy through `NodeProfile`
   resources.
2. The node provisioner installs and validates the containerd shim and runtime
   roots, advertises capabilities, and removes the startup fence only when the
   node is ready. OnDemand policy can defer fetching a root until first use.
3. The developer packages application and dependency layers confined to `/app`,
   with an explicit, language-neutral `launch.json`, and publishes the artifact.
4. An ordinary Kubernetes workload selects `runtimeClassName: brewlet` and an
   explicit `brewlet.sh/runtime-image` annotation. Admission validates its runtime
   requirements and placement; Kubernetes schedules it onto a compatible node.
5. The shim assembles the application's OCI bundle and launches it with the
   selected runtime generation. Running tasks retain that generation across
   rotations.

There is no language detection, default runtime selection, JVM flag injection,
`JavaApplication` resource, or 0.x compatibility. The shared RuntimeClass reserves
`64Mi` memory and `50m` CPU per pod for shim and staging overhead, not for the
language runtime. Workload requests and limits must cover the entire application
process, including its interpreter or VM.

## Subprojects

| Subproject | Path | Responsibility |
| --- | --- | --- |
| Core runtime | [`cmd/`](cmd/), [`pkg/`](pkg/), [`shim/`](shim/) | Provides the Brewlet CLI, artifact tooling, shared runtime packages, and containerd Runtime v2 shim. |
| Node provisioner | [`provisioner/`](provisioner/) | Installs, rotates and safely removes approved runtime roots, the shim and owned containerd configuration on Linux nodes. |
| Kubernetes platform | [`kubernetes/`](kubernetes/) | Provides the operator, admission webhook, `RuntimeImage` and `NodeProfile` APIs, manifests, RBAC, and Helm chart. |
| Maven plugin | [`maven-plugin/`](maven-plugin/) | Packages Java applications and dependency layer sets through the language-neutral CLI; it does not deploy workloads. |
| Specifications | [`specs/`](specs/) | Defines the authoritative architecture, artifact formats, runtime contracts, APIs, and compatibility rules. |
| Integration tests | [`integration-tests/`](integration-tests/) | Exercises application producers, runtime execution, Kubernetes placement, provisioning, rotation and cleanup. |
| Acceptance | [`acceptance/`](acceptance/) | Tracks requirements, scoped evidence, and strict promotion qualification. |
| Website | [`site/`](site/) | Contains the static landing page, branding assets, and searchable MkDocs documentation preview. |
| User documentation | [`docs/`](docs/) | Contains installation, configuration, operations, workload deployment, troubleshooting, and workshop guidance. |

## Quick start

### Build the CLI from source

Use Go 1.26 or newer and a reviewed source revision containing this 1.0.0
implementation:

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

### Run your first Kubernetes application

Follow the [local Kubernetes tutorial](docs/local-kubernetes.md) to create a new
disposable kind cluster, build matching components, run a real Python HTTP
application, rotate its runtime without rebuilding the application, and remove
the installation normally. The tutorial uses a private kubeconfig and does not
require host Java, Node.js or .NET toolchains.

Nodes require Linux, containerd 2.x, runc, cgroup v2 and the supported host layout.
Provisioning is privileged and modifies host configuration. The
[installation guide](docs/installation.md) covers explicitly selected pools,
reviewed component images, CRDs, chart installation and runtime approval.
Installing the chart alone does not provision a runtime or prove that an
application can start.

Do not run installation or acceptance commands against an unspecified current
Kubernetes context. Follow [safe removal](docs/uninstallation.md) before
uninstalling or reinstalling; do not bypass cleanup finalizers.

### Publish application artifacts

`brewlet build REF` validates and packages a Brewlet application into a local
OCI layout (`.brewlet/images/<REF>` unless `--store` is given). `brewlet push REF`
then publishes that build. `push` also publishes an existing OCI layout given
with `--store DIR` without changing its bytes or digests; use `--source` to
select a local reference or SHA-256 digest when the layout has several roots.
`push` verifies blob identities, not Brewlet launch or layer policy; consumers
still validate the artifact.

The launch schema accepts optional top-level `runtimeImages`; the former
`requires` wrapper is rejected. See [application packaging](docs/applications.md)
and the [language recipes](docs/language-recipes.md) for explicit launch examples.

Registry clients honor `SSL_CERT_FILE` for an additional trusted CA and
origin-bound `BREWLET_REGISTRY_USERNAME` / `BREWLET_REGISTRY_PASSWORD` credentials.
Use `--registry-scheme https` for a TLS-enabled loopback registry. TLS verification
remains enabled; `--insecure` explicitly selects HTTP instead. See
[registry trust](docs/registry-trust.md) for configuration.

## Build and test

Development requires Go 1.26 or newer. The optional Maven producer requires
Maven and JDK 17+; Python 3.12+ is used by the validation tooling.

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

## Contributing and support

Use [GitHub Issues](https://github.com/microsoft/brewlet/issues) for bugs and
feature requests. Include the source revision, relevant configuration, and
reproduction steps without credentials or application secrets.

For implementation changes, follow the
[specification](specs/SPECIFICATION.md), keep directly related documentation
current, and distinguish portable contract results from real-node evidence.
See the [security guidance](docs/security.md) for trust boundaries and
[troubleshooting](docs/troubleshooting.md) for operational diagnostics.

## Releases

Brewlet is pre-GA. [`release.json`](release.json) defines the release
identity. Every 0.x release (0.8.0 onward previews the 1.0.0 line) is a beta:
published as the latest GitHub release, but not stable and free to break or pivot.
GA will be 1.0.0.

| Artifact | Intended location |
| --- | --- |
| CLI archives and checksums | [GitHub releases](https://github.com/microsoft/brewlet/releases) |
| Operator image | `ghcr.io/microsoft/brewlet-operator:VERSION` |
| Admission webhook image | `ghcr.io/microsoft/brewlet-admission:VERSION` |
| Node provisioner and shim image | `ghcr.io/microsoft/brewlet-node-provisioner:VERSION` |
| Helm chart | `oci://ghcr.io/microsoft/charts/brewlet` |
| Optional Maven producer | `sh.brewlet:brewlet-maven-plugin:VERSION` |

Targets are Linux and macOS `amd64`/`arm64` for the CLI and Linux
`amd64`/`arm64` for component images. Pushing a `vVERSION` tag on a commit that
passed CI on `main` builds, verifies and publishes every artifact automatically.
See the [release distribution runbook](docs/supported-release.md) and
[compatibility policy](docs/compatibility-policy.md).

The 1.0.0 rewrite is inspired by Brewlet 0.x at
`fa7085335b220e29df6affbece4558c5f1259ecb`; its specification, label contract
and runbooks are not carried forward. See [reference provenance](REFERENCE.md).

## License

Brewlet is licensed under the [MIT License](LICENSE.txt).

## Third-party software

Brewlet builds on third-party open source software. The repository-level
[NOTICE.txt](NOTICE.txt) indexes the notices for each built artifact and the
commands that generate them. CLI archives, images, JARs, the chart and the
website preview carry their applicable legal resources. See the
[third-party notice documentation](docs/third-party-notices.md) for locations
and verification.

## Trademarks

This project may contain trademarks or logos for projects, products, or
services. Authorized use of Microsoft trademarks or logos is subject to and
must follow [Microsoft's Trademark & Brand
Guidelines](https://www.microsoft.com/en-us/legal/intellectualproperty/trademarks/usage/general).
Use of Microsoft trademarks or logos in modified versions of this project must
not cause confusion or imply Microsoft sponsorship. Any use of third-party
trademarks or logos are subject to those third-party's policies.
