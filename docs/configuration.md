# Configuration reference

Each layer of Brewlet configuration has one source of truth. Change the source,
not the resources Brewlet generates from it.

| Layer | Configures |
| --- | --- |
| Helm values | Control-plane options and optional `NodeProfile`s. |
| `RuntimeImage` | Approved runtime source, default user and compatibility metadata. |
| `NodeProfile` | Node pools, runtime offerings, installation policy, mirrors and worker rollout. |
| Pod or workload controller | Runtime ID, application image, resources and rollout policy. |

Manage these objects through Helm or GitOps. Do not edit the worker DaemonSets
that the operator generates; the operator reconciles them.

## Helm values

The published chart pins each component image to the digest of the release.
The tags below show which release those digests belong to.

| Key | Default | Meaning |
| --- | --- | --- |
| `operator.image` | `ghcr.io/microsoft/brewlet-operator:0.8.1` | Operator and uninstall-coordinator image. |
| `admission.image` | `ghcr.io/microsoft/brewlet-admission:0.8.1` | Admission webhook image. |
| `provisioner.image` | `ghcr.io/microsoft/brewlet-node-provisioner:0.8.1` | Privileged node worker image, including the shim. |
| `provisioner.profiles` | `[]` | `NodeProfile`s to create, as `{name, spec}` entries. Each must set `spec.nodePool.names`. |
| `provisioner.gc.grace` | `1h` | How long an unused runtime generation is kept before collection. |
| `provisioner.gc.sweepInterval` | `1m` | How often unused generations are collected. |
| `security.allowedSourceMirrorHosts` | `[]` | Registry hosts allowed as runtime-source mirrors (exact match). |
| `admission.enabled` | `true` | Deploy admission; disabling it does not disable shim enforcement. |
| `admission.nodeProfileFailurePolicy` | `Fail` | Failure policy for the `RuntimeImage` and `NodeProfile` validation webhooks. |
| `operator.rotationRestarts.enabled` | `false` | Let the operator restart opted-in workloads after a runtime rotation. Also installs the required RBAC and `ValidatingAdmissionPolicy`. |
| `operator.rotationRestarts.maxConcurrent` | `5` | Maximum workloads restarted at once. |
| `operator.rotationRestarts.nodeProgressDeadline` | `10m` | Node-rotation progress deadline. |
| `operator.rotationRestarts.workloadProgressDeadline` | `10m` | Workload restart progress deadline. |
| `operator.rotationRestarts.namespaces` | `[]` | Optional restart namespace allowlist; when set, patch rights use a Role per namespace instead of the ClusterRole. Namespaces must exist and cannot be the release or a protected namespace. |
| `operator.rotationRestarts.protectedNamespaces` | `[kube-system, kube-public, kube-node-lease]` | Namespaces the restart admission policy always refuses. |
| `operator.rotationRestarts.requireAdmissionPolicy` | `true` | Fail installation with restarts enabled when `ValidatingAdmissionPolicy` v1 (Kubernetes 1.30+) is unavailable; `false` accepts RBAC as the only bound. |
| `uninstall.timeoutSeconds` | `240` | Uninstall cleanup timeout, a whole number from 1 to 86400. |
| `metrics.enabled` | `false` | Enable operator and node Prometheus endpoints. |
| `metrics.nodePort` | `9090` | Node metrics listener/service port; this is not a NodePort service type. |

The Pod mutating webhook always fails open (`Ignore`), and the
`NodeRetirementEvidence` webhook always fails closed (`Fail`). There is no
generic `admission.failurePolicy` value.

## RuntimeImage

`node.brewlet.sh/v1alpha1`, cluster-scoped:

