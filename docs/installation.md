# Source-built installation

There is no published stable 1.0.0 installer yet; 0.8.0+ beta prereleases
preview the 1.0.0 line. Use one reviewed source revision (or one beta release)
for the CLI, chart, CRDs, control plane and provisioner.
Do not substitute legacy Brewlet 0.7.x's released CLI, chart or application format.
The [release distribution runbook](supported-release.md) describes release
coordinates, exact-artifact verification and the automated publication flow.

For a first deployment, use the [disposable local tutorial](local-kubernetes.md).
This guide describes a platform-managed installation on explicitly controlled
Linux nodes, not a command to run against an unspecified current context.

## Prerequisites and authority

Nodes need containerd 2.x, runc, cgroup v2, systemd/systemctl, crictl and the
supported overlayfs layout. The current integration requires the default
containerd socket and overlayfs snapshotter. Check the exact requirements in
[the node runbook](../provisioner/README.md).

Privileged node access is an installation decision. Select named pools and
review existing containerd configuration and any already-installed handler.
Unmanaged shim binaries/configuration are not silently adopted. Control-plane
nodes are excluded unless explicitly enabled in a profile.

Choose the cluster and namespace before executing commands:

```sh
export KUBECONFIG=/absolute/path/to/reviewed-kubeconfig
export CONTEXT=reviewed-context
export NAMESPACE=brewlet
kubectl --context "$CONTEXT" cluster-info
```

## Build and deliver matching images

Build from a clean checkout without credentials or private work directories in
the Docker build context. The local tutorial constructs a committed-source
context for this reason.

```sh
docker build -f kubernetes/Dockerfile -t registry.example.com/brewlet/control:reviewed .
docker build -f provisioner/Dockerfile -t registry.example.com/brewlet/node:reviewed .
docker push registry.example.com/brewlet/control:reviewed
docker push registry.example.com/brewlet/node:reviewed
```

Record the resulting registry digests and use them in reviewed values:

```yaml
operator:
  image: registry.example.com/brewlet/control@sha256:<control-digest>
admission:
  image: registry.example.com/brewlet/control@sha256:<control-digest>
provisioner:
  image: registry.example.com/brewlet/node@sha256:<node-digest>
  profiles: []
```

Replace the placeholders. The empty profile list intentionally provisions no
pool; it is not an accidental catch-all. Configure component image access
through your platform's namespace/service-account policy. The current chart
does not expose every field from Brewlet 0.x's chart.

## Apply schemas, then install

```sh
kubectl --context "$CONTEXT" apply -f kubernetes/deploy/crds/
helm --kube-context "$CONTEXT" upgrade --install brewlet kubernetes/charts/brewlet \
  --namespace "$NAMESPACE" --create-namespace --values reviewed-values.yaml \
  --wait --timeout 5m
```

Apply current CRDs before every chart upgrade as well. Helm installation does
not by itself mean a node runtime is provisioned.

Approve [runtime images](runtime-images.md), then create a reviewed
`NodeProfile`, either separately or through `provisioner.profiles` in Helm.
Do not edit the generated worker DaemonSet: reconciliation owns it.

## Verify the deployment

```sh
kubectl --context "$CONTEXT" -n "$NAMESPACE" get deployments,pods,services
kubectl --context "$CONTEXT" get runtimeclass brewlet
kubectl --context "$CONTEXT" get runtimeimages,nodeprofiles
kubectl --context "$CONTEXT" get nodes -L brewlet.sh/runtime
```

Check current condition generations, worker events and the absence of the
startup fence. For OnDemand, `available` without a root can be healthy; it is
not equivalent to a successful application start.

Deploy a reviewed application and verify HTTP, probes and its actual runtime
binding. Never infer those from an installed chart or a ready control-plane
Deployment. See [troubleshooting](troubleshooting.md) and
[configuration](configuration.md).

## Upgrades and reversal

Keep previous source/image digests and recovery information. The node's owned
activation transaction validates configuration, restarts/read-checks when
authorized and retains reversal obligations across upgrades. Do not replace
its journal or delete roots to force progress.

Follow [safe removal](uninstallation.md) before uninstalling or reinstalling.
Pre-GA schema compatibility is governed by the [1.0.0 policy](compatibility-policy.md),
not an assumption that Brewlet 0.x's release upgrade commands apply.
