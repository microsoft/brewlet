# Brewlet 2.0 reference

The implementation contract is
[`specs/SPECIFICATION.md`](specs/SPECIFICATION.md), which consolidates the
approved Proposals 0009 and 0010. Its design began from `microsoft/brewlet` main commit
`fa7085335b220e29df6affbece4558c5f1259ecb`.

This branch was created with an empty Git index using `git switch --orphan
brewlet-2.0`. It has no implementation ancestry. Brewlet 0.x is design
inspiration only: its specification, capability labels and runbooks are not
carried in this branch, and `SPEC §N` citations refer to them at the commit
above. `specs/SPECIFICATION.md` is the authoritative contract.

No compatibility with previous artifacts, workload APIs, or launch descriptors
is intended.
