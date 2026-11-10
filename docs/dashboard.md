# Brewlet Dashboard

The experimental Brewlet Dashboard gives Kubernetes operators a centralized view
of runtime infrastructure. It follows the
[KubeRay dashboard's resource-list and drill-down approach](https://docs.ray.io/en/master/kuberay/user-guides/dashboard.html),
but uses Brewlet's native `RuntimeImage` and `NodeProfile` APIs. It does not
deploy application workloads or replace the Kubernetes API server.

Install the dashboard on the cluster with the optional Helm component, or run
the embedded dashboard locally with your kubeconfig. Both serve the same
dependency-free frontend; no external CDN or JavaScript build is required.
Like the rest of Brewlet, it is pre-GA: evaluate it in a disposable environment.

## Install on the cluster

Choose a Brewlet release that includes the dashboard. The published OCI chart
contains the matching CRDs and a digest-pinned dashboard image, so no separate
image build or override is needed.

```sh
export CONTEXT=my-cluster
export NAMESPACE=brewlet
export BREWLET_VERSION='<release-version-including-dashboard>'
```

Replace the version placeholder with the exact published chart version. Review
the target cluster and install prerequisites as described in
[installation](installation.md). Use these commands for a fresh install or a
configuration change on an existing installation of the same release; in-place
upgrades between releases are unsupported. Helm does not upgrade CRDs, so apply
the CRDs from that chart version first:

```sh
helm show crds oci://ghcr.io/microsoft/charts/brewlet \
  --version "$BREWLET_VERSION" \
  | kubectl --context "$CONTEXT" apply -f -
helm --kube-context "$CONTEXT" upgrade --install brewlet \
  oci://ghcr.io/microsoft/charts/brewlet \
  --version "$BREWLET_VERSION" \
  --namespace "$NAMESPACE" --create-namespace \
  --set dashboard.enabled=true \
  --wait --timeout 5m
```

For an existing Helm installation already using that release, use the same CRD
step, then update it while preserving its configured values:

```sh
helm --kube-context "$CONTEXT" upgrade brewlet \
  oci://ghcr.io/microsoft/charts/brewlet \
  --version "$BREWLET_VERSION" \
  --namespace "$NAMESPACE" \
  --reset-then-reuse-values \
  --set dashboard.enabled=true \
  --wait --timeout 5m
```

The chart installs the dashboard as a ClusterIP service, disabled by default.
For private-registry mirroring, set `dashboard.image` to your mirrored,
digest-pinned image. Keep the image from the same Brewlet release as the chart.

To open it from your workstation, port-forward the service:

```sh
kubectl --context "$CONTEXT" -n "$NAMESPACE" \
  port-forward service/brewlet-dashboard 8088:8088
```

Open **http://127.0.0.1:8088**. This deployment is a hardened, single-replica
Deployment with an unprivileged ServiceAccount. Readiness/liveness probes report
process availability, not cluster health or the signed-in operator's
permissions. Disabling the dashboard on a later Helm upgrade removes only the
dashboard component.

### Sign-in and Kubernetes RBAC

Sign in using a short-lived Kubernetes bearer token issued by your platform.
The server sends that token only to its trusted in-cluster Kubernetes API
endpoint. It does **not** act as the pod ServiceAccount, impersonate users, or
grant the dashboard pod inventory/write roles. Kubernetes audit logs therefore
identify the caller's token identity.

The chart creates `brewlet-dashboard-viewer` (for release name `brewlet`),
intentionally without bindings. It grants read access to RuntimeImages,
NodeProfiles, Nodes, Pods, Events and workload controllers across namespaces.
Review that scope before granting it to a platform group or dedicated reader
identity. If the platform's Kubernetes authentication issues bearer tokens for
users in a group, an administrator can bind that group:

```sh
kubectl --context "$CONTEXT" create clusterrolebinding brewlet-dashboard-viewers \
  --clusterrole=brewlet-dashboard-viewer \
  --group=platform-operators
```

Alternatively, an administrator can create a dedicated reader ServiceAccount
and issue a short-lived token for it:

```sh
kubectl --context "$CONTEXT" -n "$NAMESPACE" create serviceaccount runtime-reader
kubectl --context "$CONTEXT" create clusterrolebinding runtime-reader-view \
  --clusterrole=brewlet-dashboard-viewer \
  --serviceaccount="$NAMESPACE:runtime-reader"
kubectl --context "$CONTEXT" -n "$NAMESPACE" \
  create token runtime-reader --duration=15m
```

Obtain each operator's token through your platform's approved identity process.
Use distinct identities when actor attribution matters. A shared ServiceAccount
token attributes actions to that ServiceAccount, not to the human using it.
There is no built-in OIDC login or token refresh flow. Do not bind the roles to
the dashboard's own ServiceAccount. These bindings and any reader
ServiceAccounts are administrator-managed; Helm does not remove them.

Tokens stay in page memory, never URL parameters or browser storage. Reloading
requires signing in again; **Sign out** clears the token, inventory, evidence
and previews, including in-flight results. Signing out does not revoke the
Kubernetes token or undo an already-submitted operation. Rejected/expired
credentials return to sign-in.

### Shared HTTPS access and runtime rotation

For shared access, configure your TLS-terminating ingress or proxy to route the
HTTPS origin to `brewlet-dashboard:8088`, and set the chart value
`dashboard.publicURL` to that exact origin, such as
`https://runtimes.example.com`. For example, add
`--set dashboard.publicURL=https://runtimes.example.com` to the install or
upgrade command above. The chart does not create an Ingress, certificate or
public LoadBalancer. Preserve the Host header, serve at the origin root, and
restrict in-cluster access to the trusted proxy with your platform's network
policies. Proxy identity headers are not trusted as Kubernetes credentials.
Never log Authorization headers or request tokens at the proxy. The browser
refuses token entry over non-loopback HTTP; loopback port-forward access remains
available even when a public origin is configured.

To enable confirmed runtime rotations, add
`--set dashboard.allowWrite=true` to the Helm install or upgrade command. This
creates the `brewlet-dashboard-operator` role, which grants `patch` on
RuntimeImages only. Bind it to the appropriate operator group or identity, in
addition to viewer access:

```sh
kubectl --context "$CONTEXT" create clusterrolebinding brewlet-dashboard-operators \
  --clusterrole=brewlet-dashboard-operator \
  --group=platform-sre
```

No bindings are created automatically. Previews are bound to the caller's token
and expire after five minutes; a pod restart or credential change invalidates
them.

In-cluster image preflight currently supports registries readable without
credentials. Shared `BREWLET_REGISTRY_USERNAME/PASSWORD` are rejected in cluster
mode: arbitrary preview references must not receive shared registry secrets.
For private-registry preflight, use a local dashboard with your own registry
credentials. Browsing existing private-runtime evidence does not require
registry access.

## Run a local session

Install Brewlet on the cluster first; see [installation](installation.md).
Use your normal kubeconfig and explicitly select the intended context:

```sh
brewlet k8s --context my-cluster dashboard
# Or select a kubeconfig file:
brewlet k8s dashboard --kubeconfig ./cluster.config --context my-cluster --port 8088
```

Open the **complete private URL** printed by the CLI in your browser. The server
listens only on `127.0.0.1`; `--port 0` selects an available port. It does not
open a browser automatically. Stop the process with Ctrl+C.

Read-only is the default, even if your Kubernetes identity can write. To enable
the runtime source rotation workflow:

```sh
brewlet k8s --context my-cluster dashboard --allow-write --namespace brewlet
```

`--namespace` identifies the operator Deployment for restart-policy inspection,
not an inventory filter. RuntimeImages and NodeProfiles are cluster-scoped;
the dashboard reads related pods and events across namespaces. To switch
clusters or identities, stop the server and start a new session.

## Operator workflows

| View | What it shows |
| --- | --- |
| Runtime images | Approval, approved generation and digest, desired versus approved source, offering profiles, rotation and restart state. |
| Runtime details | Runtime generations in use (approved generation plus older generations with freshly observed container bindings, pod and node counts), separately labeled stale evidence, last rotation time, conditions, per-node convergence (Current / Behind), the runtime generation each selecting pod runs, per-workload generation history (current and previous generations, change time), raw binding evidence, and resource JSON. |
| Node profiles | Eager/OnDemand placement and idle TTLs, ready/assigned counts, current-generation readiness, targets, retirement and cleanup conditions. |
| Nodes | Readiness, platform, startup fence, per-runtime installation and digest, timestamped container-binding observations and explicit stale/invalid/missing evidence warnings. |
| Events | The newest 200 retained Kubernetes events whose referenced UID matches the observed Brewlet resources, nodes or pods; older related events are counted as omitted. |

Search works across each view's resource data; runtime search also matches
offering profile names. The state filter isolates resources needing attention.
Select a resource name to open its details below the list.

Refresh manually or every 30 seconds. Automatic refresh pauses while the page
is hidden, while a rotation is being reviewed, or while focus is inside the
resource details. If a resource group cannot be read, it is marked unavailable,
not treated as an empty successful result. If the connection fails, the last
snapshot is explicitly stale and rotation is disabled.

### Rotate a runtime

1. Enable `dashboard.allowWrite` for an installation (or `--allow-write` locally),
   select a runtime and choose **Rotate source**.
2. Enter a registry image tag or digest and choose **Preview rotation**.
3. Review the resolved digest, platform/config checks, compatibility warnings,
   affected nodes, selecting workloads and their restart actions, and live pods.
4. Type the exact runtime ID and choose **Confirm rotation**.

Preview performs the same registry resolution and golden-image config checks
used by the CLI. The new source is always digest-pinned; insecure registry
transport and skipped image checks are not exposed in the dashboard. Local-mode
registry authentication uses the CLI's [registry environment](cli-reference.md#registry-access);
cluster mode has the registry limitation described above.

Confirmation uses a server-signed preview that expires after five minutes,
re-reads the runtime and impact, and patches only `spec.source.image` with an
optimistic resource-version lock. A changed runtime (including a status update),
changed reported impact, expired preview, or mismatched confirmation requires a
new preview. Other resources can still change after the impact read; the preview
is not a cluster-wide transaction or reservation.

A successful request is **not proof that rotation completed**. Refresh and
inspect the operator's conditions, node generations and restart summary.
Existing containers retain their leased generation. Rolling restarts require
both operator configuration and workload opt-in; OnDemand roots not yet
installed are excluded from the participating nodes.

Approval creation, placement edits and runtime removal remain
[CLI](cli-reference.md#cluster-runtime-management-with-brewlet-k8s) or GitOps
operations. For a Helm-managed runtime, update chart values as well, or a later
Helm upgrade may revert the source.

## Access boundary and permissions

Local mode uses your selected kubeconfig identity, including normal client-go
credential plugins. Installed mode uses each signed-in caller's bearer token.
Kubernetes RBAC is the authority for every operation; the dashboard never
elevates access.

| Workflow | Required Kubernetes access |
| --- | --- |
| Inventory and evidence | `list` RuntimeImages and NodeProfiles in `node.brewlet.sh`; `list` core Nodes, Pods and Events. Pods and Events are read across namespaces. |
| Rotation preview and confirmation | Inventory access above; `get` RuntimeImages and Nodes; `list` Deployments, StatefulSets and DaemonSets across namespaces; `get` the `brewlet-operator` Deployment in `--namespace`. |
| Apply rotation | `patch` the selected RuntimeImage, in addition to preview permissions. Admission policy still applies. |

If operator configuration is unreadable, workload restart actions explicitly
say that restart settings are unknown; this is not interpreted as permission to
restart. Other missing preview dependencies fail the preview.

In local mode, the URL fragment contains a random per-process bearer token. The frontend
removes it from the address bar, holds it only in page memory, and sends it in
an authorization header for API calls. **After reloading, reopen the original
printed URL.** Keep that URL private; anyone holding it on the machine can use
the dashboard with the process's Kubernetes identity and enabled write scope.

Local kubeconfig credentials never go to the browser. Evidence responses are not cached;
the server rejects unexpected Host headers and cross-origin API requests,
does not enable CORS, and serves a restrictive content security policy.
The preview signing key is separate from the browser's access token.

Do not expose **local mode** through an ingress, reverse proxy, shared
port-forward or tunnel: anyone with its private URL inherits the process's
Kubernetes identity. Loopback does not isolate other local processes.
Use `--in-cluster` via the Helm deployment for multi-user access instead.

## Evidence is not an audit log

**Export evidence** downloads the last collected, timestamped JSON snapshot,
including partial-read errors, omitted-event counts and a stale flag. Kubeconfig,
session tokens, arbitrary node annotations, Pod specifications and workload
environment variables are not included. Resource names, registry references,
event messages and runtime bindings can still be sensitive; handle exported
files accordingly.

The snapshot consists of sequential API reads, not an atomic cluster view.
Kubernetes events expire and aggregate repeated occurrences; they do not prove
who changed a resource or preserve every change. For durable actor-attributed
change history, configure Kubernetes API audit logging and retention outside
the dashboard.

The operator emits `RuntimeApproved` when it first approves a runtime, then
`RotationStarted` for a new digest and `RotationComplete` when participating nodes
have converged. These events identify the runtime generation and digest;
`RotationStarted` also includes the previous generation and digest.
Completion reports participating and excluded node counts, not a workload
restart receipt: existing containers may still use older generations.
Events are emitted after the corresponding status is persisted, regardless of
whether the change came from the dashboard, CLI or GitOps. Rotations completed
before an operator with this event support was installed are not backfilled.

Node annotations report current-generation bindings. RuntimeImage
`status.taskObservations` retains older or foreign-epoch task/lease tuples,
including stale observations. The dashboard exposes completeness, identity and
timestamps rather than inferring that missing observations mean zero bindings.
A recent observation is diagnostic evidence, not cleanup authority.
The **Runtime generations** table counts only fresh evidence: node binding
observations that are complete, identity-matched and no more than two minutes
old, plus `taskObservations` entries the operator still marks `current` within
the same window. Each container is counted once. Bindings from older or
non-current observations appear separately under **Stale evidence** with their
last-observed time; they may have exited and are not counted as in use. Nodes
without a fresh observation are named, because their containers are unknown.
The table shows which generations workloads use now, not a durable history: a
retired generation disappears once no fresh observation binds a container to
it. Brewlet's
runtime generation (`g2`) is `status.generation`, which increments on each
approved source change; it is distinct from the Kubernetes spec revision
(`metadata.generation`) shown with conditions.

### Auditing when a workload changed generation

Rotation never restarts running containers by itself; a workload adopts a new
runtime generation only when its pods are replaced, whether by a manual
`kubectl rollout restart`, a deploy, node churn or opted-in rotation restarts.
The operator therefore records the change from binding evidence, not from the
restart that caused it:

- RuntimeImage `status.workloadGenerations` lists each owning workload
  (ReplicaSet pods are attributed to their Deployment; uncontrolled pods are
  their own workload) with the generations, digests and pod counts its running
  containers are bound to, the previous set and `changedAt`. Only the latest
  change is kept. A workload with no running pods keeps its last observed
  generations with zero pods; a deleted workload is removed.
- Each change emits a `RuntimeGenerationChanged` event on the workload, for
  example `Runtime python: running containers moved from generation 2
  (sha256:…) to generation 3 (sha256:…) across 2 pod(s)`. A rolling update
  typically emits two: old → old + new, then old + new → new.
- `brewlet_runtime_workload_generation_pods` reports pods per workload and
  generation, so Prometheus retains the full timeline.

Records change only when every node claimed for the runtime has a fresh,
complete binding observation, so missing evidence never looks like a change.
The first observation of a workload is recorded without an event, and changes
are not backfilled. The operator reads only metadata of pods, ReplicaSets,
Deployments, StatefulSets, DaemonSets and Jobs for this (`get`, cluster-wide); a
workload kind it cannot read is retained rather than presumed deleted. The runtime details panel shows this as **Workload
generation history**, and the Events page includes the workload events. Events
expire, so export them or keep the metric for a durable audit trail; neither
identifies who restarted the workload — use Kubernetes API audit logs for that.
NodeProfile cleanup still requires current-generation `CleanupComplete`
conditions and the retirement lifecycle; a missing node or worker proves
neither cleanup nor safe deletion.

## Development

The HTTP integration tests use a fake Kubernetes client and a local OCI test
registry. The frontend uses native browser modules and Node's built-in test
runner, with no npm dependencies:

```sh
go test ./cmd/brewlet -run TestDashboard
make test-dashboard
```

For an explicitly synthetic browser preview without a cluster:

```sh
BREWLET_DASHBOARD_PREVIEW=1 go test ./cmd/brewlet \
  -run '^TestDashboardBrowserFixture$' -v -timeout 16m
```

Open the fixture's printed URL. The context clearly identifies the synthetic
data, and the printed image reference can exercise rotation against the local
test registry. The fixture stops after 15 minutes or when interrupted.
