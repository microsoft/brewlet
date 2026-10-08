# Brewlet capability labels

This document is the public reference for Brewlet's capability-label contract.
The node provisioner publishes these labels after it has installed and validated
the runtime inventory selected by a [`NodeProfile`](SPECIFICATION.md#56-node-profiles-per-pool-preparation).
The pod admission webhook then injects matching required node affinity for each
explicit workload request.

## Contract v1

| Node label | Meaning | Producer | Workload match |
|---|---|---|---|
| `brewlet.sh/runtime=ready` | The Brewlet shim, at least one JDK, and the containerd runtime registration are ready on the node. | Brewlet provisioner | `In: ["ready"]`, through the `brewlet` `RuntimeClass` |
| `brewlet.sh/jdk.<distribution>-<feature>` | The exact JDK root is installed. | Brewlet provisioner | `Exists` |
| `brewlet.sh/jdk-feature.<feature>` | At least one JDK distribution for the feature version is installed. | Brewlet provisioner | `Exists` |
| `brewlet.sh/launcher.<name>` | The launcher is available. The provisioner always publishes `brewlet.sh/launcher.java`; configured launcher layers add more keys. | Brewlet provisioner | `Exists` for non-`java` requests |
| `brewlet.sh/appcds-regeneration` | The active `NodeProfile` authorizes node-side AppCDS regeneration and the root-owned host policy sentinel is installed. Published only while enabled. | Brewlet provisioner | `Exists` when regeneration is requested |
| `kubernetes.io/arch=<architecture>` | The kubelet-reported node architecture. Brewlet reuses this standard label rather than publishing an architecture label of its own. | Kubelet | `In: [...]` |

The dynamic JDK, launcher, and AppCDS labels are **boolean-presence labels**.
Brewlet currently writes the value `true`, but the value is not part of the
scheduling test. Consumers MUST match these keys with Kubernetes
`Operator: Exists` and MUST NOT require `=true`. A disabled AppCDS policy removes
the key rather than publishing a false value.

`brewlet.sh/runtime` is not a boolean-presence label: only the exact value
`ready` means that the runtime may accept workloads. `kubernetes.io/arch` also
uses value matching because a workload requests one or more specific
architectures.

Provisioning selects nodes with `NodeProfile.spec.nodePool` and requires managed
ownership. See the [provisioning contract](SPECIFICATION.md#51-activation).

## Token grammar

Capability keys are derived from the inventory declared by the platform team:

- `<distribution>` is the lowercase DNS-1123 label in
  `NodeProfile.spec.jdks[].distribution`: 1-48 characters, beginning and ending
  with an ASCII lowercase letter or digit, with lowercase letters, digits, and
  hyphens in between.
- `<feature>` is the canonical base-10 rendering of a positive 32-bit JDK
  feature version, without a sign or leading zeroes; for example, `17`, `21`, or
  `25`.
- `<distribution>-<feature>` is limited to 59 characters so that
  `jdk.<distribution>-<feature>` fits the 63-character Kubernetes label-name
  segment.
- `<name>` is a launcher identifier. To produce a valid capability label, names
  use lowercase DNS-1123 label syntax and are limited to 54 characters so that
  `launcher.<name>` fits the Kubernetes label-name segment. `java` is reserved
  for the OpenJDK launcher supplied by every configured JDK. Admission and
  reconciliation reject invalid or duplicate launcher names.
- `<architecture>` is a Kubernetes architecture token such as `amd64` or
  `arm64`.

For example, this profile:

```yaml
apiVersion: node.brewlet.sh/v1alpha1
kind: NodeProfile
metadata:
  name: batch
spec:
  nodePool:
    names: ["batch"]
    key: eks.amazonaws.com/nodegroup
  jdks:
    - distribution: temurin
      feature: 21
      source:
        image: docker.io/library/eclipse-temurin@sha256:85f00967bcc624fc19fa9c2cf124ea426a5363898e267141726f31f358c2e14b
        javaHome: /opt/java/openjdk
    - distribution: microsoft
      feature: 25
      source:
        image: mcr.microsoft.com/openjdk/jdk@sha256:bfde2ed613f4c67c112d1592452575d3a1dc9ce5f7d75821bb7752aa786fa575
        javaHome: /usr/lib/jvm/msopenjdk-25
  launchers:
    - name: jaz
      source:
        image: mcr.microsoft.com/openjdk/jdk@sha256:bfde2ed613f4c67c112d1592452575d3a1dc9ce5f7d75821bb7752aa786fa575
        path: /usr/bin/jaz
  appCDS:
    regenerationEnabled: true
```

causes each successfully provisioned `batch` node to publish:

```text
brewlet.sh/runtime=ready
brewlet.sh/jdk.temurin-21=true
brewlet.sh/jdk-feature.21=true
brewlet.sh/jdk.microsoft-25=true
brewlet.sh/jdk-feature.25=true
brewlet.sh/launcher.java=true
brewlet.sh/launcher.jaz=true
brewlet.sh/appcds-regeneration=true
```

The provider's kubelet separately publishes `kubernetes.io/arch`.

## Admission-injected affinity

`JavaApplication.spec.jvm.version`, `.distribution`, `.launcher`, and
`.cds.regenerate` become pod annotations before pod admission. Equivalent raw
pods can set the annotations directly. For a distribution-agnostic JDK 21
request with the `jaz` launcher and AppCDS regeneration, the Brewlet webhook
injects the following requirements:

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

The `brewlet` `RuntimeClass` additionally contributes:

```yaml
nodeSelector:
  brewlet.sh/runtime: ready
```

An exact `temurin-21` request uses
`brewlet.sh/jdk.temurin-21 Exists` instead. A non-portable artifact requesting
`amd64` or `arm64` adds `kubernetes.io/arch In [...]`. A missing JDK request adds
no JDK affinity, and the implicit `java` launcher adds no launcher affinity
because every ready Brewlet node provides it. A pod that does not request
regeneration adds no AppCDS affinity.

When a pod already has required node affinity, Brewlet appends its requirements
to every existing node selector term. Expressions within a term remain ANDed
and terms remain ORed, so Brewlet preserves the pod author's existing selection
logic while requiring the requested runtime capabilities in every alternative.

Admission verifies explicit requests against the runtime inventory: the
current ready fleet plus every eligible `NodeProfile` (not deleting, valid under
the operator's admission policy, and free of pool conflicts). A single
candidate, a ready node or a profile, must jointly provide the requested JDK,
launcher and AppCDS authorization; otherwise the pod is denied with
`NoCompatibleJDK`, `NoCompatibleLauncher`, or `AppCDSRegenerationDisabled`.

When only a `NodeProfile` satisfies the request, for example a pool that is
currently scaled to zero, the pod is admitted with the affinity above plus an
admission warning, and stays `Pending` until an autoscaler adds a node that the
profile provisions. Architecture is not checked against profiles (it comes from
the VM size, not the profile) and never denies: a request for an architecture
with no ready node is admitted with a warning and resolved by
`kubernetes.io/arch` affinity. `NoCompatibleArch` is reserved and no longer
emitted.

The AppCDS label is a scheduling hint, not the authorization boundary. The shim
requires `/opt/brewlet/policy/appcds-regeneration-enabled` independently, so a
stale or forged label cannot grant cache-write access.

## Autoscaler recipes

### Cluster Autoscaler with a NodeProfile-managed node group

Cluster Autoscaler must see the labels that a future node will eventually
publish when it simulates scheduling. Configure the group template with
synthetic labels for `runtime=ready` and every capability installed by the
matching `NodeProfile`. Because admission counts the profile's declared
inventory, the group may scale to zero where the provider keeps template labels
off real nodes (not on AKS-managed Cluster Autoscaler; see below).

#### AKS

The AKS-managed Cluster Autoscaler builds node-group templates only from the
agent pool spec (`--labels`, `--node-taints` and the VM size). It does not read
`k8s.io_cluster-autoscaler_node-template_label_*` VMSS tags. AKS also pins pool
labels and taints: its node admission webhook refuses to remove or change them
on a Node. A pool label `brewlet.sh/runtime=ready` alone would advertise
readiness before installation and could never be withdrawn, so an AKS pool MUST
NOT carry Brewlet labels unless it also carries the Brewlet
[startup taint](#startup-taint) as a node initialization taint (preview,
removable from a Node), and MUST NOT carry Brewlet taints in `--node-taints`.
AKS accepts `nodeInitializationTaints` only on managed-cluster requests, not
on agent-pool requests (`NodeInitializationTaintsFeatureNotSupported`), after
the subscription registers `Microsoft.ContainerService/NodeInitializationTaintsPreview`.
A managed-cluster request that changes only one pool's profile scopes the taint
to that pool. New nodes carry it only after the pool's node image
is regenerated (`az aks nodepool upgrade --node-image-only`). With that taint and
the profile's exact label set in `--labels`, the managed Cluster Autoscaler
scales the pool from zero. Without them, keep `--min-count 1`: once a pool has a
provisioned node, Cluster Autoscaler templates further scale-out from that real
node's provisioner-published labels. Node Auto Provisioning (see
[Karpenter](#karpenter)) with the startup taint is the other AKS
scale-from-zero path. `kubernetes.io/arch` comes from the VM size, so one profile
can target `amd64` and `arm64` pools together
(`nodePool: {key: agentpool, names: [javax64, javaarm]}`).

#### EKS

For an EKS managed node group backed by Auto Scaling group `$ASG`, the Cluster
Autoscaler tag convention is:

```bash
aws autoscaling create-or-update-tags --tags \
  "ResourceId=$ASG,ResourceType=auto-scaling-group,Key=k8s.io/cluster-autoscaler/node-template/label/brewlet.sh/runtime,Value=ready,PropagateAtLaunch=false" \
  "ResourceId=$ASG,ResourceType=auto-scaling-group,Key=k8s.io/cluster-autoscaler/node-template/label/brewlet.sh/jdk.temurin-21,Value=true,PropagateAtLaunch=false" \
  "ResourceId=$ASG,ResourceType=auto-scaling-group,Key=k8s.io/cluster-autoscaler/node-template/label/brewlet.sh/jdk-feature.21,Value=true,PropagateAtLaunch=false" \
  "ResourceId=$ASG,ResourceType=auto-scaling-group,Key=k8s.io/cluster-autoscaler/node-template/label/brewlet.sh/launcher.java,Value=true,PropagateAtLaunch=false" \
  "ResourceId=$ASG,ResourceType=auto-scaling-group,Key=k8s.io/cluster-autoscaler/node-template/label/brewlet.sh/launcher.jaz,Value=true,PropagateAtLaunch=false" \
  "ResourceId=$ASG,ResourceType=auto-scaling-group,Key=k8s.io/cluster-autoscaler/node-template/label/brewlet.sh/appcds-regeneration,Value=true,PropagateAtLaunch=false"
```

These tags are scheduling hints to Cluster Autoscaler; with
`PropagateAtLaunch=false` they do not claim that an unprovisioned node is ready.
The EKS node joins with its normal
`eks.amazonaws.com/nodegroup=<group-name>` label, the matching `NodeProfile`
places a provisioner pod on it, and only that provisioner publishes the real
runtime and capability labels after installation succeeds.

Wherever template labels are used, they MUST exactly match the profile inventory. A template that
advertises more capabilities than the profile installs can cause unnecessary
scale-ups, although the Kubernetes scheduler still waits for the real labels
before placing the workload.

### Karpenter

Karpenter copies `NodePool.spec.template.metadata.labels` onto real NodeClaims
and Nodes; unlike Cluster Autoscaler's synthetic template tags, they are not
simulation-only hints. Do not put `brewlet.sh/runtime=ready` or Brewlet
capability labels on a Karpenter `NodePool` whose nodes still require
post-registration `NodeProfile` provisioning unless the pool also declares the
Brewlet startup taint (below). Without it there is a window in which the
scheduler can place a Brewlet pod before the runtime exists.

`NodeProfile` can provision nodes that Karpenter creates for other workloads by
selecting the Karpenter pool:

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

This pool-targeting arrangement ensures every created node receives the declared
inventory, but it is not a capability-driven Karpenter autoscaling recipe.
Karpenter cannot infer labels that another DaemonSet will add after registration,
so a pod carrying Brewlet's injected affinity cannot select this unlabeled
`NodePool`.

#### Startup taint

`startup-taint.cluster-autoscaler.kubernetes.io/brewlet` is the Brewlet startup
taint. Cluster Autoscaler and Karpenter (`NodePool.spec.template.spec.startupTaints`)
ignore it when they simulate a future node, so a pool MAY template the matching
profile's complete label set together with this `NoSchedule` taint. Brewlet
treats a node carrying it as not ready, whatever its labels say. The operator
claims such a node even though `brewlet.sh/runtime` is already present, as
long as no Brewlet profile advertisement is on it. The provisioner withdraws
the templated labels, installs and validates the runtime, publishes the real
labels, and only then removes the taint. The `NodeProfile` MUST list a
toleration for the taint, because provisioner tolerations are opt-in. The
platform MUST allow the provisioner to add and remove the taint (AKS
`--node-taints` cannot be removed; AKS node initialization taints can). The
templated labels MUST exactly match the profile inventory.

Some platforms pin templated labels so they can't be removed from a Node (AKS
pool `--labels`). When the provisioner or operator must withdraw readiness and
a label removal is refused this way, it leaves the pinned labels in place and
applies the startup taint instead, which alone makes the node not ready. A
pinned capability label that the profile inventory doesn't provide fails
provisioning with `node-advertisement-failed`, because it can never be made
true.

#### Pre-baked images

Capability-driven Karpenter provisioning without a startup taint requires an
immutable node image or bootstrap process that installs and validates Brewlet
before kubelet registers the Node. Only in that model may the `NodePool`
template truthfully include the complete label set:

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
        brewlet.sh/launcher.jaz: "true"
        brewlet.sh/appcds-regeneration: "true"
    spec:
      requirements:
        - key: kubernetes.io/arch
          operator: In
          values: ["amd64", "arm64"]
```

The image inventory and labels MUST move together. Publishing `runtime=ready`
from a Karpenter template while relying on the Brewlet provisioner to install
the runtime after node registration is unsupported. The current
`NodeProfile`-managed provisioner is therefore suitable for Karpenter nodes
created for other demand, but not as the bootstrap mechanism for a
capability-driven Karpenter `NodePool`. If the template publishes
`brewlet.sh/appcds-regeneration`, that bootstrap MUST also install the root-owned
AppCDS policy sentinel; the label alone is never authorization.

## Compatibility and versioning

This public scheduling contract is an explicit exception to the
[pre-GA compatibility policy](../docs/compatibility.md). The versioning and
dual-publication migration guarantees below remain in force; they do not
guarantee whole-installation in-place upgrades between Brewlet releases.

The labels in the contract-v1 table are public scheduling API:

- Brewlet may add new capability-key families or publish additional keys on a
  node without changing contract v1.
- Brewlet may add support for new distributions, feature versions, launchers,
  and architectures when their tokens follow the grammar above.
- Brewlet MUST NOT rename or remove a listed key family, change the meaning of
  `runtime=ready`, or change an `Exists` capability into value-sensitive
  matching within contract v1.
- A breaking change requires a new major capability-label contract, release
  notes, and a migration period in which old and new keys are published
  together so autoscaler templates and workload policies can move safely.
- Kubernetes-owned semantics for `kubernetes.io/arch` remain governed by
  Kubernetes. Brewlet's guarantee is that architecture requests continue to use
  that standard key rather than a Brewlet-specific replacement.

Node inventory annotations such as `brewlet.sh/jdks` and
`brewlet.sh/launchers` are used by Brewlet admission for current-fleet
validation. They are not substitutes for the scheduling labels and are not
part of this public autoscaler-template contract.

The operator's `brewlet.sh/owner-uid` and `brewlet.sh/owner-node-uid` labels and
`brewlet.sh/owner-name` annotation are internal node-ownership fences, not
capability labels. They MUST NOT be copied into node templates or images.
Their lifecycle and safe deprovisioning requirements are defined in
[SPECIFICATION §5.6](SPECIFICATION.md#56-node-profiles-per-pool-preparation).
