# brewlet Helm chart

Brewlet's single-install activation combines roles split across SpinKube's
Runtime Class Manager and Spin Operator. `helm install` deploys:

- **brewlet-operator** — the node lifecycle controller (§8.1). It creates and
  reconciles one `brewlet-node-provisioner-<profile>` DaemonSet per
  `NodeProfile` plus the shared `brewlet` RuntimeClass. Every JDK and launcher
  source is declared explicitly in its profile and pinned to an OCI digest.
- **node-provisioner RBAC** — the `ServiceAccount` + `ClusterRole` the DaemonSet
  the operator creates runs as (it labels/annotates the nodes it provisions).
- **brewlet-admission** — the pod admission/scheduling webhook (§8/§14): it
  stamps `brewlet.sh/artifact-ref` + `brewlet.sh/artifact-digest` onto brewlet
  pods, matches a pod's requested JDK/launcher against the ready fleet
  (`NoCompatibleJDK` / `NoCompatibleLauncher` / `NoCompatibleArch`), and injects nodeAffinity so the
  scheduler only lands pods on capable nodes. Its NodeProfile endpoint rejects
  mutable sources and unauthorized mirror destinations; the operator repeats
  that policy during reconciliation. Can be disabled with
  `--set admission.enabled=false`.

The provisioner DaemonSet and RuntimeClass themselves are **not** templated here
— the operator owns them at runtime (that is what §8.1 is for). The chart only
lays down the operator, the RBAC the operator's managed objects reference, and
the webhook.

## Install

