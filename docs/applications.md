# Package and deploy applications

The `brewlet` CLI packages applications for any language. You supply the
built files, a launch descriptor (`launch.json`) and the target OCI platforms.
Brewlet does not compile your program, discover dependencies or choose a
runtime ID.

## The artifact contract

An application is a runnable OCI image with:

- One or more application layers under `/app`. Application code, libraries,
  generated assets, and other dependencies are all ordinary application files.
- Exactly one final launch layer containing `launch.json`.
- `brewlet.sh/artifact-version=1` on its manifest.
- An OCI config that exactly mirrors the launch process.

The CLI and shim reject unknown/duplicate descriptor fields, mirror drift,
unsafe layers and unsupported platforms. Multi-platform publication is an
explicit producer assertion, not evidence that a native dependency is portable.

```json
{
  "schemaVersion": 1,
  "process": {
    "command": [
      "python3"
    ],
    "args": [
      "/app/src/main.py"
    ],
    "workingDir": "/app",
    "env": [
      {
        "name": "PYTHONPATH",
        "value": "/app/lib"
      },
      {
        "name": "PYTHONDONTWRITEBYTECODE",
        "value": "1"
      }
    ]
  }
}
```

The workload names an approved, compatible runtime explicitly.

```sh
brewlet build registry.example.com/apps/api:v2 \
  --platform linux/amd64 \
  --platform linux/arm64 \
  --launch launch.json \
  --layer ./dependencies:/app/lib \
  --layer ./src:/app/src
brewlet push registry.example.com/apps/api:v2 --push-result push.json
```

Repeated platforms mean the supplied bytes work unchanged on each platform.
Different native payloads require separate platform manifests and an explicitly
authored index descriptor.

## Producer filesystem

Use regular world-readable files and searchable directories. The packager
rejects unsafe modes, extended attributes and escaping paths instead of
silently sanitizing them.

On macOS, the producer runs locally, but the shim and node worker execute on
Linux. The producer ignores the host-local `com.apple.provenance` and
`com.apple.quarantine` attributes because layers never carry xattrs; remove any
other extended attribute rather than stripping validation checks.

## Native workload example

The following is a template, not directly executable: replace the application
digest and select an ID already approved and offered by your platform. The
annotation suffix must match the managed container's name.

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
        runtime-image.brewlet.sh/api: python-3.13
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

The runtime image `ENTRYPOINT` always starts the initial argv. Pod
`command`/`args` then override the corresponding launch descriptor fields:

| Pod fields | Command portion after runtime `ENTRYPOINT` |
| --- | --- |
| Nonempty Pod `command` | Pod `command` followed by Pod `args`; launch fields and runtime `CMD` are ignored. |
| Only nonempty Pod `args` | `launch.json` `command` (if present) followed by Pod `args`; launch args and runtime `CMD` are ignored. |
| No Pod override; either launch field is present | `launch.json` `command` followed by `args`. Explicit empty fields suppress the runtime `CMD`. |
| No Pod override; both launch fields are omitted | The runtime image `CMD`. |

Pod `command` replaces the launch command and drops launch args when Pod `args`
is omitted. Pod `args` alone replaces only launch args and keeps the launch
command. Empty Pod lists count as unset. If both the runtime `ENTRYPOINT` and
the resulting command portion are empty, startup fails with
`LaunchCommandRequired`. Exec, probes and exec hooks run their own argv and do
not pass through the runtime `ENTRYPOINT`. `workingDir` retains Kubernetes
semantics.
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
All app/init/sidecar/ephemeral containers in the Pod require v1 artifacts.

## Optional Maven producer

Java-specific project knowledge lives in the separately packaged Maven plugin.
It can prepare classpath/module argv and package the application and its
dependencies as ordinary files in app layers.
Replace the example plugin version with the release reported by
`brewlet version`.

```sh
mvn package sh.brewlet:brewlet-maven-plugin:0.11.0:push \
  -Dbrewlet.image=registry.example.com/apps/orders:v2 \
  -Dbrewlet.platforms=linux/amd64,linux/arm64 \
  -Dbrewlet.mainClass=example.Main
```

The plugin requires Java 17 or later and a matching `brewlet` CLI on the
`PATH` (or set `-Dbrewlet.executable`). It does not generate Kubernetes
manifests, choose Pod runtime annotations or apply workloads. See
[Java applications](java.md) and the
[Maven plugin reference](maven-plugin.md).
