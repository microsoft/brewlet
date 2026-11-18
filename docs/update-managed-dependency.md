---
description: Replace a vulnerable Log4j in a running Java service by publishing a new Maven dependency bundle, without rebuilding the application or changing its Java runtime.
---

# Update a managed dependency

Patch Log4j in a running Java service without rebuilding the application or
changing its runtime.

When a library such as Log4j needs an urgent update, every team that bundled it
normally has to rebuild and republish its images. In this tutorial, Log4j is
instead a **managed dependency**: the platform team keeps the approved versions
in a Maven BOM and publishes them once as a signed layer set. A release
pipeline combines that set with an app-only image. To update Log4j, the
platform team publishes a new bundle release and the pipeline recomposes the
same application bytes with it.

The tutorial uses a multi-module Maven
[sample project](https://github.com/microsoft/brewlet/tree/main/examples/managed-dependencies/log4j-update)
that you clone:

| Part | Owner | Changes during the update? |
| --- | --- | --- |
| Java 21 runtime, RuntimeImage `java-21` | Platform administrator | No |
| `app` module: a thin JAR in an app-only image | Application team | No |
| `bundle-release-1` and `bundle-release-2` modules: the Log4j bundle, as signed layer sets | Platform team | Yes, Log4j 2.14.1 to 2.26.1 |
| Release image, from `brewlet bundle` | Release pipeline | Yes, new digest |

In practice, each role has its own pipeline, keys and registry. Here you play
all of them on one machine, and local OCI layouts stand in for a registry.

!!! warning "Release 1 is vulnerable on purpose"
    Log4j 2.14.1 is affected by CVE-2021-44228 (Log4Shell), CVE-2021-45046,
    CVE-2021-45105 and CVE-2021-44832, among others. The sample service never
    exercises them, and a bundle with known CVEs isn't the same as an
    exploitable application, but run it only in the disposable local cluster
    from this tutorial. Don't deploy release 1 anywhere shared.

## Prerequisites

- Steps 1 to 5 of [your first local deployment](local-kubernetes.md). The
  `brewlet` kind cluster must still be running, with Brewlet installed and the
  `local` NodeProfile. The `hello` application from the later steps is
  optional.
- The tools from that tutorial: Docker, kind, kubectl, Helm and the Brewlet CLI
- JDK 21 or later, Maven 3.9 or later and Git
- OpenSSL or LibreSSL, such as the `openssl` included with macOS
- Network access to Maven Central, GitHub and Docker Hub
- Optional: [OSV-Scanner](https://google.github.io/osv-scanner/) for step 7

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
kubectl get nodeprofile local
```

The last command lists the `local` NodeProfile from the first tutorial. If it
reports `NotFound`, complete step 5 of that tutorial before you continue.

The `digest` function prints the image digest of a local OCI layout. Layouts
written by `brewlet layer-set` and `brewlet bundle` also hold signatures and
SBOMs, listed after the image.

## 2. Get the sample project

Clone only the sample directory, and install the two bundle POMs into your
local Maven repository:

```sh
git clone --depth 1 --filter=blob:none --sparse https://github.com/microsoft/brewlet.git "$DEMO/brewlet"
git -C "$DEMO/brewlet" sparse-checkout set examples/managed-dependencies/log4j-update
SAMPLE="$DEMO/brewlet/examples/managed-dependencies/log4j-update"
cd "$SAMPLE"
mvn -B -q install -pl '!app'
```

The project has three modules:

- `bundle-release-1` is the platform team's first release of the Log4j bundle,
  `log4j-bundle:1.0.0`. It pins Log4j 2.14.1.
- `bundle-release-2` is `log4j-bundle:2.0.0`. It pins Log4j 2.26.1.
- `app` is a small HTTP service. It imports the bundle BOM and depends on
  `log4j-api` without a version, so it follows the bundle.

Each bundle lists its members in `dependencies` and the exact approved
versions in `dependencyManagement`. The `app` module imports release 1 and
never changes. The install step stands in for the Maven repository your
platform team publishes to, which the `app` module resolves its BOM from.

## 3. Offer a Java runtime

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
kubectl wait node --all --for=jsonpath='{.metadata.labels.brewlet\.sh/runtime\.java-21}'=ready --timeout=5m
brewlet k8s runtime status java-21
```

The `java-21` RuntimeImage is the distroless Java 21 image, pinned by digest,
at generation 1. Nothing in this tutorial changes it again. The node's
`brewlet.sh/runtime.java-21=ready` label shows that it can now run Pods that
request `java-21`.

## 4. Create the release trust policy

The platform team signs layer sets, and the release pipeline signs the
composed release images. A release policy names the identity and public key
that each signature must match:

```sh
for key in publisher composer; do
  openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 \
    -pkeyopt ec_param_enc:named_curve -out "$DEMO/$key.key"
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

## 5. Publish release 1 and build the application

As the platform team, publish bundle release 1 as a signed layer set with the
[Brewlet Maven plugin](managed-composition.md#application-retain-an-app-only-immutable-input).
Define a function so that you can publish release 2 the same way later:

```sh
publish_bundle() {
  mvn -B -q -f "bundle-release-$1/pom.xml" sh.brewlet:brewlet-maven-plugin:0.10.0:layer-set \
    -Dbrewlet.sourceBom="sh.brewlet.examples:log4j-bundle:$1.0.0" \
    -Dbrewlet.layerSet="example.invalid/platform/log4j-bundle:$1.0.0" \
    -Dbrewlet.store="$DEMO/bundle-$1-set" \
    -Dbrewlet.signingKey="$DEMO/publisher.key" \
    -Dbrewlet.builderIdentity=https://ci.example.com/platform > /dev/null
  echo "Bundle release $1: $(digest "$DEMO/bundle-$1-set")"
}
publish_bundle 1
```

The plugin resolves the bundle's members, rejects any that aren't pinned in
its BOM, downloads their JARs from Maven Central and records a package lock
with each file's hash and package URL, which scanners use to match advisories.
The JARs land in `/app/lib`, and the set is signed with the publisher key.

As the application team, build the application into an app-only image:

```sh
mvn -B -q -f app/pom.xml package sh.brewlet:brewlet-maven-plugin:0.10.0:push \
  -Dbrewlet.applicationOnly=true \
  -Dbrewlet.sourceBom=sh.brewlet.examples:log4j-bundle:1.0.0 \
  -Dbrewlet.image=example.invalid/brewlet/log4j-demo:app \
  -Dbrewlet.platforms="linux/$ARCH" \
  -Dbrewlet.store="$DEMO/app-image" > /dev/null
echo "App image: $(digest "$DEMO/app-image")"
```

The image holds only `/app/app.jar` and a launch descriptor: no JDK and no
Log4j. `jar tf app/target/log4j-demo-app-1.0.0.jar` lists a single class,
`demo/Main.class`. The image can't run on its own; its class path expects the
managed JARs in `/app/lib`. `sourceBom` records which BOM the application was
built against.

## 6. Compose and deploy release 1

As the release pipeline, combine the application image with bundle release 1:

```sh
brewlet bundle example.invalid/brewlet/log4j-demo:release-1 --store "$DEMO/release-1" \
  --application "$DEMO/app-image@$(digest "$DEMO/app-image")" \
  --layer-set "$DEMO/bundle-1-set@$(digest "$DEMO/bundle-1-set")" \
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
      annotations: {runtime-image.brewlet.sh/app: java-21}
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

The response shows Java 21 from the node's runtime and Log4j 2.14.1 from the
layer set.

## 7. Scan the bundles for CVEs

Brewlet has no vulnerability scanner, but your Maven tooling can scan a bundle
before you publish it. Log4j's advisories are in `log4j-core`, which only the
bundles list, so scan the bundles, which the platform team owns. The `app`
module alone scans clean, because it depends on `log4j-api` only.

Generate a CycloneDX SBOM from each bundle with the CycloneDX Maven plugin,
then scan release 1 with OSV-Scanner:

```sh
for n in 1 2; do
  mvn -B -q org.cyclonedx:cyclonedx-maven-plugin:2.9.1:makeBom \
    -f "bundle-release-$n/pom.xml" -DoutputFormat=json
done
osv-scanner scan source -L bundle-release-1/target/bom.json
```

OSV-Scanner flags `log4j-core` 2.14.1 for several advisories, including
CVE-2021-44228 (Log4Shell, CVSS 10.0), CVE-2021-45046 and CVE-2021-45105, and
exits nonzero. The exact list grows as advisories are published. Release 2 is
the fix:

```sh
osv-scanner scan source -L bundle-release-2/target/bom.json
```

Release 2 reports `No issues found`. In a real pipeline, run the scan before
`publish_bundle` and fail the build on findings.

Other Maven plugins can do this job, with extra setup:

- **OWASP Dependency-Check**, `org.owasp:dependency-check-maven:check`, needs
  an [NVD API key](https://nvd.nist.gov/developers/request-an-api-key) to
  download its vulnerability data.
- **Sonatype OSS Index**, `org.sonatype.ossindex.maven:ossindex-maven-plugin:audit`,
  needs account credentials and skips POM-packaging modules such as these
  bundles.

## 8. Publish and recompose release 2

The platform team publishes the patched bundle as a new layer set:

```sh
publish_bundle 2
```

This is the only new input. The application team isn't involved, and the
application source and image stay as they are.

As the release pipeline, pass the deployed release itself as the application,
together with the new set, then verify the result:

```sh
brewlet bundle example.invalid/brewlet/log4j-demo:release-2 --store "$DEMO/release-2" \
  --application "$DEMO/release-1@$(digest "$DEMO/release-1")" \
  --layer-set "$DEMO/bundle-2-set@$(digest "$DEMO/bundle-2-set")" \
  --platform "linux/$ARCH" --policy "$DEMO/release-policy.yaml" \
  --signing-key "$DEMO/composer.key" --builder-identity https://ci.example.com/release \
  --push-result "$DEMO/release-2.json" > /dev/null
brewlet verify "$DEMO/release-2@$(digest "$DEMO/release-2")" \
  --platform "linux/$ARCH" --policy "$DEMO/release-policy.yaml" > /dev/null \
  && echo "release-2 verified"
```

Brewlet verifies release 1's signature before it removes the Log4j 2.14.1
layer, so it never strips layers from an unsigned or altered image. The app
layer is copied byte for byte. `brewlet verify` checks release 2's structure
and both signatures against the policy.

Compare the inputs recorded for each release:

```sh
echo "app image:        $(digest "$DEMO/app-image")"
echo "bundle release 1: $(digest "$DEMO/bundle-1-set")"
echo "bundle release 2: $(digest "$DEMO/bundle-2-set")"
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

The old Pod served Log4j 2.14.1 until the rollout replaced it: a running JVM
never swaps JARs in place. To roll back, set the image to `$RELEASE_1` again.

## 10. Clean up

Delete the applications, then let Brewlet remove the runtimes and restore the
node's configuration before deleting the cluster. Remove the sample bundles
from your local Maven repository too:

```sh
kubectl delete deployment log4j-demo hello --ignore-not-found
kubectl delete service log4j-demo hello --ignore-not-found
kubectl delete nodeprofile local --timeout=10m
kubectl delete runtimeimage java-21 python
helm uninstall brewlet --namespace brewlet
kind delete cluster --name brewlet
cd "$HOME"
rm -rf "$DEMO" ~/.m2/repository/sh/brewlet/examples
```

If you're still in the first tutorial's terminal, also run `rm -rf "$WORK"`.
To remove the CLI too, run `rm ~/.local/bin/brewlet`.

## What to know before using this in production

- A valid signature doesn't prove that the application works with the new
  library. Release 2 reuses an application built against release 1's BOM, and
  Brewlet doesn't check that the new set is compatible with it. Test the exact
  release digest before rolling it out.
- Only files in the managed layer set are replaced. Copies of Log4j shaded or
  bundled into the application need an application rebuild.
- Pods keep their old dependencies until they're replaced.
- Scanners report known advisories in the bundle's packages, not whether your
  application can reach the vulnerable code. An unavailable scan result is
  unknown, not clean. Brewlet's own SBOMs and inventory export feed the same
  scanners, as described in
  [governed composition](managed-composition.md#operators-export-facts-for-external-scanners).

For the full advisory response across a fleet, follow the
[dependency remediation runbook](dependency-remediation.md). To learn more
about layer sets and governed releases, see
[dependency layer sets](dependency-layer-sets.md) and
[governed composition](managed-composition.md).
