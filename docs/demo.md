# A guided demonstration

The [local Kubernetes tutorial](local-kubernetes.md) is the runnable demonstration.
It uses a tiny Python HTTP service, pinned unchanged distroless roots and
released components. It does not require Java, Maven, Node or .NET SDKs.

## What to show

| Scene | Observation |
| --- | --- |
| Approve infrastructure | A RuntimeImage identifies exact upstream bytes; a NodeProfile offers the ID on the private node. |
| Package the app | The artifact contains explicit launch metadata and app bytes, not a Python/OS root. |
| Start a native Deployment | HTTP returns process identity and interpreter information from the real application. |
| Inspect the binding | Pod/container identity maps to an actual inventory epoch, generation and digest. |
| Update the approved source | Node convergence completes; the original application process retains its old lease. |
| Roll the Deployment | New Pods use the updated source with the exact same application digest. |
| Remove normally | Workloads drain, profile/host cleanup completes, then the owned cluster is destroyed. |

This is a demonstration script, not a prerecorded video or a performance
benchmark. Capture a recording only from an actual run. The older Java/main
conference material is historical context, not current execution evidence.

## Boundaries to say out loud

The tutorial preimports OCI content into one owned node; it does not prove
registry authentication, cloud autoscaling or another CPU architecture.
An unchanged application digest proves that the runtime update did not rebuild
the app, not that every runtime release is ABI-compatible or fixes a specific CVE.
Existing processes change only through normal workload replacement.

For classroom use, split the tasks between the
[platform](workshops/platform.md) and [developer](workshops/developer.md)
workshops. For formal qualification, use the separately scoped
[acceptance procedures](verification.md).

## Java canvas demo

Contributors using the GitHub Copilot app can run a Java version of the same
flow from the
[`brewlet-demo` canvas extension](https://github.com/microsoft/brewlet/tree/main/.github/extensions/brewlet-demo).
It builds ShowMyJVM, packages it with `brewlet build` (no JDK in the image),
deploys it as a plain Deployment on a private kind cluster, and switches the
node-supplied runtime from Temurin 25 to 27 and back by changing one annotation.
