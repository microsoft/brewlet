# Configuration

Every knob Brewlet exposes, in one place: Helm chart values, node-provisioner
environment variables, operator/admission flags, the `RuntimeClass`, and how the
different layers relate. For *how* to install, see [Installation](installation.md);
for JDKs and launchers specifically, see [JDK management](jdk-management.md) and
[Launchers](launchers.md). For how the resulting node labels drive scheduling
and autoscaling, see
[Capability labels and autoscaling](capability-labels-and-autoscaling.md).
For the operational workflow behind the metrics values, see
[Runtime metrics and Grafana dashboards](runtime-metrics.md).

---

## Configuration layers (how they connect)

There is **one source of truth** for the JDK/launcher inventory: each
`NodeProfile` contains complete, immutable source declarations.

```
Helm values (provisioner.jdks / .launchers + source-mirror allowlist)
        │  render NodeProfiles and operator/admission flags
        ▼
Admission + reconciliation validate each digest, path, name, and mirror
        │  render indexed source variables into a DaemonSet
        ▼
Provisioner env (JDK_SOURCE_* / LAUNCHER_SOURCE_* / mirror policy)
        │  derives JDKS and LAUNCHERS, then copies approved image content
        ▼
Node state: /opt/brewlet/jdks/<dist>-<feature>/ + capability labels/annotations
```

If you use Helm, set values. If you run the operator directly, set flags. If you
hand-wire the DaemonSet, set env vars. Don't mix — the operator overwrites the
DaemonSet it manages.

> **Note on examples.** The Helm chart is installable with editable,
> digest-pinned Temurin 21, Microsoft JDK 25, and `jaz` entries. They are ordinary
> values, not a Brewlet-maintained catalog. The manager binary has no runtime
> inventory defaults; outside Helm you must create a `NodeProfile`.

---

## Helm chart values

