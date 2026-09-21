# Local Kubernetes

Install Brewlet in a **disposable kind cluster**, then build Spring PetClinic
and deploy its JAR-only image. This walkthrough shows each command: creating
the cluster, installing the runtime, building the application, and running it.

**Your existing Kubernetes cluster is not used.** If Docker Desktop already
runs Kubernetes with the kind engine, leave it alone. We will create a separate
cluster using the same Docker engine and keep its kubeconfig in a private directory.

## Before you begin

Use your normal terminal on **macOS**, **Linux**, or **Windows with WSL 2**.
On Windows, use a WSL terminal with Docker Desktop's WSL integration enabled,
not PowerShell. Start Docker Desktop with Linux containers, or a local Docker
Engine with Buildx on Linux.

This walkthrough assumes **JDK 21** and **Maven 3.9 or newer** are already
installed locally.

Install these command-line tools if you do not already have them:

- [kind 0.33.0](https://kind.sigs.k8s.io/docs/user/quick-start/#installation),
  matching the node image used below;
- [kubectl](https://kubernetes.io/docs/tasks/tools/) and
  [Helm](https://helm.sh/docs/intro/install/);
- [jq](https://jqlang.org/download/), `curl`, and `tar`.

Allow network access to GitHub, Docker Hub, GHCR, and Maven Central. Allow at
least 4 CPUs and about 8 GiB of Docker memory, with additional headroom for
existing workloads.

Check the tools before creating anything:

```sh
java -version &&
mvn -version &&
docker info &&
docker buildx version &&
kind version &&
kubectl version --client &&
helm version --short &&
jq --version
```

Both `java` and Maven's reported Java runtime must use JDK 21 before continuing.

Keep this terminal open and run the sections in order. The commands use ordinary
shell variables to remember this run's names; there are no shell options,
startup-file changes, helper functions, or special Bash session to configure.
`&&` stops a block when a command fails without exiting your terminal.
**If any command fails, stop there** and use the diagnostics or cleanup below.

!!! warning "Local evaluation only"
    Brewlet's privileged provisioner installs a shim and JDK and changes
    containerd **inside the disposable worker node**. A separate kind cluster
    is not a separate machine or security boundary: it shares Docker's CPU,
    memory, disk, and daemon with your other containers. Do not use a remote
    or production Docker engine, reset Docker Desktop's cluster, or change
    an existing cluster's configuration to follow this guide.

## 1. Create a disposable cluster

Create a private workspace and a unique cluster name. Save the names so you
can identify this run later. None of these variables changes your current
Docker or kubectl context.

```sh
BREWLET_WORK="$(mktemp -d "${TMPDIR:-/tmp}/brewlet-lab.XXXXXXXX")" &&
BREWLET_WORK="$(cd "$BREWLET_WORK" && pwd -P)" &&
BREWLET_CLUSTER="$(basename "$BREWLET_WORK" | tr '[:upper:].' '[:lower:]-')" &&
BREWLET_KUBECONFIG="$BREWLET_WORK/kubeconfig" &&
BREWLET_DOCKER_CONTEXT="$(docker context show)" &&
printf 'Workspace: %s\nCluster: %s\nKubeconfig: %s\nDocker context: %s\n' \
  "$BREWLET_WORK" "$BREWLET_CLUSTER" "$BREWLET_KUBECONFIG" "$BREWLET_DOCKER_CONTEXT" |
  tee "$BREWLET_WORK/run.txt"
```

Confirm this Docker context points to a **local Unix socket**, such as
`unix:///var/run/docker.sock` or Docker Desktop's local socket:

```sh
docker context inspect "$BREWLET_DOCKER_CONTEXT" \
  --format '{{.Endpoints.docker.Host}}'
```

If it shows `ssh://` or `tcp://`, stop and select your local Docker engine
before starting a new run.

Create one control-plane node and one worker. The worker's label will tell
Brewlet where to install the Java runtime:

```sh
cat > "$BREWLET_WORK/kind.yaml" <<'EOF'
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
  - role: control-plane
  - role: worker
    labels:
      brewlet.sh/local-pool: local-java
EOF
```

Select the node image for the Docker engine's native architecture. This avoids
accidentally running AMD64 Kubernetes under emulation on Apple Silicon:

```sh
BREWLET_ARCH="$(docker --context "$BREWLET_DOCKER_CONTEXT" version --format '{{.Server.Arch}}')" &&
BREWLET_KIND_DIGEST="$(
  docker --context "$BREWLET_DOCKER_CONTEXT" buildx imagetools inspect --raw \
    kindest/node@sha256:099e049362a1526b2db71494e1947aae99bd16290d7c895f2b7ea312e3cbfaed |
    jq -er --arg arch "$BREWLET_ARCH" \
      '.manifests[] | select(.platform.os == "linux" and .platform.architecture == $arch) | .digest'
)" &&
env -u DOCKER_DEFAULT_PLATFORM \
  DOCKER_CONTEXT="$BREWLET_DOCKER_CONTEXT" KIND_EXPERIMENTAL_PROVIDER=docker \
  kind create cluster --name "$BREWLET_CLUSTER" \
    --image "kindest/node@$BREWLET_KIND_DIGEST" \
    --config "$BREWLET_WORK/kind.yaml" --kubeconfig "$BREWLET_KUBECONFIG" --wait 180s
```

The separate kubeconfig is important: do not run `kubectl config use-context`
or export `KUBECONFIG`. Every Kubernetes command below names this file explicitly.

```sh
kubectl --kubeconfig "$BREWLET_KUBECONFIG" wait nodes --all \
  --for=condition=Ready --timeout=120s &&
kubectl --kubeconfig "$BREWLET_KUBECONFIG" get nodes -o wide
```

You should see two Ready nodes with your unique cluster name. Do not proceed
if cluster creation or either readiness check failed.

## 2. Install Brewlet

Install the latest checksum-verified Brewlet CLI **inside this workspace**,
without replacing an existing CLI or changing your terminal's `PATH`:

```sh
curl -fL https://brewlet.sh/install.sh -o "$BREWLET_WORK/install.sh" &&
sh "$BREWLET_WORK/install.sh" --version latest --install-dir "$BREWLET_WORK/bin" &&
BREWLET_VERSION="$("$BREWLET_WORK/bin/brewlet" version)" &&
printf 'Using Brewlet %s\n' "$BREWLET_VERSION"
```

The installer may suggest adding its directory to `PATH`; skip that suggestion
here. The commands below use the CLI's full path.

Choose Temurin 21 as the node's JDK. Resolve its image digest once so the
installation uses an immutable image, not a moving tag:

```sh
JDK_DIGEST="$(
  docker --context "$BREWLET_DOCKER_CONTEXT" buildx imagetools inspect \
    docker.io/library/eclipse-temurin:21-jdk --format '{{.Manifest.Digest}}'
)" &&
printf '%s\n' "$JDK_DIGEST" | grep -Eq '^sha256:[0-9a-f]{64}$'
```

Write the chart values. The `local-java` pool matches only the worker created
in step 1; the control plane stays excluded:

```sh
cat > "$BREWLET_WORK/brewlet-local.yaml" <<EOF
provisioner:
  pools: [local-java]
  poolKey: brewlet.sh/local-pool
  jdks:
    - distribution: temurin
      feature: 21
      source:
        image: docker.io/library/eclipse-temurin@${JDK_DIGEST}
        javaHome: /opt/java/openjdk
EOF
```

Install the chart version matching the CLI. The `env` settings apply only to
this command and prevent inherited Helm API-server or SQL-storage overrides
from redirecting the installation:

```sh
env -u HELM_KUBEAPISERVER HELM_DRIVER=secret \
  helm install brewlet oci://ghcr.io/microsoft/charts/brewlet \
    --kubeconfig "$BREWLET_KUBECONFIG" --kube-context "kind-$BREWLET_CLUSTER" \
    --version "$BREWLET_VERSION" --namespace brewlet --create-namespace \
    --values "$BREWLET_WORK/brewlet-local.yaml"
```

Use this guide's commands rather than the chart's generic "Next steps" output:
the commands here explicitly select the disposable cluster's kubeconfig.

Wait for the operator, admission service, and node runtime. A successful Helm
command alone does not mean the worker is ready:

```sh
kubectl --kubeconfig "$BREWLET_KUBECONFIG" rollout status \
  deployment/brewlet-operator -n brewlet --timeout=5m &&
kubectl --kubeconfig "$BREWLET_KUBECONFIG" rollout status \
  deployment/brewlet-admission -n brewlet --timeout=5m &&
kubectl --kubeconfig "$BREWLET_KUBECONFIG" wait "node/$BREWLET_CLUSTER-worker" \
  --for=jsonpath='{.metadata.labels.brewlet\.sh/runtime}'=ready --timeout=10m &&
kubectl --kubeconfig "$BREWLET_KUBECONFIG" get runtimeclass brewlet
```

At this point, **Brewlet is installed**. Next you will build an application
separately, then ask Brewlet to run it.

## 3. Build PetClinic

Download a pinned revision of the upstream Spring PetClinic source into the
workspace. This does not use or modify any of your existing source checkouts:

```sh
PETCLINIC_REVISION=b3ee2c53e76e9267f03551a7cd36b0983c859c56
curl -fL \
  "https://github.com/spring-projects/spring-petclinic/archive/$PETCLINIC_REVISION.tar.gz" \
  -o "$BREWLET_WORK/petclinic.tar.gz" &&
mkdir -p "$BREWLET_WORK/petclinic" &&
tar -xzf "$BREWLET_WORK/petclinic.tar.gz" --strip-components=1 \
  -C "$BREWLET_WORK/petclinic"
```

Build the executable Spring Boot JAR using your local Maven and JDK 21.
Maven uses its normal local dependency cache; the application source and build
output stay in this run's workspace:

```sh
mvn -q -B -f "$BREWLET_WORK/petclinic/pom.xml" \
  -DskipTests -Dcheckstyle.skip=true -Dspotless.check.skip=true \
  -Denforcer.skip=true package &&
cp "$BREWLET_WORK/petclinic/target/"*.jar "$BREWLET_WORK/petclinic.jar"
```

The result is `$BREWLET_WORK/petclinic.jar`: an ordinary Spring Boot JAR,
not an application container image.

## 4. Package and load the application

Package **only the JAR** as a runnable OCI image. The Go CLI's `push` command
writes a local OCI layout; it does not upload to a registry. Use `--format image`
for Kubernetes:

```sh
PETCLINIC_OCI_REF="localhost/brewlet/${BREWLET_CLUSTER}:local"
"$BREWLET_WORK/bin/brewlet" push "$BREWLET_WORK/petclinic.jar" "$PETCLINIC_OCI_REF" \
  --store "$BREWLET_WORK/oci" --format image
```

Read the image digest, then import the layout into the **worker's** containerd
store. Importing it into Docker Desktop's ordinary image store would not make
it available to the Kubernetes node:

```sh
PETCLINIC_DIGEST="$(
  jq -er --arg ref "$PETCLINIC_OCI_REF" \
    '.manifests[] | select(.annotations["org.opencontainers.image.ref.name"] == $ref) | .digest' \
    "$BREWLET_WORK/oci/index.json"
)" &&
PETCLINIC_IMAGE="${PETCLINIC_OCI_REF%:*}@$PETCLINIC_DIGEST"
```

```sh
COPYFILE_DISABLE=1 tar -C "$BREWLET_WORK/oci" -cf - oci-layout index.json blobs |
  docker --context "$BREWLET_DOCKER_CONTEXT" exec -i "$BREWLET_CLUSTER-worker" \
    ctr -n k8s.io images import --digests - &&
docker --context "$BREWLET_DOCKER_CONTEXT" exec "$BREWLET_CLUSTER-worker" \
  ctr -n k8s.io images tag "$PETCLINIC_OCI_REF" "$PETCLINIC_IMAGE"
```

This cluster has just one Brewlet-enabled worker, so there is only one node to
load. The `localhost/...` name does not require a registry listening on localhost.

## 5. Deploy PetClinic

Write a `JavaApplication`. It requests the worker's Temurin 21 JDK, one replica,
a Service, and a health check. `pullPolicy: Never` tells Kubernetes to use the
image you just loaded rather than contact a registry:

```sh
cat > "$BREWLET_WORK/petclinic.yaml" <<EOF
apiVersion: apps.brewlet.sh/v1alpha1
kind: JavaApplication
metadata:
  name: petclinic
  namespace: petclinic
spec:
  artifact:
    image: ${PETCLINIC_IMAGE}
    pullPolicy: Never
  replicas: 1
  jvm:
    version: 21
    distribution: temurin
    args: ["-XX:MaxRAMPercentage=75.0", "-XX:+ExitOnOutOfMemoryError"]
  resources:
    requests: {cpu: 500m, memory: 512Mi}
    limits: {cpu: "1", memory: 768Mi}
  env:
    - name: MANAGEMENT_ENDPOINTS_WEB_EXPOSURE_INCLUDE
      value: health
  ports:
    - name: http
      containerPort: 8080
  service:
    enabled: true
    type: ClusterIP
  probes:
    readiness:
      httpGet: {path: /actuator/health, port: 8080}
      periodSeconds: 5
      timeoutSeconds: 3
EOF
```

Apply it and wait:

```sh
kubectl --kubeconfig "$BREWLET_KUBECONFIG" create namespace petclinic &&
kubectl --kubeconfig "$BREWLET_KUBECONFIG" apply -f "$BREWLET_WORK/petclinic.yaml" &&
kubectl --kubeconfig "$BREWLET_KUBECONFIG" wait --for=condition=Ready \
  javaapplication/petclinic -n petclinic --timeout=5m &&
kubectl --kubeconfig "$BREWLET_KUBECONFIG" get deployment,pod,service -n petclinic
```

The operator creates the Deployment and Service. The application image has no
JDK: Brewlet runs the JAR with the JDK installed on the worker.

## 6. Open PetClinic

Forward the Service to your laptop:

```sh
kubectl --kubeconfig "$BREWLET_KUBECONFIG" -n petclinic \
  port-forward --address 127.0.0.1 service/petclinic 18080:8080
```

Keep the command running and open <http://127.0.0.1:18080>. Choose **Find Owners**
to explore the sample data. PetClinic uses an in-memory H2 database; no external
database or ingress controller is needed.

If port 18080 is occupied, use `18081:8080` instead and open port 18081. Do not
stop an unrelated process to reclaim the port.

??? note "Optional: check application health"

    In a second terminal, run:

    ```sh
    curl -f http://127.0.0.1:18080/actuator/health
    ```

    Expect `"status":"UP"`.

## 7. Clean up this cluster

Press **Ctrl+C** to stop port forwarding. **This does not delete the cluster.**
In the original terminal, confirm that the private kubeconfig belongs to this
run, then delete only the named disposable cluster:

```sh
if [ -n "$BREWLET_CLUSTER" ] && [ -f "$BREWLET_KUBECONFIG" ] &&
   [ "$(kubectl --kubeconfig "$BREWLET_KUBECONFIG" config current-context)" = "kind-$BREWLET_CLUSTER" ]; then
  env DOCKER_CONTEXT="$BREWLET_DOCKER_CONTEXT" KIND_EXPERIMENTAL_PROVIDER=docker \
    kind delete cluster --name "$BREWLET_CLUSTER" --kubeconfig "$BREWLET_KUBECONFIG"
else
  printf 'Stop: recover this run from its workspace/run.txt before deleting anything.\n' >&2
fi
```

Deleting this disposable cluster removes its Brewlet installation, JDK, and
PetClinic together. No Helm uninstall or host-runtime repair is necessary:
the node containers themselves are discarded. Your existing Kubernetes cluster,
default kubeconfig, and current kubectl context remain unchanged.

The workspace retains the downloaded CLI, source, JAR, and OCI layout for
inspection. Remove that exact directory when you no longer need it. Docker may
keep downloaded images in its cache; do not run a global prune as tutorial cleanup.

## If a step fails

A failure does not close your terminal. Stop before the next section and inspect
the failing component. For Kubernetes problems, use the private kubeconfig:

```sh
kubectl --kubeconfig "$BREWLET_KUBECONFIG" get pods -A -o wide
kubectl --kubeconfig "$BREWLET_KUBECONFIG" get events -A --sort-by=.lastTimestamp
```

For an application that exits:

```sh
kubectl --kubeconfig "$BREWLET_KUBECONFIG" logs deployment/petclinic -n petclinic --tail=100
```

You can discard a successfully created test cluster using step 7 even if
installation or deployment failed. If cluster creation itself fails,
inspect kind's error and cleanup output rather than targeting another context.

If you lose the terminal, recover the exact workspace, cluster, Docker context,
and kubeconfig names from the saved `run.txt`. Do not guess a cluster name or use
Docker Desktop's **Reset Kubernetes cluster** as cleanup.

## Where to go next

- [Build and publish your own application](building-and-publishing.md).
- [Install Brewlet in an administrator-managed cluster](installation.md).
- [Explore the CLI without Kubernetes](getting-started.md).

For a separate, fully automated demo rather than this walkthrough, the optional
[demo script](https://brewlet.sh/try-brewlet.sh) remains available.
