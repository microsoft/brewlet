# Validation and promotion evidence

## Validation gates

| Gate | Workflow and trigger | Execution | Meaning |
| --- | --- | --- | --- |
| `PR validation` | `pr.yml`, pull requests only | Only the checks, CodeQL languages, real E2E and website build impacted by the PR's changed files; native checks and E2E on amd64 | Fast required PR validation |
| `Release Gate Tests` | `release-gate-tests.yml`, weekly on `main`, manual dispatch, and called by every release | Every check, every CodeQL language, native producer/kernel checks and Python real E2E on both amd64 and arm64 | Complete end-to-end validation of one immutable commit; the release gate, not promotion qualification |
| `Language Example E2E` | `language-e2e.yml`, weekly on `main` and manual dispatch | A matrix job per fixture (Python, Java, Node.js, .NET, PHP, WebAssembly) running its real-node runtime contract and rotation (Java adds the Maven producer; WebAssembly runs the `wasm.py` Wasmtime/Spin examples) | Non-gating weekly coverage; never qualification evidence |
| `Promotion qualification` | Local `make acceptance` after `evidence_index.py fetch` (no workflow) | All current indexed receipts, all 115 baseline and 29 composition obligations, all 22 native suites | Optional full immutable-candidate qualification before promotion |

Both validation workflows reuse the same building blocks: `checks.yml`
(contracts, Maven, offline Python, legal, producer and kernel checks),
`codeql.yml`, `e2e.yml` and, for pull requests, `site-pages.yml`.

Green unit/contract checks alone cannot make either aggregate green. The Release Gate Tests
aggregate requires every prerequisite to succeed; failed, skipped or
cancelled prerequisites fail it. A manual Release Gate Tests dispatch takes no inputs
and validates the head of the selected branch. The PR aggregate requires every impacted check
to succeed and every other check to be skipped, never failed or cancelled.

The PR `context` job lists the PR's files (including rename sources) from the
API and classifies them. It fails closed to every check: listing errors, a moved
head, 3000 or more files, an empty list, unknown paths, and changes to `pr.yml`,
`checks.yml`, `e2e.yml`, `codeql.yml`, `Makefile` or `acceptance/` all select
everything. Documentation-only changes run at most the website build; runtime
source selects Go, legal, producer, kernel and E2E checks; CodeQL analyzes only
the languages of the changed files. PR E2E always uses only the Python fixture.
Java fixture changes run the producer check (the Maven producer packages it);
Node.js, .NET and PHP fixture changes are covered by the weekly
`language-e2e.yml` workflow, which can also be dispatched on the PR branch.
Fork PRs cannot enter the trusted privileged runner: a maintainer must explicitly
test the commit through a trusted repository branch. This is a failure, not a
skipped green check. No `pull_request_target` or write credentials are used.

`e2e.yml` is reusable by both validation workflows and manually dispatchable. It runs real E2E only:
there are no non-live or verify-only modes in that workflow.

## Establish a working E2E baseline before full qualification

The `pr` scenario runs the Python qualification fixture, real API admission and startup
fencing, application HTTP/exec/identity checks, rotations, observability, operator
restart/durable generations, actual protected CRI identity and normal cleanup.
It includes the protected-identity phase; `control` does not. Use `pr` on one
native architecture when debugging that path instead of restarting the entire
qualification matrix.

The PR verifier requires actual successful startup/rotation/protected-identity
assertions for the Python fixture, successful production cleanup and exact-owned
fixture destruction. It rejects any other language selection and a claim of full
promotion acceptance. A PR receipt cannot substitute for a complete receipt.
Long quarantine/accounting/controller fault scenarios and companion suites stay
in full qualification. Source repairs need ordinary validation and a new source
identity, but do not justify repeating the full qualification matrix while the
live harness is still being debugged.

Before any remote privileged execution, agree the exact source SHA, scenario,
architecture, resource scope, cleanup and runner budget. Dispatch `e2e.yml` with
`expected_sha`, `scenario=pr` and one architecture for an approved debugging run.
Every manual scenario rejects a moved ref before checkout or setup.
PR validation checks out the immutable PR **head**, not an unrecorded moving branch.
No work is authorized merely by these instructions.

## Validation receipts are artifacts, not repository contents

`checks.yml` executes each fixed non-live check below once and uploads its
sanitized JSON. Only artifacts from `release-gate-tests.yml` runs (scheduled or
manually dispatched) can be indexed as qualification receipts:

| `checks.yml` job | Fixed checks |
| --- | --- |
| contracts | `go-contracts`, `maven` |
| producer integration, native amd64 (pull requests) or both native architectures (Release Gate Tests) | `fixtures`, `registry` |
| Linux kernel contracts, native amd64 (pull requests) or both native architectures (Release Gate Tests) | `native-contracts`, `go-vet` |

The producer commands, scopes and assertion mappings remain fixed in
`run_check.py`; ledger-supplied strings are never executed. The shared native
runner still rejects emulation and skipped kernel tests. Website dependencies,
and build stay in the separate `site-pages.yml` workflow, which pull requests call
only when the website is impacted; it is not part of Release Gate Tests. Manual website preview runs produce the
`documentation` receipt for full qualification;
Pages deployment continues to reuse the checked website build.

