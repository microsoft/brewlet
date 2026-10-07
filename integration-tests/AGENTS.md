# Brewlet E2E runbook

## Isolated admission, CPU HPA and workflow scenarios

The strict scenarios in `e2e/live/` are independent of the tiered suite below.
Run `python3 integration-tests/e2e/live/hpa.py`,
`python3 integration-tests/e2e/live/admission.py` or
`python3 integration-tests/e2e/live/workflows.py` from the repository root.
`workflows.py` builds the CLI, Maven plugin, operator, admission and provisioner
images from the checkout and drives `brewlet push`, `brewlet k8s app
status|wait`, `status`, `doctor`, `jdk|launcher list|add`, `profile
list|inspect|delete`, `inspect app`, the `install` guards and
`mvn package brewlet:push` with a separate kubectl/CLI deployment handoff end to end.
`retirement.py` provisions a private three-node kind cluster, permanently removes
one invocation-owned worker container and its volumes, and recovers the blocked
profile through `NodeRetirementEvidence`. It verifies denial before an explicit
recovery-role binding, real Java serving on the unprovisioned standby replacement,
an unchanged survivor, and evidence retained after normal profile deletion.
It is provider-neutral recovery coverage, not an AKS decommissioning test.
They create their own uniquely named clusters and registries; never pass a
shared kube context or run the tier reset helper for them. Mandatory assertions
fail instead of skipping. `.github/workflows/e2e.yml` has separate scheduled/manual
live jobs, each running twice on fresh clusters; it has no push/PR triggers.
All scenarios use the same checkout-built runtime, plus the checkout
verifier for admission. There are no historical runtime modes or version selectors.
They require exactly kind v0.33.0 (`go install sigs.k8s.io/kind@v0.33.0`); setup
network failures are retried with backoff and reported as `failureClass:
infrastructure` in `result.json`, never as skipped assertions.

See [the live-validation runbook](../docs/live-validation.md) for release pins,
capacity, load leases, stabilization, fixture-only TLS/HTTP exceptions, evidence,
cleanup, and the acceptance mapping. Keep component-test and live-run claims
distinct; preserve the preview limitations until the corresponding runs pass.

The change-aware `CI` workflow selects only the smaller
`python3 integration-tests/e2e/live/smoke.py` scenario for affected PRs.
It reuses the same private checkout-built fixture to install, provision, and
run Maven `push manifest`, validate the generated fixture-owned JavaApplication,
then separately apply that YAML with kubectl, wait with the CLI, verify runtime
readiness, and serve HTTP. API, raw CRD, and chart changes select Maven verification and
this smoke. Comprehensive admission/HPA/workflow runs and cluster tiers remain
exclusively nightly/manual E2E, not duplicated by CI's main-push or scheduled
runs. Explicitly dispatch E2E on the candidate branch for high-risk pre-merge
validation. The smoke requires Python 3.12+ and the same
kind/tool prerequisites and preserves the documented cold-start GC deferral.
It does not use the tier reset helper or an existing kube context.

Offline safeguard checks:

```bash
make e2e-contract-check
PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover \
  -s integration-tests/e2e/live -p '*test*.py' -v
```

`make e2e-contract-check` requires Node 24+ and Python 3. It checks workflow
routing and monitor history using offline fixtures, without accessing a cluster.

The E2E workflow's `suite` selector accepts `all` (default), `tiers` (tiers 1-19
except retired tier 14, plus host-only tiers 1-3 on arm64), and `live` (isolated
live scenarios).
Scheduled runs execute both suites. The `scenario` selector affects only live
jobs and is ignored for `tiers`.

The E2E monitor uses `suite: tiers` with an explicit tier list. Saved runs retain
their metadata, logs, and evidence across reloads, with progress, ETA history,
and reruns available.

## Reliable invocation

Run against the monorepo checkout:

```bash
integration-tests/e2e/run.sh --reset
integration-tests/e2e/run.sh
```

Set `E2E_KUBE_CONTEXT` to pin the run to one context: the harness writes a
private, minified kubeconfig into the work directory and exports `KUBECONFIG`,
so a concurrent `kubectl config use-context` cannot redirect a running suite.
Locally built image tags carry the run's PID, so a concurrent run that shares
the Docker daemon (for example against another cluster) cannot remove them.

### Managed clusters (AKS and other real VMs)

By default the node-side tiers reach nodes with `docker exec`/`docker cp`, so
only kind/Docker Desktop nodes are provisionable. To run them on real nodes:

