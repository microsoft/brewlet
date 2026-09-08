# Brewlet E2E runbook

## Reliable invocation

Run against the monorepo checkout:

```bash
integration-tests/e2e/run.sh --reset
integration-tests/e2e/run.sh
```

The harness does not switch branches or modify component sources. It uses
`core/` and `kubernetes/` by default. Override `BREWLET_CORE_DIR` or
`BREWLET_KUBERNETES_DIR` only when testing an external checkout.

## Prerequisites

| Tool | Tiers |
|---|---|
| Go | all |
| Python 3 | 2, 4, 12, 13, 16 |
| JDK 21+ | 2, 3, 7, 8, 9, 12, 14, 15, 16 |
| Docker | 3, 6, 7, 8-12, 14, 15, 16 |
| kubectl and a reachable cluster | 4-16 |
| Helm | 4 (optional), 10, 15 |
| OpenSSL | 4, 5, 6, 10, 11 |

Host-only tiers 1-3 need no cluster. Tiers 4-7 and 13 exercise API-server
behavior. Tiers 6, 8-12, 14, 15, and 16 require local containerd nodes that
Docker can enter, such as kind. Managed clusters skip those node-side paths. Tier 13
also proves the control-plane guard: a NodeProfile that does not set
`nodePool.includeControlPlane` never counts or schedules onto a control-plane
node. Because kind and Docker Desktop label their single node as the control
plane, the node-side tiers set that opt-in explicitly. Tier 14
installs explicit custom JDK and `jaz` sources and runs a live workload through
both. Tier 10 also exercises cert-manager issuance and certificate hot reload
when cert-manager is installed. Tier 15 installs the
chart with metrics enabled, provisions one node through the real DaemonSet,
launches a Brewlet workload, and scrapes all metrics surfaces. Tier 16 covers
the SPECIFICATION §14 failure contract on a live node: a missing/unauthorized
image surfacing as an ImagePull failure, a JVM OOM exiting under
`-XX:+ExitOnOutOfMemoryError` and being restarted by the kubelet, and an
unusable shim surfacing as a containerd task failure — a `FailedCreatePodSandBox`
warning event, since a missing shim fails sandbox creation before any container
exists, or a container create/run error on runtimes that fail later — rather than
a silently non-running pod. It moves the node's shim aside for the last case and restores
it both inline and from its cleanup trap. §14's remaining row, the cgroup-v1
refusal, cannot be produced on a cgroup-v2 CI node and is covered
deterministically by `provisioner/entrypoint_test.sh` over `require_cgroup_v2`.

**Architecture coverage.** Tiers pick up the node's architecture automatically,
but hosted runners are amd64, so `.github/workflows/e2e.yml` runs the host-only
tiers 1-3 a second time on an `ubuntu-24.04-arm` runner. That is the only path
that actually executes the CLI and the shim-to-runc assembly on arm64; the
multi-arch index writer and the strict platform matcher are additionally
unit-tested in `core/internal/artifact`.

## Cleanup and diagnostics

Tiers 4 and 13 use invocation-unique, reserved-domain provisioner images that
must never execute. Their fixture teardown stops and waits for the test manager,
checks worker identity and execution history, and waits for foreground worker
deletion before releasing exact fixture-owned claims/finalizers. This is a
test-only abort, not proof of production host cleanup. Dirty or foreign workers
leave ownership intact and fail the tier.

Run the fixture safeguards independently with:

```bash
PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover \
  -s integration-tests/e2e -p 'nodeprofile_fixtures_test.py' -v
```

Run `./e2e/run.sh --reset` before repeating Kubernetes tiers. It removes only
Brewlet-owned labels, annotations, CRDs, runtime classes, webhook configuration,
RBAC, and fixed test namespaces. It does not delete application workloads outside
the harness namespaces.

Generated artifacts and diagnostic logs are written beneath the printed work
directory. Set `E2E_WORK` to retain them at a known path. For rollout failures,
read `diag-*.log` first.

Common environment-specific skips:

- Tier 5 skips when a cluster cannot reach a host-bound webhook; tier 6 covers
  the same assertions in-cluster.
- Tiers 8, 9, 12, 14, 15, and 16 skip if no schedulable local containerd node
  can be provisioned.
- Tier 12 skips when the node's `ctr` supports neither `images unpack` nor
  import-time unpack; tier 16 applies the same rule.

Specifications belong in `specs/`, and general project documentation belongs in
[`docs/`](../docs/), not this directory.
