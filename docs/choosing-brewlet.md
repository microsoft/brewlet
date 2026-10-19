# When to use Brewlet

Choose Brewlet when separating application delivery from centrally governed
runtime infrastructure is useful enough to justify operating a privileged
node runtime integration.

| Need | Brewlet | Ordinary container |
| --- | --- | --- |
| Approve and rotate a shared runtime independently of app bytes | Explicit runtime inventory and immutable generations. | Usually rebuild and roll application images with a new base. |
| Carry a bespoke OS layout or run an image entrypoint script unchanged | Requires an explicit compatible runtime and application descriptor. | Natural image-level packaging model. |
| Ship code and dependencies without repeating the runtime root | Application artifact omits that root. | Base layers remain part of the image graph; unchanged layers can be cached. |
| Use native Kubernetes controllers and resources | Supported; no replacement workload CRD. | Supported. |
| Run root, privileged setup or added Linux capabilities | Not supported for Brewlet application containers. | Depends on cluster policy and container runtime configuration. |
| Avoid node runtime administration | Not a fit: provisioning changes containerd and installs a shim. | Usually already available on the cluster. |
| Stronger isolation than runc | Not provided. | Select a separately governed sandboxed runtime if needed. |

## Good evaluation candidates

Start with services whose runtime line and ABI are well understood, whose
runtime image can run under arbitrary non-root identities, and whose app
already separates immutable code from writable data.

Distroless, custom and compatible upstream roots are all candidates. An
application requiring username lookup, extra system libraries or diagnostic
tools must select a runtime that actually provides them.

## Costs and limits

The platform must operate approval, pool placement, registry access, startup
fencing, rotation and safe deprovisioning. Applications must declare executable
paths, platforms and resource requirements explicitly.

Runtime rotation does not fix dependency copies under `/app`. A shared root
does not imply shared heaps, lower total memory, faster cold starts or less
network traffic in every workload. Those are measurements to make, not product
guarantees. See [verification boundaries](verification.md).

## Coexistence

Non-Brewlet Pods keep their ordinary runtime. Within a Brewlet Pod, every
application/init/sidecar/ephemeral container must use the v2 artifact contract
and the same runtime ID. The infrastructure sandbox is the exception.

If a sidecar requires an incompatible runtime, reconsider the Pod boundary or
use an ordinary container deployment; do not assume per-container runtime IDs.