```bash
E2E_KUBE_CONTEXT=my-aks \
E2E_NODE_ACCESS=kubectl \
E2E_POOLS=javaworkers \
integration-tests/e2e/run.sh --reset --tier 1 --tier 2  # ...or list every tier
```

- Live clusters are shared, so with `E2E_NODE_ACCESS=kubectl` the suite is
  always confined to the node pool(s) in `E2E_POOLS` (comma-separated, default
  `javaworkers`). run.sh labels those nodes `e2e.brewlet.sh/node=true` (removed
  on exit and by `--reset`) and nothing lands on other pools:
  - test pods, `kubectl run` clients, the webhook Deployments and the
    Helm-installed operator/admission/uninstall pods get a matching
    `nodeSelector`, and the bare `brewlet` RuntimeClass the tiers create carries
    it in `scheduling.nodeSelector`;
  - catch-all NodeProfiles (tiers 4, 10, 13) are narrowed to the pools, and
    tier 13 emulates its catch-all with a named profile over the pinned nodes;
  - images are side-loaded and node shells created only on Ready pool nodes.
- `E2E_POOL_KEY` is the node label naming the pool. It is detected from
  `kubernetes.azure.com/agentpool` (AKS), `cloud.google.com/gke-nodepool`
  (GKE), `eks.amazonaws.com/nodegroup` (EKS), `karpenter.sh/nodepool` or
  `agentpool`; set it for a custom label. The run aborts if no node is in
  `E2E_POOLS`.
- `E2E_NODE_ACCESS=kubectl` runs node commands through a privileged, hostPID
  node-shell pod per node in `brewlet-e2e-nodeshell` (`chroot /host nsenter
  -t 1`). The namespace is deleted on exit and by `--reset`. Override the image
  with `E2E_NODESHELL_IMAGE`; it needs only `chroot` and `sleep`. Uploads go in
  checksum-verified chunks (`E2E_UPLOAD_CHUNK`, default `2m`) because
  API-server exec streams can time out. Each chunk is retried up to
  `E2E_UPLOAD_RETRIES` (default 5) times and a retry first checks whether the
  chunk already landed, so a reset stream resumes rather than restarts.
- Connections from a workstation to a managed API server are occasionally
  reset. Idempotent kubectl calls used for node-shell setup, uploads, fixture
  teardown and its verification (`get`, `apply`, `wait`, key removals,
  `delete --ignore-not-found`) retry with exponential backoff only on transport
  errors: `connection reset by peer`, `EOF`, `Unable to connect to the server`,
  `TLS handshake timeout` and `socket is not connected`. Tune with
  `E2E_RETRIES` (default 5) and `E2E_RETRY_BACKOFF` (initial seconds, default
  2, capped at 30). Assertions are never retried. If a NodeProfile fixture
  teardown still fails, live state is re-checked and a leak is reported only
  if the profile, its workers, or a node claim actually remain.
- The tiers run the operator on the workstation, and every NodeProfile
  reconcile does several uncached cluster-wide Lists (nodes, profiles,
  DaemonSets, pods). Over a WAN link one reconcile can take minutes, so
  reconcile waits use `E2E_RECONCILE_TIMEOUT` seconds (default 360 with
  `E2E_NODE_ACCESS=kubectl`, otherwise 30).
- `E2E_NODE_SELECTOR` (a label selector, default: the pools) further restricts
  which pool nodes the provisioning tiers may pick.
- Tier 13 relabels nodes with `E2E_T13_POOL_KEY` (default `brewlet-e2e-pool`
  in kubectl mode, `agentpool` otherwise). Use a key without dots or slashes.
- `DOCKER_DEFAULT_PLATFORM` defaults to the selected nodes' architecture, so an
  arm64 workstation builds amd64 images for an amd64 pool.
- Node-side tiers really modify the selected node: they install the shim,
  JDKs and launchers under `/opt/brewlet`, rewrite and restore
  `/etc/containerd/config.toml`, and restart containerd. Use a dedicated pool.
- Tier 5 still skips because the cluster cannot reach a host-bound webhook.

The harness does not switch branches or modify component sources. It uses
`core/` and `kubernetes/` by default. Override `BREWLET_CORE_DIR` or
`BREWLET_KUBERNETES_DIR` only when testing an external checkout.

## Prerequisites

| Tool | Tiers |
|---|---|
| Go | all |
| Python 3 | 2, 4, 12, 13, 16, 17, 19 |
| JDK 21+ | 2, 3, 7, 8, 9, 12, 14-19 |
| Docker | 3, 6, 7, 8-12, 14-19 |
| kubectl and a reachable cluster | 4-19 |
| Helm | 4 (optional), 10, 15, 17 |
| OpenSSL | 4, 5, 6, 10, 11 |

