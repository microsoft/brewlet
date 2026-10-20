# When to use Brewlet

Choose Brewlet when separating application delivery from centrally governed
runtime infrastructure is useful enough to justify operating a privileged
node runtime integration.

Independent application/runtime ownership does not by itself require Brewlet.
Compare it with the strongest native alternative: a complete approved runtime
as the container image, with the application mounted read-only at `/app` from
an image volume. The inverse, mounting an arbitrary runtime under `/runtime`,
can break its expected loader, library and filesystem paths.

| Need | Conventional application image | Runtime container + application image volume | Brewlet |
| --- | --- | --- | --- |
| Publish application and runtime separately | Final application image includes the selected runtime/base graph. | Separate runtime and application image digests. | Separate application artifact and approved RuntimeImage inventory. |
| Update runtime without rebuilding application bytes | Usually publish a new application image graph with the updated base. | Change the runtime image; retain the application-volume digest. | Approve a runtime generation; retain the application artifact digest. |
| Centrally approve runtime selection | Requires platform policy and delivery tooling. | Requires platform policy and delivery tooling for both image references. | Integrated runtime inventory, approval, pool provisioning and startup checks. |
| Recheck runtime authorization on every container Create | Pod admission alone does not recheck an existing Pod's container restart. | Same limitation of Pod admission alone. | Node-local authorization must remain valid even when the root is cached. |
| Apply application launch metadata | Container image config and Pod settings. | Render explicit command, arguments, environment and working directory; a volume's image config is not executed. | Validated explicit launch descriptor with runtime-owned PATH and protected Exec state. |
| Use native Kubernetes controllers | Supported. | Supported. | Supported; no replacement workload CRD. |
| Mix heterogeneous sidecars in one Pod | Supported, subject to cluster policy. | Supported, subject to cluster policy. | All application containers require the same runtime ID and v2 artifact contract. |
| Avoid custom node runtime administration | Normally uses the installed CRI runtime. | Requires image-volume support in Kubernetes and the CRI runtime. | Requires Brewlet provisioning, containerd configuration and the shim. |
| Obtain stronger isolation than runc | Choose an appropriate sandboxed runtime. | Image volumes do not add a sandbox boundary. | Not provided by Brewlet's runc backend. |
| Protect the container's runtime view from writes | Requires a read-only root and suitable mount policy. | Same requirement; read-only app volumes alone do not protect the runtime root. | Read-only root enforced by admission and shim; workload mounts constrained to platform-approved data/configuration zones. |

## Native image-volume alternative

[Image volumes](https://kubernetes.io/docs/concepts/storage/volumes/#image)
became stable in Kubernetes 1.36, after enabled-by-default beta in 1.35;
the selected CRI implementation must support them. They already permit
independent runtime/application publication and native Deployment rollouts.
The [image-volume task guide](https://kubernetes.io/docs/tasks/configure-pod-container/image-volumes/)
describes read-only mounts, credentials and pull behavior. The earlier blanket
`noexec` requirement was
[dropped from the design](https://github.com/kubernetes/enhancements/blob/fec00b660f2cd150d4bfaf5947989994c446dd56/keps/sig-node/4639-oci-volume-source/README.md);
do not assume executable files or native libraries are categorically unusable.
ABI, mount layout and security-policy compatibility still need qualification.

Separate image fields are not separate team authorization. Kubernetes RBAC
does not ordinarily authorize individual Deployment fields, and server-side
apply field ownership is not an access-control boundary. A native platform
still needs an approved catalog, coherent writers/admission for both runtime
and application images, compatibility checks, rollout policy and observed
identity. Those are governance requirements, not proof that a custom shim
is necessary.

Brewlet supplies an integrated approach to those workflows and node-level
start authorization. Its [composition tooling](managed-composition.md) also
provides release evidence independently of dependency resolution on the node.
Choose that integration when its guarantees justify the operational cost.

## Authorization, restarts and recovery

Brewlet rechecks runtime authorization when creating a container task, including
a restart within an existing Pod. An installed root with expired or revoked
authorization cannot start a new task. Pod admission alone is not rerun on
that restart in a conventional or image-volume deployment.

That protection has an availability cost: prolonged loss of authorization
renewal can block recovery starts even with cached bytes. A native deployment
may continue restarting a cached, digest-pinned runtime unless another control
prevents it. Existing Brewlet tasks retain their leased root; revocation is not
automatic termination of running processes. See [operations](operations.md).

Neither design hot-patches running processes. Runtime changes reach existing
workloads through new task instances or controller-driven replacement, with
the chosen rollout strategy controlling disruption.

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

Native image volumes and ordinary images also benefit from content-addressed
caching and snapshotter reuse. No equal-condition benchmark establishes a
Brewlet memory, startup, network or density advantage over that alternative.
The five workload guides qualify the Brewlet path, not a native image-volume
backend or arbitrary runtime/application combinations.

## Coexistence

Non-Brewlet Pods keep their ordinary runtime. Within a Brewlet Pod, every
application/init/sidecar/ephemeral container must use the v2 artifact contract
and the same runtime ID. The infrastructure sandbox is the exception.

If a sidecar requires an incompatible runtime, reconsider the Pod boundary or
use an ordinary container deployment; do not assume per-container runtime IDs.
