# Your first local Kubernetes deployment

Run a real HTTP application, rotate its runtime without rebuilding it, and
remove the installation normally. This is a small reader tutorial, not the
complete acceptance suite.

**Scope:** one new disposable kind cluster, source-built components, one native
CPU architecture and preimported verified OCI graphs. It does not test remote
registry credentials/TLS, cloud autoscaling or another architecture.
No released 2.0 installer or host Java/Node/.NET toolchain is needed.

## Prerequisites

Use a clean checkout of `brewlet-2.0`, Bash, Go 1.26+, Python 3.12+, Docker with
BuildKit and Linux containers, kind **v0.33.0**, kubectl, Helm and tar.
Allow several GB of Docker storage and time for the first source build.
Provisioning modifies only the tutorial's new node.

Start in the repository root and keep the following steps in the **same
terminal**. The private kubeconfig never uses your current cluster.

## 1. Create an isolated work area

```sh
# tutorial-step: setup
set -euo pipefail
WORK="$(mktemp -d "$PWD/integration-tests/live-v2/run-reader-XXXXXXXX")"
RUN_ID="$(python3 -c 'import uuid; print(uuid.uuid4().hex[:12])')"
export CLUSTER="brewlet-reader-$RUN_ID" CONTEXT="kind-brewlet-reader-$RUN_ID"
export KUBECONFIG="$WORK/kubeconfig" APP_NAMESPACE="app-$RUN_ID"
export RID="python-$RUN_ID" PROFILE="profile-$RUN_ID"
export WORK
printf 'Retained tutorial work: %s\nOwned cluster: %s\n' "$WORK" "$CLUSTER"
test "$(kind version | awk '{print $2}')" = v0.33.0
test "$(docker info --format '{{.OSType}}')" = linux
case "$(docker info --format '{{.Architecture}}')" in
  arm64|aarch64) ARCH=arm64 ;;
  amd64|x86_64) ARCH=amd64 ;;
  *) echo "Unsupported Docker CPU architecture" >&2; exit 1 ;;
esac
export ARCH
export DOCKER_DEFAULT_PLATFORM="linux/$ARCH"
if kind get clusters | grep -Fxq "$CLUSTER"; then
  echo "Refusing an existing cluster name" >&2; exit 1
fi
mkdir "$WORK/context"
git archive HEAD go.mod go.sum pkg kubernetes cmd provisioner shim scripts LICENSE.txt examples/local \
  | tar -x -C "$WORK/context"
k() { kubectl --kubeconfig "$KUBECONFIG" --context "$CONTEXT" --request-timeout=30s "$@"; }
```

The Docker context contains committed source, not this work area's kubeconfig
or unrelated files. Use a committed revision containing this tutorial.

## 2. Build matching components and the application artifact

The Linux producer avoids macOS filesystem extended attributes. Its app is a
stdlib Python HTTP server with an explicit absolute launch path.

```sh
# tutorial-step: build
CONTROL="brewlet-reader-control:$RUN_ID"
WORKER="brewlet-reader-node:$RUN_ID"
docker build --platform "linux/$ARCH" -f "$WORK/context/kubernetes/Dockerfile" \
  -t "$CONTROL" "$WORK/context"
docker build --platform "linux/$ARCH" -f "$WORK/context/provisioner/Dockerfile" \
  -t "$WORKER" "$WORK/context"
export CONTROL_ID="$(docker image inspect --format '{{.Id}}' "$CONTROL")"
export WORKER_ID="$(docker image inspect --format '{{.Id}}' "$WORKER")"
printf '%s\n' "$CONTROL_ID" > "$WORK/control-image-id"
printf '%s\n' "$WORKER_ID" > "$WORK/worker-image-id"
docker build --platform "linux/$ARCH" -f "$WORK/context/examples/local/producer.Dockerfile" \
  --target artifact --output "type=local,dest=$WORK/application" "$WORK/context"
APP_DIGEST="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["digest"])' "$WORK/application/push.json")"
export APP_REF="example.invalid/brewlet/hello@$APP_DIGEST"
printf 'Application: %s\n' "$APP_REF"
```

## 3. Import pinned, unchanged runtime sources

