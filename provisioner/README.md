# Brewlet 2.0 node execution

The Linux binaries are built from the repository root:

```sh
go build ./cmd/brewlet-node
go build ./shim/cmd/containerd-shim-brewlet-v2
```

Both production Docker builders keep Go compiler data in a reusable BuildKit
cache mount, outside source-specific build snapshots. This bounds repeated local
build growth without changing node runtime/cache storage or application
confinement. The operator and admission builds also use `-trimpath`.

`brewlet-node` uses the uncached Kubernetes API client and the UID-bound
`NodeProfile` authority ledger. It freezes the initially observed Node UID,
requires literal `BREWLET_PROFILE_UID`, `BREWLET_PROFILE_GENERATION`,
`BREWLET_PROFILE_NAME`, `BREWLET_MODE`, and JSON `BREWLET_TARGET_UIDS`, and reads
`NODE_NAME`. `SOURCE_ALLOWED_MIRROR_HOSTS` is a JSON array. Source and mirror
validation is shared with Kubernetes policy.
Mirror allowlists contain unique, nonempty registry authorities (optional
ports are supported), not URLs or repository paths. Mappings must target an
allowlisted host different from their source host. Admission, reconciliation
and the worker reject malformed or self-referential policies, including unused
invalid allowlist entries, before producing any pull reference.
Runtime platform coverage is checked by the node worker, not by registry I/O in
admission or the operator. The worker freshly lists known Nodes and valid active
profiles offering the ID, applies the existing named/default pool selectors,
and requires every offered Node's Linux OS/architecture. Missing or conflicting
Node platform information and API lookup errors fail closed.

Only the pinned OCI index, platform manifests and image configs are needed for
this check; runtime layers are never fetched/unpacked for a cold OnDemand
baseline. Descriptor digests/sizes and index/config declarations are verified.
Containerd normalization handles equivalent names/default variants (for example
`aarch64`, `arm64/v8`, and `amd64/v1`); a different non-default variant is never
silently selected. The selected actual manifest digest and normalized platform
are persisted with the runtime root, and snapshot diff IDs must match that
selected config. Future, nonexistent autoscaler Nodes cannot provide platform
evidence; their real platforms are checked when they join.
The shim independently uses containerd's detected native CPU variant for
application selection and leased-runtime checks. Host compatibility normalizes
equivalent default aliases; an index descriptor and its image config must still
declare identical platform fields. Ambiguous equivalent index entries fail.
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
sets `discard_unpacked_layers=false` at
`plugins."io.containerd.grpc.v1.cri".containerd` for version 2, or
`plugins."io.containerd.cri.v1.images"` for version 3. This mandatory packed-layer
retention prevents GC between CRI unpack and the first verified shim stage;
an unpacked snapshot or a warm cache is not a substitute for cold source bytes
(Proposal 0009 §8.2 / retained SPEC §6.4).
The transaction restarts containerd only after syntax validation and commits
only after effective-config retention and live CRI handler/annotation checks.
An exposed live CRI `discardUnpackedLayers` must also be false; its absence
does not supply an invented default or replace the effective-config check.
`none` cannot repair retention: missing or enabled discard fails closed without
editing the config, installing a shim or claiming readiness.
Failure rolls back while authority remains
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
Minimum-generation barriers use strict JSON with unique members and an exact
positive integer generation; duplicate IDs, epochs or generation members are
invalid rather than last-value-wins. Protected node records use the same shared
strict decoder, retaining the object-only boundary and rejecting ambiguous
nested members without converting generation counters through floating point.
New leases atomically bind the protected task bundle and its managed snapshot
upper/work paths. Recovery enumerates actual tasks and checks those task-specific
mounts rather than mistaking a healthy sibling's shared runtime mount for a
failed Create. Create/recovery locks prevent reaping before delegation completes.
Failed Create/Delete release remains evidence-gated; leases without task-specific
provenance retain conservative generation-wide protection.
Actual unknown/unapproved entries receive no authorization or capability and
do not block approved siblings. Baseline readiness requires the known approved
Eager roots, not an impossible install of an unknown ID; the controller still
reports the profile's `UnknownRuntimeImage` condition. Unverified API lookups,
approved Eager failures, and stale provider-pinned capabilities remain blocking.

Aggregate readiness is not sticky across required-work changes. Before acquiring
a required Eager root with no validated compatible `current`, the worker
withdraws a previously advertised baseline under fresh ownership checks and
confirms removal, or confirms the standard node-wide startup-fence fallback
when a provider pins that label. Required baseline/authorization checks and
failed Eager installation cannot leave the node effectively runtime-ready.
Compatible rotations retain their validated previous root while acquiring the
new target; an optional OnDemand install failure does not withdraw unrelated
baseline readiness. Readiness returns only through normal validated publication.

`/brewlet-node --health-check` reads the pod-local protected completion marker
`/run/brewlet/ready` without API or host mutation. The worker removes the marker
on startup/failure/authority loss and writes it only after confirmed baseline
or cleanup completion. Successful cleanup stays alive until authorized teardown.
Every successful provisioning reconcile refreshes the marker only after a fresh
ownership check. A failed reconcile removes it and never certifies readiness,
even when the worker still owns the node. Marker-removal failures are explicit
errors, not a successful health fallback. This pod health gate is separate from
application execution authorization and from nonfatal collection uncertainty.

