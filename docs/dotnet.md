# .NET workloads

## How it works

Brewlet delivers your application separately from the .NET runtime. For a
framework-dependent .NET application, you publish assemblies, required
configuration and an explicit launch descriptor as an OCI image, without the
.NET runtime. The platform approves and provisions a compatible
[runtime image](runtime-images.md), and your Kubernetes Deployment selects it
by runtime ID. Brewlet runs the declared `dotnet` command with your
application files mounted read-only under `/app`.

Self-contained builds carry their own runtime, so they do not get this
separation of the .NET runtime. Their Linux userspace can still be
platform-managed.

.NET Native AOT applications are considered native workloads: they launch a
compiled executable directly rather than using a platform-supplied `dotnet`
runtime. See [Native (Go, Rust, etc)](native.md) for the platform-managed golden
image model and its maintenance boundaries.

Dependencies can ship inside the application image or be published separately
as a [dependency layer set](dependency-layer-sets.md) or
[governed bundle](managed-composition.md). Brewlet does not run NuGet or infer
your publishing model.

An update starts new Pods; it never patches a running CLR process. See
[concepts](concepts.md) and the
[launch descriptor](reference.md#launch-descriptor) for the execution model.
The runtime root is also read-only, so temporary files and caches need
[explicit writable volumes](resources.md#probes-exec-files-and-shutdown).

## Tested runtime images

Brewlet has been tested with .NET 10 using:

- unchanged Canonical `ubuntu/dotnet-runtime:10.0` images on arm64 and amd64
  ([digests](../integration-tests/fixtures/runtimes/managed-dotnet.json));
- Microsoft `mcr.microsoft.com/dotnet/runtime` 10.0 images adapted for Brewlet
  ([digests](../integration-tests/fixtures/runtimes/rotation.json)).

These are test inputs, not a security recommendation.

The .NET example fixture runs weekly against `main` in the
[language example workflow](https://github.com/microsoft/brewlet/blob/main/.github/workflows/language-e2e.yml). It does not gate pull
requests or releases; Python is the only qualification fixture (see
[verification](verification.md)).

Always use the runtime ID, `dotnet` path and platforms supplied by your
platform team. This guide assumes a runtime that provides `/usr/bin/dotnet`
under the example ID `dotnet-10`.

## Publish the application

This guide uses the sample application from the Brewlet repository. It has a
main `App.dll` and a separate `Greeting.dll` dependency, and pins .NET SDK
10.0.401 in `global.json`.

1. Get the sample and publish it framework-dependent. Run `dotnet publish`
   from the project directory so the pinned SDK is used:

    ```sh
    git clone --depth 1 --branch v0.8.1 https://github.com/microsoft/brewlet.git
    cp -R brewlet/integration-tests/fixtures/dotnet dotnet-demo
    cd dotnet-demo
    dotnet publish App/App.csproj --configuration Release \
      --output published --self-contained false --nologo
    ```

2. Split the output into an application layer and a dependency layer:

    ```sh
    mkdir application dependencies
    chmod 0755 application dependencies
    install -m 0644 published/App.dll published/App.deps.json \
      published/App.runtimeconfig.json application/
    install -m 0644 published/Greeting.dll dependencies/
    ```

The `.deps.json` and `.runtimeconfig.json` files belong with the application.
This sample deliberately moves one assembly into its own layer; do not split
arbitrary publish directories blindly. For a real application, include every
required native, configuration and content file.

## Write the launch descriptor

Save as `launch.json`:

```json
{
  "schemaVersion": 1,
  "process": {
    "entrypoint": [
      "/usr/bin/dotnet"
    ],
    "args": [
      "/app/App.dll"
    ],
    "workingDir": "/app"
  }
}
```

`Greeting.dll` must resolve beside `App.dll`, so both layers map to `/app`. A
self-contained executable would use its own path as the entrypoint and has its
own loader, native-library and platform requirements.

## Build and push

Run on Linux or macOS. On macOS, follow the
[producer filesystem requirements](applications.md#producer-filesystem).
Replace the registry with your own:

```sh
brewlet build registry.example.com/apps/dotnet-demo:v1 \
  --platform linux/arm64 \
  --launch launch.json \
  --layer "dependency=./dependencies:/app" \
  --layer "app=./application:/app"
brewlet push registry.example.com/apps/dotnet-demo:v1 --push-result push.json
```

A .NET assembly is not automatically platform-independent: RID-specific
assets, P/Invoke and native libraries still need compatible Linux targets.
[Dependency layer sets](dependency-layer-sets.md) are assembled when you build
the image; Brewlet never runs NuGet restore at container startup.

## Deploy

Replace the image digest, runtime ID, platform and namespace with values from
your platform team. Resource values are examples, not a CLR memory budget.

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: dotnet-demo
  namespace: apps
spec:
  replicas: 1
  selector:
    matchLabels: {app: dotnet-demo}
  template:
    metadata:
      labels: {app: dotnet-demo}
      annotations:
        brewlet.sh/runtime-image: dotnet-10
    spec:
      runtimeClassName: brewlet
      nodeSelector: {kubernetes.io/arch: arm64}
      containers:
        - name: app
          image: registry.example.com/apps/dotnet-demo@sha256:<application-digest>
          securityContext: {readOnlyRootFilesystem: true}
          resources:
            requests: {cpu: 100m, memory: 128Mi}
            limits: {cpu: "1", memory: 768Mi}
          readinessProbe:
            httpGet: {path: /, port: 8000}
```

Registry credentials, non-root defaults and volumes work as described in
[Package and deploy](applications.md#native-workload-example).

The sample has its own HTTP listener and does not need ASP.NET Core. An
ASP.NET Core application must select a runtime image that includes the
ASP.NET Core framework; a plain .NET runtime image is not interchangeable with
it.

## Verify

1. Wait for the rollout and run the probe through `kubectl exec`:

    ```sh
    kubectl -n apps rollout status deployment/dotnet-demo --timeout=5m
    kubectl -n apps exec deployment/dotnet-demo -- /usr/bin/dotnet \
      /app/App.dll --probe "two words"
    ```

    The output includes `"arguments":["--probe","two words"]` and a numeric,
    non-root `uid`.

2. Forward the port and query the service from another terminal:

    ```sh
    kubectl -n apps port-forward deployment/dotnet-demo 18080:8000
    curl --fail http://127.0.0.1:18080/
    ```

    Expect `"language":"dotnet"`, the actual CLR version and
    `"dependency":"shared-contract"`. Stop the port-forward afterwards.

3. Check the [actual runtime binding](observability.md#logs-and-actual-bindings)
   to confirm which runtime image the Pod uses.

## Pitfalls and limits

- The SDK used to publish and the runtime used to deploy are separate choices.
  Pin the SDK and select a compatible runtime ID explicitly.
- Framework-dependent, self-contained, single-file and Native AOT outputs have
  different loader and native-asset needs. This guide covers only the
  framework-dependent case.
- Shared runtime files do not mean a shared CLR heap. Brewlet injects no
  CLR or GC tuning. Size memory from your own tests (the sample's process plus
  `kubectl exec` probes ran out of memory at a 256Mi limit), and mount writable
  data and temp locations explicitly.
