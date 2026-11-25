# Experiment: platform-owned Tomcat configuration

This variant reuses the unchanged WAR from `../tomcat`. The application team
supplies a WAR, not Catalina configuration or a launch descriptor. The platform
packages that WAR and owns the golden image, launcher and runtime approval.
No Brewlet changes or unreleased Brewlet
artifacts are needed.

**Moving `conf/` into the golden image is not sufficient by itself.** Released
Brewlet takes the launch configuration from the application artifact and permits
Kubernetes process overrides. This example demonstrates runtime-owned
configuration, but cannot enforce use of its launcher with released Brewlet.

The launch-ownership gap is tracked in
[#422](https://github.com/microsoft/brewlet/issues/422). No additional Kubernetes
admission-policy workaround is included.

## Runtime configuration ownership

| Component | Owner / enforcement |
| --- | --- |
| WAR classes, JSP and application resources | Application team; this example uses the existing Tomcat sample unchanged. |
| `build.sh` and `platform/launch.json` | Platform packager. The resulting application artifact contains only `/app/webapps/ROOT.war` and its fixed launch layer. |
| `platform/conf/` and `platform/start-tomcat.sh` | Baked into the golden image under `/opt/brewlet/tomcat`, outside `/app`. Brewlet mounts the runtime root read-only. |
| Catalina engine, connector and host configuration | Runtime-owned `server.xml`; `catalina.base` is fixed to `/opt/brewlet/tomcat`. |
| Context deployment | Runtime-owned `Catalina/localhost/ROOT.xml` names the WAR explicitly. The host's `appBase` is an empty runtime-owned directory, not an app-controlled deployment directory. |
| Embedded `META-INF/context.xml` | `deployXML=false` disables application-supplied Catalina context descriptors. `copyXML=false` prevents copying them into the configuration directory. |
| Common/server/shared class loaders | Runtime-owned `catalina.properties`. The common loader reads only Tomcat's runtime libraries, not an application-side Catalina `lib/` directory. WAR libraries remain application-owned. |
| Workload mounts | The RuntimeImage approves only `/tmp` outside `/app`. Native Brewlet mount validation protects runtime `conf/`, but application mounts beneath `/app` remain permitted. |
| Launch | The sample packager supplies the platform launcher. Released Brewlet does not prevent a different artifact or Pod command from selecting another launcher. |

The platform launcher also rejects arguments and nonempty `JAVA_TOOL_OPTIONS`,
`JDK_JAVA_OPTIONS`, `_JAVA_OPTIONS`, `JAVA_OPTS` and `CATALINA_OPTS`. The runtime
image's Docker `ENTRYPOINT` is useful for Docker but is **not** the Brewlet
enforcement mechanism.

## Requested launcher semantics

The feature request proposes renaming `launch.json.process.entrypoint` to
`process.command` and resolving initial launch as follows:

- A runtime image's nonempty `ENTRYPOINT` is always selected; the application
  command is ignored, not appended to it.
- Without a runtime `ENTRYPOINT`, use the application's command.
- Keep application arguments as arguments to the selected launcher. A platform
  launcher can accept, reject or ignore them.
- Fail explicitly if neither source supplies an executable, and ensure Pod
  command overrides cannot bypass a runtime-owned `ENTRYPOINT`.

This is inspired by Dockerfile ENTRYPOINT/CMD separation, but does not append
the application command to the runtime entrypoint. The current sample retains
`process.entrypoint` because the released CLI does not implement this proposal.

## What was observed

Experiment: November 24, 2026, using released Brewlet 0.11.1.

| Input | Observed identity |
| --- | --- |
| Official Tomcat source | `docker.io/library/tomcat@sha256:94e91501407ed6da44f0fce35e20808922fdab488d9a1643994bdcdd4335d1cc` |
| Server / Java | Tomcat 11.0.29 / Temurin Java 25.0.5.1 |
| Golden runtime manifest | `sha256:e1e3a917e403a0e8247a7612dccc01d2a23057cc83db3aea81031b5b5707f816` |
| Application artifact | `sha256:9255b30c4646bf8a0b2ea49b3b52317de13ac98d0a4d42b7af98393bedb8ec68` |
| Execution | Native Linux arm64; local Docker and a separate disposable kind cluster, Kubernetes 1.37.0 / containerd 2.3.4 |
| Control plane | Published `oci://ghcr.io/microsoft/charts/brewlet`, version 0.11.1 |

The WAR bytes matched the original sample's WAR. The golden image strips set-ID
bits and removes file capabilities when the upstream tools are available, as in
the original experiment. Node installation accepted the resulting runtime and
the NodeProfile became Ready.

| Attempt | Actual result |
| --- | --- |
| Normal local and cluster deployment | Servlet, JSP, CSS and JavaScript returned HTTP 200. |
| Write runtime `conf/server.xml`, locally and on the cluster | Failed with `Read-only file system`. |
| Add `/app/tomcat/conf` with a different connector and common loader; set `CATALINA_BASE=/app/tomcat` | The fixed launcher still selected runtime-owned configuration and port 8080. |
| Put a deny-all Catalina valve in the WAR's `META-INF/context.xml` | The runtime-owned ROOT descriptor was used; the embedded valve was ignored and requests succeeded. |
| Supply JVM options through the launcher environment or arguments | Launcher rejected the request with exit status 64. |
| Package an application layer at `/opt/brewlet/tomcat/conf` | Released CLI rejected it with `LayerPathViolation: destination outside /app`. |
| Use the original sample's direct-Java launch against this golden image | It successfully used `/app/tomcat/conf`. This demonstrated the missing runtime-entrypoint precedence. |
| Mount over runtime `conf/` | Denied by Brewlet admission with `RuntimeMountNotAllowed`. |

## Reproduce locally

Use the released Brewlet 0.11.1 CLI on `PATH`, Docker/buildx and Maven. From the
repository root:

```sh
mvn -q -f examples/tomcat/pom.xml package
bash examples/tomcat-hardened/build.sh examples/tomcat/target/ROOT.war
cd examples/tomcat-hardened
ARCH="$(docker info --format '{{.Architecture}}' | sed 's/aarch64/arm64/; s/x86_64/amd64/')"
docker buildx build --platform "linux/$ARCH" --load \
  -f Dockerfile.golden -t example.local/tomcat-hardened:11.0.29 .
brewlet run example.local/tomcat-hardened-war:1 \
  --store build/image --runtime example.local/tomcat-hardened:11.0.29 \
  --user 65532:65532 --tmpfs /tmp \
  --name brewlet-tomcat-hardened-local -p 127.0.0.1:18080:8080
```

Once Tomcat is running, in another terminal:

```sh
curl http://127.0.0.1:18080/runtime
```

When finished:

```sh
docker stop brewlet-tomcat-hardened-local
```

Local and cluster runs demonstrate the supplied launcher and immutable
configuration, not enforcement against a user selecting a different launch
artifact.

## Reproduce the cluster working case

Also install kind, kubectl, Helm and Python 3. Use a **new disposable cluster**
with containerd 2.x and Kubernetes 1.30 or newer. The runtime installation
commands belong to the platform team.
Run from `examples/tomcat-hardened/` after the packaging step above, with
the local container stopped:

```sh
set -euo pipefail
export KUBECONFIG="$PWD/build/kubeconfig"
ARCH="$(docker info --format '{{.Architecture}}' | sed 's/aarch64/arm64/; s/x86_64/amd64/')"
DOCKER_DEFAULT_PLATFORM="linux/$ARCH" kind create cluster \
  --name brewlet-tomcat-hardened-experiment --config kind.yaml \
  --kubeconfig "$KUBECONFIG"
docker run -d --name brewlet-tomcat-hardened-registry --network kind \
  --platform "linux/$ARCH" --publish 127.0.0.1::5000 \
  registry@sha256:ddf754342cfc8acc51a56d5d0ab6af06826461864460636d8bd5c546dab2a7b8
REGISTRY="$(docker port brewlet-tomcat-hardened-registry 5000/tcp)"
NODE=brewlet-tomcat-hardened-experiment-control-plane
docker exec "$NODE" mkdir -p "/etc/containerd/certs.d/$REGISTRY"
docker cp registry-hosts.toml "$NODE:/etc/containerd/certs.d/$REGISTRY/hosts.toml"
helm install brewlet oci://ghcr.io/microsoft/charts/brewlet \
  --version 0.11.1 --namespace brewlet --create-namespace --wait --timeout 5m

docker buildx build --platform "linux/$ARCH" --provenance=false \
  -f Dockerfile.golden --output type=oci,dest=build/golden.tar .
mkdir -p build/golden
tar -xf build/golden.tar -C build/golden
brewlet push "$REGISTRY/golden/tomcat-hardened:11.0.29" \
  --store build/golden --insecure --push-result build/golden-push.json
brewlet k8s runtime add tomcat-hardened \
  --image "$REGISTRY/golden/tomcat-hardened:11.0.29" --insecure \
  --uid 65532 --gid 65532 --volume-mount-path /tmp --platform "linux/$ARCH"
brewlet k8s profile create tomcat-hardened-experiment --all-nodes \
  --include-control-plane --runtime tomcat-hardened \
  --toleration startup-taint.cluster-autoscaler.kubernetes.io/brewlet=provisioning:NoSchedule
kubectl wait --for=condition=Ready nodeprofile/tomcat-hardened-experiment --timeout=5m

brewlet push "$REGISTRY/apps/tomcat-hardened-war:1" \
  --store build/image --insecure --push-result build/app-push.json
DIGEST="$(python3 -c 'import json; print(json.load(open("build/app-push.json"))["digest"])')"
APP_IMAGE="$REGISTRY/apps/tomcat-hardened-war@$DIGEST"
sed "s|APP_IMAGE|$APP_IMAGE|g" deployment.yaml > build/deployment.yaml
kubectl apply -f build/deployment.yaml
kubectl rollout status deployment/tomcat-hardened-war --timeout=5m
kubectl port-forward service/tomcat-hardened-war 18080:8080
```

The HTTP registry and `--insecure` are only for this disposable development setup.

Once port-forwarding is ready, in another terminal:

```sh
curl http://127.0.0.1:18080/runtime
```

Stop port-forwarding before cleanup:

```sh
kubectl delete -f build/deployment.yaml
kubectl delete nodeprofile tomcat-hardened-experiment --wait --timeout=5m
kubectl delete runtimeimage tomcat-hardened
helm uninstall brewlet -n brewlet --wait --timeout=5m
kind delete cluster --name brewlet-tomcat-hardened-experiment
docker rm -fv brewlet-tomcat-hardened-registry
```

The experiment's disposable cluster, registry and local verification containers
were removed after verification. Generated build artifacts and observation logs
remain under the ignored `build/` directory.

## Limits

This is runtime configuration separation with an unresolved launch-ownership
gap, **not a sandbox for hostile Java code**. Application code still executes
inside Tomcat's JVM. App-level
`WEB-INF/web.xml`, servlet behavior and bundled application libraries remain
application-owned; platform `web.xml` supplies defaults, not a prohibition on
standard webapp configuration.

The experiment does not establish production hardening, arbitrary Tomcat/JDK
compatibility, or amd64 behavior. Runtime configuration remains read-only, but
released Brewlet cannot guarantee that an application uses the runtime's
launcher. The feature request addresses that gap rather than adding an external
admission policy to this example.
