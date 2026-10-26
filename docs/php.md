# PHP workloads

## How it works

Brewlet separates your PHP application from the PHP interpreter. You publish
your scripts, resolved Composer dependencies and an explicit launch descriptor
as an OCI artifact, without bundling PHP. The platform approves and provisions
a [runtime image](runtime-images.md) containing PHP, its extensions and its
Linux userland; your Kubernetes Deployment selects its runtime ID. Brewlet runs
your declared `php` command with application files read-only under `/app`.

Because the interpreter belongs to the node, a PHP security release is a
platform change: approve the patched runtime image, then replace Pods. The
application image digest stays the same. See
[rotate without rebuilding](operations.md#rotate-without-rebuilding-application-bytes).

## Tested runtimes

PHP is not part of Brewlet's automated fixtures. The workflow on this page was
verified manually with Brewlet 0.8.1 on a single-node arm64 kind cluster,
following the [local tutorial](local-kubernetes.md), using the unmodified Docker
Official `php:8.4.26-cli-alpine` and `php:8.4.27-cli-alpine` images:

- The application served requests as UID 65532 with PHP 8.4.26.
- After the RuntimeImage was patched to 8.4.27, the running Pod kept 8.4.26.
  The replacement Pod reported 8.4.27 with the same application digest.
- `kubectl exec` with `/usr/local/bin/php` used the same runtime.

Choose the runtime image carefully:

- **Alpine variants can be approved unchanged.** The `-cli-alpine` images
  contain no set-ID binaries.
- **Debian variants are rejected.** `php:<version>-cli` contains setuid tools
  such as `chage`, `su` and `mount`. The provisioner reports
  `runtime setuid/setgid entry usr/bin/chage` in its logs, and the NodeProfile
  stays `Provisioning`. Remove set-ID bits in your golden image pipeline, as
  the [fixture Dockerfiles](https://github.com/microsoft/brewlet/tree/main/integration-tests/fixtures/runtimes)
  do, or use an Alpine variant.
- **Brewlet 0.8.1 cannot pull runtime images from `docker.io`.** The node
  provisioner fails with `401 Unauthorized`. Use the same digest from the
  Docker Official Images mirror at `public.ecr.aws/docker/library/php`, or
  copy the image into your platform registry.

These images are test inputs, not a security recommendation. Use the runtime
images your platform team approves. Extensions are part of the runtime: check
`php -m` in the image for every extension your application needs.

## Build the payload

This example is a single script served by PHP's built-in web server:

```sh
WORK="$(mktemp -d)"
mkdir "$WORK/app"
cat > "$WORK/app/index.php" <<'EOF'
<?php
header('Content-Type: application/json');
echo json_encode([
    'php' => PHP_VERSION,
    'sapi' => PHP_SAPI,
    'uid' => posix_getuid(),
]), "\n";
EOF
chmod 0755 "$WORK/app"
chmod 0644 "$WORK/app/index.php"
```

For a Composer project, run `composer install --no-dev` in your build pipeline
and package the resulting `vendor/` directory. Never run Composer on the node or
at startup. Package dependencies with native code only if they match the
runtime's PHP version, libc and CPU architecture.

## Launch descriptor

The Docker Official PHP images install PHP at `/usr/local/bin/php`. Save as
`$WORK/launch.json`:

```json
{
  "schemaVersion": 1,
  "process": {
    "entrypoint": [
      "/usr/local/bin/php"
    ],
    "args": [
      "-S",
      "0.0.0.0:8000",
      "/app/index.php"
    ],
    "workingDir": "/app"
  }
}
```

The last argument makes `index.php` the router script for every request.

## Package

Files must be world-readable and directories searchable (see
[producer filesystem requirements](applications.md#producer-filesystem)).
Replace the registry and declare your actual platform:

```sh
brewlet build registry.example.com/apps/php-demo:v1 \
  --platform linux/arm64 \
  --launch "$WORK/launch.json" \
  --layer "app=$WORK/app:/app"
brewlet push registry.example.com/apps/php-demo:v1 --push-result "$WORK/push.json"
```

To share `vendor/` across releases, package it as a separate layer, for example
`--layer "dependency=$WORK/vendor:/app/vendor"`. See
[dependency layer sets](dependency-layer-sets.md).

## Approve the runtime

The platform team approves the runtime by digest and offers it on a pool:

```yaml
apiVersion: node.brewlet.sh/v1alpha1
kind: RuntimeImage
metadata:
  name: php-8.4
spec:
  source:
    image: public.ecr.aws/docker/library/php@sha256:<php-8.4-cli-alpine-digest>
  user: {uid: 65532, gid: 65532}
```

Add `php-8.4` to a NodeProfile's `runtimeImages`, as described in
[runtime images and pools](runtime-images.md).

## Native deployment

Use the digest from `push.json` and the approved runtime ID:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: php-demo
  namespace: apps
spec:
  replicas: 1
  selector:
    matchLabels: {app: php-demo}
  template:
    metadata:
      labels: {app: php-demo}
      annotations:
        brewlet.sh/runtime-image: php-8.4
    spec:
      runtimeClassName: brewlet
      nodeSelector: {kubernetes.io/arch: arm64}
      containers:
        - name: app
          image: registry.example.com/apps/php-demo@sha256:<application-digest>
          securityContext: {readOnlyRootFilesystem: true}
          resources:
            requests: {cpu: 100m, memory: 64Mi}
            limits: {cpu: "1", memory: 256Mi}
          readinessProbe:
            httpGet: {path: /, port: 8000}
```

## Verify

```sh
kubectl -n apps rollout status deployment/php-demo --timeout=5m
kubectl -n apps exec deployment/php-demo -- /usr/local/bin/php -v
kubectl -n apps port-forward deployment/php-demo 18080:8000
```

From another terminal, `curl --fail http://127.0.0.1:18080/` should report
the runtime's PHP version, `"sapi":"cli-server"` and `"uid":65532`.

## Apply a PHP security release

When PHP publishes a security release, approve the patched image digest under
the same runtime ID, wait for rotation, then replace Pods:

```sh
kubectl patch runtimeimage php-8.4 --type=merge \
  -p '{"spec":{"source":{"image":"public.ecr.aws/docker/library/php@sha256:<patched-digest>"}}}'
kubectl wait runtimeimage/php-8.4 --for=condition=RotationComplete --timeout=5m
kubectl -n apps rollout restart deployment/php-demo
kubectl -n apps rollout status deployment/php-demo --timeout=5m
brewlet k8s runtime status php-8.4
```

Running Pods keep the old interpreter until they are replaced. Use
`brewlet k8s runtime status` to find Pods still bound to an older generation,
or opt in to [restart on rotation](operations.md#rotate-without-rebuilding-application-bytes). A request sent during the
rollout can still reach a terminating Pod with the old version.

## Pitfalls and limits

- PHP's built-in web server is for evaluation, not production. PHP-FPM
  needs a separate web server, which this guide does not cover.
- The runtime root and `/app` are read-only. Sessions, uploads, OPcache file
  caches and logs need [approved writable volumes](resources.md#probes-exec-files-and-shutdown)
  and matching `php.ini` or `-d` settings.
- Extensions and `php.ini` defaults come from the runtime image, not your
  application. A runtime change can alter them.
- A runtime-only update does not update Composer dependencies. Rebuild the
  application for those.
