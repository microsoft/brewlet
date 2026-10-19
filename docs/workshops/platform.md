# Platform workshop

Your outcome is usable runtime infrastructure and a bounded application
handoff, not merely healthy operator Pods.

## Prepare and approve

Use [source-built installation](../installation.md), or the
[disposable local tutorial](../local-kubernetes.md) for a single-person session.
Review node mutation authority and matching components before selecting pools.

Approve a digest under a deliberate compatibility ID. Choose numeric defaults,
verify platforms and decide whether Eager or OnDemand fits the pool.
Compatible upstream/distroless roots need no special mode; custom roots remain
subject to the same security and executable-layout contract.

## Verify the infrastructure

Check current inventory approval, profile/Node UIDs, effective readiness without
the startup fence, and actual first-use installation where applicable.
Exercise a real application before calling the handoff complete.
Do not certify a cloud provider from local-kind success.

## Give developers a concrete handoff

| Item | Required answer |
| --- | --- |
| Source revision/tooling | Reviewed CLI and producer source; matching optional Maven build. |
| Namespace and permissions | Publishing/application access without runtime approval or profile-edit privileges. |
| Runtime ID | Approved compatibility ID, documented absolute executable and tool paths. |
| Platforms | Actual supported OS/architectures and required scheduling constraints. |
| Registry | Application repository, credentials delivery and CA policy. |
| Identity/data | Numeric defaults, permitted non-root overrides, writable/Secret volume policy. |
| Resources and telemetry | Requests/limits policy, collector destinations and operational contacts. |
| Updates | Runtime rollout policy versus application dependency/image updates. |

Keep runtime approval and node access out of the developer's normal workload
permissions. For follow-up exercises, observe a cold install, rotate a source
and inspect old/new bindings without editing leases.

Finish with the normal [cleanup procedure](../uninstallation.md).
