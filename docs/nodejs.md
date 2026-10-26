# Node.js workloads

## How it works

Brewlet delivers your JavaScript separately from the Node.js runtime. Your OCI
application image contains code, resolved package files and an explicit launch
descriptor, but not the Node executable. The platform approves and provisions
a Node.js [runtime image](runtime-images.md), and your Kubernetes Deployment
selects it by runtime ID. Brewlet runs the declared command with your
application files mounted read-only under `/app`.

Dependencies can ship inside the application image or be published separately
as a [dependency layer set](dependency-layer-sets.md) or
[governed bundle](managed-composition.md). Brewlet does not run npm, discover a
package entrypoint or rewrite module resolution.

An update starts new Pods; it never replaces modules inside a running Node.js
process. See [concepts](concepts.md) and the
[launch descriptor](reference.md#launch-descriptor) for the execution model.
The runtime root is also read-only, so temporary files and caches need
[explicit writable volumes](resources.md#probes-exec-files-and-shutdown).

## Tested runtime images

Brewlet has been tested with Node.js 22 using:

- unchanged distroless images (`gcr.io/distroless/nodejs22-debian13`), where
  the executable is `/nodejs/bin/node` and is not on the default `PATH`;
- Docker Official `node` 22 images adapted for Brewlet.

The exact digests are listed in the
[upstream](../integration-tests/fixtures/runtimes/upstream.json) and
[adapted](../integration-tests/fixtures/runtimes/rotation.json) test matrices.
They are test inputs, not a security recommendation.

The Node.js example fixture runs daily against `main` in the
[Node.js example workflow](https://github.com/microsoft/brewlet/blob/main/.github/workflows/language-node.yml). It does not gate pull
requests or releases; Python is the only qualification fixture (see
[verification](verification.md)).

Always use the runtime ID, executable path and platforms supplied by your
platform team. `node-22` below is an example ID; Brewlet does not infer a
Node.js version from it.

## Prepare the application files

This example uses two ES modules. `src/main.mjs` imports
`../dependencies/greeting.mjs`, so the two layers must keep that relative
layout under `/app`.

1. Create the directories:

    ```sh
    mkdir -p node-demo/src node-demo/dependencies
    cd node-demo
    ```

2. Save the dependency as `dependencies/greeting.mjs`:

    ```js
    export function message() {
      return 'shared-contract';
    }
    ```

3. Save the application as `src/main.mjs`:

    ```js
    import http from 'node:http';
    import { message } from '../dependencies/greeting.mjs';

    function report() {
      return {
        language: 'node',
        runtime: process.versions.node,
        dependency: message(),
        arguments: process.argv.slice(2),
        uid: process.getuid(),
        gid: process.getgid(),
      };
    }

    if (process.argv.includes('--probe')) {
      console.log(JSON.stringify(report()));
    } else {
      http.createServer((_request, response) => {
        response.writeHead(200, { 'Content-Type': 'application/json' });
        response.end(JSON.stringify(report()));
      }).listen(8000, '0.0.0.0');
    }
    ```

4. Make the files world-readable and the directories searchable:

    ```sh
    chmod 0755 src dependencies
    chmod 0644 src/main.mjs dependencies/greeting.mjs
    ```

This example needs no npm install. For a real npm project, install from your
reviewed lockfile before packaging and package the resulting `node_modules`
layout. Native addons and install-time builds must target the selected Linux
ABI, Node ABI and CPU architecture.

## Write the launch descriptor

Save as `launch.json`:

```json
{
  "schemaVersion": 1,
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

The import is explicit and relative, so `NODE_PATH` is not needed. Do not use
`NODE_PATH` as a general ES-module resolution mechanism: a CommonJS project may
use its documented resolution configuration, while an ESM package needs the
correct package/import layout or an explicit build output.

## Build and push

Run on Linux or macOS. On macOS, follow the
[producer filesystem requirements](applications.md#producer-filesystem).
Replace the registry with your own:

```sh
brewlet build registry.example.com/apps/node-demo:v1 \
  --platform linux/arm64 \
  --launch launch.json \
  --layer "dependency=./dependencies:/app/dependencies" \
  --layer "app=./src:/app/src"
brewlet push registry.example.com/apps/node-demo:v1 --push-result push.json
```

This produces separate dependency and app layers. `push.json` records the
pushed digest. To consume an independently published dependency set instead,
use [dependency layer sets](dependency-layer-sets.md).

## Deploy

Replace the image digest, runtime ID, platform and namespace with values from
your platform team. Resource values are examples.

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

Registry credentials, non-root defaults and volumes work as described in
[Package and deploy](applications.md#native-workload-example). The application
itself binds `0.0.0.0:8000`; an OCI `ExposedPorts` entry does not configure
that.

## Verify

1. Wait for the rollout and run the probe through `kubectl exec`:

    ```sh
    kubectl -n apps rollout status deployment/node-demo --timeout=5m
    kubectl -n apps exec deployment/node-demo -- /nodejs/bin/node \
      /app/src/main.mjs --probe "two words"
    ```

    The output includes `"arguments":["--probe","two words"]` and a numeric,
    non-root `uid`.

2. Forward the port and query the service from another terminal:

    ```sh
    kubectl -n apps port-forward deployment/node-demo 18080:8000
    curl --fail http://127.0.0.1:18080/
    ```

    Expect `"language":"node"`, the actual Node.js version and
    `"dependency":"shared-contract"`. Stop the port-forward afterwards.

3. Check the [actual runtime binding](observability.md#logs-and-actual-bindings)
   to confirm which runtime image the Pod uses.

## Pitfalls and limits

- `package.json` scripts are not used to find the entrypoint. Write the
  executable and complete arguments explicitly; no shell is assumed.
- Listing several platforms does not make `.node` addons or OS-specific
  dependencies portable. Build separate platform payloads when needed.
- `/app` is read-only. Put application data and caches on explicit volumes.
  Node.js options are set by the application, not injected by Brewlet.
- Updating the runtime image does not fix vulnerabilities in your own npm
  dependencies; rebuild or recompose the application for that.
