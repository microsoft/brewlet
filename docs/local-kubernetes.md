# Your first local Kubernetes deployment

Run a real HTTP application, rotate its runtime without rebuilding it, and
remove the installation normally.

In this tutorial, you'll install Brewlet in a local kind cluster, deploy a
Python application, and update its node-supplied runtime. You'll see how a
running Pod keeps its original runtime while a replacement Pod uses the update,
without changing the application image.

The tutorial uses **Brewlet 0.8.1**, the latest published beta release.

## Prerequisites

- macOS or Linux, on amd64 or arm64
- Docker, running Linux containers
- [kind](https://kind.sigs.k8s.io/) v0.33.0, kubectl and Helm
- Network access to GitHub and public container registries

Run every step in the **same terminal**.

## 1. Install the Brewlet CLI

Install the `brewlet` CLI into `~/.local/bin`:

```sh
# tutorial-step: cli
curl -fsSL https://brewlet.sh/install.sh | sh -s -- --version 0.8.1
export PATH="$HOME/.local/bin:$PATH"
brewlet version
```

The script downloads the release for your machine and verifies its checksum.

## 2. Create a work directory

The work directory holds the application and a separate kubeconfig, so the
tutorial doesn't touch your existing clusters.

```sh
# tutorial-step: setup
WORK="$(mktemp -d)"
export KUBECONFIG="$WORK/kubeconfig"
```

## 3. Build the application image

The application is a small Python HTTP server. Its image contains only the
application: Python itself will come from the node.

```sh
# tutorial-step: build
mkdir "$WORK/app"
cat > "$WORK/app/server.py" <<'EOF'
import json, os, sys
from http.server import BaseHTTPRequestHandler, HTTPServer

class Hello(BaseHTTPRequestHandler):
    def do_GET(self):
        body = json.dumps({"python": sys.version.split()[0], "uid": os.getuid()})
        self.send_response(200)
        self.end_headers()
        self.wfile.write(body.encode() + b"\n")

HTTPServer(("", 8000), Hello).serve_forever()
EOF
cat > "$WORK/launch.json" <<'EOF'
{"schemaVersion": 1, "process": {"entrypoint": ["/usr/bin/python3.13"],
 "args": ["/app/server.py"], "workingDir": "/app"}}
EOF
ARCH="$(uname -m | sed 's/x86_64/amd64/; s/aarch64/arm64/')"
brewlet build example.invalid/brewlet/hello:v1 --platform "linux/$ARCH" \
  --launch "$WORK/launch.json" --layer "app=$WORK/app:/app" --store "$WORK/image"
APP="example.invalid/brewlet/hello@$(grep -o 'sha256:[0-9a-f]*' "$WORK/image/index.json")"
echo "$APP"
```

`launch.json` tells Brewlet how to start the application with the
node-supplied Python. `brewlet build` writes the image to `$WORK/image`, and
`APP` refers to it by digest.

## 4. Create the cluster and load the image

Create a one-node kind cluster and load the application image into it. The
node image is pinned to a Kubernetes version whose containerd configuration
Brewlet supports:

```sh
# tutorial-step: cluster
kind create cluster --name brewlet \
  --image kindest/node@sha256:4613778f3cfcd10e615029370f5786704559103cf27bef934597ba562b269661
tar -C "$WORK/image" -cf - oci-layout index.json blobs \
  | docker exec -i brewlet-control-plane ctr -n k8s.io images import -
docker exec brewlet-control-plane ctr -n k8s.io images tag example.invalid/brewlet/hello:v1 "$APP"
```

Loading the image directly keeps this example local. To publish images through
a registry instead, see [registry and trust configuration](registry-trust.md).

## 5. Install Brewlet on the cluster and offer a runtime

Install the released Helm chart. It includes the CRDs and digest-pinned
component images:

```sh
# tutorial-step: install
helm install brewlet oci://ghcr.io/microsoft/charts/brewlet --version 0.8.1 \
  --namespace brewlet --create-namespace --wait

cat > "$WORK/python.yaml" <<'EOF'
apiVersion: node.brewlet.sh/v1alpha1
kind: RuntimeImage
metadata:
  name: python
spec:
  source:
    image: gcr.io/distroless/python3-debian13@sha256:f3d5ddc6c64a019fe520e7f005f2880be21e6afc461b10a3c15ef2e4edc71e33
  user: {uid: 65532, gid: 65532}
---
apiVersion: node.brewlet.sh/v1alpha1
kind: NodeProfile
metadata:
  name: local
spec:
  nodePool: {includeControlPlane: true}
  runtimeImages:
    - id: python
  tolerations:
    - {key: startup-taint.cluster-autoscaler.kubernetes.io/brewlet, operator: Equal, value: provisioning, effect: NoSchedule}
EOF
until kubectl apply -f "$WORK/python.yaml"; do sleep 2; done
kubectl wait --for=condition=Ready nodeprofile/local --timeout=5m
brewlet k8s runtime status python
```

The admission webhook can take a few seconds to start answering after the
install, so the `until` loop retries the apply until it succeeds.

The `python` RuntimeImage is an unchanged upstream Python image, pinned by
digest. The `local` profile tells Brewlet to install it on the cluster's node.
`includeControlPlane` allows that on this one-node cluster; leave it disabled
for control-plane nodes in a shared cluster.

## 6. Deploy and call the application

The annotation asks for the `python` runtime, and `runtimeClassName: brewlet`
lets Brewlet supply it:

```sh
# tutorial-step: application
kubectl apply -f - <<EOF
apiVersion: apps/v1
kind: Deployment
metadata:
  name: hello
spec:
  selector:
    matchLabels: {app: hello}
  template:
    metadata:
      labels: {app: hello}
      annotations: {brewlet.sh/runtime-image: python}
    spec:
      runtimeClassName: brewlet
      containers:
        - name: app
          image: $APP
          imagePullPolicy: Never
          securityContext: {readOnlyRootFilesystem: true}
          readinessProbe:
            httpGet: {path: /, port: 8000}
EOF
kubectl expose deployment hello --port=8000
kubectl rollout status deployment/hello --timeout=5m
SERVICE_IP="$(kubectl get service hello -o jsonpath='{.spec.clusterIP}')"
docker exec brewlet-control-plane curl -s --retry 5 --retry-connrefused "http://$SERVICE_IP:8000/"
```

The response shows the Python version and the runtime's UID, 65532.

## 7. Rotate the runtime, not the application

Point the RuntimeImage at a newer snapshot of the same Python image, then wait
for Brewlet to roll it out:

```sh
# tutorial-step: rotation
kubectl patch runtimeimage python --type=merge -p '{"spec":{"source":{"image":"gcr.io/distroless/python3-debian13@sha256:f47f5807043391605425f2bd162b25f50cebdc90640bacc804e0177608fe5c65"}}}'
kubectl wait runtimeimage/python --for=jsonpath='{.status.generation}'=2 --timeout=2m
kubectl wait runtimeimage/python --for=condition=RotationComplete --timeout=5m
kubectl wait runtimeimage/python --for=condition=OldGenerationTasks=true --timeout=2m
brewlet k8s runtime status python
```

The status shows the RuntimeImage at generation 2, while the running Pod is
still bound to generation 1. Brewlet never swaps the runtime under a running process.

Restart the Deployment. The replacement Pod uses the new runtime, with the same
application image:

```sh
# tutorial-step: replacement
kubectl rollout restart deployment/hello
kubectl rollout status deployment/hello --timeout=5m
docker exec brewlet-control-plane curl -s --retry 5 --retry-connrefused "http://$SERVICE_IP:8000/"
kubectl wait runtimeimage/python --for=condition=OldGenerationTasks=false --timeout=2m
brewlet k8s runtime status python
```

No Pod uses generation 1 anymore, and the application image digest is the same
one you built in step 3.

## 8. Clean up

Delete the application, then let Brewlet remove the runtime and restore the
node's configuration before deleting the cluster:

```sh
# tutorial-step: cleanup
kubectl delete deployment hello
kubectl delete nodeprofile local --timeout=10m
kubectl delete runtimeimage python
helm uninstall brewlet --namespace brewlet
kind delete cluster --name brewlet
rm -rf "$WORK"
```

To remove the CLI too, run `rm ~/.local/bin/brewlet`.

If a cleanup command fails, see [troubleshooting](troubleshooting.md) and
[safe removal](uninstallation.md).

[Continue with the platform handoff](workshops/index.md), or
[publish through a registry](applications.md).
