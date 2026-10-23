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
The standalone operator requires an explicit `--worker-image`; the chart supplies
the matching configured image rather than relying on an unqualified default.

## Current Helm values

These are the current chart keys, not Brewlet 0.x's values schema. Source-chart defaults
name candidate coordinates, not a promise of published images. Release packages
replace these tags with verified immutable image digests.

| Key | Default | Meaning |
| --- | --- | --- |
| `operator.image` | `ghcr.io/microsoft/brewlet-operator:1.0.0` | Operator and uninstall-coordinator image. |
| `admission.image` | `ghcr.io/microsoft/brewlet-admission:1.0.0` | Admission server image; source-built control image also contains this binary. |
| `provisioner.image` | `ghcr.io/microsoft/brewlet-node-provisioner:1.0.0` | Matching privileged node worker and bundled shim. |
| `provisioner.profiles` | `[]` | Named `{name, spec}` profiles; chart entries require nonempty pool names. |
| `provisioner.gc.grace` | `1h` | Positive reference-drain grace for collection. |
| `provisioner.gc.sweepInterval` | `1m` | Positive collection interval. |
| `security.allowedSourceMirrorHosts` | `[]` | Exact permitted runtime-source mirror authorities. |
| `admission.enabled` | `true` | Deploy admission; disabling it does not disable shim enforcement. |
| `admission.nodeProfileFailurePolicy` | `Fail` | Failure policy for inventory/profile validation webhooks. |
| `operator.rotationRestarts.enabled` | `false` | Enable optional native-controller restarts, the corresponding RBAC and the restart `ValidatingAdmissionPolicy`. |
| `operator.rotationRestarts.maxConcurrent` | `5` | Concurrent restart coordination bound. |
| `operator.rotationRestarts.nodeProgressDeadline` | `10m` | Node-rotation progress deadline. |
| `operator.rotationRestarts.workloadProgressDeadline` | `10m` | Workload restart progress deadline. |
| `operator.rotationRestarts.namespaces` | `[]` | Optional restart namespace allowlist; when set, patch rights use a Role per namespace instead of the ClusterRole. Namespaces must exist and cannot be the release or a protected namespace. |
| `operator.rotationRestarts.protectedNamespaces` | `[kube-system, kube-public, kube-node-lease]` | Namespaces the restart admission policy always refuses. |
| `operator.rotationRestarts.requireAdmissionPolicy` | `true` | Fail installation with restarts enabled when `ValidatingAdmissionPolicy` v1 (Kubernetes 1.30+) is unavailable; `false` accepts RBAC as the only bound. |
| `uninstall.timeoutSeconds` | `240` | Cleanup coordinator timeout, integer 1 through 86400. |
| `metrics.enabled` | `false` | Enable operator and node Prometheus endpoints. |
| `metrics.nodePort` | `9090` | Node metrics listener/service port; this is not a NodePort service type. |

The Pod mutating webhook has its own chart-defined fail-open policy. Inventory
and retirement validation have different policies; there is no generic
`admission.failurePolicy` value in this chart.

## RuntimeImage

`node.brewlet.sh/v1alpha1`, cluster-scoped:

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

Every application/init/sidecar/ephemeral container requires
`securityContext.readOnlyRootFilesystem: true`. Omission is defaulted by
admission; explicit false is rejected. The shim also rejects a writable OCI
root if admission was bypassed or unavailable.
Workload volume/device mounts outside `/app/` require the platform's immutable
`RuntimeImage.spec.volumeMountPaths` declaration. See
[runtime mount policy](runtime-images.md#declare-data-and-configuration-mount-locations).

`brewlet.sh/restart-on-rotation: "true"` belongs on the workload controller's
metadata, not its Pod template, and requires cluster restart enablement.
`brewlet.sh/rotated-to-generation` is a strict JSON inventory-epoch/minimum
barrier, not a free-form version string. See [reference](reference.md).

### RuntimeClass overhead

The raw RuntimeClass manifest and the chart defaults reserve `50m` CPU and
`64Mi` memory per Pod for shim/staging overhead. These are declared scheduling
reservations, not measured consumption or language runtime heap reservations.

| Helm value | Default | Meaning |
| --- | --- | --- |
| `runtimeClass.overhead.cpu` | `"50m"` | Nonnegative Kubernetes CPU quantity string per Pod. |
| `runtimeClass.overhead.memory` | `"64Mi"` | Nonnegative Kubernetes memory quantity string per Pod. |

Set these at installation according to measurements of your deployment, for
example by supplying this values file to `helm install ... -f overhead.yaml`:

```yaml
runtimeClass:
  overhead:
    cpu: "100m"
    memory: "96Mi"
```

These example overrides are not sizing recommendations. The chart checks
quantity-string syntax and rejects invalid types, negative or empty values;
Kubernetes remains authoritative for resource validation. Use `--set-string`
when passing plain numeric quantities on the command line. Raw-manifest users
edit `overhead.podFixed` explicitly; the shipped raw defaults remain unchanged.

[Pod overhead](https://kubernetes.io/docs/concepts/scheduling-eviction/pod-overhead/)
affects scheduling and quota accounting and is recorded when Pods are admitted.
At the defaults, 1,000 Pods declare an additional 50 CPU cores and 62.5 GiB of
memory reservation; that is not an observation of actual resource use.
Do not remove the reservation or claim density improvements without evidence.
Choose these defaults at installation. Later RuntimeClass changes are policy
for newly admitted Pods, not live resource tuning: existing Pods retain their
admitted overhead values until replaced.

## Registry and environment precedence

Producer credentials, kubelet application pulls, node runtime-source pulls and
application outbound TLS are separate configurations. See [trust](registry-trust.md).
Runtime Env supplies defaults; final CRI Env overrides ordinary names. PATH
always remains runtime-owned. Initial effective state is retained for Exec,
probes and hooks. See [launch reference](reference.md#launch-descriptor).
