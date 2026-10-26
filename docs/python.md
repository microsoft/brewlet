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

## Tested runtimes

Brewlet's tests run Python on unmodified distroless Python 3.13 images (arm64,
using `/usr/bin/python3.13`) and on Docker Official Python 3.13 images. The
exact images are listed in the [upstream runtime list](https://github.com/microsoft/brewlet/blob/main/integration-tests/fixtures/runtimes/upstream.json) and
[rotation runtime list](https://github.com/microsoft/brewlet/blob/main/integration-tests/fixtures/runtimes/rotation.json). They are test inputs, not a security
recommendation; use the runtime images your platform team approves.

Python is Brewlet's qualification fixture: pull request E2E, full validation
and promotion run it on real nodes (see [verification](verification.md)). It
also runs daily against `main` in the non-gating [Python example workflow](https://github.com/microsoft/brewlet/blob/main/.github/workflows/language-python.yml).

Ask your platform team for the runtime ID, supported platforms and interpreter
path. `python-3.13` below is an example ID. For a smaller first deployment
without dependencies, follow the [local tutorial](local-kubernetes.md).

## Build the payload

This example uses the small Python test application from the Brewlet
repository. It is a standard-library HTTP server that imports a separate
`greeting.py` dependency, so no pip installation is needed:

```sh
WORK="$(mktemp -d)"
git clone --quiet --depth 1 --branch v0.8.1 https://github.com/microsoft/brewlet.git "$WORK/brewlet"
FIXTURE="$WORK/brewlet/integration-tests/fixtures/python"
mkdir "$WORK/src" "$WORK/dependencies"
chmod 0755 "$WORK/src" "$WORK/dependencies"
install -m 0644 "$FIXTURE/src/main.py" "$WORK/src/main.py"
install -m 0644 "$FIXTURE/dependencies/greeting.py" "$WORK/dependencies/greeting.py"
```

For your own project, install your locked dependencies into the dependency
directory before packaging (for example with `pip install --target`). Native
wheels must match the target Python version, Linux libc and CPU architecture,
not your build machine.
Do not copy a host virtualenv and assume its interpreter paths are portable.

## Launch descriptor

Save as `$WORK/launch.json`:

```json
{
  "schemaVersion": 1,
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

Files must be world-readable and directories searchable (see
[producer filesystem requirements](applications.md#producer-filesystem), which
also covers macOS). Replace the registry and declare your actual platform:

```sh
brewlet build registry.example.com/apps/python-demo:v1 \
  --platform linux/arm64 \
  --launch "$WORK/launch.json" \
  --layer "dependency=$WORK/dependencies:/app/dependencies" \
  --layer "app=$WORK/src:/app/src"
brewlet push registry.example.com/apps/python-demo:v1 --push-result "$WORK/push.json"
```

To publish dependencies once and share them across applications, see
[dependency layer sets](dependency-layer-sets.md). Dependencies are still
resolved at build time, never installed on the node.

## Native deployment

Use the digest from `push.json`. The following is a template: replace the
image digest, runtime ID, namespace and architecture with values from your
platform team. Configure [registry access](registry-trust.md) and size
resources for your workload.

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

HTTP probes do not require a shell. For an Exec probe, name the interpreter
and full script arguments explicitly. The
[common workload rules](applications.md#native-workload-example) for non-root
identity and volumes apply.

## Verify

After applying the manifest:

```sh
kubectl -n apps rollout status deployment/python-demo --timeout=5m
kubectl -n apps exec deployment/python-demo -- /usr/bin/python3.13 \
  /app/src/main.py --probe "two words"
kubectl -n apps port-forward deployment/python-demo 18080:8000
```

From another terminal, `curl --fail http://127.0.0.1:18080/` should report
`language: python`, the interpreter version and `dependency: shared-contract`.
The Exec output shows `--probe` and `two words` as two distinct arguments and
the non-root numeric user. To see which runtime image generation the Pod
actually uses, check the
[actual runtime binding](observability.md#logs-and-actual-bindings). Stop the
port-forward when finished.

## Pitfalls and limits

- Python source portability does not make native extension wheels portable across CPython ABIs, libc or CPU architectures.
- HOME is metadata, not a writable-directory guarantee. Libraries using temporary/cache files need explicit writable volumes/configuration.
- Never install dependencies into read-only `/app` at startup, assume a shell exists, or set PATH in the Pod to find the interpreter (Pod PATH is ignored).
- The example HTTP server is for evaluation, not a production Python web server.

When application dependencies change, rebuild (or recompose) the application
and roll it out.
A [runtime-only update](operations.md#rotate-without-rebuilding-application-bytes)
does not update packaged Python libraries.
