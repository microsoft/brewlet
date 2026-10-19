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

```yaml
apiVersion: node.brewlet.sh/v2alpha1
kind: RuntimeImage
metadata:
  name: python-3.13
spec:
  source:
    image: registry.example.com/golden/python@sha256:<64-lowercase-hex>
  user:
    uid: 65532
    gid: 65532
```

This is a template. Replace the source with your validated golden image digest.
The IDs and numeric identities are examples, not a built-in catalog.

The local unchanged-upstream companion qualifies pinned distroless Java,
Python, Node and static sources on arm64. It is separate from the four-language
full-userland regression, which also covers .NET; it is not a claim that every
upstream image or architecture is compatible. See [verification](verification.md).

## Place IDs on pools

```yaml
apiVersion: node.brewlet.sh/v2alpha1
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
profile opts in. Tolerations are explicit; there is no blanket default.

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

`idleTTL` is permitted only for OnDemand. Reclamation waits for complete lease
and mount drain; it does not kill workloads.

## Source-built component installation

Install matching operator/admission and provisioner images from this checkout.
The host must supply containerd 2.x, systemd/systemctl, crictl, cgroup v2 and the
supported overlayfs layout. The default containerd socket is currently required.

```sh
docker build -f kubernetes/Dockerfile \
  -t registry.example.com/brewlet/control:2.0 .
docker build -f provisioner/Dockerfile \
  -t registry.example.com/brewlet/node:2.0 .
docker push registry.example.com/brewlet/control:2.0
docker push registry.example.com/brewlet/node:2.0
```

Supply reviewed values, image digests and registry access for your environment.
Apply the current CRDs before every chart install/upgrade:

```sh
kubectl apply -f kubernetes/deploy/crds/
helm upgrade --install brewlet kubernetes/charts/brewlet \
  --namespace brewlet --create-namespace \
  --set-string operator.image=registry.example.com/brewlet/control@sha256:<control-digest> \
  --set-string admission.image=registry.example.com/brewlet/control@sha256:<control-digest> \
  --set-string provisioner.image=registry.example.com/brewlet/node@sha256:<node-digest>
```

Replace every placeholder before running. An empty profile list intentionally
does not provision every node. Review and apply your runtime/profile definitions
separately, or supply named profiles through reviewed Helm values.

## Startup fencing and provider labels

`brewlet.sh/runtime=ready` is only usable readiness when the startup fence is
absent. Per-ID capabilities are `brewlet.sh/runtime.<id>=available|ready`.
Application Pods must never tolerate the provisioning fence.

Provider templates do not prove a real node is ready. OnDemand per-ID labels
must be mutable. If a provider pins a stale capability, confirmed node-wide
fencing is required; if withdrawal and fencing both fail, progress remains blocked.
No cloud scale-from-zero execution is certified by the local kind tests.

[Continue to rotation and cleanup](operations.md).
