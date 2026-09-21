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
</plugin>
```

This enables `mvn brewlet:...` without binding publishing to the build lifecycle.
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

## 4. Publish the application

Choose a unique tag and push it to the registry supplied by Ops:

```bash
mkdir -p target
export IMAGE_TAG="$BREWLET_REGISTRY/hello:$(date +%Y%m%d%H%M%S)"
export PUSH_LOG="$PWD/target/brewlet-push.log"

mvn -f integration-tests/fixtures/demo-app/pom.xml \
  brewlet:push \
  -Dbrewlet.image="$IMAGE_TAG" | tee "$PUSH_LOG"

export IMAGE_DIGEST="$(
  sed -n 's/.*index: \(sha256:[0-9a-f]\{64\}\).*/\1/p' "$PUSH_LOG" | tail -1
)"
test -n "$IMAGE_DIGEST"
export IMAGE="${IMAGE_TAG%:*}@$IMAGE_DIGEST"
rm -f "$PUSH_LOG"
```

Use your normal registry login mechanism before this command. Private registry
authentication can be configured with `spec.artifact.pullSecrets`; this
workshop assumes the nodes can read the selected repository directly.

## 5. Deploy with `JavaApplication`

```bash
cat <<EOF | kubectl apply -n "$BREWLET_NAMESPACE" -f -
apiVersion: apps.brewlet.sh/v1alpha1
kind: JavaApplication
metadata:
  name: hello
spec:
  artifact:
    image: ${IMAGE}
  jvm:
    version: ${BREWLET_JDK}
  resources:
    requests:
      cpu: 100m
      memory: 128Mi
    limits:
      cpu: "1"
      memory: 256Mi
  ports:
    - name: http
      containerPort: 8080
  service:
    enabled: true
    type: ClusterIP
  probes:
    readiness:
      httpGet:
        path: /healthz
        port: 8080
EOF
```

Watch the higher-level resource and the Kubernetes objects it owns:

```bash
kubectl get javaapplication,deployment,pod,service -n "$BREWLET_NAMESPACE"
kubectl rollout status deployment/hello -n "$BREWLET_NAMESPACE" --timeout=5m
kubectl logs deployment/hello -n "$BREWLET_NAMESPACE"
```

The generated pod uses `runtimeClassName: brewlet`. The node-resident JDK
launches the JAR directly under the pod's CPU and memory cgroups.

## 6. Call the application

In one terminal:

```bash
kubectl port-forward service/hello 8080:80 -n "$BREWLET_NAMESPACE"
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
`integration-tests/fixtures/demo-app/src/com/example/Hello.java`, choose a new
tag, then repeat the package, push, and apply steps. Kubernetes rolls out the new
artifact like any other application update.

## Cleanup

```bash
kubectl delete javaapplication hello -n "$BREWLET_NAMESPACE"
```

This removes the generated Deployment and Service but leaves the shared Brewlet
platform intact. See [Building and publishing](../building-and-publishing.md)
and [Deploying workloads](../deploying-workloads.md) for production options.
