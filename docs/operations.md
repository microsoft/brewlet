# Rotation, security and cleanup

## Runtime identity and authorization

An ID is a compatibility promise: preserve the runtime line, ABI, approved
numeric defaults, documented account/NSS behavior, PATH layout and tools across
digest rotations. Use a new ID for an
incompatible change or a separately governed held source.

The Kubernetes resource UID is the inventory epoch. Deleting and recreating
the same name cannot authorize old roots or old generation barriers.
Generations are monotonically allocated within an epoch.

Installed roots and metadata are immutable under
`/opt/brewlet/runtimes/<id>/<inventory-uid>/<generation>/`.
A relative `current` pointer publishes the validated root/metadata unit.
Separate local authorizations expire after five minutes by default and are
renewed only after fresh approval/ownership checks.

## Rotate without rebuilding application bytes

Update the approved source digest while retaining the same compatible ID.
The node validates and publishes the new generation. New containers resolve
current; existing tasks keep their durable leases. Runtime convergence is not
proof that every old application process restarted.

Optional restarts require both chart enablement and a workload-object opt-in:

```yaml
# On the Deployment/StatefulSet/DaemonSet metadata, not its Pod template:
metadata:
  annotations:
    brewlet.sh/restart-on-rotation: "true"
```

The coordinator installs an inventory-epoch/minimum-generation barrier in Pod
templates. It does not delete Pods or change update strategies. Defaults are
five concurrent workloads and a ten-minute workload progress deadline.
OnDelete reports not-applicable; paused/partitioned/cold-install/excluded-node
cases can remain deferred. A template patch or timeout is not remediation.

A workload is restarted when one of its running Pods appears in a retained
older-generation lease binding, or started before rotation completion and has
not been proven current by a fresh, complete observation of its node. Missing
or stale node reports fall back to restarting.

The annotation is the workload owner's consent; platform operators need no
patch rights in application namespaces. The chart pairs the operator's patch
RBAC with a fail-closed `ValidatingAdmissionPolicy` that only lets the operator
set the barrier annotation on opted-in Brewlet workloads. Image, command,
replica, label and opt-in changes, creates and deletes, workloads without the
annotation, and `kube-system`, `kube-public` and `kube-node-lease` are all
denied. Kubernetes 1.30 or later is required unless
`operator.rotationRestarts.requireAdmissionPolicy=false` explicitly accepts
RBAC as the only bound. To limit restarts further, list the namespaces; patch
rights then come from a Role in each listed namespace instead of a ClusterRole:

```yaml
operator:
  rotationRestarts:
    enabled: true
    namespaces: [payments, storefront]
```

## Observe actual bindings

Use native resources, runtime status and node metrics rather than the removed
Java-specific CLI:

```sh
kubectl get runtimeimages.node.brewlet.sh
kubectl get nodeprofiles.node.brewlet.sh
kubectl get nodes -L brewlet.sh/runtime
kubectl rollout status deployment/api --namespace apps
```

### Find workloads still on an older generation

`status.taskObservations` on each `RuntimeImage` lists the actual container
leases that are older than the approved generation or belong to a previous
inventory epoch, with Pod namespace, name and UID. It needs no metrics stack,
and read access to one cluster-scoped resource replaces checking every node:

```sh
kubectl get runtimeimage python-3.12 \
  -o jsonpath='{.status.conditions[?(@.type=="OldGenerationTasks")].message}{"\n"}'
kubectl get runtimeimage python-3.12 -o json | jq -r '
  .status.taskObservations[]
  | "\(.nodeName)\tcurrent=\(.current)\t\(.observedAt)" as $node
  | .bindings[]?
  | [$node, .podNamespace, .podName, .containerName, (.generation|tostring)]
  | @tsv' | sort -u
```

An observation with `current=false` means that node has stopped reporting a
fresh, complete snapshot. Its last bindings are retained rather than assumed
gone, so investigate the node before treating its Pods as patched. These
observations are diagnostics, not cleanup or revocation authority.

Node metrics are optional (`metrics.enabled=true`). Important families include
`brewlet_container_runtime_image_info`, `brewlet_runtime_image_generations`,
`brewlet_runtime_image_install_requests_total`,
`brewlet_runtime_image_install_duration_seconds` and
`brewlet_shared_storage_bytes`. Runtime bindings include actual Pod/container
identity, inventory epoch, generation and digest.

## Security boundary

Brewlet retains runc isolation and a privileged trusted provisioner. It is not
gVisor, Kata or an additional hostile-tenant boundary. Every application process
is non-root, has non-root groups, no capabilities and no privilege escalation.
Runtime-owned PATH and protected initial state apply to Exec/probes/hooks too.
PATH and account files may be absent; this does not relax numeric non-root
identity or permit CRI's default executable-search environment.

Application/dependency entries are confined to `/app`. This prevents replacement
of runtime paths, not application-owned dynamic-library lookup or explicit
volume contents. Runtime rotation cannot repair vulnerable libraries packaged
with the application. Never package secrets into world-readable app layers.
Runtime `/app` files, directories and symlinks are hidden behind the private
application boundary, without mutating approved roots. Lack of support for its
opaque overlay metadata fails closed rather than merging runtime bytes.

## Cleanup and revocation

Revocation stops new starts while existing tasks keep their roots. An expiring
authorization bounds disconnected-node approval; labels alone are not authority.

Normal cleanup must complete each original inventory obligation, publish and
read back complete per-ID receipts, reverse owned host configuration, stop
workers and only then release claims/finalizers. A failed/incomplete receipt
retains its original revoked authorization for a later valid retry.

Use normal reviewed resource deletion and Helm uninstall. Uninstall waits for
production cleanup and may fail while obligations remain. Do not force-delete
finalizers, claims, runtime roots, leases or startup fences to make it succeed.
Helm/GitOps-managed definitions should be changed at their source of truth.

The persistent cache under `/var/lib/brewlet/layers` is collected by the fenced
provisioner after positive grace and complete reference checks. Unknown task,
Exec or cross-namespace mount evidence blocks deletion, even when a Pod list
looks empty.

Missing/recreated hosts retain original-identity obligations. The explicit
retirement-evidence process requires narrowly scoped recovery authority; a
replacement node is not permission to erase another host's history.

See the [implementation and operational details](https://github.com/microsoft/brewlet/blob/brewlet-2.0/provisioner/README.md)
and [verified failure/recovery scenarios](verification.md).
