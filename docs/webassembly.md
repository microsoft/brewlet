# WebAssembly through runtime infrastructure

The examples run a Linux Wasm host through Brewlet's existing containerd/runc
path. They do not install runwasi, add a Wasm-specific shim, detect modules or
introduce a workload CRD.

**Qualification status:** both examples passed their 18-assertion real-node
companion on native arm64, including source updates, held Exec recovery and
normal cleanup. This is explicitly scoped qualification, not all WASI
interfaces, another CPU architecture or native runwasi execution.

## Tested runtime inputs

| Example | Host | Guest contract | Behavior |
| --- | --- | --- | --- |
| Wasmtime | 49.0.3 to 49.0.4 | Go-built WASIp1 core module | Command/probe, exact exit code and a held process. |
| Spin | 4.2.2 to 4.2.3 | Rust `wasm32-wasip2` component with pinned Spin SDK 7 | HTTP responses and explicit mounted dependency files. |

The two guest formats are intentionally different. A Go WASIp1 command is not
automatically a Spin HTTP component. The pinned Spin SDK determines its HTTP
interface contract (including its WASI dependencies); the Rust target name alone
is not a promise that arbitrary WASIp2/WASIp3 interfaces will work.
An attempted Go/Wagi recipe trapped in Spin's preview adapter during Go startup;
it remains failed, not relabeled as component-model proof.

## Build the payload

Run from the current source checkout with Go and the reviewed Rust 1.97.1
toolchain/`wasm32-wasip2` target installed. The committed Cargo lock pins the
Spin component's dependency graph. Build outside Brewlet core:

```sh
WORK="$(mktemp -d)"
mkdir "$WORK/wasmtime-app" "$WORK/spin-app" "$WORK/dependencies"
chmod 0755 "$WORK/wasmtime-app" "$WORK/spin-app" "$WORK/dependencies"
GOOS=wasip1 GOARCH=wasm CGO_ENABLED=0 go build -trimpath \
  -o "$WORK/command.wasm" ./examples/wasm/guest
install -m 0644 "$WORK/command.wasm" "$WORK/wasmtime-app/guest.wasm"
cargo +1.97.1 build --locked --release \
  --manifest-path examples/wasm/spin-component/Cargo.toml \
  --target wasm32-wasip2 --target-dir "$WORK/spin-target"
install -m 0644 "$WORK/spin-target/wasm32-wasip2/release/brewlet_spin_example.wasm" \
  "$WORK/spin-app/guest.wasm"
install -m 0644 examples/wasm/spin.toml "$WORK/spin-app/spin.toml"
install -m 0644 examples/wasm/dependencies/message.txt "$WORK/dependencies/message.txt"
```

The core-module and component outputs are different artifacts, not one binary
being relabeled. The fixture guest deliberately checks declared dependency,
environment and filesystem boundaries.

## Launch descriptor

Infrastructure approves an engine runtime ID, source digest and numeric
identity. Applications package `guest.wasm`, optional host configuration and
dependencies under `/app`. Native workloads still select one approved runtime
ID and the Brewlet RuntimeClass.

For example, the Wasmtime descriptor explicitly states:

```json
{
  "schemaVersion": 2,
  "process": {
    "entrypoint": [
      "/usr/local/bin/wasmtime"
    ],
    "args": [
      "run",
      "--dir",
      "/app/dependencies",
      "--dir",
      "/work",
      "--env",
      "VISIBLE=explicit",
      "/app/guest.wasm",
      "hold",
      "main"
    ],
    "workingDir": "/app",
    "env": [
      {
        "name": "XDG_CACHE_HOME",
        "value": "/cache/compiled"
      }
    ]
  },
  "runtimeImages": [
    "wasmtime-wasip1"
  ]
}
```

`runtimeImages` is an optional artifact allowlist, not a selector. The Pod
separately names `brewlet.sh/runtime-image: wasmtime-wasip1`. The removed
`requires` wrapper is rejected.

The qualification driver uses a long-running command Pod and separate
run-to-completion Pods for exit behavior. Ordinary Jobs are a deployment option;
this fixture does not claim a separate Job-controller test.

