# Brewlet

[![CI](https://github.com/microsoft/brewlet/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/microsoft/brewlet/actions/workflows/ci.yml)
[![E2E](https://github.com/microsoft/brewlet/actions/workflows/e2e.yml/badge.svg?branch=main)](https://github.com/microsoft/brewlet/actions/workflows/e2e.yml)
[![Release](https://img.shields.io/github/v/release/microsoft/brewlet?sort=semver)](https://github.com/microsoft/brewlet/releases/latest)
[![Go version](https://img.shields.io/github/go-mod/go-version/microsoft/brewlet?filename=core%2Fgo.mod)](core/go.mod)
[![License](https://img.shields.io/github/license/microsoft/brewlet)](LICENSE.txt)

**Run Java applications from OCI artifacts using JDKs managed once on each
Kubernetes node.**

Brewlet separates the application from the Java runtime. Developers publish
their JAR and launch metadata as a standard OCI artifact, while platform teams
control the JDK distributions and launchers installed on each node. Kubernetes
schedules the workload onto a compatible node and containerd starts it through
the Brewlet runtime shim.

At fleet scale, this model is intended to lower the cost per Java workload,
accelerate runtime security remediation, and reduce operational boilerplate for
both developers and AI-assisted delivery workflows. It also gives platform teams
a central control point for JDK policy, runtime optimization, and upgrades.

This repository contains the complete Brewlet implementation: the CLI and
runtime, Kubernetes platform, node provisioner, Maven plugin, specifications,
integration tests, website, and user-facing documentation.

## Project status

Brewlet is a pre-1.0 preview. It is under active development, and its APIs,
artifact formats, and operational behavior may change between minor releases.
Use a disposable evaluation environment, not a production or shared cluster.
The source and GitHub release downloads are public; no repository credentials
are needed to clone the project or install the CLI.

Live [Ratify/Gatekeeper enforcement (#95)](https://github.com/microsoft/brewlet/issues/95)
passed its required matrix twice on fresh local disposable clusters with the
released verifier/publisher, a corrected Verifier manifest and the fixed shim.
Real CPU HPA scale-up/down has also passed twice on fresh disposable clusters
with a **fixed-shim candidate** over the 0.5.0 components.
The unmodified release exposed a packed-layer GC failure during scale-out;
[metrics-driven scaling (#94)](https://github.com/microsoft/brewlet/issues/94)
and the [live runbook](docs/live-validation.md) distinguish that baseline from
the candidate, fixture-only admission settings and remaining cold-start limits.
Neither result is an unmodified 0.5.0 pass or production certification.
Brewlet 0.5.1 includes the verified warm-reuse fix and corrected Verifier manifest;
the recorded live evidence retains its explicit 0.5.0 baseline and candidate
configuration.
See [preview status and validation](https://brewlet.sh/docs/#preview-status-and-validation)
for the distinction between release smoke, component, and live cluster coverage.

- [Documentation](https://brewlet.sh/)
- [Getting started](https://brewlet.sh/docs/getting-started/)
- [Ops workshop](https://brewlet.sh/docs/workshops/operations/)
- [Developer workshop](https://brewlet.sh/docs/workshops/developers/)
- [Latest release](https://github.com/microsoft/brewlet/releases/latest)
- [Specification](specs/SPECIFICATION.md)
- [Roadmap](ROADMAP.md)

## How it works

```text
 Platform operator                   Developer
 ─────────────────                   ─────────
 NodeProfile: approved JDKs          JAR + launch metadata,
 and launchers, pinned to            packaged by the CLI or
 OCI digests                         the Maven plugin
               │                                   │
               ▼                                   ▼
 ┌────────────────────────────┐      ┌────────────────────────────┐
 │ Node provisioner           │      │ OCI registry               │
 │ installs the shim and JDK  │      │ app JAR and launch spec,   │
 │ roots, labels the node     │      │ addressed by digest        │
 └─────────────┬──────────────┘      └─────────────┬──────────────┘
               │                                   │
               │                                   ▼
               │                     ┌────────────────────────────┐
               │                     │ Admission webhook          │
               │                     │ validates the pod runtime  │
               │                     │ and picks a capable node   │
               │                     └─────────────┬──────────────┘
               │                                   │
               │                       runtimeClassName: brewlet
               │                                   │
               ▼                                   ▼
 ┌────────────────────────────────────────────────────────────────┐
 │ Brewlet-enabled node                                           │
 │ containerd -> brewlet shim -> node-resident JDK                │
 └────────────────────────────────┬───────────────────────────────┘
                                  ▼
                           Java application
```

1. The platform operator installs Brewlet and defines the supported JDK and
   launcher inventory through one or more `NodeProfile` resources. Every runtime
   source is administrator-provided and pinned to an OCI SHA-256 digest.
2. The node provisioner installs the containerd shim and approved runtimes on
   matching nodes, then advertises their capabilities through node metadata.
3. The developer packages a Java application with the CLI or Maven plugin and
   publishes the resulting OCI artifact.
4. The admission webhook validates the runtime requirements and directs the pod
   to a compatible node.
5. The Brewlet shim assembles an OCI bundle and launches the application with
   the selected node-resident JDK.

## Subprojects

| Subproject | Path | Responsibility |
| --- | --- | --- |
| Core runtime | [`core/`](core/) | Contains the Brewlet CLI, artifact tooling, shared runtime packages, and containerd Runtime v2 shim. |
| Node provisioner | [`provisioner/`](provisioner/) | Installs and removes the shim, JDK roots, launchers, and containerd configuration on Linux nodes. |
| Kubernetes platform | [`kubernetes/`](kubernetes/) | Provides the operator, admission webhook, `NodeProfile` and `JavaApplication` APIs, RBAC, manifests, and Helm chart. |
| Maven plugin | [`maven-plugin/`](maven-plugin/) | Builds and publishes Brewlet OCI artifacts and generates Kubernetes workload manifests directly from Maven projects. |
| Specifications | [`specs/`](specs/) | Defines the architecture, artifact formats, runtime contracts, APIs, and compatibility rules. Future work is tracked in the [roadmap](ROADMAP.md). |
| Integration tests | [`integration-tests/`](integration-tests/) | Exercises the CLI, JVM execution, shim, Kubernetes control plane, node provisioning, Helm installation, and representative workloads. |
| Website | [`site/`](site/) | Contains the static landing page, installer, branding assets, and MkDocs configuration published at [brewlet.sh](https://brewlet.sh/). |
| User documentation | [`docs/`](docs/) | Contains installation, configuration, operations, workload deployment, troubleshooting, and workshop guidance. |

## Quick start

### Install the CLI

The checksum-verifying installer selects the latest release's Linux or macOS archive and
installs `brewlet` to `$HOME/.local/bin`. No Go toolchain or GitHub
authentication is required:

```bash
curl -fsSL https://brewlet.sh/install.sh | sh -s -- --version latest --install-dir "$HOME/.local/bin"
export PATH="$HOME/.local/bin:$PATH"
brewlet version
```

Change `--version` to pin a release or `--install-dir` to choose a different
destination (and update `PATH` to match). See [Getting started](https://brewlet.sh/docs/getting-started/)
for the local application example and the
[CLI documentation](https://brewlet.sh/docs/cli-reference/) for the artifact
workflow.

### Alternative: build from source

For Brewlet development, build locally with Go 1.26+ instead:

```bash
git clone https://github.com/microsoft/brewlet.git
cd brewlet
make binaries
export PATH="$PWD/bin:$PATH"
brewlet version
```

### Enable a Kubernetes cluster

Brewlet requires containerd 2.0+, cgroup v2, and permission to run a privileged
host-modifying DaemonSet. Because that DaemonSet mutates the host, the chart has
no every-node default or built-in JDK catalog: name the pools it may provision
and choose the JDK source. Control-plane nodes are excluded by default;
`provisioner.includeControlPlane=true` is an explicit opt-in for single-node
development clusters.

Save this as `my-jdks.yaml`, replacing the placeholder with the full digest of
an administrator-approved JDK image. Verify that the image supports your nodes'
architectures and contains the given `javaHome`; this is a template, not an
approved runtime catalog:

```yaml
provisioner:
  jdks:
    - distribution: temurin
      feature: 21
      source:
        image: docker.io/library/eclipse-temurin@sha256:<64-lowercase-hex>
        javaHome: /opt/java/openjdk
```

Install the latest chart on your disposable cluster:

```bash
helm upgrade --install brewlet oci://ghcr.io/microsoft/charts/brewlet \
  --namespace brewlet \
  --create-namespace \
  --set provisioner.pools="{java-workers}" \
  --values my-jdks.yaml

kubectl get nodes -L brewlet.sh/runtime
brewlet doctor --namespace <developer-namespace>
```

Helm selects the latest chart when `--version` is omitted. Add
`--version x.y.z` to pin a specific release instead; component images remain
digest-pinned by the published chart.

Follow the [installation guide](https://brewlet.sh/docs/installation/) for
source-built component installation, pool labels, scoped node profiles,
configuration, upgrades, and safe removal. Keep your chosen values for upgrades.
If GHCR denies an anonymous pull, see
[package access troubleshooting](https://brewlet.sh/docs/installation/#package-access-troubleshooting);
package visibility is managed separately from repository visibility.

## Build and test

Development requires Go 1.26 or newer. Maven plugin development requires Maven
3.9+ and JDK 17+, while the optional AppCDS integration test requires a full JDK
17 or newer. Docker with Buildx is required for provisioner images.

Run all checks that do not require a Kubernetes cluster:

```bash
make check-all
```

This validates the core runtime, Kubernetes platform, Maven plugin, and
host-only integration tiers. To build the CLI and shim into `bin/`:

```bash
make binaries
```

To run the full tiered integration harness against a suitable local cluster,
see [`integration-tests/AGENTS.md`](integration-tests/AGENTS.md).

## Contributing and support

Contributions are welcome. Read [CONTRIBUTING.md](CONTRIBUTING.md) for the
development workflow and pull request requirements, and review the
[Microsoft Open Source Code of Conduct](CODE_OF_CONDUCT.md) before
participating.

Use [GitHub Issues](https://github.com/microsoft/brewlet/issues) for bugs and
feature requests. See [SUPPORT.md](SUPPORT.md) for support expectations and
[SECURITY.md](SECURITY.md) for confidential vulnerability reporting.

## Releases

Before creating a release tag, update and commit the
[specification version](specs/README.md#versioning-and-citations) to match the
release. The release workflow checks this before publishing any artifacts.

Tags matching `v*` publish version-aligned artifacts:

| Artifact | Location |
| --- | --- |
| CLI archives and checksums | [GitHub Releases](https://github.com/microsoft/brewlet/releases) |
| Operator image | `ghcr.io/microsoft/brewlet-operator:<version>` |
| Admission webhook image | `ghcr.io/microsoft/brewlet-admission:<version>` |
| Node provisioner image | `ghcr.io/microsoft/brewlet-node-provisioner:<version>` |
| Helm chart | `oci://ghcr.io/microsoft/charts/brewlet` |
| Maven plugin JAR and POM | [GitHub Releases](https://github.com/microsoft/brewlet/releases) |
| Maven plugin, sources, Javadoc, and PGP signatures | Maven Central, after the protected publishing job succeeds |

Maven Central publishing requires the `maven-central` GitHub environment,
Portal token credentials, and a PGP signing key. See the
[plugin publishing guide](maven-plugin/README.md#publishing-the-plugin-to-maven-central)
for setup and Central-only retries. Older GitHub releases are not automatically
published to Central.

The Helm chart's `appVersion` selects matching component image tags by default.
Release images support Linux `amd64` and `arm64`; CLI archives support those
architectures on Linux and macOS. Binary archives, JARs, images, and the
published website carry their applicable third-party notices inside the
artifact; container images also carry Microsoft's container legal notice.

## License

Brewlet is licensed under the [MIT License](LICENSE.txt).

## Third-party software

Brewlet builds on third-party open source software. The repository-level
[NOTICE.txt](NOTICE.txt) is an index: it lists the notices checked into this
repository and the commands that regenerate the notices produced at build time,
because each release artifact carries only the notices for the software it
actually redistributes. See the
[third-party notice documentation](docs/third-party-notices.md) for the same
breakdown with the notice location in each artifact.

## Trademarks

This project may contain trademarks or logos for projects, products, or
services. Authorized use of Microsoft trademarks or logos is subject to and
must follow [Microsoft's Trademark & Brand
Guidelines](https://www.microsoft.com/en-us/legal/intellectualproperty/trademarks/usage/general).
Use of Microsoft trademarks or logos in modified versions of this project must
not cause confusion or imply Microsoft sponsorship. Any use of third-party
trademarks or logos are subject to those third-party's policies.
