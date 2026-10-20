# Proposal 0009 acceptance ledger

`requirements.json` tracks normative requirements throughout the proposal, not
only the nine scenarios in section 13.1. `referenceMain` pins the contract.
Related requirements are grouped without deleting their security or execution
conditions. `evidence` records exact commands, outcomes, and the proof scope.

```sh
python3 acceptance/check.py
python3 -m unittest discover -s acceptance -p '*_test.py' -v
python3 acceptance/check.py --require-complete
```

The first command validates the ledger's structure, **not** implementation
completeness. The final command fails until every requirement is verified with
successful evidence. Unit tests do not establish real-node scheduling,
containerd annotation forwarding, provider autoscaling, runtime installation,
CRI disk accounting/eviction or runc execution. A passing build is not a release
acceptance result.

## Proposal 0010

`composition-requirements.json` independently maps 29 grouped normative
obligations across Proposal 0010 to contract, producer, documentation and native
matrix checks. It does not reuse Proposal 0009's successful receipts.

```sh
python3 acceptance/composition_check.py
make composition-acceptance
```

The strict target requires the four-language managed lifecycle and an additional
Wasmtime/Spin composition companion on both native architectures, normal cleanup
and successful public receipts. Four-language or old runtime-only Wasm receipts
cannot qualify the missing companion.
Unknown, partial, failed or cross-built results do not qualify an architecture.
The top-level `make acceptance` runs both ledgers.
