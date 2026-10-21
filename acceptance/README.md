# Proposal 0009 acceptance ledger

`requirements.json` tracks historical declarations about normative requirements throughout the proposal, not
only the nine scenarios in section 13.1. `referenceMain` pins the contract.
Related requirements are grouped without deleting their security or execution
conditions. `evidence` records exact commands, outcomes, and the proof scope.

```sh
python3 acceptance/check.py --structure-only
python3 -m unittest discover -s acceptance -p '*_test.py' -v
python3 acceptance/check.py --require-complete
```

The first command validates historical ledger structure, **not** implementation
completeness or current verification. Without `--structure-only`, every passed
execution declaration must resolve to public, passing, assertion-covered
evidence bound to the candidate. The final command additionally requires every
obligation to be declared verified. Unit tests do not establish real-node scheduling,
containerd annotation forwarding, provider autoscaling, runtime installation,
CRI disk accounting/eviction or runc execution. A passing build is not a release
acceptance result.

## Proposal 0010

`composition-requirements.json` independently maps 29 grouped normative
obligations across Proposal 0010 to contract, producer, documentation and native
matrix checks. It does not reuse Proposal 0009's successful receipts.

```sh
python3 acceptance/composition_check.py --structure-only
make composition-acceptance
```

The strict target requires the four-language managed lifecycle and an additional
Wasmtime/Spin composition companion on both native architectures, normal cleanup
and successful public receipts. Four-language or old runtime-only Wasm receipts
cannot qualify the missing companion.
Unknown, partial, failed or cross-built results do not qualify an architecture.
The top-level `make acceptance` runs both ledgers.

## Candidate identity and evidence-only commits

Qualification requires a clean committed checkout. Receipts record its SHA,
`identityVersion`, `productionSourceManifestSHA256`, and
`qualificationInputsSHA256`. The shared `evidence.py` algorithm hashes committed
path/mode/content identities; the harness also checks the actual isolated
production build context. Tests, fixtures, workflows, pins, contracts, legal
inputs, and the validators themselves are qualification inputs.

Only `acceptance/results/*.json`, `acceptance/RESULTS.md`, baseline ledger
status and evidence outcome/result metadata, and composition check outcome/result metadata are
excluded or normalized. Obligation text, composition check mappings/commands,
the matrix, and validation policy are not excluded. An evidence-only descendant
may attach results for the source ancestor if both hashes still match. Missing
history or changed inputs fail closed. CI fetches full history for this check.
Historical receipts are not rewritten or assigned identities they never ran.

Non-live execution evidence also needs persistent assertion records, matching
scope and candidate identity. Inspection cannot substitute for mandatory live
execution. `python3 acceptance/run_check.py go-contracts|maven|documentation`
executes a fixed checked-in command and records its actual outcome under
`acceptance-export/`; it never executes ledger-supplied command strings.
Its mapping covers only the listed non-live obligations, not native overlay,
producer-registry, admission-cluster or real-node claims. Any other required
execution still needs its own appropriately scoped record. Update a ledger's
command/scope mapping in the source candidate before freezing it, then attach
the outcome/result metadata after execution. A receipt is reviewable evidence, not a cryptographic third-party
attestation: provenance fields alone do not authenticate an untrusted author.
Review the actual workflow execution and public artifacts before importing
receipts. No import or live dispatch is automatic.

## Fresh native qualification

The current source has **no qualifying fresh complete evidence**. The old
verified ledger statuses are historical declarations, not a green release gate.
`make test` checks structure and offline correctness; `make acceptance` is
expected to fail until current evidence is obtained and mapped.

After explicit approval of privileged live execution, dispatch `e2e.yml` on the
immutable corrected candidate branch. Six scopes are required: `complete`,
`managed`, and `managed-wasm` on each of `amd64` and `arm64`. This workflow is
manual only, uses native hosted runners, has read-only repository permission,
and rejects fork repositories/non-branch refs. It does not execute untrusted
PR code using elevated repository credentials.

The complete scope runs the core harness plus cold install, reader tutorial,
runtime Wasm, unchanged upstream sources, native capacity, pinned uninstall,
retirement, and activation. `control` remains a debugging scope and cannot
satisfy complete acceptance. The runner writes only sanitized public receipts
to ignored `acceptance-export/`; private work files are never uploaded.

Review and attach the resulting `candidate-ARCH-SUITE.json` files under
`acceptance/results/`, and update the corresponding ledger evidence references
in an evidence-only commit. `python3 acceptance/qualify.py` independently
requires every suite on both architectures for the same candidate. Both
ledger validators still require their own assertion/obligation coverage,
including successful production cleanup and fixture destruction.
An unavailable runner, skipped suite, failed cleanup or missing proof is a
blocker, never a cross-build or success-shaped fallback. Native kind capacity
does not prove actual cloud-provider scale-from-zero. A provider-qualified
claim needs separate provider evidence; do not invent that claim or make it a
prerequisite for obligations explicitly scoped to native pending-capacity tests.

## Routine validation versus promotion qualification

`v2-ci.yml` reports `Brewlet validation` on main/brewlet-2.0 pushes and PRs.
It covers ordinary correctness, producer integration, both native overlay
architectures and legal packaging. Its aggregate fails if any dependency fails,
cancels or skips; no path filter suppresses it. Missing promotion evidence does
not make ordinary development validation fail. Green routine CI is **not**
promotion qualification.

The separate `.github/workflows/promotion-qualification.yml` has only manual
`workflow_dispatch` and reusable `workflow_call` entrypoints. Select the
corrected candidate branch in the Actions UI when an authorized qualification
check is needed. The `Brewlet promotion qualification` job checks out the
invoking event's immutable SHA with full history and read-only repository
permission, then always runs `make acceptance`. Missing, stale, failed,
wrong-scope or incomplete evidence makes that workflow fail. It never creates
clusters, executes live scenarios, publishes a release or merges code.

A future authorized promotion/publishing workflow must call the reusable gate:

```yaml
jobs:
  qualification:
    permissions:
      contents: read
    uses: ./.github/workflows/promotion-qualification.yml
```

Its actual promotion job must declare `needs: [qualification]` and require
`${{ needs.qualification.result == 'success' }}`. Failed, cancelled, skipped
or missing qualification must not permit promotion. Do not use
`continue-on-error`, an opt-out input or a fallback success to bypass the gate.
Reusable calls retain the caller's original event and candidate SHA; they do
not require an event named `workflow_call`.

There is currently **no publishing workflow** in this branch to attach that
promotion job to. This reusable interface and caller contract do not enforce
external/manual promotion actions or introduce release/tag triggers. Those
actions still require their own authorization and integration.

Workflow changes are qualification inputs, so this separation does not
legitimize historical receipts: fresh evidence must match the corrected
candidate. The old acceptance check's lingering `IN_PROGRESS` record despite
its completed failed execution is a separate GitHub status inconsistency,
not something this routing change claims to repair.

Main branch protection is unchanged and remains an explicit user-deferred
promotion risk. This work does not apply a ruleset or require review approvals.
