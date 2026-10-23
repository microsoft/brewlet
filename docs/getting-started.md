# Getting started

Brewlet 1.0.0 is a **source-built evaluation**. Do not use Brewlet 0.x's released installer,
old artifacts or `JavaApplication` manifests for this branch. Use a disposable
local environment; provisioning modifies the selected node.

For the shortest path to a running service, use
[your first local Kubernetes deployment](local-kubernetes.md). It needs no
Java/Maven/Node/.NET SDK and demonstrates actual HTTP, runtime rotation and
normal cleanup. The complete qualification harness below is optional
maintainer verification, not the primary onboarding path.

## Build the CLI

Use Go 1.26 or newer and Git. Maven verification additionally needs Maven and
a JDK; the complete fixture suite also requires Node.js and the .NET SDK.

```sh
git clone --branch main https://github.com/microsoft/brewlet.git
cd brewlet
make build
```

On macOS, the producer builds locally, but the shim and node worker execute on
Linux. Application sources with filesystem extended attributes are rejected by
artifact policy; use the existing Linux producer environment rather than
stripping validation checks.

## Package a minimal application

Create `src/main.py`:

```python
from http.server import SimpleHTTPRequestHandler, ThreadingHTTPServer

ThreadingHTTPServer(("0.0.0.0", 8000), SimpleHTTPRequestHandler).serve_forever()
```

Create `launch.json`:

```json
{
  "schemaVersion": 1,
  "process": {
    "entrypoint": [
      "python3"
    ],
    "args": [
      "/app/src/main.py"
    ],
    "workingDir": "/app"
  }
}
```

Declare only platforms supported by your application and approved runtime.
This example writes a local OCI layout; it does not deploy or publish anything:

```sh
./bin/brewlet push demo/api:v2 \
  --store ./oci \
  --platform linux/arm64 \
  --launch launch.json \
  --layer app=./src:/app/src

./bin/brewlet inspect ./oci
```

Use regular world-readable files and searchable directories. The packager
rejects unsafe modes, extended attributes and escaping paths instead of
silently sanitizing them.

For remote publication, replace the reference with your repository and omit
`--store`. Keep the resulting digest for the separate deployment stage. See
[application packaging](applications.md).

## Evaluate the complete local runtime

The repository provides an isolated kind-based harness. It builds this checkout's
components, creates invocation-owned nodes/registries and exercises actual
provisioning, application execution and cleanup.

Prerequisites include Docker with sufficient free storage, kind **v0.33.0**,
kubectl, Helm, OpenSSL, Go, Java/Maven, Node.js and the pinned .NET fixture SDK.
The harness checks pins and prerequisites rather than skipping mandatory
assertions. The complete suite can take substantial time and disk space.

```sh
PYTHONDONTWRITEBYTECODE=1 python3 integration-tests/live-v2/harness.py \
  --work integration-tests/live-v2/run-my-evaluation
```

Use a new work directory for each invocation. The harness does not accept your
shared cluster or current kube context. Its result explicitly distinguishes
selected-suite success from aggregate acceptance.

Consult the [full live runbook](https://github.com/microsoft/brewlet/blob/main/integration-tests/live-v2/README.md)
for exact tool/input pins and smaller, explicitly scoped scenarios.

## Configure your own disposable pool

Validate a vendor runtime before approval; compatible upstream and distroless
images can be used unchanged. Platforms, numeric identity, optional PATH and
account metadata, application isolation and privilege restrictions still apply.
Read [runtime images and pools](runtime-images.md) before installing components
or authoring a `NodeProfile`.
