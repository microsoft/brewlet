# Part 2: Build and deploy a Brewlet workload

**Audience:** Java application developers.

**Goal:** build a JAR, publish it as a runnable OCI image, and deploy it to a
Brewlet-enabled Kubernetes cluster without creating a Dockerfile or bundling a
JDK.

## 1. Receive the platform handoff

Ask the Ops participant for:

```bash
export BREWLET_CONTEXT="<kubernetes-context>"
export BREWLET_NAMESPACE="<developer-namespace>"
export BREWLET_JDK="21"
export BREWLET_VERSION="<version-from-ops-handoff>"
export BREWLET_REGISTRY="<registry-host>/<team>"
```

Use the concrete version Ops resolved during installation, not the literal
`latest` or a new latest-release lookup. The CLI, example source, Maven plugin,
and cluster components must use the same release.

You also need JDK 21+, Maven 3.9+, Git, `kubectl`, `curl`, `tar`, and credentials
for your application registry. Clone the example source from the release tag
matching the platform handoff:

```bash
git clone --depth 1 --branch "v${BREWLET_VERSION}" https://github.com/microsoft/brewlet.git
cd brewlet
```

If Ops used custom source-built components, use the source revision they supply
instead. Brewlet's source and release downloads do not require repository
credentials.

The registry repository must be readable by the cluster nodes.

Select the provided context and confirm your access:

```bash
kubectl config use-context "$BREWLET_CONTEXT"
kubectl auth can-i create deployments -n "$BREWLET_NAMESPACE"
kubectl auth can-i create javaapplications.apps.brewlet.sh -n "$BREWLET_NAMESPACE"
kubectl get runtimeclass brewlet
```

