# Observability and metrics

Observe three different things: application health, the node's actual runtime
binding, and control-plane convergence. No single ready label proves all three.

## Logs and actual bindings

Use ordinary application stdout/stderr and Kubernetes logs. Correlate Pod UID,
container ID, RuntimeImage UID/generation/digest, Node UID and profile UID.
Operator and worker logs explain approval, source acquisition, fencing and
cleanup; application logs explain application behavior.

`brewlet.sh/runtime-bindings` on a Node is a complete/freshness-marked
observation with `nodeUID`, `profileUID`, `profileGeneration`, `observedAt`,
`complete` and `bindings`. Each row identifies the actual Pod/container and
leased runtime. Check completeness and freshness before using it for diagnosis.
It is not authority to delete roots.

## Enable and scrape

Set `metrics.enabled: true` in the same reviewed Helm values used for installation.
The chart creates:

| Service | Listener | Role |
| --- | --- | --- |
| `brewlet-operator-metrics` | 8080 | Rotation, workload restart and profile observations. |
| `brewlet-node-metrics` | `metrics.nodePort` (9090 by default) | Node-local generation, storage, install and binding metrics. |

Both carry Prometheus scrape annotations. Configure your collector to honor
them or author reviewed discovery/scrape configuration. The node Service is
headless and publishes not-ready addresses; scraping it does not assert readiness.
The current chart does not bundle a ServiceMonitor or Grafana dashboard switch.

For local inspection, run in a dedicated terminal:

```sh
kubectl -n brewlet port-forward service/brewlet-operator-metrics 18081:8080
```

Then fetch `http://127.0.0.1:18081/metrics`. Restrict endpoint access using
platform network policy; do not expose inventory metrics publicly by default.

## Metric catalog

| Family | Important labels | Interpretation |
| --- | --- | --- |
| `brewlet_runtime_image_install_requests_total` | `id`, `outcome` | Installation request outcomes. |
| `brewlet_runtime_image_install_duration_seconds` | `id` | Install/validation histogram, not application startup latency. |
| `brewlet_runtime_image_generations` | `id`, `state` | Installed generations by state. |
| `brewlet_shared_storage_bytes` | `store` | Logical runtime/layer storage bytes, not guaranteed physical disk savings. |
| `brewlet_container_runtime_image_info` | `namespace`, `pod`, `pod_uid`, `container`, `container_id`, `id`, `inventory_uid`, `generation`, `digest` | Actual live container/runtime binding, value 1. |
| `brewlet_runtime_image_rotation_complete` | `id`, `inventory_uid`, `generation`, `digest` | Node-target convergence, not completed workload remediation. |
| `brewlet_runtime_image_restarts` | `id`, `inventory_uid`, `state` | Current target restart outcome counts. |
| `brewlet_runtime_image_restart_info` | workload identity, epoch/generation, `state` | Per-workload restart observation. |
| `brewlet_node_profile_nodes` | `profile`, `state` | Assigned/effectively ready target observations. |
| `brewlet_runtime_image_excluded_nodes` | `id`, `inventory_uid`, `reason` | Rotation exclusions, including fence failures. |

Collection errors are surfaced as scrape/metric errors, not fabricated zero
inventory. Watch scrape health as well as business metrics.

## PromQL starting points

```promql
sum by (id, outcome) (rate(brewlet_runtime_image_install_requests_total[5m]))
```

```promql
histogram_quantile(0.95,
  sum by (le, id) (rate(brewlet_runtime_image_install_duration_seconds_bucket[5m])))
```

```promql
count by (id, inventory_uid, generation, digest) (brewlet_container_runtime_image_info)
```

```promql
sum by (id, state) (brewlet_runtime_image_restarts)
```

Use these as dashboard panels alongside scrape health, excluded-node reasons,
application error rate and native rollout availability. Review aggregation:
the same Pod/container binding is a high-cardinality operational identity, not
a label to copy into every application metric.

Do not copy main's JDK metric names or assume its dashboard is compatible.
See [instrumentation recipes](instrumentation.md) and
[failure diagnosis](troubleshooting.md).