Spin's full descriptor is in
[`spin-launch.json`](../examples/wasm/spin-launch.json). It explicitly invokes
the local manifest with a listener, separate cache/state/temp locations and
direct file mounts. There is no build or registry resolution at startup.

## Package

Use the matching CLI and a suitable Linux producer filesystem, with
[registry access](registry-trust.md) configured. A reusable independent
dependency set is composed into both application images:

```sh
brewlet layer-set example.invalid/wasm-dependencies:v1 \
  --store "$WORK/dependency-set" --config examples/wasm/layerset.json \
  --layer "dependency=$WORK/dependencies:/app/dependencies"
brewlet push registry.example.com/apps/wasmtime-demo:v1 \
  --platform linux/arm64 --launch examples/wasm/wasmtime-launch.json \
  --layer-set "$WORK/dependency-set" --layer "app=$WORK/wasmtime-app:/app" \
  --push-result "$WORK/wasmtime-push.json"
brewlet push registry.example.com/apps/spin-demo:v1 \
  --platform linux/arm64 --launch examples/wasm/spin-launch.json \
  --layer-set "$WORK/dependency-set" --layer "app=$WORK/spin-app:/app" \
  --push-result "$WORK/spin-push.json"
```

Replace the registry and declare actual supported Linux host platforms.
The qualified IDs are explicitly constrained by these descriptors; another
compatible administrator-owned ID must be deliberately listed or the optional
allowlist omitted. Do not infer runtime selection from the guest format.

## Native deployment

