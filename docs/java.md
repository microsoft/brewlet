# Java workloads

Package application JARs and dependencies, select an approved JDK runtime ID,
and deploy a native Kubernetes workload. Java-specific project knowledge stays
in your build tooling or the optional Maven producer, not Brewlet core.

## Tested runtime inputs

The unchanged-runtime companion exercised pinned distroless Java 21 roots on
arm64 using `/usr/bin/java`. The full-userland suite separately exercised
adapted Temurin 21.0.7 and 21.0.8 roots. Exact sources are in the
[upstream matrix](../integration-tests/fixtures/runtimes/upstream.json) and
[full-userland matrix](../integration-tests/fixtures/runtimes/rotation.json).
These historical test inputs are not a current security recommendation.

Obtain the runtime ID, supported platforms and executable path from the
[platform handoff](workshops/platform.md#give-developers-a-concrete-handoff).
`java-21` below is an example administrator-owned ID, not a built-in catalog.

## Build the payload

This complete thin-JAR example uses the same application and separate
dependency as the qualification suite. Run from the source checkout with Git,
Maven and JDK 21+:

```sh
WORK="$(mktemp -d)"
git archive HEAD integration-tests/fixtures/java | tar -x -C "$WORK"
PROJECT="$WORK/integration-tests/fixtures/java"
mvn -B -q -f "$PROJECT/dependency/pom.xml" install
mvn -B -q -f "$PROJECT/bom/pom.xml" install
mvn -B -q -f "$PROJECT/app/pom.xml" package
mkdir "$WORK/application" "$WORK/dependencies"
chmod 0755 "$WORK/application" "$WORK/dependencies"
install -m 0644 "$PROJECT/app/target/fixture-java-2.0.0.jar" "$WORK/application/app.jar"
install -m 0644 "$PROJECT/dependency/target/greeting-2.0.0.jar" "$WORK/dependencies/greeting-2.0.0.jar"
```

The resulting layout is `/app/app.jar` plus `/app/lib/greeting-2.0.0.jar`.
Do not package a JDK with the application.

## Launch descriptor

Save this as `$WORK/launch.json`. It names the real fixture main class and an
explicit executable path supplied by the selected runtime:

```json
{
  "schemaVersion": 2,
  "process": {
    "entrypoint": [
      "/usr/bin/java"
    ],
    "args": [
      "--class-path",
      "/app/app.jar:/app/lib/*",
      "sh.brewlet.fixture.Main"
    ],
    "workingDir": "/app"
  }
}
```

The classpath wildcard is a Java argument, not a shell expansion. Brewlet does
not generate JVM options. Optional top-level `runtimeImages` can restrict IDs;
omission does not remove the deployment's explicit selection requirement.

## Package

Use the matching CLI and a Linux producer filesystem with ordinary readable
files and searchable directories. For macOS input/producer setup, see
[getting started](getting-started.md) and the [containerized tutorial](local-kubernetes.md).
Replace the registry with your reviewed destination:

```sh
brewlet push registry.example.com/apps/java-demo:v1 \
  --platform linux/arm64 \
  --launch "$WORK/launch.json" \
  --layer "dependency=$WORK/dependencies:/app/lib" \
  --layer "app=$WORK/application:/app" \
  --push-result "$WORK/push.json"
```

This separates dependencies but does not itself publish an independent managed
layer set. For that workflow, use [layer sets](dependency-layer-sets.md).
The optional [Maven producer](applications.md#optional-maven-producer) can
generate Java argv/dependency layers or consume a verified complete layer set.
It must match this source revision and does not apply Kubernetes resources.

## Native deployment

Use the returned immutable digest. Replace the image placeholder, runtime ID,
namespace and architecture using the platform handoff; the following is a
template, not an immediately applicable resource. Resource sizes are examples,
not measured JVM recommendations.

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: java-demo
  namespace: apps
spec:
  replicas: 1
  selector:
    matchLabels: {app: java-demo}
  template:
    metadata:
      labels: {app: java-demo}
      annotations:
        brewlet.sh/runtime-image: java-21
    spec:
      runtimeClassName: brewlet
      nodeSelector: {kubernetes.io/arch: arm64}
      containers:
        - name: app
          image: registry.example.com/apps/java-demo@sha256:<application-digest>
          securityContext: {readOnlyRootFilesystem: true}
          resources:
            requests: {cpu: 100m, memory: 64Mi}
            limits: {cpu: "1", memory: 512Mi}
          readinessProbe:
            httpGet: {path: /, port: 8000}
```

Registry authentication, numeric defaults, capabilities and startup fencing use
the [shared deployment contract](applications.md#native-workload-example).
Use explicit writable volumes if the application/JDK needs temporary storage.

## Verify

After applying the reviewed manifest in the intended context:

```sh
kubectl -n apps rollout status deployment/java-demo --timeout=5m
kubectl -n apps exec deployment/java-demo -- /usr/bin/java \
  --class-path '/app/app.jar:/app/lib/*' sh.brewlet.fixture.Main --probe "two words"
kubectl -n apps port-forward deployment/java-demo 18080:8000
```

In another terminal, `curl --fail http://127.0.0.1:18080/` should report
`language: java`, the actual runtime and `dependency: shared-contract`.
The Exec report also includes `--probe` and `two words` as distinct arguments.
Inspect the [actual runtime binding](observability.md#logs-and-actual-bindings);
rollout success alone does not identify the leased generation. Stop the
port-forward when finished and use the workload's normal cleanup process.

## Pitfalls and qualification limits

- A thin JAR needs its external dependency files; an executable/fat JAR may instead use explicit `-jar` argv. Brewlet does not unpack nested libraries or discover a main class in generic core.
- JPMS, agents, AppCDS and JVM tuning are explicit producer/application choices. Runtime rotation must preserve their compatibility; it does not repair dependency copies under `/app`.
- Requests and limits are not inferred heap settings. Leave headroom for non-heap/native allocations and writable runtime data where needed.
- The unchanged-root Java run and the Maven managed-layer-set node run are separate evidence. They do not establish managed sets across all four languages on unchanged runtimes.
- Current local physical execution is arm64; native libraries/JNI and prebuilt archives need their own platform qualification.

See [resources](resources.md), [instrumentation](instrumentation.md) and
[troubleshooting](troubleshooting.md) for the common operational boundaries.
