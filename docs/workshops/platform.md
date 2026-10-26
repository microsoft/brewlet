# Platform workshop

Your outcome is usable runtime infrastructure and a bounded application
handoff, not merely healthy operator Pods.

## Prepare and approve

Use [installation](../installation.md), or the
[disposable local tutorial](../local-kubernetes.md) for a single-person session.
Review node mutation authority and matching components before selecting pools.

Approve a digest under a deliberate compatibility ID. Choose numeric defaults,
verify platforms and decide whether Eager or OnDemand fits the pool.
Compatible upstream/distroless roots need no special mode; custom roots remain
subject to the same security and executable-layout contract.

Choose `RuntimeImage.spec.volumeMountPaths` before approval for temporary,
cache and configuration volumes outside `/app/`. This contract is immutable
within the runtime ID. Runtime roots are always read-only: a writable `/tmp`
in the source image is not sufficient. Supply reviewed workload templates with
explicit volumes, size/resource budgets and non-root permissions; do not rely
on Brewlet to inject temporary storage or language-specific cache settings.
See [mount policy](../runtime-images.md#declare-data-and-configuration-mount-locations)
and [writable storage](../resources.md#probes-exec-files-and-shutdown).

## Verify the infrastructure

Check that each RuntimeImage is `Approved`, each NodeProfile is `Ready`, nodes
are ready without the startup taint, and OnDemand runtimes install on first use.
`brewlet k8s runtime status ID` and `brewlet k8s doctor` summarize this state.
Exercise a real application before calling the handoff complete.
Do not certify a cloud provider from local-kind success.

## Give developers a concrete handoff

| Item | Required answer |
| --- | --- |
| Brewlet release | The installed release (0.8.1): matching `brewlet` CLI and, optionally, `sh.brewlet:brewlet-maven-plugin`. |
| Namespace and permissions | Publishing/application access without runtime approval or profile-edit privileges. |
| Runtime ID | Approved compatibility ID, documented absolute executable and tool paths. |
| Platforms | Actual supported OS/architectures and required scheduling constraints. |
| Registry | Application repository, credentials delivery and CA policy. |
| Identity/data | Numeric defaults, required read-only root, approved mount paths and explicit writable/Secret volume templates. |
| Resources and telemetry | Requests/limits policy, collector destinations and operational contacts. |
| Updates | Runtime rollout policy versus application dependency/image updates. |

Keep runtime approval and node access out of the developer's normal workload
permissions. For follow-up exercises, observe a cold install, rotate a source
and inspect old/new bindings without editing leases.

Finish with the normal [cleanup procedure](../uninstallation.md).
