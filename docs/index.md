# Ship the app. Not the runtime.

Brewlet brings language runtimes back to the infrastructure level, like the
OS or Kubernetes. The application remains the workload; its runtime is supplied
by infrastructure instead of being bundled into every application image.

Publish code, dependencies and an explicit launch descriptor as an OCI
application image. Infrastructure provisions approved, digest-pinned runtime
roots on the node, and Brewlet composes the application with its selected runtime
when a container is created.

**Start here:** [run your first local service](local-kubernetes.md),
[package an application](applications.md), or
[enable runtime infrastructure](installation.md).

Choose a [workload guide](language-recipes.md) for complete Java, Python,
Node.js, .NET and WebAssembly workflows. These are tested examples under one
generic execution contract, not an exhaustive built-in runtime catalog.

Choose the [developer workshop](workshops/developer.md) when a platform is
already prepared, or the [platform workshop](workshops/platform.md) to establish
approval, pools and a concrete handoff. Read
[when to use Brewlet](choosing-brewlet.md) before selecting it for a workload.

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
workload CRD in current Brewlet.

All application containers in a Brewlet Pod use the same runtime ID and version-1
artifact contract. The infrastructure sandbox alone uses the standard runc path.
Existing non-Brewlet workloads continue to use their ordinary runtime.

## Know what is verified

The amended local full-userland suite passed 82 selected assertions and the
unchanged-upstream distroless companion passed 28. The baseline cold-install
companion passed 13. A separate 18-assertion companion qualifies explicit
[Wasmtime and Spin HTTP examples](webassembly.md). A 111-requirement ledger combines explicitly scoped
producer, native-kernel and real-node evidence; historical acceptance is not
relabeled as proof of the revised image-shape rules.

That is not a stable release, cloud-provider autoscaling certification or a
performance benchmark. Read the [verification boundaries](verification.md)
before evaluating the system on an isolated node.
