# WebAssembly workloads

## How it works

Brewlet delivers your WebAssembly application separately from the host engine
that runs it. You publish a `.wasm` guest, any host configuration and an
explicit launch descriptor as an OCI image. The platform approves and
provisions a Linux [runtime image](runtime-images.md) that contains a
compatible host such as Wasmtime or Spin, and your Kubernetes workload selects
it by runtime ID. Brewlet runs that host through the normal containerd/runc
path, with application files mounted read-only under `/app`. It does not
install runwasi or infer the guest's interface contract.

Files that the guest loads at run time can ship with the guest or be published
separately as a [dependency layer set](dependency-layer-sets.md) or
[governed bundle](managed-composition.md). Libraries compiled into the guest
are part of `guest.wasm` and need a guest rebuild. Host-engine fixes belong in
the runtime image.

An update starts new Pods; it never patches a running guest. See
[concepts](concepts.md) and the
[launch descriptor](reference.md#launch-descriptor) for the execution model.
The runtime root is also read-only, so caches and temporary files need
[explicit writable volumes](resources.md#probes-exec-files-and-shutdown).

## Tested hosts and guest formats

| Example | Host | Guest | Behavior |
| --- | --- | --- | --- |
| Wasmtime | 49.0.3 and 49.0.4 | Go-built WASIp1 core module | Command, probe, exact exit code and a long-running process. |
| Spin | 4.2.2 and 4.2.3 | Rust `wasm32-wasip2` component built with Spin SDK 7 | HTTP responses and mounted dependency files. |

The two guest formats are different. A Go WASIp1 command is not a Spin HTTP
component. The Spin SDK version determines the HTTP interface contract; the
Rust target name alone does not promise that arbitrary WASIp2 or WASIp3
interfaces work. Go guests running under Spin's Wagi/preview adapter did not
work in testing.

Both examples have been tested on arm64 and amd64 nodes, and run daily against
`main` in the non-gating [WebAssembly example workflow](https://github.com/microsoft/brewlet/blob/main/.github/workflows/language-wasm.yml).
Other WASI interfaces and native runwasi execution are not covered.

## Build the guests

The examples live in the Brewlet repository under
[`examples/wasm`](https://github.com/microsoft/brewlet/tree/v0.8.1/examples/wasm).
You need Go and Rust 1.97.1 with the `wasm32-wasip2` target. The committed
`Cargo.lock` pins the Spin component's dependencies.

1. Get the example sources and create the layer directories:

    ```sh
    git clone --depth 1 --branch v0.8.1 https://github.com/microsoft/brewlet.git
    cd brewlet
    WORK="$PWD/wasm-work"
    mkdir -p "$WORK/wasmtime-app" "$WORK/spin-app" "$WORK/dependencies"
    chmod 0755 "$WORK/wasmtime-app" "$WORK/spin-app" "$WORK/dependencies"
    ```

2. Build the Wasmtime command module:

    ```sh
    GOOS=wasip1 GOARCH=wasm CGO_ENABLED=0 go build -trimpath \
      -o "$WORK/command.wasm" ./examples/wasm/guest
    install -m 0644 "$WORK/command.wasm" "$WORK/wasmtime-app/guest.wasm"
    ```

3. Build the Spin component and copy its manifest:

    ```sh
    cargo +1.97.1 build --locked --release \
      --manifest-path examples/wasm/spin-component/Cargo.toml \
      --target wasm32-wasip2 --target-dir "$WORK/spin-target"
    install -m 0644 "$WORK/spin-target/wasm32-wasip2/release/brewlet_spin_example.wasm" \
      "$WORK/spin-app/guest.wasm"
    install -m 0644 examples/wasm/spin.toml "$WORK/spin-app/spin.toml"
    ```

4. Copy the shared dependency file:

    ```sh
    install -m 0644 examples/wasm/dependencies/message.txt "$WORK/dependencies/message.txt"
    ```

The guests deliberately check their dependency, environment and filesystem
boundaries and fail if those are violated.

## Write the launch descriptors

The platform approves the engine runtime image and its ID. Your application
packages `guest.wasm`, optional host configuration and dependencies under
`/app`.

The Wasmtime descriptor,
[`wasmtime-launch.json`](../examples/wasm/wasmtime-launch.json):

```json
{
  "schemaVersion": 1,
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

`runtimeImages` is an optional allowlist, not a selector. The Pod still names
its runtime with `brewlet.sh/runtime-image: wasmtime-wasip1`. If your platform
uses different IDs, list them here or omit `runtimeImages`.

The Spin descriptor,
[`spin-launch.json`](../examples/wasm/spin-launch.json), runs
`spin up --from /app/spin.toml` with a listener on `0.0.0.0:8000`, separate
cache, state and temporary locations, and `--direct-mounts`. Nothing is built
or fetched from a registry at startup. Its allowlist is `spin-http-sdk7`.

## Build and push

Run on Linux or macOS with [registry access](registry-trust.md) configured.
This publishes the dependency file once as a local layer set and composes it
into both application images:

```sh
brewlet layer-set example.invalid/wasm-dependencies:v1 \
  --store "$WORK/dependency-set" --config examples/wasm/layerset.json \
  --layer "dependency=$WORK/dependencies:/app/dependencies"

brewlet build registry.example.com/apps/wasmtime-demo:v1 \
  --platform linux/arm64 --launch examples/wasm/wasmtime-launch.json \
  --layer-set "$WORK/dependency-set" --layer "app=$WORK/wasmtime-app:/app"
brewlet push registry.example.com/apps/wasmtime-demo:v1 --push-result "$WORK/wasmtime-push.json"

brewlet build registry.example.com/apps/spin-demo:v1 \
  --platform linux/arm64 --launch examples/wasm/spin-launch.json \
  --layer-set "$WORK/dependency-set" --layer "app=$WORK/spin-app:/app"
brewlet push registry.example.com/apps/spin-demo:v1 --push-result "$WORK/spin-push.json"
```

Replace the registry and list the Linux platforms your hosts actually support.
Portable Wasm bytes do not make the Linux engine architecture-independent.

## Host runtime images

Your platform team must provide and approve a runtime image for each host
before you deploy. The tested hosts are custom images, not unchanged upstream
engine images: a pinned distroless C/C++ userland plus the checksum-verified,
unchanged upstream release binary. The Wasmtime binary needs the GNU/Linux
loader and unwinder, so a bare executable-only root or a minimal musl image is
not enough. The pinned engine releases and base images are listed in
[`runtimes.json`](https://github.com/microsoft/brewlet/blob/v0.8.1/examples/wasm/runtimes.json).

Each RuntimeImage must also declare `volumeMountPaths: ["/cache", "/work"]` so
that the workload's cache and work volumes are admitted. See
[declare mount locations](runtime-images.md#declare-data-and-configuration-mount-locations).

## Deploy

Replace the image digest, namespace, runtime ID, platform and resource values.
This Pod runs the Wasmtime command:

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

The default `hold main` command keeps running until the Pod is terminated. For
Spin, use runtime ID
`spin-http-sdk7`, the Spin image digest and an HTTP readiness probe on `/` port
8000, with the same two volumes. For a long-running service, put the same Pod
spec in a Deployment template.

## Verify

1. For the Wasmtime Pod, wait for readiness and run the probe:

    ```sh
    kubectl -n apps wait --for=condition=Ready pod/wasmtime-demo --timeout=5m
    kubectl -n apps exec wasmtime-demo -- /usr/local/bin/wasmtime run \
      --dir /app/dependencies --dir /work --env VISIBLE=explicit \
      /app/guest.wasm probe "two words"
    ```

    Expect `"message":"Hello from Wasm"`, the dependency file content,
    `"arguments":["probe","two words"]` and `true` for
    `runtimeFileHidden`, `dependencyWriteDenied` and `engineCacheHidden`.

2. For Spin, check the guest's actual HTTP response, not only the engine's
   health.

3. Check the [actual runtime binding](observability.md#logs-and-actual-bindings),
   and run the engine with `--version` through `kubectl exec` to confirm the
   host version.

## Guest access is not host access

The Linux host process receives the Pod's environment. Wasmtime forwards only
the explicit guest variable and preopened directories to the guest. Spin uses
an explicit local manifest, a declared file mapping, an empty outbound-host
allowlist and the standard SDK HTTP executor.

The example guests read their dependency file but cannot read files from the
runtime root, cannot modify their packaged dependency and do not see host-only
environment variables. These checks are not a complete guest network security
audit.

`kubectl exec` starts another Linux host process, not a shell inside the Wasm
instance. Use explicit engine invocations or HTTP for probes. Host UID/GID,
capabilities and `noNewPrivileges` apply to the host process; there is no WASI
user identity. Common [security](security.md) and [resource](resources.md)
rules still apply.

## Caches and immutable roots

Both engines, including Spin's embedded Wasmtime compiler, need a writable
compilation cache. The descriptors set `XDG_CACHE_HOME=/cache/compiled`, and
Spin also gets explicit asset-cache, state and temporary paths. `/cache` and
`/work` are separate per-Pod `emptyDir` volumes; `/app` and the runtime remain
read-only.

The guest receives `/work` only where needed and never the engine's `/cache`.
Do not put an engine's compiled-code cache inside a guest-writable directory.
Brewlet provides no shared JIT cache or precompiled/AOT artifact workflow.

## Governed external dependency replacement

Wasm workloads use the same [governed composition](managed-composition.md)
flow as other languages: `brewlet layer-set` publishes a signed, locked
dependency bundle, `brewlet bundle` combines it with an app-only image,
`brewlet verify` checks the result and `brewlet inventory` traces running
workloads back to it. There is no Wasm-specific composer.

In the examples, the external dependency is `message.txt`. To update it:

1. The platform publishes the changed `message.txt` as a new signed layer set.
2. `brewlet bundle` replaces only the dependency layers. The guest bytes,
   application layers, launch descriptor and selected runtime stay the same.
3. Test the new candidate image, then roll it out. Existing Pods keep the old
   dependency until they are replaced.
4. Use `brewlet inventory` to confirm which dependency set the running Pods
   use.

This updates an external data file only. A Rust or Go library compiled into
`guest.wasm` cannot be fixed by swapping a layer set; rebuild the guest and
publish a new application image. Update the RuntimeImage for engine or OS
fixes. Layer-set SBOMs do not cover code compiled into the guest or the
runtime image.

See [Wasmtime CLI options](https://docs.wasmtime.dev/cli-options.html),
[Spin HTTP trigger](https://spinframework.dev/v4/http-trigger) and
[concepts](concepts.md).
