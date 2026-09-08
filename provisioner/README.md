# Brewlet node provisioner

[![CI](https://github.com/microsoft/brewlet/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/microsoft/brewlet/actions/workflows/ci.yml)
[![Release](https://github.com/microsoft/brewlet/actions/workflows/release.yml/badge.svg)](https://github.com/microsoft/brewlet/actions/workflows/release.yml)
[![node-provisioner image](https://img.shields.io/badge/ghcr.io-brewlet%2Fnode--provisioner-blue?logo=docker)](https://github.com/microsoft/brewlet/pkgs/container/node-provisioner)
[![License](https://img.shields.io/github/license/microsoft/brewlet)](../LICENSE.txt)

The node provisioner prepares a Linux Kubernetes node to run Brewlet workloads.
Its privileged entrypoint first validates every configured JDK and launcher
source, then installs the containerd shim, materializes runtime
roots and launcher layers, registers the `brewlet` containerd runtime, validates
the installation, and advertises node readiness.
The host must run containerd 2.0 or newer.

See the [Brewlet specification](../specs/SPECIFICATION.md) for the node
provisioning model and the [user documentation](../docs/)
for installation and operations guidance.

## Build

From the repository root:

```bash
make provisioner-image
make provisioner-image-push
```

Override the destination when needed:

```bash
make provisioner-image PROVISIONER_IMAGE=ghcr.io/acme/brewlet-provisioner:1.2.3
```

The multi-stage Docker build compiles `containerd-shim-brewlet-v2`,
`brewlet-source-policy`, and `brewlet-metrics-exporter` for the target Linux
architecture and packages them with the entrypoint.

## Runtime source model

Brewlet has no built-in JDK or launcher catalog. The operator renders every
`NodeProfile` source as indexed environment variables. Each image must be a
fully qualified, tagless
`repository@sha256:<64 lowercase hex>` reference, and each source path must be a
clean absolute path below `/`.

The provisioner validates **all** entries and mirrors before it installs the shim
or performs a host containerd operation. Missing indexed fields, mutable or
malformed images, invalid paths or names, duplicate inventory entries, and
unapproved mirrors fail closed. `JDKS` and `LAUNCHERS` are derived only after
this preflight, so an independent inventory cannot diverge from its sources.

### Supply-chain integrity

The image build downloads `kubectl`, the containerd `ctr` client, and `crictl`
only in a build-only stage. Each asset is matched against the repository-pinned
SHA-256 value for `amd64` or `arm64` before it is exposed, then checked again
immediately before extraction. The final runtime stage contains the verified
tools but not `curl`. All external Dockerfile base images are pinned by
multi-architecture manifest digest.

When updating one of these tools:

1. Update its version and source commit in `provisioner/Dockerfile` and its
   registration in `provisioner/cgmanifest.json`.
2. Retrieve the published checksum for both supported architectures:
   - kubectl: `https://dl.k8s.io/release/<version>/bin/linux/<arch>/kubectl.sha256`
   - containerd: the matching `.tar.gz.sha256sum` release asset
   - crictl: the matching `.tar.gz.sha256` release asset
3. Update `provisioner/checksums/<arch>.sha256`. If a source commit changes,
   update the matching entry in `provisioner/checksums/licenses.sha256`.
4. Refresh changed base-image manifest digests with
   `docker buildx imagetools inspect <image>:<tag>`.
5. Run:

   ```bash
   make container-security-check
   make container-security-test
   ```

The Docker-based test deliberately corrupts every downloaded asset and requires
the checksum gate to reject each build.

## Configuration

| Environment variable | Default | Purpose |
| --- | --- | --- |
| `JDK_SOURCE_COUNT` | required | Positive number of indexed JDK image sources |
| `JDK_SOURCE_<n>_TOKEN` | required | Safe `<distribution>-<feature>` inventory token |
| `JDK_SOURCE_<n>_IMAGE` | required | Fully qualified, tagless `repository@sha256:<64 lowercase hex>` reference containing the runtime and its userland |
| `JDK_SOURCE_<n>_JAVA_HOME` | required | Clean absolute JDK or jlink runtime path inside that image |
| `LAUNCHER_SOURCE_COUNT` | `0` | Number of indexed optional launcher sources |
| `LAUNCHER_SOURCE_<n>_NAME` | required per entry | Lowercase launcher name; `java` is reserved |
| `LAUNCHER_SOURCE_<n>_IMAGE` | required per entry | Fully qualified, tagless SHA-256 digest reference |
| `LAUNCHER_SOURCE_<n>_PATH` | required per entry | Clean absolute binary path inside that image |
| `JDKS` / `LAUNCHERS` | derived | Internal inventories; never accepted as independent input |
| `BREWLET_APP_CDS_REGENERATION_ENABLED` | `false` | Authorize node-side AppCDS regeneration for the active profile |
| `NODE_NAME` | downward API | Kubernetes node to label |
| `BREWLET_PROFILE_UID` | empty | Operator-managed profile UID; when set, readiness is published only while this exact profile still exists |
| `BREWLET_PROFILE_GENERATION` | `0` | Operator-managed generation paired with `BREWLET_PROFILE_UID` |
| `BREWLET_PREFIX` | `/opt/brewlet` | Host installation prefix |
| `CONTAINERD_CONFIG` | `/etc/containerd/config.toml` | containerd configuration |
| `CONTAINERD_DROPIN_DIR` | `/etc/containerd/config.toml.d` | Host drop-in directory used when the primary config imports it |
| `CONTAINERD_DROPIN_FILE` | `<drop-in-dir>/99-brewlet.toml` | Brewlet-managed runtime drop-in |
| `CONTAINERD_ADDRESS` | `/run/containerd/containerd.sock` | containerd socket |
| `CONTAINERD_NAMESPACE` | `k8s.io` | containerd namespace |
| `BREWLET_MODE` | `provision` | `provision` installs; `cleanup` reverses it |
| `BREWLET_CONTAINERD_RESTART` | `validated` | `validated`, `sighup`, or `none` |
| `BREWLET_VALIDATE` | `true` | Run JDK smoke tests and launcher executable checks before readiness |
| `MIRRORS` | empty | Strict comma-separated `<upstream-host>=<mirror-host[/path]>` mappings |
| `SOURCE_ALLOWED_MIRROR_HOSTS` | empty | Comma-separated exact destination registry hosts; empty disables mirrors |
| `SOURCE_POLICY_BIN` | `/opt/brewlet-dist/brewlet-source-policy` | Digest-reference, path, registry-host, and mirror-target validator |

Distribution and launcher names are administrator-owned inventory identifiers;
they do not select images. The provisioner copies each JDK image's complete root
filesystem so the sandbox has the loader and native libraries required by
`bin/java`. The source image need not contain shell or copy tools.

Provisioning is idempotent, records the source image and Java home, and
reinstalls a token when either changes. An existing root is never executed until
its recorded source matches the newly verified source. Runtime roots retain the source image's
filesystem modes; the shim keeps the shared lower layer and Java-home bind mount
read-only for workloads.

When AppCDS regeneration is enabled, the provisioner atomically creates the
root-owned, mode `0444`
`/opt/brewlet/policy/appcds-regeneration-enabled` sentinel before publishing
`brewlet.sh/appcds-regeneration=true`. Disabling the policy, cleanup, or any
provisioning failure removes both. The node label is a scheduling hint; the shim
checks the sentinel authoritatively.

Mirror destinations are approved outside `NodeProfile` by the manager/admission
`--allowed-source-mirror-hosts` setting and passed into each provisioner pod.
Destination hosts, including explicit ports, must match exactly. Schemes,
whitespace, empty or malformed hosts, duplicate mappings, self-mappings, and
unapproved destinations are rejected. A permitted rewrite retains the original
`@sha256:` suffix; the mirror must preserve that manifest/index digest.

## Containerd configuration

The provisioner first queries the host server through `ctr version` and rejects
containerd 1.x. Brewlet's immutable image identity requires protected CRI
requested-image metadata that older servers discard. This server-version gate
is independent of the TOML config format: containerd 2 remains supported with
either config `version = 2` or `version = 3`.

The default `validated` mode checks whether the host's primary containerd
configuration imports `/etc/containerd/config.toml.d/*.toml`. When it does,
Brewlet writes only `99-brewlet.toml`; otherwise it appends the same runtime
block to the primary configuration and preserves the original as
`config.toml.brewlet.bak`.

Before activation, the provisioner runs
`containerd --config /etc/containerd/config.toml config dump` in the host mount
namespace. Provisioning fails unless the configuration parses and the dumped
effective configuration contains the `brewlet` runtime handler. A failed render
is removed or restored before exit, the node remains unready, and
`brewlet.sh/provision-error` reports either a rejected config dump or a missing
handler.

In the default `validated` restart mode, a changed containerd configuration is
activated with `systemctl restart containerd` from the host PID namespace. The
provisioner then checks the containerd socket and queries the live CRI status to
verify that the `brewlet` runtime handler is registered. A restart or
health-check failure restores the primary configuration backup (or removes the
renderer's drop-in), restarts containerd, verifies ordinary containerd recovery,
leaves the node unready, and sets an actionable
`brewlet.sh/provision-error`. A recovery failure is reported separately as
`rollback-failed`.

Re-running an unchanged valid render still verifies the effective configuration
and health-checks containerd without an unnecessary restart. `sighup` retains
the legacy in-place render and reload behavior without the config-dump gate,
while `none` is the immutable-image mode and does not mutate or signal
containerd.

When Helm runtime metrics are enabled, the profile-managed DaemonSet includes a
best-effort exporter sidecar that serves `/metrics` and listens for shim
telemetry on `/opt/brewlet/metrics/telemetry.sock`. It also reads the installed
JDK and launcher roots directly, including each JDK's exact `release` metadata,
source image, and node installation timestamp. The sidecar mounts `/opt/brewlet`
read-only and receives a separate read-write mount of `/opt/brewlet/metrics` for
the socket alone. The sidecar is disabled by default.

Copy-from-image commands run through the bundled `ctr` client in the host mount
namespace. This is required because the provisioner connects to the host
containerd socket and unpack mounts must be visible in the node's namespace.
Launcher extraction uses `ctr image pull` + `ctr images mount` and a host-side
`install`; it does not execute the source image, grant host networking, or
expose a writable host bind mount.

## Readiness validation

The provisioner container becomes Ready only after its script has finished
successfully. Both provisioning and cleanup publish `/tmp/brewlet-complete`
inside the container; the DaemonSet's exec readiness probe checks that file.
Every entrypoint invocation clears stale completion state first. A running or
sleeping process alone is not evidence of completion, and failed work never
publishes the marker. There is no startup/liveness timeout for long-running
installation work.

During valid NodeProfile deletion, the operator stops the provisioner and waits for its
pods to terminate before starting host cleanup. The cleanup container publishes
the same completion signal only after reversal has finished. The operator records
that completion, then waits for the cleanup DaemonSet and its pods to terminate
before releasing the profile finalizer.

With `BREWLET_VALIDATE=true`, the provisioner validates every configured
runtime component before it advertises runtime or capability labels:

- each JDK root runs `bin/java -version` inside its installed root;
- each staged launcher must be present as an executable regular file.

Arbitrary launcher binaries are not executed because Brewlet cannot assume a
universal safe probe. A missing/non-executable launcher or failed JDK probe
leaves the node unready and records a bounded component-specific reason in
`brewlet.sh/provision-error`. No JDK or launcher capability labels are retained
after failure. `BREWLET_VALIDATE=false` skips these checks and preserves the
opt-out behavior. It does not disable the completion-based readiness gate.

> The provisioner is privileged and host-mutating. Run it only on nodes
> controlled by the platform team.
