# When to use Brewlet

Image volumes already separate runtime and application delivery. Brewlet adds
an integrated system for treating runtimes as infrastructure: approval, node
provisioning, generation tracking, controlled rotation, start authorization
and retirement. Choose it when those guarantees justify operating a privileged
node runtime integration, not merely to publish application and runtime
bytes separately.

Compare it with the strongest native alternative: a complete approved runtime
as the container image, with the application mounted read-only at `/app` from
an image volume. The inverse, mounting an arbitrary runtime under `/runtime`,
can break its expected loader, library and filesystem paths.

| Need | Conventional application image | Runtime container + application image volume | Brewlet |
| --- | --- | --- | --- |
| Publish application and runtime separately | Final application image includes the selected runtime/base graph. | Separate runtime and application image digests. | Separate application artifact and approved RuntimeImage inventory. |
| Update runtime without rebuilding application bytes | Usually publish a new application image graph with the updated base. | Change the runtime image; retain the application-volume digest. | Approve a runtime generation; retain the application artifact digest. |
| Centrally approve runtime selection | Requires platform policy and delivery tooling. | Requires platform policy and delivery tooling for both image references. | Integrated runtime inventory, approval, pool provisioning and startup checks. |
| Recheck runtime authorization on every container Create | Pod admission does not rerun on restart; registry checks can gate starts with an availability cost. | Same tradeoff; see restart policies below. | Renewable node-local authorization must remain valid even when the root is cached. |
| Apply application launch metadata | Container image config and Pod settings. | Render explicit command, arguments, environment and working directory; a volume's image config is not executed. | Validated explicit launch descriptor with runtime-owned PATH and protected Exec state. |
| Use native Kubernetes controllers | Supported. | Supported. | Supported; no replacement workload CRD. |
| Mix heterogeneous sidecars in one Pod | Supported, subject to cluster policy. | Supported, subject to cluster policy. | Each managed container selects its own runtime; unselected containers use ordinary OCI behavior. |
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

The [native image-volume experiment](image-volumes.md) runs without
installing Brewlet:

- **Reproduced:** runtime rotation, dependency replacement and rollback with
  an unchanged application image.
- **Approximated:** an ID-based runtime catalog with built-in admission
  policies, using Python, Java and Node on one shared base. Mounting runtimes
  into a shared base couples their ABI to it.
- **Observed lifecycle gaps:** admission-time catalog changes did not trigger
  rollouts, scale-up mixed runtime versions within one ReplicaSet, and
  `rollout undo` did not restore the previous runtime.
- **Observed restart tradeoff:** cached runtimes restarted after revocation;
  `Always` blocked restarts after registry deletion and during registry outages.

The shared-base ABI coupling is not inherent to image volumes. Admission
could instead inject a complete runtime as the container image, preserving
its filesystem and libraries while selecting it from a platform-owned catalog.
The experiment verified those building blocks separately, but did not test
that combination end to end.

Separate image fields are not separate team authorization. Kubernetes RBAC
does not ordinarily authorize individual Deployment fields, and server-side
apply field ownership is not an access-control boundary. A native platform
still needs an approved catalog, coherent writers/admission for both runtime
and application images, compatibility checks, rollout policy and observed
identity. Those are governance requirements, not proof that a custom shim
is necessary.

Brewlet supplies an integrated approach to runtime management and node-level
start authorization. Choose that integration when its guarantees justify the
operational cost.

## Rotation and rollback

The catalog findings apply to **digest injection at Pod admission**, not to
all image-volume deployments. In that design, the Deployment template records
a runtime ID but not its resolved digest. Changing the catalog creates no new
ReplicaSet; scaling can introduce a newer runtime under the same template
hash. Undoing a Deployment revision still resolves the current catalog entry.

Recording runtime digests in workload templates preserves native rollout
history, as the experiment's explicit-reference layout demonstrates. A native
platform can use delivery automation or a controller to propagate catalog
changes into those templates and retain rollback targets.

Brewlet integrates generation tracking, node-observed bindings and opt-in
rotation restarts. With rotation restarts enabled, its operator sets a
minimum-generation barrier in opted-in workload templates and uses each
workload's rollout strategy. This provides visibility and controlled
convergence, not an atomic switch or a guarantee that every replica always
runs the same generation. The barrier is not an exact-version pin, and
`rollout undo` is not a Brewlet runtime rollback mechanism. See
[rotation and cleanup](operations.md#rotate-without-rebuilding-application-bytes).

## Authorization, restarts and recovery

Brewlet rechecks runtime authorization when creating a container task, including
a restart within an existing Pod. An installed root with expired or revoked
authorization cannot start a new task. Pod admission alone is not rerun on
that restart in a conventional or image-volume deployment.

The mechanisms have different failure modes:

| Mechanism | Revocation and restart behavior | Availability dependency |
| --- | --- | --- |
| Native cached runtime (`IfNotPresent`) | Removing a catalog entry does not stop an existing Pod restarting its pinned runtime from cache. | Cached bytes can permit restarts during a registry outage. |
| Native runtime with `Always` | Registry deletion blocked restarts in the experiment; removing only a catalog entry does not revoke the existing Pod's reference. | Registry outages also blocked restarts, despite cached layers. |
| Brewlet installed runtime | Every container create checks node-local approval and ownership authorization. | No registry round trip is needed for an installed runtime, but authorization expires after five minutes without renewal. |

For a runtime mounted as an image volume, the native setting is
`volumes[].image.pullPolicy`; for a runtime used as the container image, it is
`containers[].imagePullPolicy`. The experiment exercised the registry outage
and deletion cases on runtime volumes. Registry deletion is a coarse control:
it affects every consumer of that repository's manifest, not copies published
elsewhere.

Brewlet separates start authorization from registry availability for installed
runtimes; it does not eliminate availability dependencies. Prolonged loss of
authorization renewal blocks recovery starts even with cached bytes. See the
[experiment's restart findings](image-volumes.md#9-restart-authorization-revocation-versus-availability).
Existing Brewlet tasks retain their leased root; revocation is not
automatic termination of running processes. See [operations](operations.md).

Neither design hot-patches running processes. Runtime changes reach existing
workloads through new task instances or controller-driven replacement, with
the chosen rollout strategy controlling disruption.

## Choosing between them

- **Choose native image volumes** when independent delivery, explicit-digest
  rollouts and existing platform policies meet the requirements. They also
  avoid installing a custom privileged node runtime integration.
- **Choose Brewlet** when integrated approval, pool provisioning, generation
  visibility, opt-in rotation, restart authorization and retirement evidence
  are worth that integration's operating cost. Its value is supplying and
  enforcing the runtime lifecycle, rather than requiring the platform team
  to assemble and maintain the equivalent controls.

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
The five workload guides qualify the Brewlet path. The native image-volume
experiment qualifies only its minimal Python, Java and Node fixtures and pinned
inputs, not arbitrary runtime/application combinations.

The experiment ran native Kubernetes only. Its observations are measured
native results; the Brewlet column describes documented behavior, not a
head-to-head validation in the same run. Controls not implemented in the
experiment are not proven impossible with native platform engineering.
Neither performance nor production-reliability superiority follows from these
results.

## Coexistence

Non-Brewlet Pods keep their ordinary runtime. Within a Brewlet Pod, each selected application, init, sidecar or ephemeral
container uses the v1 artifact contract and its own selected runtime ID.
Unselected containers retain ordinary OCI behavior, including conventional
debug containers. The infrastructure sandbox is not an application container.
