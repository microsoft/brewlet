# CLI reference — `brewlet`

The `brewlet` CLI lets a developer ship
**their Java application** — a fat JAR, plus optional classpath layers — as an OCI
artifact and the node-resident JVM runs it (e.g. `java -jar`). Download a
verified platform archive with the installer:

```bash
curl -fsSL https://brewlet.sh/install.sh | sh -s -- --version latest --install-dir "$HOME/.local/bin"
export PATH="$HOME/.local/bin:$PATH"
brewlet version
```

The installer selects the latest release by default; `--version latest` above
also overrides any previously exported `BREWLET_VERSION`. Replace `latest` with
a release number to pin it, or change `--install-dir` to choose another destination.
You can instead build the CLI with `make binaries` (producing `./bin/brewlet`).

```
brewlet push    <jar> <ref> [flags]   publish a JAR to a registry or local OCI layout
brewlet dependency-bundle <tar> <ref> publish an approved dependency bundle
brewlet keygen              [flags]   generate an ECDSA P-256 key pair
brewlet inspect <ref>       [flags]   show the artifact manifest + config
brewlet run     <ref>       [flags]   pull + launch java -jar on this node
brewlet bundle  <ref>       [flags]   emit an OCI runc bundle (the shim path)
brewlet stage-gc            [flags]   reclaim orphaned runnable-image stages (Linux)
brewlet k8s jdk list        [flags]   list JDKs available across the cluster
brewlet k8s doctor          [flags]   diagnose cluster and developer readiness
brewlet k8s <command>        [flags]   manage and inspect Brewlet on Kubernetes
brewlet version                       print the CLI version
```

`<ref>` is a `name:tag`, e.g. `demo/hello:1.0.0`.

> The native-artifact path reads and writes a local **OCI layout** directory
> (`--store`, default `./oci`) for local CLI / bundle workflows. The default
> runnable-image format publishes the Kubernetes workload image to a registry.
> See [Building & publishing](building-and-publishing.md).

Flags may appear before *and* after positional arguments. For `run`, everything
after a literal `--` is passed as **extra JVM args**.

---

## `brewlet keygen`

Generate the ECDSA P-256 key pair used to sign managed dependency bundle
provenance or final-image managed-dependency attestations.

```bash
brewlet keygen --private FILE --public FILE
```

| Flag | Default | Meaning |
| --- | --- | --- |
| `--private` | *(required)* | Output path for the PKCS#8 PEM private key. |
| `--public` | *(required)* | Output path for the SubjectPublicKeyInfo PEM public key. |

```bash
brewlet keygen --private platform-private.pem --public platform-public.pem
```

Keep the private key in a CI secret store. Public-key distribution and rotation
are out of band.

---

## `brewlet dependency-bundle`

Publish an approved dependency classpath to a local OCI layout. The command
expects the caller to provide a deterministic flat classpath tar and canonical
dependency lock.

```bash
brewlet dependency-bundle <classpath-tar> <ref> \
  --name NAME --version VERSION --source-bom G:A:V --lock FILE \
  [--compatible-jdks LIST] [--signing-key PEM --signer-identity IDENTITY] \
  [--store DIR]
```

| Flag | Default | Meaning |
| --- | --- | --- |
| `--name` | *(required)* | Stable bundle name. |
| `--version` | *(required)* | Bundle version. |
| `--source-bom` | *(required)* | Approved Maven BOM in `groupId:artifactId:version` form. |
| `--lock` | *(required)* | Canonical dependency-lock JSON corresponding to the classpath tar. |
| `--compatible-jdks` | *(none)* | Comma-separated compatible JDK feature versions, such as `21,25`. |
| `--signing-key` | *(none)* | PKCS#8 PEM ECDSA P-256 private key used to sign bundle provenance; requires `--signer-identity`. |
| `--signer-identity` | *(none)* | Bundle-publisher identity recorded in signed provenance; requires `--signing-key`. |
| `--store` | `./oci` | OCI layout directory to write. |

```bash
brewlet dependency-bundle ./deps.tar platform/java-deps/spring-web:2026.08 \
  --store ./oci \
  --name spring-web \
  --version 2026.08 \
  --source-bom com.example.platform:approved-spring-bom:2026.08 \
  --lock ./dependency-lock.json \
  --compatible-jdks 21,25 \
  --signing-key platform-private.pem \
  --signer-identity https://ci.example.com/platform-bundles
```

The CLI reads and writes its local `--store` layout and requires caller-supplied
locks. Use the Maven plugin for BOM import, Maven graph derivation, and direct
registry publication or consumption. See
[Managed dependency bundles](managed-dependency-bundles.md).

---

## `brewlet push`

Publish a JAR to an OCI registry or a local OCI layout (generates a minimal launch
config, or embeds one you provide). By default it publishes a **runnable image**
that kubelet/containerd can pull and unpack; pass `--format=artifact` for the
native artifact used by current local OCI-layout / CLI / `prepare-bundle`
workflows. Both formats share the current
launch contract and sandbox assembly. Native artifacts are not Kubernetes pod
images, even if their blobs are delivered out of band (see
[runnable-image delivery](runnable-image.md)).

```
brewlet push <jar> <ref> [--format image|artifact] [--store DIR] [--config FILE]
                         [--arch LIST | --no-arch]
                         [--classpath-layer TAR ...] [--module-layer TAR ...]
                         [--dependency-bundle REF --dependency-lock FILE
                          --main-class CLASS
                          [--trusted-public-key PEM --trusted-signer-identity IDENTITY]
                          [--signing-key PEM --builder-identity IDENTITY]]
                         [--appcds-archive JSA | --appcds [--appcds-java JAVA]
                          [--appcds-timeout SEC] [--appcds-arg ARG ...]]
                         [--push-result FILE]
                         [--insecure-registry HOST[:PORT] ...]
                         [--allowed-token-realm HOST[:PORT] ...]
```

**Where it publishes.** When `<ref>` names a registry host — its first path
component contains `.` or `:` or is `localhost` (e.g.
`myacr.azurecr.io/team/app:1.4.2`, `localhost:5000/app:1`) — `push` uploads the
image straight to that registry and prints the digest-pinned deploy image. A ref
without a host (`demo/hello:1.0.0`) is written to the local `--store` layout and
**never** defaults to Docker Hub; use `docker.io/<user>/app:tag` to target Docker
Hub explicitly. Passing `--store` explicitly keeps a host-qualified ref local, so
existing `--store` workflows are unchanged.

