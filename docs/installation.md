# Installation

This page enables Brewlet on a Kubernetes cluster: the operator, the node
provisioner, and the admission webhook. After this, any pod with
`runtimeClassName: brewlet` runs a Java application in the runnable OCI image
format directly on a node JDK. The workload image must be digest-pinned
(`repo@sha256:…`); native artifacts remain for local OCI-layout / CLI workflows.

There are three paths:

- **[Helm (recommended)](#helm-recommended)** — explicit pool and JDK configuration
  followed by installation.
- **[Brewlet CLI](#brewlet-cli-brewlet-k8s-install)** — `brewlet k8s install`, a
  version-pinned wrapper around the same Helm chart for fresh clusters.
- **[Manual](#manual-without-helm)** — apply the raw manifests yourself.

For a first run on your laptop, use [Local Kubernetes](local-kubernetes.md)
to run PetClinic in a disposable kind cluster. It does not install anything
into your existing cluster. The instructions below are for administrators
intentionally configuring an existing cluster.

For an existing Brewlet installation, release updates default to **safe
teardown/reinstallation**, not an in-place `helm upgrade`. Read
[Upgrading](#upgrading) and the [pre-GA compatibility policy](compatibility.md)
before changing releases. Fresh-install examples using `helm upgrade --install`
are not upgrade support decisions.

> ⚠️ **Node provisioning is privileged and mutates the host** (installs a shim
> and runtime roots, and registers the runtime through containerd configuration).
> Provision only nodes your platform team controls; on mixed clusters scope with
> named pools in `NodeProfile`s (§5.6). There is no all-nodes default. See
> [Security](security.md).

---

## Prerequisites

| Requirement | Why |
|---|---|
| Kubernetes with **containerd 2.0 or newer** as the CRI runtime | The Runtime v2 shim requires protected CRI requested-image metadata that containerd 1.x does not preserve. |
| **cgroup v2** on nodes | Brewlet requires it; the provisioner refuses cgroup v1-only nodes. |
| Nodes you control | Provisioning is privileged and host-mutating. |
| `kubectl` + `helm` (for the Helm path) | To install and manage node provisioning. |
| A reachable **OCI registry** | Where developers push OCI artifacts (and where component + vendor JDK images live). |
| Node access to **JDK images** | Vendor JDK/launcher images (Temurin, MS OpenJDK) pulled copy-from-image via the host containerd; mirror them for air-gapped clusters. |

### Released components

Brewlet's source and CLI downloads are publicly available. Install the released
CLI with the
[checksum-verifying installer](getting-started.md#install-the-released-cli-recommended)
and use the published chart below. Release installation is intended to work
without repository or package credentials. If GHCR denies a chart or component
pull, see [package access troubleshooting](#package-access-troubleshooting).

Use a disposable evaluation cluster; Brewlet is preproduction software. Start
with the fresh Helm install below.

Brewlet publishes version-aligned multi-architecture component images and an OCI
Helm chart. A published chart records the **immutable digest** of each component
image it was built against, so installing a released chart resolves
`ghcr.io/microsoft/brewlet-operator@sha256:…` rather than a tag that could later
be repointed. Charts packaged from a source checkout have no recorded digests and
fall back to the shared `images.tag`.

Every published artifact also carries [SLSA build
provenance](#verify-a-release) signed by the release workflow.
That verifies Brewlet's release components, not arbitrary application images:
general cosign or standard SLSA admission for user workloads is future work.

To build the components from source instead, use the
[Kubernetes component Makefile](https://github.com/microsoft/brewlet/blob/main/kubernetes/Makefile):

```bash
git clone https://github.com/microsoft/brewlet.git
cd brewlet
docker buildx build --platform linux/amd64,linux/arm64 \
  -f kubernetes/Dockerfile -t <registry>/operator:<tag> --push .
docker buildx build --platform linux/amd64,linux/arm64 \
  -f kubernetes/Dockerfile --build-arg CMD=admission \
  -t <registry>/admission:<tag> --push .
make provisioner-image-push \
  PROVISIONER_IMAGE=<registry>/node-provisioner:<tag>
```

These commands build multi-arch (`linux/amd64,linux/arm64`) images via `buildx`
and require a logged-in registry. The provisioner image compiles the shim
**inside** the build for each target arch, so the installed shim always matches
the node.
Use the local chart at `./kubernetes/charts/brewlet` for this path, with explicit
component image overrides shown below. Configure the nodes and component
service accounts for access to your registry before installing.

### Package access troubleshooting

GHCR package visibility is independent of GitHub repository visibility. A
public source repository does not automatically make existing packages public.
An anonymous `helm pull` or component-image pull returning `401`, `403`, or
`denied` can therefore indicate a release-publication problem, not a Kubernetes
configuration error.

For Brewlet's published packages, report the failing reference and version to
the maintainers. A package administrator must ensure all four packages permit
anonymous pulls: `microsoft/charts/brewlet`, `microsoft/brewlet-operator`,
`microsoft/brewlet-admission`, and `microsoft/brewlet-node-provisioner`.
Repository access alone is not a workaround. The source-build path above is
available while package access is corrected.

Authentication is still required for your own private registries and mirrors.
Use their normal credential mechanisms; do not put tokens in URLs or shell
history.

---

## Verify a release

The release workflow publishes SLSA build provenance for each component image,
the OCI Helm chart, and every GitHub Release asset. Provenance for images and the
chart is pushed to GHCR as an OCI referrer, so it can be verified straight from
the registry without trusting the release page.

With the GitHub CLI installed and authenticated through its normal credential
store, resolve the latest release once and verify all of its artifacts:

```bash
git clone https://github.com/microsoft/brewlet.git
cd brewlet
release_tag="$(gh release view --repo microsoft/brewlet --json tagName --jq .tagName)" &&
BREWLET_VERSION="${release_tag#v}" &&
./scripts/verify-release-provenance.sh "$BREWLET_VERSION"
```

To verify an installed or pinned release instead, set `BREWLET_VERSION` to its
concrete version and pass that to the script without repeating the latest-release
lookup. The script requires a release number, not the literal `latest`.

The script checks that each image, the chart, and every release asset was built
by `microsoft/brewlet`'s release workflow, and that `checksums.txt` matches the
published files. The release workflow runs the same script against the version it
just published, so a release that cannot produce verifiable provenance fails.

To verify a single artifact directly, reuse the same `BREWLET_VERSION`:

```bash
# A component image, straight from the registry.
gh attestation verify "oci://ghcr.io/microsoft/brewlet-operator:${BREWLET_VERSION}" \
  --repo microsoft/brewlet \
  --signer-workflow microsoft/brewlet/.github/workflows/release.yml

# A downloaded CLI archive.
gh attestation verify "brewlet_${BREWLET_VERSION}_linux_amd64.tar.gz" \
  --repo microsoft/brewlet \
  --signer-workflow microsoft/brewlet/.github/workflows/release.yml
```

`--signer-workflow` is the important part: it requires the attestation to come
from this repository's release workflow, not merely from some workflow in the
repository.

Every release produced by the current release workflow carries build provenance.

---

## Helm (recommended)

The [`charts/brewlet`](https://github.com/microsoft/brewlet/tree/main/kubernetes/charts/brewlet) chart installs the operator, the
provisioner RBAC, and the admission webhook. The operator then creates and
reconciles the provisioner DaemonSet and the `brewlet` RuntimeClass from the chart's
values — so there is a single runtime source of truth for the JDK/launcher inventory.

The install must name two things, and the chart fails to render without either.

**The node pools Brewlet may provision** (`provisioner.pools`). Provisioning is
privileged and mutates the host, so there is no every-node default.

**The JDK sources to install** (`provisioner.jdks`). Brewlet ships no built-in
runtime catalog: the platform team chooses every digest-pinned build it runs and
owns its CVE posture ([§5.2/§5.3](https://github.com/microsoft/brewlet/blob/main/specs/SPECIFICATION.md)).
A chart-shipped default digest would be a JDK nobody chose that could never be
patched, so the chart ships none.

Before running Helm, save the following as `my-jdks.yaml`. Replace the
placeholder with the full lowercase SHA-256 digest of an administrator-approved
image; confirm the Java feature, architectures, and JDK root in that image.
This template is not a runtime recommendation or a built-in catalog:

```yaml
provisioner:
  jdks:
    - distribution: temurin
      feature: 21
      source:
        image: docker.io/library/eclipse-temurin@sha256:<64-lowercase-hex>
        javaHome: /opt/java/openjdk
```

Choose an existing node pool named `javaworkers`, or replace it in the command.
Use the pool's real name as your provider reports it. AKS pool names are
lowercase alphanumeric only (no dashes; at most 12 characters for Linux and 6
for Windows pools), so names such as `java-workers` are not valid there.
Install the latest chart:

```bash
helm upgrade --install brewlet oci://ghcr.io/microsoft/charts/brewlet \
  --namespace brewlet \
  --create-namespace \
  --set provisioner.pools="{javaworkers}" \
  --values my-jdks.yaml

# The chart renders a default NodeProfile scoped to those pools (§5.6) — there
# is no per-node opt-in step. The operator provisions each node in them and the
# provisioner marks it ready once the shim, runtime inventory, containerd
# handler, and configured readiness probes are healthy. Watch:
kubectl get nodes -L brewlet.sh/runtime -w
```

Omitting `--version` selects the latest released chart; do not pass
`--version latest`. The chart still pins its component images to immutable
digests. To reproduce a specific release, add `--version x.y.z`, replacing
`x.y.z` with that release number. For an existing installation, follow
[Upgrading](#upgrading) for safe teardown and reinstallation.

For a fresh cluster, you can instead install the same chart with
[`brewlet k8s install`](#brewlet-cli-brewlet-k8s-install).

`provisioner.poolKey` pins the node label the pool names are matched on. Leave
it unset on AKS, EKS, and GKE, where the well-known provider label is
auto-detected; set it explicitly on bare metal or kubeadm.

Every entry in `my-jdks.yaml` needs a fully qualified, tagless, SHA-256
digest-pinned image and the JDK root inside it. If required, add a separately
approved launcher to the same `provisioner` mapping (vanilla `java` is implicit):

```yaml
provisioner:
  # Keep the jdks list from my-jdks.yaml above.
  launchers:
    - name: jaz
      source:
        image: mcr.microsoft.com/openjdk/jdk@sha256:<64-lowercase-hex>
        path: /usr/bin/jaz
```

Pick the digests you intend to patch on your own schedule; see
[JDK management](jdk-management.md#helm-examples-temurin-and-microsoft) and
[Launchers](launchers.md#helm-example-jaz).

> Control-plane nodes are excluded **by default** by node affinity regardless of taints, and
> the provisioner tolerates only what a profile declares. On a single-node kind
> or Docker Desktop cluster — whose only node is labelled as the control plane —
> add `--set provisioner.includeControlPlane=true`, or nothing will be
> provisioned. See [Where the provisioner may run](configuration.md#where-the-provisioner-may-run).

> To manage profiles yourself instead of through the chart, disable the
> chart's default profile (`--set defaultProfile.enabled=false`) and define named
> `NodeProfile`s scoped to your pools — see [Configuration](configuration.md#helm-chart-values)
> (`profiles` / `defaultProfile`) and [SPECIFICATION §5.6](https://github.com/microsoft/brewlet/blob/main/specs/SPECIFICATION.md).
> For the labels those profiles publish and their autoscaler implications, see
> [Capability labels and autoscaling](capability-labels-and-autoscaling.md).

### Safe activation and readiness

The default rollout is fail-safe:

- `provisioner.rollout.validate=true` executes every installed JDK's
  `java -version` and checks that each staged launcher is executable before
  readiness or capability labels are published. Arbitrary launchers are not
  executed as probes.
- `provisioner.rollout.containerdRestart=validated` renders an imported
  `/etc/containerd/config.toml.d/99-brewlet.toml` drop-in when supported, or a
  backed-up in-place configuration otherwise. It validates the effective config,
  restarts the host containerd service only when required, and verifies both
  containerd and the live `brewlet` runtime handler.
- If validation, restart, or a health check fails, Brewlet removes/restores its
  change, recovers containerd, leaves the node unready, and records a stable
  reason code in `brewlet.sh/provision-error` (with human detail in
  `brewlet.sh/provision-error-message`). The codes are enumerated in
  [SPECIFICATION §14](https://github.com/microsoft/brewlet/blob/main/specs/SPECIFICATION.md).

Use `containerdRestart: none` when containerd registration is managed in the node
image or by another system; the JDK smoke tests and launcher executable checks
still run.
See [Configuration](configuration.md#helm-chart-values) for the values.

For source-built components, use the local chart and pin the images you pushed.
Replace each placeholder with its actual repository and full manifest digest;
do not use a mutable tag for these overrides:

```bash
helm upgrade --install brewlet ./kubernetes/charts/brewlet \
  --namespace brewlet \
  --create-namespace \
  --set provisioner.pools="{javaworkers}" \
  --values my-jdks.yaml \
  --set "images.operator=<registry>/operator@sha256:<64-lowercase-hex>" \
  --set "images.provisioner=<registry>/node-provisioner@sha256:<64-lowercase-hex>" \
  --set "images.admission=<registry>/admission@sha256:<64-lowercase-hex>"
```

Every value is documented in [Configuration](configuration.md#helm-chart-values).
Preview with the **same** inventory and pool before installing:

```bash
helm template brewlet ./kubernetes/charts/brewlet \
  --namespace brewlet \
  --set provisioner.pools="{javaworkers}" \
  --values my-jdks.yaml > brewlet-rendered.yaml
```

For the source-built path also pass the same component image overrides. The
Makefile's Helm checks use synthetic test-only sources and are not an approved
runtime catalog or a replacement for previewing your chosen values.

### Upgrading

Helm installs the chart's CRDs on a fresh environment. For release changes,
use safe teardown and reinstallation; there are no supported in-place release
pairs. See the [pre-GA compatibility policy](compatibility.md).

#### Default: safe teardown and reinstallation

1. Save your reviewed values, profile and workload manifests, and recovery
   evidence. Pause NodeProfile/GitOps writers and drain or move Brewlet workloads.
2. Follow [Uninstall](#uninstall) using the installed release's cleanup path.
   Keep its operator, provisioner RBAC, and API access available until host
   cleanup and worker teardown complete. Use the explicit profile-cleanup
   sequence if the installation has no uninstall hook.
3. Stop if cleanup is blocked. Recover with the installed release's
   components; do not bypass finalizers, scheduling gates, ownership records, or
   live-reference checks. See [Blocked cleanup recovery](#blocked-cleanup-recovery)
   for hosts that cannot be safely cleaned.
4. Review retained CRDs, custom resources, shared RuntimeClass, namespaces, and
   host state with their owners. Helm uninstall does not make the environment
   fresh. Do not delete CRDs with surviving resources or cleanup evidence, or
   remove shared resources blindly. Only after cleanup completes and owners
   confirm no surviving custom resources or required evidence depend on them,
   remove the reviewed Brewlet CRDs so the fresh install creates the target
   schema. The CLI refuses existing Brewlet CRDs. Use a fresh evaluation
   environment if the old one cannot safely be prepared, while preserving the
   old environment's recovery and cleanup obligations.
   Include [retained AppCDS cache files](#retained-appcds-cache-files) in the
   host-state review; uninstall does not establish that retained files are unused.
5. On the prepared environment, follow the fresh-install instructions with the
   target release's matching chart, components, and CRDs. Recreate reviewed
   manifests in the target format and rebuild/republish artifacts where required.
   [Verify readiness](#verify-the-installation) before restoring workloads and
   automation.

JDK rotation and configuration maintenance within the installed release are
separate operations. Pin that chart version and preserve component overrides
when using `helm upgrade` for those operations.

#### Blocked cleanup recovery

`Ready=False/CleanupBlocked` and `OwnershipConflict` refuse competing or
unfenced workers, host advertisements without UID-bound ownership, and
unavailable or replaced targets. They do not authorize adoption, scheduling
changes, or host cleanup; investigate the condition message rather than forcing
deletion.

Preserve the installed release's manifests, worker/node identities, and cleanup
records. Pause automation and drain workloads, then use the installed
components, RBAC, and API access to finish host cleanup and worker teardown. Do
not fabricate claims, clear status, or bypass finalizers, scheduling gates, and
live-reference checks. Verify containerd health and worker/shim termination on
every affected node; a deleted DaemonSet or readiness label is not proof of
cleanup.

The operator and uninstall hook refuse competing workers across namespaces
without adopting or deleting them. Never attach a NodeProfile to an unfenced or
unverifiable host. If cleanup cannot be completed safely, use your platform's
reviewed node decommissioning/replacement process or a separate fresh
environment, preserving recovery evidence and obligations. Review retained
resources and files before following the
[fresh-install procedure](#default-safe-teardown-and-reinstallation).

#### Activating runnable-stage GC

Fresh nodes enable periodic cleanup automatically after successful provisioning.
Fresh means no installed shim copies or safety record and an absent or empty
stage root. Existing installations require a matching
`/opt/brewlet/.stage-gc-compatible` record bound to both installed shim copies and
the stage path. This is local safety evidence, not an in-place upgrade promise.
It is preserved across ordinary current-release maintenance, including disabling
and re-enabling GC. A missing or mismatched record keeps GC blocked without
preventing runtime readiness.

There is no acknowledgment override for blocked nodes. Follow the
[default teardown/reinstallation procedure](#default-safe-teardown-and-reinstallation)
using the installed release's cleanup path. Retire unguarded shims, finish their
launches, and retire or regenerate exported bundles that reference stage files.
Replacing the shim binary alone does not stop existing shim processes.
Review retained stage roots and their consumers before reinstalling: uninstall
does not remove these trees or establish that they are unused. Do not forge
records, rename retained roots, bypass finalizers, or delete in-use files.
Use a safely prepared or replacement node when safety cannot be established.

Keep `stageGC.enabled=true` for activation on verified installations. For
current-release configuration maintenance, pin the installed chart version and
retain its component choices. Check the provisioner logs for each managed node,
not just pod readiness:

```bash
kubectl get pods -n brewlet -l app=brewlet-node-provisioner -o wide
kubectl logs -n brewlet <provisioner-pod> -c provisioner --tail=100
```

Look for `stage GC: successful_sweeps=...` with a recent `last_success` timestamp;
zero removed stages is a valid successful sweep. `last_success=never` with
failed attempts means cleanup has not succeeded. Investigate inspection,
namespace, socket-access, and lock errors; do not bypass them by deleting stage
directories. Neither GC failure counters nor installation safety blocking are part of
the pod readiness gate.

Disable automatic GC with `stageGC.enabled=false` before introducing unguarded
consumers; this does not make rollback or mixed-version operation supported.
Do not add a separate timer or
invoke destructive manual cleanup to bypass blocked activation: manual commands
do not enforce the provisioner's installation safety gate. See
[Runnable stage cleanup](runnable-image.md#reclaiming-unused-stages) for the
reclamation rules and [Configuration](configuration.md#runnable-image-stage-cleanup)
for all settings.

### What the chart deploys vs. what the operator creates

| Deployed by the chart | Created/reconciled by the operator at runtime |
|---|---|
| `brewlet-operator` Deployment + RBAC | Per-profile `brewlet-node-provisioner-<profile>` and cleanup DaemonSets |
| Node-provisioner `ServiceAccount` + `ClusterRole` | The `brewlet` `RuntimeClass` |
| `brewlet-admission` webhook + serving cert | (tracks node readiness, emits events) |

---

## Brewlet CLI (`brewlet k8s install`)

The [released `brewlet` CLI](getting-started.md#install-the-released-cli-recommended)
can install Brewlet on a **fresh** cluster. It is a thin, version-pinned wrapper
around the [Helm chart](#helm-recommended): it runs `helm install` against
`oci://ghcr.io/microsoft/charts/brewlet` with your kubeconfig, so it deploys
exactly the same components and requires the same
[prerequisites](#prerequisites), including `kubectl` and `helm` on your `PATH`.

Compared with calling Helm directly, the CLI:

- requires an **exact chart version** (`--version x.y.z`; never `latest` or a range);
- requires at least one **values file** (`--values`/`-f`) — pools and runtime
  sources are never chosen for you;
- **refuses to run if Brewlet CRDs already exist**, rather than silently skipping
  their migration;
- creates the namespace (default `brewlet`) and sets the chart's `namespace`
  value to match, so the release and its resources cannot land in different
  namespaces;
- waits for the chart's rollout (`--wait-timeout`, default `5m`), reporting
  each step and live Deployment readiness and pod problems (for example
  `ImagePullBackOff`) on stderr while Helm waits.

It does not create a cluster or node pools, upgrade an existing release, or
clean up after a failed installation.

### Prepare values

The CLI takes complete Helm values files, so put your node pools in a file
instead of `--set`. Save the following as `my-pools.yaml`, replacing
`javaworkers` with an existing node pool you control:

```yaml
provisioner:
  pools:
    - javaworkers
```

Combine it with the `my-jdks.yaml` inventory from the [Helm](#helm-recommended)
section, or put both lists in a single file. Any other chart values — launchers,
`provisioner.poolKey`, `provisioner.includeControlPlane`, mirrors, or your own
profiles with `defaultProfile.enabled=false` — go in the same files. See
[Configuration](configuration.md#helm-chart-values) for every setting.

### Preview and install

Set `RELEASE_VERSION` to the exact chart release you want (for example, the
version of your `brewlet` CLI from `brewlet version`) and `evaluation` to the
kubeconfig context of the target cluster:

```bash
# Exact approved chart release, not "latest".
RELEASE_VERSION=x.y.z

# Render the chart locally (helm template --include-crds); does not contact the cluster.
brewlet k8s install --version "$RELEASE_VERSION" \
  --values my-pools.yaml --values my-jdks.yaml --dry-run

# Install into a deliberately selected fresh cluster.
brewlet k8s install --context evaluation --version "$RELEASE_VERSION" \
  --values my-pools.yaml --values my-jdks.yaml --namespace brewlet

# Helm readiness is not node provisioning; check rollout and node inventory.
brewlet k8s status --context evaluation --namespace brewlet
brewlet k8s jdk list --context evaluation --output wide
```

| Flag | Default | Meaning |
|---|---|---|
| `--version X.Y.Z` | *(required)* | Exact chart release to install. |
| `--values FILE`, `-f FILE` | *(required)* | Helm values file; repeatable, applied in Helm precedence order. |
| `--namespace NAME` | `brewlet` | Release and component namespace (created if missing). |
| `--release NAME` | `brewlet` | Helm release name. |
| `--wait-timeout DURATION` | `5m` | Helm rollout deadline. Node provisioning continues afterwards. |
| `--dry-run` | `false` | Render manifests locally instead of installing. |
| `--kubeconfig FILE`, `--context NAME` | kubectl/Helm defaults | Target cluster, without changing your current context. |

The dry run validates chart rendering only, not image contents or node
readiness, and still needs access to the chart registry. Installation is
privileged and mutates the selected nodes. To upgrade later, or if Brewlet CRDs
remain from a previous installation, follow the retained-state and safe
teardown/reinstallation guidance in [Upgrading](#upgrading); do not bypass the
CLI check by blindly applying a chart over old resources.
See the [CLI reference](cli-reference.md#installation) for full details.

---

## Manual (without Helm)

Manual deployment is an advanced assembly path, not a second one-command
installation. Start from a source checkout matching your component
revision, then prepare reviewed manifests:

- Apply `kubernetes/deploy/nodeprofile-crd.yaml` and
  `kubernetes/deploy/javaapplication-crd.yaml` before creating custom resources.
- Apply `kubernetes/deploy/provisioner-rbac.yaml` for the Namespace and
  provisioner ServiceAccount/RBAC. It contains no worker; provisioning and
  cleanup DaemonSets are created only by the operator for NodeProfiles.
- Configure `kubernetes/config/operator.yaml` with your approved operator image
  digest and provisioner image digest. Configure admission, TLS, and its
  permissions as described in [Configuration](configuration.md#admission-webhook).
- Create your own `my-nodeprofile.yaml` below, choosing the pool and replacing
  the JDK digest placeholder. Do not apply the sample profile collection as an
  approved runtime catalog.

```yaml
apiVersion: node.brewlet.sh/v1alpha1
kind: NodeProfile
metadata:
  name: java-workers
spec:
  nodePool:
    names: ["javaworkers"]
  jdks:
    - distribution: temurin
      feature: 21
      source:
        image: docker.io/library/eclipse-temurin@sha256:<64-lowercase-hex>
        javaHome: /opt/java/openjdk
  rollout:
    validate: true
    containerdRestart: validated
```

Use the same administrator-approved source as `my-jdks.yaml`. Set
`spec.nodePool.key` on bare metal or kubeadm; control-plane provisioning requires
an explicit `spec.nodePool.includeControlPlane: true` opt-in. Apply this profile
only after your reviewed namespace/RBAC, operator, and admission manifests are
installed and healthy:

```bash
kubectl apply -f my-nodeprofile.yaml
```

Runtime inventory belongs in `NodeProfile`, not operator flags. The operator
creates the RuntimeClass and profile-scoped DaemonSets; never opt every node in
with a blanket label command. All operator and admission flags are in
[Configuration](configuration.md#operator-flags).

> The operator itself does **not** need to be privileged — it only talks to the API
> server. The privileged, host-mutating work is done by the DaemonSet it manages.

---

## Verify the installation

```bash
# Use the CLI from the same release or source revision as the cluster components.
brewlet k8s doctor --namespace default

# 1. Components are running:
kubectl get pods -n brewlet

# 2. Nodes are being provisioned → ready:
kubectl get nodes -L brewlet.sh/runtime
#   NAME     STATUS   RUNTIME
#   node-1   Ready    ready        ← provisioned

# 3. Inspect what a node advertises:
kubectl get node node-1 -o jsonpath='{.metadata.annotations.brewlet\.sh/jdks}{"\n"}'
#   temurin-21             ← must match your chosen inventory
kubectl get node node-1 -o jsonpath='{.metadata.annotations.brewlet\.sh/launchers}{"\n"}'
#   java                   ← additional launchers only if configured

# 4. The RuntimeClass exists:
kubectl get runtimeclass brewlet

# 5. The operator's view of each node:
kubectl get node node-1 -o jsonpath='{.metadata.annotations.brewlet\.sh/provision-state}{"\n"}'
#   Ready
```

The failure reason annotation is empty after successful provisioning:

```bash
kubectl get node node-1 -o jsonpath='{.metadata.annotations.brewlet\.sh/provision-error}{"\n"}'
```

Watch provisioning events if a node isn't going ready:

```bash
kubectl get events --field-selector reason=NodeReady
kubectl get events --field-selector reason=ProvisionFailed
```

See [Troubleshooting](troubleshooting.md) if a node stays `Provisioning`/`Failed`.

---

## Smoke test with a workload

```bash
kubectl apply -f kubernetes/deploy/raw-deployment.yaml
kubectl get pods -l app=hello -w
kubectl logs -l app=hello
```

For the full deploy story (raw Deployment, `JavaApplication` CRD, requesting a
specific JDK/launcher), see [Deploying workloads](deploying-workloads.md).

---

## Uninstall

Drain or move Brewlet workloads and pause NodeProfile/GitOps writers first.
Cleanup does not migrate application Pods, and the uninstall coordinator cannot
atomically lock out new cluster-wide profile creation. Keep the operator,
provisioner RBAC, and API access available throughout cleanup.

For a Helm installation:

```bash
helm uninstall brewlet --namespace brewlet --timeout 5m
```

The hook uses the **same operator image**, running a bounded cleanup coordinator
with a dedicated unprivileged service account. It identifies profiles by both
Helm release ownership annotations and the `app.kubernetes.io/managed-by=Helm`
label. It requests deletion with UID/resource-version preconditions and waits
for profiles and their provisioning/cleanup workers to disappear. It never
removes finalizers or kills privileged workers to force completion. The operator
performs the normal stop, host-cleanup, and worker-teardown sequence before Helm
may remove the control plane.

Any manually managed or other-release NodeProfile blocks uninstall: this
operator is cluster-wide, so removing it would strand those profiles. Have their
owners deprovision them safely first, or retain the operator. A cleanup failure,
unavailable node, API error, or timeout also fails the hook and leaves the
operator/RBAC available. Repair the cause, let cleanup finish, and retry.

```bash
kubectl get nodeprofiles
kubectl get daemonsets,pods -n brewlet
kubectl get jobs -n brewlet -l app=brewlet-uninstall
kubectl logs -n brewlet -l app=brewlet-uninstall --all-containers=true
```

The default coordinator timeout is 240 seconds; the Job allows another 20
seconds and a 10-second termination grace period. For longer operations, set
`uninstall.timeoutSeconds` **before** uninstalling and use a Helm `--timeout`
greater than that value plus 30 seconds. Configure `uninstall.imagePullSecrets`
when the operator image requires registry credentials. Failed cleanup Jobs
remain for diagnosis until the next attempt. Helm may remove earlier successful
hook resources even when the Job fails; retry recreates the dedicated hook RBAC.
The normal operator/RBAC remain available. Do not use `--no-hooks`, delete the
namespace, or remove finalizers as a timeout workaround.

The component namespace is retained to avoid cascading deletion of unrelated
objects. Helm also retains CRDs; the operator-created shared RuntimeClass is not
a chart-owned resource. Review these leftovers before removing them manually.

### Retained AppCDS cache files

AppCDS maintenance manages only [64-hex private cache entries and writer
markers](appcds.md#43-node-side-regeneration-the-durable-answer-for-a-patched-fleet);
unrecognized paths are untouched.
Helm uninstall and provisioner teardown do not remove the AppCDS cache.

For manual reclamation, drain workloads, stop local cache consumers and writers,
pause provisioning automation, and finish host cleanup and worker teardown.
Review `/opt/brewlet/cds` (or the configured `BREWLET_CDS_CACHE`) with its owner.
Remove only individually verified, unused paths belonging to the retired
installation. Do not follow symlinks, purge the root, use wildcard deletion,
delete live files, or discard recovery evidence. Unverifiable ownership or use
requires investigation, not deletion.

### Manual control-plane removal

For raw manifests or an installed chart without a cleanup hook, delete reviewed
profiles and wait for their finalizers **before** removing the control plane.
For Helm, inspect the installed hooks with `helm get hooks brewlet -n brewlet`:

```bash
kubectl get nodeprofiles
# Replace the placeholder with reviewed profile names after draining workloads.
kubectl delete nodeprofile <reviewed-profile-names>
kubectl wait --for=delete nodeprofile <reviewed-profile-names> --timeout=10m
# Proceed only after all profiles and provisioning/cleanup workers are gone.
helm uninstall brewlet --namespace brewlet --timeout 5m
```

For raw manifests, remove the reviewed control-plane manifests only after the
same cleanup checks. Worker inventory is cluster-wide and read-only; competing
or foreign-namespace workers block removal and require
[blocked cleanup recovery](#blocked-cleanup-recovery) before removing
shared RBAC.

## Next steps

- **[Configuration](configuration.md)** — tune every knob.
- **[Capability labels and autoscaling](capability-labels-and-autoscaling.md)** —
  connect node-pool provisioning to workload scheduling.
- **[JDK management](jdk-management.md)** — add/patch JDK roots (copy-from-image),
  go multi-arch.
- **[Launchers](launchers.md)** — install and use `jaz`.
