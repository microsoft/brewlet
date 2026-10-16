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
