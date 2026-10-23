# Troubleshooting

Start with the failing boundary, not with deleting runtime state. Pin your
reviewed kubeconfig/context and preserve original object UIDs, source digests,
conditions and events before changing anything.

| Symptom | Inspect | Safe next action |
| --- | --- | --- |
| Pod remains Pending | Pod events, node affinity, pool offerings and startup taints. | Correct explicit ID/platform constraints or provisioning; do not tolerate the startup fence. |
| `NoCompatibleRuntime` | Approved ID and valid profiles offering it. | Fix approval/placement. A valid empty pool is different from nonexistent inventory. |
| `RuntimeImageInstalling` | Worker logs, first-use request and runtime status. | Observe the bounded install/retry path; do not run workload code before publication. |
| `RuntimeImageInstallFailed` | Source access, platform coverage, image policy and worker error. | Correct the source/configuration; preserve failure evidence. |
| `RuntimePATHRequired` | Descriptor and Exec/probe executable. | Use the documented absolute executable path or approve a compatible runtime PATH; Pod PATH is not a fallback. |
| Root or privilege rejection | Effective Pod/container securityContext. | Use nonzero UID/GID, no capabilities and no privilege escalation. |
| ImagePull failure | Application registry identity, imagePullSecrets, CA and digest. | Fix kubelet application access separately from runtime-source access. |
| Expired, revoked or epoch-mismatched authority | RuntimeImage, current Node/profile UID and worker authority. | Restore valid approval/ownership or complete normal drain; labels alone cannot authorize starts. |
| New runtime is current, old process still runs | Actual container binding and controller rollout status. | This is expected until the application rolls. Check opt-in/barriers if restart coordination was requested. |
| Deferred rollout | Controller strategy, paused/partitioned state, excluded node and failed replacement Pod. | Resolve the specific blocker; do not equate a template patch with completion. |
| Cleanup/uninstall remains blocked | Original targets, leases, tasks/Exec/mounts, worker teardown and provider pins. | Remove real blockers and retry normal cleanup; never remove finalizers to claim success. |

## Collect a useful snapshot

Replace names and namespace with the actual workload:

```sh
kubectl get runtimeimages,nodeprofiles -o yaml
kubectl get nodes -L brewlet.sh/runtime
kubectl -n apps describe pod <pod>
kubectl -n apps get deployment <deployment> -o yaml
kubectl -n apps get events --sort-by=.metadata.creationTimestamp
kubectl -n brewlet logs deployment/brewlet-operator --tail=200
kubectl -n brewlet get pods -o wide
```

Read the selected worker's logs and the node's protected
`brewlet.sh/runtime-bindings` observation. Check UID, completeness and freshness;
the annotation is diagnostics, not cleanup authority. Host containerd journals
require administrator access. Do not dump registry credentials or private keys
into bug reports.

## Shell-free diagnostics

A distroless runtime may have no `sh`, `cat`, `curl` or account lookup tools.
Use HTTP probes or an explicit runtime/app diagnostic executable, for example:

```sh
kubectl -n apps exec <pod> -- /usr/bin/python3.13 -c \
  'import os,sys; print(sys.version); print(os.getuid(), os.getgid(), os.getcwd())'
```

That path is an example for a runtime that provides it, not a detected default.
Ephemeral containers in a Brewlet Pod also need compatible v1 artifacts and
the same ID; an arbitrary diagnostic container is not automatically supported.

## After a restart

Distinguish worker recovery of durable leases, containerd reconnection to
surviving shims, a killed shim's actual task-service outcome, and a broken
client streaming connection. They are not equivalent. A surviving process
does not guarantee that an old `kubectl exec` stream reconnects.

## Still blocked

Preserve diagnostics, source revision and original identities. Use
[safe removal/recovery](uninstallation.md) and the
[verified scenario boundaries](verification.md). Do not manually rewrite
authorization, clocks, claims, generation barriers or cleanup receipts.
