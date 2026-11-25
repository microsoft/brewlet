# Experiment: Apache Tomcat and WAR apps on Brewlet

This experiment used released Brewlet 0.11.1, first with `brewlet run`, then on a
separate disposable kind cluster with the released Helm chart, operator, admission
service, provisioner and shim.

The official Tomcat image works locally with an explicit non-root user and writable scratch
space, but **does not qualify unchanged as a cluster golden image**. Its OS contains set-ID
files. A small platform-owned image derived from the released Tomcat
image, stripping those bits and any file capabilities, passed node validation.

For the variant where the platform team owns Catalina's `conf/` and launcher,
see the [hardened Tomcat experiment](../tomcat-hardened/).

## Inputs actually tested

Release discovery and experiment: November 24, 2026 (US Eastern).
`v0.11.1` was GitHub's latest Brewlet release.

| Input | Observed version / immutable identity |
| --- | --- |
| Brewlet CLI and Helm chart | 0.11.1; CLI commit `91b368adf08ada433a6ff8ea5ec8fe551244c472` |
| Producer | macOS arm64; Maven; host Java 26.0.2; WAR compiled with `--release 17` |
| Execution platform | Linux arm64, native Docker and kind |
| Official `tomcat:latest` | Tomcat 11.0.29, Temurin Java 25.0.5.1 |
| Official JDK25 source | `docker.io/library/tomcat@sha256:94e91501407ed6da44f0fce35e20808922fdab488d9a1643994bdcdd4335d1cc` |
| Official `tomcat:11.0-jdk21-temurin` | Tomcat 11.0.29, Temurin Java 21.0.13 |
| Official JDK21 source | `docker.io/library/tomcat@sha256:ca78484162e74bde0bd267ff8d8f74f00fbfbabcbfe797a8cbfb9fefa7953614` |
| Servlet API | Released `jakarta.servlet-api:6.1.0`, Maven `provided` scope |

The release archives were checked against the release's `checksums.txt`.
Attestations were not independently verified. Chart component images were the
digest-pinned images shipped in the downloaded 0.11.1 chart.

## What the application image contains

`pom.xml` builds an ordinary `ROOT.war`, not an executable JAR and not embedded
Tomcat. `/runtime` is an annotated servlet; `/` is a JSP that exercises Jasper
compilation rather than just serving static content. The JSP uses `app.js` to
show the servlet's response in place; without JavaScript, the link opens the
JSON directly. Each servlet request is logged with UTC timestamps, and its
request timestamp is included in the JSON.

The application OCI artifact has three layers:

1. `/app/webapps/ROOT.war`: application classes, JSP, CSS and JavaScript.
2. `/app/tomcat/conf`: the small server, servlet, logging and ROOT context configuration.
3. `launch.json`: the process descriptor.

There is no JDK, Tomcat implementation or Servlet API JAR in the application.
Tomcat's bootstrap JARs, server libraries, Jasper and Java come from the golden
runtime. The generic released CLI packages the WAR as an ordinary file.
The Brewlet Maven plugin was not needed or tested; this experiment does not
establish WAR support in that plugin.

## Tomcat-specific adjustments, not Brewlet changes

- **Launch Java directly.** Runtime image `CMD`, working directory and user are
  not the workload launch contract. `launch.json` invokes
  `/opt/java/openjdk/bin/java` and `org.apache.catalina.startup.Bootstrap`
  with the runtime's `bootstrap.jar` and `tomcat-juli.jar`. No
  `catalina.sh`, image entrypoint initialization or app-image shell is needed.
- **Separate home from base.** `catalina.home=/usr/local/tomcat` supplies server
  binaries. `catalina.base=/app/tomcat` supplies application-side configuration.
  `appBase=/app/webapps` finds the WAR in the immutable application layer.
- **Keep the WAR packed.** `unpackWARs=false` avoids extracting it beside the
  WAR under read-only `/app`. `autoDeploy=false` disables background deployment
  scanning, while startup deployment remains enabled.
