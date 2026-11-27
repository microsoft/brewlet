# Configuration reference

Each layer of Brewlet configuration has one source of truth. Change the source,
not the resources Brewlet generates from it.

| Layer | Configures |
| --- | --- |
| Helm values | Control-plane options and optional `NodeProfile`s. |
| `RuntimeImage` | Approved runtime source, default user and compatibility metadata. |
| `NodeProfile` | Node pools, runtime offerings, installation policy, mirrors and worker rollout. |
| Pod or workload controller | Per-container runtime selections, application image, resources and rollout policy. |

Manage these objects through Helm or GitOps. Do not edit the worker DaemonSets
that the operator generates; the operator reconciles them.

## Helm values

This section lists every value the chart accepts. To print the defaults of a
specific release, run:

```sh
helm show values oci://ghcr.io/microsoft/charts/brewlet --version VERSION
```

The published chart pins each component image to the digest of the release.
The source chart defaults to matching image tags; `VERSION` below is the
chart's release version. The chart requires Kubernetes 1.30 or later.

### How values are validated

Values are checked at several stages. The **Checked by** column in each table
below says which stage rejects an invalid value.

| Stage | When it runs | What happens on an invalid value |
| --- | --- | --- |
| Schema | `helm install`, `upgrade`, `template` and `lint` | Helm refuses to render for incorrect types, unsupported enum values, invalid formats, out-of-range numbers, missing required values or unknown keys. |
| Template | Chart rendering | Helm fails with a message naming the value. |
| API server | Helm applies the rendered objects | The install or upgrade fails because Kubernetes rejects the object. |
| Operator | `brewlet-operator` startup | The operator exits and its Pod enters `CrashLoopBackOff`; the reason is in its logs. Without `--wait`, Helm can still report success, so check the Pod after changing these values. |
| Profile policy | `NodeProfile` admission, operator and node worker | The affected `NodeProfile`s are rejected or reported invalid and not provisioned. |

All chart values are declared in `values.schema.json`, and unknown keys are
rejected at every level. Some semantic constraints cannot be checked by the
schema: for example, durations must be positive, mirror hosts must be valid
registry authorities, and namespaces must exist. Their consuming operator,
profile policy or Kubernetes API checks them at runtime or apply time. Use
`helm template` to check the rendered result.

