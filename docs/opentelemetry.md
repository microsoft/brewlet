# OpenTelemetry for Java workloads

A Brewlet workload is an ordinary `java` process in an ordinary runc sandbox, so
[OpenTelemetry](https://opentelemetry.io/docs/languages/java/) instrumentation
works as it does elsewhere. What differs is **where the agent JAR comes from**
and **how the `-javaagent` flag reaches the JVM**. A Brewlet image carries only
the application: it has no Dockerfile or base image into which to copy an
agent, and some familiar Kubernetes injection patterns do not apply.

This page describes the supported approaches, recommends a default, and lists
the patterns that do not work with the current Brewlet handler.

Related: [Observability & day‑2](observability.md) ·
[Resource requests, limits & JVM tuning](resource-tuning.md) ·
[JDK management](jdk-management.md) ·
[Runtime metrics](runtime-metrics.md) (Brewlet's own telemetry, which is
separate from application telemetry).

!!! note "Validation scope"
    The approaches marked as working below were checked manually on a
    disposable local kind cluster. Each one exported spans with the expected
    `service.name` to an OpenTelemetry Collector. That run used the Java agent
    2.x line, Spring Boot 4 and a Temurin 21 node JDK. It is not part of the
    automated end-to-end suite. Re-check these approaches against your own JDK,
    agent and framework versions.

---

## Choosing an approach

| Approach | Who owns the agent | Works with | Recommendation |
|---|---|---|---|
| [Agent in the node JDK image](#agent-in-the-node-jdk-image) | Platform team | `JavaApplication`, raw Pods | **Recommended** for fleets with a shared agent version. |
| [Agent packaged in the application image](#agent-packaged-in-the-application-image) | Application team | `JavaApplication`, raw Pods | **Recommended** when each team chooses its own agent version or extensions. |
| [In-application SDK or framework starter](#in-application-sdk-or-framework-starter) | Application team | Any | Good for manual or framework-native instrumentation without an agent. |
| [Kubernetes image volume](#kubernetes-image-volume) | Platform or application team | Raw Pods and Deployments only | Works; use when you need raw Pod manifests anyway. |
| [`hostPath` volume](#hostpath-volume) | Node administrator | Raw Pods and Deployments only | Works, but is not recommended. |
| [Runtime self-attach](#avoid-runtime-self-attach) | Application team | Any | **Avoid.** The JDK is phasing out dynamic agent loading. |
| [Init container, sidecar or OpenTelemetry Operator injection](#patterns-that-do-not-work) | — | — | **Not supported** by the current handler. |

In every approach except the SDK/starter, the JVM needs the
`-javaagent:<path>` flag. Deliver it through
[`jvm.args`](#delivering-the-javaagent-flag) where possible.

Keep the OpenTelemetry Collector in its own Pods, such as a `Deployment` or
`DaemonSet` that uses the normal container runtime rather than
`runtimeClassName: brewlet`. Point workloads at it with the standard `OTEL_*`
environment variables ([common configuration](#common-configuration)).

---

## Common configuration

The OpenTelemetry Java agent and SDK read the standard
[`OTEL_*` environment variables](https://opentelemetry.io/docs/languages/java/configuration/).
Set them in the deployment (`spec.env` on a `JavaApplication`, or the container
`env` on a raw Pod), not in the artifact's launch configuration
([why](#keep-otel-settings-out-of-the-artifact)).

```yaml
env:
  - name: OTEL_SERVICE_NAME
    value: orders
  - name: OTEL_EXPORTER_OTLP_ENDPOINT
    value: http://otel-collector.observability:4318
  - name: OTEL_EXPORTER_OTLP_PROTOCOL
    value: http/protobuf
  - name: POD_NAME
    valueFrom: { fieldRef: { fieldPath: metadata.name } }
  - name: POD_NAMESPACE
    valueFrom: { fieldRef: { fieldPath: metadata.namespace } }
  - name: OTEL_RESOURCE_ATTRIBUTES
    value: k8s.pod.name=$(POD_NAME),k8s.namespace.name=$(POD_NAMESPACE)
```

`JavaApplication.spec.env` accepts standard Kubernetes `EnvVar` entries, so
Downward API `valueFrom` references and `$(VAR)` expansion work.

---

## Agent in the node JDK image

The platform team builds a JDK source image that also contains the agent, and
declares it as a separate JDK distribution. Brewlet copies the complete image
userland to each node and uses it as the workload's root filesystem. A file
at `/opt/otel/opentelemetry-javaagent.jar` in the source image therefore
appears at that path inside every workload that selects this distribution.

```dockerfile
FROM docker.io/library/eclipse-temurin@sha256:<reviewed-temurin-21-digest>
COPY opentelemetry-javaagent.jar /opt/otel/opentelemetry-javaagent.jar
RUN chmod 0644 /opt/otel/opentelemetry-javaagent.jar
```

Push that image, then add it alongside the plain JDK. Give it a distinct
`distribution` name
([multiple JDKs of the same feature](jdk-management.md#multiple-jdks-of-the-same-feature-version)):

```yaml
provisioner:
  jdks:
    - distribution: temurin
      feature: 21
      source:
        image: docker.io/library/eclipse-temurin@sha256:<reviewed-temurin-21-digest>
        javaHome: /opt/java/openjdk
    - distribution: temurin-otel
      feature: 21
      source:
        image: registry.example.com/platform/temurin-otel@sha256:<reviewed-digest>
        javaHome: /opt/java/openjdk
```

Applications opt in by selecting that distribution and passing the flag:

```yaml
apiVersion: apps.brewlet.sh/v1alpha1
kind: JavaApplication
metadata: { name: orders }
spec:
  artifact:
    image: registry.example.com/demo/orders@sha256:REPLACE_WITH_IMAGE_DIGEST
  jvm:
    version: 21
    distribution: temurin-otel
    args: ["-javaagent:/opt/otel/opentelemetry-javaagent.jar"]
  env:
    - { name: OTEL_SERVICE_NAME, value: orders }
    - { name: OTEL_EXPORTER_OTLP_ENDPOINT, value: "http://otel-collector.observability:4318" }
```

On a raw Pod, use the `brewlet.sh/jdk: "temurin-otel-21"` annotation and the
`brewlet.sh/jvm-args` annotation (a JSON array of strings) instead.

Why this is a good default for a fleet:

- The application image stays JAR-only. No team has to repackage to adopt or
  upgrade the agent.
- The platform team governs the agent the same way as the JDK: by reviewing
  and pinning a digest. Agent upgrades follow the
  [JDK patching workflow](jdk-management.md#patching-upgrading-jdks).
  Running pods keep their current root until they restart.
- Workloads that do not pass `-javaagent` are unaffected by the extra file.

Trade-offs: the agent version is coupled to the JDK root, so every workload on
that distribution moves together. Name the distribution explicitly. A bare
`version: 21` request may select either root (see
[JDK selection](deploying-workloads.md#jdk-selection)).

---

## Agent packaged in the application image

The application team attaches the agent to its own image as a class-path layer.
Class-path layers are unpacked read-only under `/app/lib`:

```bash
mkdir -p otel-layer
cp opentelemetry-javaagent.jar otel-layer/
tar -C otel-layer -cf otel-agent-layer.tar opentelemetry-javaagent.jar

brewlet push ./target/orders.jar demo/orders:1.0.0 \
  --store ./oci --classpath-layer otel-agent-layer.tar
```

Then reference it from the deployment:

```yaml
spec:
  jvm:
    version: 21
    args: ["-javaagent:/app/lib/opentelemetry-javaagent.jar"]
```

This keeps the agent version, and any agent extensions, under the application
team's control and in the same signed, digest-addressed image as the code.

Caveats:

- It was validated with the default `jar` entry mode (`java -jar`), where
  `/app/lib` is not on the application class path. In `classpath` entry mode
  with a `lib/*` wildcard, the agent JAR would also land on the application
  class path, which the OpenTelemetry project advises against. In that mode,
  prefer the [node JDK image](#agent-in-the-node-jdk-image) approach.
- The turnkey `brewlet push --appcds` option cannot be combined with
  `--classpath-layer`. Also see [AppCDS interaction](#appcds-interaction).

---

## In-application SDK or framework starter

Frameworks can export telemetry without an agent. For example, Spring Boot's
`spring-boot-starter-opentelemetry` exports traces over OTLP when configured
through the deployment environment:

```yaml
env:
  - { name: OTEL_SERVICE_NAME, value: orders }
  - { name: MANAGEMENT_OPENTELEMETRY_TRACING_EXPORT_OTLP_ENDPOINT, value: "http://otel-collector.observability:4318/v1/traces" }
  - { name: MANAGEMENT_TRACING_SAMPLING_PROBABILITY, value: "1.0" }
```

Nothing Brewlet-specific is required: the dependencies are part of the JAR. The
trade-off is coverage. You get what the framework and your code instrument,
rather than the agent's broad library auto-instrumentation.

---

## Kubernetes image volume

On clusters that support Kubernetes
[image volumes](https://kubernetes.io/docs/concepts/storage/volumes/#image),
a raw Pod or Deployment can mount an agent-only OCI image read-only and pass
the flag through the `brewlet.sh/jvm-args` annotation:

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: orders
  annotations:
    brewlet.sh/jdk: "temurin-21"
    brewlet.sh/jvm-args: '["-javaagent:/otel/javaagent.jar"]'
spec:
  runtimeClassName: brewlet
  containers:
    - name: orders
      image: registry.example.com/demo/orders@sha256:REPLACE_WITH_IMAGE_DIGEST
      volumeMounts:
        - { name: otel-agent, mountPath: /otel, readOnly: true }
  volumes:
    - name: otel-agent
      image:
        reference: registry.example.com/platform/otel-agent@sha256:<digest>
        pullPolicy: IfNotPresent
```

`JavaApplication` does not expose volumes or pod annotations. Use this approach
only when you already manage raw Pods or Deployments.

---

## `hostPath` volume

Mounting an agent that an administrator placed on each node through a
`hostPath` volume also works with raw Pods. It is not recommended: nothing
tracks, pins or reconciles the file, and `hostPath` is often restricted by Pod
Security admission. Use the [node JDK image](#agent-in-the-node-jdk-image)
approach to get the same result through the provisioner.

---

## Delivering the `-javaagent` flag

Prefer **`jvm.args`** (`spec.jvm.args`, or `brewlet.sh/jvm-args` on a raw Pod).
Brewlet passes these directly on the launcher command line, just before the
entrypoint ([details](resource-tuning.md#who-tunes-the-jvm)).

`JAVA_TOOL_OPTIONS` in `env` also works, and is common in APM vendor
instructions. Be aware that:

- the JVM prints `Picked up JAVA_TOOL_OPTIONS: …` to stderr on every start;
- it can also apply to other JVMs started in the container, such as a `jcmd`
  run through `kubectl exec`; and
- when a `JavaApplication` sets both `jvm.args` and `JAVA_TOOL_OPTIONS`, it
  reports `JVMArgsApplied=True` with reason `EnvOptionsOverlap` and emits a
  warning event. This is informational: both are applied, and `jvm.args` win
  on conflict.

The artifact's launch configuration has no free-form JVM argument field, so
the agent flag cannot be embedded in the image. That is deliberate:
observability agents are deployment tuning, not application code.

---

## Keep OTEL settings out of the artifact

The artifact launch configuration can carry `env` entries. When the same
variable is set both in the artifact and in the deployment, **the artifact
value wins**. For example, an artifact `OTEL_SERVICE_NAME` silently overrides
the `OTEL_SERVICE_NAME` in `JavaApplication.spec.env`. Keep all `OTEL_*`
settings, endpoints and resource attributes in the deployment, where
operators can change them per environment.

---

## Avoid runtime self-attach

Libraries such as `opentelemetry-runtime-attach` load the agent into the
running JVM from `main()`. This works on Brewlet, but it relies on dynamic
agent loading, which
[JEP 451](https://openjdk.org/jeps/451) is phasing out. Since JDK 21 the JVM
prints `WARNING: A Java agent has been loaded dynamically …`. A future JDK
will disallow dynamic loading by default, and the attach fails at startup
today when run with `-XX:-EnableDynamicAgentLoading`. Self-attach also needs
a writable temporary directory to extract the agent.

Loading the agent **at startup** with `-javaagent` is unaffected by JEP 451 and
produces no such warning. Use one of the approaches above instead.

---

## Patterns that do not work

The current Brewlet handler treats every non-sandbox container in a
`runtimeClassName: brewlet` Pod as a Brewlet application image. An ordinary
container image in the same Pod fails to start. Tag references are rejected
with `must be digest-pinned`, and digest-pinned ordinary images fail while
parsing their JVM launch configuration. As a result, these patterns are not
supported:

- **Init containers** that copy an agent into a shared `emptyDir`.
- **Collector or agent sidecars** in the application Pod. Run the Collector as
  a separate `Deployment` or `DaemonSet` instead.
- **OpenTelemetry Operator auto-instrumentation** through the
  `instrumentation.opentelemetry.io/inject-java` annotation. The operator
  injects an init container, so the Pod stays in `Init:RunContainerError`. Do
  not apply the injection annotation to namespaces that run Brewlet workloads.
  The operator's Collector management is unaffected.

This is the same limitation that applies to ordinary-image ephemeral debug
containers ([Deploying workloads](deploying-workloads.md#raw-deployment)).

---

## AppCDS interaction

The Java agent appends itself to the bootstrap class path. With class data
sharing enabled, the JVM then prints:

```text
Sharing is only supported for boot loader classes because bootstrap classpath has been appended
```

The workload still runs. However, shared-archive benefits for application
classes, including a shipped [AppCDS](appcds.md) archive, should be expected
to shrink. Measure startup with the agent enabled before relying on AppCDS
gains for instrumented workloads.

---

## Verifying instrumentation

Check that the flag reached the JVM (requires a JDK, not a JRE, in the selected
root):

```bash
kubectl exec <pod> -- jcmd 1 VM.command_line
kubectl logs <pod> | grep -i 'opentelemetry-javaagent - version'
```

Then confirm that spans with the expected `service.name` arrive at the
Collector, for example through its `debug` exporter output.
