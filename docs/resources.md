# Resources and workload behavior

Each application process retains its own Kubernetes resources and runc
isolation. Shared runtime files do not create a shared memory budget or heap.

## Requests, limits and overhead

| Setting | Meaning |
| --- | --- |
| CPU/memory requests | Scheduling/capacity reservations; CPU request is also the utilization denominator for CPU HPA. |
| CPU limit | Container cgroup quota, not an inferred interpreter thread count. |
| Memory limit | Container memory ceiling, not a Brewlet-calculated heap setting. |
| RuntimeClass overhead | An additional `50m` CPU/`64Mi` per-Pod shim/staging reservation. |
| Ephemeral-storage request/limit | Native Kubernetes accounting/eviction policy, including managed writable container storage. |

The memory limit must cover the process's full footprint: heap, native memory,
stacks, agents and other allocations. Container awareness belongs to the
selected runtime. Brewlet injects no GC, heap or interpreter tuning.

## Keep runtime-specific tuning explicit

Use deliberate descriptor argv or the runtime's documented environment
configuration. For example, a Java producer may author JVM options and a Node
application may use Node's documented options; neither becomes a core
language-detection feature. Keep workload scaling settings in native Kubernetes
manifests and environment-dependent tuning out of reusable app bytes where practical.

Kubernetes `args` replaces the image's argument array; it does not append.
Preserve the complete intended invocation when overriding a descriptor.

## Probes, Exec, files and shutdown

Prefer HTTP/TCP probes where suitable. Exec probes and hooks use the initial
container's effective identity, environment and leased generation. Distroless
and pathless runtimes require suitable explicit executable paths; no shell is
provided implicitly.

Use a writable volume for application data and a scoped Secret volume for
secrets. `/app` remains read-only except explicit mounted destinations.
`readOnlyRootFilesystem: true` also restricts runtime-visible root writes;
HOME being `/tmp` does not imply that `/tmp` is writable.

Signals and termination grace remain native runc/Kubernetes behavior. Implement
application signal handling, choose a sufficient grace period and verify real
exit behavior. Brewlet does not wrap the process in an inferred shell.

## Autoscaling

Use ordinary HPA resources against native Deployments/StatefulSets. Define
requests explicitly, install a compatible metrics pipeline and verify actual
scale and availability behavior. Runtime files on disk are not an HPA metric.

Node scale-from-zero additionally needs compatible
[provider templates and startup fencing](providers.md). CPU HPA does not prove
that new nodes can provision the chosen runtime.
