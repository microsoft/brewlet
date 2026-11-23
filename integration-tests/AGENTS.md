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
  `complete` and focused suites) and the `wasm.py` companion.
- `fixtures/` — the Python qualification fixture, Java/Node.js/.NET/PHP example
  fixtures (weekly matrix `language-e2e.yml` workflow, which also runs `wasm.py`), pinned runtime inputs and
  `fixtures/check.py`, the producer/consumer conformance check.
- `live-v2/image_volumes.py` — isolated native Kubernetes alternative, without
  Brewlet components. Covers delivery, rotation and rollback; an admission-policy
  runtime catalog with Python, Java and Node; Eager/OnDemand placement; ABI
  mismatches; and restart authorization. See
  [report and reproduction](../docs/image-volumes.md).
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
impacts runtime, fixture or harness code, and for Release Gate Tests. Its own
weekly schedule and manual dispatch default to `complete` on both native
architectures via `acceptance/qualify.py`. Final promotion evidence verification
is separate (see `acceptance/README.md`).

## Rules

- Mandatory assertions fail; they never skip. Keep component-test, scoped
  live-run and full-qualification claims distinct.
- Generated receipts are Actions artifacts, not source files; export them only
  through `acceptance/export_result.py`.
- Specifications belong in `specs/`, and project documentation in
  [`docs/`](../docs/), not this directory.