Only an uninstalled, currently authorized OnDemand runtime creates an empty
`.requests/<id>` marker. Create waits at most thirty seconds, including lock
contention. Failed installs fail Create without issuing a new request.
When CRI forwards a shorter request deadline, the install wait reserves ten
percent of the remaining time for returning its explicit installation result
instead of consuming that deadline. Hosts must configure the kubelet runtime
request timeout above the default thirty-second install wait; the pinned local
kind fixture uses the Kubernetes default two-minute request timeout.
Admission and the shim share the same strict minimum-generation barrier parser,
including duplicate/escaped-member and exact-integer rejection.
An unauthorized ID's marker is removed and logged with the literal profile
UID/generation only after a fresh ownership check. Stale workers neither remove
nor log it as processed; successors revalidate requests rather than adopting
the previous profile's authority.
Installed runtimes rotate normally, without installing cold OnDemand entries.
Publication checks fresh cluster authority and the exact target again after
validation and immediately before changing `current`.
Idle-TTL reclamation checks all durable leases and initial-host-namespace mount
evidence before removing current and returning authorization to `available`.
Generation collection runs conservatively after recovery, retaining current,
leased, mounted, young, or uncertain generations.
`BREWLET_GC_GRACE` and `BREWLET_GC_SWEEP_INTERVAL` configure positive real
durations (defaults `1h` and `1m`). They do not change clocks or bypass evidence.
A fresh worker's first normal sweep runs fenced `Store.Recover` and, only after
successful recovery and fresh authority readback, emits
`runtime-lease-recovery: {"retainedTasks":[...],"removedTasks":[...],"complete":true}`.
Those sorted task IDs come from actual protected before/after records. This
proves lease recovery across a fresh worker process, not reconstruction of an
arbitrarily killed runc task-server process.
The grace period starts at retirement, not the original installation date.
Layer-cache collection uses an exclusive cross-process guard shared by Create,
immutable publication receipts, durable per-task cache references, complete
cross-namespace mount evidence, and a positive age floor. Pending creates or
ambiguous evidence prevent deletion. Bind-mount evidence resolves each protected
host path through the host mount table, then compares filesystem device and
filesystem-relative roots across process namespaces. This retains mounts of
cache or runtime data on separate filesystems even when their source path is
not printed as an absolute host path. Missing or malformed source mappings
block collection; equally specific stacked mounts are retained conservatively.
Unrelated namespace-filesystem roots such as `net:[inode]` are opaque kernel
identities, not malformed host paths. They do not block inspection of a different
filesystem; an ambiguous root on the protected filesystem still fails closed.
Staged runtime roots carry ownership and
snapshot-view records; fresh authority loss cancels and joins acquisition,
leaving private data for authorized recovery.
Completing acquisition also stops and joins its authority monitor. Cancellation
of an in-flight monitor read by that local shutdown is not an installation
failure; parent cancellation, monitor failures and all publication fences remain
authoritative.

### Recovery versus healthy rotation

Healthy rotation can continue advertising an older validated, compatible
`current` while the approved target advances. Recovery cannot: an excluded or
withdrawn ID must have fresh authority, a live local authorization, and validated
`current` matching the latest approved epoch, generation and digest before
`ready` is republished. A node-wide startup fence applies that requirement to
every offered/affected ID, including installed OnDemand IDs; recovery cannot
reclassify a formerly installed ID as cold or reclaim it through idle TTL to
bypass the check. A genuinely cold OnDemand entry can recover as `available`
with latest authorization without fetching a root.
The diagnostic inventory records `installed` independently of the advertised
approval generation. A cold `available` entry has the current approved generation
but `installed: false`; it is not a rotation participant. A validated current
publishes `installed: true`, and retained installed evidence keeps a retrying
OnDemand node in the participant set. Publication rejects a contradictory
installation flag rather than inferring root presence from a positive generation.
Recovery also retains explicit installed history when a failed authorization and
lost `current` pointer would otherwise make a formerly installed ID look cold.
A generation barrier on a genuinely uninstalled OnDemand ID still reports
`RuntimeImageInstalling` and uses the first-use marker. Only an older installed
current waits as `RuntimeImageUpdating`. Controller deadline diagnostics preserve
a never-started kubelet `StartError` while it is in restart backoff; stale errors
after actual execution do not turn application failures into deferred installs.

The worker checks all IDs before publication and repeats the checks after
advertisement readback, before removing the startup fence. Target changes in an
otherwise unaffected ID block release. Retained capability keys, values and
install modes must be valid and offered. Unknown, obsolete or nonoffered labels
are not silently deleted or normalized while fenced: actual pool correction is
required. OnDemand mutability is re-probed during recovery even if an earlier
healthy probe succeeded; provider refusal preserves its label and the confirmed
node-wide fence. Node readiness, schedulability, UID-bound ownership and current
profile authority must also pass.
Unexpected nonoffered capabilities on a previously healthy node trigger freshly
authorized withdrawal before host work. A real provider refusal must establish
and read back the node-wide startup fence; logging a publication error while
leaving the node eligible is not safe. Once fenced, stale provider evidence is
preserved until pool correction.

