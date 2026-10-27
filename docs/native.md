# Native workloads

## How it works

Brewlet separates a native application from its platform-managed Linux
userspace. You publish a compiled binary and an explicit launch descriptor as
an OCI artifact, without bundling a base OS image. The platform approves and
provisions a [golden runtime image](runtime-images.md), and your Kubernetes
Deployment selects it by runtime ID. Brewlet launches your executable directly
from read-only `/app`; the node does not need your build toolchain.

This model applies to Go, Rust, C, C++, Zig, Swift, Fortran, .NET Native AOT
and GraalVM Native Image applications. The contract is the compiled Linux
executable and its userspace requirements, not the source language. No
separate interpreter or VM is needed for these native executables, but some
builds still depend on a dynamic loader, shared libraries or external data.

The workflow is the same across these toolchains: compile for your target
platform, package the executable and required application files under `/app`,
declare the command and select a compatible golden image. The worked example
below uses a Rust HTTP server to illustrate that language-neutral flow.

## Why a golden image still matters

A native executable does not necessarily contain its entire execution
environment. Even without a separately installed language runtime, a
platform-managed golden image gives you:

- **A governed userspace:** platform teams approve digest-pinned images,
  numeric execution identities and allowed volume locations independently
  of application releases.
- **Shared system assets:** applications can use centrally maintained CA
  certificates, timezone data and account metadata instead of shipping their
  own copies. An application must actually use those external assets to
  benefit from their updates.
- **Independent maintenance:** compatible changes to image-supplied files
  reach replacement Pods without recompiling or republishing application
  binaries. Dynamically linked applications can also consume platform-managed
  loaders and shared libraries, subject to their ABI requirements.
- **Controlled placement and rotation:** approved environments are offered on
  selected node pools, and runtime bindings identify which generation each
  task uses.

The boundary matters: compiled-in language runtimes, standard libraries,
application dependencies and statically linked libraries remain part of the
application binary. Examples include Go's runtime, Rust crates and runtime
code embedded by AOT compilation. Fixing those requires an
application rebuild. A golden-image rotation cannot patch them. A completely
self-contained binary has less to gain from independently maintained system
assets, but still uses the platform's approval, identity and placement policy.
Ordinary OCI images already share identical layers; smaller downloads alone
are not the reason to adopt this model.

The golden image supplies container userspace, not a private kernel or a node
OS upgrade. Running tasks retain their selected generation until replaced;
rotation is not live patching.

## Choose the runtime and build platform

Ask your platform team for an approved runtime ID and supported architectures.
Choose the root from the binary's actual requirements:

- **Statically linked applications** can use a minimal root, such as a
  platform-approved distroless static image, with any external data they need.
- **Dynamically linked applications** need a root containing the expected
  loader, compatible libc and every required shared library. Check the
  compiler target, library versions and ABI rather than choosing an image
  solely by source language.

In either case, build a Linux executable for the node architecture and package
all application-owned assets. Use your toolchain's normal release process;
Brewlet does not compile, relink or discover native dependencies. A macOS
executable cannot run on a Linux node. An explicit absolute entrypoint needs
no runtime shell or PATH.

Brewlet already tests static binaries with a distroless static root on arm64
(see the [workload overview](language-recipes.md)); that is not qualification
of every native application or upstream image. Follow your platform's validation
process before approving an image.

## Worked example: Rust HTTP server

This example uses only Rust's standard library and is statically linked for
Linux arm64. It selects the example runtime ID `native-static`. Other
native applications follow the same packaging and deployment steps once their
compatible Linux payload is built.

For this example, you need Docker for the Linux Rust build, the Brewlet CLI
and access to a Brewlet-enabled Kubernetes pool. For amd64, change Docker's
platform to `linux/amd64`, the Brewlet platform flag and the Deployment node
selector together.

### Build the Rust payload

Create a small HTTP server using Rust's standard library:

```sh
WORK="$(mktemp -d)"
mkdir -p "$WORK/rust-src" "$WORK/rust-app/bin"
chmod 0755 "$WORK/rust-app" "$WORK/rust-app/bin"

cat > "$WORK/rust-src/main.rs" <<'EOF'
use std::io::{self, Read, Write};
use std::net::TcpListener;

fn main() -> io::Result<()> {
    let args: Vec<String> = std::env::args().skip(1).collect();
    if args.first().map(String::as_str) == Some("--probe") {
        println!("{args:?}");
        return Ok(());
    }
    let listener = TcpListener::bind("0.0.0.0:8000")?;
    let body = "{\"language\":\"rust\",\"message\":\"Hello from Rust\"}\n";
    for stream in listener.incoming() {
        let mut stream = stream?;
        let mut request = [0; 4096];
        if stream.read(&mut request)? == 0 {
            continue;
        }
        write!(
            stream,
            "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\n\
             Content-Length: {}\r\nConnection: close\r\n\r\n{}",
            body.len(),
            body
        )?;
    }
    Ok(())
}
EOF

docker run --rm --platform linux/arm64 \
  --mount "type=bind,src=$WORK/rust-src,dst=/source" \
  --workdir /source rust:alpine \
  rustc --edition=2021 -C opt-level=2 -C target-feature=+crt-static \
    main.rs -o server
install -m 0755 "$WORK/rust-src/server" "$WORK/rust-app/bin/server"
```

