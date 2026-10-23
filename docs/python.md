# Python workloads

## How it works

Brewlet separates your Python application from platform-managed runtime
infrastructure. You publish source and an explicit launch descriptor as an OCI
artifact, without bundling a Python interpreter. The platform approves and
provisions a [runtime image](runtime-images.md) containing Python and its Linux
userland; your Kubernetes Deployment selects its runtime ID. Brewlet runs your
declared interpreter command with application files read-only under `/app`.

Resolve dependencies in your build pipeline, not on the node. They can ship with
the application or optionally come from an approved
[managed bundle](managed-composition.md). Release-time composition joins your
source and a locked dependency set into a verifiable application artifact.
Compatible package updates can preserve your source, but must be tested against
the selected Python ABI, native libraries and import layout. Brewlet does not
run pip or infer an interpreter.

Updates use new task instances rather than modifying a running Python process.
See the [architecture overview](concepts.md) and
[launch contract](reference.md#launch-descriptor) for the complete workflow.
The runtime root is read-only too; temporary files and caches need
[approved writable volumes](resources.md#probes-exec-files-and-shutdown).

## Tested runtime inputs

The unchanged-runtime companion exercised pinned distroless Python sources
using `/usr/bin/python3.13` on arm64. The full-userland suite separately used
adapted Docker Official Python 3.13.5 and 3.13.6 roots.
See the [upstream](../integration-tests/fixtures/runtimes/upstream.json) and
[full-userland](../integration-tests/fixtures/runtimes/rotation.json) matrices.
Historical test versions are not security recommendations.

Confirm the actual path/platforms with the platform; `python-3.13` below is an
example approved ID. For a first end-to-end deployment without dependencies,
the [local tutorial](local-kubernetes.md) is the smaller path.

## Build the payload

This stdlib HTTP fixture imports a real separate `greeting.py` dependency.
No pip installation is needed:

```sh
WORK="$(mktemp -d)"
mkdir "$WORK/src" "$WORK/dependencies"
chmod 0755 "$WORK/src" "$WORK/dependencies"
install -m 0644 integration-tests/fixtures/python/src/main.py "$WORK/src/main.py"
install -m 0644 integration-tests/fixtures/python/dependencies/greeting.py "$WORK/dependencies/greeting.py"
```

For your own project, resolve a reviewed lock into the intended dependency
directory before packaging. Build/install native wheels for the actual target
Python ABI, Linux libc and CPU architecture, not the producer host by accident.
Do not copy a host virtualenv and assume its interpreter paths are portable.

## Launch descriptor

Save as `$WORK/launch.json`:

```json
{
  "schemaVersion": 2,
  "process": {
    "entrypoint": [
      "/usr/bin/python3.13"
    ],
    "args": [
      "/app/src/main.py"
    ],
    "workingDir": "/app",
    "env": [
      {
        "name": "PYTHONPATH",
        "value": "/app/dependencies"
      },
      {
        "name": "PYTHONDONTWRITEBYTECODE",
        "value": "1"
      }
    ]
  }
}
```

`PYTHONPATH` configures this explicit dependency layout, not executable lookup.
PATH remains runtime-owned. Disabling bytecode writes suits the read-only
payload; other application writes need a separately mounted destination.

## Package

Use the matching CLI on a suitable Linux producer filesystem. Follow the
[producer setup](getting-started.md) for macOS rather than dropping filesystem
validation. Replace the registry and declare your actual platform:

```sh
brewlet build registry.example.com/apps/python-demo:v1 \
  --platform linux/arm64 \
  --launch "$WORK/launch.json" \
  --layer "dependency=$WORK/dependencies:/app/dependencies" \
  --layer "app=$WORK/src:/app/src"
brewlet push registry.example.com/apps/python-demo:v1 --push-result "$WORK/push.json"
```

For reusable independent dependency publication, see [layer sets](dependency-layer-sets.md).
It is a producer composition workflow, not runtime pip installation.

## Native deployment

Replace the digest, ID, namespace and architecture using the platform handoff.
These resources are illustrative; configure [registry access](registry-trust.md)
and choose measured resource sizes for your workload.

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: python-demo
  namespace: apps
spec:
  replicas: 1
  selector:
    matchLabels: {app: python-demo}
  template:
    metadata:
      labels: {app: python-demo}
      annotations:
        brewlet.sh/runtime-image: python-3.13
    spec:
      runtimeClassName: brewlet
      nodeSelector: {kubernetes.io/arch: arm64}
      containers:
        - name: app
          image: registry.example.com/apps/python-demo@sha256:<application-digest>
          securityContext: {readOnlyRootFilesystem: true}
          resources:
            requests: {cpu: 100m, memory: 64Mi}
            limits: {cpu: "1", memory: 256Mi}
          readinessProbe:
            httpGet: {path: /, port: 8000}
```

HTTP probes do not require a shell. If an Exec probe is preferable, name the
interpreter and full script arguments explicitly. Apply shared
[non-root and volume policy](applications.md#native-workload-example).

## Verify

In the intended cluster/context:

```sh
kubectl -n apps rollout status deployment/python-demo --timeout=5m
kubectl -n apps exec deployment/python-demo -- /usr/bin/python3.13 \
  /app/src/main.py --probe "two words"
kubectl -n apps port-forward deployment/python-demo 18080:8000
```

From another terminal, `curl --fail http://127.0.0.1:18080/` should report
`language: python`, the interpreter version and `dependency: shared-contract`.
The Exec report must retain the two distinct arguments. Check its numeric
identity and [actual leased runtime](observability.md#logs-and-actual-bindings),
then stop the port-forward when finished.

## Pitfalls and qualification limits

- Python source portability does not make native extension wheels portable across CPython ABIs, libc or CPU architectures.
- HOME is metadata, not a writable-directory guarantee. Libraries using temporary/cache files need explicit writable volumes/configuration.
- Never install dependencies into read-only `/app` during startup, add a shell assumption or use a Pod PATH override as an interpreter fallback.
- Python signed layer-set publication/trust/reuse has registry integration evidence. The unchanged-runtime node run uses separate dependency layers; it is not the combined managed-set/unchanged-runtime matrix.
- Physical local qualification is arm64. The fixture HTTP server is an evaluation example, not a production Python web-server recommendation.

For application dependency changes, recompose and roll the application.
A [runtime-only update](operations.md#rotate-without-rebuilding-application-bytes)
does not update packaged Python libraries.
