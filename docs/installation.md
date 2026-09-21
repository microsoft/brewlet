# Installation

This page enables Brewlet on a Kubernetes cluster: the operator, the node
provisioner, and the admission webhook. After this, any pod with
`runtimeClassName: brewlet` runs a Java application in the runnable OCI image
format directly on a node JDK. The workload image must be digest-pinned
(`repo@sha256:…`); native artifacts remain for local OCI-layout / CLI workflows.

There are two paths:

- **[Helm (recommended)](#helm-recommended)** — explicit pool and JDK configuration
  followed by installation.
- **[Manual](#manual-without-helm)** — apply the raw manifests yourself.

For a first run on your laptop, use [Local Kubernetes](local-kubernetes.md)
to run PetClinic in a disposable kind cluster. It does not install anything
into your existing cluster. The instructions below are for administrators
intentionally configuring an existing cluster.

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
with the fresh Helm install below, not the existing-installation migration
section.

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
store, verify everything for a release in one step:

```bash
git clone https://github.com/microsoft/brewlet.git
cd brewlet
./scripts/verify-release-provenance.sh 0.5.1
```

The script checks that each image, the chart, and every release asset was built
by `microsoft/brewlet`'s release workflow, and that `checksums.txt` matches the
published files. The release workflow runs the same script against the version it
just published, so a release that cannot produce verifiable provenance fails.

To verify a single artifact directly:

```bash
# A component image, straight from the registry.
gh attestation verify oci://ghcr.io/microsoft/brewlet-operator:0.5.1 \
  --repo microsoft/brewlet \
  --signer-workflow microsoft/brewlet/.github/workflows/release.yml

# A downloaded CLI archive.
gh attestation verify brewlet_0.5.1_linux_amd64.tar.gz \
  --repo microsoft/brewlet \
  --signer-workflow microsoft/brewlet/.github/workflows/release.yml
```

`--signer-workflow` is the important part: it requires the attestation to come
from this repository's release workflow, not merely from some workflow in the
repository.

Every release produced by the current release workflow carries build
provenance. Any older artifact that predates it can only be verified with the
published `checksums.txt`.

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

Choose an existing node pool named `java-workers`, or replace it in the command.
Install the latest chart:

```bash
helm upgrade --install brewlet oci://ghcr.io/microsoft/charts/brewlet \
  --namespace brewlet \
  --create-namespace \
  --set provisioner.pools="{java-workers}" \
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
[Upgrading](#upgrading) to update matching CRDs and retain your chosen values.

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

Use `containerdRestart: sighup` only for the legacy in-place SIGHUP path. Use
`containerdRestart: none` when containerd registration is managed in the node
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
  --set provisioner.pools="{java-workers}" \
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
  --set provisioner.pools="{java-workers}" \
  --values my-jdks.yaml > brewlet-rendered.yaml
```

For the source-built path also pass the same component image overrides. The
Makefile's Helm checks use synthetic test-only sources and are not an approved
runtime catalog or a replacement for previewing your chosen values.

### Upgrading

**Skip this section for a fresh installation.** Helm installs the chart's CRDs
when they are not already present; there is no separate CRD upgrade or legacy
migration step before deploying Brewlet for the first time. The following
guidance applies only when updating an existing installation, including a
development cluster that retains an older Brewlet release or CRDs.

Keep the administrator-selected inventory, pools, and component overrides in
your reviewed values files; do not replace them with test fixtures or a newly
copied example. The upgrade examples below use `values.yaml` for your saved
cluster configuration (including `provisioner.pools`) and `my-jdks.yaml` for
your chosen runtime inventory.

Upgrade the operator and provisioner images together. The operator's provisioning
and cleanup readiness probes require the provisioner to publish the
container-local `/tmp/brewlet-complete` marker after successful work. An older or
custom image without that protocol stays NotReady and blocks completion rather
than allowing premature cleanup. Do not bypass a blocked cleanup by removing its
finalizer; restore compatible images and inspect the provisioner logs.

Ownership-aware builds also require the operator/provisioner node-claim protocol
and the new NodeProfile status schema. Apply the matching CRDs **before** rolling
out the new control plane; Helm does not upgrade CRDs in a chart's `crds/`
directory. Node ownership and retirement records must not be silently pruned by
an older schema. Apply the JavaApplication CRD too before using newly supported
fields such as `spec.env[].valueFrom`:

```bash
RELEASE_VERSION=x.y.z
kubectl apply -f \
  "https://raw.githubusercontent.com/microsoft/brewlet/v${RELEASE_VERSION}/kubernetes/deploy/nodeprofile-crd.yaml"
kubectl apply -f \
  "https://raw.githubusercontent.com/microsoft/brewlet/v${RELEASE_VERSION}/kubernetes/deploy/javaapplication-crd.yaml"
```

For source-built components, use the CRDs from the matching source revision
instead. For an ordinary upgrade with compatible profiles, after updating CRDs:

```bash
helm upgrade brewlet oci://ghcr.io/microsoft/charts/brewlet \
  --version "$RELEASE_VERSION" \
  --namespace brewlet \
  --values values.yaml \
  --values my-jdks.yaml \
  --wait
```

For source builds, use the local chart instead and retain your digest-pinned
component overrides. For incompatible legacy profiles, use the maintenance
sequence below instead of an ordinary upgrade.

An older CRD prunes unsupported fields when a resource is saved. Updating the CRD
cannot restore those values; reapply the original JavaApplication manifests
afterward.

Legacy ownership migration may pause provisioning while old workers terminate
and their node ownership is recorded. It temporarily uses `OnDelete` updates and
the `node.brewlet.sh/migration` scheduling gate to prevent replacement workers
from running before their possible targets are recorded. Pending pods' node
affinity and old pods' per-node cleanup policy are part of that inventory; a
missing DaemonSet does not mean its advertised hosts were cleaned. Do not remove
migration gates manually. Investigate any `OwnershipMigration`,
`OwnershipConflict`, or `CleanupBlocked` condition; do not delete ownership
labels or status records to bypass it. Drain workloads before this maintenance
operation and retain the original profile manifests for recovery.
Unverifiable legacy UID, revision, or cleanup-policy evidence remains blocked
for recovery; matching profile names alone never authorize adoption.

Only this legacy migration path requires Pod Scheduling Readiness support
(stable in Kubernetes 1.30): the API server, DaemonSet controller, and scheduler
must preserve and honor scheduling gates.

Before upgrading an existing Brewlet installation to a release that requires explicit
JDK and launcher sources, plan a maintenance window: the `v1alpha1` launcher
wire format changed from strings to structured sources, so legacy profiles
cannot remain present during the control-plane rollout. Delete them while the
old controller can still clean their nodes, then upgrade once with profile
creation disabled:

```bash
RELEASE_VERSION=x.y.z

kubectl delete nodeprofiles.node.brewlet.sh --all
kubectl wait --for=delete nodeprofiles.node.brewlet.sh --all --timeout=10m

kubectl apply -f \
  "https://raw.githubusercontent.com/microsoft/brewlet/v${RELEASE_VERSION}/kubernetes/deploy/nodeprofile-crd.yaml"

cat >brewlet-no-profiles.yaml <<'EOF'
defaultProfile:
  enabled: false
profiles: []
EOF

helm upgrade brewlet oci://ghcr.io/microsoft/charts/brewlet \
  --version "$RELEASE_VERSION" \
  --namespace brewlet \
  -f values.yaml \
  -f my-jdks.yaml \
  -f brewlet-no-profiles.yaml \
  --wait

# Re-enable the migrated, digest-pinned profiles after the new webhook is ready.
helm upgrade brewlet oci://ghcr.io/microsoft/charts/brewlet \
  --version "$RELEASE_VERSION" \
  --namespace brewlet \
  -f values.yaml \
  -f my-jdks.yaml \
  --wait
```

### What the chart deploys vs. what the operator creates

| Deployed by the chart | Created/reconciled by the operator at runtime |
|---|---|
| `brewlet-operator` Deployment + RBAC | `brewlet-node-provisioner` DaemonSet |
| Node-provisioner `ServiceAccount` + `ClusterRole` | The `brewlet` `RuntimeClass` |
| `brewlet-admission` webhook + serving cert | (tracks node readiness, emits events) |

---

## Manual (without Helm)

Manual deployment is an advanced assembly path, not a second one-command
installation. Start from a source checkout matching your component
revision, then prepare reviewed manifests:

- Apply `kubernetes/deploy/nodeprofile-crd.yaml` and
  `kubernetes/deploy/javaapplication-crd.yaml` before creating custom resources.
- Copy the Namespace and provisioner ServiceAccount/RBAC documents from
  `kubernetes/deploy/node-provisioner.yaml` into your own manifest. **Do not
  include its standalone DaemonSet** alongside the operator-managed provisioner.
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
    names: ["java-workers"]
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
brewlet doctor --namespace default

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

For a chart containing the pre-delete cleanup hook:

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

### Older charts and manual installations

Hooks are stored with the installed Helm release. Updating a source checkout
does not add a hook to an existing release; inspect `helm get hooks brewlet -n
brewlet`. A chart with this hook requires an operator image implementing its
cleanup mode; do not combine it with an older image.

For a chart without the hook, delete reviewed profiles and wait for their
finalizers **before** uninstalling the control plane:

```bash
kubectl get nodeprofiles
# Replace the placeholder with reviewed profile names after draining workloads.
kubectl delete nodeprofile <reviewed-profile-names>
kubectl wait --for=delete nodeprofile <reviewed-profile-names> --timeout=10m
# Proceed only after all profiles and provisioning/cleanup workers are gone.
helm uninstall brewlet --namespace brewlet --timeout 5m
```

Apply the same ordering to raw manifests. Nodes provisioned by a standalone
`brewlet.sh/provision=true` DaemonSet without a profile require separate
deprovisioning or replacement. A remaining canonical standalone provisioner
DaemonSet or Pod blocks the hook before profile deletion; the hook never adopts
or removes it. Finish standalone deprovisioning and remove those workers before
uninstalling their shared RBAC.
Worker inventory is cluster-wide and read-only, so moving the operator namespace
does not hide an old installation. Known workers outside the configured operator
namespace block removal until their installation is deprovisioned separately;
the hook never deletes those workers.

## Next steps

- **[Configuration](configuration.md)** — tune every knob.
- **[Capability labels and autoscaling](capability-labels-and-autoscaling.md)** —
  connect node-pool provisioning to workload scheduling.
- **[JDK management](jdk-management.md)** — add/patch JDK roots (copy-from-image),
  go multi-arch.
- **[Launchers](launchers.md)** — install and use `jaz`.