Registry pushes use the same credential chain and trust policy as the
[Maven plugin](building-and-publishing.md#option-c-maven-plugin) (minus
`settings.xml`):

1. Docker config (`$DOCKER_CONFIG/config.json` or `~/.docker/config.json`): a
   matching per-registry `credHelpers` entry with a nonempty helper name is
   authoritative **within Docker config**: inline `auths` and the default
   `credsStore` are not tried, even if that helper fails or returns no usable
   credentials. Without a matching nonempty helper entry, Brewlet tries inline
   `auths` (`identitytoken` before `auth`), then the default `credsStore` if no
   usable inline credentials are found. Helpers run as
   `docker-credential-<name> get` — so `docker login` and `az acr login` just work.
2. `BREWLET_REGISTRY_USERNAME` / `BREWLET_REGISTRY_PASSWORD` if Docker config
   yields no usable credentials, including after a per-registry helper failure.
3. Otherwise anonymous.

Identity tokens (e.g. from `az acr login`) are exchanged with an OAuth2
refresh-token grant and are never sent as Basic credentials. Registry credentials
are only sent to the registry origin or its token service on the same origin;
a token realm on another origin receives them only if listed with
`--allowed-token-realm` (Docker Hub's `auth.docker.io` is built in). Plain HTTP is
used only for loopback registries and `--insecure-registry` entries. Blobs the
registry already has are skipped, and upload progress is reported on stderr.
`--dependency-bundle` is not supported for registry pushes — use the Maven plugin
or build into a local layout with `--store`.

| Flag | Default | Meaning |
| --- | --- | --- |
| `--format` | `image` | Delivery format: `image` (standard, kubelet-pullable runnable image — the `runtimeClassName: brewlet` pod image) or `artifact` (native artifact for local OCI-layout / CLI / `prepare-bundle` workflows, not Kubernetes execution). See [runnable-image delivery](runnable-image.md). |
| `--store` | `./oci` | OCI layout directory to write the artifact into. Setting it explicitly keeps a registry-hosted `<ref>` local instead of uploading it. |
| `--config` | *(none)* | Path to a `jvm-config.json` to embed verbatim (overrides the generated one). See the [launch config schema](building-and-publishing.md#2-the-launch-config). |
| `--arch` | *(auto-detected)* | Comma-separated architecture constraint (e.g. `amd64` or `amd64,arm64`) for a **non-portable (JNI) JAR**: injects `kubernetes.io/arch` nodeAffinity and denies scheduling with `NoCompatibleArch` when no ready node matches. Overrides native-library auto-detection. Omit for arch-neutral bytecode (the default). See [multi-arch](multi-arch.md). |
| `--no-arch` | `false` | Disable native-library auto-detection and publish with **no** arch constraint (force arch-neutral), even when bundled natives are found. |
| `--classpath-layer` | *(none)* | Tar of dependency JARs to attach as a class-path layer (repeatable), unpacked to `/app/lib`. See [layered classpath deployment](layered-classpath-deployment.md). |
| `--module-layer` | *(none)* | Tar of library modules to attach as a module-path layer (repeatable), unpacked to `/app/mods` and fed to `--module-path`. See [JPMS support](jpms-support.md). |
| `--dependency-bundle` | *(none)* | Approved managed dependency bundle reference in the same local `--store`. Requires `--format image` and `--dependency-lock`; mutually exclusive with classpath/module layers and AppCDS. |
| `--dependency-lock` | *(none)* | Canonical lock for the application's resolved Maven runtime graph; required with `--dependency-bundle`. |
| `--main-class` | *(none)* | Application main class for managed classpath launch; required with `--dependency-bundle` unless a classpath-mode `--config` supplies `entry.mainClass`. |
| `--trusted-public-key` | *(none)* | ECDSA P-256 public key trusted to sign the selected bundle; paired with `--trusted-signer-identity` when bundle provenance is present. |
| `--trusted-signer-identity` | *(none)* | Expected bundle-publisher identity in signed bundle provenance; paired with `--trusted-public-key`. |
| `--signing-key` | *(none)* | PKCS#8 PEM ECDSA P-256 private key used to sign final-image managed-dependency evidence; paired with `--builder-identity`. |
| `--builder-identity` | *(none)* | Application-builder identity recorded in the signed final-image evidence; paired with `--signing-key`. |
| `--appcds-archive` | *(none)* | Prebuilt Application Class-Data Sharing archive (`.jsa`) to ship, mounted at `/app/<name>` and launched with `-Xshare:auto -XX:SharedArchiveFile`. Sets `cds.archive` in the config from the file's basename (unless `--config` already declares one, which must then match). Best-effort startup accelerator; see [AppCDS](appcds.md). |
| `--appcds` | `false` | Generate the AppCDS archive **turnkey** — run a self-terminating training JVM against the JAR, then ship the result (the generate-it-for-me equivalent of `--appcds-archive`). Fat-JAR only; mutually exclusive with `--appcds-archive`, `--classpath-layer`, and `--module-layer`. See [AppCDS §4.2](appcds.md). |
| `--appcds-java` | *(auto)* | `java` executable (or a `JAVA_HOME` directory) used for `--appcds` training. Defaults to `$JAVA_HOME/bin/java`, then `java` on `PATH`. |
| `--appcds-timeout` | `120` | Seconds to wait for the `--appcds` training JVM to self-terminate. |
| `--appcds-arg` | *(none)* | Workload argument passed to the `--appcds` training JVM to drive class loading (repeatable). |
| `--push-result` | *(none)* | Registry push only: write a `push.json` handoff — `{"image","digest","deployImage","format"}`, the same schema as the Maven plugin's `target/brewlet/push.json` — to this file. |
| `--insecure-registry` | *(none)* | Registry push only: `HOST[:PORT]` that may be reached (and whose token realm may be reached) over plain HTTP (repeatable). Loopback registries always may. |
| `--allowed-token-realm` | *(none)* | Registry push only: `HOST[:PORT]` of a cross-origin token service trusted to receive registry credentials (repeatable). |

By default `push` scans the JAR for bundled native libraries and sets the `arch`
constraint automatically for non-portable artifacts (pass `--arch` to override, or
`--no-arch` to opt out). AppCDS is opt-in: ship a prebuilt archive with
`--appcds-archive`, or let the CLI build one with `--appcds` (the two are mutually
exclusive). During `--appcds` training the CLI prints a heartbeat to stderr at
least every 30 seconds and reports the archive size and training time when done.

```bash
brewlet push ./target/app.jar demo/hello:1.0.0                        # runnable image (default)
brewlet push ./target/app.jar myacr.azurecr.io/team/hello:1.0.0       # straight to a registry
brewlet push ./target/app.jar localhost:5000/hello:1 --push-result push.json
brewlet push ./target/app.jar demo/hello:1.0.0 --format artifact      # native artifact
brewlet push ./target/app.jar demo/hello:1.0.0 --config ./jvm-config.json
brewlet push ./target/app.jar demo/hello:1.0.0 --config ./cfg.json --classpath-layer deps.tar
brewlet push ./target/orders.jar demo/orders:1.0.0 --module-layer mods.tar
brewlet push ./target/app.jar demo/hello:1.0.0 --appcds-archive ./target/app.jsa
brewlet push ./target/app.jar demo/hello:1.0.0 --appcds                # generate + ship an AppCDS archive
brewlet push ./target/native-app.jar demo/native:1.0.0 --arch amd64,arm64   # non-portable (JNI) JAR
brewlet push ./target/orders.jar apps/orders:1.4.2 \
  --store ./oci --format image \
  --dependency-bundle platform/java-deps/spring-web:2026.08 \
  --dependency-lock ./application-dependency-lock.json \
  --main-class com.example.OrdersApplication
```

Output confirms the pushed digest and store (for a runnable image, the multi-arch index
digest and target platforms; for a native artifact, the manifest digest and
`artifactType`) — and reminds you that you shipped **only the JAR**, no Dockerfile.
A registry push prints the registry, how many blobs were uploaded or already
present, and the digest-pinned `deploy image:` to use in Kubernetes manifests.

---

## `brewlet inspect`

Print the artifact's OCI manifest and its JVM launch config. Managed dependency
bundles and final images also show their lock/evidence. Optional trust flags
verify signed bundle provenance or a final-image attestation.

```
brewlet inspect <ref> [--store DIR]
                      [--trusted-public-key PEM --trusted-signer-identity IDENTITY]
```

| Flag | Default | Meaning |
| --- | --- | --- |
| `--store` | `./oci` | OCI layout directory to read from. |
| `--trusted-public-key` | *(none)* | ECDSA P-256 public key used to verify managed provenance or attestation; requires `--trusted-signer-identity`. |
| `--trusted-signer-identity` | *(none)* | Expected signed identity. For a bundle this is its publisher; for a final image this is its application builder. Requires `--trusted-public-key`. |

```bash
brewlet inspect demo/hello:1.0.0
# == manifest ==   (OCI manifest with brewlet media types)
# == jvm config == (mainJar, entry, enablePreview, addOpens, systemProperties, …)
```

---

## `brewlet run`

Resolve the artifact, assemble a local sandbox, and launch `java -jar` in the
foreground using a node-resident JDK and the invoking OS user's credentials.
This is the Layer‑1 demo path (no cgroups).

```
brewlet run <ref> [--store DIR] [--jdk-root DIR] [--launcher NAME] [--appcds-regenerate] [-- <extra jvm args>]
```

| Flag | Default | Meaning |
| --- | --- | --- |
| `--store` | `./oci` | OCI layout directory to read from. |
| `--jdk-root` | *(none)* | Node JDK home to launch with. When unset, falls back to `BREWLET_JDK_HOME`, then `JAVA_HOME`, then `java` on `PATH`. |
| `--launcher` | `java` | Launcher binary **name** under the selected JDK (or a compatible node-installed launcher name such as `jaz`), resolved on `PATH`. It is a lowercase DNS-1123 token, never a path: separators, `..` and absolute paths are rejected. |
| `--appcds-regenerate` | `false` | Opt into **node-side AppCDS regeneration** ([AppCDS §4.3](appcds.md); the local-dev equivalent of the deployment's `spec.jvm.cds.regenerate`). Maintains a private entry keyed by the fixed local scope, verified resolved manifest digest, and exact JDK build under `$BREWLET_CDS_CACHE` (default `/opt/brewlet/cds`). `-XX:+AutoCreateSharedArchive` (JDK 19+) self-heals it after a central JDK patch; any shipped archive is optional seed data. |
| `-- <args>` | *(none)* | Everything after `--` is appended as extra JVM args, immediately before the artifact's entrypoint (the local-dev equivalent of descriptor `jvm.args`). Args that select the entrypoint — `-jar`, `-cp`/`-classpath`/`--class-path`, `-p`/`--module-path`, `-m`/`--module`, and `@argfile` — are rejected: the artifact owns the entrypoint. |

```bash
brewlet run demo/hello:1.0.0
brewlet run demo/hello:1.0.0 -- -Dspring.profiles.active=dev -XX:+UseZGC
brewlet run demo/hello:1.0.0 --jdk-root /opt/brewlet/jdks/temurin-21
brewlet run demo/hello:1.0.0 --launcher jaz
brewlet run demo/hello:1.0.0 --appcds-regenerate
```

It prints the selected node JDK, the launcher (if a custom one like `jaz` is
configured), the sandbox path, and the exact launch command line before the JVM's
own output.

---

## `brewlet bundle`

Emit the **OCI runtime bundle** (`config.json` + rootfs layout) that the containerd
shim feeds to runc. Useful to see exactly what will run on a node.

```
brewlet bundle <ref> [--store DIR] [--cpu N] [--memory M] [--uid UID] [--gid GID] [--jdk-root DIR] [--launcher NAME] [--launcher-root DIR] [--appcds-regenerate] [--out DIR]
```

| Flag | Default | Meaning |
| --- | --- | --- |
| `--store` | `./oci` | OCI layout directory to read from. |
| `--cpu` | *(unlimited)* | CPU limit, e.g. `2` or `500m` → sandbox `cpu.max`. Minimum `10m` (`0.01` CPU), matching Linux's minimum quota for the 100ms period. |
| `--memory` | *(unlimited)* | Memory limit, e.g. `512Mi` or `1Gi` → sandbox `memory.max`. |
| `--uid` | `65532` | Trusted runtime process UID (`0`–`4294967294`) written to the OCI bundle. Artifact metadata cannot set it. |
| `--gid` | `65532` | Trusted runtime process GID (`0`–`4294967294`) written to the OCI bundle. Artifact metadata cannot set it. |
| `--jdk-root` | `/opt/brewlet/jdks/temurin-21` | Node JDK runtime root to mount read-only. |
| `--launcher` | `java` | Launcher binary **name** to record in the runtime spec annotations and execute. Same token rule as `brewlet run --launcher`. |
| `--launcher-root` | *(none)* | Node launcher-layer root for a custom launcher (e.g. `jaz`). |
| `--appcds-regenerate` | `false` | Opt into **node-side AppCDS regeneration** ([AppCDS §4.3](appcds.md); the local equivalent of the deployment's `spec.jvm.cds.regenerate`). Bind-mounts only the private local cache entry keyed by `--uid` at `/run/brewlet/cds` and prepends `-XX:+AutoCreateSharedArchive` (JDK 19+). |
| `--out` | `./bundle` | Output bundle directory. |

Supplied limits must be positive and representable. Malformed values, unsupported
units such as `512MB`, zero, overflow, and CPU limits below `10m` fail before the
bundle is written; they do not silently become unlimited. Omit a limit flag to
leave that resource unconstrained.

```bash
brewlet bundle demo/hello:1.0.0 --cpu 2 --memory 512Mi --uid 1000 --gid 1000 --out ./bundle
cat ./bundle/config.json
# On a Linux node the shim runs the equivalent of:
#   runc run -b ./bundle brewlet-<id>
```

---

## `brewlet stage-gc`

Perform one reference-aware cleanup sweep on a Linux node. Run as root in the
host PID, mount, and initial user namespaces, with complete host `/proc`
visibility and access to containerd.

```text
brewlet stage-gc [--stage-root DIR] [--address SOCKET] [--min-age DURATION] [--dry-run]
                 [--allow-nested-pid-namespace]
```

| Flag | Default | Meaning |
| --- | --- | --- |
| `--stage-root` | `BREWLET_RUNNABLE_STAGE`, otherwise `/tmp/brewlet-runnable` on Linux (ignores `TMPDIR`) or `os.TempDir()/brewlet-runnable` elsewhere | Administrator-owned staging root; must match the shim. |
| `--address` | `/run/containerd/containerd.sock` | containerd endpoint; references are checked across all namespaces. |
| `--min-age` | `24h` | Positive Go duration. Only published stages with directory modification times older than this floor are eligible; this is not time since last use. |
| `--dry-run` | `false` | Apply the same eligibility checks and report candidates without deleting them. |
| `--allow-nested-pid-namespace` | `false` | Test-only. Accept a non-initial PID namespace when the reaper shares it with that namespace's PID 1, for nodes that are themselves containers (kind). Processes outside that namespace are not inspected; never use on real nodes. |

On a node installed by the provisioner, the CLI is packaged under a helper name:

```bash
sudo /usr/local/bin/brewlet-stage-gc stage-gc --dry-run
```

With the standalone `brewlet` CLI installed on the host:

```bash
sudo brewlet stage-gc --dry-run
# Only on a safely established installation with no unguarded consumers:
sudo brewlet stage-gc --min-age 24h
```

Only canonical `immutable-v2/<manifest-hex>` directories are considered. Image
and content references, live mounts, or an active staging guard prevent
reclamation. Incomplete reference inspection fails closed; unmanaged layouts and abandoned
pending directories are not removed. The command reports stage counts, logical
bytes reclaimed (or eligible in dry-run), and remaining logical stage bytes.
In dry-run, remaining bytes describe the current tree, not projected usage.
Errors, including lock contention, exit nonzero. A five-minute context deadline
and TERM/INT cancellation bound cooperative work; filesystem operations already
in progress may take longer to return.

**Manual invocation does not check `stageGC.enabled`, node ownership, or the
provisioner's installation safety record.** Establish the
[consumer-safety prerequisites](runnable-image.md#existing-installations-and-unguarded-consumers) before
deleting anything. NodeProfile-managed nodes already have a periodic loop; do not
add a second timer. For non-Kubernetes CLI/bundle use only, arrange a host timer
after those checks; this does not provide an operator-free Kubernetes
provisioning model. Manual cleanup is not a workaround for blocked automatic GC.
See [Runnable stage cleanup](runnable-image.md#reclaiming-unused-stages).

---

## Removed CLI aliases

**Pre-GA breaking change ([#183](https://github.com/microsoft/brewlet/issues/183)):**
the original top-level aliases are no longer supported. Update scripts and
saved commands using these replacements:

| Removed command | Supported replacement |
| --- | --- |
| `brewlet jdks` | `brewlet k8s jdk list` |
| `brewlet doctor` | `brewlet k8s doctor` |

Keep applicable flags after the replacement command, including `--kubeconfig`,
`--context`, `--output`, the inventory's `--selector`, and doctor's `--namespace`.
Inventory and readiness checks retain their behavior and output formats.
The removed forms exit with status 2, print the replacement hint to stderr,
leave stdout empty, and do not contact the cluster, even with `--help`.
This follows the [pre-GA compatibility policy](compatibility.md); there is no
automatic forwarding or deprecation window.

## `brewlet k8s jdk list`

List the JDKs available across the cluster — **vendor, major version, minor
version, and architecture** — so you can match your dev and CI toolchains to
production. It reads the `brewlet.sh/jdks-info` inventory annotation each Brewlet
node advertises, via `kubectl get nodes` (no in-process Kubernetes client, so it
uses your existing kubeconfig/context).

```
brewlet k8s jdk list [--output table|wide|json] [--kubeconfig FILE] [--context CTX] [--selector SEL]
```

| Flag | Default | Meaning |
| --- | --- | --- |
| `--output` | `table` | `table` = distinct JDKs across the fleet with a node count; `wide` = one row per node; `json` = machine-readable aggregation (e.g. to drive a CI matrix). |
| `--kubeconfig` | *(kubectl default)* | Path to a kubeconfig file. |
| `--context` | *(current)* | kubeconfig context to use. |
| `--selector` | *(none)* | Label selector to filter nodes (passed to `kubectl -l`). |

```bash
brewlet k8s jdk list
# VENDOR             DISTRIBUTION   MAJOR   VERSION   ARCH    NODES
# Microsoft          microsoft      25      25        amd64   3
# Eclipse Adoptium   temurin        21      21.0.5    amd64   3
# Eclipse Adoptium   temurin        21      21.0.5    arm64   2

brewlet k8s jdk list --output wide              # per-node breakdown
brewlet k8s jdk list --output json              # for scripting / CI matrices
brewlet k8s jdk list --selector brewlet.sh/runtime=ready
```

Only structured `brewlet.sh/jdks-info` entries are listed. Nodes with absent,
blank, or empty structured inventory are omitted, even if they advertise
`brewlet.sh/jdks` compact tokens. An empty result succeeds with an explanatory
message in table/wide output or `[]` in JSON. Malformed nonblank structured
metadata fails explicitly with the node and annotation name; compact data
never substitutes for it. See
[JDK management → Inspecting the JDKs available](jdk-management.md#inspecting-the-jdks-available-on-the-cluster)
for plain-`kubectl` queries and guidance for compact-only nodes.

---

## `brewlet k8s doctor`

Check whether a cluster is ready to accept Brewlet workloads and whether the
current identity can deploy a `JavaApplication` in a target namespace.

```
brewlet k8s doctor [--namespace NS] [--output table|json|yaml]
                   [--kubeconfig FILE] [--context CTX]
```

The command checks the selected context, API connectivity, the `brewlet`
RuntimeClass, the `JavaApplication` CRD, schedulable Brewlet-ready containerd
nodes, advertised JDK inventory, and create permission in the selected
namespace. Without `--namespace`, it uses the selected context's namespace, or
`default` when the context sets none. Failed checks include a remediation hint. In table output each
check is printed as soon as it completes, with a progress indicator on stderr
while the next `kubectl` call runs.

```bash
brewlet k8s doctor --namespace my-team
brewlet k8s doctor --context staging --namespace my-team
brewlet k8s doctor --namespace my-team --output json
```

The command exits non-zero when a blocking check fails. JSON output is suitable
for CI and platform handoff automation.

The JDK inventory check uses only `brewlet.sh/jdks-info`. It fails if no
structured JDK entries are advertised anywhere or if structured metadata is
malformed. It does not require every node to advertise structured inventory.
If only compact `brewlet.sh/jdks` tokens are present, check current
node-provisioner publication as well as the active NodeProfile's configuration;
desired JDK configuration alone does not prove that diagnostic metadata exists.

---

## `brewlet k8s`

Brewlet-specific Kubernetes operations, using your installed `kubectl` and
existing kubeconfig credentials. Installation also requires Helm. No command
creates a cluster, changes your current context, directly modifies a node, or
opens a debugger.

```text
brewlet k8s jdk list
brewlet k8s launcher list
brewlet k8s profile list
brewlet k8s profile inspect NAME
brewlet k8s profile delete NAME [--wait] [--wait-timeout 10m] [--yes]
brewlet k8s inspect app NAME
brewlet k8s app status NAME
brewlet k8s app wait NAME [--wait-timeout 5m]
brewlet k8s status
brewlet k8s doctor
brewlet k8s install --version X.Y.Z --values FILE
brewlet k8s jdk add --profile NAME --distribution NAME --feature N --image REF --java-home PATH
brewlet k8s launcher add --profile NAME --name NAME --image REF --path PATH
```

**Command verbs describe their effects:** `add` updates the live profile,
`profile delete` deletes it, and `install` installs Brewlet. `--dry-run` previews those operations without saving
changes and prints the proposed YAML/JSON only after validation succeeds.
Failed dry runs exit nonzero with an error on stderr and no rendered output on
stdout. `list`, `inspect`, `status`, `app status`, `app wait`, and `doctor` are read-only. Redirecting
stdout to a file does not change these semantics.

### Connections and output

Common flags may precede the command or follow its full name, for example
`brewlet k8s --context staging profile inspect java-workers --output json`.
Flags may also follow positional names.

| Flag | Default | Meaning |
| --- | --- | --- |
| `--kubeconfig FILE` | Tool default | Kubeconfig passed to every Kubernetes/Helm invocation. |
| `--context NAME` | Current context | Explicit context, without modifying kubeconfig. |
| `--namespace NAME` | Command-dependent | The namespace the command operates on, as in `kubectl`. See [Namespaces](#namespaces). |
| `--timeout DURATION` | `30s` | Deadline for each kubectl invocation, including credential helpers. |
| `--output FORMAT` | Command-dependent | Read commands default to `table` and support `json`/`yaml`. JDK/launcher inventory supports `table`/`wide`/`json` instead. `add`, including dry runs, defaults to `yaml` and also supports `json`. Installation reports step-by-step progress and live rollout status on stderr (a spinner on terminals, periodic lines otherwise) and prints Helm output to stdout after a successful command. |

### Namespaces

`--namespace` always selects the namespace the command operates on:

| Command | Meaning | Default |
| --- | --- | --- |
| `status` | Brewlet control-plane namespace | Auto-discovered; `brewlet` if none is found or discovery is forbidden |
| `install` | Control-plane and Helm release namespace | `brewlet` |
| `inspect app` | Application namespace | Current context namespace |
| `app status`, `app wait` | Application namespace | Current context namespace |
| `doctor` | Application namespace for the permission check | Current context namespace, else `default` |
| `jdk list`, `launcher list`, `profile list`, `profile inspect`, `profile delete`, `jdk add`, `launcher add` | Not applicable: these resources are cluster-scoped | Rejected with an error |

Inspection's default output is a structured YAML report rather than a flattened
table. All commands propagate tool/API failures; RBAC-denied reads are not
reported as empty inventories. Output goes to stdout and mutation notices to
stderr.

### Inventory, status, and inspection

```bash
brewlet k8s jdk list --output wide
brewlet k8s launcher list --selector brewlet.sh/runtime=ready
brewlet k8s profile list --output json
brewlet k8s profile inspect java-workers
brewlet k8s status --namespace brewlet --output json
brewlet k8s inspect app orders --namespace my-team
brewlet k8s doctor --namespace my-team
```

JDK inventory uses [structured-only JDK aggregation](#brewlet-k8s-jdk-list).
Launcher inventory aggregates optional launcher
names from `brewlet.sh/launchers`; the JDK's implicit `java` launcher is not a
separate entry. Both inventories describe **node-advertised state**, not a
catalog, a live probe of node files, or proof that a particular Pod uses a JDK.
Use `--selector` to restrict nodes, such as to runtime-ready nodes.

Profile listing shows **desired** JDK/launcher inventories, pool selection,
readiness, and node counts. Profile inspection adds the full spec, conditions,
field-management information, and claimed node summaries. Node identity and
ownership must match the profile's UID. Missing or stale observed generations
never count as a current Ready condition.

`status` reports the operator and admission **Deployment rollout state** in
the control-plane namespace, profile conditions, and node readiness and
provisioning errors. Without `--namespace`, it lists Deployments labeled
`app.kubernetes.io/name=brewlet` across all namespaces and uses the namespace
holding `brewlet-operator`/`brewlet-admission`. It stops and lists the
candidates when more than one namespace matches, and falls back to `brewlet`
when none matches or the cluster-wide list is forbidden. When neither component
exists in the selected namespace, it fails with
`no Brewlet control plane found in namespace "NAME"; pass --namespace`. The
selected namespace is printed first in table output and as `namespace` in
JSON/YAML. It exits nonzero if either component is absent or
unready, any profile/node is unready, or there are no profiles or no uncordoned,
runtime-ready, Kubernetes-ready nodes. An intentionally disabled admission
Deployment therefore also produces a nonzero result. This is a conservative
rollout snapshot, not a webhook TLS, certificate, endpoint, or scheduling probe.

Application inspection follows controller ownership from JavaApplication to
Deployment, ReplicaSets, and Pods, and includes their events. Matching labels
alone do not establish ownership. Reports distinguish the **requested JDK**
from the actual per-replica runtime, which Brewlet does not yet report. They
show Pod placement, restarts, and container failure reasons without dumping
environment variables or Secrets. Condition/event messages are operator-provided
text; review them before sharing a report. Inspection succeeds even for an
unready resource; use `status` or `doctor` for readiness exit codes.

### Application readiness and waiting

```bash
brewlet k8s app status orders --namespace my-team
brewlet k8s app status orders --namespace my-team --output json

# After kubectl apply, a GitOps sync, or in a CI script:
kubectl apply -f orders.yaml -n my-team
brewlet k8s app wait orders --namespace my-team --wait-timeout 5m
```

`app status` is a concise readiness view of a JavaApplication: its phase
(`Ready`, `Pending`, `Progressing`, `Failed` for `ReconcileError`, or
`Terminating`), the `Ready` reason and message, `metadata.generation` versus
`status.observedGeneration`, ready replicas, the selected JDK
(`status.selectedJdk`), the nodes running its Pods with their per-Pod JDK
request, conditions, and up to 10 most recent events for the application and
the objects it owns. Ownership is traversed as in `inspect app`. `--output`
accepts `table` (default), `json`, or `yaml`. It exits zero for an unready
application; use `app wait` for a readiness exit code.

`app wait` polls the JavaApplication every 3 seconds until its `Ready`
condition is `True` **for the current generation**: a `Ready=True` condition
whose `observedGeneration` is older than `metadata.generation` does not count.
This is the same rule the Maven plugin's `brewlet:deploy` goal uses, so both
tools report the same thing. Each status change (`Reason: message`) is printed
to stderr with the elapsed time; a spinner (terminals) or a periodic heartbeat
(CI, pipes) shows it is still waiting. A missing application or a failed
`kubectl` call is reported and retried, so you can start waiting before a GitOps
sync has created the object. On success it prints
`NAME is Ready in namespace NS (JDK temurin-21)` to stdout and exits zero. When
`--wait-timeout` (default `5m`) expires it exits nonzero with the last status
and a `kubectl describe javaapplication NAME -n NS` hint. `--timeout` remains
the per-kubectl-call deadline.

Read permissions are required for the objects each command inspects.
`app wait` only gets the JavaApplication; `app status` also lists the same
objects as application inspection.
Application inspection lists Deployments, ReplicaSets, Pods, and Events in its
namespace; profile inspection reads the profile and lists Nodes. `status` lists
control-plane Deployments (cluster-wide when discovering the namespace),
NodeProfiles, and Nodes.

### Installation

`--values FILE` takes a **Helm values file that you create**, not a Brewlet CLI
configuration file. `values.yaml` is an example filename, not a file generated
by the command or automatically loaded from this repository. You may name it
anything and pass its path with `--values` or `-f`.

#### Draft your values file

For a first installation, save the following as `values.yaml`. This is a
minimal configuration for one existing worker pool and one JDK; other settings
inherit the selected chart release's defaults.

```yaml
provisioner:
  pools:
    - javaworkers
  jdks:
    - distribution: temurin
      feature: 21
      source:
        image: docker.io/library/eclipse-temurin@sha256:<64-lowercase-hex>
        javaHome: /opt/java/openjdk
```

Replace the example choices before running the command:

| Field | What to supply |
|---|---|
| `provisioner.pools` | Names of existing node pools Brewlet may provision, not individual node names. The CLI does not create these pools. Replace `javaworkers` with a pool you control; AKS pool names are lowercase alphanumeric only (no dashes). |
| `provisioner.jdks[].distribution` | A lowercase inventory name for the JDK, such as `temurin` or `microsoft`. It does not select or download an image automatically. |
| `provisioner.jdks[].feature` | The Java feature version actually supplied by your source image, such as `21` or `25`. |
| `provisioner.jdks[].source.image` | The fully qualified, tagless image reference pinned to the approved manifest or multi-platform index digest. Replace `<64-lowercase-hex>` with all 64 lowercase hexadecimal characters from that digest. |
| `provisioner.jdks[].source.javaHome` | The absolute JDK home **inside the source image**, not your workstation's `JAVA_HOME`. Confirm that `<javaHome>/bin/java` exists in that image. |

The Temurin entry is an example, not a built-in runtime recommendation. Choose
an image that supports every architecture in the selected pool; see the
[JDK source model](jdk-management.md#source-model) for source requirements.
Pool labels are detected automatically for supported providers. For bare-metal
or nonstandard labels, set `provisioner.poolKey` as described in
[pool configuration](configuration.md#where-the-provisioner-may-run).

The chart deliberately leaves `provisioner.pools` and `provisioner.jdks` empty:
they are required while `defaultProfile.enabled` is true. Copying the chart's
default file unchanged will therefore fail. You do not need to copy every
setting into your own file; include your pool, complete JDK inventory, and any
other overrides you need. Additional JDKs are entries in the same `jdks` list.
Optional launchers belong in `provisioner.launchers`; omit that list to use the
JDK's implicit `java` launcher.

The [full Helm values reference](configuration.md#helm-chart-values) and
[annotated chart defaults](https://github.com/microsoft/brewlet/blob/main/kubernetes/charts/brewlet/values.yaml)
describe optional settings, including launchers, mirrors, tolerations, rollout
policy, and additional profiles. Use settings supported by the chart version
you select. For GitOps-managed profiles, you may instead disable
`defaultProfile.enabled` and supply your own NodeProfiles; see
[installation and profile ownership](installation.md#helm-recommended).

#### Preview and install

After saving and reviewing `values.yaml`, replace `x.y.z` with the exact chart
release you want and `evaluation` with your intended kubeconfig context:

```bash
# Set this to the exact approved chart release, not "latest".
RELEASE_VERSION=x.y.z

# Download and render the chart; does not contact the Kubernetes API.
brewlet k8s install --version "$RELEASE_VERSION" --values values.yaml --dry-run

# Install into a deliberately selected fresh cluster.
brewlet k8s install --context evaluation --version "$RELEASE_VERSION" \
  --values values.yaml --namespace brewlet
```

Preview validates chart rendering, not source image contents or node readiness.
If it fails, fix the reported values error before installing. Installation is
privileged and modifies the selected hosts; review the
[cluster prerequisites](installation.md#prerequisites) first. Control-plane
nodes and tainted pools require deliberate configuration, not an implicit opt-in.

The CLI invokes `helm install` against
`oci://ghcr.io/microsoft/charts/brewlet`, creates the namespace, and waits for
chart resources. `--release` defaults to `brewlet`; `--wait-timeout` defaults to
`5m`. The Helm process deadline includes that duration plus `--timeout` for
setup. `--values`/`-f` may be repeated in Helm precedence order. The CLI sets the
chart's `namespace` value to the selected release namespace so resources and the
release cannot accidentally land in different namespaces.

Installation refuses existing Brewlet CRDs and Helm refuses an existing release.
It does **not** upgrade a release or migrate CRDs. Pre-GA release updates default
to [safe teardown/reinstallation](installation.md#upgrading), including review of
CRDs retained after uninstall; their presence does not authorize an in-place
upgrade with Helm. Preview uses `helm template --include-crds`; it
still needs chart registry access, and it is not server-side validation.

Helm readiness is not node provisioning completion. Follow installation with
`brewlet k8s status` and inventory inspection using the same kubeconfig/context
and the installation namespace as `--namespace`. Sources are never chosen for you,
control-plane provisioning is never implicitly enabled, and no cleanup or
uninstall is attempted after a failed installation.

### Safe JDK and launcher additions

`jdk add` and `launcher add` **update the live NodeProfile by default**. They
always select an existing profile and return its updated declaration. The
source image must be a fully qualified, tagless SHA-256 digest reference; paths
and inventory names follow the [JDK](jdk-management.md#source-model) and
[launcher](launchers.md#launcher-names-are-tokens-not-paths) source contracts.

| Command mode | Effect |
| --- | --- |
| `jdk add` / `launcher add` | Persist the inventory change to the live profile. |
| `add --dry-run` | Validate locally and print the proposed declaration; equivalent to `--dry-run=client`. |
| `add --dry-run=client` | Validate the requested source and inventory change locally without sending a patch to the API server. |
| `add --dry-run=server` | Validate the conditional patch through the API server and print the result without saving it. |
| `add --dry-run --file FILE` | Read a source NodeProfile and validate/generate its proposed update offline. |
| `add --dry-run --values FILE` | Read complete Helm values and validate/generate their proposed update offline. |

Bare `--dry-run` and `--dry-run=client` still read the live profile unless
`--file` or `--values` is supplied. They do not validate registry contents, node
compatibility, or server admission policy. Use `--dry-run=server` when you need
API-server validation. Invalid modes are rejected; omit `--dry-run` entirely
to apply changes.

**Validation happens before rendering.** A failed source check, file read,
inventory update, cluster read, server dry run, or result decode returns an error
without printing a manifest. There is no fallback from a failed server dry run
to client-generated output. Shell redirection may still create or truncate its
destination file before the command runs; always redirect to a different file
and check the exit status.

Replace the digest placeholders with approved 64-character lowercase digests:

```bash
# Add a JDK to an unmanaged live profile. This changes the cluster.
brewlet k8s jdk add --profile java-workers \
  --distribution temurin --feature 25 \
  --image 'docker.io/library/eclipse-temurin@sha256:<64-lowercase-hex>' \
  --java-home /opt/java/openjdk

# Validate a launcher addition through the API server without saving it.
brewlet k8s launcher add --profile java-workers \
  --name jaz \
  --image 'mcr.microsoft.com/openjdk/jdk@sha256:<64-lowercase-hex>' \
  --path /usr/bin/jaz --dry-run=server

# Validate and print a GitOps change entirely offline from its source manifest.
brewlet k8s launcher add --dry-run --profile java-workers --file profile.yaml \
  --name jaz \
  --image 'mcr.microsoft.com/openjdk/jdk@sha256:<64-lowercase-hex>' \
  --path /usr/bin/jaz > profile-next.yaml

# Generate proposed values for a Helm-owned profile; does not deploy them.
brewlet k8s jdk add --dry-run --profile default --values values.yaml \
  --distribution temurin --feature 25 \
  --image 'docker.io/library/eclipse-temurin@sha256:<64-lowercase-hex>' \
  --java-home /opt/java/openjdk > values-next.yaml
```

`--file` accepts one NodeProfile YAML/JSON document whose name matches
`--profile`. `--values` accepts one complete Helm values document: `default`
updates `provisioner`, while other names select an existing entry in `profiles`.
Other inventories, profiles, rollout policies, and chart settings are retained.
`--file` and `--values` are mutually exclusive and require client dry-run mode
(bare `--dry-run` or `--dry-run=client`). They cannot be used for a real update
or a server dry run. Input files are never overwritten by the CLI; redirect to
a **different** file, review the diff, and commit the desired source change.
YAML formatting and comments are not retained. For layered Helm values, use the
complete inventory-owning values, not a partial list overlay: Helm replaces
arrays rather than merging entries.

Adding an identical entry is idempotent. Replacing the source for an existing
distribution/feature or launcher name requires `--replace`. Duplicate identities
and inventories above the CRD's 32-entry limit are rejected. Local generation
validates the supplied entry, not registry contents, node architecture
compatibility, or the cluster's full admission policy.

For an **unmanaged live profile**, append `--dry-run=server` to `add` to validate a
conditional patch without saving; remove `--dry-run` to persist it.
There is no `--apply` flag. **`add ... > result.yaml` still changes the cluster**;
use `add ... --dry-run > proposal.yaml` for a client-validated preview.

Live application rejects terminating profiles, Helm/Argo CD/Flux ownership
markers, owner references, and unrecognized spec field managers. It never
forces ownership. The patch changes only the selected inventory and tests both
the profile UID and resource version; a concurrent change fails without a
blind retry. These ownership checks also apply to `add --dry-run=server`. Use
client dry-run mode to prepare Helm/GitOps source changes and deploy them through
the owning system instead of bypassing those guards. Generation strips
server-owned status and lifecycle metadata from the emitted manifest; the actual
API patch leaves them untouched.

A successful patch means **the declaration was accepted**, not that every node
has installed the source. Inspect the profile, its node generations, and
advertised inventory before deploying workloads that require the new runtime.

### Deleting a profile

`profile delete NAME` deletes an unmanaged live NodeProfile and, with `--wait`,
follows the operator's host cleanup until the profile is gone. **Deletion is
destructive:** the operator removes the profile's JDK/launcher runtimes,
shim, and runtime labels from every node it claimed (see
[provisioning completion and deletion](jdk-management.md#provisioning-completion-and-deletion)).

```bash
# Preview: run every guard and print the plan without deleting.
brewlet k8s profile delete java-workers --dry-run

# Delete and follow per-node cleanup, failing after 15 minutes.
brewlet k8s profile delete java-workers --wait --wait-timeout 15m

# Follow a deletion that is already in progress (no new request is sent).
brewlet k8s profile delete java-workers --wait --output json
```

| Flag | Default | Meaning |
| --- | --- | --- |
| `--wait` | Off | Report per-node cleanup progress on stderr until the profile no longer exists. |
| `--wait-timeout DURATION` | `10m` | Overall `--wait` deadline. Requires `--wait`. `--timeout` remains the per-kubectl-call deadline. |
| `--yes` | Off | Delete even though Java workloads still run on the profile's claimed nodes. |
| `--dry-run[=client]` | Off | Run all guards and print the plan; nothing is sent to the API server. |
| `--dry-run=server` | Off | Also validate the conditional deletion through the API server without deleting. |
| `--output FORMAT` | `table` | `table`, `json`, or `yaml` result report on stdout. |

Guards run before anything is deleted:

- **Ownership.** Profiles with Helm, Argo CD, or Flux markers, owner
  references, or unrecognized spec field managers are refused, as for `add`.
  Remove the profile from its source of truth instead: for Helm, drop it from
  `provisioner.pools`/`profiles` (or set `defaultProfile.enabled=false`) and run
  `helm upgrade`; for GitOps, remove it from the repository. You can then run
  `profile delete NAME --wait` to follow the cleanup, because attaching to a
  deletion that is already in progress changes nothing.
- **Java workloads.** Claimed nodes are the profile's recorded targets plus nodes
  labelled `brewlet.sh/owner-uid=<profile UID>`. Non-terminal Pods with
  `runtimeClassName: brewlet` on those nodes block deletion unless you pass
  `--yes`, including Pods with a deletion timestamp that may still be running
  through their grace period or `preStop` hook. If Pods cannot be listed across
  namespaces, deletion fails closed unless you pass `--yes`.
- **Concurrent changes.** The delete request carries UID and resourceVersion
  preconditions. A profile edited or recreated under the same name since it
  was read is not deleted; re-inspect it and retry.

Without `--wait`, the command returns once the API server accepts the
deletion; cleanup continues asynchronously. With `--wait`, it shows a spinner
on terminals and periodic heartbeat lines otherwise, plus a line whenever a
node's state changes: `pending` (no cleanup worker yet), `cleaning` (with any
container problem such as `ImagePullBackOff`), `cleaned` (its cleanup worker
reports completion), or `released` (its ownership claim was removed).
Per-node progress reads the `brewlet-cleanup` worker Pods in the control-plane
namespace (auto-discovered as for `status`). The overall result comes from the
profile itself.

The command exits nonzero when a guard refuses the deletion, when the profile
reports `Ready=False/CleanupBlocked` or `Ready=False/UnsupportedPreClaimState`
(the actual condition reason, message, recovery guidance, and a
[troubleshooting](troubleshooting.md#nodeprofile-deletion-does-not-finish)
link are printed), or when `--wait-timeout` expires. A timeout does not cancel
cleanup. Without `--wait`, a profile that is already deleting and blocked also
exits nonzero. The CLI **never removes finalizers, ownership labels, or
status** to force deletion.

`CleanupBlocked` requires repairing the cause identified by its condition
message. `UnsupportedPreClaimState` is a permanent refusal by this release,
not a state that ordinary spec repair can migrate. Save original manifests and
evidence, pause automation, drain or move workloads, and restore the original
release's compatible components to finish cleanup and worker teardown before
reinstalling. See [pre-claim recovery](installation.md#unsupported-pre-claim-workers).
Both refusal reasons stop `--wait` as soon as they are observed; they do not
wait for the timeout or claim cleanup continues in the background.

Required permissions: `get` and `delete` on `nodeprofiles.node.brewlet.sh`,
`list` on Nodes and on Pods in all namespaces (the workload guard), and,
for `--wait` progress, `list` on Deployments across namespaces to discover the
control-plane namespace (`brewlet` is assumed when that list is forbidden) and
on Pods in that namespace.

---

## `brewlet version`

Print the release version embedded in the binarybrewlet version

Source builds without release linker flags print `dev`.

---

## Environment variables

| Variable | Used by | Meaning |
| --- | --- | --- |
| `JAVA_HOME` | `run`, `push --appcds` | Default node JDK home if `--jdk-root` is unset (`run`); default training JVM if `--appcds-java` is unset (`push --appcds`). |
| `BREWLET_JDK_HOME` | `run` | Overrides `JAVA_HOME` for JDK resolution. |
| `BREWLET_STORE_ROOT` | shim (`layout` resolver) | OCI layout root used by the local layout resolver. |
| `BREWLET_CDS_CACHE` | `run`, `bundle`, shim | AppCDS cache root (default `/opt/brewlet/cds`). Entries are private `<key>/archive.jsa` directories; Kubernetes keys include the trusted sandbox namespace, verified platform-manifest digest, JDK build, and CRI process UID. The cache root itself is never mounted into a workload. |
| `BREWLET_METRICS_SOCKET` | `run`, `bundle`, shim | Unix datagram telemetry destination (default `/opt/brewlet/metrics/telemetry.sock`). Unset or blank selects the default, not disabled telemetry. Match the exporter's `--socket-path` when overriding it. Sending is best-effort; an unavailable exporter must not fail workload launch. See [runtime metrics](runtime-metrics.md). |

---

## Exit codes

- `0` — success.
- `1` — runtime error (message on stderr, prefixed `error:`).
- `2` — usage error (unknown/mis-invoked command; prints usage).

For `run`, the launched JVM's exit code propagates as the process exit status.

## See also

- [Building & publishing](building-and-publishing.md) — the developer workflow.
- [Getting started](getting-started.md) — integration-test tiers 2 and 3 exercise
  these commands against the monorepo source.
- [Reference](reference.md) — media types, schema, and well-known paths.
