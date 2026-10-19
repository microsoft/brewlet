# Configuration reference

Configure the source of truth, not its generated child resources.

```text
Helm values -> control-plane options and optional NodeProfiles
RuntimeImage -> approved source, identity and inventory epoch
NodeProfile -> offering pools, installation policy, mirrors and worker authority
Pod/controller -> runtime ID, application image, resources and rollout policy
Fenced worker -> trusted local roots, authorization, leases and observations
```

Use Helm/GitOps source changes for managed objects. Do not mix manual edits to
worker DaemonSets with operator reconciliation.

## Current Helm values

These are the current chart keys, not main's values schema. Image defaults are
development names, not a promise of published images.

| Key | Default | Meaning |
| --- | --- | --- |
| `operator.image` | `brewlet-operator:2.0` | Operator and uninstall-coordinator image. |
| `admission.image` | `brewlet-admission:2.0` | Admission server image; source-built control image also contains this binary. |
| `provisioner.image` | `brewlet-node:2.0` | Matching privileged node worker and bundled shim. |
| `provisioner.profiles` | `[]` | Named `{name, spec}` profiles; chart entries require nonempty pool names. |
| `provisioner.gc.grace` | `1h` | Positive reference-drain grace for collection. |
| `provisioner.gc.sweepInterval` | `1m` | Positive collection interval. |
| `security.allowedSourceMirrorHosts` | `[]` | Exact permitted runtime-source mirror authorities. |
| `admission.enabled` | `true` | Deploy admission; disabling it does not disable shim enforcement. |
| `admission.nodeProfileFailurePolicy` | `Fail` | Failure policy for inventory/profile validation webhooks. |
| `operator.rotationRestarts.enabled` | `false` | Enable optional native-controller restarts and corresponding RBAC. |
| `operator.rotationRestarts.maxConcurrent` | `5` | Concurrent restart coordination bound. |
| `operator.rotationRestarts.nodeProgressDeadline` | `10m` | Node-rotation progress deadline. |
| `operator.rotationRestarts.workloadProgressDeadline` | `10m` | Workload restart progress deadline. |
| `uninstall.timeoutSeconds` | `240` | Cleanup coordinator timeout, integer 1 through 86400. |
| `metrics.enabled` | `false` | Enable operator and node Prometheus endpoints. |
| `metrics.nodePort` | `9090` | Node metrics listener/service port; this is not a NodePort service type. |

The Pod mutating webhook has its own chart-defined fail-open policy. Inventory
and retirement validation have different policies; there is no generic
`admission.failurePolicy` value in this chart.

## RuntimeImage

`node.brewlet.sh/v2alpha1`, cluster-scoped:

| Field | Meaning |
| --- | --- |
| `spec.source.image` | Canonical digest-pinned OCI source, not a mutable tag. |
| `spec.user.uid`, `spec.user.gid` | Immutable usable non-root numeric defaults; zero and the reserved maximum are rejected. |
| `spec.description` | Human-readable information. |
| `spec.compatibility.ecosystem`, `spec.compatibility.abi` | Declared compatibility metadata, not executable discovery or a version solver. |

The Kubernetes UID is the inventory epoch. `status.generation` is Brewlet's
allocated runtime generation; it is not interchangeable with the Kubernetes
resource's `metadata.generation`.

## NodeProfile

| Field | Meaning |
| --- | --- |
| `spec.nodePool.key`, `names` | Pool label and selected names. Explicit named pools are recommended. |
| `spec.nodePool.includeControlPlane` | Explicit opt-in, normally false. |
| `spec.runtimeImages[].id` | Approved runtime ID offered on this pool. |
| `spec.runtimeImages[].install` | `Eager` by default, or `OnDemand`. |
| `spec.runtimeImages[].idleTTL` | Optional idle retention for OnDemand only. |
| `spec.tolerations` | Explicit worker tolerations; no blanket default. |
| `spec.registry.mirrors` | Source-to-mirror mapping under the shared allowlist policy. |
| `spec.rollout.maxUnavailable` | Bound worker rollout availability. |
| `spec.rollout.containerdRestart` | `validated` by default, or `none`. |

`none` requires operator-provided, already-correct host configuration. It cannot
repair missing packed-layer retention or handler configuration, and does not
erase an earlier activation transaction's cleanup obligation.

## Pod and controller settings

Use `runtimeClassName: brewlet` and the Pod annotation
`brewlet.sh/runtime-image: <id>`. All application containers use the same ID.
Native architecture constraints remain explicit.

`brewlet.sh/restart-on-rotation: "true"` belongs on the workload controller's
metadata, not its Pod template, and requires cluster restart enablement.
`brewlet.sh/rotated-to-generation` is a strict JSON inventory-epoch/minimum
barrier, not a free-form version string. See [reference](reference.md).

The raw and chart RuntimeClass reserve `50m` CPU and `64Mi` memory per Pod for
shim/staging overhead. These are not language runtime heap reservations.

## Registry and environment precedence

Producer credentials, kubelet application pulls, node runtime-source pulls and
application outbound TLS are separate configurations. See [trust](registry-trust.md).
Runtime Env supplies defaults; final CRI Env overrides ordinary names. PATH
always remains runtime-owned. Initial effective state is retained for Exec,
probes and hooks. See [launch reference](reference.md#launch-descriptor).
