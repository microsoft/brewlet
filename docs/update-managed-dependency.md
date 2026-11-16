---
description: Replace Log4j in a running Java service on a local kind cluster without rebuilding the application or changing its Java runtime.
---

# Update a managed dependency

Patch Log4j in a running Java service without rebuilding the application or
changing its runtime.

When a library such as Log4j needs an urgent update, every team that bundled it
normally has to rebuild and republish its images. In this tutorial, Log4j is
instead a **managed dependency**: the platform team publishes it once as a
signed layer set, and a release pipeline combines that set with an app-only
image. To update Log4j, the platform team publishes a new set and the pipeline
recomposes the same application bytes with it.

| Part | Owner | Changes during the update? |
| --- | --- | --- |
| Java 21 runtime, RuntimeImage `java-21` | Platform administrator | No |
| `app.jar` and `launch.json`, in an app-only image | Application team | No |
| Log4j JARs, in a signed layer set | Platform team | Yes, 2.26.0 to 2.26.1 |
| Release image, from `brewlet bundle` | Release pipeline | Yes, new digest |

In practice, each role has its own pipeline, keys and registry. Here you play
all of them on one machine, and local OCI layouts stand in for a registry.
Log4j 2.26.1 stands in for whichever patched release an advisory requires; the
steps don't depend on a particular vulnerability.

## Prerequisites

- Steps 1 to 7 of [your first local deployment](local-kubernetes.md), without
  step 8. The `brewlet` kind cluster must still be running, with Brewlet
  installed and the `local` NodeProfile.
- The tools from that tutorial: Docker, kind, kubectl, Helm and the Brewlet CLI
- OpenSSL or LibreSSL, such as the `openssl` included with macOS
- Network access to Maven Central and Docker Hub

You can continue in the first tutorial's terminal or open a new one. Run every
step of this tutorial in the **same terminal**.

## 1. Reconnect to the cluster

Create a work directory with its own kubeconfig for the existing cluster:

```sh
export PATH="$HOME/.local/bin:$PATH"
DEMO="$(mktemp -d)"
kind get kubeconfig --name brewlet > "$DEMO/kubeconfig"
export KUBECONFIG="$DEMO/kubeconfig"
ARCH="$(docker info --format '{{.Architecture}}' | sed 's/x86_64/amd64/; s/aarch64/arm64/')"
digest() { grep -o 'sha256:[0-9a-f]\{64\}' "$1/index.json" | head -1; }
kubectl get deployment hello
```

The `hello` Deployment from the first tutorial is still listed. The `digest`
function prints the image digest of a local OCI layout. Layouts written by
`brewlet layer-set` and `brewlet bundle` also hold signatures and SBOMs,
listed after the image.

## 2. Offer a Java runtime

Add a Java 21 runtime next to the `python` runtime from the first tutorial:

```sh
kubectl apply -f - <<'EOF'
apiVersion: node.brewlet.sh/v1alpha1
kind: RuntimeImage
metadata:
  name: java-21
spec:
  source:
    image: gcr.io/distroless/java21-debian13@sha256:f3ac871fb5802c2739bb23b67212e67f341cbe754131528d2e9849072f9efb9a
  user: {uid: 65532, gid: 65532}
EOF
kubectl patch nodeprofile local --type=json \
  -p '[{"op":"add","path":"/spec/runtimeImages/-","value":{"id":"java-21"}}]'
kubectl wait runtimeimage/java-21 --for=condition=RotationComplete --timeout=5m
brewlet k8s runtime status java-21
```

The `java-21` RuntimeImage is the distroless Java 21 image, pinned by digest,
at generation 1. Nothing in this tutorial changes it again.

## 3. Create the release trust policy

The platform team signs layer sets, and the release pipeline signs the
composed release images. A release policy names the identity and public key
that each signature must match:

```sh
for key in publisher composer; do
  openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 -out "$DEMO/$key.key"
  chmod 600 "$DEMO/$key.key"
  openssl pkey -in "$DEMO/$key.key" -pubout -out "$DEMO/$key.pub"
done
FROM="$(date -u +%Y-%m-%d)T00:00:00Z"
UNTIL="$(( $(date -u +%Y) + 1 ))-12-31T23:59:59Z"
cat > "$DEMO/release-policy.yaml" <<EOF
schemaVersion: 1
mode: production
notAfter: "$UNTIL"
publisher:
  identity: https://ci.example.com/platform
  keys:
    - notBefore: "$FROM"
      notAfter: "$UNTIL"
      publicKey: |
$(sed 's/^/        /' "$DEMO/publisher.pub")
composer:
  identity: https://ci.example.com/release
  keys:
    - notBefore: "$FROM"
      notAfter: "$UNTIL"
      publicKey: |
$(sed 's/^/        /' "$DEMO/composer.pub")
EOF
```