- **Provide scratch explicitly.** Host `workDir=/tmp/tomcat-work` and
  `java.io.tmpdir=/tmp` place JSP compilation and temporary files in a writable
  volume. The RuntimeImage approves `/tmp`; the Pod supplies an `emptyDir`
  mounted there, with `fsGroup: 65532`. Local runs use `--tmpfs /tmp`.
- **Log to the console.** JUL uses a console handler, not files under
  `$CATALINA_BASE/logs`. The server shutdown port is disabled; container
  termination shuts down the JVM. The servlet uses `java.util.logging` through
  Tomcat's JULI manager and its `OneLineFormatter`; the request log includes
  the same UTC timestamp returned in the JSON.
- **Declare static asset MIME types.** The minimal Tomcat `web.xml` does not
  include the usual MIME mappings. Without an explicit `css` to `text/css`
  mapping, Tomcat served the stylesheet without a `Content-Type` header. The
  sample also maps `js` to `text/javascript` for its client-side script.
- **Prepackage the context directory.** Initially Tomcat served the WAR but
  logged `Unable to create directory for deployment:
  [/app/tomcat/conf/Catalina/localhost]`. Including the real
  `conf/Catalina/localhost/ROOT.xml` fixed that error without writable config.
- **Use a numeric identity.** UID/GID 65532 works without adding an OS account.
  `id` cannot resolve its username, but servlet deployment and JSP compilation
  work. The application does not depend on username lookup.

The minimal `web.xml` enables the default and JSP servlets. The remaining
Tomcat defaults used successfully in this test do not require copying the
entire upstream configuration into the application.

## Observed failures and boundaries

| Attempt | Actual result / learning |
| --- | --- |
| Local non-root run, no writable `/tmp` | `/runtime` returned HTTP 200, but `/` returned HTTP 500. A servlet-only health check would have missed broken JSP support. |
| Local non-root run with `--tmpfs /tmp` | Both endpoints returned HTTP 200; JSP compiled successfully. |
| Cluster installation of upstream root via local OCI mirror | Config preflight passed, but the node rejected `runtime setuid/setgid entry usr/bin/chage`. Local `brewlet run` success is not proof of golden-image eligibility. |
| Cluster installation of sanitized root | NodeProfile became Ready, and the real Brewlet workload served both endpoints. |
| Writes inside runtime and application | `touch` under `/usr/local/tomcat` and `/app` failed with `Read-only file system`. Compiled JSP classes appeared under `/tmp/tomcat-work/ROOT/org/apache/jsp/`. |

The upstream image inspection found eleven set-ID files, including `chage`,
`mount`, `passwd` and `su`. `Dockerfile.golden` removes those permission bits
and removes file capabilities if present. Inspection of the resulting root
found no set-ID files or file capabilities, and the node independently
accepted it. This is a compatibility preparation step, not a vulnerability
assessment or a claim that the runtime is production-hardened.

Two transport problems occurred in this environment:

- Direct CLI approval of a Docker Hub digest reported `DigestMismatch`;
  a fully qualified tag reported invalid manifest JSON beginning with `<`.
  Docker could pull the images. The cause was not established and Brewlet was
  not patched. Exporting the pulled upstream stage to an OCI layout and
  publishing it to the dedicated local registry let the actual filesystem
  eligibility test proceed, without `--skip-image-checks`.
- `docker push` to the registry's dynamically published loopback port failed
  from the Docker engine. `docker buildx --output type=oci` followed by the
  released `brewlet push --insecure` worked. `--insecure` was confined to this
  disposable HTTP registry. Containerd's `hosts.toml` mapped its host-facing
  authority to the registry container on the kind network.

The upstream OCI re-export preserved the root contents, but changed the
manifest representation/digest. It was not presented as a byte-identical
mirror of the Docker Hub index.

## Runtime replacement with the same application image

The cluster used the application reference:

