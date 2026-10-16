# Brewlet 2.0 operations and verification

This branch is under implementation, not a deployable or production-certified
release. Check `acceptance/requirements.json` before enabling any capability.
The complete proposal remains normative even where code is incomplete.

## Contracts

Only a reviewed `RuntimeImage` ID selects runtime bytes. Its source is a
canonical digest-pinned image, and its Kubernetes UID establishes the inventory
epoch. A stable ID promises compatible ABI, account, language line, PATH and
documented tools across rotations. Incompatible updates and frozen holds need
separate administrator-owned IDs. Runtime rotations never remediate dependency
copies packaged by the application under `/app`.

All application, init, sidecar and ephemeral containers use v2 artifacts and the
same pod-wide ID. The infrastructure sandbox alone follows the ordinary runc
path. Workload manifests must explicitly select native-content architectures
using standard `kubernetes.io/arch` selection; artifact platform declarations
are not scheduler instructions. The CLI never infers either.

The runtime supplies PATH. Explicit pod PATH is ignored with an admission
warning. Runtime Env defaults are merged with final CRI environment values.
Initial and exec processes must share effective environment, non-root identity,
HOME and the leased generation. Explicit argv and working-directory overrides
retain Kubernetes semantics. No VM/interpreter flags are injected.

Packaged `/app` is read-only and world-readable. It cannot contain secrets.
Use Secret volumes or another scoped secret mechanism and explicit writable
volumes for application data. Non-root UID/GID and no privilege escalation are
mandatory, including on fail-open admission and exec paths.

## Provisioning, fencing and rotation

Only the authorized provisioner may write runtime roots. Before host mutation
it checks the original Node UID, UID-bound claim, literal profile UID, durable
profile generation and target authority. Requests are not authority. Initial
fence failures must not mutate even request markers or advertisements.

Root publication, current resolution, leases and reclamation require the
per-ID lock. Running tasks retain their generation across rotation. Collection
must conservatively preserve roots and cached layers while tasks, execs, mounts
or uncertain recovery evidence remain. Authorization expires after five minutes
by default and cannot be extended without fresh cluster checks.

An OnDemand-only node validates the shim, handler, forwarded annotations,
ownership and authorization before baseline readiness, without fetching roots.
First-use requests have a bounded wait and kubelet retries. Failed installation
withdraws per-ID capability and backs off; it must not silently leave a false
`available` advertisement.

The startup fence is the removable NoSchedule taint
`startup-taint.cluster-autoscaler.kubernetes.io/brewlet=provisioning`.
Application pods cannot tolerate it, including via wildcard tolerations.
Readiness observations must account for it even when provider-pinned labels
continue to say ready. If a provider refuses capability withdrawal, the
authorized worker must apply **and read back** the node-wide fence. If neither
withdrawal nor fence succeeds, progress and deletion remain blocked. Cleanup
does not authorize fence removal while stale provider labels remain.

Rotation restarts require both cluster enablement and a workload-object opt-in.
They patch templates, never delete pods or change strategies. The propagated
epoch/generation barrier prevents stale starts even on already-bound pods.
OnDelete, paused, partitioned, cold-install and excluded-node rollouts require
accurate deferred/not-applicable status; a template patch is not completed
remediation.

## Provider templates

EKS Cluster Autoscaler simulation tags use `PropagateAtLaunch=false`. Their
capabilities must exactly match valid profile inventory; synthetic readiness
must not be placed on an unprovisioned real node.

Karpenter copies template labels to real nodes. Use its `startupTaints` for
nodes requiring post-registration provisioning. An OnDemand ID needs mutable
real-node labels; a pinned per-ID `available` label is unsupported.

AKS pool labels are real-node labels, not simulation-only tags. Eager pinned
ready labels require removable **node initialization taints**, not pinned
ordinary `--node-taints`. Initialization taint changes require the provider's
feature registration and node-image regeneration. Do not claim scale-from-zero
solely from an approved profile or a pending Pod. No provider-scale proof has
been established on this branch.

## Validation boundaries

`make test` covers portable unit/contract tests and the optional Maven producer.
It is not proof of Linux mounts, real scheduling or provider behavior.
`integration-tests/fixtures` contains four application producers exercising
ordinary argv/dependency contracts. Host execution of a fixture proves only that
fixture works, not that Brewlet runs it.

Real-node acceptance needs an invocation-owned Linux/containerd 2 cluster with
cgroup v2 and runc, an invocation-owned registry, pinned approved runtime
images, and build tools for Java, Python, Node.js and .NET. Never run against a
shared current kubectl context. Pin `KUBECONFIG` and cluster/registry names; retain
failure diagnostics and remove only invocation-owned resources.

Every section 13.1 scenario needs its specified proof, including an actual
provider/admission interceptor for label refusals. CRI filesystem usage and
eviction need real conformance, not a file-location assertion. A skipped test,
fake client, synthetic local authorization, pre-existing runtime installation
or direct `docker run` is not successful Brewlet end-to-end coverage.

Deletion and uninstall must retain ownership, ledgers, finalizers and needed
RBAC until verified host cleanup, worker teardown, conditional claim release and
all missing-host obligations are resolved. Do not remove finalizers or rewrite
claims as a recovery shortcut.