These recovery behaviors are implemented and covered by focused race/contract
tests, not yet proven by the complete live recovery matrix. They do not weaken
per-ID generation barriers or the requirement for genuine cleanup receipts.

Revocation first persists the local authorization tombstone and confirms
capability withdrawal or a node-wide fence. After all roots drain, the
UID/generation-bound `brewlet.sh/runtime-revocations` receipt is published and
read back; root removal does not discard the revocation tombstone. Cleanup uses
the installed shim's maintenance entrypoint entered through `nsenter`, rather
than relaxing initial-host namespace evidence requirements inside the worker.

Set `BREWLET_METRICS_ADDRESS` (for example `:9090`) to expose install request
outcomes/durations, current/retired generation counts, live task/runtime
bindings, and separate shared runtime/layer logical-byte metrics. The runtime
info gauge is one per actual container and includes protected namespace, Pod
name/UID, container name/ID plus ID/epoch/generation/digest labels, so vulnerable
generation observations can be joined to Pods. Inspection
errors fail a scrape rather than report zero. Runtime starts and bounded waits
also publish structured containerd events on `/brewlet/runtime-image`.
Initial application-state read failures during successful Start are explicitly
logged as runtime-image event failures; no fabricated resolved event or false
failed-start result replaces the actual runc outcome. Expected missing
application state is suppressed only when protected containerd metadata
identifies the infrastructure sandbox. Failed protected-kind lookups are
reported rather than silently treated as infrastructure.
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
* Custom containerd roots, sockets and snapshotters remain unsupported.
  Normalized platform declarations, strict variants and current known-pool
  architecture coverage are implemented; full live multi-architecture coverage
  still requires real Nodes of those architectures.
* Idle-TTL, generation, and layer-cache sweeping are wired and unit-tested, but
  still need full real-node lease/mount/rotation validation.
* Cache parents are protected, but cache corruption/restart durability needs
  additional acceptance coverage. The artifact package's native Linux tests
  cover real overlay whiteout representations. Layer composition semantics
  remain governed by the shared artifact materializer/validator.
* OnDemand capability publication is followed by a real removal/readback
  mutability probe, repeated during recovery; provider refusals retain the
  node-wide fence. Fake-client refusal tests exercise the confirmed fence;
  real provider/interceptor integration validation remains necessary.
  All-ID recovery is implemented as described above; its full live matrix
  remains acceptance work. The proposal requires shim/container events, emitted
  through containerd here, rather than prescribing Kubernetes Event mirroring.
  Workload restart Kubernetes events are emitted by the control plane.
  Prometheus/containerd telemetry needs real-node
  scraping/event validation.
* The shim retains original digest/size-verified index, manifest, config and
  compressed layer bytes in a protected, immutable, policy-bound cache.
  Later content removal permits reuse only when the source is genuinely absent;
  this is warm-reuse protection, not permission to enable unpacked-layer discard
  or a solution to the first-Create GC race. Corrupt, inaccessible
  or rejected source content never falls back. Resolution still validates the
  original graph, protected CRI identities, platform, launch/config mirror and
  payload diff IDs. No metadata is reconstructed and the shim never pulls.
  Durable task references protect retained originals alongside materialized
  layers; collection uses the exclusive cache guard, grace, complete mount
  evidence and fresh authority. Older task records without original-content
  references conservatively retain that cache. Cold-to-warm source-removal and
  corruption contracts pass; real-node content-GC repeat-start acceptance
  remains required.
* The complete four-language rotation/revocation/recovery/cleanup matrix and
  telemetry scraping remain acceptance work. A focused native-arm64 run
  verified actual CRI writable-layer growth and kubelet ephemeral-storage
  eviction, without an `emptyDir` proxy or kubelet configuration changes; see
  [the exact component-image evidence](../acceptance/results/cri-accounting.json).
  This is not a passing complete four-language rotation/cleanup suite.

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
evidence. `ROOT-01` metadata-only coverage/variant contracts are implemented and
unit-tested; complete live heterogeneous-pool evidence remains separate.
`RUN-08` is not considered verified without actual CRI usage and eviction
evidence. The authoritative cross-component results are tracked in
[`acceptance/requirements.json`](../acceptance/requirements.json); implementation
or unit-test presence alone is not an E2E pass.

`FENCE-04`/`ROT-05` recovery contracts are exercised by
`TestRecoveryQuarantineRejectsOldCurrentButHealthyRotationAllowsIt`,
`TestRecoveryAllIDsMustBeLatestBeforeNodeWideRelease`,
`TestRecoveryRechecksEveryTargetAfterAdvertisementReadback`,
`TestRecoveryPreservesStaleKeysAndProviderPinnedOnDemandEvidence`, and
`TestRecoveryRetainedInvalidAndUnknownCapabilitiesRequirePoolCorrection`.
These focused API/local-inventory tests are not real-node recovery evidence.
