# Validation status

**Full promotion qualification remains unproven.** No current-candidate bundle
is indexed. Green unit/contract checks, partial live assertions and historical
declarations do not satisfy the 115 baseline obligations, 29 composition
obligations or 22-suite native matrix.

The PR gate includes real four-language application execution,
rotations, protected CRI identity and normal production cleanup. It is distinct
from full promotion qualification. Actual execution and current run URLs are reported by the required GitHub
`Brewlet validation` check, not by updates to this versioned report. Offline
regression results are not that proof.

## Historical evidence

Generated execution payloads no longer belong in the working tree.
[`evidence-index.json`](evidence-index.json) records all 180 original public
files by immutable commit, original path, size and SHA-256. Ninety are from the
five stopped rounds below; ninety predate this remediation.

Original bytes, identities and outcomes were preserved and byte-verified before
removal. They remain in Git history at
[`4c93db4e`](https://github.com/microsoft/brewlet/tree/4c93db4e6c4affd3a1f2deb6a5099fffda3de8e5/acceptance/results)
and a verified local migration backup. The
[original detailed historical report](https://github.com/microsoft/brewlet/blob/4c93db4e6c4affd3a1f2deb6a5099fffda3de8e5/acceptance/RESULTS.md)
retains its scope limitations and declarations without relabeling.

Archival state and run/artifact IDs are recorded in the index's `history`
section: a null archive run means pending. The archive job
reconstructs only the indexed public JSON and verifies every byte before upload.
The archival index is recorded only after the uploaded archive is downloaded
and verified. History is never substituted for current evidence.
See [the artifact and retention procedure](README.md#identities-history-and-retention).

| Source | Non-live runs, passed | Native complete runs, failed | Stopping problem |
| --- | --- | --- | --- |
| `56dccc25` | [amd64](https://github.com/microsoft/brewlet/actions/runs/39673571516), [arm64](https://github.com/microsoft/brewlet/actions/runs/39673721658) | [amd64](https://github.com/microsoft/brewlet/actions/runs/39675536254), [arm64](https://github.com/microsoft/brewlet/actions/runs/39675599495) | Context-manifest serialization order |
| `f6e70022` | [amd64](https://github.com/microsoft/brewlet/actions/runs/39697436850), [arm64](https://github.com/microsoft/brewlet/actions/runs/39697469935) | [amd64](https://github.com/microsoft/brewlet/actions/runs/39698207309), [arm64](https://github.com/microsoft/brewlet/actions/runs/39698248714) | Conflicting invalid-profile deletion lifecycle |
| `59a5f411` | [amd64](https://github.com/microsoft/brewlet/actions/runs/39707690863), [arm64](https://github.com/microsoft/brewlet/actions/runs/39707701073) | [amd64](https://github.com/microsoft/brewlet/actions/runs/39708311390), [arm64](https://github.com/microsoft/brewlet/actions/runs/39708330433) | Held-profile cleanup and masked-error reporting |
| `c6059fd2` | [amd64](https://github.com/microsoft/brewlet/actions/runs/39782287929), [arm64](https://github.com/microsoft/brewlet/actions/runs/39782309953) | [amd64](https://github.com/microsoft/brewlet/actions/runs/39783732807), [arm64](https://github.com/microsoft/brewlet/actions/runs/39783747912) | Post-delete metadata/provisioning-generation equality |
| `0abd6122` | [amd64](https://github.com/microsoft/brewlet/actions/runs/39792970699), [arm64](https://github.com/microsoft/brewlet/actions/runs/39793003919) | [amd64](https://github.com/microsoft/brewlet/actions/runs/39795134330), [arm64](https://github.com/microsoft/brewlet/actions/runs/39795166884) | Executing the identity helper from node `noexec` tmpfs |

Each stopped round retains sixteen non-live success files and two failed core
files. Its other twenty native files were not produced; zero native suites
qualified. Managed/managed-Wasm rows never ran, and no attempts were retried.
The latest pair retained 45 actual passing assertions per architecture, including
four-language execution/rotations and the repaired admission lifecycle, but
neither complete run finished.

All failed rounds passed exact-owned fixture destruction, explicitly **not
production cleanup evidence**. Their production cleanup remains unproven.
No old receipt can qualify changed source/validation inputs. No private/raw
observations are added to the historical archive.
