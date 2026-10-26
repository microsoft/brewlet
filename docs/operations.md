# Rotation, security and cleanup

## Runtime identity and authorization

A runtime ID is a compatibility promise. Across digest rotations, keep the
runtime version line, ABI, numeric user and group, account behavior, PATH
layout and tools the same. Use a new ID for an incompatible change, or to hold
some workloads on an older source.

The `RuntimeImage`'s Kubernetes UID identifies its inventory epoch. If you
delete and recreate a `RuntimeImage` with the same name, it gets a new epoch;
old installed roots and old generation barriers are not reused. Generations
increase monotonically within an epoch.

Each generation is installed read-only under
`/opt/brewlet/runtimes/<id>/<inventory-uid>/<generation>/`. A `current`
pointer selects the validated generation. Each node holds a local
authorization for every runtime it offers; it expires after five minutes and
is renewed only after the node rechecks approval and ownership.

## Rotate without rebuilding application bytes

1. Update `spec.source.image` on the `RuntimeImage` to the new digest, keeping
   the same ID.
2. Each node validates and installs the new generation.
3. New containers use the new generation. Running containers keep the
   generation they started with until they are replaced.

Rotation alone does not restart running applications. To have Brewlet restart
them, enable `operator.rotationRestarts.enabled` in the chart and opt in each
workload:

```yaml
# On the Deployment/StatefulSet/DaemonSet metadata, not its Pod template:
metadata:
  annotations:
    brewlet.sh/restart-on-rotation: "true"
```

The operator restarts a workload by setting the
`brewlet.sh/rotated-to-generation` barrier annotation on its Pod template, so
the workload's own rollout strategy replaces its Pods. It does not delete Pods
or change update strategies. By default it restarts up to five workloads at
once, with a ten-minute progress deadline per workload. Workloads using
`OnDelete` are reported as not applicable. Paused or partitioned rollouts, and
Pods on nodes still installing or excluded from rotation, can stay deferred.
A patched template does not mean the restart succeeded; check the rollout.

A workload is restarted when one of its running Pods uses an older generation,
or started before the rotation completed and its node has not reported a
fresh, complete view. If a node's report is missing or stale, Brewlet restarts
the workload to be safe.

The annotation is the workload owner's consent, so platform operators need no
patch rights in application namespaces. The chart pairs the operator's patch
RBAC with a `ValidatingAdmissionPolicy` that only lets the operator set the
barrier annotation on opted-in Brewlet workloads. It denies all other changes,
creates and deletes, workloads without the annotation, and the
`kube-system`, `kube-public` and `kube-node-lease` namespaces. The policy needs
Kubernetes 1.30 or later; set
`operator.rotationRestarts.requireAdmissionPolicy=false` only if you accept
RBAC as the only safeguard. To limit restarts to specific namespaces, list
them; patch rights then come from a Role in each namespace instead of a
ClusterRole:

```yaml
operator:
  rotationRestarts:
    enabled: true
    namespaces: [payments, storefront]
```

## Observe actual bindings

Use Kubernetes resources, runtime status and node metrics:

```sh
kubectl get runtimeimages.node.brewlet.sh
kubectl get nodeprofiles.node.brewlet.sh
kubectl get nodes -L brewlet.sh/runtime
kubectl rollout status deployment/api --namespace apps
```

### Find workloads still on an older generation

`status.taskObservations` on each `RuntimeImage` lists running containers that
use an older generation or a previous inventory epoch, with their Pod
namespace, name and UID. You need only read access to the `RuntimeImage`; no
metrics stack or node access is required:

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

An observation with `current=false` means that node has stopped reporting.
Its last bindings are kept rather than assumed gone, so investigate the node
before treating its Pods as updated. These observations are for diagnosis
only; Brewlet does not use them to decide cleanup.

Node metrics are optional (`metrics.enabled=true`); see
[observability](observability.md). Useful metrics include
`brewlet_container_runtime_image_info`, `brewlet_runtime_image_generations`,
`brewlet_runtime_image_install_requests_total`,
`brewlet_runtime_image_install_duration_seconds` and
`brewlet_shared_storage_bytes`. Binding metrics identify the Pod and
container, inventory epoch, generation and digest.

## Security boundary

Brewlet uses runc isolation and a privileged provisioner that you must trust.
It is not gVisor or Kata and adds no isolation between hostile tenants. Every
application process runs as non-root, with non-root groups, no capabilities
and no privilege escalation. The same rules, and the runtime-owned PATH, apply
to Exec sessions, probes and hooks. A runtime image without PATH or account
files is still run as a numeric non-root user, and CRI's default PATH is not
used.

Application and dependency layers are confined to `/app`, so they cannot
replace runtime files. This does not control how the application loads its own
libraries or what volumes contain. Rotating the runtime does not fix
vulnerable libraries packaged with the application. Never package secrets into
application layers; they are world-readable. Any `/app` content in the runtime
image is hidden from the application; if the node cannot hide it, the
container fails to start rather than mixing runtime and application files.
See [security and hardening](security.md).

## Cleanup and revocation

When you remove a runtime ID from a pool or delete its `RuntimeImage`, new
containers can no longer start with it, while running containers keep their
runtime files. A disconnected node loses its authorization when it expires
after five minutes; node labels alone never authorize a start.

Cleanup removes each installed runtime, reverses the host configuration
Brewlet changed and stops the workers. Only then does it release its
finalizers. If a step fails, the runtime stays revoked and cleanup retries.

Delete resources and uninstall the chart normally. Uninstall waits for cleanup
and fails if it cannot complete. Do not force-delete finalizers, runtime
roots, leases or startup fences to make it succeed. Change Helm- or
GitOps-managed objects in their source.

The provisioner collects unused layers from the cache under
`/var/lib/brewlet/layers` after the `provisioner.gc.grace` period. If it
cannot confirm that no container, Exec session or mount still uses a layer, it
keeps the layer, even when no Pods appear to use it.

A node that was deleted or recreated keeps its original cleanup pending. A
replacement node does not clear it. To resolve it, follow the retirement
evidence process in [safe removal](uninstallation.md).

See the [node runbook](https://github.com/microsoft/brewlet/blob/main/provisioner/README.md)
for details.
