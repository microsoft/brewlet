# Pools, providers and startup fencing

This page describes how to configure node pools and autoscalers for Brewlet.
Brewlet has not been tested for scale-from-zero on EKS, Karpenter or AKS.
Test your provider and tooling versions with newly created nodes before you
rely on these templates.

## Readiness is observed, not templated into existence

The aggregate label is `brewlet.sh/runtime=ready`; per-ID capabilities are
`brewlet.sh/runtime.<id>=available|ready`. Effective readiness also requires
the absence of:

```text
startup-taint.cluster-autoscaler.kubernetes.io/brewlet=provisioning:NoSchedule
```

Application Pods must not tolerate this fence, including through wildcard
tolerations. The provisioner worker needs a matching toleration in the
`NodeProfile`. Brewlet changes OnDemand per-ID labels on first use, on failure
and on idle reclamation, so the provider must not pin them. A pinned
`available` label is unsupported; use mutable labels or an Eager runtime with
the startup fence.

If Brewlet cannot remove a capability label, the worker fences the whole node
instead. If that also fails, provisioning and cleanup stop until the node is
fixed.

## EKS Cluster Autoscaler: simulation tags

Cluster Autoscaler can scale a node group from zero if you describe the
labels its nodes will have. Use node-template tags on the Auto Scaling group,
for example for runtime ID `static`:

```text
k8s.io/cluster-autoscaler/node-template/label/brewlet.sh/runtime = ready
k8s.io/cluster-autoscaler/node-template/label/brewlet.sh/runtime.static = available
```

Set `PropagateAtLaunch=false`. These tags only inform the autoscaler's
simulation; do not apply them as real node labels through bootstrap. Real nodes
join with their pool label, and the worker publishes the runtime labels after
it validates the node.

Select the node group with a profile such as:

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

The runtime ID must already be approved by a `RuntimeImage` that supports the
pool's architectures. This example does not create a node group, credentials
or cloud permissions.

## Karpenter: labels copied to real nodes

Unlike autoscaler tags, Karpenter NodePool template labels are applied to real
nodes. If a template declares runtime labels in advance, it must also apply
the startup fence:

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

This is a fragment of an existing NodePool, not a complete resource. Add your
NodeClass and OS and architecture requirements. In the `NodeProfile`, select
the pool with `{key: karpenter.sh/nodepool, names: [<pool>]}`. Make sure your
policies let Brewlet change the runtime labels.

## AKS: initialization, reimage and pinned labels

AKS manages node pool labels and keeps them pinned. If you set runtime labels
on a pool, use Eager installation with matching `ready` labels, and select the
pool with `{key: agentpool, names: [<pool>]}`.

New nodes must also start with the startup fence, using AKS node
initialization taints, which can be removed. Ordinary `--node-taints` are
reapplied by AKS and cannot be used as the fence.

Check which AKS API and CLI versions support initialization taints, and whether
a setting applies to the cluster or to a single pool. Brewlet does not register
preview features or change cloud configuration.

Changing the setting does not affect existing nodes. Upgrade or reimage the
pool's nodes through your maintenance procedure, then verify that new nodes
join with the fence. If you cannot confirm this, do not set ready labels on the
pool; keep at least one validated node running instead of scaling to zero.

## Scale-in and removal

Removing a taint from an existing node does not fix the provider's template.
Correct pool labels and bootstrap settings when you decommission a pool. If a
node is deleted before Brewlet cleans it up, its cleanup stays pending until
you resolve it; see [safe removal](uninstallation.md).

References: [EKS guidance](https://docs.aws.amazon.com/eks/latest/best-practices/cas.html),
[Karpenter NodePools](https://karpenter.sh/docs/concepts/nodepools/),
[AKS taints](https://learn.microsoft.com/en-us/azure/aks/use-node-taints).
See [multi-architecture fleets](multi-arch.md) and [safe removal](uninstallation.md).