The platform must approve corresponding host images first; see
[runtime source provenance](#runtime-source-provenance).
Here is the Wasmtime command-Pod template. Replace the image digest, namespace,
ID/platform choices and illustrative resources before applying:

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: wasmtime-demo
  namespace: apps
  annotations:
    brewlet.sh/runtime-image: wasmtime-wasip1
spec:
  runtimeClassName: brewlet
  restartPolicy: Never
  nodeSelector: {kubernetes.io/arch: arm64}
  containers:
    - name: app
      image: registry.example.com/apps/wasmtime-demo@sha256:<application-digest>
      securityContext: {readOnlyRootFilesystem: true}
      resources:
        requests: {cpu: 100m, memory: 64Mi}
        limits: {cpu: "1", memory: 512Mi}
      volumeMounts:
        - {name: work, mountPath: /work}
        - {name: cache, mountPath: /cache}
      readinessProbe:
        exec:
          command:
            - /usr/local/bin/wasmtime
            - run
            - --dir
            - /app/dependencies
            - --dir
            - /work
            - --env
            - VISIBLE=explicit
            - /app/guest.wasm
            - probe
            - two words
  volumes:
    - {name: work, emptyDir: {}}
    - {name: cache, emptyDir: {}}
```

The command's default `hold main` invocation remains alive until normal Pod
termination or its explicit fixture release. For Spin HTTP, use
`spin-http-sdk7`, the Spin application digest and an HTTP probe at `/` on port
8000, preserving the two volume mounts. The same Pod spec can be placed under
a normal Deployment template for service operation; the qualification receipt
uses direct Pods rather than claiming a separate Wasm controller test.

## Verify

For the Wasmtime template in the intended context:

```sh
kubectl -n apps wait --for=condition=Ready pod/wasmtime-demo --timeout=5m
kubectl -n apps exec wasmtime-demo -- /usr/local/bin/wasmtime run \
  --dir /app/dependencies --dir /work --env VISIBLE=explicit \
  /app/guest.wasm probe "two words"
```

Expect `message: Hello from Wasm`, the actual dependency content and true
filesystem/cache exclusion checks, with the two probe arguments preserved.
For Spin, verify the actual guest HTTP response, not only the engine's health
endpoint. Inspect the [leased runtime binding](observability.md#logs-and-actual-bindings)
and host version using an explicit engine `--version` Exec.

Use the isolated qualification below to reproduce source rotation, retained
leases, restart recovery and normal cleanup. Producer success alone does not
establish any of these node behaviors.

## Pitfalls and qualification limits

This guide qualifies the named command/component/host combinations, not every
WASI import, package or interface. Keep native host platforms, guest capabilities
and runtime-version compatibility explicit. The additional boundaries below
are specific to Wasm hosts; common [security](security.md) and
[resource](resources.md) rules still apply.

## Guest access is not host access

The Linux host receives the effective Pod/runtime environment. Wasmtime
forwards only the explicit guest variable and preopens. Spin uses an explicit
local manifest, declared file mapping, empty outbound-host allowlist and the
standard SDK HTTP component executor. No registry source or guest build is
resolved at startup.

The guest actually reads its dependency file, cannot read the runtime's
`/etc/runtime-sentinel`, cannot modify its packaged dependency and does not see
the artificial host-only environment variable. These filesystem/environment
tests are not a complete guest-network security audit.

`kubectl exec` starts another Linux host process; it is not a shell inside a
Wasm instance. The examples use explicit engine/probe invocations and HTTP,
not an inferred shell. Numeric host UID/GID, capabilities and noNewPrivileges
are inspected from actual process evidence, not fabricated WASI user APIs.

## Caches and immutable roots

Both engines, including Spin's embedded Wasmtime compiler, need a writable
compilation-cache location. The descriptor sets `XDG_CACHE_HOME=/cache/compiled`.
Spin additionally receives explicit asset-cache, state and temporary paths.
`/cache` and `/work` are separate per-Pod emptyDirs; `/app` and the selected
runtime remain read-only. The guest receives `/work` only where required, never
the engine's `/cache`. Both guests prove the host cache is inaccessible.
Do not put an engine's trusted compiled-code cache inside guest-writable
preopens. No global shared JIT cache or externally supplied precompiled/AOT
artifact workflow is introduced.

## Runtime source provenance

These are **custom platform-built fixture images**, not unchanged upstream
engine OCI images. They combine a pinned distroless C/C++ userland with
checksum-verified, unchanged upstream release executable bytes. The Wasmtime
binary needs the GNU/Linux loader and unwinder; a bare executable-only root
or an incomplete musl image is not sufficient.

The [input matrix](https://github.com/microsoft/brewlet/blob/brewlet-2.0/examples/wasm/runtimes.json)
pins native amd64/arm64 sources. The fixture imports verified base blobs,
appends the exact release binary and records archive/binary/source identities.
It never executes runtime code during root inspection or weakens set-ID,
capability, platform or ownership policy.

## Run the isolated qualification

From the current source checkout, use Python 3.12+, Go, Docker Linux containers,
kind v0.33.0, kubectl and Helm. The command is built with
`GOOS=wasip1 GOARCH=wasm` using Go. Spin's component uses Rust **1.97.1** and the
`wasm32-wasip2` target, Spin SDK **7.0.0** and the committed Cargo lock.
Install that explicit target through your reviewed Rust toolchain setup; the
driver does not replace an unsupported guest with another interface silently.
No TinyGo, Java, Node or .NET SDK is needed.

```sh
PYTHONDONTWRITEBYTECODE=1 python3 integration-tests/live-v2/wasm.py \
  --work integration-tests/live-v2/run-my-wasm-example
```

Use a new work directory below that folder. The driver creates its own cluster,
kubeconfig and images. It composes a real independent dependency layer set,
preimports OCI graphs, approves both roots and performs actual execution.
Preimport is not remote registry TLS/authentication proof.

Qualification requires source-version rotation without application rebuilding,
old lease retention, same-generation Exec/probes, a real held guest across
worker/containerd restarts, allowlist rejection and normal profile/host cleanup
before exact-owned fixture destruction. Failed attempts remain failed.
The [public qualification receipt](../acceptance/results/wasm-runc-arm64.json)
records pinned engine/userland inputs and both compiled guest identities.
Managed dependency composition is tested for these two examples; this does not
close the separate four-native-language/unchanged-runtime layer-set matrix.

For your own deployment, build and publish reviewed runtime/application images,
use the [installation](installation.md) and [trust](registry-trust.md) guides,
mount workload-local writable state and retain explicit native platform
constraints. Portable Wasm bytes do not make the Linux engine architecture-free.

See [Wasmtime CLI options](https://docs.wasmtime.dev/cli-options.html),
[Spin HTTP/Wagi](https://spinframework.dev/v4/http-trigger) and
[the execution contract](concepts.md).
