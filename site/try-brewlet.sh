#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

if [[ "${BASH_SOURCE[0]}" != "$0" ]]; then
  printf 'Run this script with bash; do not source it into your terminal.\n' >&2
  return 1
fi
set -Eeuo pipefail

kind_version=0.33.0
kubectl_version=1.36.4
helm_version=4.3.0
jq_version=1.8.1
kind_image=kindest/node@sha256:099e049362a1526b2db71494e1947aae99bd16290d7c895f2b7ea312e3cbfaed
builder_image=docker.io/library/maven@sha256:c2a2c58516d160f43b50f12baa427ca86989e0bc942609e04aff61da5d9a7d74
petclinic_revision=b3ee2c53e76e9267f03551a7cd36b0983c859c56
work=""
cluster=""
cluster_started=false
forward_pid=""
builder_pid=""

die() { printf 'Error: %s\n' "$*" >&2; exit 1; }
step() { printf '\n%s\n' "$*"; }
download() { curl -q -fLsS --retry 2 --connect-timeout 15 --max-time 300 "$1" -o "$2"; }
k() { "$work/bin/kubectl" --kubeconfig "$work/kubeconfig" --context "kind-$cluster" --cache-dir "$work/kubectl-cache" "$@"; }
h() { "$work/bin/helm" --kubeconfig "$work/kubeconfig" --kube-context "kind-$cluster" "$@"; }

verify() {
  local actual
  [[ "$2" =~ ^[0-9a-f]{64}$ ]] || die "Missing or invalid checksum for $1"
  if command -v sha256sum >/dev/null 2>&1; then
    actual="$(sha256sum "$1" | awk '{print $1}')"
  else
    actual="$(shasum -a 256 "$1" | awk '{print $1}')"
  fi
  [[ "$actual" == "$2" ]] || die "Checksum verification failed for $1"
}

verified_download() {
  download "$1" "$3"
  download "$2" "$3.sha256"
  verify "$3" "$(awk 'NR == 1 {print $1}' "$3.sha256")"
}

cleanup() {
  local result=$? cid
  trap - EXIT INT TERM ERR
  set +e
  if [[ -n "$builder_pid" ]]; then
    if kill -0 "$builder_pid" 2>/dev/null; then
      kill -KILL "$builder_pid" 2>/dev/null
    fi
    wait "$builder_pid" 2>/dev/null
  fi
  if [[ -n "$forward_pid" ]]; then
    if kill -0 "$forward_pid" 2>/dev/null; then
      kill "$forward_pid"
    fi
    wait "$forward_pid" 2>/dev/null || :
  fi
  [[ -n "$work" ]] || exit "$result"
  if [[ -f "$work/builder.cid" ]]; then
    cid="$(cat "$work/builder.cid")"
    if docker container inspect "$cid" > /dev/null 2>&1; then
      if [[ "$(docker inspect --format '{{index .Config.Labels "brewlet.sh/demo"}}' "$cid")" == "$cluster" ]]; then
        if ! docker rm -f "$cid" >> "$work/demo.log" 2>&1; then
          printf 'Could not remove the demo build container %s. See %s/demo.log\n' "$cid" "$work" >&2
          result=1
        fi
      else
        printf 'Build container ownership changed; leaving %s untouched.\n' "$cid" >&2
        result=1
      fi
    fi
  fi
  if [[ "$cluster_started" == true ]]; then
    step "Collecting diagnostics and removing disposable cluster $cluster..."
    if ! "$work/bin/kind" export logs "$work/cluster-logs" --name "$cluster" >> "$work/demo.log" 2>&1; then
      printf 'Some cluster diagnostics could not be collected; see %s/demo.log\n' "$work" >&2
    fi
    if ! "$work/bin/kind" delete cluster --name "$cluster" --kubeconfig "$work/kubeconfig" >> "$work/demo.log" 2>&1; then
      printf 'Cluster cleanup failed. Tools and diagnostics are retained in %s\n' "$work" >&2
      printf 'Retry cleanup against the same Docker engine:\n' >&2
      cat "$work/cleanup-command.txt" >&2
      exit 1
    fi
    printf 'Removed only the disposable cluster %s.\n' "$cluster"
  fi
  # These directories were created inside this run's private mktemp directory.
  if ! rm -rf "$work/downloads" "$work/source" "$work/oci" "$work/helm-cache" "$work/kubectl-cache" ||
      ! rm -f "$work/petclinic.jar"; then
    printf 'Some local demo files could not be removed from %s.\n' "$work" >&2
    result=1
  fi
  printf 'Diagnostics and downloaded tools: %s\n' "$work"
  exit "$result"
}

