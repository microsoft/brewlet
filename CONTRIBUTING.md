# Contributing to Brewlet

Thank you for your interest in Brewlet. Contributions of code, tests,
documentation, and issue reports are welcome.

## Before you start

- Search the existing issues and pull requests before opening a new one.
- Open an issue before making a substantial behavioral or architectural change.
- Follow the [Microsoft Open Source Code of Conduct](CODE_OF_CONDUCT.md).
- Report security vulnerabilities privately as described in
  [SECURITY.md](SECURITY.md).

## Local prerequisites

Run commands from the repository root unless noted. For core development,
install [Go 1.26+](https://go.dev/dl/), GNU Make, and Bash. The
`go.mod` files specify the minimum Go version. For the full `make check-all`
path, also install [Python 3](https://www.python.org/downloads/),
[Maven 3.9+](https://maven.apache.org/download.cgi),
a full [JDK 21+](https://adoptium.net/), [Helm](https://helm.sh/docs/intro/install/),
and `unzip`. Maven itself supports building the plugin on JDK 17+, but JDK
21+ also covers the host-only JVM integration tiers. Ensure `java`, `javac`,
and `mvn` use the intended JDK (`JAVA_HOME` if necessary). Go, Maven, and
envtest fetch dependencies and test binaries on their first run; allow network
access for initial setup.

Confirm the toolchain before running the full suite:

```bash
go version
java -version
javac -version
mvn -version
helm version --short
python3 --version
```

Install tools as needed for the area you change:

| Work | Additional software |
| --- | --- |
| Core CLI and Go unit tests | Go 1.26+; no cluster or Docker daemon |
| Kubernetes control-plane tests and chart | Helm; `make -C kubernetes test-envtest` downloads the pinned `setup-envtest` tool and Kubernetes API server/etcd assets |
| Node provisioner and component container images | [Docker](https://docs.docker.com/get-docker/) with Buildx; multi-architecture build tests also need working QEMU/Buildx support |
| Cluster-dependent integration tiers | [`kubectl`](https://kubernetes.io/docs/tasks/tools/), a reachable **disposable** Kubernetes cluster (such as [kind](https://kind.sigs.k8s.io/)), and, for node-side tiers, Docker, containerd 2.0+, cgroup v2, and a Linux node; some tiers need OpenSSL and cert-manager |
| Website preview | Python 3 and the packages in `site/requirements.txt`; [Node.js 24](https://nodejs.org/en/download) and npm for notice generation |

macOS is suitable for builds and host-only tests, but it cannot execute the
Linux containerd shim or node-side cluster tests natively. Do not run the
privileged provisioner on a shared or production cluster: it changes the host's
containerd configuration. See the [E2E runbook](integration-tests/AGENTS.md)
for prerequisites and scoped cleanup before running cluster tiers.

## Monorepo map

This is **not** one Go workspace: run Go commands in the owning module (or use
`go -C <module>`). In particular, `go test ./...` at the repository root does
not test all components.

| Path | Ownership and entry points |
| --- | --- |
| [`core/`](core/) | Go module for `cmd/brewlet` (CLI), `shim/cmd/containerd-shim-brewlet-v2` (node runtime), `cmd/brewlet-metrics-exporter`, `cmd/brewlet-source-policy`, and shared artifact/runtime packages. |
| [`kubernetes/`](kubernetes/) | Separate Go module for `cmd/manager` (operator), `cmd/admission` (webhook), APIs and controllers in `api/` and `internal/`, raw manifests in `deploy/`, and Helm chart in `charts/brewlet/`. Its own `Makefile` owns envtest and chart checks. |
| [`provisioner/`](provisioner/) | Privileged node-installation scripts and Dockerfile; its image builds the shim and helpers from `core/`. |
| [`admission/`](admission/) | Optional Ratify/Gatekeeper policies and a separate Go module in `ratify-verifier/`; the verifier uses `core/pkg/attest` via a local module replacement. |
| [`maven-plugin/`](maven-plugin/) | Maven plugin (`pom.xml`, `src/`) for building/publishing application artifacts and generating workload manifests. |
| [`integration-tests/`](integration-tests/) | Cross-component E2E harness, Java fixtures, and benchmarks; read its `AGENTS.md` before running cluster tiers. |
| [`specs/`](specs/) | Authoritative architecture, artifact, and API contracts; proposals live in `specs/proposals/`. |
| [`docs/`](docs/) and [`site/`](site/) | User/operator documentation and workshops; website assets, MkDocs configuration, installer, and site checks. |
| [`scripts/`](scripts/) and [`.github/workflows/`](.github/workflows/) | Repository-wide policy, release, license/notice checks, and CI jobs. |

The [specification](specs/SPECIFICATION.md) is the source of truth when a
change crosses modules. Keep implementations, CRDs, chart templates, examples,
tests, and user documentation consistent. In particular, the chart and raw
`NodeProfile` CRDs must remain in sync (`make -C kubernetes helm-check` checks
this).

## Build and test locally

For a first pass, build the CLI and shim, confirm the CLI starts, and run the
core checks (no Maven, Helm, or cluster required):

```bash
make binaries                 # outputs bin/brewlet and bin/containerd-shim-brewlet-v2
./bin/brewlet version
make check                    # core build, race tests, vet, formatting and policy checks
```

`bin/` is local build output, not a source directory. The shim can be built on
macOS, but executing it requires Linux/containerd. For a change spanning
components, run all non-cluster checks:

```bash
make check-all
```

`check-all` adds Kubernetes envtest and Helm checks, Maven plugin verification,
Ratify verifier checks, site contracts, offline E2E suite contracts (Node 24+
and Python 3), and host-only E2E tiers 1-2. The E2E
runner may skip optional tier-2 cases (for example, registry/referrer tests
without Docker and Maven); inspect its PASS/SKIP summary rather than treating
a zero exit as complete coverage. It does **not** prove that the shim or
provisioner works on a real node. Run focused checks while developing:

```bash
go -C core test ./...                         # core only
make -C kubernetes operator-build admission-build
make -C kubernetes ci                         # Go, envtest, Helm chart
make -C kubernetes test-cli-integration        # CLI against isolated envtest API
make maven-plugin-check
make admission-check
make site-contract-check
make e2e-contract-check                      # routing and monitor history; no cluster
bash provisioner/entrypoint_test.sh
make container-security-check
```

The explicit Kubernetes CLI integration target requires Go and Helm and runs
with a private envtest API server, not your current kube context. For image
changes, use `make provisioner-image` and, with a configured Docker Buildx
builder, `make container-security-test`. For a quick documentation preview, see
[`site/README.md`](site/README.md#local-preview).

To exercise a particular E2E tier, first read the
[harness runbook](integration-tests/AGENTS.md) and list the current tiers:

```bash
integration-tests/e2e/run.sh --list
integration-tests/e2e/run.sh --tier 1 --tier 2  # host-only, no cluster
```

Cluster tiers require an explicitly selected disposable context and may skip
when prerequisites are missing; a successful run with skips is not a live
validation pass. The separate `integration-tests/e2e/live/` scenarios have
stricter requirements and their own cluster lifecycle; follow
[`docs/live-validation.md`](docs/live-validation.md) instead of using the
tiered suite's reset helper for them. The E2E workflow accepts `suite: all`,
`tiers`, or `live`; `tiers` preserves all 19 tiers and arm64 coverage. The
`scenario` selector applies only to live jobs.

### Registry conformance (Go and Maven)

`core/internal/registry/testdata/conformance.json` is the shared, table-driven
contract for the Go registry publisher and Maven registry client. Add overlapping
cases there rather than copying expected datasets between languages. Go embeds
the file in its test binary; Maven copies that same file onto the test classpath
via `${project.basedir}`. Both consumers run in the standard component suites,
independent of the invocation directory. Missing or malformed fixtures fail the
tests.

The cases cover publishing-reference validation and defaults, Docker Hub
normalization, Docker-config/helper/environment credential selection, identity
tokens, exact insecure authorities, token-realm trust, and request-level
credential scoping across redirects. The request tests use ephemeral loopback
HTTP servers; credential tests use temporary Docker configs and injected
environment/helper inputs, never the developer's login or credential helpers.
No external registry, Docker daemon, or cluster is required.

Run the focused suites from the repository root:

```bash
go -C core test ./internal/registry/...
mvn -B --no-transfer-progress -f maven-plugin/pom.xml -Dmaven.compiler.release=17 \
  '-Dtest=*ConformanceTest,RegistryClient*Test,RegistryTrustPolicyTest,CredentialResolverTest,PublishingReferenceTest' test
```

Then verify the owning components:

```bash
go -C core test -race ./internal/registry/...
go -C core build ./...
mvn -B --no-transfer-progress -f maven-plugin/pom.xml -Dmaven.compiler.release=17 verify
```

`RegistryClientMountTest` publishes a real managed runnable image to a local
request-recording registry. It checks mount success without body upload,
declined mounts reusing returned upload locations (relative, absolute, and
cross-origin), unsupported-mount fallback, source/destination authentication
scopes, existing blobs, child manifests before the tagged index, and mount,
upload, token, blob-check, and manifest errors. Existing `PublishingReferenceTest`
retains the broader digest-destination, digest-source, and publishing-default
regressions; `CredentialResolverTest` retains Maven settings decryption coverage.

Intentional boundaries are not parity failures:

| Surface | Contract |
| --- | --- |
| Maven settings | Maven checks `settings.xml` first and fails explicitly on decryption errors; Go has no Maven settings source. Shared credential cases exercise the Docker/helper/environment chain without settings. |
| Remote application destination | Both publishers require an explicit registry and a mutable tag (omitting the tag means `latest`). Maven can derive the destination from `registry` and project coordinates. |
| Bundle/local references | Maven's dependency-bundle goal and low-level reference helpers retain implicit Docker Hub defaults; Go's remote push parser does not. Digest source references and child-manifest addresses remain valid, unlike top-level publish destinations. |
| Managed bundles | Maven can consume registry-hosted managed bundles and mount their layers. Go's managed-bundle composition remains local-layout-only; its remote publisher does not gain bundle consumption or mounting. |

This is a bounded Brewlet contract, not a claim of complete OCI Distribution
conformance or interchangeable parsers for every possible registry authority.

## Development workflow

1. Fork the repository and create a focused branch. Search existing issues and
   pull requests, and discuss substantial behavioral or architectural changes
   in an issue first.
2. Make the smallest coherent change, with tests and documentation for changed
   behavior. Keep changes in their owning component; do not duplicate contracts
   or verification logic across modules.
3. Run the relevant component checks above, then `make check-all` when your
   local prerequisites allow it. State which commands ran and any skipped or
   unavailable checks in the pull request.
4. Open a pull request using the repository
   [template](.github/PULL_REQUEST_TEMPLATE.md), including compatibility,
   security, and operational impact.

For automated agents as well as human contributors: inspect the owning module's
README and scripts before editing; run commands from the intended directory;
never assume a skipped E2E tier passed; avoid mutating a user's Kubernetes
context or deploying the privileged provisioner without an isolated disposable
cluster. Do not commit generated build outputs, credentials, or local test
artifacts. For changes to public behavior, use the specification and existing
tests as the contract, not an unimplemented roadmap proposal.

## Changing GitHub Actions workflows

### PR coverage and merge gate

The `CI` workflow always starts on PRs targeting `main`; it does not use
workflow-level path filters. `scripts/ci_plan.py` compares the complete PR
head against its merge base, including removed paths and both sides of
renames, then selects jobs by dependency impact. Unknown paths, shared
CI/build/specification changes, unavailable diff information, and empty diffs
select all component checks and smoke tests. Pushes to `main`, scheduled CI, and
manual CI runs also select this full CI set, never the exhaustive E2E scenarios.

| Change | Required coverage |
| --- | --- |
| Every PR | Source headers, workflow and Dockerfile policy, routing/gate tests, release-version guard, offline site/installation and E2E fixture contracts; CodeQL runs separately |
| Docs/site only | Strict documentation build and website notices; no runtime or image jobs |
| Core | Go checks, downstream Kubernetes CLI integration and Ratify tests, notices, released CLI cross-builds, container builds, host tiers 2-3, and fresh-cluster smoke |
| Kubernetes | Go race tests, envtest and CLI integration, Helm render checks, notices, container builds, and fresh-cluster smoke |
| Maven or shared registry/artifact contracts | Maven verification on the JDK 17 baseline, applicable host tests, and Maven deployment smoke; Maven source/packaging changes also run the Central dry run |
| Kubernetes API, raw manifests, or chart | Maven JDK 17 verification and deployment smoke in addition to Kubernetes/chart checks |
| Provisioner | Source-policy checks, images, host tier 3, and fresh-cluster smoke |
| Ratify admission | Verifier component checks; exhaustive live enforcement remains in E2E |

The routing table in `ci_plan.py` is authoritative and deliberately conservative.
When adding a component, shared fixture, or dependency, update its routing and
regression cases together. Tier 1 is not repeated in PR smoke jobs. Tier 2
installs the plugin without repeating Maven unit tests; the single JDK 17
baseline job owns those tests and the notice checks. Helm lifecycle tests run once in the Kubernetes race suite
with Helm and envtest required; `helm-render-check` retains the separate static
chart checks and the local `helm-check` target still includes lifecycle tests.

Container jobs retain native provisioner inspection and builds of both Kubernetes
entry points on amd64/arm64, reusing one Buildx builder. Corrupt-download builds
run for provisioner Dockerfile/download/checksum/test changes and full runs, not
ordinary source edits. The separate `E2E` workflow still runs the complete
tier matrix and native arm64 tests nightly/manually, including repeated
fresh-cluster live scenarios. AppCDS (8), metrics (15), GC (17), JDK rollout (18),
dependency remediation (19), and the comprehensive admission/HPA/workflow
scenarios belong only to E2E, not the CI merge gate, main-push CI, or scheduled CI.
For high-risk changes, explicitly dispatch E2E on the candidate branch before
merging and inspect its results; it is not automatically covered by `PR checks`.
CI runs only one fresh-cluster smoke, without repeating those scenario matrices.

PR tiers set `E2E_REQUIRE_ALL=true`: skipped assertions or a tier with no
passing assertions fail the job. The small `live/smoke.py` scenario uses checkout-built
components, a private registry and cluster, a real provisioned JDK, and a
real Maven `deploy` invocation to publish, generate the JavaApplication manifest,
apply it against the installed CRD, and wait for readiness. Its digest-pinned
workload must serve HTTP; this catches plugin/schema drift without running the
comprehensive workflow scenarios. It retains the strict
fixture's documented cold-start GC deferral; it does not establish arbitrary
containerd-GC timing or replace the dedicated GC/HPA scenarios. Evidence is
redacted and retained even on failure.

`PR checks` is the aggregate merge check. It requires the selector and repository
safeguards to succeed, every selected job to succeed, and every unselected job
to be explicitly skipped. Failures, cancellations, missing selections, or
unexpected skips fail the gate. After the workflow has produced a successful
check on GitHub, configure **PR checks** as required in the repository's `main`
ruleset, preserving any independent requirements such as CodeQL. Do not require
the path-conditional component checks individually, and do not enable the new
requirement before GitHub can produce it. Workflow files do not change repository
rules automatically.

Validate routing and workflow wiring without containers:

```bash
make ci-contract-check e2e-contract-check
make -C kubernetes helm-render-check
```

Compare recent Actions job durations and queue times after rollout; no fixed
speedup is assumed. Unrelated PRs should avoid image/cluster work, while runtime
and shared-infrastructure PRs intentionally retain more expensive coverage.

Workflows are part of the release supply chain, so `make workflow-security-check`
enforces two rules that CI will not let you skip:

- **Every `uses:` must pin a full 40-character commit SHA**, followed by a comment
  naming the version it resolves to — for example
  `uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1`.
  A version tag can be moved by whoever controls the action repository; a commit
  SHA cannot. Resolve one with:

  ```bash
  gh api repos/<owner>/<repo>/commits/<tag> --jq .sha
  ```

  Dependabot keeps these pins current, so you rarely need to bump one by hand.

- **`write` permissions belong on jobs, not on the workflow.** Declare
  `permissions:` at workflow scope with read-only values, then grant the narrow
  write scopes on the individual job that publishes. This keeps build-only jobs
  from carrying the ability to replace a release or a package.

Actions inside this repository (`uses: ./...`) are exempt; they are already read
from the checked-out commit.

## Pull request requirements

- Keep commits and pull requests focused.
- Follow the [pre-GA compatibility policy](docs/compatibility.md). Superseded
  Brewlet interfaces have no automatic compatibility obligation; retaining an
  alternative requires a current use case and documented maintainer support
  decision. Propose removals explicitly, document incompatible changes and
  operator actions, and preserve named exceptions and operational safeguards.
- Pre-GA release updates default to safe teardown/reinstallation. An in-place
  exception must name its source/target releases, scope, prerequisites,
  validation evidence, and recovery limits; migration code alone is not a promise.
- Do not include credentials, proprietary data, or unrelated generated files.
- Ensure commits contain only work that you have the right to contribute.
- Add the language-appropriate Microsoft MIT copyright header to every new
  source file. Run `make license-check` to verify coverage.

This project welcomes contributions and suggestions. Most contributions require
you to agree to a Contributor License Agreement (CLA) declaring that you have
the right to, and actually do, grant us the rights to use your contribution. For
details, visit <https://cla.opensource.microsoft.com>.

When you submit a pull request, a CLA bot will determine whether you need to
provide a CLA and decorate the pull request appropriately. Follow the bot's
instructions if action is required.
