# Local Kubernetes

Run a Java application on Brewlet in Docker Desktop's **kind** Kubernetes
cluster. This guide takes you from a local cluster to Spring PetClinic in your
browser: install Brewlet, build the application JAR, load it into the cluster,
and deploy it with a `JavaApplication`.

PetClinic is the sample workload, not a special integration. Its Spring Boot
JAR runs on a JDK supplied by the node, without an application Dockerfile or a
base image. The sample uses an in-memory H2 database, so no database setup is
needed and data resets when the pod is replaced.

You can start here without completing [Getting started](getting-started.md).
That guide explores the CLI without Kubernetes; this one runs the full local
Kubernetes path. No registry account, Maven plugin installation, or Brewlet
source build is required. Both guides share the released CLI and one
`BREWLET_SOURCE` checkout, so you can follow them in either order without
cloning Brewlet again.

!!! warning "Inspect before installing"
    Brewlet is preproduction software. Its privileged provisioner installs a
    shim and JDK on the selected Kubernetes node and restarts that node's
    containerd. Do not follow this guide against a shared or production cluster.
    An existing local cluster does not need to be reset. First determine whether
    to reuse a healthy Brewlet installation, install on an unused worker, or
    stop for recovery. Never overwrite another installation or discard existing
    workloads just to complete this tutorial.

## Before you begin

Use macOS or Linux on `amd64` or `arm64`, with:

- Docker Desktop running Linux containers, with the **containerd image store**
  enabled. The instructions below use the Kubernetes view in Docker Desktop
  4.51 or later.
- JDK **21** to build PetClinic. The cluster will receive its own JDK 21; your
  host JDK is not copied into the application image.
- `bash`, Git, `curl`, `tar`, `unzip`, `jq`, `kubectl`, and Helm. PetClinic's
  Maven wrapper downloads Maven for the build.
- Network access to GitHub, Maven Central, Docker Hub, and GHCR.

Start with at least 4 CPUs and 8 GiB of memory allocated to Docker Desktop.
These are suggested development settings, not a measured minimum.

Run the commands in one **Bash** terminal unless instructed otherwise. Keep it
open so the variables remain available, and stop at any failed command. If
continuing in the Bash terminal from Getting started, skip the `bash` command:

```bash
bash
set -euo pipefail
```

??? note "Optional: check installed tools"

    If you are unsure whether the prerequisites are available, check them now:

    ```bash
    java -version
    git --version
    docker info
    docker buildx version
    kubectl version --client
    helm version --short
    jq --version
    ```

Ensure `JAVA_HOME` and `java` on `PATH` both select JDK 21.
For example, if you already use SDKMAN, run `sdk use java <installed-21-id>`
before starting the Bash terminal. This changes the current shell, not your
default JDK.

Create a fresh work directory and unique names for **this run**. The `k` helper
pins every Kubernetes command to the chosen context without changing your
global current context:

```bash
export BREWLET_CONTEXT="docker-desktop"
export BREWLET_RUN="$(date -u +%Y%m%d%H%M%S)-$$"
export BREWLET_NAMESPACE="petclinic-${BREWLET_RUN}"
BREWLET_WORK="$(mktemp -d "$PWD/brewlet-local.XXXXXX")"
export BREWLET_WORK
export BREWLET_INSTALLED_HERE=false
export BREWLET_POOL_LABEL_ADDED=false
k() { kubectl --context "$BREWLET_CONTEXT" "$@"; }
printf 'Context: %s\nNamespace: %s\nWork directory: %s\n' \
  "$BREWLET_CONTEXT" "$BREWLET_NAMESPACE" "$BREWLET_WORK"
```

Keep this terminal and these values for cleanup. If you lose them, inspect the
resources and recover the exact names; do not guess or delete a generic
`petclinic` namespace.

## 1. Choose and inspect the local cluster

### If Docker Desktop already has a cluster

Do not create, edit, or reset it yet. Inspect the existing state:

```bash
kubectl config get-contexts
k get nodes -o wide
k get namespaces
k get deployment,daemonset,pod -A -o wide
helm list --kube-context "$BREWLET_CONTEXT" --all-namespaces \
  --deployed --failed --pending --uninstalling --uninstalled --superseded
k get runtimeclasses
k get crd nodeprofiles.node.brewlet.sh javaapplications.apps.brewlet.sh \
  --ignore-not-found
k get clusterrole,clusterrolebinding,mutatingwebhookconfiguration,validatingwebhookconfiguration \
  -o name | awk '/brewlet/'
```

