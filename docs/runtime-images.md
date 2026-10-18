# Runtime images and pools

Approval and placement are independent. A cluster-scoped `RuntimeImage`
approves a digest-pinned runtime; a `NodeProfile` offers IDs on named pools.
Pods select an ID, never a runtime digest.

## Prepare a valid golden image

The platform's image pipeline must produce a root that:

- Supports every actual architecture of its offering pools.
- Provides exactly one PATH, with nonempty absolute directories outside `/app`.
- Has no duplicate environment names and no nonempty `/app`.
- Defines an `app` account and primary group matching the immutable approved UID/GID.
- Provides the account's existing home directory.
- Contains no set-ID binaries or file capabilities.

The runtime config contributes only Env. Runtime Entrypoint, Cmd, WorkingDir
and User do not define the application process. Brewlet does not build or
silently sanitize the approved image.

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
The IDs and account numbers are examples, not a built-in catalog.

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
