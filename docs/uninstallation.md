# Safe removal and recovery

Removal is a production lifecycle operation, not deleting files until Helm
looks successful. Preserve original Node/profile/inventory UIDs and recovery
information before starting.
The [prerelease transition policy](supported-release.md#bounded-transitions-and-recovery)
does not promise in-place upgrade, downgrade or persisted-state reuse.

## 1. Review ownership and stop new writers

Pin the intended kubeconfig/context. Inventory the release, profiles,
RuntimeImages, actual bindings and any missing-host obligations. Update
Helm/GitOps source definitions so they do not recreate objects while you remove
them. Do not delete unrelated namespaces, workloads or runtime inventory.

## 2. Drain the affected workloads

Use the native controller's reviewed scale/delete procedure and wait for actual
application, Exec and mount drain. Revocation stops new starts but deliberately
preserves running tasks. Namespace disappearance or an empty Pod list alone is
not complete host evidence.

If a profile is deleted while tasks remain, its finalizers/claim must remain
until production cleanup can finish. Do not clear them manually.

## 3. Remove the reviewed profiles normally

For an unmanaged profile selected for removal:

```sh
kubectl delete nodeprofile <reviewed-profile> --wait=false
kubectl wait --for=delete nodeprofile/<reviewed-profile> --timeout=10m
```

For chart-managed profiles, change their owning Helm/GitOps definition under
the reviewed maintenance procedure instead of racing the controller.

The worker must revoke original inventory obligations, prove roots drained,
publish/read back complete receipts, reverse owned host configuration and
remove only the owned shim. Worker teardown precedes conditional claim release.
Uncertainty retains the obligation.

## 4. Uninstall the control plane

Only after reviewing the release's scope:

```sh
helm uninstall brewlet --namespace brewlet --timeout 10m
```

The pre-delete coordinator waits for production profile cleanup. Configure
`uninstall.timeoutSeconds` beforehand; Helm's timeout must exceed the
coordinator deadline plus startup/observation overhead.
Do not use `--no-hooks` to bypass cleanup. A failed hook is retained evidence,
not permission to delete its RBAC or finalizers.

## 5. Verify before reinstalling

Check original profile/worker obligations, node claims, current capabilities,
owned containerd reversal and actual CRI health. CRDs and unrelated inventory
may remain after Helm; do not blindly delete shared schemas to make another
install succeed. Apply reviewed current schemas before a matching installation.

## Blocked or missing hosts

Provider-pinned stale labels require provider configuration repair; cleanup
cannot release a startup fence over stale capabilities. A replacement Node
with the same name does not discharge the original Node UID's obligation.

Permanent external decommission requires the narrowly authorized
`NodeRetirementEvidence` process, with original identities and an actual
platform decommission record. It is not a generic force-delete switch or
evidence to invent during a tutorial. Use the detailed
[node runbook](../provisioner/README.md) and [failure guide](troubleshooting.md).
