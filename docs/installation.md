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

The commands below install the latest release into the cluster selected by
your current kubectl context. Changing the release of an existing installation
is not a supported in-place upgrade; see [Upgrades](#upgrades).

## 1. Install the CLI

The installer selects the latest release by default and verifies its checksum.
If `BREWLET_VERSION` is already set in your environment, unset it first so the
CLI and chart use the same release. The CLI is installed to `~/.local/bin` by
default; make sure that directory is on your `PATH` before checking the version.

```sh
curl -fsSL https://brewlet.sh/install.sh | sh
brewlet version
```

## 2. Install the control plane

These commands select the latest chart. Use them for a fresh installation or a
reviewed configuration change on an installation already using that release,
not to upgrade an older release. Helm does not upgrade CRDs, so apply the
chart's CRDs first:

```sh
helm show crds oci://ghcr.io/microsoft/charts/brewlet | kubectl apply -f -
helm upgrade --install brewlet \
  oci://ghcr.io/microsoft/charts/brewlet \
  --namespace brewlet --create-namespace \
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
Helm installation, port-forward, HTTPS ingress and access instructions.

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
Brewlet workloads, approve the runtimes your applications need, then create a
`NodeProfile` that installs them on the pool.

### Approve runtimes with the CLI

This example approves two runtimes: Python 3 from the official Docker Hub
image and Java 25 from the Microsoft Build of OpenJDK. Preview each operation
first. The CLI resolves the tag to an immutable digest, checks that the image
covers your nodes' architectures and prints the `RuntimeImage` it would create:

```sh
brewlet k8s runtime add python-3 \
  --image registry-1.docker.io/library/python:3-alpine \
  --uid 65532 --gid 65532 --dry-run
```

After reviewing the output, create both runtimes:

```sh
brewlet k8s runtime add python-3 \
  --image registry-1.docker.io/library/python:3-alpine \
  --uid 65532 --gid 65532
brewlet k8s runtime add java-25 \
  --image mcr.microsoft.com/openjdk/jdk:25-distroless \
  --uid 65532 --gid 65532
```

A few details about these images:

- Docker Hub images need the full `registry-1.docker.io/library/...` name; the
  CLI does not expand short names such as `python:3`.
- Both images run as root by default, so `--uid` and `--gid` set the non-root
  identity that workloads using the runtime will run as.
- Runtimes must not contain setuid or setgid binaries. The Debian-based Python
  images (`python:3`, `python:3-slim`) include them and fail provisioning on the
  node, which is why this example uses the Alpine variant.

See [runtime images](runtime-images.md) for the full image requirements,
GitOps manifests and rotation.

### Create a NodeProfile

This example assumes your cluster already has a worker node pool named
`userpool`. It does not create the pool. Save the following as
`node-profile.yaml`:

```yaml
apiVersion: node.brewlet.sh/v1alpha1
kind: NodeProfile
metadata:
  name: userpool
spec:
  nodePool:
    names: [userpool]
  runtimeImages:
    - id: python-3
      install: Eager
    - id: java-25
      install: OnDemand
  rollout:
    containerdRestart: validated
    maxUnavailable: 1
  tolerations:
    - key: startup-taint.cluster-autoscaler.kubernetes.io/brewlet
      operator: Equal
      value: provisioning
      effect: NoSchedule
```

With `nodePool.key` omitted, Brewlet detects a standard pool label used by GKE,
AKS, EKS or Karpenter. If your cluster uses a different pool label or has
multiple recognized pool label keys, set `nodePool.key` to the correct label
on the `userpool` nodes. See [pools and providers](providers.md) for details.

`Eager` installs the runtime on every node in the pool during provisioning;
`OnDemand` fetches it the first time a workload needs it. Apply the profile and
wait for the pool to become ready. (You can instead declare profiles through
`provisioner.profiles` in your Helm values; see [configuration](configuration.md).)

```sh
kubectl apply -f node-profile.yaml
kubectl wait --for=condition=Ready \
  nodeprofile/userpool --timeout=10m
brewlet k8s runtime list
```

Once a profile exists, `brewlet k8s runtime add` can offer new runtimes on it
directly with `--profile userpool --install Eager|OnDemand`.

The operator generates and reconciles the provisioner worker DaemonSet; do not
edit it. If the profile does not become ready, check the provisioner pod logs
in the `brewlet` namespace and see [troubleshooting](troubleshooting.md).

## 4. Verify the deployment

```sh
kubectl -n brewlet get deployments,pods,services
kubectl get runtimeclass brewlet
kubectl get runtimeimages,nodeprofiles
kubectl get nodes -L brewlet.sh/runtime
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
