# Installation

This guide installs a released Brewlet on a Kubernetes cluster whose Linux
nodes you control. For a first look, use the
[disposable local tutorial](local-kubernetes.md) instead.

Every artifact comes from [GitHub Releases](https://github.com/microsoft/brewlet/releases).
Use the same release for the CLI, chart, CRDs and component images. The
[release distribution runbook](supported-release.md) describes release
coordinates and artifact verification.

## Prerequisites

Nodes that will run Brewlet workloads need:

- Linux with cgroup v2 and systemd/systemctl
- containerd 2.x on its default socket, using the overlayfs snapshotter
- runc and crictl

See [the node runbook](../provisioner/README.md) for the exact requirements.

Provisioning is privileged and modifies node configuration, so Brewlet only
touches node pools you name in a `NodeProfile`. It does not adopt unmanaged
shim binaries or containerd configuration, and control-plane nodes are excluded
unless a profile explicitly enables them. Review existing containerd
configuration on the target pools before you start.

Pin every command to a reviewed cluster and namespace rather than the current
context:

```sh
export KUBECONFIG=/absolute/path/to/reviewed-kubeconfig
export CONTEXT=reviewed-context
export NAMESPACE=brewlet
export BREWLET_VERSION='<published-release-version>'
kubectl --context "$CONTEXT" cluster-info
```

Replace the version placeholder with the same published release for every
component. An existing installation must already use that release; changing
releases is not a supported in-place upgrade.

## 1. Install the CLI

```sh
curl -fsSL https://brewlet.sh/install.sh | sh -s -- --version "$BREWLET_VERSION"
brewlet version
```

The script downloads the selected release and verifies its checksum.

## 2. Install the control plane

Helm does not upgrade CRDs, so apply the chart's CRDs first, on a fresh install
or a reviewed change within the same release. Then install the chart:

```sh
helm show crds oci://ghcr.io/microsoft/charts/brewlet \
  --version "$BREWLET_VERSION" \
  | kubectl --context "$CONTEXT" apply -f -
helm --kube-context "$CONTEXT" upgrade --install brewlet \
  oci://ghcr.io/microsoft/charts/brewlet \
  --version "$BREWLET_VERSION" \
  --namespace "$NAMESPACE" --create-namespace \
  --wait --timeout 5m
```

The chart installs the operator, admission webhook and `brewlet` RuntimeClass,
using the component images of the same release. By default it provisions no
node pool. To customize the installation, pass a values file with
`--values values.yaml`; see [configuration](configuration.md).

### Optional cluster dashboard

The chart's optional `dashboard.enabled=true` component installs a ClusterIP
dashboard for platform teams. It uses each operator's Kubernetes bearer token,
not a privileged shared pod identity. Use a release that includes the dashboard;
its published chart contains the matching digest-pinned image. See
[dashboard installation and RBAC](dashboard.md#install-on-the-cluster) for
version-pinned Helm installation, port-forward, HTTPS ingress and access
instructions.

### Mirrored images

If your cluster pulls from a private registry, mirror the release images and
pin their digests in your values file:

```yaml
operator:
  image: registry.example.com/brewlet/brewlet-operator@sha256:<operator-digest>
admission:
  image: registry.example.com/brewlet/brewlet-admission@sha256:<admission-digest>
provisioner:
  image: registry.example.com/brewlet/brewlet-node-provisioner@sha256:<node-digest>
dashboard:
  image: registry.example.com/brewlet/brewlet-dashboard@sha256:<dashboard-digest>
```

The dashboard coordinate applies only to releases that include that component.
Configure registry access through your platform's namespace and
service-account policy.

## 3. Provision node pools

Installing the chart does not provision any node. To make a pool ready for
Brewlet workloads:

1. Approve the [runtime images](runtime-images.md) your applications need.
2. Create a reviewed `NodeProfile` that selects the pool, either directly or
   through `provisioner.profiles` in your Helm values.

The operator generates and reconciles the provisioner worker DaemonSet; do not
edit it.

## 4. Verify the deployment

```sh
kubectl --context "$CONTEXT" -n "$NAMESPACE" get deployments,pods,services
kubectl --context "$CONTEXT" get runtimeclass brewlet
kubectl --context "$CONTEXT" get runtimeimages,nodeprofiles
kubectl --context "$CONTEXT" get nodes -L brewlet.sh/runtime
```

Check that conditions report the current generation, worker events are clean
and the startup fence is gone from provisioned nodes. With `OnDemand`
installation, a runtime reported as `available` but not yet fetched is healthy.

A ready control plane does not prove that applications run. Deploy a reviewed
application and confirm its HTTP responses, probes and runtime binding. See
[packaging and deploying applications](applications.md) and [troubleshooting](troubleshooting.md).

## Upgrades

Brewlet does not currently promise in-place upgrades, downgrades or reuse of
persisted state across releases. A reviewed Helm values change within the same
release is not a platform-release upgrade. See the
[compatibility policy](compatibility-policy.md) and
[bounded transitions](supported-release.md#bounded-transitions-and-recovery)
before changing releases. Do not edit the provisioner's journal or delete
runtime roots to force progress.

## Uninstall

Follow [safe removal](uninstallation.md) before uninstalling or reinstalling.
Do not bypass cleanup finalizers.
