# Brewlet integration tests

Cross-component end-to-end harness for the Brewlet monorepo. This directory owns
test orchestration and fixture applications. Production code, Kubernetes
manifests, and specifications remain in their owning monorepo directories.

## Layout

```text
e2e/       runner, reset helper, shared library, and tiers 1-17
fixtures/  harness-owned Java demo applications and PetClinic build fixture
```

## Running from the monorepo

From the repository root, run:

```bash
integration-tests/e2e/run.sh --tier 1 --tier 2
```

The harness defaults to `core/` and `kubernetes/` in the monorepo. The
`BREWLET_CORE_DIR` and `BREWLET_KUBERNETES_DIR` overrides remain available for
testing external checkouts.

## Running

```bash
integration-tests/e2e/run.sh                 # all 17 tiers
integration-tests/e2e/run.sh --list          # tier catalog
integration-tests/e2e/run.sh --tier 4        # one tier
integration-tests/e2e/run.sh --reset         # remove Brewlet test state
integration-tests/e2e/run.sh --reset --tier 10
E2E_WORK=/tmp/brewlet-e2e integration-tests/e2e/run.sh --tier 9
```

Tiers skip when an optional host prerequisite is unavailable and fail only when
an exercised capability fails. PR CI sets `E2E_REQUIRE_ALL=true`, which also
fails skipped assertions and tiers with no passing assertions. Tier 2 tests
Maven goals without repeating the unit suite; run `make maven-plugin-check`
for Maven unit coverage. The suite covers:

| Tier | Scope | Primary components |
|---:|---|---|
| 1 | Go unit/component suites | core + Kubernetes |
| 2 | CLI push, inspect, run, bundle, classpath, and JPMS; Maven plugin `config`, `inspect`, `build`, `appcds`, and `push` | core + maven-plugin + fixtures |
| 3 | shim to runc under Linux cgroups | core + fixtures |
| 4 | operator control plane and Helm packaging | Kubernetes |
| 5-6 | host and in-cluster admission webhooks | Kubernetes |
| 7 | Spring PetClinic artifact, layered deployment, runc, reconcile | both + fixtures |
| 8-9 | AppCDS lifecycle, cross-namespace cache isolation, and serving through kubelet/CRI | core + fixtures |
| 10-11 | installed Helm stack and webhook resilience | Kubernetes |
| 12 | runnable image pulled and unpacked by kubelet | core + fixtures |
| 13 | NodeProfile lifecycle | Kubernetes |
| 14 | retired: every JDK is NodeProfile-sourced, so its coverage moved to tier 18 and NodeProfile envtests | — |
| 15 | live opt-in Prometheus metrics through Helm, provisioner, shim, and exporter | both + fixtures |
| 16 | SPECIFICATION §14 failure modes on a live node | core + fixtures |
| 17 | default-enabled runnable-stage GC: fresh activation, installation safety blocking, reference protection, and reclamation | both + fixtures |
| 18 | containerd restart rollback, sourced JDK + `jaz` launcher provisioning, and patched JDK digest rollout under a live workload | both + fixtures |
| 19 | dependency CVE remediation across environments | both + fixtures |

Run Tier 17 alone against a dedicated fresh node. It refuses existing shim,
installation-record, or nonempty staging-root state; `--reset` only resets
Kubernetes test state and does not make a used node fresh. A multi-tier run
(including the default all-tiers run) therefore runs Tier 17 before every other
Kubernetes tier, uses the first fresh node, and reports SKIP with instructions
if no node is fresh; run alone, Tier 17 FAILs on a used node. The CI matrix
runs it separately on a new kind cluster. Never clear retained host files or
manufacture a safety record to satisfy this preflight.

See [AGENTS.md](AGENTS.md) for cluster requirements, cleanup, and troubleshooting.

PR selection is documented in [Contributing](../CONTRIBUTING.md#pr-coverage-and-merge-gate).
`python3 integration-tests/e2e/live/smoke.py` runs one install/provision/publish/
deploy/serve cycle against its own disposable cluster using checkout-built
components and the Maven plugin's generated manifest. It is the only cluster
smoke in PR CI; exhaustive cluster tiers and live scenarios run nightly or by
explicit E2E dispatch, including pre-merge validation of high-risk changes.
It shares the strict live fixtures' prerequisites, cleanup, evidence, and
cold-start GC deferral; it is not a replacement for the full nightly scenarios.

### Monitoring runs from the GitHub Copilot app

Enter `/e2e` in a GitHub Copilot app session for this repository to open or
focus the **Brewlet E2E Tests** canvas. The command is a repository skill in
`.github/skills/e2e/SKILL.md`; if it was added during an existing session, use
`/skills reload` to discover it. Opening the panel does not start, reset, or
stop tests. Select a suite and start a run from the canvas when ready.

The project extension in `.github/extensions/e2e-monitor/` adds an
**Integration tests** canvas. It starts `run.sh` tiers, the live HPA/admission
scenarios, or the offline safeguard tests as detached processes, and shows
per-tier progress and ETA, assertion results, failures, a live log, and the
`E2E_WORK` artifacts. Run records are kept in the session's state directory
under `e2e-runs/`, so they survive extension reloads.

When the selected kube context is a live cluster (anything other than Docker
Desktop, kind, k3d, minikube, Rancher Desktop, OrbStack or Colima), the canvas
shows a **Node pool** picker listing the cluster's pools and their Ready nodes,
defaulting to `javaworkers`. The run gets `E2E_NODE_ACCESS=kubectl` and
`E2E_POOLS=<pool>` automatically. Values set in **Environment overrides** win.

## Kubernetes CLI API integration

The process/API integration suite lives in the existing Kubernetes Go module
so it can reuse envtest and the production controllers:

```bash
make -C kubernetes test-cli-integration
```

It builds the CLI and invokes real kubectl/Helm against a fresh API server and
etcd using only private fixture kubeconfigs. No Docker daemon, existing cluster,
or tier reset is used. Missing prerequisites fail this explicit target. The
Kubernetes pull-request CI job runs the same tests.

See [CLI integration tests](../kubernetes/README.md#cli-integration-tests) for
coverage and limitations. In particular, fixture readiness and node inventory
are simulated; this is not coverage of host provisioning or JVM execution.
