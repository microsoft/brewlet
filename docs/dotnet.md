# .NET workloads

## How it works

Brewlet separates application delivery from platform-managed runtime
infrastructure. For a framework-dependent .NET application, you publish
assemblies, required configuration and an explicit launch descriptor as an OCI
artifact, without the .NET runtime. The platform approves and provisions a
compatible [runtime image](runtime-images.md); your Kubernetes Deployment
selects its runtime ID. Brewlet runs the declared `dotnet` command with
application files read-only under `/app`. Self-contained builds carry their own
runtime and do not provide that same runtime separation.

Dependencies can ship with the application or optionally come from an approved
[managed bundle](managed-composition.md). Release-time composition combines
app-only files and a locked dependency set into a verifiable artifact.
Compatible assembly updates can preserve your application assembly, but require
testing against its dependency manifests, loading layout and native libraries.
Brewlet does not run NuGet or infer your publishing model.

Updates run in new task instances, not by patching a live CLR process. See the
[architecture overview](concepts.md) and [launch contract](reference.md#launch-descriptor)
for the complete workflow.
The runtime root is read-only too; temporary files and caches need
[approved writable volumes](resources.md#probes-exec-files-and-shutdown).

## Tested runtime inputs

The full-userland suite exercised adapted MCR .NET runtime **10.0.0** and
**10.0.1** sources on arm64, with a framework-dependent application and a
separate `Greeting.dll`. Source pins are in the
[full-userland matrix](../integration-tests/fixtures/runtimes/rotation.json).
These historical inputs are not security recommendations.

The separate [managed-composition matrix](managed-composition.md) now covers
an **unchanged Canonical OCI .NET 10 runtime** on native arm64 and amd64, including an
independently signed dependency bundle, actual `Greeting.dll` replacement,
HTTP/Exec parity, two-replica rollout and normal cleanup. Exact source pins are
in the [managed .NET inputs](../integration-tests/fixtures/runtimes/managed-dotnet.json);
separate historical [arm64](https://github.com/microsoft/brewlet/blob/4c93db4e6c4affd3a1f2deb6a5099fffda3de8e5/acceptance/results/managed-composition-arm64.json) and
[amd64](https://github.com/microsoft/brewlet/blob/4c93db4e6c4affd3a1f2deb6a5099fffda3de8e5/acceptance/results/managed-composition-amd64.json) receipts preserve
their original native execution scopes, not current-candidate qualification.
The earlier unchanged distroless matrix's fourth root remains static/native.
Confirm the actual `dotnet` path with the platform. This recipe explicitly
targets a root providing `/usr/bin/dotnet` and an example ID `dotnet-10`.

## Build the payload

The fixture pins SDK **10.0.401** in
[`global.json`](../integration-tests/fixtures/dotnet/global.json).
Publish from the copied project directory so that SDK selection is honored:

```sh
WORK="$(mktemp -d)"
git archive HEAD integration-tests/fixtures/dotnet | tar -x -C "$WORK"
PROJECT="$WORK/integration-tests/fixtures/dotnet"
(
  cd "$PROJECT"
  dotnet publish App/App.csproj --configuration Release \
    --output "$WORK/published" --self-contained false --nologo
)
mkdir "$WORK/application" "$WORK/dependencies"
chmod 0755 "$WORK/application" "$WORK/dependencies"
install -m 0644 "$WORK/published/App.dll" "$WORK/application/App.dll"
install -m 0644 "$WORK/published/App.deps.json" "$WORK/application/App.deps.json"
install -m 0644 "$WORK/published/App.runtimeconfig.json" "$WORK/application/App.runtimeconfig.json"
install -m 0644 "$WORK/published/Greeting.dll" "$WORK/dependencies/Greeting.dll"
```

The runtime configuration/dependency manifests belong with the application.
This controlled fixture intentionally separates one managed assembly; do not
partition arbitrary publish directories blindly. Include all required native,
configuration and content files for your actual application.

## Launch descriptor

Save as `$WORK/launch.json`:

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

For this fixture, `Greeting.dll` must resolve beside `App.dll`; both layers map
to `/app`. A self-contained executable would use its own explicit path instead,
with separately reviewed loader/native-library/platform requirements.

## Package

Use the matching CLI on a suitable Linux producer filesystem; consult
[producer filesystem requirements](applications.md#producer-filesystem) for macOS. Replace the registry:

```sh
brewlet build registry.example.com/apps/dotnet-demo:v1 \
  --platform linux/arm64 \
  --launch "$WORK/launch.json" \
  --layer "dependency=$WORK/dependencies:/app" \
  --layer "app=$WORK/application:/app"
brewlet push registry.example.com/apps/dotnet-demo:v1 --push-result "$WORK/push.json"
```

A .NET assembly is not automatically platform-independent: RID-specific
assets, P/Invoke and native libraries still need compatible Linux targets.
Independent [layer sets](dependency-layer-sets.md) are a producer feature,
not NuGet restore at container startup.

## Native deployment

Replace the digest, approved ID, namespace and platform using the handoff.
The following template uses illustrative resource values, not a CLR memory budget:

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
            requests: {cpu: 100m, memory: 64Mi}
            limits: {cpu: "1", memory: 256Mi}
          readinessProbe:
            httpGet: {path: /, port: 8000}
```

Apply the [shared non-root/registry/volume contract](applications.md#native-workload-example).
The fixture provides its own HTTP listener and needs no ASP.NET runtime.
An ASP.NET application must select a runtime that actually includes its
framework; a runtime-only source is not interchangeable with an ASP.NET source.

## Verify

In the intended context:

```sh
kubectl -n apps rollout status deployment/dotnet-demo --timeout=5m
kubectl -n apps exec deployment/dotnet-demo -- /usr/bin/dotnet \
  /app/App.dll --probe "two words"
kubectl -n apps port-forward deployment/dotnet-demo 18080:8000
```

In another terminal, `curl --fail http://127.0.0.1:18080/` should report
`language: dotnet`, the CLR version and `dependency: shared-contract`.
Check identity and Exec argument boundaries, inspect the
[actual leased generation](observability.md#logs-and-actual-bindings), then stop
the port-forward after verification.

## Pitfalls and qualification limits

- SDK choice, framework references and deployment runtime compatibility are separate decisions. Pin the producer SDK and select a compatible approved runtime line explicitly.
- Framework-dependent, self-contained, single-file and Native AOT outputs have different loader/native-asset needs. Only the documented fixture is covered by this recipe's evidence.
- Shared files do not imply shared CLR heaps or automatic GC settings. Size resources and mount writable data/temp locations deliberately.
- The separate managed-set/unchanged-runtime matrix uses a reviewed 768Mi workload limit for the main CLR plus real Exec readiness processes. Its earlier 256Mi attempt OOMed and remains recorded as failed; core injects no CLR/GC tuning.
- Native local arm64 and CI amd64 receipts qualify the pinned inputs, not every .NET publishing model or mixed-fleet deployment.