The source tree's OCI import helper copies verified public blobs; it does not
run the image or derive a custom root. The matrix pins both architectures.
These two sources preserve the same runtime/ABI promise; different root bytes
do not necessarily imply a different reported Python version or a specific CVE fix.

```sh
# tutorial-step: sources
export RUNTIME_BEFORE="$(python3 -c 'import json,os; print(json.load(open("integration-tests/fixtures/runtimes/upstream.json"))["python"]["previous"][os.environ["ARCH"]])')"
export RUNTIME_AFTER="$(python3 -c 'import json,os; print(json.load(open("integration-tests/fixtures/runtimes/upstream.json"))["python"][os.environ["ARCH"]])')"
go run ./integration-tests/live-v2/runtimeimage --source "$RUNTIME_BEFORE" \
  --output "$WORK/runtime-before" > "$WORK/runtime-before.json"
go run ./integration-tests/live-v2/runtimeimage --source "$RUNTIME_AFTER" \
  --output "$WORK/runtime-after" > "$WORK/runtime-after.json"
```

## 4. Create the private node and load OCI content

```sh
# tutorial-step: cluster
kind create cluster --name "$CLUSTER" --kubeconfig "$KUBECONFIG" \
  --image kindest/node@sha256:4613778f3cfcd10e615029370f5786704559103cf27bef934597ba562b269661 \
  --wait 180s
export NODE="$CLUSTER-control-plane"
export NODE_ID="$(docker inspect --format '{{.Id}}' "$NODE")"
NODE_IMAGE_ID="$(docker inspect --format '{{.Image}}' "$NODE_ID")"
test "$(docker image inspect --format '{{.Architecture}}' "$NODE_IMAGE_ID")" = "$ARCH"
export NODE_UID="$(k get node "$NODE" -o jsonpath='{.metadata.uid}')"
python3 - <<'PY'
import json, os
from pathlib import Path
keys = ("CLUSTER", "CONTEXT", "NODE", "NODE_ID", "NODE_UID", "KUBECONFIG", "APP_NAMESPACE", "RID", "PROFILE")
(Path(os.environ["WORK"]) / "owned-fixture.json").write_text(json.dumps({k: os.environ[k] for k in keys}, indent=2))
PY
nodecmd() {
  test "$(k get node "$NODE" -o jsonpath='{.metadata.uid}')" = "$NODE_UID"
  test "$(docker inspect --format '{{.Id}} {{index .Config.Labels "io.x-k8s.kind.cluster"}}' "$NODE_ID")" = "$NODE_ID $CLUSTER"
  docker exec "$NODE_ID" "$@"
}
test "$(nodecmd stat -fc %T /sys/fs/cgroup)" = cgroup2fs
kind load docker-image --name "$CLUSTER" "$CONTROL" "$WORKER"
for item in application runtime-before runtime-after; do
  layout="$WORK/$item"
  if test "$item" = application; then layout="$layout/oci"; fi
  nodecmd true
  tar -C "$layout" -cf - oci-layout index.json blobs \
    | docker exec -i "$NODE_ID" ctr -n k8s.io images import --all-platforms -
done
nodecmd ctr -n k8s.io images tag example.invalid/brewlet/hello:v1 "$APP_REF"
k label node "$NODE" "brewlet.example.com/pool=$RUN_ID"
```

This preload is deliberately local. Production publication and pulls use the
separate [registry/trust configurations](registry-trust.md).

## 5. Install and approve the infrastructure

