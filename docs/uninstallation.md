# Uninstalling Brewlet safely

Removing the Helm release is **not** the same as preparing a fresh installation.
Brewlet must finish node runtime cleanup before its control plane is removed;
Helm then intentionally retains the CRDs. The CLI rejects a fresh install while
either Brewlet CRD exists, even when both contain no resources.

Use this guide for a complete uninstall or before
[replacing a release](installation.md#upgrading). There is no `brewlet uninstall`
command: use profile cleanup and the installed control plane's removal path.
These operations do not delete your Kubernetes cluster, node pools, or registry
artifacts. They also do not purge retained host caches or staging trees.

!!! warning "Cleanup needs the installed control plane and original nodes"
    Plan downtime or migrate workloads first. Keep the operator, provisioner
    RBAC, Kubernetes API access, and original target nodes available until host
    cleanup and worker teardown finish. Do not delete the namespace first,
    remove finalizers, kill cleanup workers, or use Helm `--no-hooks` to bypass
    a failure. Pause node scale-in/replacement as well as configuration writers;
    losing a recorded node UID can make cleanup unrecoverable in place.

## 1. Inventory and preserve recovery information

Confirm the intended kubeconfig/context for **kubectl, Helm, and Brewlet**.
Examples use release `brewlet` in release namespace `brewlet`. The component
namespace can differ; replace `-n brewlet` in worker, operator, and Job commands
with the installed component namespace, not necessarily the release namespace.

Start with read-only inventory:

```bash
kubectl config current-context
helm list --all --all-namespaces
helm status brewlet -n brewlet
helm get values brewlet -n brewlet --all
helm get manifest brewlet -n brewlet
helm get hooks brewlet -n brewlet
kubectl get nodeprofiles.node.brewlet.sh -o yaml
kubectl get javaapplications.apps.brewlet.sh --all-namespaces -o yaml
kubectl get deployments,statefulsets,daemonsets,jobs,cronjobs,hpa --all-namespaces
kubectl get daemonsets,pods --all-namespaces -o yaml
kubectl get nodes -o yaml
kubectl get runtimeclass brewlet -o yaml
kubectl get crd nodeprofiles.node.brewlet.sh javaapplications.apps.brewlet.sh -o yaml
```

Preserve the installed chart version and component image digests, original
values files, profile/workload source manifests, and any customized paths or
runtime registration policy. Save the inventory securely, including profile
status/cleanup ledgers, node and worker UIDs, ownership records, condition
messages, and relevant logs. Exports may contain sensitive configuration; do
not publish them or commit credentials. Keep source-of-truth manifests separate
from diagnostic exports: do not reapply old status, UIDs, or finalizers as new
installation manifests.

If a release or API type is already absent, record that fact and investigate
the remaining installation rather than rerunning earlier deletion steps.
Permission errors, API failures, and inaccessible nodes are **not** evidence
that inventory is empty. Obtain cluster-wide visibility before proceeding.

Check the **installed** output of `helm get hooks`, not a newly downloaded
chart. The current cleanup hook has `helm.sh/hook: pre-delete`, a Job labelled
`app=brewlet-uninstall`, and dedicated ServiceAccount/ClusterRole/ClusterRoleBinding
resources. If it is absent, follow
[manual control-plane removal](#manual-control-plane-removal); do not assume a
plain Helm uninstall will clean the nodes.

## 2. Stop workloads and their writers

Arrange downtime or move applications to a separately provisioned environment.
Pause GitOps/Helm automation, NodeProfile writers, scheduled jobs, and autoscaling
or other reconcilers that could recreate workloads or change node ownership.
Coordinate removals through each resource's source of truth; pause further
recreation after the intended removal is applied. **Keep the Brewlet operator
running** so it can reconcile profile cleanup.

Deleting Pods alone is insufficient: their controllers will replace them.
For a reviewed **raw Deployment**, with its HPA and external writers disabled,
scale the controller to zero or remove it through its owner. For example:

```bash
kubectl scale deployment <raw-deployment> -n <workload-namespace> --replicas=0
kubectl get pods -n <workload-namespace> -o wide
```

For a `JavaApplication`, changing its generated Deployment alone is not durable:
the Brewlet operator reconciles that Deployment, and an enabled HPA owns its
replica count. Change the owning `JavaApplication` source to disable autoscaling
and set `spec.replicas: 0`, or remove the application through its owner. A
reviewed, directly managed application can be removed with:

```bash
kubectl delete javaapplication <application> -n <workload-namespace> \
  --cascade=foreground --wait=true --timeout=10m
```

Wait for dependent controllers and Pods too; a successful request is not proof
that all processes have stopped. Apply equivalent controller-level handling to
StatefulSets, Jobs/CronJobs, DaemonSets, and other workload owners.

Inspect **all namespaces**, including pending and terminating Pods:

```bash
kubectl get pods --all-namespaces \
  -o custom-columns='NAMESPACE:.metadata.namespace,NAME:.metadata.name,RUNTIME:.spec.runtimeClassName,NODE:.spec.nodeName,PHASE:.status.phase,DELETING:.metadata.deletionTimestamp'
```

Do not continue while Brewlet workloads are running, being recreated, or
terminating through grace periods or `preStop` hooks. Remove pending work that
could launch later. Also retire direct host launches and exported bundles that
consume Brewlet runtime or stage files. Profile deletion does not migrate
workloads, and the Helm hook is not a substitute for this workload check.

## 3. Deprovision profiles outside the release

The operator is cluster-wide. Any unmanaged or other-release NodeProfile blocks
the release's uninstall hook: removing the shared operator would strand it.
Coordinate with those owners first, or retain the control plane.

For each **unmanaged** profile, after its workloads have stopped:

```bash
# Replace java-workers with one reviewed unmanaged profile name.
brewlet k8s profile delete java-workers --dry-run
brewlet k8s profile delete java-workers --wait --wait-timeout 15m
```

The dry run reads live state and runs guards without deleting; it is not a
reservation against later changes. `--dry-run=server` additionally validates
the conditional deletion through the API server. Do not combine dry run with
`--wait`. The live command checks ownership and uses UID/resourceVersion
preconditions; re-inspect concurrent changes rather than forcing a retry.

Do not use `--yes` in this safe-uninstall sequence: it bypasses the workload
guard, including inability to list Pods. It does **not** bypass Helm/GitOps
ownership or complete host cleanup. See the full
[profile delete flags and guards](cli-reference.md#deleting-a-profile).

Helm/GitOps-owned profiles must be removed through their owning source, not
by stripping ownership markers. Leave this release's Helm-owned profiles for
its cleanup hook. Owners of other-release profiles must deprovision those
profiles before this shared control plane is removed. Once an owner has
requested deletion, `brewlet k8s profile delete NAME --wait --wait-timeout 15m`
can follow it without issuing another deletion.

`--wait` ends when the profile is gone; a timeout does not cancel cleanup.
`Ready=False/CleanupBlocked` exits nonzero immediately and needs repair.
The operator stops provisioning workers, cleans recorded targets, persists
completion, and tears down cleanup workers before releasing the finalizer.
Confirm worker teardown cluster-wide, not just the CLI exit status:

```bash
kubectl get nodeprofiles.node.brewlet.sh
kubectl get daemonsets,pods --all-namespaces -o wide
```

Review worker labels **and owner references/UIDs** against the saved inventory,
including standalone, orphaned, and foreign-namespace workers. A filtered
namespace or label-only query is not a complete inventory. The hook refuses
standalone and foreign-namespace workers; their responsible owners must finish
deprovisioning without adoption or forced deletion.

## 4. Remove the control plane

### Helm installation with the cleanup hook

Only after the previous steps, run:

```bash
helm uninstall brewlet --namespace brewlet --timeout 5m
```

The pre-delete Job uses the **same operator image** in bounded coordinator-only
mode with its own unprivileged service account. It selects profiles by both
Helm release ownership annotations and `app.kubernetes.io/managed-by=Helm`,
requests deletion with UID/resourceVersion preconditions, and waits for profiles
and provisioning/cleanup workers to disappear. The running operator performs
host cleanup. Neither coordinator success nor its repeated empty scans lock
out concurrent writers; keep automation paused.

### Blocked hooks, timeouts, and retry

A cleanup failure, foreign profile/worker, unavailable node, API error, or
timeout fails the hook and leaves the normal operator/RBAC available. Inspect
the condition messages and logs before retrying:

```bash
kubectl get nodeprofiles.node.brewlet.sh -o yaml
kubectl get daemonsets,pods --all-namespaces -o wide
kubectl get jobs -n brewlet -l app=brewlet-uninstall
kubectl logs -n brewlet -l app=brewlet-uninstall --all-containers=true
kubectl logs -n brewlet deployment/brewlet-operator --all-containers=true --tail=200
kubectl logs -n brewlet <cleanup-pod> -c provisioner --tail=200
kubectl describe pod -n brewlet <blocked-pod>
```

Use the actual operator Deployment and worker names from inventory. The default
coordinator timeout is 240 seconds; the Job adds 20 seconds and allows a
10-second termination grace period. For longer operations, configure
`uninstall.timeoutSeconds` **before uninstalling**, with the installed chart
version and preserved component/value overrides. Set Helm's `--timeout` greater
than that value plus 30 seconds. Increasing only Helm's timeout does not extend
the coordinator deadline. Configure `uninstall.imagePullSecrets` when its image
needs credentials; see [chart values](configuration.md#helm-chart-values).

Save failed Job logs before retrying: failed Jobs remain for diagnosis until the
next attempt. Helm may remove successful earlier hook resources even if the Job
fails; a retry recreates dedicated hook RBAC. Repair the actual cause, let
cleanup finish, then retry the same Helm uninstall with the appropriate timeout.
Do not use `--no-hooks`, force-delete workers, remove finalizers, delete the
namespace, or fabricate ownership, cleanup status, readiness, or safety records.

For `CleanupBlocked`, missing/replaced nodes, or unverifiable host state, follow
[blocked cleanup recovery](installation.md#blocked-cleanup-recovery).
Restore the installed components and access if they were removed prematurely;
a fresh install is not a recovery mechanism. If in-place cleanup cannot be
established, preserve evidence and obligations and use a reviewed platform
decommissioning/replacement process or separate fresh environment.

### Manual control-plane removal

For raw manifests or an **installed chart without a cleanup hook**, perform
explicit profile cleanup while the original operator/RBAC/API and nodes remain
available. After the same workload and ownership review, remove profiles through
their source of truth. Use the CLI sequence above for unmanaged profiles.
For legacy Helm-managed profiles, remove them from the installed chart's values
through a reviewed same-version maintenance update; do not use a CLI ownership
bypass.

If a reviewed raw-manifest installation requires explicit API deletion, use
exact names, never a blanket deletion:

```bash
kubectl delete nodeprofile <reviewed-profile-name> --wait=true --timeout=10m
kubectl wait --for=delete nodeprofile/<reviewed-profile-name> --timeout=10m
```

Wait for **every** profile and provisioning/cleanup worker to disappear across
namespaces and verify host cleanup before removing shared RBAC. For the older
chart, only then run the Helm uninstall command above. For raw manifests,
remove only the reviewed control-plane resources from that installation.
Do not delete an entire manifest bundle containing CRDs or a namespace as a
shortcut. Legacy standalone workers require their installed release's cleanup
procedure; deleting their DaemonSet alone is not host cleanup.

## 5. Review retained resources before reinstalling

**Normal uninstall completion:** workloads are stopped or migrated, all profile
cleanup and worker teardown have finished, and the reviewed control plane is
removed. CRDs, the component namespace, the shared RuntimeClass, and retained
host files may still exist. This is expected, not permission to purge them.

**Fresh-install eligibility:** additionally review retained resources and host
state with their owners, resolve remaining cleanup obligations, and remove only
the reviewed empty Brewlet CRDs. Absence of a Helm release alone is insufficient.

### Inspect and remove reviewed empty CRDs

After cleanup, repeat the cluster-wide inventory and preserve any needed
recovery evidence. In particular:

```bash
kubectl get crd nodeprofiles.node.brewlet.sh javaapplications.apps.brewlet.sh
kubectl get nodeprofiles.node.brewlet.sh -o yaml
kubectl get javaapplications.apps.brewlet.sh --all-namespaces -o yaml
kubectl get daemonsets,pods --all-namespaces -o yaml
kubectl get nodes -o yaml
```

Each existing CRD must have **zero surviving custom resources**, including
terminating resources. A scaled-to-zero `JavaApplication` still exists: preserve
its source and remove it through its owner, then verify its dependents are gone.
If one CRD is already absent, explicitly confirm that absence and inspect the
remaining type. Do not treat a failed resource listing as an empty result.

!!! danger "CRD deletion deletes all resources of that type"
    Do not delete a CRD to unstick a profile, remove finalizers, or erase cleanup
    evidence. Proceed only when owners confirm there are no surviving custom
    resources, provisioning/cleanup workers, unresolved host-cleanup obligations,
    or required recovery evidence depending on these CRDs. Keep writers paused
    and recheck immediately before deletion.

Only after those checks, explicitly delete the two reviewed empty CRDs:

```bash
kubectl delete crd nodeprofiles.node.brewlet.sh javaapplications.apps.brewlet.sh \
  --wait=true --timeout=5m
kubectl get crd nodeprofiles.node.brewlet.sh javaapplications.apps.brewlet.sh \
  --ignore-not-found
```

The final command should list neither CRD. If only one existed, delete only
that exact name. If deletion blocks, investigate; do not force it or strip
finalizers.

### Namespace and shared RuntimeClass

The component namespace is retained to protect unrelated objects. The
operator-created `brewlet` RuntimeClass is shared and is not chart-owned.
Inspect both and coordinate any removal with their owners; do not blindly
delete them. Neither is the CLI's **existing-CRD guard**. Their presence does
not justify deletion, and their absence does not establish clean host state.

### Retained AppCDS cache files

Helm uninstall and provisioner teardown do not remove the AppCDS cache.
Maintenance manages only
[64-hex private entries and writer markers](appcds.md#43-node-side-regeneration-the-durable-answer-for-a-patched-fleet);
unrecognized paths are untouched. Review `/opt/brewlet/cds` or the configured
`BREWLET_CDS_CACHE` with its owner.

Before manual reclamation, drain workloads, stop cache consumers and writers,
pause provisioning automation, and finish host cleanup and worker teardown.
Remove only individually verified unused paths belonging to the retired
installation. Do not follow symlinks, purge the root, use wildcard deletion,
delete live files, or discard recovery evidence. Unverifiable ownership or use
requires investigation, not deletion.

### Retained runnable stage roots

Uninstall does not remove runnable staging trees or prove their files unused.
Review all configured `BREWLET_RUNNABLE_STAGE` locations (default
`/tmp/brewlet-runnable` on Linux), including legacy layouts, pending directories,
and references from shims, mounts, local launches, and exported bundles.
Replacing a shim binary does not terminate old shim processes.

Follow [stage reclamation rules](runnable-image.md#reclaiming-unused-stages) and
[activation safety requirements](installation.md#activating-runnable-stage-gc).
Do not rename retained roots, forge `.stage-gc-compatible` records, or bypass
live-reference/locking checks. There is no blanket host-directory or cache purge
step. Missing Brewlet API labels, Pods, or CRDs alone is **not** proof that host
files are absent or unused.

## Troubleshooting: "Brewlet CRDs already exist"

`brewlet k8s install` may report this after a successful Helm uninstall:

```text
Brewlet CRDs already exist; install is fresh-install-only. Complete safe teardown and retained-resource review before reinstalling (docs/installation.md#upgrading)
```

The CLI checks for either `nodeprofiles.node.brewlet.sh` or
`javaapplications.apps.brewlet.sh`; even an empty retained CRD triggers the error.
Do not bypass the guard with a different installation command.

If the release, profiles, applications, and workers are already gone, do not
repeat destructive cleanup blindly. Verify that cleanup really completed,
review retained host state/evidence, then follow
[the exact-name empty-CRD removal step](#inspect-and-remove-reviewed-empty-crds).
A remaining shared RuntimeClass or namespace is not the cause of this message.

After the environment is safely prepared, retry the original `brewlet k8s install`
command with its reviewed values and target release, or follow
[fresh installation](installation.md). Use matching chart/components/CRDs;
recreate reviewed manifests in the target format and rebuild artifacts where
required by the [compatibility policy](compatibility.md). Verify
[installation readiness](installation.md#verify-the-installation) before
restoring workloads, autoscaling, and configuration writers.