```text
127.0.0.1:51790/apps/tomcat-war@sha256:8975a3b48de90193ff4d526e904da8ea7d7813ed0f9c8a1664f4ba7feb3dd0df
```

The sanitized JDK25 runtime manifest was
`sha256:6c4f3196f5c53d492579e7062409d8b0cdff9e6a8a1a0155f9842cc23a8c5395`;
the sanitized JDK21 runtime manifest was
`sha256:c3a1ec55499c095bfa67ae6d9dabe0641b89e0b8252c5c54196ffc1ba77ecf7b`.

| Stage | `/runtime` Java | Tomcat | Application reference |
| --- | --- | --- | --- |
| Initial sanitized runtime, generation 2 | 25.0.5.1 | 11.0.29 | Same digest above |
| Runtime changed to generation 3, original Pod still running | 25.0.5.1 | 11.0.29 | Same digest above |
| Deployment restarted after rotation completed | 21.0.13 | 11.0.29 | Same digest above |

Rotation reached `RotationComplete`; `OldGenerationTasks=True` confirmed the
old running task remained observed. A replacement Pod used the new generation.
Its UID differed, but its application image reference was identical.
Both servlet and JSP still worked, and the HTTP smoke test passed
against Java 21.0.13. No WAR build or push occurred between these three stages.
The later independent `build.sh` check generated a different application digest,
but that rebuild was not deployed or used for the rotation comparison.

This deliberately compares two released JDK variants of the same Tomcat
version. It does not prove arbitrary Tomcat-major upgrades, incompatible
Servlet API transitions, or application-specific JDK compatibility.

## Reproduce the local working case

With the released Brewlet 0.11.1 CLI on `PATH`, run from the repository root:

```sh
cd examples/tomcat
bash build.sh
docker pull --platform linux/arm64 \
  tomcat@sha256:94e91501407ed6da44f0fce35e20808922fdab488d9a1643994bdcdd4335d1cc
brewlet run example.local/tomcat-war:1 \
  --store build/image \
  --runtime tomcat@sha256:94e91501407ed6da44f0fce35e20808922fdab488d9a1643994bdcdd4335d1cc \
  --user 65532:65532 --tmpfs /tmp \
  --name brewlet-tomcat-local -p 127.0.0.1:18080:8080
```

In another terminal, run:

```sh
curl http://127.0.0.1:18080/runtime
```

When finished, stop the local container:

```sh
docker stop brewlet-tomcat-local
```

Re-running the same command with `--runtime` set to the JDK21 source digest
(no rebuild) also passed the curl smoke test, so the runtime swap
works locally as well as in the cluster.

The example above is the architecture actually tested. `build.sh` detects the
Docker engine architecture, but amd64 execution was not tested here.

## Reproduce the cluster working case

Prerequisites: the released Brewlet 0.11.1 CLI on `PATH`, Docker, buildx OCI
export support, kind with containerd 2.x, kubectl, Helm, Maven, curl, Python 3
and registry access. Use a new cluster; do not apply the profile to shared
nodes. The names below belong exclusively to this experiment. Run from
`examples/tomcat/` after `bash build.sh`:

