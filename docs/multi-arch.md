# Multi-architecture fleets

Three separate declarations must agree:

| Declaration | Meaning |
| --- | --- |
| Application image platforms | The platforms the packaged application runs on. |
| Approved runtime image platforms | The runtime image must support every architecture in the pools that offer it. |
| Workload scheduling constraints | Kubernetes places the workload only on a supported architecture. |

An OCI image index does not constrain scheduling. Brewlet does not infer
platforms from source files or language names.

## Portable and native payloads

Repeat `--platform` to build a multi-platform image index in one step. An
unprefixed `--layer` is shared, byte for byte, by every declared platform, so
use it only for payloads that work unchanged on each one:

```sh
brewlet build registry.example.com/apps/api:v2 \
  --platform linux/amd64 --platform linux/arm64 \
  --launch launch.json --layer app=./src:/app/src
brewlet push registry.example.com/apps/api:v2
```

Native extensions, binaries, architecture-specific wheels, JNI libraries and
prebuilt archives may require different bytes. Prefix such a layer with the
platform it belongs to; do not label one native payload as portable:

```sh
brewlet build registry.example.com/apps/api:v2 \
  --platform linux/amd64 --platform linux/arm64 --launch launch.json \
  --layer linux/amd64:dependency=./wheels-amd64:/app/dependencies \
  --layer linux/arm64:dependency=./wheels-arm64:/app/dependencies \
  --layer app=./src:/app/src
brewlet push registry.example.com/apps/api:v2
```

Each platform gets the shared layers plus its own, in command-line order, and
must end up with at least one `app` layer. A prefix naming an undeclared
platform fails with `InvalidPlatform`. Layer sets cannot carry platform
prefixes. `brewlet index` remains available to combine manifests produced
elsewhere into one index. See [CLI options](cli-reference.md).

## Scheduling and pool coverage

For a single-architecture payload, set the ordinary selector:

```yaml
nodeSelector:
  kubernetes.io/arch: arm64
```

For a multi-architecture image, list the supported architectures in a node
affinity:

```yaml
affinity:
  nodeAffinity:
    requiredDuringSchedulingIgnoredDuringExecution:
      nodeSelectorTerms:
        - matchExpressions:
            - key: kubernetes.io/arch
              operator: In
              values: [amd64, arm64]
```

Admission keeps your selectors and affinity, and adds a requirement for nodes
that offer the Pod's runtime ID. It does not derive architecture affinity from
the application image. Standard platform aliases are normalized; ambiguous
index entries and conflicting declarations fail.

The worker checks the architecture of existing nodes against the runtime
images their profiles offer. Nodes that an autoscaler has not created yet are
checked when they join; [provider labels](providers.md) do not validate images.

Brewlet's own tests run on physical arm64 nodes only. Qualify amd64 and any
other architecture on your own fleet before relying on it.
