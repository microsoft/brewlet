# Runtime images and pools

Approval and placement are independent. A cluster-scoped `RuntimeImage`
approves a digest-pinned runtime; a `NodeProfile` offers IDs on named pools.
Pods select an ID, never a runtime digest.

## Prepare a valid golden image

An approved ("golden") image need not be custom-built. A compatible upstream
OCI image can be approved unchanged, including a shell-free distroless image.
The platform must validate that its root:

- Supports every actual architecture of its offering pools.
- Has unique environment names. PATH is optional; when present, its components
  are nonempty canonical absolute directories outside `/app`. Missing
  directories are allowed; symlinks must not escape, loop or traverse `/app`.
- Supports the approved nonzero numeric UID/GID. No account named `app`,
  passwd/group database or pre-existing home is required. Present account
  metadata must be well formed and unambiguous.
- Contains no set-ID binaries or file capabilities.

The runtime config contributes only Env. Runtime Entrypoint, Cmd, WorkingDir
and User do not define the application process. Brewlet does not build or
silently sanitize the approved image.

Use an explicit absolute executable in `launch.json`, such as
`/usr/bin/python3.13` or `/nodejs/bin/node`, when the runtime does not supply a
suitable PATH. Without a runtime PATH, relative initial/Exec invocation fails
with `RuntimePATHRequired`; CRI defaults and the working directory are not
fallback search paths. No runtime shell, entrypoint script or account-management
utility is invoked by the provisioner.

Runtime `/app` contents are hidden, not merged with application layers or
removed from the approved image. A private opaque boundary in the container's
overlay keeps packaged application/dependency bytes isolated and read-only;
explicit Kubernetes volumes still follow that boundary. Runtime and payload
caches remain unchanged.

HOME uses an explicit final environment value when supplied, otherwise optional
passwd metadata for the actual UID, otherwise `/tmp`. This does not create a
home directory or make NSS lookups work without the relevant database.
Applications requiring username lookup must select a suitable runtime.

## Declare data and configuration mount locations

Brewlet requires a read-only container root, not just an immutable shared
node cache. A process cannot modify a private copy of the runtime either.
The platform explicitly approves additional volume locations with
`spec.volumeMountPaths`:

```yaml
apiVersion: node.brewlet.sh/v1alpha1
kind: RuntimeImage
metadata:
  name: python-3.13
spec:
  source:
    image: registry.example.com/golden/python@sha256:<64-lowercase-hex>
  user:
    uid: 65532
    gid: 65532
  volumeMountPaths:
    - /cache
    - /tmp
```

This allows workload volumes at those paths or their descendants; it does
not create volumes or make the directories writable automatically. Omit the
list for workloads that need only explicit mounts beneath `/app/`.
The policy covers read-only configuration mounts as well as writable storage,
so a volume cannot silently replace runtime programs or libraries.

Choose canonical data/configuration directories, not symlinks, runtime PATH
directories or their ancestors. Lists are sorted, unique and nonoverlapping,
with at most 32 entries. The policy is immutable within a runtime inventory
identity; use a new ID for a different mount contract. Standard Kubernetes
system mounts and read-only service-account projection are handled separately.

App teams mount their `emptyDir`, PVC or configuration volumes in those
approved locations and set any runtime-specific cache variables themselves.
For example, a WebAssembly runtime might declare `/cache` and `/work`, keeping
engine compilation caches separate from guest-writable storage. See
[writable storage](resources.md#probes-exec-files-and-shutdown) for the workload side.

This is a template. Replace the source with your validated golden image digest.
The IDs and numeric identities are examples, not a built-in catalog. Brewlet
does not guarantee that every upstream image or architecture is compatible;
validate each image you approve.

## Place IDs on pools

```yaml
apiVersion: node.brewlet.sh/v1alpha1
kind: NodeProfile
metadata:
  name: application-pool
spec:
  nodePool:
    key: brewlet.example.com/pool
    names: [applications]
  runtimeImages:
    - id: python-3.13
      install: OnDemand
      idleTTL: 72h
  rollout:
    containerdRestart: validated
    maxUnavailable: 1
  tolerations:
    - key: startup-taint.cluster-autoscaler.kubernetes.io/brewlet
      operator: Equal
      value: provisioning
      effect: NoSchedule
```

Selecting a pool authorizes privileged host modification. Use only disposable
or explicitly controlled nodes. Control-plane nodes are excluded unless the
profile opts in. `spec.tolerations` applies to the provisioner worker only, and
there is no default; the example lets the worker run on nodes that still carry
the startup fence.

## Installation modes

| Mode/state | Meaning |
| --- | --- |
| Eager (default) | Validate/install the required root before aggregate readiness. |
| OnDemand available | Baseline is validated; the runtime root has not been fetched. |
| OnDemand installing | A freshly authorized first-use request is being processed. |
| Ready | A validated immutable generation is current. |
| Failed | Scheduling capability is withdrawn; installation retries with backoff. |

The shim creates an empty private marker for a cold authorized ID, never pulls
the runtime. Its default install wait is at most 30 seconds and remains below
a forwarded request deadline; hosts must configure kubelet's runtime request
timeout above that default. Failure does not authorize new markers.

`idleTTL` is permitted only for OnDemand. Reclamation waits until no container
uses the runtime; it does not kill workloads.

Apply `RuntimeImage` and `NodeProfile` objects with `kubectl`, or supply named
profiles through the `provisioner.profiles` Helm value (see
[configuration](configuration.md)). With no profiles, Brewlet provisions no
nodes.

## Startup fencing and provider labels

`brewlet.sh/runtime=ready` is only usable readiness when the startup fence is
absent. Per-ID capabilities are `brewlet.sh/runtime.<id>=available|ready`.
Application Pods must never tolerate the provisioning fence.

Provider templates do not prove a real node is ready. OnDemand per-ID labels
must be mutable. If a provider pins a stale capability, confirmed node-wide
fencing is required; if withdrawal and fencing both fail, progress remains blocked.
Cloud scale-from-zero has not been verified.

[Continue to rotation and cleanup](operations.md).