Generated receipts belong in Actions artifacts or the ignored local
`acceptance/results/` cache. Git retains only `evidence-index.json`, the normative
binding ledgers and explicit synthetic fixtures under `testdata/`.
The index records immutable source identities, run/artifact IDs and file
checksums. It contains no executable commands or replacement test policy.
PR artifacts and failed/debug artifacts are not promotion evidence.

## Full native qualification

After the baseline is stable and inputs are frozen, the full requirement remains
11 suites per architecture on native Linux amd64 and arm64:

`core`, `cold`, `tutorial`, `wasm`, `upstream`, `native`, `uninstall`,
`retirement`, `activation`, `managed`, `managed-wasm`.

Six manual native scopes supply the 22 receipts: `complete`, `managed` and
`managed-wasm` on each architecture. `complete` expands into the first nine.
Use the already completed CI run for ten distinct non-live filenames, plus a
manual `site-pages.yml` run at the same immutable source for the two website
receipts. These runs do not need repeating before every live debugging attempt.
Review each scope's actual inputs, source, native runner, assertions and cleanup.
Stop on failure for review; do not retry or change inputs under old authorization.
No source inspection, cross-build, partial pass or runner disposal substitutes
for actual execution and production cleanup.

## Index, retrieve and verify genuine evidence

Only after reviewing successful immutable workflow executions, use a clean
checkout with an empty receipt cache and a read-only `GITHUB_TOKEN`:

```sh
python3 acceptance/evidence_index.py record-candidate \
  --run CI_RUN_ID --run WEBSITE_RUN_ID --run AMD64_COMPLETE --run ARM64_COMPLETE \
  --run AMD64_MANAGED --run ARM64_MANAGED \
  --run AMD64_MANAGED_WASM --run ARM64_MANAGED_WASM
```

Supply the CI run, standalone website run and all six native scope runs. The
command fetches only the
expected public artifact groups from this repository, checks successful run
event/workflow/source and artifact ownership/expiry, rejects unsafe ZIP paths,
verifies all current bindings, and records per-file checksums. It never runs
commands from the index and never overwrites an existing cache.

Commit the **index only** as an evidence-only descendant. Both input hashes must
remain equal to the frozen source. From that clean committed descendant:

```sh
make acceptance
python3 acceptance/check.py --require-complete
python3 acceptance/composition_check.py --require-complete
python3 acceptance/qualify.py
```

`make acceptance` requests verification of every immutable binding in memory;
source `pending` labels are not manually rewritten into evidence. The existing
validators still require actual successful assertion coverage, exact fixed
commands/scopes/workloads/native architectures, authenticated TLS core delivery,
production cleanup, fixture destruction and matching source/input identities.
Any missing, stale, failed, intervened or incomplete payload fails.

A fresh checkout retrieves the reviewed index's artifacts explicitly:

```sh
python3 acceptance/evidence_index.py fetch
make acceptance
```

Retrieval requires repository access and permission to read its Actions
artifacts; a read-only `GITHUB_TOKEN` with `contents: read` and `actions: read` suffices.
GitHub only permits downloads before an artifact expires
([artifact access and expiration](https://docs.github.com/en/actions/how-tos/manage-workflow-runs/download-workflow-artifacts)).
Missing access, expired/deleted artifacts and transport failures are errors.
An index, checksum, run URL or green status without the actual assertion bytes
never qualifies anything. A verified local byte cache can be checked offline;
a fresh checkout cannot fall back to metadata when retrieval fails.

There is no promotion-qualification workflow. Run these commands locally from a
clean, full-history checkout of the exact approved index descendant; they never
provision nodes, publish, merge or regenerate missing evidence. Releases do not
depend on this step: every release runs Release Gate Tests, including the Python
real-node E2E on amd64 and arm64.

## Identities, history and retention

`identityVersion`, `productionSourceManifestSHA256` and
`qualificationInputsSHA256` still bind committed bytes and modes, including
tests, workflows, pins, obligation mappings and validation policy. The copied
production context must equal the same canonical string-key-sorted manifest.
Alternate hashes do not qualify. Changed inputs invalidate old receipts.

Only result JSON, `RESULTS.md`, the data-only evidence index and active
status/outcome/result metadata are evidence-only. Historical ledger declarations
remain unchanged inputs. An evidence-only descendant must retain matching
hashes and contain the receipt source in its Git ancestry.

The index's `history` section pins the original 180 public files by Git commit,
path, byte count and SHA-256. Their original bytes remain in Git history and a
verified local migration backup. Their one-time archive was uploaded and recorded
by the predecessor `v2-ci.yml` workflow; history is never loaded as current
qualification, and no current workflow re-archives it.

Current/debug artifacts retain the existing 14-day retention. Actions artifacts are **not permanent storage**:
expired or unavailable current evidence fails retrieval, never turns into a
success-shaped fallback. Retain a verified local copy when longer audit
retention is needed; no new cloud storage or publication is configured here.
Checksums and provenance metadata are review aids, not third-party attestations:
review the workflow and actual execution before accepting an index.

`check.py --structure-only`, `composition_check.py --structure-only` and
`reconcile.py` remain source-preparation checks, not execution proof.
Do not run `reconcile.py --write` after freezing inputs or use it to claim success.
See [current status and historical provenance](RESULTS.md).
