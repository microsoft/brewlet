# Brewlet 2.0 node execution

The Linux binaries are built from the repository root:

```sh
go build ./cmd/brewlet-node
go build ./shim/cmd/containerd-shim-brewlet-v2
```

`brewlet-node` uses the uncached Kubernetes API client and the UID-bound
`NodeProfile` authority ledger. It freezes the initially observed Node UID,
requires literal `BREWLET_PROFILE_UID`, `BREWLET_PROFILE_GENERATION`,
`BREWLET_PROFILE_NAME`, `BREWLET_MODE`, and JSON `BREWLET_TARGET_UIDS`, and reads
`NODE_NAME`. `SOURCE_ALLOWED_MIRROR_HOSTS` is a JSON array. Source and mirror
validation is shared with Kubernetes policy.
Matching CRDs, operator, admission, and worker builds are required. Before
minting authorization, the worker requires an unresolved
`RuntimeImage.status.revocationTargets` row for this Node UID/profile UID with
the exact allocated generation and digest. Missing or pruned ledger fields fail
closed. Pending valid rotations preserve bounded existing authorization; they
never install a new source under an old generation.

Before baseline readiness, the worker installs its bundled shim and configures
containerd config version 2 or 3. A root-owned `.host` transaction records the
original configuration, desired bytes/digests, profile/Node UIDs, and
Prepared/Applied/Committed phases before changing host files. Validated policy
restarts containerd only after syntax validation and commits only after live
CRI handler/annotation checks. Failure rolls back while authority remains
valid; lost authority leaves the journal for the authorized successor.
Cleanup drains Brewlet tasks (including infrastructure sandboxes), restores the
original configuration, removes only the owned shim, restarts/read-checks when
recorded policy requires it, and archives a Restored receipt. A later `none`
rollout does not erase the prior transaction's reversal obligation.
Owned shim upgrades retain the original cleanup backup and a prior-committed
binary rollback target. A failed upgrade restores the preceding committed
binary instead of uninstalling a previously healthy handler.

The host must already provide `containerd`, `systemctl`, and `crictl`;
preflight checks all three before installation. Unmanaged shim binaries/handlers
are not adopted, and external changes block reversal rather than being
overwritten. Both image bases are digest-pinned.

The operator mounts the node at `/host`, plus `/var/lib/containerd` and
`/run/containerd` at their original paths. The default containerd socket and
`overlayfs` snapshotter are currently required. Host inventory lives under
`/opt/brewlet/runtimes`; immutable generations contain `rootfs` and
`metadata.json`. `current` is a relative epoch/generation symlink. Root-owned
`authorization.json` records are separate, expire after five minutes, and are
renewed only from fresh approval and ownership reads. A single cross-process
writer lock prevents competing provisioners. Per-ID locks serialize current
selection, leases, generation publication, and collection.
Actual unknown/unapproved entries receive no authorization or capability and
do not block approved siblings. Baseline readiness requires the known approved
Eager roots, not an impossible install of an unknown ID; the controller still
reports the profile's `UnknownRuntimeImage` condition. Unverified API lookups,
approved Eager failures, and stale provider-pinned capabilities remain blocking.

`/brewlet-node --health-check` reads the pod-local protected completion marker
`/run/brewlet/ready` without API or host mutation. The worker removes the marker
on startup/failure/authority loss and writes it only after confirmed baseline
or cleanup completion. Successful cleanup stays alive until authorized teardown.

Only an uninstalled, currently authorized OnDemand runtime creates an empty
`.requests/<id>` marker. Create waits at most thirty seconds, including lock
contention. Failed installs fail Create without issuing a new request.
Installed runtimes rotate normally, without installing cold OnDemand entries.
Publication checks fresh cluster authority and the exact target again after
validation and immediately before changing `current`.
Idle-TTL reclamation checks all durable leases and initial-host-namespace mount
evidence before removing current and returning authorization to `available`.
Generation collection runs conservatively after recovery, retaining current,
leased, mounted, young, or uncertain generations.
The grace period starts at retirement, not the original installation date.
Layer-cache collection uses an exclusive cross-process guard shared by Create,
immutable publication receipts, durable per-task cache references, complete
cross-namespace mount evidence, and a positive age floor. Pending creates or
ambiguous evidence prevent deletion. Staged runtime roots carry ownership and
snapshot-view records; fresh authority loss cancels and joins acquisition,
leaving private data for authorized recovery.

Revocation first persists the local authorization tombstone and confirms
capability withdrawal or a node-wide fence. After all roots drain, the
UID/generation-bound `brewlet.sh/runtime-revocations` receipt is published and
read back; root removal does not discard the revocation tombstone. Cleanup uses
the installed shim's maintenance entrypoint entered through `nsenter`, rather
than relaxing initial-host namespace evidence requirements inside the worker.

