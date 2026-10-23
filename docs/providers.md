# Pools, providers and startup fencing

These are configuration contracts, **not executed EKS/Karpenter/AKS
scale-from-zero certification**. Qualify your actual provider/tooling versions
and newly created nodes before relying on templates.

## Readiness is observed, not templated into existence

The aggregate label is `brewlet.sh/runtime=ready`; per-ID capabilities are
`brewlet.sh/runtime.<id>=available|ready`. Effective readiness also requires
the absence of:

```text
startup-taint.cluster-autoscaler.kubernetes.io/brewlet=provisioning:NoSchedule
```

Application Pods must not tolerate this fence, including wildcard tolerations.
Workers require explicit matching tolerations. OnDemand per-ID labels must be
mutable during first use, failure and idle reclamation. A pinned `available`
label is unsupported; use mutable labels or a correctly fenced Eager recipe.

If withdrawal is refused, the authorized worker must apply/read back a node-wide
fence. If both operations fail, progress and cleanup stay blocked.

## EKS Cluster Autoscaler: simulation tags

On a reviewed Auto Scaling group, a baseline and ID `static` may be described
to the autoscaler using simulation-only tags:

```text
k8s.io/cluster-autoscaler/node-template/label/brewlet.sh/runtime = ready
k8s.io/cluster-autoscaler/node-template/label/brewlet.sh/runtime.static = available
```

Use `PropagateAtLaunch=false`. These tags must not become unvalidated real-node
labels through bootstrap. Real nodes join with their pool identity; the worker
publishes observed runtime capabilities.

Select the actual group through a profile such as:

```yaml
apiVersion: node.brewlet.sh/v1alpha1
kind: NodeProfile
metadata:
  name: runtime-pool
spec:
  nodePool:
    key: eks.amazonaws.com/nodegroup
    names: [runtime-pool]
  runtimeImages:
    - id: static
      install: OnDemand
  tolerations:
    - key: startup-taint.cluster-autoscaler.kubernetes.io/brewlet
      operator: Equal
      value: provisioning
      effect: NoSchedule
  rollout:
    containerdRestart: validated
```

The ID must already be approved and cover actual pool platforms. This example
does not create a node group, credentials or cloud permissions.

## Karpenter: labels copied to real nodes

Karpenter NodePool template labels reach NodeClaims/Nodes, unlike simulation-only
tags. If templates predeclare readiness, they must also provide the removable
startup fence:

```yaml
spec:
  template:
    metadata:
      labels:
        brewlet.sh/runtime: ready
        brewlet.sh/runtime.static: available
    spec:
      startupTaints:
        - key: startup-taint.cluster-autoscaler.kubernetes.io/brewlet
          value: provisioning
          effect: NoSchedule
```

This is a fragment for an existing reviewed NodePool, not a complete resource.
Supply the actual NodeClass and OS/architecture requirements. Use the
corresponding Brewlet profile pool selector
`{key: karpenter.sh/nodepool, names: [<actual-pool>]}`.
Verify that policies permit runtime capability changes.

## AKS: initialization, reimage and pinned labels

AKS pool labels are provider-managed real-node labels. For a provider-pinned
recipe, use Eager with matching `ready` labels and select the actual pool with
`{key: agentpool, names: [<actual-pool>]}`.

The removable startup fence must be arranged through supported node
initialization taints before accepting templated readiness. Ordinary pinned
`--node-taints` are not an equivalent removable initialization mechanism.

Check current AKS API/feature/tooling support. The retained contract describes
managed-cluster-level initialization configuration, not an agent-pool request
carrying an unsupported initialization field. Cluster CLI options may affect
all pools; pool-scoped configuration needs a reviewed supported request.
Brewlet does not register preview features or mutate cloud configuration.

An initialization update alone is insufficient: roll/reimage the intended
nodes under the platform maintenance procedure and verify that new/reimaged
nodes actually join fenced. If this cannot be established, do not template
ready labels; retain nonzero validated capacity instead.

## Scale-in and removal

Removing a taint from a current Node does not repair stale provider templates.
Correct pool labels/bootstrap settings as part of decommissioning. Missing
hosts retain original UID-bound cleanup obligations.

References: [EKS guidance](https://docs.aws.amazon.com/eks/latest/best-practices/cas.html),
[Karpenter NodePools](https://karpenter.sh/docs/concepts/nodepools/),
[AKS taints](https://learn.microsoft.com/en-us/azure/aks/use-node-taints).
See [multi-architecture fleets](multi-arch.md) and [safe removal](uninstallation.md).
