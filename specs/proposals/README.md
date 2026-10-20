# Brewlet 2.0 design contracts

This directory contains the implemented design contracts for the Brewlet 2.0
branch. Implementation and qualification are distinct: consult the scoped
[validation results](../../acceptance/RESULTS.md) and acceptance ledgers rather
than interpreting a design document as proof of every deployment.

## Implemented contracts

- [0009 — Brewlet 2.0: language-agnostic runtime images](0009-language-agnostic-runtime-images.md)
  — application artifacts, approved runtime inventory, provisioning, admission,
  runc execution and lifecycle/security boundaries across all five workload
  categories. Qualification is tracked in the
  [baseline ledger](../../acceptance/requirements.json).
- [0010 — Governed dependency composition and remediation](0010-managed-dependency-composition.md)
  — producer/release composition, final-image evidence and observed dependency
  attribution for external tooling, not runtime dependency fetching or a
  built-in vulnerability scanner. Qualification is tracked in the
  [composition ledger](../../acceptance/composition-requirements.json).

## Deferred research

Stronger sandbox isolation and stock-runtime baked export remain possible
future directions, not supported alternatives or committed roadmap features.
Their boundaries are recorded briefly in
[0009's open questions](0009-language-agnostic-runtime-images.md#14-open-questions).

The earlier Java-oriented proposals are not carried as active 2.0 designs.
Proposal numbers are preserved; committed history retains the removed documents.