Host-only tiers 1-3 need no cluster. Tiers 4-7 and 13 exercise API-server
behavior. Tiers 6, 8-12, and 15-19 require containerd nodes the harness can
enter: local kind nodes by default, or any Linux node with
`E2E_NODE_ACCESS=kubectl` (see "Managed clusters"). Otherwise managed clusters
skip those node-side paths. Tier 13
also proves the control-plane guard: a NodeProfile that does not set
`nodePool.includeControlPlane` never counts or schedules onto a control-plane
node. Because kind and Docker Desktop label their single node as the control
plane, the node-side tiers set that opt-in explicitly. Tier 18
first proves an induced post-restart handler failure rolls containerd back,
then installs digest-pinned JDK and `jaz` sources and runs a live workload
through both across a JDK patch rotation. Tier 14 is retired (no renumbering). Tier 10 also exercises cert-manager issuance and certificate hot reload
when cert-manager is installed. Tier 15 installs the
chart with metrics enabled, provisions one node through the real DaemonSet,
launches a Brewlet workload, and scrapes all metrics surfaces. Tier 16 covers
the SPECIFICATION §14 failure contract on a live node: a missing/unauthorized
image surfacing as an ImagePull failure, a JVM OOM exiting under
`-XX:+ExitOnOutOfMemoryError` and being restarted by the kubelet, and an
unusable shim surfacing as a containerd task failure — a `FailedCreatePodSandBox`
warning event, since a missing shim fails sandbox creation before any container
exists, or a container create/run error on runtimes that fail later — rather than
a silently non-running pod. It moves the node's shim aside for the last case and restores
it both inline and from its cleanup trap. §14's remaining row, the cgroup-v1
refusal, cannot be produced on a cgroup-v2 CI node and is covered
deterministically by `provisioner/entrypoint_test.sh` over `require_cgroup_v2`.
Tier 17 requires a dedicated fresh node (no shim, safety record, or stage
tree, and no image records that already reference the tier's deterministic
demo image, since those would keep its stage alive). Kubernetes `--reset` alone does not prepare a fresh node, and the tier
rejects retained host state without clearing it. It uses the first fresh
schedulable node. In a multi-tier run (including the default all-tiers run),
`run.sh` moves tier 17 ahead of every Kubernetes tier, right after host-only
tiers 1-3, so earlier tiers cannot dirty the node first. If no node is fresh
even then (a previous run already provisioned it), a multi-tier run reports a
SKIP with instructions; run alone (`--tier 17`), the tier still FAILs. For a
definitive result, run `--tier 17` alone on a new kind cluster, as CI does. It installs the chart with default `stageGC` values and verifies
fresh activation without the metrics exporter. Current-release configuration
changes to `interval=5s` and `minAge=1s` replace the worker and preserve its
installation safety record. A runnable stage survives while its pod runs and
while containerd holds the image, then is reclaimed after content collection;
unmanaged and pending trees survive. Finally, the tier disables GC and waits for
the sweeping worker to terminate before removing only its own safety record.
Re-enabling GC retains runtime readiness but blocks repeated attempts and
preserves an otherwise eligible stage. No acknowledgment or fabricated record
enables cleanup. That fabricated stage sits at the canonical path of the
deterministic demo image that tiers 8 and 9 also deploy, so successful
teardown removes exactly that path and fails if it remains. Fixture teardown
preserves recovery evidence and finalizers if cleanup cannot complete. The E2E
workflow's "Tier 17 then 08" entry runs both tiers on one node to guard
against such host-state leaks.
Tier 18 covers a patched-JDK rollout. It provisions a Temurin 21 digest,
runs a Brewlet workload on it, then replaces the NodeProfile digest with a
patched release. It asserts that the node re-advertises the new version for
the new profile generation, that the DaemonSet and `.brewlet-source` carry the
new digest, and that the previous root is retired rather than deleted. It also
checks that the running pod keeps its original JDK and container, and that a
retired-root sweep keeps the root that pod still uses. Finally, it asserts
that `kubectl rollout restart` moves the same application image onto the
patched JDK, after which an idle sweep reclaims the retired root.
A cold node pulls roughly 1 GB of JDK and `jaz` images here, so each wait for
the node to advertise a JDK is progress-aware rather than fixed. It keeps
waiting while the node's in-flight containerd ingests or the provisioner log
advance. It fails after `E2E_PROVISION_IDLE_TIMEOUT` seconds without progress
(default 240) or after `E2E_PROVISION_TIMEOUT` seconds in total (default
900). The failure detail names which limit stopped the wait.
Tier 19 rehearses a critical CVE in a library shipped by a managed dependency
bundle. It publishes a bundle with a stub `log4j-core` 2.14.1, composes three
thin-JAR apps (two on that bundle, one control on an unrelated bundle), and
deploys them to a prod and a staging namespace. `e2e/cve_sweep.py` then traces
every running Brewlet pod from its digest-pinned image through `brewlet inspect`
evidence to the bundle's CycloneDX SBOM. It flags exactly the affected prod
workloads and fails closed on images it cannot trace. The tier publishes a
patched bundle (2.17.1) and recomposes the *unchanged* thin JARs onto it,
asserting that only the classpath layer changed. It rolls the Deployments by
digest (`maxUnavailable: 0`) while a client polls the Service, and asserts that
no request was dropped and that the post-remediation prod sweep is clean. There
is no in-place JVM swap: a "hot" redeploy is a recompose plus a rolling update.

