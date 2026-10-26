# Brewlet operations and verification

The maintained reader journeys start at [the documentation overview](index.md):
[local deployment](local-kubernetes.md), [installation](installation.md),
[provider templates](providers.md), [operations](operations.md) and
[troubleshooting](troubleshooting.md).
This source-only background index is excluded from the built site; its useful
provider material is now published in the dedicated, reviewed provider guide.
The branch has scoped local implementation acceptance, not a stable release or
cloud-provider certification. Check the current ledger and
[verification boundaries](verification.md).

## Contracts

Only a reviewed `RuntimeImage` ID selects runtime bytes. Its source is a
canonical digest-pinned image, and its Kubernetes UID establishes the inventory
epoch. A stable ID promises compatible ABI, numeric defaults, documented account/NSS behavior, runtime line, PATH and
documented tools across rotations. Incompatible updates and frozen holds need
separate administrator-owned IDs. Runtime rotations never remediate dependency
copies packaged by the application under `/app`.

All application, init, sidecar and ephemeral containers use v1 artifacts and the
same pod-wide ID. The infrastructure sandbox alone follows the ordinary runc
path. Workload manifests must explicitly select native-content architectures
using standard `kubernetes.io/arch` selection; artifact platform declarations
are not scheduler instructions. The CLI never infers either.

The runtime supplies PATH when present; absent PATH requires absolute initial/Exec executables.
Explicit pod PATH is ignored with an admission
warning. Runtime Env defaults are merged with final CRI environment values.
Initial and exec processes must share effective environment, non-root identity,
HOME and the leased generation. Explicit argv and working-directory overrides
retain Kubernetes semantics. No VM/interpreter flags are injected.

Packaged `/app` is read-only and world-readable. It cannot contain secrets.
Use Secret volumes or another scoped secret mechanism and explicit writable
volumes for application data. Non-root UID/GID and no privilege escalation are
mandatory, including on fail-open admission and exec paths.

## Provisioning, fencing and rotation

Only the authorized provisioner may write runtime roots. Before host mutation
it checks the original Node UID, UID-bound claim, literal profile UID, durable
profile generation and target authority. Requests are not authority. Initial
fence failures must not mutate even request markers or advertisements.

Root publication, current resolution, leases and reclamation require the
per-ID lock. Running tasks retain their generation across rotation. Collection
must conservatively preserve roots and cached layers while tasks, execs, mounts
or uncertain recovery evidence remain. Authorization expires after five minutes
by default and cannot be extended without fresh cluster checks.

An OnDemand-only node validates the shim, handler, forwarded annotations,
ownership and authorization before baseline readiness, without fetching roots.
First-use requests have a bounded wait and kubelet retries. Failed installation
withdraws per-ID capability and backs off; it must not silently leave a false
`available` advertisement.

The startup fence is the removable NoSchedule taint
`startup-taint.cluster-autoscaler.kubernetes.io/brewlet=provisioning`.
Application pods cannot tolerate it, including via wildcard tolerations.
Readiness observations must account for it even when provider-pinned labels
continue to say ready. If a provider refuses capability withdrawal, the
authorized worker must apply **and read back** the node-wide fence. If neither
withdrawal nor fence succeeds, progress and deletion remain blocked. Cleanup
does not authorize fence removal while stale provider labels remain.

Rotation restarts require both cluster enablement and a workload-object opt-in.
They patch templates, never delete pods or change strategies. The propagated
epoch/generation barrier prevents stale starts even on already-bound pods.
OnDelete, paused, partitioned, cold-install and excluded-node rollouts require
accurate deferred/not-applicable status; a template patch is not completed
remediation.

## Provider templates

These are configuration contracts, not a claim of verified cloud autoscaling.
Only kind-based node execution and controlled provider-refusal policies have
been exercised locally. No EKS, Karpenter or AKS provider scale-from-zero has
been verified. An accepted zero-node inventory/profile and a Pending Pod are
API/placement observations, not proof that a provider will create a usable node.

All template capabilities must exactly match the approved inventory offered by
the pool: use `brewlet.sh/runtime=ready` for the validated baseline and
`brewlet.sh/runtime.<id>=available` for a cold OnDemand ID or `ready` for Eager.
Do not publish the old JDK, feature, architecture, libc or Java-ABI label family.
Workloads select native architectures explicitly with `kubernetes.io/arch`;
template labels never replace image platform validation.

### EKS Cluster Autoscaler: simulation-only tags

For an existing managed node group's Auto Scaling group `$ASG`, the following
tags describe a baseline plus approved OnDemand ID `static`:

```sh
aws autoscaling create-or-update-tags --tags \
  "ResourceId=$ASG,ResourceType=auto-scaling-group,Key=k8s.io/cluster-autoscaler/node-template/label/brewlet.sh/runtime,Value=ready,PropagateAtLaunch=false" \
  "ResourceId=$ASG,ResourceType=auto-scaling-group,Key=k8s.io/cluster-autoscaler/node-template/label/brewlet.sh/runtime.static,Value=available,PropagateAtLaunch=false"
```

`PropagateAtLaunch=false` is essential: these are Cluster Autoscaler simulation
inputs, not readiness labels on booting nodes. Do not add equivalent labels to
kubelet bootstrap or another real-node label source. Real nodes join with the
provider's pool label; only validated Brewlet provisioning publishes their
actual readiness/capabilities. A matching profile is:

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

The `RuntimeImage/static` must already be approved and its actual userland must
meet the declared platform/account contract. Other taints are not tolerated
implicitly. Provisioner tolerations are explicit; application Pods must not
tolerate the Brewlet startup fence, including via wildcard `Exists`.

