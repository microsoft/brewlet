# Brewlet metrics exporter

The node-local exporter receives bounded, best-effort telemetry from the
containerd shim over `/opt/brewlet/metrics/telemetry.sock` and exposes
Prometheus metrics for sandbox launches, artifact resolution, AppCDS decisions,
installed JDK and launcher inventory, and runnable-image staging usage.

`brewlet_runnable_stage_bytes` reports logical regular-file bytes under
`--stage-root` (default: `BREWLET_RUNNABLE_STAGE`, or `/tmp/brewlet-runnable`
on Linux regardless of `TMPDIR`). The directory must be the host's actual staging
root; a containerized exporter needs a read-only host mount. The operator
provides that mount for the default location. The gauge includes non-evictable
unmanaged layouts and pending trees, does not follow symlinks, and refreshes on every scrape.
It is not a measure of allocated disk space, free space, or reclaimable bytes.
Inspection errors fail the scrape instead of reporting zero.

On Helm-managed nodes, the provisioner already schedules stage GC by default,
independently of whether this exporter is enabled. The exporter never deletes
stages. Do not add a second timer: it would run outside the operator's
`stageGC.enabled` setting and installation safety gate.

To inspect eligibility on a managed Linux node, run the installed helper as
root in the host namespaces:

```sh
sudo /usr/local/bin/brewlet-stage-gc stage-gc --dry-run
```

Manual invocations retain the reaper's reference and mount checks but do not
consult the provisioner's installation safety record. There is no acknowledgment
override, and manual deletion must not bypass blocked automatic GC.
Outside the provisioner, install the standalone `brewlet` CLI and arrange a
host timer only after establishing the same consumer-safety prerequisites.
See [stage cleanup](../../../docs/runnable-image.md#reclaiming-unused-stages)
for reference checks and recovery precautions.

## Screenshots

These images were captured from Grafana 12.1.0 backed by Prometheus 3.5.0. The
metric samples came from a live metrics-enabled tier 15 integration test and
are stored with the exporter for reuse by the documentation site.

### Grafana runtime dashboard

![Brewlet runtime metrics dashboard](brewlet-metrics-dashboard.png)

### Grafana Explore

![Brewlet metrics in Grafana Explore](brewlet-grafana-explore.png)
