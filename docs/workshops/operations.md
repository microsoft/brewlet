# Part 1: Enable Brewlet on a Kubernetes cluster

**Audience:** Kubernetes platform engineers and cluster operators.

**Goal:** install Brewlet, provision an approved node JDK, verify the runtime,
and give application developers a small, explicit platform contract.

## 1. Prerequisites

You need:

- cluster-admin access to a disposable Kubernetes cluster;
- containerd 2.0+ nodes using cgroup v2;
- permission to run privileged DaemonSets and modify the node runtime;
- `kubectl`, Helm, and `curl`; and
- an OCI registry repository that Dev participants can push to and cluster
  nodes can pull from.

Confirm the active cluster before changing it:

```bash
kubectl config current-context
kubectl get nodes -o custom-columns=NAME:.metadata.name,RUNTIME:.status.nodeInfo.containerRuntimeVersion
kubectl auth can-i create customresourcedefinitions.apiextensions.k8s.io
```

Do not use a production or shared cluster for this preproduction workshop.

Set the application registry used by both workshop parts:

```bash
export BREWLET_REGISTRY="<registry-host>/<team>"
```

Authenticate with your application registry using your organization's normal
mechanism. Never put access tokens in URLs or shell history. Brewlet's released
CLI, chart, and component images can be downloaded without registry credentials.

Install the latest released CLI with the
[checksum-verifying installer](../getting-started.md#install-the-released-cli-recommended):

```bash
curl -fsSL https://brewlet.sh/install.sh | sh -s -- --version latest --install-dir "$HOME/.local/bin"
export PATH="$HOME/.local/bin:$PATH"
BREWLET_VERSION="$(brewlet version)"
export BREWLET_VERSION
printf 'Using Brewlet %s\n' "$BREWLET_VERSION"
```

Keep this resolved `BREWLET_VERSION` for the chart, components, and developer
handoff below; do not resolve latest again partway through the workshop.
To use a specific release instead, replace `latest` in the installer command
with its release number. No Go toolchain or source build is required. For custom
component builds, follow the [source-build installation path](../installation.md#released-components)
and use the same source revision for the CLI.

## 2. Preview the installation

Choose an administrator-approved JDK 21 image and save `my-jdks.yaml`:

```yaml
provisioner:
  jdks:
    - distribution: temurin
      feature: 21
      source:
        image: docker.io/library/eclipse-temurin@sha256:<64-lowercase-hex>
        javaHome: /opt/java/openjdk
```

Replace the placeholder with the **full digest you approved**, checking the
image's supported node architectures and JDK root. This is a template, not a
shipped runtime catalog. Keep this file for installation and upgrades. If you
choose a different Java feature, update `BREWLET_JDK` in the developer handoff.

Render and inspect the released chart before applying it:

```bash
export BREWLET_POOL="java-workers"

helm template brewlet oci://ghcr.io/microsoft/charts/brewlet \
  --version "$BREWLET_VERSION" \
  --namespace brewlet \
  --set provisioner.pools="{$BREWLET_POOL}" \
  --values my-jdks.yaml \
  > brewlet-rendered.yaml
```

Both the pool name and JDK sources are required: leave `provisioner.pools` or
`provisioner.jdks` empty and rendering fails. Brewlet has no built-in JDK or
launcher catalog; vanilla `java` comes from your chosen JDK. Set
`provisioner.poolKey` for a custom node-pool label on bare metal or kubeadm.
Control-plane nodes are excluded by default regardless of taints; only for a
single-node development cluster, add `--set provisioner.includeControlPlane=true`
to both preview and install. See [pool configuration](../configuration.md#where-the-provisioner-may-run).

## 3. Install Brewlet

```bash
helm upgrade --install brewlet oci://ghcr.io/microsoft/charts/brewlet \
  --version "$BREWLET_VERSION" \
  --namespace brewlet \
  --create-namespace \
  --set provisioner.pools="{$BREWLET_POOL}" \
  --values my-jdks.yaml
```

The chart installs the operator and admission components. The operator creates
the `brewlet` RuntimeClass and a privileged provisioner DaemonSet that installs
the shim and JDK on each selected node.

## 4. Prepare the developer namespace

```bash
export BREWLET_CONTEXT="$(kubectl config current-context)"
export BREWLET_NAMESPACE="brewlet-workshop"
export BREWLET_JDK="21"

kubectl create namespace "$BREWLET_NAMESPACE" \
  --dry-run=client -o yaml | kubectl apply -f -
```

Apply your normal developer RBAC before the handoff. Registry credentials and
organization-specific RBAC remain part of the platform's existing security
model.

## 5. Wait for and diagnose the platform

```bash
kubectl rollout status deployment/brewlet-operator -n brewlet --timeout=5m
kubectl rollout status deployment/brewlet-admission -n brewlet --timeout=5m
kubectl rollout status daemonset -n brewlet \
  -l app=brewlet-node-provisioner --timeout=10m
kubectl get pods -n brewlet
kubectl get runtimeclass brewlet
kubectl get nodes -L brewlet.sh/runtime
brewlet doctor \
  --context "$BREWLET_CONTEXT" \
  --namespace "$BREWLET_NAMESPACE"
```

Every node selected by the profile must report `brewlet.sh/runtime=ready`.
Inspect the advertised runtime inventory:

```bash
kubectl get nodes -o custom-columns=\
NAME:.metadata.name,\
STATE:.metadata.annotations.brewlet\\.sh/provision-state,\
JDKS:.metadata.annotations.brewlet\\.sh/jdks,\
LAUNCHERS:.metadata.annotations.brewlet\\.sh/launchers
```

If a node does not become ready:

```bash
kubectl get events -A --sort-by=.lastTimestamp | tail -30
kubectl logs -n brewlet -l app=brewlet-node-provisioner \
  --all-containers --tail=100
```

Do not hand the cluster to developers until the RuntimeClass exists and at least
one schedulable node is ready.

## 6. Define the developer handoff

Give the developer:

| Value | Meaning |
|---|---|
| Kubernetes context | Cluster containing the Brewlet runtime |
| Namespace | Namespace where the developer may deploy |
| RuntimeClass | `brewlet` |
| Supported JDK | `21` in this workshop |
| Brewlet version | Resolved `$BREWLET_VERSION` from the CLI installation |
| Registry prefix | Repository where the developer can push OCI images |
| Pull secret | Required only when the registry is private |

```bash
printf '%s\n' \
  "export BREWLET_CONTEXT=\"$BREWLET_CONTEXT\"" \
  "export BREWLET_NAMESPACE=\"$BREWLET_NAMESPACE\"" \
  "export BREWLET_JDK=\"$BREWLET_JDK\"" \
  "export BREWLET_VERSION=\"$BREWLET_VERSION\"" \
  "export BREWLET_REGISTRY=\"$BREWLET_REGISTRY\""
```

Continue with [Part 2: Build and deploy a workload](developers.md).

## 7. Optional platform exercises

- Configure named `NodeProfile`s for different node pools or JDK inventories.
- Add the `jaz` launcher.
- Mirror component and JDK images into an internal registry.
- Run `./integration-tests/e2e/run.sh --tier 13` on a disposable test cluster to
  exercise the complete `NodeProfile` lifecycle.

## Cleanup

Complete cleanup only after the Dev workshop:

```bash
helm uninstall brewlet -n brewlet --timeout 5m
kubectl get nodeprofiles -w
```

Wait for profile cleanup finalizers to restore node state before deleting the
cluster. See [Installation](../installation.md) for installation details,
scoping, upgrades, and uninstall behavior.
