# Verification and limitations

The authoritative records are the
[requirements ledger](https://github.com/microsoft/brewlet/blob/brewlet-2.0/acceptance/requirements.json)
and [exact commands, sources and scoped results](https://github.com/microsoft/brewlet/blob/brewlet-2.0/acceptance/RESULTS.md).

## Local implementation evidence

| Evidence | Scope |
| --- | --- |
| 107 reconciled requirements | Original 101 plus six approved amendment obligations, with changed image-shape rules reverified. |
| 82-assertion amended full-userland run | Four languages, delivery, rotation, CRI accounting/eviction, controllers, invalid-PATH Eager failure and normal cleanup. |
| 13-assertion final cold companion | Two actual nodes, held authenticated transfer, real Create/workload deadline and recovery. |
| 18-assertion focused lifecycle companion | Genuine orphan Exec retention, failed-install recovery, exact signal/exit behavior and reversal. |
| Separate returning-host/capacity/uninstall runs | Physical filesystem return under a new Node UID, Pending capacity, and actual cleanup refusal. |
| Producer and native contracts | Maven/CLI/shim conformance and Linux filesystem, mount, lock and cache invariants. |
| 28-assertion unchanged-upstream companion | Pinned shell-free Java/Python/Node/static roots, real source rotation, numeric pathless execution, isolated `/app`, held Exec across worker/daemon restarts, revocation and normal cleanup on arm64. |

The upstream-runtime amendment adds six obligations to the historical
101-requirement baseline. Its source pins are recorded in the
[upstream matrix](https://github.com/microsoft/brewlet/blob/brewlet-2.0/integration-tests/fixtures/runtimes/upstream.json).
The original full-userland results alone do not verify the amended image-shape
rules; the fresh amended full-userland regression has its own successful
receipt and normal production-cleanup evidence.

Each selected-suite receipt deliberately retains `fullAcceptance:false`; no
single suite is presented as proof of the entire proposal. Failed attempts
remain failed and separate from their corrected repeats.

## Reproduce component checks

```sh
make test
make build
make acceptance
```

The reader tutorial has separate documentation checks:

```sh
python3 site/scripts/tutorial_check.py
```

This default checks shell/step structure only. Explicit `--run` executes the
exact guide blocks on one new private kind cluster, including HTTP, binding
identity, source rotation, unchanged app digest and normal cleanup. It requires
the tutorial prerequisites and is not the complete qualification suite.

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

`RuntimePATHRequired` means a relative executable was requested without an
approved runtime PATH. Use the runtime's documented absolute executable path
in the application descriptor or the Exec/probe command; do not add a Pod PATH
override or an inferred shell/entrypoint.

If cleanup is blocked, inspect its original Node/profile/inventory identity,
durable receipt and actual task/mount references. An attempted removal is not
proof of absence. Preserve the diagnostics and follow the
[cleanup contract](operations.md#cleanup-and-revocation).