| Field | Meaning |
| --- | --- |
| `spec.source.image` | Digest-pinned OCI image reference. Tags are rejected. |
| `spec.user.uid`, `spec.user.gid` | Default non-root numeric user and group. Immutable; `0` and the reserved maximum are rejected. |
| `spec.description` | Human-readable description. |
| `spec.compatibility.ecosystem`, `spec.compatibility.abi` | Informational compatibility metadata. Brewlet does not resolve versions from it. |
| `spec.volumeMountPaths` | Immutable list of paths outside `/app/` where workloads may mount volumes. See [mount locations](runtime-images.md#declare-data-and-configuration-mount-locations). |

The object's Kubernetes UID identifies the inventory epoch. `status.generation`
is the runtime generation Brewlet allocated. It is not the same as
`metadata.generation`.

## NodeProfile

| Field | Meaning |
| --- | --- |
| `spec.nodePool.key`, `spec.nodePool.names` | Node label key and the pool names it selects. Name pools explicitly. |
| `spec.nodePool.includeControlPlane` | Opt in to control-plane nodes. Defaults to `false`. |
| `spec.runtimeImages[].id` | Approved runtime ID offered on this pool. |
| `spec.runtimeImages[].install` | `Eager` (default) or `OnDemand`. |
| `spec.runtimeImages[].idleTTL` | `OnDemand` only. Uninstall the runtime after it is unused for this long. |
| `spec.tolerations` | Tolerations for the worker. None are added by default. |
| `spec.registry.mirrors` | Source-to-mirror registry mapping. Mirrors must be in `security.allowedSourceMirrorHosts`. |
| `spec.rollout.maxUnavailable` | Maximum nodes updated at once during a worker rollout. |
| `spec.rollout.containerdRestart` | `validated` (default) or `none`. |

With `none`, Brewlet never restarts containerd, so the host's containerd
configuration must already be correct. Brewlet cannot then repair missing
layer-retention or runtime-handler settings, and cleanup of earlier changes is
still required.

## Pod and controller settings

Set `runtimeClassName: brewlet` and the Pod annotation
`brewlet.sh/runtime-image: <id>`. All containers in the Pod use the same
runtime ID. Brewlet does not add architecture constraints; set node affinity
yourself (see [multi-architecture fleets](multi-arch.md)).

Every container (application, init, sidecar and ephemeral) must use
`securityContext.readOnlyRootFilesystem: true`. Admission sets it when it is
omitted and rejects an explicit `false`. The shim also refuses a writable root
filesystem if admission was bypassed.

Volume and device mounts outside `/app/` must be listed in the runtime's
`RuntimeImage.spec.volumeMountPaths`. See
[mount locations](runtime-images.md#declare-data-and-configuration-mount-locations).

To have the operator restart a workload after a runtime rotation, set
`brewlet.sh/restart-on-rotation: "true"` on the workload controller's metadata
(not on its Pod template) and enable `operator.rotationRestarts.enabled`.
`brewlet.sh/rotated-to-generation` is a JSON minimum-generation barrier, not a
free-form version string. See [public workload annotations](reference.md#public-workload-annotations).

### RuntimeClass overhead

The `brewlet` RuntimeClass reserves `50m` CPU and `64Mi` memory per Pod for
shim and staging overhead. These are scheduling reservations, not measured
usage, and they do not include the language runtime's own memory.

| Helm value | Default | Meaning |
| --- | --- | --- |
| `runtimeClass.overhead.cpu` | `"50m"` | Nonnegative Kubernetes CPU quantity string per Pod. |
| `runtimeClass.overhead.memory` | `"64Mi"` | Nonnegative Kubernetes memory quantity string per Pod. |

Set these at installation, based on measurements of your own workloads. For
example, pass this file with `--values overhead.yaml`:

```yaml
runtimeClass:
  overhead:
    cpu: "100m"
    memory: "96Mi"
```

These values are an example, not a sizing recommendation. The chart rejects
empty, negative or non-string values. When passing a plain number on the
command line, use `--set-string`.

[Pod overhead](https://kubernetes.io/docs/concepts/scheduling-eviction/pod-overhead/)
affects scheduling and quota accounting and is recorded when Pods are admitted.
At the defaults, 1,000 Pods reserve an extra 50 CPU cores and 62.5 GiB of
memory. Do not remove the reservation without measurements that justify it.
Changing the overhead affects only newly admitted Pods; existing Pods keep
their values until they are replaced.

## Registry and environment precedence

Credentials for pushing artifacts, kubelet pulls of application images, node
pulls of runtime sources, and application outbound TLS are configured
separately. See [registry and certificate trust](registry-trust.md).

The runtime image's environment provides defaults. Environment variables set in
the container spec override them, except `PATH`, which the runtime always
controls. Exec sessions, probes and hooks use the same environment as the main
process. See [launch descriptor](reference.md#launch-descriptor).