The Alpine builder uses a Linux musl target; `+crt-static` explicitly selects
static C runtime linking. The builder is not the deployed runtime image.
Pin your approved builder version or digest in a repeatable build pipeline.
For a real Cargo project, resolve locked dependencies at build time and check
the resulting binary's native-library requirements; not every crate supports
this static build.

## Launch descriptor

For any native workload, name the packaged executable directly and supply its
arguments, working directory and environment explicitly. This example places
its executable at `/app/bin/server` and uses this descriptor:

```sh
cat > "$WORK/launch.json" <<'EOF'
{
  "schemaVersion": 1,
  "process": {
    "entrypoint": [
      "/app/bin/server"
    ],
    "workingDir": "/app"
  }
}
EOF
```

Unlike an interpreter-based workload, the executable comes from your
application layer, not the golden image. Its absolute path works even when
the approved image has no PATH. Brewlet does not infer a command or run a
shell.

## Package

Replace the registry with your own and configure
[registry access](registry-trust.md). Files must be world-readable,
executables executable and directories searchable; see the
[producer filesystem requirements](applications.md#producer-filesystem),
including the macOS guidance.

```sh
brewlet build registry.example.com/apps/rust-demo:v1 \
  --platform linux/arm64 \
  --launch "$WORK/launch.json" \
  --layer "app=$WORK/rust-app:/app"
brewlet push registry.example.com/apps/rust-demo:v1 \
  --push-result "$WORK/rust-push.json"
```

Package any application-owned configuration, content or separately loaded
dependencies alongside your executable. The example binary has no separate
dependency directory. Libraries compiled into an executable cannot be replaced
with a [dependency layer set](dependency-layer-sets.md); external data files or
separately loaded dependencies can use that mechanism when appropriate.

## Approve the golden image

The platform team approves a compatible root by digest and offers it on a pool.
For the statically linked example:

```yaml
apiVersion: node.brewlet.sh/v1alpha1
kind: RuntimeImage
metadata:
  name: native-static
spec:
  source:
    image: registry.example.com/golden/native-static@sha256:<approved-digest>
  user: {uid: 65532, gid: 65532}
```

This is a template, not a built-in runtime catalog. Replace the source with a
validated image and add `native-static` to a NodeProfile's `runtimeImages`, as
described in [runtime images and pools](runtime-images.md). The root must meet
Brewlet's validation rules even if it supplies no language executable.

## Deploy

Save this template as `$WORK/rust-deployment.yaml`. Replace the digest with the
Rust image digest from `rust-push.json`, and use the namespace, runtime ID and
platform supplied by your platform team. Resource values are examples.

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: rust-demo
  namespace: apps
spec:
  replicas: 1
  selector:
    matchLabels: {app: rust-demo}
  template:
    metadata:
      labels: {app: rust-demo}
      annotations:
        brewlet.sh/runtime-image: native-static
    spec:
      runtimeClassName: brewlet
      nodeSelector: {kubernetes.io/arch: arm64}
      containers:
        - name: app
          image: registry.example.com/apps/rust-demo@sha256:<application-digest>
          securityContext: {readOnlyRootFilesystem: true}
          resources:
            requests: {cpu: 100m, memory: 64Mi}
            limits: {cpu: "1", memory: 256Mi}
          readinessProbe:
            httpGet: {path: /, port: 8000}
```

```sh
kubectl apply -f "$WORK/rust-deployment.yaml"
```

The common [workload rules](applications.md#native-workload-example) for
non-root identity, registry credentials and volumes apply. This example
needs neither a writable root nor a shell.

## Verify

Wait for the Deployment and invoke the packaged executable directly:

```sh
kubectl -n apps rollout status deployment/rust-demo --timeout=5m
kubectl -n apps exec deployment/rust-demo -- /app/bin/server --probe "two words"
kubectl -n apps port-forward deployment/rust-demo 18080:8000
```

The Exec output should show `--probe` and `two words` as two distinct
arguments. From another terminal, `curl --fail http://127.0.0.1:18080/` should
return `"language":"rust"` and `"message":"Hello from Rust"`. Stop the
port-forward when finished.

Check the [actual runtime binding](observability.md#logs-and-actual-bindings)
to confirm which golden-image generation the Pod uses. The response identifies
the application, not the golden-image version.

## Update the environment without rebuilding the binary

For a compatible golden-image update, the platform follows the normal
[runtime rotation procedure](operations.md#rotate-without-rebuilding-application-bytes):
approve the replacement source digest under the selected runtime ID, wait for
rotation to complete, then replace the application Pods. Application image
digests can stay unchanged. In this example, the Deployment selects
`native-static`. Running Pods keep the previous generation until replaced.

This updates only image-supplied userspace. Any change to compiled-in code,
whether a language runtime, standard library or application dependency,
requires rebuilding and rolling out the affected application instead.

## Pitfalls and limits

- The server is an evaluation example, not a production HTTP implementation.
  It handles connections sequentially and implements only a minimal HTTP
  response.
- Static linking does not make an executable architecture-independent.
  Match the binary, OCI platform and node architecture.
- Dynamically linked builds require the correct Linux loader, libc and shared
  libraries. A distroless static root is not interchangeable with a glibc or
  musl userspace.
- `/app` and the runtime root are read-only. Applications that write files,
  caches or temporary data need
  [approved writable volumes](resources.md#probes-exec-files-and-shutdown).
- Brewlet does not rebuild binaries, resolve build-time dependencies or certify
  native ABI compatibility for you.
