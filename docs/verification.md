# Verification and limitations

The authoritative records are the
[requirements ledger](https://github.com/microsoft/brewlet/blob/brewlet-2.0/acceptance/requirements.json)
and [exact commands, sources and scoped results](https://github.com/microsoft/brewlet/blob/brewlet-2.0/acceptance/RESULTS.md).

## Local implementation evidence

| Evidence | Scope |
| --- | --- |
| 101 reconciled requirements | Aggregate Proposal 0009 requirements-to-evidence mapping. |
| 82-assertion final complete run | Four languages, delivery, rotation, CRI accounting/eviction, controllers and normal cleanup. |
| 13-assertion final cold companion | Two actual nodes, held authenticated transfer, real Create/workload deadline and recovery. |
| 18-assertion focused lifecycle companion | Genuine orphan Exec retention, failed-install recovery, exact signal/exit behavior and reversal. |
| Separate returning-host/capacity/uninstall runs | Physical filesystem return under a new Node UID, Pending capacity, and actual cleanup refusal. |
| Producer and native contracts | Maven/CLI/shim conformance and Linux filesystem, mount, lock and cache invariants. |

Each selected-suite receipt deliberately retains `fullAcceptance:false`; no
single suite is presented as proof of the entire proposal. Failed attempts
remain failed and separate from their corrected repeats.

## Reproduce component checks

```sh
make test
make build
make acceptance
```

The strict acceptance gate checks verified evidence records. It does not create
a cluster, rerun every receipt or certify an arbitrary deployment. Native kernel
tests require the documented isolated Linux privileges; a portable skipped
kernel case must not be called real-node proof.

## What these results do not claim

- A stable production release or available 2.0 installer.
- Executed EKS/Karpenter/AKS provider scale-from-zero.
- Physical alternate-CPU node joins; final local physical execution is arm64.
- Better throughput, memory use or startup time than conventional containers.
- Shared application processes/heaps or automatic JVM/interpreter tuning.
- Final-application attestation/Ratify parity from layer-set signing alone.
- Compatibility with old descriptors, artifact formats or workload APIs.

Cross-builds cover Linux amd64 and arm64. The current real-node GitHub Actions
workflow is manually dispatched; scheduling an orphan-branch workflow does not
create a default-branch nightly guarantee.

## Interpreting failures

`RuntimeImageInstalling` means Create waited for a cold validated runtime;
`RuntimeImageInstallFailed` means the installation failed. Neither authorizes
running workload code before publication.
`RuntimeImageRevoked`, `RuntimeImageAuthorizationExpired`,
`RuntimeImageEpochMismatch` and `RuntimeImageUpdating` are deliberate fail-closed
boundaries. The operational response is to restore valid authority/provisioning
or let a workload owner perform an appropriate native rollout, not bypass the
barrier.

If cleanup is blocked, inspect its original Node/profile/inventory identity,
durable receipt and actual task/mount references. An attempted removal is not
proof of absence. Preserve the diagnostics and follow the
[cleanup contract](operations.md#cleanup-and-revocation).