Brewlet is a pre-1.0 preview. Use a disposable evaluation cluster with
containerd 2.0+ and cgroup v2. The published chart and component images are
intended for anonymous pulls. If GHCR denies access, see
[package access troubleshooting](../../../docs/installation.md#package-access-troubleshooting).
For a source-build path, see
[Installation](../../../docs/installation.md#released-components).

Save `my-jdks.yaml` with an administrator-chosen runtime source:

```yaml
provisioner:
  jdks:
    - distribution: temurin
      feature: 21
      source:
        image: docker.io/library/eclipse-temurin@sha256:<64-lowercase-hex>
        javaHome: /opt/java/openjdk
```

Replace the placeholder with the full digest of an approved image and verify
its Java feature, architectures, and JDK root. This is a template, not an approved
runtime catalog. Name the node pool the privileged provisioner may modify:

```bash
helm upgrade --install brewlet oci://ghcr.io/microsoft/charts/brewlet \
  --namespace brewlet \
  --create-namespace \
  --set provisioner.pools="{javaworkers}" \
  --values my-jdks.yaml

# The default NodeProfile provisions the named pools (§5.6) — no per-node opt-in
# step. The operator provisions each node; the provisioner marks it ready.
kubectl get nodes -L brewlet.sh/runtime
```

With `--version` omitted, Helm installs the latest chart. Component images
remain pinned to the immutable digests recorded in that chart. To reproduce a
specific release, add `--version x.y.z` with your chosen release number. For
existing installations, follow the [update guide](../../../docs/installation.md#upgrading):
pre-GA release changes default to safe teardown/reinstallation. In-place support
requires an explicit source/target release decision, not merely compatible CRDs.

> Provisioning is privileged and mutates the host. See the
> [Brewlet specification](../../../specs/SPECIFICATION.md).
> There is no every-node install: the chart fails to render until
> `provisioner.pools` names the pools the provisioner may mutate **and**
> `provisioner.jdks` declares at least one approved digest-pinned source, or
> `defaultProfile.enabled=false` hands profile authorship to you (§5.6).
> Control-plane nodes are excluded by default by node affinity whether or not they are
> tainted, and the provisioner tolerates only what a profile declares. On a
> single-node kind or Docker Desktop cluster, whose one node is labelled as the
> control plane, add `--set provisioner.includeControlPlane=true`.

On bare metal or kubeadm, also set `provisioner.poolKey` to the node label
carrying your pool name. Keep `my-jdks.yaml` and the chosen pool configuration
for upgrades; pass them again rather than substituting example defaults.

## Values

| Key | Default | Meaning |
|-----|---------|---------|
| `namespace` | `brewlet` | Component namespace, created by the chart and retained on uninstall. May differ from the Helm release namespace. |
| `images.registry` | `ghcr.io/microsoft` | Registry prefix used for generated component image references. |
| `images.tag` | chart `appVersion` | Shared component tag. |
| `images.operator` | generated | Explicit operator image override. |
| `images.provisioner` | generated | Explicit provisioner image override. |
| `images.admission` | generated | Explicit admission image override. |
| `images.digests.operator` | recorded at release | Immutable `sha256:<64 hex>` digest rendering `<registry>/brewlet-operator@<digest>`; takes precedence over `images.tag`. |
| `images.digests.provisioner` | recorded at release | Immutable digest for the node-provisioner image. |
| `images.digests.admission` | recorded at release | Immutable digest for the admission image. |
| `images.pullPolicy` | `IfNotPresent` | Image pull policy for all components. |
| `security.allowedSourceMirrorHosts` | `[]` | Exact destination registry hosts, including explicit ports, approved for JDK/launcher mirror rewrites. Empty disables mirrors. |
| `provisioner.pools` | `[]` (**required**) | Node pool names the default profile may provision. Rendering fails when `defaultProfile.enabled` is true and this is empty — the privileged provisioner is never cluster-wide by default. |
| `provisioner.poolKey` | `""` | Node label carrying the pool name. Empty auto-detects the well-known provider keys. |
| `provisioner.includeControlPlane` | `false` | Allow the default profile onto control-plane nodes. Needed only on single-node clusters such as kind. |
| `provisioner.tolerations` | `[]` | Tolerations for the default profile's provisioner pods. Each entry must name a `key`; nothing is tolerated implicitly. |
| `provisioner.jdks` | `[]` (**required**) | Administrator-chosen JDK entries with `distribution`, `feature`, digest-pinned `source.image`, and absolute `source.javaHome` (§5.3). No built-in runtime catalog. |
| `provisioner.launchers` | `[]` | Optional entries with `name`, digest-pinned `source.image`, and absolute `source.path` (§5.4). Empty = vanilla `java` only. |
| `provisioner.registry.mirrors` | `{}` | Upstream host → approved mirror host/path map. Rewrites preserve the source digest. |
| `provisioner.appCDS.regenerationEnabled` | `false` | Authorize node-side AppCDS regeneration for the default profile. |
| `stageGC.enabled` | `true` | Automatically reclaim orphaned runnable-image stages on managed nodes, independently of metrics. |
| `stageGC.interval` | `5m` | Time between sweeps, plus up to 10% jitter. |
| `stageGC.minAge` | `24h` | Minimum stage directory age, not time since last use or loss of references. |
| `stageGC.allowNestedPIDNamespace` | `false` | Test-only. Let the reaper run on nodes that are themselves containers with a private PID namespace (kind). Never enable on real nodes. |
| `operator.leaderElect` | `true` | Enable operator leader election. |
| `operator.nodeSelector` | `{}` | Node labels the operator pod must match. |
| `profiles` | `[]` | Additional profiles, each with a unique name, nonempty named `pools`, and explicit JDK sources. |
| `uninstall.timeoutSeconds` | `240` | Cleanup coordinator timeout, 1-86400 whole seconds. Configure before uninstall; Helm's `--timeout` must exceed this plus 30 seconds. |
| `uninstall.imagePullSecrets` | `[]` | Registry Secret references for the cleanup Job's dedicated service account. |
| `uninstall.nodeSelector` | `{}` | Extra node labels for the cleanup Job pod; `kubernetes.io/os: linux` is always set. |
| `metrics.enabled` | `false` | Enable control-plane metrics listeners and the node exporter, and expose scrape Services/ports. |
| `metrics.nodePort` | `9090` | Port served by the exporter in each provisioner pod. |
| `metrics.serviceMonitor.enabled` | `false` | Create a Prometheus Operator `ServiceMonitor`. |
| `metrics.grafanaDashboard.enabled` | `false` | Create the starter Grafana dashboard ConfigMap. |
| `networkPolicy.enabled` | `false` | Create ingress NetworkPolicies for admission and enabled metrics endpoints. |
| `networkPolicy.healthProbes.ingressFrom` | `[]` | NetworkPolicy peers used by kubelets to reach control-plane health endpoints on port 8081. |
| `networkPolicy.admission.apiServerCIDRs` | `[]` | API-server source CIDRs permitted to call the admission webhook when NetworkPolicies are enabled. |
| `networkPolicy.admission.ingressFrom` | `[]` | Additional NetworkPolicy peers permitted to call the admission webhook, such as API-server proxy pods that CIDR rules do not match. At least one entry in it or `apiServerCIDRs` is required. |
| `networkPolicy.metrics.ingressFrom` | `[]` | NetworkPolicy peers permitted to scrape enabled metrics endpoints. |
| `admission.enabled` | `true` | Deploy the admission/scheduling webhook. |
| `admission.failurePolicy` | `Ignore` | Webhook failure policy — `Ignore` never blocks workloads on a webhook outage. |
| `admission.nodeSelector` | `{}` | Node labels the admission webhook pods must match. |
| `admission.nodeProfileFailurePolicy` | `Ignore` | Transport-error policy for NodeProfile validation. The reconciler independently enforces the same rules before privileged work. |
| `admission.port` | `9443` | Webhook server port. |
| `admission.selfSigned.validityDays` | `90` | Lifetime of the dependency-free Helm-generated serving certificate. |
| `admission.certManager.enabled` | `false` | Let cert-manager issue and renew the serving certificate and inject webhook CA bundles. |
| `admission.certManager.createSelfSignedIssuer` | `false` | Create a namespaced self-signed CA chain instead of using an existing issuer. |
| `admission.certManager.issuerRef` | empty `Issuer` reference | Existing `Issuer` or `ClusterIssuer` used when chart-managed issuance is disabled. |
| `admission.certManager.duration` | `2160h` | Requested serving certificate lifetime. |
| `admission.certManager.renewBefore` | `720h` | How early cert-manager renews the serving certificate. |
| `admission.certManager.caRenewBefore` | `2160h` | How early a chart-managed CA renews; must exceed the serving certificate renewal interval. |

### Automatic stage cleanup

Stage GC settings are operator-wide defaults for **all** managed NodeProfiles,
including profiles created outside Helm. Fresh nodes activate after successful
provisioning; no extra DaemonSet, CronJob, kubelet access, or RBAC is needed.
The provisioner cleans only eligible unreferenced, unmounted stages; kubelet and
containerd remain responsible for image retention and content reclamation.
Durations must be positive whole seconds (Go duration syntax, at most
`2147483647s`); invalid settings are rejected by the operator.

Existing nodes without a matching installation safety record log `stage GC blocked`
and continue serving workloads. The record binds both installed shim copies and
the stage path; it is not a cross-release compatibility promise.
There is no acknowledgment override.
Recover through safe teardown/reinstallation, retiring unguarded shims and
stage-dependent bundles and reviewing retained host state before reinstalling.
Installing new shims or renaming stage roots does not prove consumers have stopped.
To opt out, set `stageGC.enabled=false`.

See [runnable stage cleanup](../../../docs/runnable-image.md#reclaiming-unused-stages)
for safety constraints, recovery guidance, and observability. These settings do
not copy or alter kubelet image GC thresholds, and reclamation may lag
containerd content GC.

All JDK distributions use the structured inventory form:

```yaml
provisioner:
  appCDS:
    regenerationEnabled: false
  jdks:
    - distribution: zulu
      feature: 21
      source:
        image: docker.io/library/azul-zulu@sha256:<64-lowercase-hex>
        javaHome: /usr/lib/jvm/zulu21
```

String inventories are not supported. Every source must be a fully qualified,
tagless `repository@sha256:<64 lowercase hex>` reference. An image may contain a
full JDK or a centrally built jlink runtime, but must include the userland
libraries required by `javaHome/bin/java`.

Optional launchers use the same explicit form:

```yaml
provisioner:
  launchers:
    - name: jaz
      source:
        image: mcr.microsoft.com/openjdk/jdk@sha256:<64-lowercase-hex>
        path: /usr/bin/jaz
```

For an air-gapped mirror, approve its exact registry host outside the profile:

```yaml
security:
  allowedSourceMirrorHosts:
    - registry.internal.example.com
provisioner:
  registry:
    mirrors:
      docker.io: registry.internal.example.com/dockerhub
      mcr.microsoft.com: registry.internal.example.com/mcr
```

The admission webhook, reconciler, and privileged provisioner all enforce the
allowlist. Schemes, whitespace, duplicate/self mappings, and unapproved
destinations fail closed. The mirror must preserve the original OCI
manifest/index digest.

AppCDS regeneration is denied by default. Enable it only on profiles whose
nodes are authorized to maintain namespace-partitioned cache entries:

```yaml
defaultProfile:
  enabled: false
profiles:
  - name: appcds-builders
    pools: ["cdsbuilders"]
    jdks:
      - distribution: temurin
        feature: 21
        source:
          image: docker.io/library/eclipse-temurin@sha256:<64-lowercase-hex>
          javaHome: /opt/java/openjdk
    appCDS:
      regenerationEnabled: true
```

The provisioner creates the root-owned
`/opt/brewlet/policy/appcds-regeneration-enabled` authorization sentinel and
advertises `brewlet.sh/appcds-regeneration=true`. The webhook requires that
label when `brewlet.sh/cds-regenerate: "true"` is requested; the shim's sentinel
check remains authoritative.

> **Release updates:** A fresh Helm install creates the chart's CRDs when absent.
> Helm does not upgrade existing CRDs. Follow the
> [teardown/reinstallation procedure](../../../docs/installation.md#upgrading)
> before replacing a release. Preserve reviewed pools, runtime sources, component
> choices, and ownership evidence. If cleanup is blocked, recover with the
> installed release's components rather than removing finalizers or adopting
> unverified hosts.

## Uninstall

Drain Brewlet workloads and pause profile/GitOps writers first. The pre-delete
Job uses the operator image to delete only profiles owned by this exact Helm
release and waits for finalizer-driven host cleanup and worker teardown.
Manually managed or other-release profiles block uninstall rather than being
silently orphaned. Failures/timeouts keep the operator and RBAC available;
repair the cause and retry without bypassing hooks or finalizers.

```bash
helm uninstall brewlet --namespace brewlet --timeout 5m
```

The namespace and CRDs are retained; the shared RuntimeClass is operator-created,
not chart-owned. See [installation and recovery guidance](../../../docs/installation.md#uninstall)
for retained-resource review and manually managed profiles.

## Requesting a JDK / launcher

A pod (or the raw Deployment) opts into a specific JDK/launcher via annotations,
which the webhook validates and turns into scheduling constraints:

```yaml
metadata:
  annotations:
    brewlet.sh/jdk: "21"          # bare feature (any distribution) or "temurin-21"
    brewlet.sh/launcher: "jaz"    # optional; omit/"java" for the vanilla launcher
spec:
  runtimeClassName: brewlet
  containers:
    - image: registry.example.com/demo/hello@sha256:<64-lowercase-hex>
```

Replace the workload digest with the one reported by your image publication.
It is not a runnable example until you supply that digest.

If no ready node provides a compatible JDK/launcher or authorizes requested
AppCDS regeneration, the pod is rejected with `NoCompatibleJDK`,
`NoCompatibleLauncher`, or `AppCDSRegenerationDisabled`, surfaced on the owning
controller (§14). With no annotation, the pod is admitted (ref/digest still
stamped) and the shim keeps its runtime compatibility check.

## Runtime metrics

Runtime metrics are disabled by default. Set `metrics.enabled=true` to run the
node exporter, enable the control-plane metrics listeners, and expose three
scrape surfaces:

Scraping the operator or admission controller-runtime endpoint requires
`metrics.enabled=true`; preserve that explicit choice when preparing target
release values or maintaining the installed release.

- `brewlet-node-metrics` discovers the exporter in every profile-managed
  provisioner pod and reports sandbox launch phases/outcomes, artifact resolution,
  AppCDS decisions, and JDK/launcher inventory.
- `brewlet-operator-metrics` exposes controller-runtime metrics plus Brewlet
  NodeProfile readiness and provisioning transitions.
- `brewlet-admission` exposes webhook metrics on its `metrics` Service port,
  including admitted, denied, errored, and fail-open outcomes.

Enable optional integrations with:

```yaml
metrics:
  enabled: true
  serviceMonitor:
    enabled: true
    interval: 30s
  grafanaDashboard:
    enabled: true
```

The node exporter intentionally reports exact JDK build/source and installation
time rather than claiming the node installation age is the upstream patch age.
It also reports artifact resolution, not containerd pull-cache hits, because the
pull occurs before the shim is invoked.

## Serving certificate

The simple default uses Helm `genSignedCert` to create a 90-day self-signed
serving certificate. The CA is injected into both webhook configurations, and a
checksum annotation rolls the webhook pods whenever `helm upgrade` generates a
new certificate. Change the lifetime with
`admission.selfSigned.validityDays`; this mode does not renew between Helm
operations. It is intended for evaluation and simple installations. A
long-running installation must enable cert-manager or complete a Helm upgrade
before the generated certificate expires.

For automatic issuance and renewal, install cert-manager with its CA injector and
enable the integration. A chart-managed self-signed CA chain is the smallest
configuration:

```yaml
admission:
  certManager:
    enabled: true
    createSelfSignedIssuer: true
    duration: 2160h
    renewBefore: 720h
```

For a production PKI, reference an existing namespaced `Issuer` or
cluster-scoped `ClusterIssuer` instead:

```yaml
admission:
  certManager:
    enabled: true
    issuerRef:
      name: platform-ca
      kind: ClusterIssuer
      group: cert-manager.io
```

In cert-manager mode the chart renders a `Certificate`, does not generate the TLS
Secret, and annotates both webhook configurations with
`cert-manager.io/inject-ca-from`. cert-manager owns the
`brewlet-admission-cert` Secret and CA-bundle rotation. The admission process
watches the mounted keypair and reloads it without a pod restart. The default
Helm-managed keypair uses the distinct
`brewlet-admission-selfsigned-cert` Secret so switching modes or rolling back
cannot cause resource-ownership conflicts.

`admission.nodeProfileFailurePolicy` defaults to `Ignore` so initial profile
creation cannot race certificate issuance. This does not authorize privileged
work: the operator repeats all NodeProfile source, mirror, pool, and rollout
validation before creating a DaemonSet. Set the value to `Fail` after webhook
availability is guaranteed when synchronous rejection on transport errors is
preferred.

## NetworkPolicies

NetworkPolicies are disabled by default because the API-server source addresses
and Prometheus identity are cluster-specific. When enabled, the chart requires
the API-server sources needed by admission (`apiServerCIDRs` and/or
`ingressFrom` peers) and, when metrics are enabled, explicit metrics peers:

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
    apiServerCIDRs:
      - 10.0.0.0/24
  metrics:
    ingressFrom:
      - namespaceSelector:
          matchLabels:
            kubernetes.io/metadata.name: monitoring
        podSelector:
          matchLabels:
            app.kubernetes.io/name: prometheus
```

The policies select the admission and operator pods plus every
`app=brewlet-node-provisioner` pod created dynamically by the operator. They
allow the webhook target port, control-plane health port 8081 from configured
kubelet peers, and enabled metrics ports; egress is unchanged. Confirm both the
API-server and kubelet source addresses used by your managed Kubernetes service
and CNI before enabling the policies. Incorrect peers can block webhook calls or
make healthy pods fail their probes.

Some managed services reach webhooks through in-cluster proxy pods rather than
from a host address. On AKS the API server dials pods through
`konnectivity-agent` pods in `kube-system`, and CNIs such as Azure CNI powered
by Cilium never match in-cluster pods with `ipBlock` rules. Allow those pods
with a selector peer:

```yaml
networkPolicy:
  admission:
    ingressFrom:
      - namespaceSelector:
          matchLabels:
            kubernetes.io/metadata.name: kube-system
        podSelector:
          matchLabels:
            app: konnectivity-agent
```
