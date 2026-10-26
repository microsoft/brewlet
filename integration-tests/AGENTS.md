# Brewlet integration tests

Current Brewlet is a rewrite; 0.x E2E tiers, `core/` paths and JDK-specific
runbooks do not apply to this branch. The contract is the
[Brewlet specification](../specs/SPECIFICATION.md);
obligations and evidence rules live in [`acceptance/`](../acceptance/README.md).

## Layout

- `live-v2/` — private real-node acceptance. `harness.py` creates its own
  invocation-unique kind cluster, kubeconfig, registry and checkout-built
  images; it never uses an existing cluster or context. See
  [`live-v2/README.md`](live-v2/README.md) for scenarios (`pr`, `control`,
  `complete` and focused suites) and `wasm.py`/`managed_wasm.py` companions.
- `fixtures/` — four-language application fixtures, pinned runtime inputs and
  `fixtures/check.py`, the producer/consumer conformance check.
- `registry.py` — real registry delivery checks.
- `admission.py` — real API-server admission checks; not node execution.
- `linux-cli.sh` — sandboxed Linux CLI wrapper used by the fixture and registry
  checks for cross-architecture evidence.
- `maven_conformance.py` — Maven producer conformance used by `fixtures/check.py`.

## Running

Offline Python tests (also part of `make test`):

```sh
make test-python
```

Producer checks, as CI runs them after `make build`:

```sh
python3 acceptance/run_check.py fixtures
python3 acceptance/run_check.py registry
```

Real-node suites, from the repository root with a new work directory under
`live-v2/`:

```sh
PYTHONDONTWRITEBYTECODE=1 python3 integration-tests/live-v2/harness.py \
  --scenario pr --work integration-tests/live-v2/run-pr-01
```

`.github/workflows/e2e.yml` runs the `pr` scenario for PR validation when a PR
impacts runtime, fixture or harness code, and for weekly/release full validation; full promotion
qualification runs through `acceptance/qualify.py` and the promotion
qualification workflow.

## Rules

- Mandatory assertions fail; they never skip. Keep component-test, scoped
  live-run and full-qualification claims distinct.
- Generated receipts are Actions artifacts, not source files; export them only
  through `acceptance/export_result.py`.
- Specifications belong in `specs/`, and project documentation in
  [`docs/`](../docs/), not this directory.
