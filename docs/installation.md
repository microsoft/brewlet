# Installation

This page enables Brewlet on a Kubernetes cluster: the operator, the node
provisioner, and the admission webhook. After this, any pod with
`runtimeClassName: brewlet` runs a Java application in the runnable OCI image
format directly on a node JDK. The workload image must be digest-pinned
(`repo@sha256:…`); native artifacts remain for local OCI-layout / CLI workflows.

There are two paths:

- **[Helm (recommended)](#helm-recommended)** — the SpinKube-style single-command
  activation.
- **[Manual](#manual-without-helm)** — apply the raw manifests yourself.

> ⚠️ **Node provisioning is privileged and mutates the host** (installs a shim
> and runtime roots, and registers the runtime through containerd configuration).
> Provision only nodes your platform team controls; on mixed clusters scope with
> named `NodeProfile`s (§5.6) rather than the all-nodes default profile. See
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

Brewlet publishes version-aligned multi-architecture component images and an OCI
Helm chart. A published chart records the **immutable digest** of each component
image it was built against, so installing chart `0.4.0` resolves
`ghcr.io/microsoft/brewlet-operator@sha256:…` rather than a tag that could later
be repointed. Charts packaged from a source checkout have no recorded digests and
fall back to the shared `images.tag`.

Every published artifact also carries [SLSA build
provenance](#verify-a-release) signed by the release workflow.

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

---

## Verify a release

The release workflow publishes SLSA build provenance for each component image,
the OCI Helm chart, and every GitHub Release asset. Provenance for images and the
chart is pushed to GHCR as an OCI referrer, so it can be verified straight from
the registry without trusting the release page.

Verify everything for a release in one step:

```bash
git clone https://github.com/microsoft/brewlet.git
cd brewlet
./scripts/verify-release-provenance.sh 0.4.0
```

The script checks that each image, the chart, and every release asset was built
by `microsoft/brewlet`'s release workflow, and that `checksums.txt` matches the
published files. The release workflow runs the same script against the version it
just published, so a release that cannot produce verifiable provenance fails.

To verify a single artifact directly:

```bash
# A component image, straight from the registry.
gh attestation verify oci://ghcr.io/microsoft/brewlet-operator:0.4.0 \
  --repo microsoft/brewlet \
  --signer-workflow microsoft/brewlet/.github/workflows/release.yml

# A downloaded CLI archive.
gh attestation verify brewlet_0.4.0_linux_amd64.tar.gz \
  --repo microsoft/brewlet \
  --signer-workflow microsoft/brewlet/.github/workflows/release.yml
```

`--signer-workflow` is the important part: it requires the attestation to come
from this repository's release workflow, not merely from some workflow in the
repository.

Releases before `0.4.0` predate build provenance and can only be verified with
the published `checksums.txt`.

---

## Helm (recommended)

The [`charts/brewlet`](https://github.com/microsoft/brewlet/tree/main/kubernetes/charts/brewlet) chart installs the operator, the
provisioner RBAC, and the admission webhook. The operator then creates and
reconciles the provisioner DaemonSet and the `brewlet` RuntimeClass from the chart's
values — so there is a single runtime source of truth for the JDK/launcher inventory.

The install must name the node pools Brewlet may provision. Provisioning is
privileged and mutates the host, so the chart has no every-node default: it
fails to render until `provisioner.pools` is set.

```bash
helm upgrade --install brewlet oci://ghcr.io/microsoft/charts/brewlet \
  --version 0.3.1 \
  --namespace brewlet \
  --create-namespace \
  --set provisioner.pools="{java-workers}"

# The chart renders a default NodeProfile scoped to those pools (§5.6) — there
# is no per-node opt-in step. The operator provisions each node in them and the
# provisioner marks it ready once the shim, runtime inventory, containerd
# handler, and configured readiness probes are healthy. Watch:
kubectl get nodes -L brewlet.sh/runtime -w
```

`provisioner.poolKey` pins the node label the pool names are matched on. Leave
it unset on AKS, EKS, and GKE, where the well-known provider label is
auto-detected; set it explicitly on bare metal or kubeadm.

The chart's editable default `NodeProfile` uses explicit, digest-pinned Temurin
21, Microsoft JDK 25, and `jaz` sources. Review or replace them in a values file;
see [JDK management](jdk-management.md#helm-examples-temurin-and-microsoft) and
[Launchers](launchers.md#helm-example-jaz).

> Control-plane nodes are excluded by node affinity regardless of taints, and
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
  change, recovers containerd, leaves the node unready, and records the reason in
  `brewlet.sh/provision-error`.

Use `containerdRestart: sighup` only for the legacy in-place SIGHUP path. Use
`containerdRestart: none` when containerd registration is managed in the node
image or by another system; the JDK smoke tests and launcher executable checks
still run.
See [Configuration](configuration.md#helm-chart-values) for the values.

Point the chart at your own registry or image digests if required:

```bash
helm upgrade --install brewlet oci://ghcr.io/microsoft/charts/brewlet \
  --version 0.3.1 \
  --namespace brewlet \
  --create-namespace \
  --set provisioner.pools="{java-workers}" \
  --set images.operator=<registry>/operator:<tag> \
  --set images.provisioner=<registry>/node-provisioner:<tag> \
  --set images.admission=<registry>/admission:<tag>
```

Every value is documented in [Configuration](configuration.md#helm-chart-values).
Lint / preview the rendered manifests before installing:

```bash
make -C kubernetes helm-lint
make -C kubernetes helm-template
```

### Upgrading

Helm does not upgrade CRDs placed under a chart's `crds/` directory. Before
upgrading an existing Brewlet installation to a release that requires explicit
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

cat >/tmp/brewlet-no-profiles.yaml <<'EOF'
defaultProfile:
  enabled: false
profiles: []
EOF

helm upgrade brewlet oci://ghcr.io/microsoft/charts/brewlet \
  --version "$RELEASE_VERSION" \
  -f values.yaml \
  -f /tmp/brewlet-no-profiles.yaml \
  --wait

# Re-enable the migrated, digest-pinned profiles after the new webhook is ready.
helm upgrade brewlet oci://ghcr.io/microsoft/charts/brewlet \
  --version "$RELEASE_VERSION" \
  -f values.yaml \
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

If you'd rather not use Helm, apply the raw manifests and run the operator directly.

```bash
# 1. Namespace + provisioner ServiceAccount/RBAC (and, if you want to hand-wire it,
#    the provisioner DaemonSet):
kubectl apply -f kubernetes/deploy/node-provisioner.yaml

# 2. The operator ServiceAccount + RBAC + Deployment:
kubectl apply -f kubernetes/config/operator.yaml

# 3. Opt nodes in. The standalone provisioner DaemonSet schedules onto nodes
#    carrying this LABEL — it drives nodeAffinity, so it must be a label, not an
#    annotation:
kubectl label node --all brewlet.sh/provision=true
```

You can also run the operator locally against your current kubeconfig (useful
for debugging). Runtime inventory belongs in `NodeProfile`, not operator flags:

```bash
make -C kubernetes operator-build
./kubernetes/bin/operator \
  --namespace=brewlet \
  --provisioner-image=<registry>/node-provisioner:<tag>

kubectl apply -f kubernetes/deploy/sample-nodeprofile.yaml
```

The RuntimeClass and provisioner DaemonSet the operator generates mirror
[`deploy/runtimeclass.yaml`](https://github.com/microsoft/brewlet/blob/main/kubernetes/deploy/runtimeclass.yaml) and
[`deploy/node-provisioner.yaml`](https://github.com/microsoft/brewlet/blob/main/kubernetes/deploy/node-provisioner.yaml). All operator
and admission flags are in [Configuration](configuration.md#operator-flags).

> The operator itself does **not** need to be privileged — it only talks to the API
> server. The privileged, host-mutating work is done by the DaemonSet it manages.

---

## Verify the installation

```bash
# Install the CLI version that matches the chart, then run the readiness check:
export BREWLET_VERSION="0.3.1"
curl -fsSL https://brewlet.sh/install.sh | sh
export PATH="$HOME/.local/bin:$PATH"
brewlet doctor --namespace default

# 1. Components are running:
kubectl get pods -n brewlet

# 2. Nodes are being provisioned → ready:
kubectl get nodes -L brewlet.sh/runtime
#   NAME     STATUS   RUNTIME
#   node-1   Ready    ready        ← provisioned

# 3. Inspect what a node advertises:
kubectl get node node-1 -o jsonpath='{.metadata.annotations.brewlet\.sh/jdks}{"\n"}'
#   temurin-21,microsoft-25
kubectl get node node-1 -o jsonpath='{.metadata.annotations.brewlet\.sh/launchers}{"\n"}'
#   java,jaz

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

```bash
helm uninstall brewlet
```

> Uninstalling deletes the control-plane components **and** the chart's `NodeProfile`
> objects. Each profile carries a `node.brewlet.sh/cleanup` finalizer, so the operator
> holds the object while a short-lived `brewlet-cleanup-<profile>` DaemonSet
> (`BREWLET_MODE=cleanup`) removes the Brewlet drop-in or restores the primary-config
> backup, removes the shim + JDK roots, and drops the runtime + capability labels on
> every assigned node — reversing host state automatically before the object is
> garbage-collected (§5.6). Watch it with
> `kubectl get daemonset -n brewlet -w`. If a cluster was provisioned the older way (a
> bare `brewlet.sh/provision=true` node **label** with no profile), drain and clean those
> nodes (or replace them) to fully reverse provisioning.

## Next steps

- **[Configuration](configuration.md)** — tune every knob.
- **[Capability labels and autoscaling](capability-labels-and-autoscaling.md)** —
  connect node-pool provisioning to workload scheduling.
- **[JDK management](jdk-management.md)** — add/patch JDK roots (copy-from-image),
  go multi-arch.
- **[Launchers](launchers.md)** — install and use `jaz`.
