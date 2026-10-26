# Java workloads

## How it works

Brewlet lets your team ship Java application code independently of
platform-managed runtime infrastructure. You publish JARs and an explicit launch
descriptor as an OCI artifact, without bundling a JDK. The platform approves and
provisions a Java [runtime image](runtime-images.md); your Kubernetes Deployment
selects its runtime ID. Brewlet runs your declared Java command with application
files mounted read-only under `/app`.

Dependencies can ship with your application or optionally come from an approved
[managed bundle](managed-composition.md). Release-time composition combines your
thin JAR with the locked dependency set into a new, verifiable application
artifact. Compatible library updates can preserve your application JAR, but
still require testing. The
[optional Maven plugin](applications.md#optional-maven-producer) prepares
Java-specific inputs; the `brewlet` CLI itself does not resolve Maven
dependencies or construct your classpath.

Updates run in new task instances, not by patching a live JVM. See the
[architecture overview](concepts.md) and [launch contract](reference.md#launch-descriptor)
for ownership, execution and rollout details.
The runtime root is read-only too; temporary files and caches need
[approved writable volumes](resources.md#probes-exec-files-and-shutdown).

## Tested runtimes

Brewlet's tests run Java on unmodified distroless Java 21 images (arm64, using
`/usr/bin/java`) and on Eclipse Temurin 21 images. The exact images are listed
in the [upstream runtime list](https://github.com/microsoft/brewlet/blob/main/integration-tests/fixtures/runtimes/upstream.json) and
[rotation runtime list](https://github.com/microsoft/brewlet/blob/main/integration-tests/fixtures/runtimes/rotation.json). They are test inputs, not a security
recommendation; use the runtime images your platform team approves.

The Java example fixture, including the Maven producer, runs weekly against
`main` in the [language example workflow](https://github.com/microsoft/brewlet/blob/main/.github/workflows/language-e2e.yml). It does not gate
pull requests or releases; Python is the only qualification fixture (see
[verification](verification.md)).

Ask your platform team for the runtime ID, supported platforms and Java
executable path (see the
[platform handoff](workshops/platform.md#give-developers-a-concrete-handoff)).
`java-21` below is an example ID chosen by an administrator; Brewlet has no
built-in runtime catalog.

## Build the payload

This complete thin-JAR example uses the small Java test application and its
separate `greeting` dependency from the Brewlet repository. You need Git, Maven
and JDK 21 or later:

```sh
WORK="$(mktemp -d)"
git clone --quiet --depth 1 --branch v0.8.1 https://github.com/microsoft/brewlet.git "$WORK/brewlet"
PROJECT="$WORK/brewlet/integration-tests/fixtures/java"
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

Save this as `$WORK/launch.json`. It names the application's main class and
the Java executable path in the selected runtime image:

```json
{
  "schemaVersion": 1,
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
not generate JVM options. The optional top-level `runtimeImages` list restricts
which runtime IDs may run the artifact; the Deployment must still name one.

## Package

Files must be world-readable and directories searchable (see
[producer filesystem requirements](applications.md#producer-filesystem), which
also covers macOS). Replace the registry with your own:

```sh
brewlet build registry.example.com/apps/java-demo:v1 \
  --platform linux/arm64 \
  --launch "$WORK/launch.json" \
  --layer "dependency=$WORK/dependencies:/app/lib" \
  --layer "app=$WORK/application:/app"
brewlet push registry.example.com/apps/java-demo:v1 --push-result "$WORK/push.json"
```

This puts dependencies in their own layer but does not publish a shared,
independently managed layer set. For that, see
[dependency layer sets](dependency-layer-sets.md).

Instead of the manual steps above, the optional
[Maven plugin](applications.md#optional-maven-producer) can generate the Java
arguments and dependency layers, or consume a verified layer set. Use plugin
version 0.8.1 with the 0.8.1 CLI. The plugin does not apply Kubernetes
resources.

## Native deployment

Use the digest from `push.json`. The following is a template: replace the
image digest, runtime ID, namespace and architecture with values from your
platform team. Resource sizes are examples, not JVM sizing recommendations.

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

Registry authentication, default non-root identity, capabilities and startup
tolerations follow the
[common workload rules](applications.md#native-workload-example). Mount
writable volumes if the application or JVM needs temporary storage.

## Verify

After applying the manifest:

```sh
kubectl -n apps rollout status deployment/java-demo --timeout=5m
kubectl -n apps exec deployment/java-demo -- /usr/bin/java \
  --class-path '/app/app.jar:/app/lib/*' sh.brewlet.fixture.Main --probe "two words"
kubectl -n apps port-forward deployment/java-demo 18080:8000
```

In another terminal, `curl --fail http://127.0.0.1:18080/` should report
`language: java`, the actual runtime and `dependency: shared-contract`.
The Exec report also includes `--probe` and `two words` as distinct arguments.
To see which runtime image generation the Pod actually uses, check the
[actual runtime binding](observability.md#logs-and-actual-bindings); rollout
success alone does not tell you. Stop the port-forward when finished.

## Pitfalls and limits

- A thin JAR needs its external dependency files; an executable (fat) JAR can instead use explicit `-jar` arguments. Brewlet does not unpack nested libraries or discover a main class.
- JPMS, agents, AppCDS and JVM tuning are your choices, written explicitly in `launch.json` (or generated by the Maven plugin). Runtime rotation must stay compatible with them; it does not update dependency copies under `/app`.
- Requests and limits are not inferred heap settings. Leave headroom for non-heap/native allocations and writable runtime data where needed.
- Native libraries (JNI) and prebuilt archives such as AppCDS are platform-specific; test them on every platform you declare.

See [resources](resources.md), [instrumentation](instrumentation.md) and
[troubleshooting](troubleshooting.md) for the common operational boundaries.