Install the released CLI with the
[checksum-verifying installer](../getting-started.md#install-the-released-cli-recommended):

```bash
curl -fsSL https://brewlet.sh/install.sh | sh -s -- --version "$BREWLET_VERSION" --install-dir "$HOME/.local/bin"
export PATH="$HOME/.local/bin:$PATH"
brewlet version
```

The CLI must report `BREWLET_VERSION`. For custom source-built components, use
the [matching source-built CLI](../getting-started.md#alternative-build-from-source)
instead. Run the same readiness check used by Ops:

```bash
brewlet doctor \
  --context "$BREWLET_CONTEXT" \
  --namespace "$BREWLET_NAMESPACE"
```

If your RBAC permits reading nodes, you can also inspect the available JDKs:

```bash
kubectl get nodes -l brewlet.sh/runtime=ready \
  -o custom-columns=NAME:.metadata.name,JDKS:.metadata.annotations.brewlet\\.sh/jdks
```

## 2. Build the example JAR

The included dependency-free application exposes `/hello`, `/healthz`, and
`/info`.

```bash
mvn -f integration-tests/fixtures/demo-app/pom.xml clean package
jar --describe-module \
  --file integration-tests/fixtures/demo-app/target/app.jar
```

The output is an ordinary executable JAR. It contains neither Linux nor a JDK.

## 3. Configure and exercise the Maven plugin

Maven downloads the released plugin from
[Maven Central](https://central.sonatype.com/artifact/sh.brewlet/brewlet-maven-plugin).
No manual installation, plugin source build, GitHub token, or custom repository
is needed. Use the same concrete `BREWLET_VERSION` exported in the Ops handoff;
do not resolve a separate latest plugin version.

Add this plugin alongside the existing entries under `<build><plugins>` in
`integration-tests/fixtures/demo-app/pom.xml`:

```xml
<plugin>
  <groupId>sh.brewlet</groupId>
  <artifactId>brewlet-maven-plugin</artifactId>
  <version>${env.BREWLET_VERSION}</version>
  <configuration>
    <appName>hello</appName>
    <ports>
      <port><name>http</name><containerPort>8080</containerPort></port>
    </ports>
    <probes>
      <readiness><path>/healthz</path></readiness>
    </probes>
    <cpuRequest>100m</cpuRequest>
    <memoryRequest>128Mi</memoryRequest>
    <cpuLimit>1</cpuLimit>
    <memoryLimit>256Mi</memoryLimit>
  </configuration>
</plugin>
```

This enables `mvn brewlet:...` without binding publishing to the build lifecycle.
The `<configuration>` describes how the application runs on Kubernetes: its
name, the port it listens on, the endpoint that reports readiness, and its
CPU and memory requests and limits. The plugin never guesses health probes; it
uses only the ones declared here.
For reproducible builds outside the workshop, pin the handoff version in your
project POM or CI environment. Use a platform release available on Central
(starting with 0.5.1). For a just-published release, wait until the matching
plugin is available on Central rather than changing the plugin version.

Build a registry-free runnable OCI layout first:

```bash
mvn -f integration-tests/fixtures/demo-app/pom.xml \
  brewlet:config brewlet:build \
  -Dbrewlet.image=demo/hello:workshop

test -f integration-tests/fixtures/demo-app/target/brewlet/jvm-config.json
test -f integration-tests/fixtures/demo-app/target/brewlet/oci/index.json
```

The image layout contains standard OCI layers for `amd64` and `arm64`. Each
platform manifest carries the Brewlet launch contract in
`brewlet.sh/jvm-config`; the image does not contain a base image.

## 4. Publish and deploy

Log in with your normal registry tooling first, for example `docker login` or
`az acr login --name <registry>`. The plugin reads `~/.docker/config.json`,
including credential helpers (`credsStore`/`credHelpers`) and identity tokens,
so no extra environment variables are needed.

Then push, generate the `JavaApplication`, apply it, and wait until it is Ready
in one step:

```bash
mvn -f integration-tests/fixtures/demo-app/pom.xml \
  brewlet:deploy \
  -Dbrewlet.registry="$BREWLET_REGISTRY" \
  -Dbrewlet.namespace="$BREWLET_NAMESPACE" \
  -Dbrewlet.kubeContext="$BREWLET_CONTEXT" \
  -Dbrewlet.jdkFeature="$BREWLET_JDK"
```

The image defaults to `$BREWLET_REGISTRY/<artifactId>:<version>`. The plugin
pushes it, records the digest-pinned deploy image in
`target/brewlet/push.json`, and writes the applied manifest to
`target/brewlet/`. It includes the port, a ClusterIP Service, and the
readiness probe from the POM:

```bash
cat integration-tests/fixtures/demo-app/target/brewlet/javaapplication.yaml
```

Private registry pull authentication for the nodes can be configured with
`spec.artifact.pullSecrets`; this workshop assumes the nodes can read the
selected repository directly.

## 5. Inspect what was deployed

Watch the higher-level resource and the Kubernetes objects it owns:

```bash
kubectl get javaapplication,deployment,pod,service -n "$BREWLET_NAMESPACE"
kubectl logs deployment/hello -n "$BREWLET_NAMESPACE"
```

The generated pod uses `runtimeClassName: brewlet`. The node-resident JDK
launches the JAR directly under the pod's CPU and memory cgroups.

## 6. Call the application

In one terminal:

```bash
kubectl port-forward service/hello 8080:8080 -n "$BREWLET_NAMESPACE"
```

In another:

```bash
curl http://127.0.0.1:8080/hello
curl http://127.0.0.1:8080/info
```

The `/info` response shows the selected Java runtime and the CPU and memory
limits observed by the JVM.

## 7. Make and deploy a change

Change the response in
`integration-tests/fixtures/demo-app/src/com/example/Hello.java`, then run the
same `brewlet:deploy` command again. The new push produces a new digest, so
Kubernetes rolls out the updated artifact like any other application update.

Your own applications work the same way: add the plugin with a
`<configuration>` that declares their ports and health endpoints (for example
`/actuator/health/readiness` with Spring Boot Actuator), then run
`mvn package brewlet:deploy` with the same `-Dbrewlet.*` properties.

## Cleanup

```bash
kubectl delete javaapplication hello -n "$BREWLET_NAMESPACE"
```

This removes the generated Deployment and Service but leaves the shared Brewlet
platform intact. See [Building and publishing](../building-and-publishing.md)
and [Deploying workloads](../deploying-workloads.md) for production options.