The explicit status filters work with both Helm 3 and Helm 4; Helm 4 removed
`helm list --all`. Keep unrelated resources intact. An empty Helm list alone
does **not** prove Brewlet is absent: earlier manual installs or test runs can
leave CRDs, webhooks, node labels, shim binaries, JDK directories, or containerd
configuration behind.

### If Docker Desktop has no cluster

In Docker Desktop:

1. Under **Settings > General**, enable **Use containerd for pulling and storing
   images** if it is not already enabled.
2. Open the **Kubernetes** view and select **Create cluster**.
3. Choose **kind**, not kubeadm, and create a cluster with **two nodes**: one
   control-plane node and one worker.
4. Enable **Show system containers (advanced)** so the kind node containers
   are visible to Docker commands. Wait for Kubernetes to report that it is
   running.

Changing an existing cluster's provisioning method or resetting it can remove
workloads. See Docker's
[Kubernetes setup instructions](https://docs.docker.com/desktop/use-desktop/kubernetes/)
for version-specific controls.

### Select a worker and inspect its runtime

```bash
k get nodes \
  -l '!node-role.kubernetes.io/control-plane,!node-role.kubernetes.io/master' \
  -o wide
```

Choose an untainted, schedulable worker from that output, not the control plane.
Set its exact name; Docker Desktop normally calls it `desktop-worker`:

```bash
export BREWLET_NODE="desktop-worker"
k wait "node/$BREWLET_NODE" --for=condition=Ready --timeout=5m
k describe node "$BREWLET_NODE"
k get node "$BREWLET_NODE" -o json | jq '{
  labels: (.metadata.labels | with_entries(select(.key | startswith("brewlet.sh/")))),
  annotations: ((.metadata.annotations // {}) | with_entries(select(.key | startswith("brewlet.sh/"))))
}'
docker exec "$BREWLET_NODE" ctr version
docker exec "$BREWLET_NODE" stat -fc %T /sys/fs/cgroup
docker exec "$BREWLET_NODE" sh -c '
  find /opt /usr/local/bin /etc/containerd -maxdepth 2 -iname "*brewlet*" -print &&
  sed -n "/brewlet/p" /etc/containerd/config.toml
'
```

If no worker is found, do not remove control-plane taints or reset the cluster.
Use a separate local cluster instead. The selected worker must be schedulable,
without `NoSchedule` or `NoExecute` taints.
`ctr version` must report **containerd 2.0 or newer** on the server, and `stat`
must print `cgroup2fs`. Select a newer kind/Kubernetes version in Docker Desktop
in a separate cluster if these requirements are not met.

The `docker exec` commands must reach the worker container. If they fail, check
that you selected kind, enabled system-container visibility, and are using the
Docker Desktop Docker context (`docker context ls`). Managed Desktop security
policies must permit node access and privileged provisioning; do not bypass
those policies.

### Decide whether to reuse, install, or stop

| What you found | Safe path |
|---|---|
| A healthy Brewlet release in namespace `brewlet`, with a ready worker advertising `temurin-21` | Confirm it matches the latest CLI version in step 2, then reuse it. Keep its values, JDK digest, profiles, and pool labels unchanged. Skip the fresh-install section; use the readiness checks in step 3. |
| No Brewlet cluster resources, no Brewlet node ownership, and no runtime files/configuration on the selected worker | Follow the fresh-install section in step 3. Existing unrelated namespaces do not need to be deleted. |
| A failed/pending release, an older or custom installation, resources in another namespace, or a terminating profile | Stop installation. Have its owner follow [upgrade/recovery guidance](installation.md#upgrading), or use an isolated cluster. Do not run `helm upgrade --install` over it. |
| No release, but `/opt/brewlet`, a Brewlet shim, containerd handler, owner labels, or other Brewlet resources remain | Treat this as an unmanaged or incomplete installation, not a clean node. Preserve the evidence and recover with its owner, or use an isolated cluster. Do not delete files, clear owner labels/finalizers, or mark the node ready manually. |

For an existing managed installation, inspect it before choosing the reuse path:

```bash
helm status brewlet --kube-context "$BREWLET_CONTEXT" -n brewlet
helm get values brewlet --kube-context "$BREWLET_CONTEXT" -n brewlet
k get nodeprofiles
k get javaapplications -A
k get nodes -l brewlet.sh/runtime=ready \
  -o custom-columns=NAME:.metadata.name,JDKS:.metadata.annotations.brewlet\\.sh/jdks
```

Run these commands only if the corresponding release and CRDs exist. A missing
resource is a reason to classify the installation, not a reason to reinstall it.

??? note "Optional: use an isolated kind cluster"

    If the existing cluster cannot be safely reused, the
    [kind CLI](https://kind.sigs.k8s.io/docs/user/quick-start/#installation) can
    create a separate cluster using Docker Desktop's Docker engine. This is
    **not** the Desktop-managed `docker-desktop` cluster. It needs additional
    CPU and memory. Install kind 0.33.0 for the pinned node image below.
    Kind nodes must use the Docker engine's native architecture. A global
    `DOCKER_DEFAULT_PLATFORM=linux/amd64` override on an Apple Silicon machine
    can make the Kubernetes API server fail to start under emulation. Clearing
    the override alone may still reuse a cached AMD64 image. Select the native
    manifest from the pinned image index explicitly:

    ```bash
    cat > "$BREWLET_WORK/kind.yaml" <<'EOF'
    kind: Cluster
    apiVersion: kind.x-k8s.io/v1alpha4
    nodes:
      - role: control-plane
      - role: worker
    EOF

    export BREWLET_KIND_CLUSTER="brewlet-${BREWLET_RUN}"
    KIND_ARCH="$(docker version --format '{{.Server.Arch}}')"
    KIND_DIGEST="$(
      docker buildx imagetools inspect --raw \
        kindest/node@sha256:099e049362a1526b2db71494e1947aae99bd16290d7c895f2b7ea312e3cbfaed |
        jq -er --arg arch "$KIND_ARCH" \
          '.manifests[] | select(.platform.os == "linux" and .platform.architecture == $arch) | .digest'
    )"
    env -u DOCKER_DEFAULT_PLATFORM kind create cluster --name "$BREWLET_KIND_CLUSTER" \
      --image "kindest/node@${KIND_DIGEST}" \
      --config "$BREWLET_WORK/kind.yaml" \
      --kubeconfig "$BREWLET_WORK/kubeconfig" --wait 180s
    export KUBECONFIG="$BREWLET_WORK/kubeconfig"
    export BREWLET_CONTEXT="kind-${BREWLET_KIND_CLUSTER}"
    export BREWLET_NODE="${BREWLET_KIND_CLUSTER}-worker"
    k get nodes -o wide
    ```

    The separate kubeconfig keeps your usual contexts unchanged. Repeat the
    worker inspection above with this `BREWLET_NODE`; do not reset it to
    `desktop-worker`. If creation fails, stop and inspect the kind output
    rather than changing the original cluster.

## 2. Obtain Brewlet and the example source

### Install or reuse the released CLI

Both local guides install the latest checksum-verified CLI in
`$HOME/.local/bin`. If you just completed the released-CLI setup in
[Getting started](getting-started.md#install-the-released-cli-recommended) in
this terminal, keep that CLI and `BREWLET_VERSION` and skip this block.
Otherwise, install or update it now; `--version latest` overrides any old
`BREWLET_VERSION` in the shell:

```bash
curl -fsSL https://brewlet.sh/install.sh | sh -s -- --version latest --install-dir "$HOME/.local/bin"
export PATH="$HOME/.local/bin:$PATH"

BREWLET_VERSION="$(brewlet version)"
export BREWLET_VERSION
printf 'Using Brewlet %s\n' "$BREWLET_VERSION"
```

The resolved `BREWLET_VERSION` selects the chart below, keeping the CLI and
cluster components aligned without hardcoding a release. If reusing Brewlet,
compare it with the **APP VERSION** from `helm list` in step 1. If they differ,
stop and follow the upgrade/recovery guidance or use an isolated cluster;
do not upgrade an existing installation just for this tutorial.

### Get or reuse the example source

Use the same checkout as Getting started, not a new copy under this run's work
directory. If you already have Brewlet source (including an extracted release
archive), set `BREWLET_SOURCE` to its absolute path first. Otherwise, both
guides default to `$HOME/brewlet-examples`, even in a new terminal:

```bash
export BREWLET_SOURCE="${BREWLET_SOURCE:-$HOME/brewlet-examples}"
if [ ! -e "$BREWLET_SOURCE" ]; then
  git clone --depth 1 --branch "v${BREWLET_VERSION}" \
    https://github.com/microsoft/brewlet.git "$BREWLET_SOURCE"
fi
if [ ! -f "$BREWLET_SOURCE/integration-tests/fixtures/demo-app/pom.xml" ] ||
   [ ! -f "$BREWLET_SOURCE/integration-tests/fixtures/spring-petclinic/build.sh" ]; then
  printf 'Stop: BREWLET_SOURCE must point to Brewlet source containing both examples.\n' >&2
  exit 1
fi
```

A new checkout uses the installed release's tag. Existing source stays
untouched: no second clone, checkout switch, or pull. If you keep source
elsewhere, set the same `BREWLET_SOURCE` in each new terminal. Only generated
cluster files and the local OCI layout belong in this run's `BREWLET_WORK`.

## 3. Prepare the Brewlet runtime

### Fresh installation only

**When reusing Brewlet, skip directly to the
[readiness checks](#readiness-checks-for-both-fresh-and-reused-installations).**
The next three subsections are for fresh installations only. Proceed only after
step 1 found no existing Brewlet installation or unmanaged runtime state.
Use `helm install`, not an upgrade, so a conflicting release fails rather than
changing its values.

For this local evaluation, use the Linux Temurin 21 JDK image. Resolve its
current image-index digest and inspect the available platforms:

```bash
JDK_DIGEST="$(
  docker buildx imagetools inspect docker.io/library/eclipse-temurin:21-jdk \
    --format '{{.Manifest.Digest}}'
)"
printf '%s\n' "$JDK_DIGEST" | grep -Eq '^sha256:[0-9a-f]{64}$'
docker buildx imagetools inspect "docker.io/library/eclipse-temurin@$JDK_DIGEST"
```

Confirm the image includes your worker's architecture. This resolves a mutable
vendor tag once, then pins the installation to that digest. It is an explicit
evaluation choice, not a Brewlet-provided runtime catalog or a production
approval policy.

Save the pool and JDK inventory before changing any cluster resources:

```bash
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

### Check node image pulls before installing

Render the released chart and pull its three digest-pinned component images
through the worker's **CRI**, the same image service kubelet uses. This checks
the node's registry/mirror path before creating a release, webhook, profile,
or pool label. A successful host-side `docker pull` or chart download does not
prove that Kubernetes can pull the component images.

```bash
helm template brewlet oci://ghcr.io/microsoft/charts/brewlet \
  --version "$BREWLET_VERSION" \
  --namespace brewlet \
  --values "$BREWLET_WORK/brewlet-local.yaml" \
  > "$BREWLET_WORK/brewlet-rendered.yaml"

if ! BREWLET_COMPONENT_IMAGES="$(
  grep -Eo 'ghcr\.io/microsoft/brewlet-(operator|admission|node-provisioner)@sha256:[0-9a-f]{64}' \
    "$BREWLET_WORK/brewlet-rendered.yaml" | sort -u
)" || [ "$(printf '%s\n' "$BREWLET_COMPONENT_IMAGES" | wc -l)" -ne 3 ]; then
  printf 'Stop: expected three digest-pinned component images in the released chart.\n' >&2
  exit 1
fi
while IFS= read -r image; do
  if ! docker exec "$BREWLET_NODE" crictl pull "$image"; then
    printf 'Stop: node image pull failed; do not label the worker or install Helm resources.\n' >&2
    exit 1
  fi
done <<< "$BREWLET_COMPONENT_IMAGES"
```

If this fails, inspect the pull error and see
[node image-pull failures](#node-image-pull-failures) below. No Brewlet
Kubernetes resources have been created by this fresh-install path yet.
On clusters with additional schedulable nodes, check their CRI pulls too:
the operator and admission deployments are not restricted to the runtime pool.

??? note "Optional: preview the Kubernetes resources"

    Open `$BREWLET_WORK/brewlet-rendered.yaml` to inspect the resources that
    the preflight rendered. Rendering does not change the cluster.

### Install on the selected worker

**Still fresh installations only.** Check pool ownership before adding a
label, so a conflicting pool does not leave this worker modified:

```bash
POOL_BEFORE="$(k get node "$BREWLET_NODE" \
  -o jsonpath='{.metadata.labels.brewlet\.sh/local-pool}')"
if [ -n "$POOL_BEFORE" ] && [ "$POOL_BEFORE" != local-java ]; then
  printf 'Stop: worker already belongs to pool %s; do not overwrite it.\n' "$POOL_BEFORE" >&2
  exit 1
fi
POOL_NODES="$(k get nodes -l brewlet.sh/local-pool=local-java \
  -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}')"
if [ -n "$POOL_NODES" ] && [ "$POOL_NODES" != "$BREWLET_NODE" ]; then
  printf 'Stop: local-java selects other nodes; do not provision this pool.\n' >&2
  exit 1
fi
if [ -z "$POOL_BEFORE" ]; then
  k label node "$BREWLET_NODE" brewlet.sh/local-pool=local-java
  export BREWLET_POOL_LABEL_ADDED=true
fi
```

The control plane remains excluded. There is no need to remove its taints or
enable `includeControlPlane`. The guard requires the pool to contain **only the
selected worker**; installing a broader profile would provision other nodes
too. An existing matching pool label is
reused, but is not owned by this run and must not be removed during cleanup.

Install with the selected inventory:

```bash
helm install brewlet oci://ghcr.io/microsoft/charts/brewlet \
  --kube-context "$BREWLET_CONTEXT" \
  --version "$BREWLET_VERSION" \
  --namespace brewlet \
  --create-namespace \
  --values "$BREWLET_WORK/brewlet-local.yaml"
export BREWLET_INSTALLED_HERE=true
```

Set `BREWLET_INSTALLED_HERE` only after this run's install succeeds. If Helm
fails, inspect `helm status` and the pods: it may have left a partial release.
Do not retry with an upgrade, blindly uninstall it, or proceed to the app.

### Readiness checks for both fresh and reused installations

```bash
k rollout status deployment/brewlet-operator -n brewlet --timeout=5m
k rollout status deployment/brewlet-admission -n brewlet --timeout=5m
k wait "node/$BREWLET_NODE" \
  --for=jsonpath='{.metadata.labels.brewlet\.sh/runtime}'=ready --timeout=10m
k get runtimeclass brewlet
```

Wait until the selected worker reports `brewlet.sh/runtime=ready`. This confirms
the provisioner installed and validated the node runtime; a successful Helm
command alone does not. Downloads and the containerd restart may take several
minutes.

??? note "Optional: inspect the runtime inventory"

    ```bash
    k get nodeprofiles
    k get nodes -l brewlet.sh/runtime=ready -L brewlet.sh/jdk.temurin-21
    ```

## 4. Build and package PetClinic

The shared source includes a script that fetches a pinned upstream PetClinic
revision and builds its executable Spring Boot JAR:

The script reserves `PETCLINIC_REF` for an upstream Git revision; keep the OCI
image name in `PETCLINIC_OCI_REF` instead. If an earlier tutorial exported an
OCI image name as `PETCLINIC_REF`, run `unset PETCLINIC_REF` **before building**
to restore the pinned revision. Otherwise, a retry fails with
`couldn't find remote ref`.

```bash
export FIXTURE_DIR="$BREWLET_SOURCE/integration-tests/fixtures/spring-petclinic"
"$FIXTURE_DIR/build.sh"

export BREWLET_STORE="$BREWLET_WORK/oci"
export PETCLINIC_OCI_REF="localhost/brewlet/${BREWLET_NAMESPACE}:local"
brewlet push "$FIXTURE_DIR/target/spring-petclinic.jar" "$PETCLINIC_OCI_REF" \
  --store "$BREWLET_STORE" \
  --format image
```

Use **`--format image`** for Kubernetes, not the native artifact format used
in the CLI-only quick start.

??? note "Optional: inspect the JAR and launch contract"

    ```bash
    test -f "$FIXTURE_DIR/target/spring-petclinic.jar"
    brewlet inspect "$PETCLINIC_OCI_REF" --store "$BREWLET_STORE"
    ```

    The launch contract should contain `entry.mode: jar` and
    `mainJar: spring-petclinic.jar`.

Despite its name, the Go CLI's `push` command writes a **local OCI layout**; it
does not upload to a registry. The `localhost/brewlet/...` reference is just the
image's name in that layout. Its repository is unique to this run, so it cannot
replace an earlier tutorial's image alias. No registry is listening on localhost.

## 5. Load the image into every eligible local node

Read the image-index digest from the layout and import the image into the
Kubernetes containerd namespace. A reused cluster may have more than one
Brewlet-enabled node; the application can land on any compatible node, not just
the worker you inspected:

```bash
PETCLINIC_DIGEST="$(
  jq -er --arg ref "$PETCLINIC_OCI_REF" \
    '.manifests[] | select(.annotations["org.opencontainers.image.ref.name"] == $ref) | .digest' \
    "$BREWLET_STORE/index.json"
)"
export PETCLINIC_IMAGE="${PETCLINIC_OCI_REF%:*}@${PETCLINIC_DIGEST}"

BREWLET_NODES="$(
  k get nodes -l 'brewlet.sh/runtime=ready,brewlet.sh/jdk.temurin-21=true' \
    -o json | jq -er '.items[].metadata.name'
)"
printf 'Image will be loaded on:\n%s\n' "$BREWLET_NODES"
while IFS= read -r node; do
  docker exec "$node" ctr version
done <<< "$BREWLET_NODES"
```

If the list is empty or any node is not a local Docker container, stop. Do not
deploy with `Never` until every eligible node has the image; use
[registry publication](building-and-publishing.md#3-publish-the-artifact) for
non-local nodes instead.

```bash
while IFS= read -r node; do
  COPYFILE_DISABLE=1 tar -C "$BREWLET_STORE" -cf - oci-layout index.json blobs |
    docker exec -i "$node" ctr -n k8s.io images import --digests -
  docker exec "$node" ctr -n k8s.io images tag \
    "$PETCLINIC_OCI_REF" "$PETCLINIC_IMAGE"
done <<< "$BREWLET_NODES"
```

??? note "Optional: verify the imported image through CRI"

    ```bash
    while IFS= read -r node; do
      docker exec "$node" crictl inspecti "$PETCLINIC_IMAGE"
    done <<< "$BREWLET_NODES"
    ```

Importing into Docker Desktop's ordinary image store is not enough: the kind
worker has its own containerd store. The explicit `repository@sha256:...` alias
also makes the digest-pinned identity available to Kubernetes.

If you later enable more workers, load the image on those workers too, or
[publish it to a registry](building-and-publishing.md#3-publish-the-artifact)
and use `IfNotPresent` instead of `Never`.

## 6. Deploy the application

Create an isolated namespace:

```bash
k create namespace "$BREWLET_NAMESPACE"
k label namespace "$BREWLET_NAMESPACE" brewlet.sh/tutorial-run="$BREWLET_RUN"
```

??? note "Optional: run platform diagnostics"

    ```bash
    brewlet doctor --context "$BREWLET_CONTEXT" --namespace "$BREWLET_NAMESPACE"
    ```

Save and apply the descriptor. It uses the digest from the previous step, the
node's Temurin 21 JDK, one replica, and a readiness check. There is no autoscaler,
external database, or ingress controller to install:

```bash
cat > "$BREWLET_WORK/petclinic.yaml" <<EOF
apiVersion: apps.brewlet.sh/v1alpha1
kind: JavaApplication
metadata:
  name: petclinic
  namespace: ${BREWLET_NAMESPACE}
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
    requests:
      cpu: 500m
      memory: 512Mi
    limits:
      cpu: "1"
      memory: 768Mi
  env:
    - name: SPRING_PROFILES_ACTIVE
      value: default
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
      httpGet:
        path: /actuator/health
        port: 8080
      periodSeconds: 5
      timeoutSeconds: 3
EOF

k apply -f "$BREWLET_WORK/petclinic.yaml"
k wait --for=condition=Ready javaapplication/petclinic \
  -n "$BREWLET_NAMESPACE" --timeout=5m
```

The operator creates the Deployment and Service; the node-resident JDK runs
the application under its pod's resource limits. `pullPolicy: Never` is
intentional for this preloaded local image.

??? note "Optional: inspect the generated workload and logs"

    ```bash
    k get javaapplication,deployment,pod,service -n "$BREWLET_NAMESPACE" -o wide
    k logs deployment/petclinic -n "$BREWLET_NAMESPACE" --tail=50
    k get deployment petclinic -n "$BREWLET_NAMESPACE" \
      -o jsonpath='{.spec.template.spec.runtimeClassName}{"\n"}'
    ```

    The last command must print `brewlet`.

## 7. Open PetClinic

In the same terminal, forward the Service to your laptop:

```bash
k -n "$BREWLET_NAMESPACE" port-forward service/petclinic 18080:8080
```

Keep that command running. Open <http://localhost:18080> in your browser, then
choose **Find Owners** to explore the sample data.

??? note "Optional: check the health endpoint"

    In a second terminal:

    ```bash
    curl -fsS http://127.0.0.1:18080/actuator/health
    ```

    Expect `"status":"UP"`.

You have now run an ordinary Spring Boot application on local Kubernetes with
Brewlet supplying the node JDK, rather than packaging a JDK in every application
image.

## If something does not become ready

Inspect the failing resource before resetting or reinstalling anything:

```bash
k get pods -n brewlet -o wide
k logs -n brewlet -l app=brewlet-node-provisioner \
  --all-containers --tail=100
k describe node "$BREWLET_NODE"
k describe javaapplication petclinic -n "$BREWLET_NAMESPACE"
k describe pods -n "$BREWLET_NAMESPACE"
k get events -n "$BREWLET_NAMESPACE" --sort-by=.lastTimestamp
```

| Symptom | What to check |
|---|---|
| Worker never gets `brewlet.sh/runtime=ready` | Check the pool label, `brewlet.sh/provision-error` node annotation, provisioner logs, containerd version, and cgroup v2. Do not label the node ready manually. |
| Brewlet components show `ImagePullBackOff` | Read `k describe pods -n brewlet` and `k get events -n brewlet --sort-by=.lastTimestamp`. An `unexpected EOF`/`short read` can come from the node's registry mirror, not package permissions; see [node image-pull failures](#node-image-pull-failures). |
| PetClinic shows `ErrImageNeverPull` | Inspect the node shown by `k get pods -n "$BREWLET_NAMESPACE" -o wide`. Verify the exact digest-pinned reference with `crictl inspecti` and load the image on all eligible nodes. |
| Pod stays `Pending` or admission reports `NoCompatibleJDK` | Wait for the worker's Temurin 21 inventory, and check its taints and available CPU/memory. Do not remove control-plane taints to work around a missing worker. |
| PetClinic exits or readiness keeps failing | Read `k logs deployment/petclinic -n "$BREWLET_NAMESPACE"`; for a restarted container also use `--previous`. Check for `OOMKilled` and available Docker Desktop memory. |
| Port-forward cannot bind port 18080 | Use `18081:8080` instead and open <http://localhost:18081>. Do not stop an unrelated process to reclaim the port. |
| A rerun reports an existing namespace, release, or image alias | Do not add `--force`, overwrite a label, or delete the conflicting resource. Recover this run's variables, or start with a fresh work directory and unique namespace. Reuse a healthy platform rather than reinstalling it. |

See [Troubleshooting](troubleshooting.md) for runtime diagnostics.

### Node image-pull failures

Docker Desktop's kind nodes can use a registry mirror configured under
`/etc/containerd/certs.d/_default/hosts.toml`. In a reproduced failure, that
mirror returned an empty GHCR child-manifest response and CRI reported
`short read: expected 3504 bytes but got 0: unexpected EOF`. The same released
digest pulled successfully directly from GHCR. Helm still reported the release
as `deployed`, although neither deployment could start.

Inspect the node configuration without editing it:

```bash
docker exec "$BREWLET_NODE" sh -c \
  'find /etc/containerd/certs.d -name hosts.toml -print -exec cat {} \;'
```

A chart pull checks Helm's registry path; `crictl pull` checks Kubernetes'
path. Do not reset the cluster, change package visibility, or remove registry
security controls to fix a truncated response. Repair the mirror with its
owner, or use the isolated kind cluster from step 1.
For authorization errors instead, see
[released-package access troubleshooting](installation.md#package-access-troubleshooting).

On a local development node where direct access to these public registries is
permitted, you can also preload the **same chart-pinned digests** directly,
without changing the mirror configuration or the chart:

```bash
BREWLET_NODE_ARCH="$(k get node "$BREWLET_NODE" \
  -o jsonpath='{.metadata.labels.kubernetes\.io/arch}')"
while IFS= read -r image; do
  docker exec "$BREWLET_NODE" ctr -n k8s.io images pull \
    --platform "linux/$BREWLET_NODE_ARCH" "$image"
  docker exec "$BREWLET_NODE" crictl inspecti "$image"
done <<< "$BREWLET_COMPONENT_IMAGES"
```

Use the preflight's `BREWLET_COMPONENT_IMAGES`, not guessed tags. Then repeat
the CRI preflight before installing. For an already failed release, recover its
exact image references from `helm get manifest` and inspect its state instead
of installing again. The uninstall hook needs the operator image, and node
cleanup needs the provisioner image; restore image availability before
uninstalling. Never bypass hooks or remove finalizers to work around a pull
failure.

## Clean up

Stop port-forwarding with **Ctrl+C**. In that terminal, verify the namespace's
run marker before deleting **only this run's namespace**:

```bash
RUN_OWNER="$(k get namespace "$BREWLET_NAMESPACE" \
  -o jsonpath='{.metadata.labels.brewlet\.sh/tutorial-run}')"
if [ "$RUN_OWNER" != "$BREWLET_RUN" ]; then
  printf 'Stop: namespace is not owned by this tutorial run.\n' >&2
  exit 1
fi
k delete namespace "$BREWLET_NAMESPACE" --wait=true --timeout=120s
```

If you **reused** Brewlet, stop here: leave its release, profiles, node labels,
JDKs, and CRDs intact. Do not prune the shared containerd store.

If you **installed** Brewlet in this run and no other workloads or profiles now
depend on it, inspect the platform and then remove only that installation:

```bash
k get javaapplications -A
k get pods -A -o json | jq -r \
  '.items[] | select(.spec.runtimeClassName == "brewlet") | [.metadata.namespace, .metadata.name] | @tsv'
k get nodeprofiles
```

Do not proceed if other users or applications have started using this runtime.
With only this run's profile remaining:

```bash
if [ "$BREWLET_INSTALLED_HERE" = true ]; then
  helm uninstall brewlet --kube-context "$BREWLET_CONTEXT" -n brewlet --timeout 5m
fi
if [ "$BREWLET_POOL_LABEL_ADDED" = true ]; then
  POOL_NOW="$(k get node "$BREWLET_NODE" \
    -o jsonpath='{.metadata.labels.brewlet\.sh/local-pool}')"
  if [ "$POOL_NOW" != local-java ]; then
    printf 'Stop: pool label changed; leave it for its owner.\n' >&2
    exit 1
  fi
  k label node "$BREWLET_NODE" brewlet.sh/local-pool-
fi
```

Helm's uninstall hook waits for profile cleanup while the operator is still
running. If it fails, stop before removing the pool label and follow
[uninstall guidance](installation.md#uninstall); do not remove cleanup
finalizers or delete the Brewlet namespace to force it through.

The shared source checkout, local OCI layout, and imported application image remain
available for inspection. Helm may retain CRDs: their presence on a subsequent
run is a recovery/reuse decision, not permission to blindly install over them.
Never use Docker Desktop's **Reset cluster** as tutorial cleanup.

If you created the optional isolated kind cluster, and nobody else has started
using it, you can discard that exact cluster instead of keeping it:

```bash
kind delete cluster --name "$BREWLET_KIND_CLUSTER" \
  --kubeconfig "$BREWLET_WORK/kubeconfig"
```

Do not run this for a pre-existing cluster. It removes every resource in the
named isolated cluster, not just PetClinic.

## Where to go next

- [Explore the CLI without Kubernetes](getting-started.md), reusing this CLI
  and `BREWLET_SOURCE` checkout.
- [Build and publish your own application](building-and-publishing.md).
- [Configure a shared cluster](installation.md) with your own JDK policy.
- [Split platform and developer responsibilities](workshops/index.md).
