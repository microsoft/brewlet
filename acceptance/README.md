# Validation and promotion evidence

## Two different gates

| Gate | Execution | Meaning |
| --- | --- | --- |
| `Brewlet validation` in `v2-ci.yml` | Contracts, native producer/kernel checks, and a real four-language Kubernetes E2E | Required PR validation, not full promotion qualification |
| `Brewlet promotion qualification` | All current indexed receipts, all 115 baseline and 29 composition obligations, all 22 native suites | Full immutable-candidate qualification before promotion |

Green unit/contract checks alone cannot make `Brewlet validation` green. When
E2E is in scope its aggregate requires the real E2E job to succeed; failed,
skipped or cancelled prerequisites fail the aggregate. E2E starts as soon as the
CI context is validated, in parallel with the other prerequisites.

The only E2E opt-out is a pull request whose exact head commit changes nothing
but documentation or workflow files: `docs/`, `site/`, `release-notes/`, any
`*.md`, or `.github/` other than `e2e.yml` and `v2-ci.yml`. The `context` job
lists the PR's files (including rename sources) from the API and fails closed:
listing errors, a moved head, 3000 or more files, pushes to `main` and manual
runs all require E2E. When E2E is out of scope, the aggregate requires it to be
skipped, never failed or cancelled.
Fork PRs cannot enter the trusted privileged runner: a maintainer must explicitly
test the commit through a trusted repository branch. This is a failure, not a
skipped green check. No `pull_request_target` or write credentials are used.

`e2e.yml` is reusable by CI and manually dispatchable. It runs real E2E only:
there are no non-live or verify-only modes in that workflow.

## Establish a working E2E baseline before full qualification

The `pr` scenario runs all four native languages, real API admission and startup
fencing, application HTTP/exec/identity checks, rotations, observability, operator
restart/durable generations, actual protected CRI identity and normal cleanup.
It includes the protected-identity phase; `control` does not. Use `pr` on one
native architecture when debugging that path instead of restarting the entire
qualification matrix.

The PR verifier requires actual successful startup/rotation/protected-identity
assertions for **each** language, successful production cleanup and exact-owned
fixture destruction. It rejects reduced language selection and a claim of full
promotion acceptance. A PR receipt cannot substitute for a complete receipt.
Long quarantine/accounting/controller fault scenarios and companion suites stay
in full qualification. Source repairs need ordinary validation and a new source
identity, but do not justify repeating the full qualification matrix while the
live harness is still being debugged.

Before any remote privileged execution, agree the exact source SHA, scenario,
architecture, resource scope, cleanup and runner budget. Dispatch `e2e.yml` with
`expected_sha`, `scenario=pr` and one architecture for an approved debugging run.
Every manual scenario rejects a moved ref before checkout or setup.
PR CI checks out the immutable PR **head**, not an unrecorded moving branch.
No work is authorized merely by these instructions.

## Validation receipts are artifacts, not repository contents

Routine CI executes each fixed non-live check below once and uploads its
sanitized JSON:

| CI job | Fixed checks |
| --- | --- |
| contracts | `go-contracts`, `maven` |
| producer integration, both native architectures | `fixtures`, `registry` |
| Linux kernel contracts, both native architectures | `native-contracts`, `go-vet` |

The producer commands, scopes and assertion mappings remain fixed in
`run_check.py`; ledger-supplied strings are never executed. The shared native
runner still rejects emulation and skipped kernel tests. Website dependencies,
and build stay in the separate `site-pages.yml` workflow, not routine CI or its
required validation aggregate. Manual website preview runs produce the
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
artifacts; CI grants `contents: read` and `actions: read`, not write access.
GitHub only permits downloads before an artifact expires
([artifact access and expiration](https://docs.github.com/en/actions/how-tos/manage-workflow-runs/download-workflow-artifacts)).
Missing access, expired/deleted artifacts and transport failures are errors.
An index, checksum, run URL or green status without the actual assertion bytes
never qualifies anything. A verified local byte cache can be checked offline;
a fresh CI checkout cannot fall back to metadata when retrieval fails.

The manual/reusable promotion gate does exactly this with read-only repository
and artifact permissions. It requires the exact approved index-descendant
`expected_sha` and full Git history. Its caller must depend on successful
qualification; it never provisions nodes, publishes, merges or regenerates
missing evidence. Final verification requires separate execution approval.

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
verified local migration backup. The first approved CI run uploads a separate
byte-verified historical archive; until that run is verified, remote archival is
**pending**, not claimed complete. To verify and record its artifact:

```sh
python3 acceptance/evidence_index.py record-history --run CI_RUN_ID
```

This checks every original byte and changes only archival metadata; history is
never loaded as current qualification. CI avoids repeating the archive once its
verified run/artifact IDs are indexed. `archive-history` is also an explicit CI
dispatch purpose and cannot emit a green `Brewlet validation` check.

Current/debug artifacts retain the existing 14-day retention; the one-time
historical archive uses 90 days. Actions artifacts are **not permanent storage**:
expired or unavailable current evidence fails retrieval, never turns into a
success-shaped fallback. Retain a verified local copy when longer audit
retention is needed; no new cloud storage or publication is configured here.
Checksums and provenance metadata are review aids, not third-party attestations:
review the workflow and actual execution before accepting an index.

`check.py --structure-only`, `composition_check.py --structure-only` and
`reconcile.py` remain source-preparation checks, not execution proof.
Do not run `reconcile.py --write` after freezing inputs or use it to claim success.
See [current status and historical provenance](RESULTS.md).
