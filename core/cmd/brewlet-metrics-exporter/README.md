# Brewlet metrics exporter

The node-local exporter receives bounded, best-effort telemetry from the
containerd shim over `/opt/brewlet/metrics/telemetry.sock` and exposes
Prometheus metrics for sandbox launches, artifact resolution, AppCDS decisions,
installed JDK and launcher inventory, and runnable-image staging usage.

`brewlet_runnable_stage_bytes` reports logical regular-file bytes under
`--stage-root` (default: `BREWLET_RUNNABLE_STAGE`, or
`os.TempDir()/brewlet-runnable`). The directory must be the host's actual staging
root; a containerized exporter needs a read-only host mount. The operator
provides that mount for the default location. The gauge includes legacy and
pending stages, does not follow symlinks, and refreshes on every scrape.
Inspection errors fail the scrape instead of reporting zero.

Use the host command `brewlet stage-gc --dry-run` to inspect reclaimable stages,
then schedule `brewlet stage-gc --min-age 24h` with a node-level timer.
See [stage cleanup](../../../docs/runnable-image.md#reclaiming-unused-stages)
for reference checks and rollout precautions.

## Screenshots

These images were captured from Grafana 12.1.0 backed by Prometheus 3.5.0. The
metric samples came from a live metrics-enabled tier 15 integration test and
are stored with the exporter for reuse by the documentation site.

### Grafana runtime dashboard

![Brewlet runtime metrics dashboard](brewlet-metrics-dashboard.png)

### Grafana Explore

![Brewlet metrics in Grafana Explore](brewlet-grafana-explore.png)
