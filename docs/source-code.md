# Source code

Brewlet's complete implementation lives in the
[microsoft/brewlet repository on GitHub](https://github.com/microsoft/brewlet).
It includes the CLI, containerd shim, Kubernetes platform, node provisioner,
dashboard, optional Maven plugin, tests, specifications, and this documentation.
The project is open source under the [MIT License](../LICENSE.txt).

The code implements a language-neutral contract: developers publish application
artifacts, while infrastructure supplies approved runtimes on Kubernetes nodes.
Start with [Concepts and architecture](concepts.md) for the execution flow, then
read the [Specification](../specs/SPECIFICATION.md) for the authoritative
requirements.

## Find your way around

| Area | Where to look | What it does |
| --- | --- | --- |
| CLI | [`cmd/brewlet/`](https://github.com/microsoft/brewlet/tree/main/cmd/brewlet) | Packages and inspects application artifacts, publishes them to registries, and provides Kubernetes management commands. |
| Dashboard | [`cmd/brewlet/dashboard/`](https://github.com/microsoft/brewlet/tree/main/cmd/brewlet/dashboard) | Contains the dashboard's HTML, CSS, and browser modules, embedded in the CLI. |
| Shared Go packages | [`pkg/`](https://github.com/microsoft/brewlet/tree/main/pkg) | Implements artifact validation, runtime inventory, node execution, and shared platform logic. |
| Containerd shim | [`shim/`](https://github.com/microsoft/brewlet/tree/main/shim) | Assembles the application's runtime environment and manages tasks through containerd's Runtime v2 interface. |
| Node provisioner | [`provisioner/`](https://github.com/microsoft/brewlet/tree/main/provisioner) | Installs, rotates, and removes approved runtime roots, the shim, and owned containerd configuration. |
| Kubernetes platform | [`kubernetes/`](https://github.com/microsoft/brewlet/tree/main/kubernetes) | Contains the operator, admission webhook, APIs, CRDs, RBAC, and Helm chart. |
| Maven producer | [`maven-plugin/`](https://github.com/microsoft/brewlet/tree/main/maven-plugin) | Packages Java applications and dependency layer sets through the CLI; it does not deploy workloads. |
| Tests and evidence | [`integration-tests/`](https://github.com/microsoft/brewlet/tree/main/integration-tests), [`acceptance/`](https://github.com/microsoft/brewlet/tree/main/acceptance) | Exercises producers and runtime behavior, and records requirements and scoped qualification evidence. |
| Contracts and design | [`specs/`](https://github.com/microsoft/brewlet/tree/main/specs) | Holds the specification and design proposals. |
| Documentation and website | [`docs/`](https://github.com/microsoft/brewlet/tree/main/docs), [`site/`](https://github.com/microsoft/brewlet/tree/main/site) | Contains user guides, the static landing page, and the MkDocs configuration and build tooling. |

For a focused starting point, follow the CLI into `pkg/artifact/` for packaging,
look in `kubernetes/` and `provisioner/` for runtime placement and installation,
or start in `shim/` for task execution. The dashboard's browser modules use
native JavaScript without an npm dependency installation.

## Build and explore locally

Install Git, Make, and the Go version required by [`go.mod`](../go.mod), then:

```bash
git clone https://github.com/microsoft/brewlet.git
cd brewlet
make build
./bin/brewlet version
```

This builds the CLI and its license and third-party notices in `bin/`; it does
not install anything into a Kubernetes cluster. An unstamped source build
reports `dev`/`unknown` rather than a published release identity.

The [contributing guide](../CONTRIBUTING.md) covers component-image builds,
dashboard development, and test commands. The broader `make test` target also
uses Maven, a JDK, Python, and Node.js. Portable tests and real-node qualification
prove different things; see [Verification and limitations](verification.md)
before interpreting results. Use a disposable, explicitly selected cluster for
runtime experiments.

For changes to this documentation or the landing page, follow the
[website build guide](../site/README.md). The site build checks local links and
anchors as well as rendering the pages.

## Contribute or report a problem

Use [GitHub Issues](https://github.com/microsoft/brewlet/issues) for bugs and
feature requests. Include the source revision, relevant configuration, and
reproduction steps, without credentials or application secrets.

Before changing behavior, review the specification and the
[contribution guidance](../CONTRIBUTING.md), and update directly related
documentation. To inspect a published version instead of ongoing development,
start from its [GitHub release](https://github.com/microsoft/brewlet/releases)
and corresponding source tag.