main() {
  [[ "$#" -eq 0 ]] || die "Usage: bash try-brewlet.sh"
  local command os arch jq_os endpoint cpus memory node_arch kind_digest
  local version jdk_digest components image image_ref image_digest port attempt
  for command in docker curl tar awk grep sort tr mktemp chmod id; do
    command -v "$command" >/dev/null 2>&1 || die "Required command not found: $command"
  done
  if ! command -v sha256sum >/dev/null 2>&1 && ! command -v shasum >/dev/null 2>&1; then
    die "Install sha256sum or shasum to verify downloads."
  fi
  case "$(uname -s)" in
    Darwin) os=darwin; jq_os=macos ;;
    Linux) os=linux; jq_os=linux ;;
    *) die "Use a macOS, Linux, or WSL terminal, not Windows PowerShell." ;;
  esac
  case "$(uname -m)" in
    arm64|aarch64) arch=arm64 ;;
    x86_64|amd64) arch=amd64 ;;
    *) die "This demo supports amd64 and arm64 hosts." ;;
  esac
  if [[ -n "${DOCKER_CONTEXT:-}" || -z "${DOCKER_HOST:-}" ]]; then
    endpoint="$(docker context inspect --format '{{.Endpoints.docker.Host}}')"
  else
    endpoint="$DOCKER_HOST"
  fi
  case "$endpoint" in
    unix://*) ;;
    *) die "Select a local Docker engine first. This demo will not use a remote Docker endpoint: $endpoint" ;;
  esac
  # Pin the selected local engine for this child process, even if another terminal switches context.
  export DOCKER_HOST="$endpoint"
  unset DOCKER_CONTEXT DOCKER_DEFAULT_PLATFORM DOCKER_TLS_VERIFY DOCKER_CERT_PATH
  docker info > /dev/null 2>&1 || die "Start Docker Desktop (with WSL integration on Windows) or your local Docker engine."
  docker buildx version > /dev/null 2>&1 || die "Docker Buildx is required; it is included with Docker Desktop."
  cpus="$(docker info --format '{{.NCPU}}')"
  memory="$(docker info --format '{{.MemTotal}}')"
  [[ "$cpus" -ge 4 && "$memory" -ge 7516192768 ]] ||
    die "Allow at least 4 CPUs and about 8 GiB of Docker memory, plus headroom for existing workloads. No settings were changed."
  node_arch="$(docker version --format '{{.Server.Arch}}')"
  case "$node_arch" in amd64|arm64) ;; *) die "Unsupported Docker engine architecture: $node_arch" ;; esac

  work="$(mktemp -d "${TMPDIR:-/tmp}/brewlet-demo.XXXXXXXX")"
  work="$(cd "$work" && pwd -P)"
  cluster="$(basename "$work" | tr '[:upper:].' '[:lower:]-')"
  mkdir -p "$work/bin" "$work/downloads" "$work/source"
  export KUBECONFIG="$work/kubeconfig"
  export KIND_EXPERIMENTAL_PROVIDER=docker
  unset KIND_EXPERIMENTAL_DOCKER_NETWORK KUBERNETES_MASTER HELM_DRIVER HELM_DRIVER_SQL_CONNECTION_STRING
  unset HELM_KUBEAPISERVER HELM_KUBETOKEN HELM_KUBECAFILE HELM_KUBEASUSER HELM_KUBEASGROUPS
  unset HELM_KUBECONTEXT HELM_KUBETLS_SERVER_NAME HELM_KUBEINSECURE_SKIP_TLS_VERIFY
  export HELM_CACHE_HOME="$work/helm-cache" HELM_CONFIG_HOME="$work/helm-config" HELM_DATA_HOME="$work/helm-data"
  export HELM_REGISTRY_CONFIG="$work/helm-config/registry.json"
  export HELM_REPOSITORY_CONFIG="$work/helm-config/repositories.yaml" HELM_REPOSITORY_CACHE="$work/helm-cache/repository"
  export HELM_PLUGINS="$work/helm-plugins"
  printf 'Private workspace: %s\nDisposable cluster: %s\n' "$work" "$cluster"
  printf 'Your existing Kubernetes clusters and current context will not be changed.\n'

  step "Downloading checksum-verified tools into the private workspace..."
  verified_download "https://kind.sigs.k8s.io/dl/v$kind_version/kind-$os-$arch" \
    "https://kind.sigs.k8s.io/dl/v$kind_version/kind-$os-$arch.sha256sum" "$work/bin/kind"
  verified_download "https://dl.k8s.io/release/v$kubectl_version/bin/$os/$arch/kubectl" \
    "https://dl.k8s.io/release/v$kubectl_version/bin/$os/$arch/kubectl.sha256" "$work/bin/kubectl"
  verified_download "https://get.helm.sh/helm-v$helm_version-$os-$arch.tar.gz" \
    "https://get.helm.sh/helm-v$helm_version-$os-$arch.tar.gz.sha256sum" "$work/downloads/helm.tar.gz"
  tar -xzf "$work/downloads/helm.tar.gz" -C "$work/downloads" "$os-$arch/helm"
  cp "$work/downloads/$os-$arch/helm" "$work/bin/helm"
  download "https://github.com/jqlang/jq/releases/download/jq-$jq_version/jq-$jq_os-$arch" "$work/bin/jq"
  download "https://github.com/jqlang/jq/releases/download/jq-$jq_version/sha256sum.txt" "$work/downloads/jq-checksums.txt"
  verify "$work/bin/jq" "$(awk -v name="jq-$jq_os-$arch" '$2 == name {print $1}' "$work/downloads/jq-checksums.txt")"
  chmod +x "$work/bin/kind" "$work/bin/kubectl" "$work/bin/helm" "$work/bin/jq"
  export PATH="$work/bin:$PATH"
  download https://brewlet.sh/install.sh "$work/downloads/install.sh"
  sh "$work/downloads/install.sh" --version latest --install-dir "$work/bin" >> "$work/demo.log" 2>&1
  version="$("$work/bin/brewlet" version)"
  [[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || die "Unexpected release version: $version"
  printf 'Using released Brewlet %s.\n' "$version"

  step "Building PetClinic with JDK 21 in a temporary container (no host Java or Maven needed)..."
  download "https://github.com/spring-projects/spring-petclinic/archive/$petclinic_revision.tar.gz" "$work/downloads/source.tar.gz"
  tar -xzf "$work/downloads/source.tar.gz" --strip-components=1 -C "$work/source"
  mkdir -p "$work/source/.home"
  docker run --rm --platform "linux/$node_arch" --cidfile "$work/builder.cid" \
    --label "brewlet.sh/demo=$cluster" --user "$(id -u):$(id -g)" \
    --volume "$work/source:/work" --workdir /work \
    --env HOME=/work/.home --env MAVEN_CONFIG=/work/.home/.m2 \
    --env MAVEN_OPTS=-Duser.home=/work/.home "$builder_image" \
    mvn -q -B -DskipTests -Dcheckstyle.skip=true -Dspotless.check.skip=true -Denforcer.skip=true \
    package >> "$work/demo.log" 2>&1 &
  builder_pid=$!
  wait "$builder_pid"
  builder_pid=""
  set -- "$work/source/target/"*.jar
  [[ "$#" -eq 1 && -f "$1" ]] || die "Expected one executable PetClinic JAR after the build."
  cp "$1" "$work/petclinic.jar"
  image_ref="localhost/brewlet/$cluster:local"
  "$work/bin/brewlet" push "$work/petclinic.jar" "$image_ref" \
    --store "$work/oci" --format image >> "$work/demo.log" 2>&1
  image_digest="$(jq -er --arg ref "$image_ref" \
    '.manifests[] | select(.annotations["org.opencontainers.image.ref.name"] == $ref) | .digest' "$work/oci/index.json")"

  step "Creating a separate two-node kind cluster on the Docker engine's native architecture..."
  kind_digest="$(docker buildx imagetools inspect --raw "$kind_image" |
    jq -er --arg arch "$node_arch" '.manifests[] | select(.platform.os == "linux" and .platform.architecture == $arch) | .digest')"
  [[ "$kind_digest" =~ ^sha256:[0-9a-f]{64}$ ]] || die "No native kind image found for $node_arch"
  cat > "$work/kind.yaml" <<'EOF'
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
  - role: control-plane
  - role: worker
    labels:
      brewlet.sh/local-pool: local-java
EOF
  "$work/bin/kind" get clusters > "$work/existing-clusters.txt"
  if grep -Fxq "$cluster" "$work/existing-clusters.txt"; then
    die "Cluster name already exists; refusing to adopt or delete it: $cluster"
  fi
  printf 'env -u DOCKER_CONTEXT -u DOCKER_TLS_VERIFY -u DOCKER_CERT_PATH DOCKER_HOST=%q KIND_EXPERIMENTAL_PROVIDER=docker %q delete cluster --name %q --kubeconfig %q\n' \
    "$DOCKER_HOST" "$work/bin/kind" "$cluster" "$work/kubeconfig" > "$work/cleanup-command.txt"
  # Retain partial nodes for diagnostics; the EXIT handler owns cleanup after this point.
  cluster_started=true
  "$work/bin/kind" create cluster --name "$cluster" --image "kindest/node@$kind_digest" \
    --config "$work/kind.yaml" --kubeconfig "$work/kubeconfig" --wait 180s --retain >> "$work/demo.log" 2>&1
  k wait "node/$cluster-worker" --for=condition=Ready --timeout=120s >> "$work/demo.log" 2>&1

  step "Installing Brewlet only in the disposable cluster..."
  jdk_digest="$(docker buildx imagetools inspect docker.io/library/eclipse-temurin:21-jdk --format '{{.Manifest.Digest}}')"
  [[ "$jdk_digest" =~ ^sha256:[0-9a-f]{64}$ ]] || die "Could not resolve the Temurin 21 runtime digest"
  cat > "$work/brewlet-local.yaml" <<EOF
provisioner:
  pools: [local-java]
  poolKey: brewlet.sh/local-pool
  jdks:
    - distribution: temurin
      feature: 21
      source:
        image: docker.io/library/eclipse-temurin@${jdk_digest}
        javaHome: /opt/java/openjdk
EOF
  h template brewlet oci://ghcr.io/microsoft/charts/brewlet --version "$version" \
    --namespace brewlet --values "$work/brewlet-local.yaml" > "$work/brewlet-rendered.yaml"
  components="$(grep -Eo 'ghcr\.io/microsoft/brewlet-(operator|admission|node-provisioner)@sha256:[0-9a-f]{64}' \
    "$work/brewlet-rendered.yaml" | sort -u)"
  [[ "$(printf '%s\n' "$components" | wc -l)" -eq 3 ]] || die "The chart must contain three digest-pinned component images."
  while IFS= read -r image; do
    docker exec "$cluster-worker" crictl pull "$image" >> "$work/demo.log" 2>&1
  done <<< "$components"
  h install brewlet oci://ghcr.io/microsoft/charts/brewlet --version "$version" \
    --namespace brewlet --create-namespace --values "$work/brewlet-local.yaml" >> "$work/demo.log" 2>&1
  k rollout status deployment/brewlet-operator -n brewlet --timeout=5m >> "$work/demo.log" 2>&1
  k rollout status deployment/brewlet-admission -n brewlet --timeout=5m >> "$work/demo.log" 2>&1
  k wait "node/$cluster-worker" --for=jsonpath='{.metadata.labels.brewlet\.sh/runtime}'=ready --timeout=10m >> "$work/demo.log" 2>&1

  step "Loading the JAR-only image and starting PetClinic..."
  COPYFILE_DISABLE=1 tar -C "$work/oci" -cf - oci-layout index.json blobs |
    docker exec -i "$cluster-worker" ctr -n k8s.io images import --digests - >> "$work/demo.log" 2>&1
  image="${image_ref%:*}@$image_digest"
  docker exec "$cluster-worker" ctr -n k8s.io images tag "$image_ref" "$image" >> "$work/demo.log" 2>&1
  k create namespace petclinic >> "$work/demo.log" 2>&1
  cat > "$work/petclinic.yaml" <<EOF
apiVersion: apps.brewlet.sh/v1alpha1
kind: JavaApplication
metadata:
  name: petclinic
  namespace: petclinic
spec:
  artifact:
    image: ${image}
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
      httpGet: {path: /actuator/health, port: 8080}
      periodSeconds: 5
      timeoutSeconds: 3
EOF
  k apply -f "$work/petclinic.yaml" >> "$work/demo.log" 2>&1
  k wait --for=condition=Ready javaapplication/petclinic -n petclinic --timeout=5m >> "$work/demo.log" 2>&1
  "$work/bin/kubectl" --kubeconfig "$work/kubeconfig" --context "kind-$cluster" \
    --cache-dir "$work/kubectl-cache" -n petclinic port-forward \
    --address 127.0.0.1 service/petclinic :8080 > "$work/port-forward.log" 2>&1 &
  forward_pid=$!
  port=""
  for attempt in {1..60}; do
    kill -0 "$forward_pid" 2>/dev/null || die "Port forwarding stopped; see $work/port-forward.log"
    port="$(sed -n 's/^Forwarding from 127\.0\.0\.1:\([0-9]*\) -> 8080$/\1/p' "$work/port-forward.log")"
    if [[ -n "$port" ]] && curl -q -fsS --max-time 3 "http://127.0.0.1:$port/actuator/health" > "$work/health.json" 2>/dev/null; then
      if jq -e '.status == "UP"' "$work/health.json" >/dev/null; then
        break
      fi
    fi
    sleep 1
  done
  [[ -n "$port" ]] && jq -e '.status == "UP"' "$work/health.json" >/dev/null ||
    die "PetClinic did not become healthy through the local port forward."
  printf '\nPetClinic is ready: http://127.0.0.1:%s\n' "$port"
  printf 'Open Find Owners to explore the sample data.\nPress Ctrl+C here to delete this demo cluster and return to your terminal.\n'
  wait "$forward_pid"
  die "Port forwarding ended unexpectedly; the demo will be cleaned up."
}

trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
trap 'printf "Demo failed at line %s. See %s/demo.log for details.\n" "$LINENO" "$work" >&2' ERR
main "$@"