Set `BREWLET_METRICS_ADDRESS` (for example `:9090`) to expose install request
outcomes/durations, current/retired generation counts, live task/runtime
bindings, and separate shared runtime/layer logical-byte metrics. Inspection
errors fail a scrape rather than report zero. Runtime starts and bounded waits
also publish structured containerd events on `/brewlet/runtime-image`.
The protected `brewlet.sh/runtime-bindings` Node annotation carries complete,
UID-bound observations derived from actual containerd tasks, durable runtime
leases, and protected CRI Pod/container identities. These are diagnostics, never
cleanup authority or inferred runtime generations. Inspection failures retain
the prior observation; snapshots are never truncated, and unchanged snapshots
have a bounded heartbeat.

The shim registers containerd's real runc-backed Runtime v2 task service,
decorating Create, Exec, and Delete. Application resolution uses protected CRI
metadata, never compatibility hints. Verified gzip/zstd payload layers use
digest/policy-keyed persistent cache entries under `/var/lib/brewlet/layers`.
The root overlay uses reverse manifest order over the leased runtime and reuses
CRI-managed overlay upper/work directories. Packaged `/app` is self-bound
read-only; explicit Kubernetes volume mounts follow it. Namespaces, resources,
argv, cwd, terminal settings, and ordinary runc lifecycle are retained.
Missing child mountpoints under `/app` are prepared in the managed per-container
upper before the read-only bind, without changing immutable cached bytes. The
snapshot remains writable for runc's mount setup; the original OCI
`Root.Readonly` still controls the final root remount. Host-only runtime,
authorization, lease, request, and cache state cannot be exposed through
workload bind mounts, including kubelet subPath aliases.

Initial effective identity, environment, leased runtime, and cache roots are
persisted before delegating Create. Exec rewrites the actual RPC process using
this protected state; it does not merely edit the original bundle. Missing
state fails closed. Durable leases survive restart. Recovery and retirement
require complete containerd task/exec and cross-process mount evidence;
unreadable or noninitial namespaces block collection rather than discard leases.

## Current limits and unverified requirements

This is not yet a complete node-runtime acceptance implementation:

* Activation/rollback and reversal are covered by fault-injection unit tests.
  Private-kind integration additionally verified automatic initial
  installation/configuration/restart, live handler forwarding, cold-only
  readiness without fetching a root, real Python Create/Exec/HTTP, final
  read-only root and `/app`, a writable volume absent from packaged layers,
  runtime PATH/HOME, and no capabilities/privilege escalation. Upgrade,
  rollback, and reversal still need the full live acceptance suite. Systemd
  hosts with conventional config/socket paths are required.
* Custom containerd roots, sockets, snapshotters, and runtime platform variants
  are unsupported. The selected architecture is checked; coverage of every
  architecture across a pool is not established by the node worker.
* Idle-TTL, generation, and layer-cache sweeping are wired and unit-tested, but
  still need full real-node lease/mount/rotation validation.
* Cache parents are protected, but cache corruption/restart durability needs
  additional acceptance coverage. The artifact package's native Linux tests
  cover real overlay whiteout representations. Layer composition semantics
  remain governed by the shared artifact materializer/validator.
* OnDemand capability publication is followed by a real removal/readback
  mutability probe once per worker lifetime; provider refusals retain the
  node-wide fence. Fake-client refusal tests exercise the confirmed fence;
  real provider/interceptor integration validation remains necessary.
  Exclusion-aware all-ID startup recovery and Kubernetes-native event mirroring
  still need completion. Prometheus/containerd telemetry needs real-node
  scraping/event validation.
* The complete four-language rotation/revocation/recovery/cleanup matrix and
  telemetry scraping remain acceptance work. Application metadata/packed
  layers must remain available in containerd's content store; cache reuse does
  not reconstruct missing launch/config blobs. CRI filesystem usage and eviction
  conformance remain unverified until the live suite proves them; reusing
  managed upper/work paths alone is not accounting evidence.

Portable validation is `go test -race ./pkg/node ./provisioner`.
Linux builds must cover both `linux/amd64` and `linux/arm64`. Shim RPC tests are
Linux unit tests, not E2E tests.

### Acceptance ledger mapping

The focused portable/native contracts cover the node portions of `INV-04`,
`INV-06`, `ROOT-02`, `ROOT-03`, `LEASE-01`–`LEASE-03`, `OD-02`–`OD-06`,
`ROT-07`, `PROC-01`–`PROC-03`, and `SEC-02`–`SEC-04`.
`PROF-01` is exercised by
`TestUnknownEagerDoesNotBlockApprovedEntriesFakeAPI` and
`TestUnverifiedLookupAndApprovedEagerFailureStillGateBaselineFakeAPI`; these
prove API decisions, not real scheduling. Native overlay/RPC tests additionally
exercise the node portions of `RUN-06`, `RUN-07`, and `RUN-09`.

`RUN-02`–`RUN-05`, `OWN-01`–`OWN-02`, and `OBS-01`–`OBS-03` integrate shared
artifact/control code and require the live suite for complete production
evidence. `ROOT-01` remains limited by the platform restrictions above.
`RUN-08` is not considered verified without actual CRI usage and eviction
evidence. The authoritative cross-component results are tracked in
[`acceptance/requirements.json`](../acceptance/requirements.json); implementation
or unit-test presence alone is not an E2E pass.