Durations are [Go duration](https://pkg.go.dev/time#ParseDuration) strings: one
or more decimal numbers, each followed by a unit of `ns`, `us` (also `µs` or
`μs`), `ms`, `s`, `m` or `h`, such as `90s`, `10m` or `1h30m`. Days (`d`) and
bare numbers such as `600` are not accepted. The schema checks the format;
operator and worker startup checks that durations are positive.

### Images

| Key | Type | Default | Valid values and constraints | Checked by |
| --- | --- | --- | --- | --- |
| `operator.image` | string | `ghcr.io/microsoft/brewlet-operator:VERSION` | Non-empty image reference by tag or `@sha256:` digest. Also runs the uninstall pre-delete Job. | Schema, kubelet pull |
| `admission.image` | string | `ghcr.io/microsoft/brewlet-admission:VERSION` | Non-empty image reference. Used only when `admission.enabled` is `true`. | Schema, kubelet pull |
| `provisioner.image` | string | `ghcr.io/microsoft/brewlet-node-provisioner:VERSION` | Non-empty image reference for the privileged node worker, including the shim. Workers pull it with `IfNotPresent`. | Schema, kubelet pull |
| `dashboard.image` | string | `ghcr.io/microsoft/brewlet-dashboard:VERSION` | Non-empty image reference. Used only when `dashboard.enabled` is `true`. | Schema, kubelet pull |

All images must come from the same Brewlet release as the chart. Override them
only to point at a mirror of the same release. The chart has no
`imagePullSecrets` value: nodes must be able to pull these images without
per-Pod credentials. Schema validation requires only a non-empty image string;
the kubelet validates the image reference when pulling it. Only the dashboard
has a pull-policy value; the operator and admission Pods use the Kubernetes
default for their image reference.

### Operator and rotation restarts

These values live under `operator.rotationRestarts`. All of them except
`nodeProgressDeadline` take effect only when `enabled` is `true`.

| Key | Type | Default | Valid values and constraints | Checked by |
| --- | --- | --- | --- | --- |
| `enabled` | boolean | `false` | `true` or `false`. Lets the operator restart workloads that set `brewlet.sh/restart-on-rotation: "true"` after a runtime rotation. | Schema |
| `maxConcurrent` | integer | `5` | Whole number of at least `1`. The cluster-wide maximum of workload restart rollouts in progress at once. | Schema, operator |
| `nodeProgressDeadline` | duration | `10m` | Positive duration. How long a node may fail to make rotation progress before the operator quarantines it. **Applies even when restarts are disabled.** | Schema, operator |
| `workloadProgressDeadline` | duration | `10m` | Positive duration. The longest the operator watches one workload's restart rollout. | Schema, operator |
| `namespaces` | list of strings | `[]` | Empty means every namespace except the protected ones. Otherwise each entry must be a DNS-1123 label (lowercase letters, digits and `-`, at most 63 characters, starting and ending with a letter or digit). List each namespace once. Entries must not be the release namespace or appear in `protectedNamespaces`. Each namespace must exist before you install or upgrade. | Schema, Template, operator, API server |
| `protectedNamespaces` | list of strings | `[kube-system, kube-public, kube-node-lease]` | Namespace names, matched exactly. Restarts are always refused in these namespaces, even when they are listed in `namespaces`. Add namespaces as needed; removing the defaults removes that protection. | Schema, Template |

Setting `enabled: true` installs, in addition to the operator flags:

- `patch` rights on Deployments, StatefulSets and DaemonSets. Without
  `namespaces`, these come from the operator's ClusterRole. With `namespaces`,
  the ClusterRole keeps only `get`, `list` and `watch`, and each listed namespace
  gets a Role and RoleBinding named `<release>-brewlet-rotation-restarts`.
- A Role and RoleBinding named `<release>-brewlet-restart-election` in
  `kube-system`, for the cluster-wide restart lease
  `brewlet-operator.node.brewlet.sh`.
- A `ValidatingAdmissionPolicy` and binding named
  `<release>-brewlet-rotation-restarts`, with `failurePolicy: Fail`. It lets the
  operator change only the runtime-generation barrier annotation on opted-in
  workloads, and enforces `namespaces` and `protectedNamespaces`.

Restarts still require leader election, which the chart always enables. A
workload must also opt in on its controller metadata; see
[Pod and controller settings](#pod-and-controller-settings).

### Provisioner

| Key | Type | Default | Valid values and constraints | Checked by |
| --- | --- | --- | --- | --- |
| `provisioner.gc.grace` | duration | `1h` | Positive duration. How long a node keeps a runtime generation that no Pod leases before collecting it. | Schema, operator, node worker |
| `provisioner.gc.sweepInterval` | duration | `1m` | Positive duration. How often node workers collect unused runtime generations and cached layers. | Schema, operator, node worker |
| `provisioner.profiles` | list of objects | `[]` | `NodeProfile`s for Helm to manage. Each entry has exactly `name` and `spec`, described below. | Schema, Template, API server, profile policy |
| `provisioner.profiles[].name` | string | — | A valid Kubernetes object name: a DNS-1123 subdomain of lowercase letters, digits, `-` and `.`, at most 253 characters. | Schema, API server |
| `provisioner.profiles[].spec` | object | — | A `NodeProfile` spec. All [NodeProfile](#nodeprofile) constraints apply. The chart also requires a non-empty `spec.nodePool.names`; create a catch-all profile, which selects every node no other profile claims, outside the chart. | Schema, Template, API server, profile policy |

Profiles created by the chart are owned by the Helm release. Change them
through Helm values; Helm overwrites direct edits. Removing an entry deletes that
`NodeProfile` on the next upgrade, which starts cleanup on its nodes.

### Security

| Key | Type | Default | Valid values and constraints | Checked by |
| --- | --- | --- | --- | --- |
| `security.allowedSourceMirrorHosts` | list of strings | `[]` | Registry authorities that `NodeProfile.spec.registry.mirrors` may point to. Each entry is a host name or IP address with an optional port, such as `mirror.example.com` or `10.0.0.5:5000`. No scheme, path, `@`, comma, surrounding whitespace, wildcard or duplicate entry. Matching is exact; subdomains are not included. | Schema, profile policy |

An invalid entry makes **every** `NodeProfile` fail validation, including
profiles that declare no mirrors. The list is passed to the operator, the
admission webhook and each node worker. See
[registry trust](registry-trust.md) for how mirrors are used.

### Admission

| Key | Type | Default | Valid values and constraints | Checked by |
| --- | --- | --- | --- | --- |
| `admission.enabled` | boolean | `true` | `true` or `false`. See below. | Schema |
| `admission.nodeProfileFailurePolicy` | string | `Fail` | `Fail` or `Ignore`, case-sensitive. Failure policy of the `NodeProfile` and `RuntimeImage` validating webhooks when the admission service is unreachable. | Schema, API server |

With `admission.enabled: false`, the chart installs no admission Deployment and
no mutating or validating webhooks. Pods that select a runtime then do not get
node-capability scheduling constraints or a defaulted read-only root
filesystem, and `NodeProfile`, `RuntimeImage` and `NodeRetirementEvidence`
writes are not validated at admission. The runtime-selection
`ValidatingAdmissionPolicy` is still installed, the operator still validates
`NodeProfile`s before acting on them, and the shim still enforces its checks.

With `nodeProfileFailurePolicy: Ignore`, `NodeProfile` and `RuntimeImage` writes
are accepted unvalidated while the admission service is down. The Pod mutating
webhook always fails open (`Ignore`), and the `NodeRetirementEvidence` webhook
always fails closed (`Fail`). There is no generic `admission.failurePolicy`
value.

### RuntimeClass

| Key | Type | Default | Valid values and constraints | Checked by |
| --- | --- | --- | --- | --- |
| `runtimeClass.overhead.cpu` | string | `"50m"` | Required. Nonnegative Kubernetes CPU quantity, as a string, such as `"100m"` or `"0.25"`. | Schema, API server |
| `runtimeClass.overhead.memory` | string | `"64Mi"` | Required. Nonnegative Kubernetes memory quantity, as a string, such as `"96Mi"` or `"128M"`. | Schema, API server |

`runtimeClass` and `runtimeClass.overhead` accept no other keys. See
[RuntimeClass overhead](#runtimeclass-overhead) for sizing guidance.

### Dashboard

The schema validates the whole `dashboard` block and rejects unknown keys.

| Key | Type | Default | Valid values and constraints | Checked by |
| --- | --- | --- | --- | --- |
| `dashboard.enabled` | boolean | `false` | `true` or `false`. Installs the optional [cluster dashboard](dashboard.md#install-on-the-cluster) and a `<release>-dashboard-viewer` ClusterRole for you to bind to people. | Schema |
| `dashboard.imagePullPolicy` | string | `IfNotPresent` | `Always`, `IfNotPresent` or `Never`. | Schema |
| `dashboard.allowWrite` | boolean | `false` | `true` or `false`. Enables confirmed runtime rotations and installs a `<release>-dashboard-operator` ClusterRole with `patch` on `RuntimeImage`s. Callers still need that permission themselves. | Schema |
| `dashboard.publicURL` | string | `""` | Empty, meaning access only through a loopback port-forward, or an HTTPS origin `https://HOST[:PORT]` with an optional trailing `/`. No credentials, path, query, fragment or whitespace. | Schema, dashboard |
| `dashboard.resources` | object | Requests `cpu: 50m`, `memory: 64Mi`; limits `cpu: 500m`, `memory: 256Mi` | Only `requests` and `limits` keys. Each maps resource names to nonnegative quantity strings. Quote plain numbers, such as `cpu: "1"`. | Schema, API server |

The dashboard image is listed under [Images](#images).

### Uninstall

| Key | Type | Default | Valid values and constraints | Checked by |
| --- | --- | --- | --- | --- |
| `uninstall.timeoutSeconds` | integer | `240` | Whole number from `1` to `86400`. How long the pre-delete hook waits for profile cleanup. The hook Job's deadline is this value plus 10 seconds. | Schema, Template, operator |

Set `helm uninstall --timeout` longer than this value. See
[uninstallation](uninstallation.md).

### Metrics

| Key | Type | Default | Valid values and constraints | Checked by |
| --- | --- | --- | --- | --- |
| `metrics.enabled` | boolean | `false` | `true` or `false`. Exposes operator metrics on port `8080` through the `brewlet-operator-metrics` Service, and node metrics through the headless `brewlet-node-metrics` Service. Both carry `prometheus.io` scrape annotations. | Schema |
| `metrics.nodePort` | integer | `9090` | TCP port from `1` to `65535`. Used only when `metrics.enabled` is `true`. | Schema, operator |

Node workers use host networking, so `metrics.nodePort` is bound on the host
network of every Brewlet node. Choose a port that is free on those nodes and
reachable by your Prometheus. Despite its name, it is not a Service of type
`NodePort`. Changing it updates the worker DaemonSets, which roll out according
to each profile's `spec.rollout.maxUnavailable`. See
[observability](observability.md).

## RuntimeImage

`node.brewlet.sh/v1alpha1`, cluster-scoped:

| Field | Meaning |
| --- | --- |
| `spec.source.image` | Digest-pinned OCI image reference. Tags are rejected. |
| `spec.user.uid`, `spec.user.gid` | Default non-root numeric user and group. Immutable; `0` and the reserved maximum are rejected. |
| `spec.description` | Human-readable description. |
| `spec.compatibility.ecosystem`, `spec.compatibility.abi` | Informational compatibility metadata. Brewlet does not resolve versions from it. |
| `spec.volumeMountPaths` | Immutable list of paths outside `/app/` where workloads may mount volumes. See [mount locations](runtime-images.md#declare-data-and-configuration-mount-locations). |

The object's Kubernetes UID identifies the inventory epoch. `status.generation`
is the runtime generation Brewlet allocated. It is not the same as
`metadata.generation`.

## NodeProfile

`node.brewlet.sh/v1alpha1`, cluster-scoped. Create it directly or through
[`provisioner.profiles`](#provisioner).

| Field | Type | Default | Valid values and constraints |
| --- | --- | --- | --- |
| `spec.nodePool.key` | string | Detected | Node label key that identifies pools, such as `agentpool`. Must be a qualified label key. When empty, Brewlet uses the first of `cloud.google.com/gke-nodepool`, `agentpool`, `eks.amazonaws.com/nodegroup` and `karpenter.sh/nodepool` found on any node; if none is found, a profile with named pools selects no nodes. All profiles with named pools must use the same key. |
| `spec.nodePool.names` | list of strings | `[]` | Pool names to select, each a valid label value of 1 to 63 characters, listed once. A pool may belong to only one profile. Empty makes a catch-all profile that selects every node not claimed by another profile; a cluster may have only one, and the chart cannot create it. Name pools explicitly. |
| `spec.nodePool.includeControlPlane` | boolean | `false` | Opt in to control-plane nodes. |
| `spec.runtimeImages` | list of objects | — | Required, with at least one entry. |
| `spec.runtimeImages[].id` | string | — | Required. Name of an approved `RuntimeImage`: at most 55 characters of lowercase letters, digits, `-` and `.`, with each dot-separated part starting and ending with a letter or digit. Listed once. |
| `spec.runtimeImages[].install` | string | `Eager` | `Eager` installs the runtime on every selected node; `OnDemand` installs it when a Pod needs it. |
| `spec.runtimeImages[].idleTTL` | duration | None | `OnDemand` only. Positive Go duration of at most 64 characters, such as `72h`. Uninstall the runtime after it is unused for this long. |
| `spec.tolerations` | list of objects | `[]` | Tolerations for the worker; none are added by default. `key` is required and must be a qualified name. `operator` is `Equal` (default) or `Exists`; `value` must be empty with `Exists`. `effect` is empty, `NoSchedule`, `NoExecute` or `PreferNoSchedule`. `tolerationSeconds` is an integer. |
| `spec.registry.mirrors` | map of strings | `{}` | Maps a source registry authority (`HOST[:PORT]`, no path) to a mirror `HOST[:PORT][/PATH]`. The mirror must not end with `/`, contain `@` or use the source's host, and its host must be in `security.allowedSourceMirrorHosts`. |
| `spec.rollout.maxUnavailable` | integer | `1` | At least `1`. Maximum nodes whose worker updates at once. |
| `spec.rollout.containerdRestart` | string | `validated` | `validated` or `none`. |

With `none`, Brewlet never restarts containerd, so the host's containerd
configuration must already be correct. Brewlet cannot then repair missing
layer-retention or runtime-handler settings, and cleanup of earlier changes is
still required.

## Pod and controller settings

Set `runtimeClassName: brewlet` and add a Pod annotation for each managed
container, such as `runtime-image.brewlet.sh/app: python-3.13`. Unselected
containers use ordinary OCI behavior. A Pod may select multiple runtimes, but a
single eligible `NodeProfile` must offer the full set. Brewlet adds the runtime
capability constraints while preserving your other scheduling requirements.

Selected containers (application, init, or ephemeral) must use
`securityContext.readOnlyRootFilesystem: true`. Admission sets it when omitted
and rejects an explicit `false`; the shim also refuses a writable root if
admission was bypassed. Unselected OCI containers retain ordinary security
defaults subject to cluster policy.

Volume and device mounts outside `/app/` must be listed in the runtime's
`RuntimeImage.spec.volumeMountPaths`. See
[mount locations](runtime-images.md#declare-data-and-configuration-mount-locations).

To have the operator restart a workload after a runtime rotation, set
`brewlet.sh/restart-on-rotation: "true"` on the workload controller's metadata
(not on its Pod template) and enable `operator.rotationRestarts.enabled`.
`runtime-generation.brewlet.sh/<id>` is a JSON minimum-generation barrier, not a
free-form version string. See [public workload annotations](reference.md#public-workload-annotations).

The chart requires Kubernetes 1.30 or later. Its runtime-selection
ValidatingAdmissionPolicy installs even when the mutating webhook and rotation
restart feature are disabled.

### RuntimeClass overhead

The `brewlet` RuntimeClass reserves `50m` CPU and `64Mi` memory per Pod for
shim and staging overhead. These are scheduling reservations, not measured
usage, and they do not include the language runtime's own memory.

They are set by the [`runtimeClass.overhead` values](#runtimeclass). Set them at installation, based on measurements of your own workloads. For
example, pass this file with `--values overhead.yaml`:

```yaml
runtimeClass:
  overhead:
    cpu: "100m"
    memory: "96Mi"
```

These values are an example, not a sizing recommendation. The chart rejects
empty, negative or non-string values. When passing a plain number on the
command line, use `--set-string`.

[Pod overhead](https://kubernetes.io/docs/concepts/scheduling-eviction/pod-overhead/)
affects scheduling and quota accounting and is recorded when Pods are admitted.
At the defaults, 1,000 Pods reserve an extra 50 CPU cores and 62.5 GiB of
memory. Do not remove the reservation without measurements that justify it.
Changing the overhead affects only newly admitted Pods; existing Pods keep
their values until they are replaced.

## Registry and environment precedence

Credentials for pushing artifacts, kubelet pulls of application images, node
pulls of runtime sources, and application outbound TLS are configured
separately. See [registry and certificate trust](registry-trust.md).

The runtime image's environment provides defaults. Environment variables set in
the container spec override them, except `PATH`, which the runtime always
controls. Exec sessions, probes and hooks use the same environment as the main
process. See [launch descriptor](reference.md#launch-descriptor).
