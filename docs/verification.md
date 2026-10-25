# Verification and limitations

The authoritative records are the
[requirements ledger](https://github.com/microsoft/brewlet/blob/main/acceptance/requirements.json)
and [exact commands, sources and scoped results](https://github.com/microsoft/brewlet/blob/main/acceptance/RESULTS.md).

## Current candidate: acceptance blocked

The ledger's verified statuses and the results below are historical declarations,
not qualification of the corrected candidate. Fresh complete, managed-native,
and managed-Wasm evidence is still required on **both native amd64 and arm64**.
No live qualification was dispatched for the P1 remediation.

Strict acceptance now opens every mandatory execution receipt, checks assertion
coverage and scope, requires successful cleanup, and compares immutable checkout,
production-source and qualification-input identities. Evidence-only descendant
commits can attach results without changing the qualified inputs. See the
[candidate qualification procedure](https://github.com/microsoft/brewlet/blob/main/acceptance/README.md).
Native capacity does not prove cloud-provider scale-from-zero. Main protection
remains deferred at the user's direction.

## Routine CI is not promotion qualification

The ordinary `Brewlet validation` check runs on PRs and pushes to `main`.
It must pass its correctness, producer, native-overlay and packaging checks, but
does not require promotion receipts for every development change. Its real
E2E starts immediately in parallel with those checks. Pull requests run it
against the Python fixture only, for speed; pushes to `main`, which gate
releases, run all four languages. Pull requests that change only documentation
or workflow files skip it.

Strict `make acceptance` runs separately in
[`promotion-qualification.yml`](https://github.com/microsoft/brewlet/blob/main/.github/workflows/promotion-qualification.yml),
which is manual or explicitly reusable. Whenever invoked, its
`Brewlet promotion qualification` job must fail for missing or stale evidence
bound to that immutable candidate. It does not execute live scenarios.

Any future authorized promotion/publishing job must depend on that reusable
qualification job and require its successful result. There is no publishing
workflow in this branch, so this interface does not enforce external/manual
promotion, add release triggers or grant publishing authority. See the
[caller contract](https://github.com/microsoft/brewlet/blob/main/acceptance/README.md#routine-validation-versus-promotion-qualification).
Live execution still needs separate approval. The old lingering acceptance
check record is a separate GitHub status inconsistency, not a claimed fix here.

## Historical local implementation evidence

| Evidence | Scope |
| --- | --- |
| 111 reconciled requirements | Original 101, six upstream amendment obligations, flat launch schema and three explicit Wasm qualification obligations. |
| 82-assertion amended full-userland run | Four languages, delivery, rotation, CRI accounting/eviction, controllers, invalid-PATH Eager failure and normal cleanup. |
| 13-assertion final cold companion | Two actual nodes, held authenticated transfer, real Create/workload deadline and recovery. |
| 18-assertion focused lifecycle companion | Genuine orphan Exec retention, failed-install recovery, exact signal/exit behavior and reversal. |
| Separate returning-host/capacity/uninstall runs | Physical filesystem return under a new Node UID, Pending capacity, and actual cleanup refusal. |
| Producer and native contracts | Maven/CLI/shim conformance and Linux filesystem, mount, lock and cache invariants. |
| 28-assertion unchanged-upstream companion | Pinned shell-free Java/Python/Node/static roots, real source rotation, numeric pathless execution, isolated `/app`, held Exec across worker/daemon restarts, revocation and normal cleanup on arm64. |
| 18-assertion Wasm/runc companion | Wasmtime command and Spin SDK HTTP component, independent layer-set composition, explicit guest access, separate engine cache, exact exit 37, host-version updates, real Exec recovery and normal cleanup on arm64. |
| Separate 32-assertion managed-composition runs | Java/Python/Node/Canonical .NET unchanged roots on native arm64 and amd64, independently signed complete sets, preserved app/launch bytes, candidate compatibility checks, controlled native rolling updates, observed package/SBOM export and normal cleanup. |
| Separate 18-assertion managed Wasm runs | Signed external asset composition/replacement for Wasmtime and Spin on native arm64 and amd64, actual guest hashes/behavior, candidate checks, native Deployment rollout, observed package/SBOM export and normal cleanup. |

The upstream-runtime amendment adds six obligations to the historical
101-requirement baseline. Its source pins are recorded in the
[upstream matrix](https://github.com/microsoft/brewlet/blob/main/integration-tests/fixtures/runtimes/upstream.json).
The original full-userland results alone do not verify the amended image-shape
rules; the fresh amended full-userland regression has its own successful
receipt and normal production-cleanup evidence.

The [historical Wasm receipt](https://github.com/microsoft/brewlet/blob/4c93db4e6c4affd3a1f2deb6a5099fffda3de8e5/acceptance/results/wasm-runc-arm64.json) is separate from
the four-language/unchanged-runtime layer-set matrix. Its platform-built
runtime images preserve pinned engine binary bytes on a pinned distroless C/C++
base; they are not unchanged upstream engine OCI images. The current schema
also passed 69 shared validator cases through CLI/Maven and the native shim,
plus 34 applicable cases through real Maven publication. Old `requires` forms
are deliberately invalid; archived receipts and the pinned comparison remain
historical rather than being rewritten as new-format proof.

Each selected-suite receipt deliberately retains `fullAcceptance:false`; no
single suite is presented as proof of the entire proposal. Failed attempts
remain failed and separate from their corrected repeats.

Composition (specification §16) has a separate
[29-obligation checklist](../acceptance/composition-requirements.json) and
[historical native arm64 receipt](https://github.com/microsoft/brewlet/blob/4c93db4e6c4affd3a1f2deb6a5099fffda3de8e5/acceptance/results/managed-composition-arm64.json)
and [historical native amd64 receipt](https://github.com/microsoft/brewlet/blob/4c93db4e6c4affd3a1f2deb6a5099fffda3de8e5/acceptance/results/managed-composition-amd64.json).
Current candidate evidence is separately bound and verified through the
[artifact index](../acceptance/README.md#index-retrieve-and-verify-genuine-evidence).
It does not reuse the 111-obligation baseline as composition acceptance.
Native amd64 qualification passed the separate manually dispatched `managed`
[E2E run](https://github.com/microsoft/brewlet/actions/runs/39445945006);
a cross-build is not that proof. The matrix updates explicit local
fixture libraries, not an actual vulnerable production package, and contains
no built-in CVE scanner.
The additional [managed Wasm companion](webassembly.md#governed-external-dependency-replacement)
is mandatory and separately scoped; the older runtime-only Wasm receipt and
four-language passes cannot qualify it. Both named hosts passed locally on
arm64 and in the separate [native amd64 run](https://github.com/microsoft/brewlet/actions/runs/39454221579).

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

The recorded exact-command guide passed locally on a fresh arm64 node:
real HTTP and numeric identity, old-binding retention during source rotation,
a new Pod/generation with the same application digest, normal profile/host
cleanup, Helm uninstall and exact-owned node destruction. Its
[historical public receipt](https://github.com/microsoft/brewlet/blob/4c93db4e6c4affd3a1f2deb6a5099fffda3de8e5/acceptance/results/reader-tutorial-arm64.json) preserves
the observed bindings, input hashes and the separate failed development attempts.
The complete manually dispatched E2E workflow also selects this reader check.
Source, webhook and client-environment failures were corrected in the guide;
no image policy or runtime protection was relaxed to obtain a pass.

The strict acceptance gate checks current candidate evidence records. It does not create
a cluster, rerun every receipt or certify an arbitrary deployment. Native kernel
tests require the documented isolated Linux privileges; a portable skipped
kernel case must not be called real-node proof.

## What these results do not claim

- A stable production release or available 1.0.0 installer.
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