### Karpenter: real-node template labels

Karpenter copies NodePool template labels to NodeClaims/Nodes. Therefore a
template that advertises Brewlet capabilities before post-registration
installation must also declare the startup fence. For example, with an
existing platform-reviewed `EC2NodeClass/runtime-nodes`:

```yaml
apiVersion: karpenter.sh/v1
kind: NodePool
metadata:
  name: brewlet-runtime
spec:
  template:
    metadata:
      labels:
        brewlet.sh/runtime: ready
        brewlet.sh/runtime.static: available
    spec:
      nodeClassRef:
        group: karpenter.k8s.aws
        kind: EC2NodeClass
        name: runtime-nodes
      startupTaints:
        - key: startup-taint.cluster-autoscaler.kubernetes.io/brewlet
          value: provisioning
          effect: NoSchedule
      requirements:
        - key: kubernetes.io/os
          operator: In
          values: [linux]
        - key: kubernetes.io/arch
          operator: In
          values: [arm64]
```

Use the corresponding NodeProfile with
`nodePool: {key: karpenter.sh/nodepool, names: [brewlet-runtime]}` and the same
inventory/toleration above. Choose architectures actually supplied by the
approved runtime source; the example does not assert availability in any
region or account. Karpenter ignores startup taints for provisioning decisions,
but the real scheduler still blocks the Pod until the authorized worker
validates the baseline and required roots and removes the taint.

OnDemand requires changing/removing the real per-ID label during first use,
failure and idle reclamation. Confirm that the provider/policy permits those
changes. A provider-pinned `available` label is unsupported: use mutable labels
or change both the profile and matching template to Eager.

### AKS: initialization and reimage requirements

AKS pool labels are real, provider-managed labels. AKS-managed Cluster
Autoscaler does not obtain this simulation contract from EKS-style VMSS tags.
For a provider-pinned Eager recipe, use
`brewlet.sh/runtime=ready` and `brewlet.sh/runtime.static=ready`, change the
profile to `install: Eager`, and select the actual pool with
`nodePool: {key: agentpool, names: [runtimepool]}`. An OnDemand per-ID pin is
not supported.

Ready labels alone are unsafe. Before allowing such labels onto an
unprovisioned node, the pool must arrange the removable Brewlet
**node initialization taint** at boot. Ordinary AKS `--node-taints` are pinned
and cannot serve as this removable fence.

The retained provider contract and current AKS documentation describe
`NodeInitializationTaintsPreview` registration under
`Microsoft.ContainerService`, with supported preview tooling/API. Initialization
taints are configured through a **managed-cluster-level** create/update request,
not an agent-pool-level request carrying `NodeInitializationTaints`; the latter
is rejected. Cluster CLI initialization options can affect every pool; a
pool-scoped configuration requires a reviewed managed-cluster ARM request
targeting that pool's profile. Verify provider/API support before changing a
real cluster; Brewlet does not perform feature registration or cloud updates.

Changing initialization settings alone is insufficient. Regenerate the
affected pool's node image and roll/reimage the intended nodes under the
platform's maintenance procedure, for example with a reviewed
`az aks nodepool upgrade --node-image-only` operation. Verify that newly
created/reimaged nodes actually arrive startup-tainted before accepting
templated readiness. Existing nodes and future scale-up replicas must not be
assumed initialized from a successful API update alone.

If these prerequisites cannot be met, do not template Brewlet ready labels and
do not advertise verified scale-from-zero; retain nonzero provisioned capacity
instead. During retirement/uninstall, removing a taint from one current Node
does not remove it from provider initialization settings. Correct stale pool
labels and initialization configuration explicitly; Brewlet cleanup never
authorizes releasing a fence over stale provider capabilities.

Provider references: [EKS Cluster Autoscaler](https://docs.aws.amazon.com/eks/latest/best-practices/cas.html),
[Karpenter NodePools](https://karpenter.sh/docs/concepts/nodepools/), and
[AKS node/initialization taints](https://learn.microsoft.com/en-us/azure/aks/use-node-taints).
The governing Brewlet contract remains
[specification §9.2](../specs/SPECIFICATION.md#92-startup-fencing-and-provider-managed-labels);
the 0.x capability labels are not restored.

## Validation boundaries

`make test` covers portable unit/contract tests and the optional Maven producer.
It is not proof of Linux mounts, real scheduling or provider behavior.
`integration-tests/fixtures` contains four application producers exercising
ordinary argv/dependency contracts. Host execution of a fixture proves only that
fixture works, not that Brewlet runs it.

Real-node acceptance needs an invocation-owned Linux/containerd 2 cluster with
cgroup v2 and runc, an invocation-owned registry, pinned approved runtime
images, and build tools for Java, Python, Node.js and .NET. Never run against a
shared current kubectl context. Pin `KUBECONFIG` and cluster/registry names; retain
failure diagnostics and remove only invocation-owned resources.

Every section 13.1 scenario needs its specified proof, including an actual
provider/admission interceptor for label refusals. CRI filesystem usage and
eviction need real conformance, not a file-location assertion. A skipped test,
fake client, synthetic local authorization, pre-existing runtime installation
or direct `docker run` is not successful Brewlet end-to-end coverage.

Deletion and uninstall must retain ownership, ledgers, finalizers and needed
RBAC until verified host cleanup, worker teardown, conditional claim release and
all missing-host obligations are resolved. Do not remove finalizers or rewrite
claims as a recovery shortcut.