```sh
# tutorial-step: install
k apply -f kubernetes/deploy/crds/
helm --kubeconfig "$KUBECONFIG" --kube-context "$CONTEXT" upgrade --install brewlet \
  kubernetes/charts/brewlet --namespace brewlet --create-namespace \
  --set-string "operator.image=$CONTROL" --set-string "admission.image=$CONTROL" \
  --set-string "provisioner.image=$WORKER" --wait --timeout 5m
k -n brewlet rollout status deployment/brewlet-admission --timeout=120s
cat > "$WORK/runtime.yaml" <<EOF
apiVersion: node.brewlet.sh/v2alpha1
kind: RuntimeImage
metadata:
  name: $RID
spec:
  source: {image: "$RUNTIME_BEFORE"}
  user: {uid: 65532, gid: 65532}
EOF
python3 examples/local/observe.py webhook --file "$WORK/runtime.yaml"
k apply -f "$WORK/runtime.yaml"
k wait --for=condition=Approved "runtimeimage/$RID" --timeout=120s
export RUNTIME_UID="$(k get runtimeimage "$RID" -o jsonpath='{.metadata.uid}')"
cat > "$WORK/profile.yaml" <<EOF
apiVersion: node.brewlet.sh/v2alpha1
kind: NodeProfile
metadata:
  name: $PROFILE
spec:
  nodePool:
    key: brewlet.example.com/pool
    names: ["$RUN_ID"]
    includeControlPlane: true
  runtimeImages:
    - {id: $RID, install: Eager}
  tolerations:
    - {key: startup-taint.cluster-autoscaler.kubernetes.io/brewlet, operator: Equal, value: provisioning, effect: NoSchedule}
  rollout: {maxUnavailable: 1, containerdRestart: validated}
EOF
k apply -f "$WORK/profile.yaml"
k wait --for=condition=Ready "nodeprofile/$PROFILE" --timeout=5m
```

Control-plane opt-in is only for this disposable single-node cluster.
No account named `app`, runtime shell or runtime-image entrypoint is required.
The helper's 60-second server dry-run retries only this inventory webhook's
startup transport failures; admission denials and certificate failures fail
immediately. Ready control-plane Pods alone do not prove webhook reachability.

## 6. Deploy and call the application

```sh
# tutorial-step: application
k create namespace "$APP_NAMESPACE"
cat > "$WORK/deployment.yaml" <<EOF
apiVersion: apps/v1
kind: Deployment
metadata: {name: hello, namespace: $APP_NAMESPACE}
spec:
  replicas: 1
  selector: {matchLabels: {app: hello}}
  template:
    metadata:
      labels: {app: hello}
      annotations: {brewlet.sh/runtime-image: $RID}
    spec:
      runtimeClassName: brewlet
      nodeSelector: {kubernetes.io/arch: $ARCH}
      containers:
        - name: app
          image: $APP_REF
          imagePullPolicy: Never
          securityContext: {readOnlyRootFilesystem: true}
          resources:
            requests: {cpu: 10m, memory: 32Mi}
            limits: {cpu: 500m, memory: 128Mi}
          ports: [{containerPort: 8000}]
          readinessProbe:
            httpGet: {path: /, port: 8000}
            periodSeconds: 2
EOF
k apply -f "$WORK/deployment.yaml"
k -n "$APP_NAMESPACE" expose deployment hello --port=8000
k -n "$APP_NAMESPACE" rollout status deployment/hello --timeout=5m
SERVICE_IP="$(k -n "$APP_NAMESPACE" get service hello -o jsonpath='{.spec.clusterIP}')"
nodecmd curl --fail --silent --show-error --noproxy '*' --max-time 10 "http://$SERVICE_IP:8000/" \
  | tee "$WORK/http-before.json"
export POD="$(k -n "$APP_NAMESPACE" get pods -l app=hello -o jsonpath='{.items[0].metadata.name}')"
python3 examples/local/observe.py binding --source "$RUNTIME_BEFORE" > "$WORK/binding-before.json"
k -n "$APP_NAMESPACE" exec "$POD" -- /usr/bin/python3.13 -c \
  'import os; print(os.getuid(), os.getgid(), os.getcwd())'
```

To browse from your host, run `kubectl` with the same private kubeconfig/context
in a second terminal and port-forward `service/hello` in the application
namespace from `127.0.0.1:18080` to `8000`. Stop that terminal before cleanup.

## 7. Rotate runtime bytes, not the application

The update uses UID/resourceVersion tests so it cannot overwrite an unnoticed
inventory replacement or concurrent edit.

```sh
# tutorial-step: rotation
k get runtimeimage "$RID" -o json > "$WORK/inventory-before.json"
python3 - <<'PY'
import json, os
from pathlib import Path
w = Path(os.environ["WORK"])
image = json.loads((w / "inventory-before.json").read_text())
assert image["metadata"]["uid"] == os.environ["RUNTIME_UID"]
patch = [{"op":"test","path":"/metadata/uid","value":image["metadata"]["uid"]},
         {"op":"test","path":"/metadata/resourceVersion","value":image["metadata"]["resourceVersion"]},
         {"op":"replace","path":"/spec/source/image","value":os.environ["RUNTIME_AFTER"]}]
(w / "rotation.json").write_text(json.dumps(patch))
PY
k patch runtimeimage "$RID" --type=json --patch-file "$WORK/rotation.json"
python3 examples/local/observe.py rotation --source "$RUNTIME_AFTER" > "$WORK/rotation-result.json"
python3 examples/local/observe.py binding --source "$RUNTIME_BEFORE" > "$WORK/binding-retained.json"
```

