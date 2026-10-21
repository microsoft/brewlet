# Package and deploy applications

The generic producer accepts explicit source directories, a versioned launch
descriptor and OCI platforms. It does not compile your program, discover
dependencies or choose a runtime ID.

## The artifact contract

An application is a runnable OCI image with:

- One or more app layers and optional dependency layers under `/app`.
- Exactly one final launch layer containing `launch.json`.
- `brewlet.sh/artifact-version=2` on its manifest.
- An OCI config that exactly mirrors the launch process.

The CLI and shim reject unknown/duplicate descriptor fields, mirror drift,
unsafe layers and unsupported platforms. Multi-platform publication is an
explicit producer assertion, not evidence that a native dependency is portable.

```json
{
  "schemaVersion": 2,
  "process": {
    "entrypoint": [
      "python3"
    ],
    "args": [
      "/app/src/main.py"
    ],
    "workingDir": "/app",
    "env": [
      {
        "name": "PYTHONPATH",
        "value": "/app/dependencies"
      },
      {
        "name": "PYTHONDONTWRITEBYTECODE",
        "value": "1"
      }
    ]
  },
  "runtimeImages": [
    "python-3.13"
  ]
}
```

The optional top-level `runtimeImages` is an allowlist, not a selector. The
workload must still name an approved runtime explicitly.
Omission imposes no artifact-level ID restriction; an explicit empty array
denies all IDs. Null and the former `requires` wrapper are rejected.

```sh
brewlet push registry.example.com/apps/api:v2 \
  --platform linux/amd64 \
  --platform linux/arm64 \
  --launch launch.json \
  --layer dependency=./dependencies:/app/dependencies \
  --layer app=./src:/app/src \
  --push-result push.json
```

Repeated platforms mean the supplied bytes work unchanged on each platform.
Different native payloads require separate platform manifests and an explicitly
authored index descriptor.

## Independent dependency layer sets

`brewlet layer-set` publishes neutral dependency layers, a config and optional
lock. Consumption with `--layer-set` reuses the compressed layers byte-for-byte.
Manifest evidence binds their identities. Optional signed provenance and
CycloneDX inventory govern the layer set, not every final application.

The kubelet still pulls one application image per container. The shim never
resolves dependencies from a package manager or pulls a layer set itself.

## Native workload example

The following is a template, not directly executable: replace the application
digest and select an ID already approved and offered by your platform.

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: api
spec:
  replicas: 1
  selector:
    matchLabels:
      app: api
  template:
    metadata:
      labels:
        app: api
      annotations:
        brewlet.sh/runtime-image: python-3.13
    spec:
      runtimeClassName: brewlet
      nodeSelector:
        kubernetes.io/arch: arm64
      containers:
        - name: api
          image: registry.example.com/apps/api@sha256:<64-lowercase-hex>
          ports:
            - containerPort: 8000
          resources:
            requests:
              cpu: 100m
              memory: 64Mi
            limits:
              memory: 256Mi
          securityContext:
            readOnlyRootFilesystem: true
          readinessProbe:
            httpGet:
              path: /
              port: 8000
```

Admission defaults non-root identity and rejects root groups, privileges,
capabilities and startup-fence tolerations. Explicit non-root identities remain
subject to the execution contract.

## Kubernetes overrides and environment

`command`, `args` and `workingDir` retain Kubernetes semantics. In particular,
`args` replaces the image argument array; it does not append to it.
Runtime Env provides defaults and CRI Env overrides them, except that PATH is
runtime-owned. Explicit Pod PATH is ignored with a warning.

HOME is synthesized from validated account metadata when not explicitly set.
Account metadata is optional: an unknown numeric UID uses `/tmp`. This does not
create passwd entries or home directories. Absolute executable paths work
without runtime PATH; relative initial/Exec invocation then fails with
`RuntimePATHRequired`. Author paths explicitly in the descriptor instead of
relying on a runtime image entrypoint script or a shell.
Exec, probes and exec hooks reuse the initial process's effective environment,
identity and leased generation.

Packaged `/app` is read-only and world-readable. Do not bake credentials into it.
Use scoped Secret volumes for secrets and explicit writable volumes for data.
All app/init/sidecar/ephemeral containers in the Pod require v2 artifacts.

## Optional Maven producer

Java-specific project knowledge lives in the separately packaged Maven plugin.
It can prepare classpath/module argv and dependency layers, or consume one
verified complete Java layer-set graph.

```sh
mvn package sh.brewlet:brewlet-maven-plugin:2.0.0-alpha.1:push \
  -Dbrewlet.image=registry.example.com/apps/orders:v2 \
  -Dbrewlet.platforms=linux/amd64,linux/arm64 \
  -Dbrewlet.mainClass=example.Main
```

Until this candidate is actually published, build/install the matching source plugin.
The matching Brewlet CLI must be available explicitly. The plugin does not
generate Kubernetes manifests, choose Pod runtime annotations or apply workloads.
Managed dependency consumption requires a genuinely thin JAR, not a nested-JAR
application. See the [producer's complete contracts and source instructions](https://github.com/microsoft/brewlet/blob/brewlet-2.0/maven-plugin/README.md).
