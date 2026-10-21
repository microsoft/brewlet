# Proposal 0009 acceptance ledger

`requirements.json` tracks current evidence bindings for normative requirements throughout the proposal, not
only the nine scenarios in section 13.1. Original declarations remain unchanged
under each row's `historicalEvidence`; they do not qualify the current candidate.
All current statuses remain pending until genuine execution is reviewed.
`referenceMain` pins the contract.
Related requirements are grouped without deleting their security or execution
conditions. `evidence` records exact commands, outcomes, and the proof scope.

```sh
python3 acceptance/check.py --structure-only
python3 -m unittest discover -s acceptance -p '*_test.py' -v
python3 acceptance/check.py --require-complete
```

The first command validates ledger structure and current mappings, **not** implementation
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
The active composition checks now reference the fixed non-live command receipts
and `candidate-ARCH-managed.json` / `candidate-ARCH-managed-wasm.json` suite receipts, with outcomes pending
until actual qualification. `historicalChecks` preserves the original execution
declarations unchanged and is not consulted by the gate. Updating only a pending
outcome cannot qualify a missing, stale, failed or assertion-incomplete receipt.

## Candidate identity and evidence-only commits

Qualification requires a clean committed checkout. Receipts record its SHA,
`identityVersion`, `productionSourceManifestSHA256`, and
`qualificationInputsSHA256`. The shared `evidence.py` algorithm hashes committed
path/mode/content identities; the harness also checks the actual isolated
production build context. Tests, fixtures, workflows, pins, contracts, legal
inputs, and the validators themselves are qualification inputs.
Committed production inputs and the copied live Docker context use the same
canonical manifest byte encoder: full POSIX string keys sorted lexically,
with the existing indented JSON format. Path-component traversal order is not
canonical (`cmd/brewlet` and `cmd/brewlet-node` sort differently). The actual
context must still equal the immutable production hash exactly before any
component image is built; alternate encodings/hashes never qualify.

Only `acceptance/results/*.json`, `acceptance/RESULTS.md`, baseline ledger
status and evidence outcome/result metadata, and composition check outcome/result metadata are
excluded or normalized. Obligation text, composition check mappings/commands,
the matrix, and validation policy are not excluded. An evidence-only descendant
may attach results for the source ancestor if both hashes still match. Missing
history or changed inputs fail closed. CI fetches full history for this check.
Historical receipts are not rewritten or assigned identities they never ran.
Historical metadata is not evidence-only mutable: changing `historicalEvidence`
invalidates the candidate input hash. Only the active evidence outcome/result
and row status fields are normalized.

Non-live execution evidence also needs persistent assertion records, matching
scope and candidate identity. Inspection cannot substitute for mandatory live
execution. `python3 acceptance/run_check.py CHECK` executes one of these fixed
checked-in commands, never a ledger-supplied command string:

| Check | Scope | Public receipt |
| --- | --- | --- |
| `go-contracts` | Go race/unit/contract suite, not privileged kernel proof | `go-contracts.json` |
| `maven` | Actual optional producer verification | `maven.json` |
| `documentation` | Strict documentation build and site contracts | `documentation.json` |
| `fixtures` | Native four-language host probes, real CLI/Maven packaging and shared conformance | `fixtures-ARCH.json` |
| `registry` | Actual owned local registry, signed provenance, exact compressed descriptors and diff IDs | `registry-ARCH.json` |
| `native-contracts` | Native Linux artifact/shim/node/provisioner/worker tests, with no skipped tests | `native-contracts-ARCH.json` |
| `go-vet` | Native Linux static checks including architecture-specific source | `go-vet-ARCH.json` |
| `site-integration` | Real browser interactions and documentation navigation | `site-integration.json` |

`python3 acceptance/run_check.py all` runs all eight checks, retains failed
outcomes (including missing tools and input changes), and exits nonzero if any
command fails. These commands require a clean committed source candidate;
build `bin/brewlet` with `make build` and install the pinned documentation/browser
dependencies first, as the non-live workflow does. The registry fixture explicitly
uses the actual native runner architecture, not its old amd64 default.
The shared native contract runner checks the Go target and Docker architecture,
uses the existing pinned userland and capability/mount restrictions, and removes
only exact invocation-owned containers and its unchanged owned image tag.
Routine CI uses the same kernel-test implementation without importing receipts.

Run non-live qualification on both native Linux architectures: four receipts
per architecture plus four architecture-independent receipts make twelve distinct
required non-live filenames. Review duplicated common receipts and retain
failed attempts, rather than claiming every workflow attempt succeeded.
The real API admission contract is executed inside the complete native harness,
not inferred from unit tests or from these non-live receipts.