```sh
set -euo pipefail
export KUBECONFIG="$PWD/build/kubeconfig"
ARCH="$(docker info --format '{{.Architecture}}' | sed 's/aarch64/arm64/; s/x86_64/amd64/')"
DOCKER_DEFAULT_PLATFORM="linux/$ARCH" kind create cluster \
  --name brewlet-tomcat-experiment --config kind.yaml \
  --kubeconfig "$KUBECONFIG"
docker run -d --name brewlet-tomcat-experiment-registry --network kind \
  --platform "linux/$ARCH" --publish 127.0.0.1::5000 \
  registry@sha256:ddf754342cfc8acc51a56d5d0ab6af06826461864460636d8bd5c546dab2a7b8
REGISTRY="$(docker port brewlet-tomcat-experiment-registry 5000/tcp)"
NODE=brewlet-tomcat-experiment-control-plane
docker exec "$NODE" mkdir -p "/etc/containerd/certs.d/$REGISTRY"
docker cp registry-hosts.toml "$NODE:/etc/containerd/certs.d/$REGISTRY/hosts.toml"
helm install brewlet oci://ghcr.io/microsoft/charts/brewlet \
  --version 0.11.1 --namespace brewlet --create-namespace --wait

SOURCE=tomcat@sha256:94e91501407ed6da44f0fce35e20808922fdab488d9a1643994bdcdd4335d1cc
docker buildx build --platform "linux/$ARCH" --build-arg "TOMCAT_IMAGE=$SOURCE" \
  -f Dockerfile.golden --output type=oci,dest=build/golden.tar .
mkdir -p build/golden
tar -xf build/golden.tar -C build/golden
brewlet push "$REGISTRY/golden/tomcat:25" --store build/golden \
  --insecure --push-result build/golden-push.json
brewlet k8s runtime add tomcat --image "$REGISTRY/golden/tomcat:25" \
  --insecure --uid 65532 --gid 65532 \
  --volume-mount-path /tmp --platform "linux/$ARCH"
brewlet k8s profile create tomcat-experiment --all-nodes \
  --include-control-plane --runtime tomcat \
  --toleration startup-taint.cluster-autoscaler.kubernetes.io/brewlet=provisioning:NoSchedule
kubectl wait --for=condition=Ready nodeprofile/tomcat-experiment --timeout=5m
brewlet push "$REGISTRY/apps/tomcat-war:1" --store build/image \
  --insecure --push-result build/app-push.json
DIGEST="$(python3 -c 'import json; print(json.load(open("build/app-push.json"))["digest"])')"
sed "s|APP_IMAGE|$REGISTRY/apps/tomcat-war@$DIGEST|" deployment.yaml > build/deployment.yaml
kubectl apply -f build/deployment.yaml
kubectl rollout status deployment/tomcat-war --timeout=5m
kubectl port-forward service/tomcat-war 18080:8080
```

Once port-forwarding is ready, run in another terminal:

```sh
curl http://127.0.0.1:18080/runtime
```

Stop port-forwarding before cleanup. The working-case recipe starts with the
sanitized image; it deliberately omits the earlier upstream rejection test.
The Dockerfile's `upstream` target can export the unmodified root for repeating
that negative test.

To repeat runtime replacement, export and push another golden image using
the JDK21 source digest above, then patch **only** `RuntimeImage.spec.source.image`
to its pushed, digest-pinned reference. Wait for the new
`status.generation` and `RotationComplete` before restarting the Deployment.
Call `/runtime` before and after restarting; do not rebuild or change the
application image.

Cleanup, after stopping any local run / port-forward:

```sh
kubectl delete -f build/deployment.yaml
kubectl delete nodeprofile tomcat-experiment --wait --timeout=5m
kubectl delete runtimeimage tomcat
helm uninstall brewlet -n brewlet --wait --timeout=5m
kind delete cluster --name brewlet-tomcat-experiment
docker rm -f brewlet-tomcat-experiment-registry
```

The experiment's actual disposable resources were removed after verification.

## Remaining limits

This is a one-node development proof, not production qualification. It does
not test multi-node placement, amd64, PVC persistence, sessions across rolling
restarts, manager uploads, WAR hot deployment, external databases, TLS,
authentication, native libraries, instrumented applications or automated
workload restarts. A custom webapp can still assume an exploded WAR, writable
application files or `ServletContext.getRealPath()` returning a real directory;
those assumptions need separate testing with `unpackWARs=false`.

The platform would own the Tomcat/JDK golden image, non-root policy and volume
contract. The application would own the WAR and its launch/configuration.
The result demonstrates that a servlet container can move into infrastructure
along with the JVM; Brewlet need not gain WAR-specific packaging logic.