From [`charts/brewlet/values.yaml`](https://github.com/microsoft/brewlet/blob/main/kubernetes/charts/brewlet/values.yaml). Override
with `--set key=value` or a values file.

| Key | Default | Meaning |
|---|---|---|
| `namespace` | `brewlet` | Namespace all components install into (created by the chart). |
| `images.registry` | `ghcr.io/microsoft` | Registry prefix used to generate component image references. |
| `images.tag` | chart `appVersion` | Shared component tag. A versioned OCI chart therefore selects matching images automatically. |
| `images.operator` | generated | Explicit operator image override; supports tags or digests. |
| `images.provisioner` | generated | Explicit provisioner image override; supports tags or digests. |
| `images.admission` | generated | Explicit admission webhook image override; supports tags or digests. |
| `images.pullPolicy` | `IfNotPresent` | Image pull policy for all components. |
| `security.allowedSourceMirrorHosts` | `[]` | Exact registry destination hosts, including explicit ports, that NodeProfiles may use for JDK/launcher mirrors. Empty disables mirrors. The chart passes the same list to manager and admission. |
| `provisioner.jdks` | structured Temurin 21 and Microsoft 25 examples | Required JDK entries with `distribution`, `feature`, digest-pinned `source.image`, and absolute `source.javaHome` ([§JDK management](jdk-management.md#source-model)). |
| `provisioner.launchers` | structured `jaz` example | Optional launcher entries with `name`, digest-pinned `source.image`, and absolute `source.path` ([§Launchers](launchers.md#helm-example-jaz)). Empty = vanilla `java` only. |
| `provisioner.appCDS.regenerationEnabled` | `false` | Authorize node-side AppCDS regeneration for the chart-managed default `NodeProfile`. |
| `provisioner.rollout.maxUnavailable` | `null` | Bounds the default profile's provisioner DaemonSet rolling update. `null` keeps the DaemonSet default. |
| `provisioner.rollout.validate` | `true` | Gate node readiness on post-install JDK smoke tests and staged-launcher executable checks. Arbitrary launchers are not executed as a probe. Renders the provisioner `BREWLET_VALIDATE` env. |
| `provisioner.rollout.containerdRestart` | `validated` | Select containerd activation: transactional config validation, service restart, live health checks, and rollback (`validated`); legacy in-place render plus SIGHUP (`sighup`); or no containerd mutation/signal (`none`). Renders `BREWLET_CONTAINERD_RESTART` ([§5.5](https://github.com/microsoft/brewlet/blob/main/specs/SPECIFICATION.md)). |
| `provisioner.registry.mirrors` | `{}` | `<upstream-host>: <mirror-host[/path]>` map applied to digest-pinned source pulls. Every destination host must appear in `security.allowedSourceMirrorHosts`. |
| `defaultProfile.enabled` | `true` | Render the chart-managed **default** `NodeProfile` from `provisioner.*`. Disable to manage the default profile yourself, e.g. via GitOps ([§5.6](https://github.com/microsoft/brewlet/blob/main/specs/SPECIFICATION.md)). |
| `profiles` | `[]` | Additional per-pool `NodeProfile` CRs, each binding node pool(s) to their own JDK/launcher inventory plus AppCDS, rollout, and registry policy ([§5.6](https://github.com/microsoft/brewlet/blob/main/specs/SPECIFICATION.md)). |
| `operator.replicas` | `1` | Operator replica count. |
| `operator.leaderElect` | `true` | Enable leader election for HA. |
| `operator.resources` | requests `50m/64Mi`, limits `200m/128Mi` | Operator pod resources. |
| `metrics.enabled` | `false` | Opt in to Brewlet runtime and control-plane Prometheus endpoints. Enables the operator and admission listeners plus the node exporter sidecar and Services. |
| `metrics.nodePort` | `9090` | Port exposed by each node-local metrics exporter and the `brewlet-node-metrics` headless Service. |
| `metrics.serviceMonitor.enabled` | `false` | Create a Prometheus Operator `ServiceMonitor`. Requires the `monitoring.coreos.com/v1` CRDs. |
| `metrics.serviceMonitor.interval` | `30s` | Scrape interval rendered into the optional `ServiceMonitor`. |
| `metrics.serviceMonitor.additionalLabels` | `{}` | Extra `ServiceMonitor` labels, commonly used to match a Prometheus `serviceMonitorSelector`. |
| `metrics.grafanaDashboard.enabled` | `false` | Create the `brewlet-grafana-dashboard` ConfigMap for dashboard sidecar discovery; does not install Grafana or a data source. |
| `metrics.grafanaDashboard.labels` | `grafana_dashboard: "1"` | Labels applied to the dashboard ConfigMap for sidecar discovery. |
| `admission.enabled` | `true` | Deploy the admission/scheduling webhook. Set `false` to skip it; the shim still enforces runtime image identity and JDK compatibility. |
| `admission.replicas` | `1` | Webhook replica count. |
| `admission.failurePolicy` | `Ignore` | Webhook failure policy. `Ignore` keeps availability high, but runtime identity resolution still fails closed in the shim if containerd metadata cannot be resolved. |
| `admission.port` | `9443` | Webhook server port. |
| `admission.resources` | requests `50m/64Mi`, limits `200m/128Mi` | Webhook pod resources. |

Example production values (own runtime source, no optional launcher):

```yaml
# values-production.yaml
images:
  operator: registry.example.com/brewlet/operator@sha256:<digest>
  provisioner: registry.example.com/brewlet/node-provisioner@sha256:<digest>
  admission: registry.example.com/brewlet/admission@sha256:<digest>
provisioner:
  jdks:
    - distribution: platform
      feature: 21
      source:
        image: registry.example.com/java/runtime@sha256:<digest>
        javaHome: /opt/java/runtime
  launchers: []
```

```bash
helm upgrade --install brewlet oci://ghcr.io/microsoft/charts/brewlet \
  --version 0.3.1 -f values-production.yaml
```

> JDKs and launchers are always obtained **copy-from-image** from explicit,
> tagless SHA-256 digest references. For air-gapped clusters, mirror the exact
> digest and approve the destination host explicitly.

AppCDS regeneration is default-deny. Enable it for the default profile with
`--set provisioner.appCDS.regenerationEnabled=true`, or on a named profile with:

```yaml
profiles:
  - name: appcds-builders
    pools: ["appcds-builders"]
    jdks:
      - distribution: temurin
        feature: 21
        source:
          image: docker.io/library/eclipse-temurin@sha256:85f00967bcc624fc19fa9c2cf124ea426a5363898e267141726f31f358c2e14b
          javaHome: /opt/java/openjdk
    appCDS:
      regenerationEnabled: true
```

---

## Operator flags

The source for these flags lives in
[`cmd/manager`](https://github.com/microsoft/brewlet/tree/main/kubernetes/cmd/manager).
When you install via Helm, the chart populates them for you.

| Flag | Default | Meaning |
|---|---|---|
| `--namespace` | `brewlet` | Namespace the provisioner DaemonSet is managed in. |
| `--provisioner-image` | `ghcr.io/microsoft/brewlet-node-provisioner:0.3.1` | Image the DaemonSet runs. |
| `--allowed-source-mirror-hosts` | *(empty)* | Comma-separated exact destination registry hosts approved for NodeProfile runtime-source rewrites. Configure the same value on manager and admission; empty disables mirrors. |
| `--leader-elect` | `false` | Enable leader election for HA. |
| `--metrics-bind-address` | `0` | Metrics endpoint; `0` disables it. |
| `--health-probe-bind-address` | `:8081` | Health/readiness endpoint. |
| `--node-metrics-enabled` | `false` | Run the node-local exporter sidecar in managed provisioner pods. |
| `--node-metrics-port` | `9090` | Exporter port when node metrics are enabled. |

```bash
./bin/operator --namespace=brewlet \
  --provisioner-image=ghcr.io/microsoft/brewlet-node-provisioner:0.3.1 \
  --allowed-source-mirror-hosts=registry.internal.example.com
kubectl apply -f nodeprofile.yaml
```

---

## Node-provisioner environment variables

The provisioner environment contract lives in the core runtime's
[`provisioner/README.md`](https://github.com/microsoft/brewlet/blob/main/provisioner/README.md).
The Kubernetes operator sets these variables on the DaemonSet it manages; you
only touch them directly if you hand-wire the DaemonSet.

| Env var | Default | Meaning |
|---|---|---|
| `JDK_SOURCE_COUNT` | required | Positive number of indexed JDK source entries. |
| `JDK_SOURCE_<n>_TOKEN` | required | Safe `<distribution>-<feature>` inventory token. |
| `JDK_SOURCE_<n>_IMAGE` | required | Fully qualified, tagless `repository@sha256:<64 lowercase hex>` source image. |
| `JDK_SOURCE_<n>_JAVA_HOME` | required | Clean absolute Java-home path inside the image. |
| `LAUNCHER_SOURCE_COUNT` | `0` | Number of indexed optional launcher sources. |
| `LAUNCHER_SOURCE_<n>_NAME` | required per entry | Lowercase launcher name; `java` is reserved. |
| `LAUNCHER_SOURCE_<n>_IMAGE` | required per entry | Fully qualified, tagless SHA-256 digest source image. |
| `LAUNCHER_SOURCE_<n>_PATH` | required per entry | Clean absolute launcher path inside the image. |
| `JDKS` / `LAUNCHERS` | derived | Internal comma-separated inventories derived from the indexed entries. They are not accepted as independent inputs. |
| `BREWLET_APP_CDS_REGENERATION_ENABLED` | `false` | Internal operator-to-provisioner policy transport. When true, atomically creates the root-owned AppCDS authorization sentinel and publishes `brewlet.sh/appcds-regeneration=true`; when false or during cleanup/failure, removes both. Configure `spec.appCDS.regenerationEnabled`, not this variable directly. |
| `NODE_NAME` | (downward API) | The node to label; injected from `spec.nodeName`. |
| `BREWLET_PROFILE_UID` | *(empty)* | Operator-managed profile UID used to fence stale provisioners before readiness publication. |
| `BREWLET_PROFILE_GENERATION` | `0` | Operator-managed generation paired with `BREWLET_PROFILE_UID`. |
| `BREWLET_PREFIX` | `/opt/brewlet` | Host install prefix (`bin/`, `jdks/`, `launchers/`). |
| `CONTAINERD_CONFIG` | `/etc/containerd/config.toml` | Primary containerd configuration. Validated mode uses an imported drop-in when supported and otherwise patches this file with a backup. |
| `CONTAINERD_DROPIN_DIR` | `/etc/containerd/config.toml.d` | Drop-in directory used when the primary config imports `*.toml` from it. |
| `CONTAINERD_DROPIN_FILE` | `/etc/containerd/config.toml.d/99-brewlet.toml` | Brewlet-managed runtime drop-in. |
| `CONTAINERD_ADDRESS` | `/run/containerd/containerd.sock` | Host containerd socket (used for copy-from-image). |
| `CONTAINERD_NAMESPACE` | `k8s.io` | containerd namespace for image pulls. |
| `BREWLET_MODE` | `provision` | `provision` installs the runtime; `cleanup` reverses it (removes the Brewlet drop-in or restores the primary-config backup, removes the shim, and drops runtime/capability labels) for a deleted `NodeProfile`. The operator sets it on the short-lived `brewlet-cleanup-<profile>` DaemonSet (§5.6). |
| `BREWLET_CONTAINERD_RESTART` | `validated` | `validated` smoke-tests the runtime inventory, validates the effective config with `containerd config dump`, restarts the host service only when needed, checks containerd and the live `brewlet` handler, and restores known-good config on activation failure. `sighup` preserves the legacy in-place render/reload path without the config-dump gate. `none` neither mutates nor signals containerd. Rendered from `spec.rollout.containerdRestart`. |
| `BREWLET_VALIDATE` | `true` | Run `java -version` for every JDK and verify every staged launcher is executable before publishing runtime or capability labels. Arbitrary launchers are not executed. `false` skips both sets of checks. Rendered from `spec.rollout.validate`. |
| `MIRRORS` | *(empty)* | Strict comma-separated `<upstream-host>=<mirror-host[/path]>` pairs rendered from `spec.registry.mirrors`. Schemes, whitespace, empty entries, duplicates, self-mappings, and unapproved destinations fail closed. |
| `SOURCE_ALLOWED_MIRROR_HOSTS` | *(empty)* | Exact destination host allowlist rendered from the operator's `--allowed-source-mirror-hosts`; empty disables `MIRRORS`. |
| `SOURCE_POLICY_BIN` | `/opt/brewlet-dist/brewlet-source-policy` | Baked validator for digest references, source paths, registry hosts, and mirror targets. |

---

## Admission webhook

The [`brewlet-admission`](https://github.com/microsoft/brewlet/tree/main/kubernetes/cmd/admission/) webhook is
mutating+validating. For every pod on CREATE with `runtimeClassName: brewlet` it:

- **overwrites** `brewlet.sh/artifact-container`, `brewlet.sh/artifact-ref`, and
  `brewlet.sh/artifact-digest` (when digest-pinned) as compatibility hints from
  the selected Pod image; the shim resolves the JAR from containerd-owned
  metadata and the content store by digest, not from the hints;
- **matches** any requested JDK/launcher/AppCDS-regeneration combination against
  the ready fleet, denying with `NoCompatibleJDK`, `NoCompatibleLauncher`, or
  `AppCDSRegenerationDisabled`;
- **steers** scheduling via `nodeAffinity` onto per-capability node labels.

Its NodeProfile endpoint also rejects mutable source references and mirrors
outside `--allowed-source-mirror-hosts`. The `NodeProfileReconciler` repeats the
same checks before creating a privileged DaemonSet, so webhook disablement or
bypass does not weaken the source boundary.

Admission matches JDK and launcher capability keys with `Operator: Exists`.
The [Capability labels and autoscaling](capability-labels-and-autoscaling.md)
guide explains the end-to-end scheduling flow and autoscaler integration. The
[canonical capability-label contract](https://github.com/microsoft/brewlet/blob/main/specs/CAPABILITY_LABELS.md)
defines the complete key grammar and compatibility guarantees.

Non-brewlet pods pass through untouched. With `admission.failurePolicy: Ignore`
(default) a webhook outage never blocks workloads. For AppCDS regeneration this
is only an availability choice: the shim independently requires the root-owned
`/opt/brewlet/policy/appcds-regeneration-enabled` sentinel, so fail-open
admission cannot grant cache-write authority. The shim also fails closed rather
than guessing when it cannot resolve runtime image identity from containerd
metadata.

**Serving certificate.** By default Helm generates a 90-day self-signed serving
certificate and injects the CA as the `caBundle`. A `helm upgrade` regenerates
the Secret and CA bundle together and rolls the webhook pods, but this simple
mode does not renew certificates between Helm operations. Configure its lifetime
with `admission.selfSigned.validityDays`. Treat it as the simple evaluation path:
long-running installations must either enable cert-manager or schedule a Helm
upgrade before the certificate expires.

For automatic renewal, install cert-manager with its CA injector and enable:

```yaml
admission:
  certManager:
    enabled: true
    issuerRef:
      name: platform-ca
      kind: ClusterIssuer
      group: cert-manager.io
    duration: 2160h
    renewBefore: 720h
```

Set `createSelfSignedIssuer: true` instead of `issuerRef.name` when a
chart-managed namespaced self-signed CA chain is appropriate. In either
cert-manager mode, cert-manager creates and rotates the
`brewlet-admission-cert` Secret and injects the CA into both webhook
configurations. The admission process watches the mounted keypair and reloads
renewed certificates without a pod restart.

`admission.nodeProfileFailurePolicy` defaults to `Ignore` so certificate
bootstrap cannot block chart-created profiles. The operator still repeats every
NodeProfile validation before privileged reconciliation. Set it to `Fail` once
webhook availability is guaranteed if API transport failures must reject profile
writes synchronously.

**Network isolation.** The chart can add ingress NetworkPolicies for the
admission pod and all enabled metrics endpoints:

```yaml
metrics:
  enabled: true
networkPolicy:
  enabled: true
  healthProbes:
    ingressFrom:
      - ipBlock:
          cidr: 10.1.0.0/16
  admission:
    apiServerCIDRs: ["10.0.0.0/24"]
  metrics:
    ingressFrom:
      - namespaceSelector:
          matchLabels:
            kubernetes.io/metadata.name: monitoring
        podSelector:
          matchLabels:
            app.kubernetes.io/name: prometheus
```

The chart requires explicit kubelet health-probe peers, API-server CIDRs, and
metrics peers rather than guessing cluster-specific identities. Determine the
source CIDRs used by your nodes and control plane, then verify health probes and
webhook connectivity before rolling this setting into production.
NetworkPolicies require a CNI that enforces the Kubernetes `NetworkPolicy` API.

Pod-side annotations the webhook reads (developer-facing) — see
[Deploying workloads](deploying-workloads.md):

| Annotation | Example | Meaning |
|---|---|---|
| `brewlet.sh/jdk` | `21` or `temurin-21` | Request a specific JDK feature (any distro) or exact `<dist>-<feature>`. |
| `brewlet.sh/launcher` | `jaz` | Request a launcher. Empty / `java` = vanilla OpenJDK launcher. |
| `brewlet.sh/cds-regenerate` | `true` | Request node-side AppCDS regeneration. Requires a ready node whose `NodeProfile.spec.appCDS.regenerationEnabled` is true. |
| `brewlet.sh/artifact-container` | `app` | Selects which regular container's `image` the webhook mirrors into Pod-wide compatibility hints. The webhook normalizes this value to the selected container name; other tasks ignore the shared hints and remain bound to their own CRI images. |

---

## RuntimeClass

The operator manages the `brewlet` `RuntimeClass`; this is what it generates (mirrors
[`deploy/runtimeclass.yaml`](https://github.com/microsoft/brewlet/blob/main/kubernetes/deploy/runtimeclass.yaml)):

```yaml
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata:
  name: brewlet
handler: brewlet                 # matches the containerd runtime name
scheduling:
  nodeSelector:
    brewlet.sh/runtime: "ready"  # only land on provisioned nodes
overhead:
  podFixed:
    memory: "64Mi"               # JVM/runtime baseline overhead accounting
    cpu: "50m"
```

`overhead.podFixed` is how the scheduler and `LimitRange`/quotas account for the
JVM/runtime baseline. Adjust it if your JVMs have a materially different fixed
footprint.

---

## Precedence & defaults you should know

- **JVM launch flags:** artifact structured knobs carry app-intrinsic correctness
  flags; deployment-descriptor `jvm.args` carries heap/GC/agent tuning and comes
  after those knobs. The descriptor's `jvm.launcher` / `brewlet.sh/launcher`
  selects `java` or a node-installed launcher. Brewlet injects **no** `-XX` flags
  itself. See [Resource requests, limits & JVM tuning](resource-tuning.md).
- **JDK selection:** the deployment descriptor is authoritative:
  `spec.jvm.version` (plus optional `spec.jvm.distribution`) on `JavaApplication`,
  or `brewlet.sh/jdk` on raw pods, drives validation, scheduling, and shim launch
  selection. A bare feature matches any distribution; `<distribution>-<feature>`
  pins one.
- **JDK source selection:** every `spec.jdks[]` entry carries exactly one
  digest-pinned source and Java-home path. Distribution names never resolve to
  Brewlet-owned images.
- **Mirror selection:** a NodeProfile mapping is accepted only when its
  destination host exactly matches the external operator/admission allowlist.
- **cgroup v2 is mandatory** on nodes; the provisioner refuses cgroup v1-only nodes.
- **Digest-pinned artifact refs are required for Kubernetes execution**
  (`repo@sha256:…`). The shim resolves that exact target from the content store,
  verifies its platform-manifest config against CRI metadata, and rejects
  tag-only requests.

## Next steps

- **[JDK management](jdk-management.md)** — explicit source requirements,
  copy-from-image mechanics, and patching.
- **[Launchers](launchers.md)** — installing and choosing `jaz`.
- **[Capability labels and autoscaling](capability-labels-and-autoscaling.md)** —
  connect `NodeProfile` inventories to workload affinity and node-pool scaling.
- **[Deploying workloads](deploying-workloads.md)** — put these knobs to use.
- **[Runtime metrics and Grafana dashboards](runtime-metrics.md)** — enable,
  scrape, query, visualize, and troubleshoot Brewlet telemetry.
