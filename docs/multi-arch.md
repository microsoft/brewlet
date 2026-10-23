# Multi-architecture fleets

There are three independent declarations. All must agree:

| Declaration | Meaning |
| --- | --- |
| Application OCI platforms | The packaged bytes are asserted runnable on those platforms. |
| Approved runtime image platforms | Runtime roots cover every actual architecture of the offering pools. |
| Native workload scheduling constraints | Kubernetes places the workload only on a supported architecture. |

An OCI index is not a scheduler constraint. Brewlet does not infer platforms
from source files or language names.

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

For multi-architecture workloads, author the appropriate native affinity.
Admission preserves existing selectors/required alternatives and adds runtime
capability constraints; it does not turn an application index into architecture
affinity. Standard normalized platform aliases are supported, but ambiguous
equivalent index entries and conflicting declarations fail.

The worker checks known Nodes and valid offering profiles. It cannot qualify
future autoscaler hardware merely from a template; real platforms are checked
when nodes arrive. [Provider labels](providers.md) are not image validation.

## Qualification scope

Current local physical execution is arm64. Linux amd64 cross-builds and pinned
amd64 upstream inputs are not physical amd64 node proof. Keep test claims
separate from a platform's own fleet qualification and region/capacity support.