`python3 acceptance/reconcile.py` checks the reviewed command/scope/architecture/
suite bindings for all 115 obligations without executing anything.
`python3 acceptance/reconcile.py --write` is a **source-preparation operation**:
it preserves original history, installs those mappings, and resets active
statuses/outcomes to pending. Use it only before freezing inputs, never to import
receipts or claim qualification. Native live bindings enforce the named suite's
exact scenario/workloads as well as architecture and ordinary receipt checks.
Update any command/scope mapping in the source candidate before freezing it,
then attach only reviewed outcome/result metadata after execution.
A receipt is reviewable evidence, not a cryptographic third-party
attestation: provenance fields alone do not authenticate an untrusted author.
Review the actual workflow execution and public artifacts before importing
receipts. No import or live dispatch is automatic.
Runners refuse to overwrite existing receipt filenames before executing new
commands. Preserve prior attempts under distinct reviewed filenames/locations
first; failed or interrupted runs are never silently replaced.

## Fresh native qualification

The current source has **no qualifying fresh complete evidence**. The old
verified ledger statuses are historical declarations, not a green release gate.
`make test` checks structure and offline correctness; `make acceptance` is
expected to fail until current evidence is obtained and mapped.

After explicit approval of privileged live execution, dispatch `e2e.yml` on the
immutable corrected candidate branch with `mode=native` (the default). Six scopes are required: `complete`,
`managed`, and `managed-wasm` on each of `amd64` and `arm64`. This workflow is
manual only, uses native hosted runners, has read-only repository permission,
and rejects fork repositories/non-branch refs. It does not execute untrusted
PR code using elevated repository credentials.
Every mode also requires `expected_sha` containing the exact authorized
40-character commit. The first job step rejects a moved dispatch branch or
incorrect SHA before checkout, tool installation or fixture execution. Checking
the branch before dispatch alone is not sufficient to close that race.

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
`python3 acceptance/qualify.py --status-json` reports all 22 expected filenames
as `missing`, `failed` (execution or validation), or `qualified`, together with
the current candidate identity and each available receipt's identity. It exits
nonzero unless every receipt qualifies. A failed tutorial or interrupted suite
launch also leaves a failed public receipt; unexecuted later suites remain
missing rather than acquiring invented results.
An unavailable runner, skipped suite, failed cleanup or missing proof is a
blocker, never a cross-build or success-shaped fallback. Native kind capacity
does not prove actual cloud-provider scale-from-zero. A provider-qualified
claim needs separate provider evidence; do not invent that claim or make it a
prerequisite for obligations explicitly scoped to native pending-capacity tests.

## Routine validation versus promotion qualification

The registered E2E workflow provides two additional explicit, manual modes on
the selected branch, so evidence qualification does not depend on registering
a new workflow on the default branch or invoking a publication operation:

| Mode | Execution | Evidence and limits |
| --- | --- | --- |
| `native` | Existing selected native scenario/architecture | Privileged disposable owned fixtures; six dispatches for all 22 suite receipts |
| `non-live` | `make build`, then `python3 acceptance/run_check.py all` | Fixed contract/producer/registry/browser checks on each native architecture; includes elevated Docker kernel tests but no node provisioning |
| `verify-only` | `make acceptance` | Strict read-only evidence validation from the immutable full-history checkout; no live fixtures or publication |

All modes reject unsupported execution contexts and retain read-only repository
permissions. Non-live/native artifacts contain only public JSON from
`acceptance-export/`. Verify-only does not import artifacts or fabricate missing
evidence; attach reviewed genuine receipts and reconciled metadata in a clean
evidence-only descendant before dispatching it. Approve the exact branch/SHA,
mode and inputs separately before remote execution. Source or qualification-input
changes require refreezing and fresh evidence; do not dispatch a moving branch
and assume an earlier SHA ran. Inspect each workflow run's actual `head_sha`.
For final verify-only execution, pass the exact evidence-only descendant SHA as
`expected_sha`, not the ancestor source SHA. Its normalized input hashes must
still match the frozen source, and the source receipt checkout must be its
ancestor. If that descendant SHA was not included in the authorization, request
approval for the exact verification dispatch separately.

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

The manual `release.yml` prerelease publisher implements this dependency:
ordinary CI and strict qualification must both succeed before read-only
artifact/evidence preparation, and every publishing surface is behind those
same successful prerequisites plus preparation. There are no tag-push triggers.
The [prerelease runbook](../docs/supported-release.md) describes additional
exact-distribution evidence, legal/credential blockers and authorization limits.
This does not authorize external/manual publication or replace fresh candidate
qualification.

Workflow changes are qualification inputs, so this separation does not
legitimize historical receipts: fresh evidence must match the corrected
candidate. The old acceptance check's lingering `IN_PROGRESS` record despite
its completed failed execution is a separate GitHub status inconsistency,
not something this routing change claims to repair.

Main branch protection is unchanged and remains an explicit user-deferred
promotion risk. This work does not apply a ruleset or require review approvals.