**Architecture coverage.** Tiers pick up the node's architecture automatically,
but hosted runners are amd64, so `.github/workflows/e2e.yml` runs the host-only
tiers 1-3 a second time on an `ubuntu-24.04-arm` runner. That is the only path
that actually executes the CLI and the shim-to-runc assembly on arm64; the
multi-arch index writer and the strict platform matcher are additionally
unit-tested in `core/internal/artifact`.

**Required PR assertions.** CI runs selected host tiers 2-3 with `E2E_REQUIRE_ALL=true`.
Any skipped assertion, or a selected tier that records no passing assertion,
fails the run. The default remains optional-prerequisite skipping for local
exploration and the existing nightly matrix. Invalid tier numbers always fail.
Tier 2 installs the checkout Maven plugin with `-DskipTests` before exercising
its goals; Maven unit tests belong to the JDK 17 CI job / `make maven-plugin-check`.
Do not count a tier-2-only run as Maven unit-suite coverage.

## Cleanup and diagnostics

Tiers 4 and 13 use invocation-unique, reserved-domain provisioner images that
must never execute. Their fixture teardown stops and waits for the test manager,
checks worker identity and execution history, and waits for foreground worker
deletion before releasing exact fixture-owned claims/finalizers. This is a
test-only abort, not proof of production host cleanup. Dirty or foreign workers
leave ownership intact and fail the tier.

Run the fixture safeguards independently with:

```bash
PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover \
  -s integration-tests/e2e -p 'nodeprofile_fixtures_test.py' -v
```

Run the transient-retry, resumable-upload and tier-order helpers offline (fake
kubectl, no cluster) with:

```bash
PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover \
  -s integration-tests/e2e -p 'lib_retry_test.py' -v
```

`make e2e-contract-check` runs both of these suites as well.

Run tier 19's SBOM sweep logic offline (fake CLI, no cluster) with:

```bash
PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover \
  -s integration-tests/e2e -p 'cve_sweep_test.py' -v
```

Run `./e2e/run.sh --reset` before repeating Kubernetes tiers. It removes only
Brewlet-owned labels, annotations, CRDs, runtime classes, webhook configuration,
RBAC, and fixed test namespaces. It does not delete application workloads outside
the harness namespaces.

Generated artifacts and diagnostic logs are written beneath the printed work
directory. Set `E2E_WORK` to retain them at a known path. For rollout failures,
read `diag-*.log` first.
Hosted tier jobs export only redacted `runner.log`, `diag-*.log`, and
`tN-fixture-teardown.log` files to 14-day artifacts; kubeconfigs, keys, manifests,
and other private work files are not uploaded. The tier job budget is 60 minutes
(30 for arm64 host-only); admission/HPA get 180 minutes for two fresh runs,
and each workflows matrix entry gets 90 minutes.

Common environment-specific skips:

- Tier 5 skips when a cluster cannot reach a host-bound webhook; tier 6 covers
  the same assertions in-cluster.
- Tiers 8, 9, 12, and 14-19 skip if no schedulable containerd node can be
  provisioned (on managed clusters, set `E2E_NODE_ACCESS=kubectl`).
- Tier 12 skips when the node's `ctr` supports neither `images unpack` nor
  import-time unpack; tier 16 applies the same rule.

Specifications belong in `specs/`, and general project documentation belongs in
[`docs/`](../docs/), not this directory.
