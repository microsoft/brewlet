# Ship the app. Share the runtime.

Brewlet 2.0 brings language runtimes back to the infrastructure level, like the
OS or Kubernetes. The application remains the workload; its runtime is supplied
by infrastructure instead of being bundled into every application image.

Publish code, dependencies and an explicit launch descriptor as an OCI
application image. Infrastructure provisions approved, digest-pinned runtime
roots on the node, and Brewlet composes the application with its selected runtime
when a container is created.

**Start here:** [build from source](getting-started.md), [package an
application](applications.md), or [approve a runtime and select its pools](runtime-images.md).

## One mechanism, multiple ecosystems

Java, Python, Node.js and .NET use the same generic artifact, inventory,
provisioning and execution path. Core does not detect languages, infer
entrypoints, select interpreters or inject VM tuning.

| Application workload | Runtime infrastructure |
| --- | --- |
| Application and dependency bytes under `/app`. | Runtime image root, userland and authoritative PATH. |
| `launch.json` with explicit argv, environment and working directory. | Approved runtime IDs and digest sources. |
| Supported OCI platforms. | Pool placement, Eager/OnDemand policy and runtime compatibility. |
| Reviewed native Kubernetes workloads and resources. | Node provisioning, authorization, fencing and cleanup. |

The application image does not contain the platform runtime root. Standard OCI
registries and kubelet pulls still deliver application layers. Each workload
retains its own process and resources; shared files do not imply shared heaps.

## Native Kubernetes, deliberately

Opt in using `runtimeClassName: brewlet` and the explicit
`brewlet.sh/runtime-image` Pod annotation. Use ordinary Deployments, StatefulSets,
DaemonSets, Services and HPAs. There is no `JavaApplication` or replacement
workload CRD in 2.0.

All application containers in a Brewlet Pod use the same runtime ID and version-2
artifact contract. The infrastructure sandbox alone uses the standard runc path.
Existing non-Brewlet workloads continue to use their ordinary runtime.

## Know what is verified

The final local complete suite passed 82 selected assertions and the separate
cold-install suite passed 13. A 101-requirement ledger combines these with
producer, native-kernel and real-node companion evidence.

That is not a stable release, cloud-provider autoscaling certification or a
performance benchmark. Read the [verification boundaries](verification.md)
before evaluating the system on an isolated node.

[What changes from main?](brewlet-2-comparison.md)

The comparison is revision-pinned. Its website/distribution gaps describe that
baseline; this source-built website preview was added afterward and does not
restore a stable-release installer or publication pipeline.