`publisher` is the platform team that publishes Log4j, and `composer` is the
release pipeline. In `production` mode, Brewlet rejects unsigned inputs and
signatures from any other key or identity. Real pipelines keep their private
keys in a secret store and share only the policy. See
[trust policy](managed-composition.md#trust-policy) for key rotation and
revocation.

## 4. Publish Log4j 2.26.0 as a layer set

As the platform team, define a function that downloads a Log4j release from
Maven Central and publishes it as a signed layer set:

```sh
publish_log4j() {
  v="$1"
  mkdir "$DEMO/log4j-$v"
  for artifact in api core; do
    curl -fsSLo "$DEMO/log4j-$v/log4j-$artifact-$v.jar" \
      "https://repo1.maven.org/maven2/org/apache/logging/log4j/log4j-$artifact/$v/log4j-$artifact-$v.jar"
  done
  cat > "$DEMO/log4j-$v.lock.json" <<EOF
{"schemaVersion": 1, "entries": [
  {"ecosystem": "java", "group": "org.apache.logging.log4j", "name": "log4j-api", "version": "$v",
   "path": "/app/lib/log4j-api-$v.jar", "sha256": "$2",
   "purl": "pkg:maven/org.apache.logging.log4j/log4j-api@$v"},
  {"ecosystem": "java", "group": "org.apache.logging.log4j", "name": "log4j-core", "version": "$v",
   "path": "/app/lib/log4j-core-$v.jar", "sha256": "$3",
   "purl": "pkg:maven/org.apache.logging.log4j/log4j-core@$v"}
]}
EOF
  cat > "$DEMO/log4j-$v.json" <<EOF
{"schemaVersion": 1, "name": "log4j", "version": "$v", "ecosystem": "java", "layers": []}
EOF
  brewlet layer-set "example.invalid/platform/log4j:$v" --store "$DEMO/log4j-$v-set" \
    --config "$DEMO/log4j-$v.json" --lock "$DEMO/log4j-$v.lock.json" \
    --layer "dependency=$DEMO/log4j-$v:/app/lib" \
    --signing-key "$DEMO/publisher.key" --builder-identity https://ci.example.com/platform \
    > /dev/null
  echo "Log4j $v: $(digest "$DEMO/log4j-$v-set")"
}
publish_log4j 2.26.0 \
  820de7bbaf430eaf2e3e5ccb92f23e2085797c5ac4d4f7fc6c8b660fbefa1dbc \
  406bb3132c47a5d322e5c571feb0787ca84496469f346eb51f1ef2ee45499902
```

The arguments are the version and the SHA-256 hashes of the official
`log4j-api` and `log4j-core` JARs. The lock records each file's path, hash and
package URL, which external scanners use to match advisories.
`brewlet layer-set` refuses a file whose hash doesn't match its lock entry, or
that has no entry, so a corrupted or substituted download stops here. The JARs
land in `/app/lib`, and the set is signed with the publisher key.

## 5. Build the application without Log4j

As the application team, write a small HTTP service that logs with Log4j and
reports the Java and Log4j versions it runs with:

```sh
mkdir -p "$DEMO/src/demo" "$DEMO/app"
cat > "$DEMO/src/demo/Main.java" <<'EOF'
package demo;

import com.sun.net.httpserver.HttpServer;
import java.net.InetSocketAddress;
import java.nio.charset.StandardCharsets;
import org.apache.logging.log4j.LogManager;
import org.apache.logging.log4j.Logger;

public final class Main {
    private static final Logger LOG = LogManager.getLogger(Main.class);

    public static void main(String[] args) throws Exception {
        String log4j = LogManager.getContext(false).getClass().getPackage().getImplementationVersion();
        HttpServer server = HttpServer.create(new InetSocketAddress(8000), 0);
        server.createContext("/", exchange -> {
            LOG.info("GET {}", exchange.getRequestURI());
            byte[] body = String.format("{\"java\": \"%s\", \"log4j\": \"%s\"}%n",
                    Runtime.version(), log4j).getBytes(StandardCharsets.UTF_8);
            exchange.sendResponseHeaders(200, body.length);
            try (var out = exchange.getResponseBody()) {
                out.write(body);
            }
        });
        server.start();
        LOG.info("Listening on port 8000 with Log4j {}", log4j);
    }
}
EOF
```

Compile it in a JDK container, then package only `app.jar` and its launch
descriptor:

```sh
tar -C "$DEMO" -cf - src log4j-2.26.0/log4j-api-2.26.0.jar \
  | docker run --rm -i --platform "linux/$ARCH" \
      eclipse-temurin:21-jdk@sha256:0a3f329821dcadf54b2a3033a15e2e9f007069a9db43d7f51897e6b80e726b07 sh -c '
      cd "$(mktemp -d)" && tar --warning=no-unknown-keyword -xf - &&
      javac --release 21 -d classes -cp log4j-2.26.0/log4j-api-2.26.0.jar src/demo/Main.java &&
      jar --create -C classes .' > "$DEMO/app/app.jar"
cat > "$DEMO/launch.json" <<'EOF'
{"schemaVersion": 1, "process": {"entrypoint": ["/usr/bin/java"],
 "args": ["-Dlog4j2.level=INFO", "--class-path", "/app/app.jar:/app/lib/*", "demo.Main"],
 "workingDir": "/app"}}
EOF
brewlet build example.invalid/brewlet/log4j-demo:app --platform "linux/$ARCH" \
  --launch "$DEMO/launch.json" --layer "app=$DEMO/app:/app" --store "$DEMO/app-image"
```

The application compiles against the Log4j API without packaging it, like a
Maven `provided` dependency. The image holds only `/app/app.jar` and
`launch.json`: no JDK and no Log4j. It can't run on its own; its class path
expects the managed JARs in `/app/lib`.

Maven builds can produce the same kind of image with the
[Brewlet Maven plugin](managed-composition.md#application-retain-an-app-only-immutable-input).

## 6. Compose and deploy release 1

As the release pipeline, combine the application image with the Log4j
2.26.0 set:

```sh
brewlet bundle example.invalid/brewlet/log4j-demo:release-1 --store "$DEMO/release-1" \
  --application "$DEMO/app-image@$(digest "$DEMO/app-image")" \
  --layer-set "$DEMO/log4j-2.26.0-set@$(digest "$DEMO/log4j-2.26.0-set")" \
  --platform "linux/$ARCH" --policy "$DEMO/release-policy.yaml" \
  --signing-key "$DEMO/composer.key" --builder-identity https://ci.example.com/release \
  --push-result "$DEMO/release-1.json" > /dev/null
RELEASE_1="example.invalid/brewlet/log4j-demo@$(digest "$DEMO/release-1")"
echo "$RELEASE_1"
```

`brewlet bundle` checks the layer set's signature against the policy and
writes an ordinary application image: the app layer, the Log4j layer and the
launch descriptor. It signs a record of exactly which inputs it used, and saves
the result in `release-1.json`.

Load the release into the node and deploy it with the `java-21` runtime:

```sh
tar -C "$DEMO/release-1" -cf - oci-layout index.json blobs \
  | docker exec -i brewlet-control-plane ctr -n k8s.io images import -
docker exec brewlet-control-plane ctr -n k8s.io images tag \
  example.invalid/brewlet/log4j-demo:release-1 "$RELEASE_1"
kubectl apply -f - <<EOF
apiVersion: apps/v1
kind: Deployment
metadata:
  name: log4j-demo
spec:
  selector:
    matchLabels: {app: log4j-demo}
  template:
    metadata:
      labels: {app: log4j-demo}
      annotations: {brewlet.sh/runtime-image: java-21}
    spec:
      runtimeClassName: brewlet
      containers:
        - name: app
          image: $RELEASE_1
          imagePullPolicy: Never
          securityContext: {readOnlyRootFilesystem: true}
          readinessProbe:
            httpGet: {path: /, port: 8000}
EOF
kubectl expose deployment log4j-demo --port=8000
kubectl rollout status deployment/log4j-demo --timeout=5m
SERVICE_IP="$(kubectl get service log4j-demo -o jsonpath='{.spec.clusterIP}')"
docker exec brewlet-control-plane curl -s --retry 5 --retry-connrefused "http://$SERVICE_IP:8000/"
```

The response shows Java 21 from the node's runtime and Log4j 2.26.0 from the
layer set.

## 7. Publish Log4j 2.26.1

The platform team publishes the patched Log4j release:

```sh
publish_log4j 2.26.1 \
  f1810a4704ccce019d02bba029dc02a4f2ac0c997f647b465e7d409c1927f822 \
  2ec8c3bf5a6d7c89be8620116a2fc973f7dda1ad0261ad393c4d4726edfd3dcb
```

This is the only new input. The application team isn't involved, and the
application source and image stay as they are.

## 8. Recompose release 2 from release 1

The release pipeline passes the deployed release itself as the application,
together with the new set, then verifies the result:

```sh
brewlet bundle example.invalid/brewlet/log4j-demo:release-2 --store "$DEMO/release-2" \
  --application "$DEMO/release-1@$(digest "$DEMO/release-1")" \
  --layer-set "$DEMO/log4j-2.26.1-set@$(digest "$DEMO/log4j-2.26.1-set")" \
  --platform "linux/$ARCH" --policy "$DEMO/release-policy.yaml" \
  --signing-key "$DEMO/composer.key" --builder-identity https://ci.example.com/release \
  --push-result "$DEMO/release-2.json" > /dev/null
brewlet verify "$DEMO/release-2@$(digest "$DEMO/release-2")" \
  --platform "linux/$ARCH" --policy "$DEMO/release-policy.yaml" > /dev/null \
  && echo "release-2 verified"
```

Brewlet verifies release 1's signature before it removes the Log4j 2.26.0
layer, so it never strips layers from an unsigned or altered image. The app
layer is copied byte for byte. `brewlet verify` checks release 2's structure
and both signatures against the policy.

Compare the inputs recorded for each release:

```sh
echo "app image:    $(digest "$DEMO/app-image")"
echo "Log4j 2.26.0: $(digest "$DEMO/log4j-2.26.0-set")"
echo "Log4j 2.26.1: $(digest "$DEMO/log4j-2.26.1-set")"
for release in release-1 release-2; do
  echo "$release:"
  grep -oE '"(applicationRoot|dependencySet)":\{[^}]*\}' "$DEMO/$release.json" \
    | sed -E 's/^"([a-zA-Z]+)".*"digest":"(sha256:[0-9a-f]+)".*/  \1: \2/'
done
```

Both releases record the same `applicationRoot`, the app image you built in
step 5. Only the `dependencySet` changed.

## 9. Roll out release 2

Load release 2 into the node and update the Deployment's image:

```sh
RELEASE_2="example.invalid/brewlet/log4j-demo@$(digest "$DEMO/release-2")"
tar -C "$DEMO/release-2" -cf - oci-layout index.json blobs \
  | docker exec -i brewlet-control-plane ctr -n k8s.io images import -
docker exec brewlet-control-plane ctr -n k8s.io images tag \
  example.invalid/brewlet/log4j-demo:release-2 "$RELEASE_2"
OLD_POD="$(kubectl get pods -l app=log4j-demo -o name)"
kubectl set image deployment/log4j-demo app="$RELEASE_2"
kubectl rollout status deployment/log4j-demo --timeout=5m
kubectl wait "$OLD_POD" --for=delete --timeout=2m
docker exec brewlet-control-plane curl -s --retry 5 --retry-all-errors "http://$SERVICE_IP:8000/"
kubectl logs deployment/log4j-demo
kubectl get runtimeimage java-21
```

The replacement Pod answers with Log4j 2.26.1 and the same Java version as
before, and its log comes from the new Log4j. The `java-21` RuntimeImage is
still at generation 1.

The old Pod served Log4j 2.26.0 until the rollout replaced it: a running JVM
never swaps JARs in place. To roll back, set the image to `$RELEASE_1` again.

## 10. Clean up

Delete both applications, then let Brewlet remove the runtimes and restore the
node's configuration before deleting the cluster:

```sh
kubectl delete deployment log4j-demo hello
kubectl delete service log4j-demo hello
kubectl delete nodeprofile local --timeout=10m
kubectl delete runtimeimage java-21 python
helm uninstall brewlet --namespace brewlet
kind delete cluster --name brewlet
rm -rf "$DEMO"
```

If you're still in the first tutorial's terminal, also run `rm -rf "$WORK"`.
To remove the CLI too, run `rm ~/.local/bin/brewlet`.

## What to know before using this in production

- Brewlet has no vulnerability scanner or advisory database. Use your existing
  scanner with the SBOM published with each layer set, as described in
  [governed composition](managed-composition.md#operators-export-facts-for-external-scanners).
- A valid signature doesn't prove that the application works with the new
  library. Test the exact release digest before rolling it out.
- Only files in the managed layer set are replaced. Copies of Log4j shaded or
  bundled into the application need an application rebuild.
- Pods keep their old dependencies until they're replaced.

For the full advisory response across a fleet, follow the
[dependency remediation runbook](dependency-remediation.md). To learn more
about layer sets and governed releases, see
[dependency layer sets](dependency-layer-sets.md) and
[governed composition](managed-composition.md).
