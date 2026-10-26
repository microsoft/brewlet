# Safe removal and recovery

Removing Brewlet reverses changes it made to your nodes. Let cleanup finish
instead of deleting resources until Helm reports success. Brewlet does not
support in-place downgrades or reusing state across releases; to move between
releases, remove Brewlet with this procedure and reinstall. See
[transitions and recovery](supported-release.md#bounded-transitions-and-recovery).

Before you start, pin the kubeconfig and context you intend to change, and
record the UIDs of your `NodeProfile`s, `RuntimeImage`s and selected nodes.
You need them if a node goes missing during removal.

## 1. Stop new changes

1. List the Helm release, `NodeProfile`s and `RuntimeImage`s, and check
   `status.taskObservations` for running containers (see
   [rotation and cleanup](operations.md#find-workloads-still-on-an-older-generation)).
2. Update your Helm or GitOps source so it does not recreate the objects you
   are removing.
3. Leave unrelated namespaces, workloads and runtime images alone.

## 2. Drain the affected workloads

Scale down or delete the Brewlet workloads through their controllers. Wait
until their containers, Exec sessions and mounts are gone. Removing a runtime
stops new containers from starting, but running ones keep going. An empty Pod
list or a deleted namespace does not prove a node is drained.

If you delete a profile while containers still run, its finalizers stay until
cleanup can finish. Do not remove them manually.

## 3. Remove the profiles

For a profile you created with `kubectl`:

```sh
kubectl delete nodeprofile <profile> --wait=false
kubectl wait --for=delete nodeprofile/<profile> --timeout=10m
```

For profiles created through `provisioner.profiles`, remove them from your Helm
values and upgrade the release.

On each node, the worker removes the installed runtimes once no container uses
them, reverses the containerd configuration it changed and removes the shim it
installed. It then stops, and the profile's finalizer is released. If the
worker cannot confirm a step, the profile stays until it can.

## 4. Uninstall the control plane

```sh
helm uninstall brewlet --namespace brewlet --timeout 10m
```

A pre-delete hook waits for profile cleanup for up to
`uninstall.timeoutSeconds` (default 240). Set Helm's `--timeout` longer than
that value, with some margin for the hook to start.

Do not use `--no-hooks` to bypass cleanup. If the hook fails, find out why;
do not delete its RBAC or finalizers to make the uninstall succeed.

## 5. Verify before reinstalling

1. Check that the profiles are gone and that nodes no longer carry
   `brewlet.sh/runtime` labels or the startup fence.
2. Check that containerd's configuration is restored and that the container
   runtime is healthy (for example, `crictl info`).
3. Helm leaves the CRDs and any remaining `RuntimeImage`s in place. Do not
   delete shared CRDs to make another install succeed. Apply the new release's
   CRDs before installing it, as described in [installation](installation.md).

## Blocked or missing nodes

If a provider keeps applying stale runtime labels, fix the provider's pool
configuration (see [providers](providers.md)); cleanup cannot remove the
startup fence while stale labels remain. A new node with the same name does
not complete the cleanup owed by the original node.

If a node was permanently destroyed outside Kubernetes before its cleanup
finished, its profile stays blocked. A platform administrator resolves this by
creating a `NodeRetirementEvidence` that records the destruction:

```yaml
apiVersion: node.brewlet.sh/v1alpha1
kind: NodeRetirementEvidence
metadata:
  name: web-original-instance-retired
spec:
  profileName: web
  profileUID: <original-profile-uid>
  nodeName: <original-node-name>
  nodeUID: <original-node-uid>
  providerID: <original-provider-id>
  systemUUID: <original-system-uuid>
  externalInstanceID: <destroyed-instance-id>
  platformRecord: https://audit.example.com/retirement/<record>
  permanentlyDecommissioned: true
```

Brewlet accepts it only when the original node no longer exists and every
identity field matches the node the profile recorded. Creating it requires the
chart's `<release>-brewlet-retirement-attestor` ClusterRole, which is not bound
to anyone by default; grant it only to the administrators who may attest
permanent decommissioning. Accepted evidence cannot be deleted.

This is not a force-delete mechanism. Use it only for hosts that have actually
been destroyed. See the [node runbook](../provisioner/README.md) and
[troubleshooting](troubleshooting.md).
