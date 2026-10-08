# Capability labels and autoscaling

Brewlet publishes node labels that describe which runtime capabilities are
actually ready, then admission adds matching required node affinity to workloads.
This page explains how to connect that behavior to node pools and autoscalers.

!!! info "Canonical capability-label contract"

    The complete key catalog, token grammar, matching semantics, and compatibility
    guarantees are maintained in the
    [Brewlet capability-label contract](https://github.com/microsoft/brewlet/blob/main/specs/CAPABILITY_LABELS.md).
    Treat that document as normative; this page focuses on operator workflows.

Related: [Installation](installation.md) · [Configuration](configuration.md) ·
[JDK management](jdk-management.md).

---

## How capabilities reach the scheduler

1. A `NodeProfile` selects one or more node pools with `spec.nodePool`, declares
   the JDKs and launchers Brewlet must install, and optionally authorizes AppCDS
   regeneration with `spec.appCDS.regenerationEnabled`.
2. The operator durably records matching node identities, acquires exclusive
   UID-bound claims, and only then places provisioners on those nodes.
3. The provisioner installs and validates the inventory, registers the runtime,
   and only then publishes `brewlet.sh/runtime=ready` and the corresponding
   capability labels.
4. For an explicit workload request, admission verifies the current ready fleet
   plus the inventory declared by valid `NodeProfile`s, and injects required
   affinity for the requested JDK, launcher, architecture, and AppCDS policy.

For example, this profile prepares an EKS managed node group:

```yaml
apiVersion: node.brewlet.sh/v1alpha1
kind: NodeProfile
metadata:
  name: jdk21
spec:
  nodePool:
    key: eks.amazonaws.com/nodegroup
    names: ["jdk21-workers"]
  jdks:
    - distribution: temurin
      feature: 21
      source:
        image: docker.io/library/eclipse-temurin@sha256:85f00967bcc624fc19fa9c2cf124ea426a5363898e267141726f31f358c2e14b
        javaHome: /opt/java/openjdk
  launchers:
    - name: jaz
      source:
        image: mcr.microsoft.com/openjdk/jdk@sha256:bfde2ed613f4c67c112d1592452575d3a1dc9ce5f7d75821bb7752aa786fa575
        path: /usr/bin/jaz
  appCDS:
    regenerationEnabled: true
```

After preparation succeeds, the node advertises the runtime and inventory:

```text
brewlet.sh/runtime=ready
brewlet.sh/jdk.temurin-21=true
brewlet.sh/jdk-feature.21=true
brewlet.sh/launcher.java=true
brewlet.sh/launcher.jaz=true
brewlet.sh/appcds-regeneration=true
```

The capability-label values are not the scheduling contract. JDK, launcher, and
AppCDS capabilities are matched by **key presence**, using `Operator: Exists`;
do not write policies that require `=true`. Disabled AppCDS policy removes its
key. The exact `runtime=ready` value is value-sensitive and is selected by the
`brewlet` `RuntimeClass`.

Operator-managed provisioning selects nodes through `NodeProfile.spec.nodePool`.

---

## Admission-injected affinity

A distribution-agnostic JDK 21 request with the `jaz` launcher and AppCDS
regeneration produces requirements equivalent to:

```yaml
spec:
  runtimeClassName: brewlet
  affinity:
    nodeAffinity:
      requiredDuringSchedulingIgnoredDuringExecution:
        nodeSelectorTerms:
          - matchExpressions:
              - key: brewlet.sh/jdk-feature.21
                operator: Exists
              - key: brewlet.sh/launcher.jaz
                operator: Exists
              - key: brewlet.sh/appcds-regeneration
                operator: Exists
```

The `brewlet` `RuntimeClass` additionally selects:

```yaml
nodeSelector:
  brewlet.sh/runtime: ready
```

An exact distribution request uses a key such as
`brewlet.sh/jdk.temurin-21`. A non-portable artifact also receives a
`kubernetes.io/arch In [...]` requirement. AppCDS affinity is added only when
`brewlet.sh/cds-regenerate: "true"` is requested. If the pod already has
required node affinity, Brewlet adds its requirements to every existing selector
term so the author's alternatives remain intact while every alternative still
requires the requested Brewlet capabilities.

Admission validates explicit requests against the **ready fleet plus every valid
`NodeProfile`** before the scheduler sees the pod. One candidate (a ready node or
a profile) must provide the whole request; capabilities are never combined
across candidates. A request nothing provides is denied (`NoCompatibleJDK`,
`NoCompatibleLauncher`); a regeneration request with no otherwise-compatible
authorized node or profile is denied with `AppCDSRegenerationDisabled`.

When only a profile provides the request, such as a pool scaled to zero, the pod
is admitted with an admission warning and stays `Pending` until the autoscaler
adds a node and the profile provisions it:

```text
Warning: brewlet: no ready node currently provides jdk=temurin-21; the pod will
stay Pending until matching capacity is provisioned (declared by NodeProfile "jdk21")
```

Architecture is never a denial. Profiles do not declare architecture (it comes
from the VM size), so a `brewlet.sh/arch` request without a ready node of that
architecture is admitted with the same warning and resolved by
`kubernetes.io/arch` affinity.

The AppCDS label is only a scheduling hint: the shim independently requires the
root-owned `/opt/brewlet/policy/appcds-regeneration-enabled` sentinel.

---

## Cluster Autoscaler

Cluster Autoscaler simulates whether a future node from a group could schedule a
pending pod. Its node-group template must therefore advertise the same runtime,
JDK, launcher, and AppCDS labels that the group's `NodeProfile` will install.

On AKS, the managed Cluster Autoscaler builds a scale-from-zero template only
from the agent pool spec (`--labels`, `--node-taints`, and the VM size). It
ignores `k8s.io_cluster-autoscaler_node-template_label_*` VMSS tags. Pool labels
and taints are also pinned: AKS's node admission webhook refuses to remove or
change them on a node. A pool that templates `brewlet.sh/runtime=ready` would
therefore advertise readiness before installation, and could never withdraw it
during reprovisioning or cleanup. That's unsupported, so **AKS-managed Cluster
Autoscaler pools can't scale from zero for Brewlet capability requests**. Keep
at least one node in each Brewlet pool:

```bash
for pool in javax64 javaarm; do
  az aks nodepool update -g "$RG" --cluster-name "$CLUSTER" -n "$pool" \
    --update-cluster-autoscaler --min-count 1 --max-count 5
done
```

Once a pool has a provisioned node, Cluster Autoscaler uses that real node,
including its provisioner-published Brewlet labels, as the template for further
scale-out. A single profile can cover pools of different architectures, and
workloads pick one with `spec.arch` instead of naming a pool:

```yaml
apiVersion: node.brewlet.sh/v1alpha1
kind: NodeProfile
metadata:
  name: jdk21
spec:
  nodePool:
    key: agentpool
    names: [javax64, javaarm]
  jdks:
    - distribution: temurin
      feature: 21
      source:
        image: docker.io/library/eclipse-temurin@sha256:85f00967bcc624fc19fa9c2cf124ea426a5363898e267141726f31f358c2e14b
        javaHome: /opt/java/openjdk
```

`kubernetes.io/arch` is derived from the VM size. Never put Brewlet labels in
`--labels` or Brewlet taints in `--node-taints`. AKS node initialization taints
(preview, removable with `kubectl`) don't help either. AKS accepts them only on
managed-cluster operations (`az aks update --nodepool-initialization-taints`),
which apply them to every pool in the cluster. A per-pool request is rejected
with `NodeInitializationTaintsFeatureNotSupported`. For scale-from-zero on AKS, use
[Node Auto Provisioning](#karpenter) (Karpenter) with a Brewlet startup taint.

On EKS, add synthetic node-template labels to the backing Auto Scaling group:

```bash
aws autoscaling create-or-update-tags --tags \
  "ResourceId=$ASG,ResourceType=auto-scaling-group,Key=k8s.io/cluster-autoscaler/node-template/label/brewlet.sh/runtime,Value=ready,PropagateAtLaunch=false" \
  "ResourceId=$ASG,ResourceType=auto-scaling-group,Key=k8s.io/cluster-autoscaler/node-template/label/brewlet.sh/jdk.temurin-21,Value=true,PropagateAtLaunch=false" \
  "ResourceId=$ASG,ResourceType=auto-scaling-group,Key=k8s.io/cluster-autoscaler/node-template/label/brewlet.sh/jdk-feature.21,Value=true,PropagateAtLaunch=false" \
  "ResourceId=$ASG,ResourceType=auto-scaling-group,Key=k8s.io/cluster-autoscaler/node-template/label/brewlet.sh/launcher.java,Value=true,PropagateAtLaunch=false" \
  "ResourceId=$ASG,ResourceType=auto-scaling-group,Key=k8s.io/cluster-autoscaler/node-template/label/brewlet.sh/launcher.jaz,Value=true,PropagateAtLaunch=false" \
  "ResourceId=$ASG,ResourceType=auto-scaling-group,Key=k8s.io/cluster-autoscaler/node-template/label/brewlet.sh/appcds-regeneration,Value=true,PropagateAtLaunch=false"
```

These tags are simulation hints, not real node readiness claims.
`PropagateAtLaunch=false` prevents them from being copied onto a joining node.
The node joins with its provider pool label, the matching `NodeProfile` prepares
it, and the Brewlet provisioner publishes the real labels only after validation.

Keep the template labels synchronized with the `NodeProfile`. Advertising a
capability the profile does not install can trigger an unnecessary scale-up,
although the scheduler still waits for the real provisioner-owned labels before
placing the workload.

---

## Karpenter

Karpenter handles template labels differently: labels under
`NodePool.spec.template.metadata.labels` are copied onto real NodeClaims and
Nodes. Do **not** put `brewlet.sh/runtime=ready` or Brewlet capability labels on a
Karpenter `NodePool` when Brewlet will be installed after the node registers,
unless the pool also declares the Brewlet startup taint (see below). Otherwise
the scheduler can place a pod before the runtime is ready.

A `NodeProfile` can safely target Karpenter nodes that are created for other
demand:

```yaml
apiVersion: karpenter.sh/v1
kind: NodePool
metadata:
  name: brewlet-jdk21
spec:
  template:
    spec:
      requirements:
        - key: kubernetes.io/arch
          operator: In
          values: ["amd64", "arm64"]
---
apiVersion: node.brewlet.sh/v1alpha1
kind: NodeProfile
metadata:
  name: brewlet-jdk21
spec:
  nodePool:
    key: karpenter.sh/nodepool
    names: ["brewlet-jdk21"]
  jdks:
    - distribution: temurin
      feature: 21
      source:
        image: docker.io/library/eclipse-temurin@sha256:85f00967bcc624fc19fa9c2cf124ea426a5363898e267141726f31f358c2e14b
        javaHome: /opt/java/openjdk
  launchers:
    - name: jaz
      source:
        image: mcr.microsoft.com/openjdk/jdk@sha256:bfde2ed613f4c67c112d1592452575d3a1dc9ce5f7d75821bb7752aa786fa575
        path: /usr/bin/jaz
  appCDS:
    regenerationEnabled: true
```

This ensures created nodes receive the inventory, but it is not
capability-driven scale-from-zero: Karpenter cannot infer labels that another
DaemonSet will add after registration.

### Scale-from-zero with a startup taint

Karpenter, including AKS Node Auto Provisioning, ignores a `NodePool`'s
`startupTaints` when it simulates scheduling, and doesn't pin labels on the
node. A pool can therefore template the profile's ready label set if it also
declares the Brewlet startup taint
`startup-taint.cluster-autoscaler.kubernetes.io/brewlet`. Until the taint is
gone, the node isn't ready to Brewlet, and the scheduler places nothing there.
The provisioner first withdraws the templated labels, installs and validates
the runtime, and publishes the real labels. Only then does it remove the taint:

```yaml
apiVersion: karpenter.sh/v1
kind: NodePool
metadata:
  name: brewlet-jdk21
spec:
  template:
    metadata:
      labels:
        brewlet.sh/runtime: ready
        brewlet.sh/jdk.temurin-21: "true"
        brewlet.sh/jdk-feature.21: "true"
        brewlet.sh/launcher.java: "true"
    spec:
      startupTaints:
        - key: startup-taint.cluster-autoscaler.kubernetes.io/brewlet
          value: provisioning
          effect: NoSchedule
---
apiVersion: node.brewlet.sh/v1alpha1
kind: NodeProfile
metadata:
  name: brewlet-jdk21
spec:
  nodePool:
    key: karpenter.sh/nodepool
    names: ["brewlet-jdk21"]
  tolerations:
    - key: startup-taint.cluster-autoscaler.kubernetes.io/brewlet
      operator: Exists
      effect: NoSchedule
  jdks:
    - distribution: temurin
      feature: 21
      source:
        image: docker.io/library/eclipse-temurin@sha256:85f00967bcc624fc19fa9c2cf124ea426a5363898e267141726f31f358c2e14b
        javaHome: /opt/java/openjdk
```

The `NodeProfile` must tolerate the startup taint, because provisioner
tolerations are opt-in. The templated labels must exactly match the profile
inventory. Without the startup taint, the templated labels are unsupported.

### Pre-baked images

A Karpenter pool is also eligible for Brewlet capability requests when it uses an
immutable node image or a pre-registration bootstrap that installs and validates
Brewlet before kubelet registers the node. Only then may the Karpenter template
truthfully publish the complete ready label set without a startup taint:

```yaml
spec:
  template:
    metadata:
      labels:
        brewlet.sh/runtime: ready
        brewlet.sh/jdk.temurin-21: "true"
        brewlet.sh/jdk-feature.21: "true"
        brewlet.sh/launcher.java: "true"
        brewlet.sh/launcher.jaz: "true"
        brewlet.sh/appcds-regeneration: "true"
```

The image inventory and labels must move together. Publishing
`brewlet.sh/runtime=ready` before bootstrap has made that claim true is
unsupported. Publishing `brewlet.sh/appcds-regeneration` additionally requires
the bootstrap to install the root-owned AppCDS policy sentinel; the shim will
reject regeneration if only the label exists.

---

## Choose an integration pattern

| Provisioning model | Autoscaler configuration | Scale-from-zero for capability requests |
|---|---|---|
| Cluster Autoscaler + `NodeProfile` | Synthetic node-template labels matching the profile; provisioner publishes the real labels | Yes, where template labels aren't applied to real nodes (for example EKS ASG tags); admission counts the profile's declared inventory |
| AKS-managed Cluster Autoscaler + `NodeProfile` | `--min-count 1`; no Brewlet `--labels` or `--node-taints` | No; scale-out from the first provisioned node works |
| Karpenter + post-registration `NodeProfile` | Select the Karpenter pool in `NodeProfile.spec.nodePool`; do not template Brewlet readiness labels | No; suitable for nodes created for other demand |
| Karpenter / AKS NAP + `NodeProfile` + Brewlet startup taint | Template the profile's labels plus `startupTaints`; the profile tolerates the taint | Yes; the provisioner withdraws the templated labels and releases the taint when ready |
| Karpenter + pre-baked or pre-registration Brewlet bootstrap | Template the complete labels only after the image/bootstrap makes them true | Yes, when a valid `NodeProfile` declares the same inventory (admission needs a ready node or profile) |

## Scale-in, consolidation, and replacement

Scale-out does not establish a safe scale-in workflow. Before removing a managed
Node or VM, drain its workloads, exclude it from every remaining profile's
desired targets (including any catch-all), and keep it registered and reachable
until retirement cleanup, worker teardown, and ownership release finish.
Cordon/drain alone does not change profile membership. Coordinate any controller
that would restore pool labels or reclaim the node while retirement is running.

Brewlet does not supply an autoscaler termination hook. Suspend automatic
scale-in/consolidation or provide external deprovisioning coordination; do not
assume deleting a Kubernetes Node proves its host was cleaned. Before a durable
cleanup-completion checkpoint, a missing/reused Node UID is copied into the
durable `status.detachedRetirements` history after old workers terminate. It is
then removed from active membership, so healthy and genuinely distinct replacement
hosts can provision and upgrade without waiting for evidence. Retained hosts'
runtime roots and capability advertisements are preserved. `RetirementPending`
reports outstanding cleanup history independently of active `Ready` status.

Deletion/uninstall and resolution of missing hosts' cleanup obligations remain
blocked without
[authorized, identity-bound retirement evidence](installation.md#verified-external-host-retirement).
Recreating the Node name or observing a stopped VM does not resolve the original
obligation. Known providerID/systemUUID conflicts prevent host reuse, including
re-registration under a different Node name. An administrator must verify
permanent retirement of the original host,
then submit immutable `NodeRetirementEvidence`; Brewlet does not verify the cloud
record itself. A disconnected original host can recover normally if its Node
object and UID remain intact. After durable `Teardown` has already proven cleanup,
later Node disappearance may permit completion once workers are gone. Never
delete ownership metadata or finalizers to manufacture that proof.

The internal `brewlet.sh/owner-uid`, `brewlet.sh/owner-node-uid`, and
`brewlet.sh/owner-name` metadata is not a capability template. Never copy it
into autoscaler templates or pre-baked images. See
[node ownership and retargeting](jdk-management.md#node-ownership-and-retargeting).

## Next steps

- **[Brewlet capability-label contract](https://github.com/microsoft/brewlet/blob/main/specs/CAPABILITY_LABELS.md)** —
  normative keys, grammar, matching semantics, and compatibility guarantees.
- **[Configuration](configuration.md)** — define `NodeProfile` inventories and
  pool selection.
- **[JDK management](jdk-management.md)** — install and validate JDK roots.