The original Pod still uses its original digest. Now perform an ordinary
workload rollout; no automatic restarts were enabled:

```sh
# tutorial-step: replacement
k -n "$APP_NAMESPACE" rollout restart deployment/hello
k -n "$APP_NAMESPACE" rollout status deployment/hello --timeout=5m
export POD="$(k -n "$APP_NAMESPACE" get pods -l app=hello -o json | python3 -c 'import json,sys; p=[p for p in json.load(sys.stdin)["items"] if not p["metadata"].get("deletionTimestamp")]; assert len(p)==1; print(p[0]["metadata"]["name"])')"
python3 examples/local/observe.py binding --source "$RUNTIME_AFTER" > "$WORK/binding-after.json"
nodecmd curl --fail --silent --show-error --noproxy '*' --max-time 10 "http://$SERVICE_IP:8000/" \
  | tee "$WORK/http-after.json"
test "$(k -n "$APP_NAMESPACE" get deployment hello -o jsonpath='{.spec.template.spec.containers[0].image}')" = "$APP_REF"
python3 - <<'PY'
import json, os
from pathlib import Path
w = Path(os.environ["WORK"])
before, retained, after = [json.loads((w / f).read_text()) for f in
    ("binding-before.json", "binding-retained.json", "binding-after.json")]
assert before["containerID"] == retained["containerID"] and before["digest"] == retained["digest"]
assert after["digest"] == os.environ["RUNTIME_AFTER"].split("@")[1]
assert after["generation"] > before["generation"] and after["podUID"] != before["podUID"]
assert json.loads((w / "http-after.json").read_text())["uid"] == 65532
print("Verified: real HTTP, retained old lease, new runtime generation, unchanged application digest")
PY
```

## 8. Complete normal cleanup

Do not jump directly to destroying the node as proof of production cleanup.
If these waits fail, retain the work directory and use
[troubleshooting](troubleshooting.md) and [safe removal](uninstallation.md).

```sh
# tutorial-step: cleanup
k -n "$APP_NAMESPACE" delete deployment hello --wait=true --timeout=120s
k delete namespace "$APP_NAMESPACE" --wait=true --timeout=120s
k delete nodeprofile "$PROFILE" --wait=false
k wait --for=delete "nodeprofile/$PROFILE" --timeout=10m
nodecmd test ! -e "/opt/brewlet/runtimes/$RID/current"
nodecmd test ! -e /usr/local/bin/containerd-shim-brewlet-v2
nodecmd crictl info > "$WORK/cri-after-cleanup.json"
test -z "$(k get node "$NODE" -o jsonpath='{.metadata.labels.brewlet\.sh/owner-uid}')"
k delete runtimeimage "$RID" --wait=true --timeout=120s
helm --kubeconfig "$KUBECONFIG" --kube-context "$CONTEXT" uninstall brewlet \
  --namespace brewlet --timeout 10m
test "$(docker inspect --format '{{.Id}} {{index .Config.Labels "io.x-k8s.kind.cluster"}}' "$NODE_ID")" = "$NODE_ID $CLUSTER"
kind delete cluster --name "$CLUSTER" --kubeconfig "$KUBECONFIG"
test "$(docker image inspect --format '{{.Id}}' "$CONTROL")" = "$CONTROL_ID"
test "$(docker image inspect --format '{{.Id}}' "$WORKER")" = "$WORKER_ID"
docker image rm "$CONTROL" "$WORKER"
printf 'Tutorial complete. Evidence retained in %s\n' "$WORK"
```

Keep the evidence directory as needed. Never prune unrelated Docker resources
or remove claims/finalizers to make a tutorial appear successful.

[Continue with the platform handoff](workshops/index.md), or
[publish through a registry](applications.md).
