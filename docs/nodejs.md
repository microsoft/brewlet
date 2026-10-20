# Node.js workloads

Package resolved JavaScript/dependency files and an explicit invocation. The
approved runtime provides Node and userland; Brewlet does not run npm, discover
a package entrypoint or rewrite module resolution.

## Tested runtime inputs

Unchanged distroless Node 22 roots were qualified on arm64 with
`/nodejs/bin/node`, a path outside their ordinary upstream PATH.
The full-userland suite separately exercised adapted Docker Official Node
22.17.0 and 22.18.0 roots. Exact digests are in the
[upstream](../integration-tests/fixtures/runtimes/upstream.json) and
[full-userland](../integration-tests/fixtures/runtimes/rotation.json) matrices.
The pinned historical versions are not a current security recommendation.

Use the path and platforms supplied by the platform handoff. `node-22` below
is an example approved ID, not an inferred Node version selector.

## Build the payload

The qualification fixture uses ES modules. Its `main.mjs` imports
`../dependencies/greeting.mjs`, so the two layer destinations preserve that
relative relationship:

```sh
WORK="$(mktemp -d)"
mkdir "$WORK/src" "$WORK/dependencies"
chmod 0755 "$WORK/src" "$WORK/dependencies"
install -m 0644 integration-tests/fixtures/node/src/main.mjs "$WORK/src/main.mjs"
install -m 0644 integration-tests/fixtures/node/dependencies/greeting.mjs "$WORK/dependencies/greeting.mjs"
```

No npm installation is needed for this fixture. For an actual npm project,
resolve its reviewed lock outside Brewlet and package the resulting supported
layout. Native addons and install-time builds must target the selected Linux
ABI, Node ABI and architecture.

## Launch descriptor

Save as `$WORK/launch.json`:

```json
{
  "schemaVersion": 2,
  "process": {
    "entrypoint": [
      "/nodejs/bin/node"
    ],
    "args": [
      "/app/src/main.mjs"
    ],
    "workingDir": "/app"
  }
}
```

This fixture does not need `NODE_PATH`: the import is explicit and relative.
Do not use `NODE_PATH` as a general ES-module resolution mechanism. A CommonJS
project may use its documented resolution configuration, while an ESM package
typically needs the correct package/import layout or an explicit build output.

## Package

Use a matching CLI on a Linux producer filesystem, following
[producer setup](getting-started.md) if working on macOS. Replace the registry:

```sh
brewlet push registry.example.com/apps/node-demo:v1 \
  --platform linux/arm64 \
  --launch "$WORK/launch.json" \
  --layer "dependency=$WORK/dependencies:/app/dependencies" \
  --layer "app=$WORK/src:/app/src" \
  --push-result "$WORK/push.json"
```

These are separate dependency/app layers. For an independently published
dependency graph, use [layer-set composition](dependency-layer-sets.md);
the shim never resolves npm packages.

## Native deployment

Replace every image/ID/platform/namespace choice using your platform handoff;
do not apply the placeholder unchanged. Resource values are examples.

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: node-demo
  namespace: apps
spec:
  replicas: 1
  selector:
    matchLabels: {app: node-demo}
  template:
    metadata:
      labels: {app: node-demo}
      annotations:
        brewlet.sh/runtime-image: node-22
    spec:
      runtimeClassName: brewlet
      nodeSelector: {kubernetes.io/arch: arm64}
      containers:
        - name: app
          image: registry.example.com/apps/node-demo@sha256:<application-digest>
          securityContext: {readOnlyRootFilesystem: true}
          resources:
            requests: {cpu: 100m, memory: 64Mi}
            limits: {cpu: "1", memory: 256Mi}
          readinessProbe:
            httpGet: {path: /, port: 8000}
```

Use normal [registry credentials, non-root defaults and explicit volumes](applications.md#native-workload-example).
The fixture binds `0.0.0.0:8000`; declaring an OCI ExposedPort or Node image
entrypoint does not configure that application behavior.

## Verify

After applying in the intended context:

```sh
kubectl -n apps rollout status deployment/node-demo --timeout=5m
kubectl -n apps exec deployment/node-demo -- /nodejs/bin/node \
  /app/src/main.mjs --probe "two words"
kubectl -n apps port-forward deployment/node-demo 18080:8000
```

Use `curl --fail http://127.0.0.1:18080/` from another terminal. Expect
`language: node`, the actual runtime version and `dependency: shared-contract`;
check the Exec report's argument boundaries and numeric identity. Inspect the
[actual runtime binding](observability.md#logs-and-actual-bindings) separately
from desired inventory. Stop the port-forward after verification.

## Pitfalls and qualification limits

- `package.json` scripts are not Brewlet entrypoint discovery. Author the executable and complete arguments explicitly, without assuming a shell.
- Repeated platform flags cannot make `.node` addons or OS-specific dependencies portable. Build separate platform payloads when necessary.
- Configure application data/cache writes on explicit volumes. `/app` remains read-only; Node/runtime options are application-owned, not injected by Brewlet.
- Separate dependency layers executed on both adapted and unchanged Node roots. A dedicated managed layer-set lifecycle on an unchanged Node root is not yet qualified.
- Current local physical execution is arm64. Runtime-source updates do not remediate the application's own npm dependencies.
