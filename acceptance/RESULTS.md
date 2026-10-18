# Brewlet 2.0 validation status

**Incomplete acceptance; not a release claim.** Proposal 0009 remains the
contract. The 101-entry ledger deliberately distinguishes verified requirements
from implemented or partially tested behavior.

## Latest combined actual-node result

**`run-local-complete-62` passed all 81 selected assertions with no failures.**
Tested checkout: `6785f90683acd88dcaa27ed4840297a59904ae16`.
Production-source manifest:
`a6b8e45cbf7f67f4620284f27df9c20e3bb0533c50a58f094c52d76744fe1d00`.

```sh
PYTHONDONTWRITEBYTECODE=1 python3 integration-tests/live-v2/harness.py \
  --retain-on-failure --work integration-tests/live-v2/run-local-complete-62
```

Exit status was zero. All four languages passed actual dependencies, initial/
exec/probe identity and environment, Kubernetes overrides, immutable mounts,
strict barriers and real vendor updates with unchanged application digests.
Both Maven producer formats, every container role, authenticated TLS delivery
and its malformed/unsafe/platform failures, real CRI usage and kubelet eviction,
authorization expiry/recreation, worker/daemon recovery, live lease/mount
retention and normal grace collection passed. Native workload strategy,
supersession, global pacing/deadline, real provider-refusal/all-ID fencing and
claim-release CAS cases passed. Production root/finalizer/claim cleanup and
subsequent exact-owned fixture destruction both passed without manual
intervention.

The [public receipt](results/complete-local-four-language-arm64.json) preserves
the exact source, inputs and scope. `fullAcceptance` remains **false**:
companion scenarios and remaining detailed ledger obligations are not implied
by success of this combined suite. Later required marker-removal logging and
additional decision tests have separate native/contract evidence; this receipt
is not relabeled as a later checkout. No GitHub Actions or publication was used
to obtain this local result.

## Earlier combined actual-node result

**`run-complete-28` passed all 77 selected assertions with no failures.**
Tested checkout: `c9391295536688fb20173fdf51ea8d90e8bcc40d`.
Production-source manifest:
`dc20e47ecf37994e46e531f3c3015243ca6a446aeec2496ddbaabbe34ac2f0c4`.

```sh
PYTHONDONTWRITEBYTECODE=1 python3 integration-tests/live-v2/harness.py \
  --work integration-tests/live-v2/run-complete-28
```

Exit status was zero. The same fresh native arm64 node completed authenticated
TLS delivery, four-language vendor updates without app rebuilds, actual CRI
accounting/eviction, authorization expiry, worker/daemon recovery, uncertain
inspection retention, retired-generation/idle collection, native controller
strategies/pacing/deadlines, provider-pin fencing, and production cleanup.
Both bundled and BOM-managed Maven artifacts passed actual generic execution,
probes, security and runtime rotation without app republish. Main, completed
init, regular/native sidecars and ephemeral containers passed real execution,
postStart/exec/probe parity and independent malformed-artifact denials; their
remaining active leases were drained by the final production cleanup.
Actual operator claim release encountered an injected API resourceVersion
conflict and completed only after fresh ownership/teardown revalidation.
Production cleanup and subsequent disposable-fixture destruction each passed.
No manual intervention was recorded. The staging-monitor correction is now
confirmed through the previously failing actual OnDemand installation and the
complete combined callback chain.

[`results/expanded-producer-roles-real-node.json`](results/expanded-producer-roles-real-node.json) is the
whitelisted public result, not private logs, credentials or kubeconfig.
Its historical `gaps` array retains initial legacy strings, including some
superseded by its passing assertions. The driver now reports the remaining
unverified cases rather than those stale strings; the current ledger below
remains authoritative.

**This is selected-suite success, not complete Proposal 0009 acceptance.**
The ledger has **57 verified and 44 not fully verified requirements**.
Four complete rows were promoted from run 24: INV-06,
OD-06, E2E-02 and E2E-08. Worker recovery is not proof of recovery after killing
the shim task-server; normal backend exec drain is not forced orphan survival.
Physical recreated-Node cleanup and the remaining detailed ledger obligations
are still unverified. Stale-pin production uninstall passed separately in run 35.
Cold installation deadline/recovery passed separately in run 27 and verifies
ROT-10. No actual provider autoscaler or alternate-hardware join is claimed.
INV-03 was separately reconciled using the allocation-journal contracts:
controller reconstruction, pruned status, interrupted A/B/A and missing-history
fail-closed checks passed, along with local digest/generation reuse refusal.
Those contract tests complement actual monotonic rotations in run 24; they
are not being presented as real-node scheduling or physical-host recovery proof.

The separately executed cold-install case is
`python3 integration-tests/live-v2/cold_install.py --work integration-tests/live-v2/NEW_COLD_RUN`.
It creates two actual native nodes and uses an authenticated TLS proxy to hold
one fresh runtime layer beyond the real workload deadline. The initial topology
probe has verified the additional worker's boot-time startup taint,
Node/container UID-bound host access and exact two-container destruction.
Its first failed executions and subsequent passing result are preserved below;
they are not all being described as the current verdict.
The first actual execution, `run-cold-25`, failed before restart admission:
the controller incorrectly treated a cold `available` node's positive approved
generation as proof of an installed root, so it waited for that node to install
before completing rotation. The cold root was genuinely absent and the worker
had never received an install request. The inventory now publishes explicit
`installed` evidence independently of approval generation; the controller
excludes genuinely cold OnDemand entries but retains installed retrying entries.
Publication validates the flag against the ready/available state and actual
local-current checks. The regression uses the real positive-generation cold
shape, not the previous unrealistic zero-generation fixture. Native Linux
publication/all-ID recovery contracts passed without skips. Run 25's exact
two-node fixture destruction passed; cold deadline/recovery proof remains pending
the corrected execution.
`run-cold-26` progressed past the corrected participant selection and genuinely
scheduled restart-created Pods on the cold worker behind the held layer.
It exposed two further production issues: a minimum-generation barrier forced
the wrong `RuntimeImageUpdating` timeout reason even when no root existed, and
the controller lost runtime-install diagnostics when kubelet moved a failed
Create into `CrashLoopBackOff` with its `StartError` in `lastState`. Cold
resolution now preserves the actual wait reason, and restart classification
uses only never-started Create failures during backoff, never stale errors
after real execution. Main/init/ephemeral status contracts and the corresponding
actual-evidence guard were covered before the complete passing run 27 below.
The combined driver also now includes actual-node execution of both optional
Maven producer formats (bundled and BOM-managed dependencies), with exact
unchanged TLS publication, ordinary initial/exec/probe/security checks and an
additional runtime rotation without app republish. Its producer commands,
private Maven repository, explicit platforms, digest preservation and mandatory
callback ordering are covered by offline tests. The later run 28 passed that
callback for both formats; the Maven-on-node gap is removed only after success.
`run-cold-27` then passed all **11** selected assertions against
`19c64a5388842d023a791228a0ffdb5060bd1567`, production manifest
`dc20e47ecf37994e46e531f3c3015243ca6a446aeec2496ddbaabbe34ac2f0c4`.
The actual cold worker's blocked Pod reported `RuntimeImageInstalling` with its
UID, Node and minimum generation. Its 60-second workload deadline caused
`RestartDeferred`; the second slot started 61 seconds after the first.
Releasing the real TLS transfer yielded latest-runtime execution and Complete
status for both controllers without another template patch. Both Node claims
were released by production profile cleanup; fixture destruction separately
passed. [`results/cold-install-deadline.json`](results/cold-install-deadline.json)
preserves the sanitized result. ROT-10 is verified; ROT-09 still needs complete
default/earlier-native-failure reconciliation.

The passing run 28 also adds actual main/init/regular-sidecar/native-sidecar/
ephemeral execution, postStart/exec/probe parity, distinct named leases and
independent malformed-artifact denial for every non-main role. Those cases are
implemented with offline guards for ephemeral UID/spec CAS and real execution
versus preallocated StartError identities; all seven producer/role assertions
passed together with the original 70 and final production cleanup.
An independent `native_capacity.py` runner now implements the exact §13.1
no-ready-native-capacity case using truthful native hardware and an initially
empty eligible pool. It preserves the standard architecture selector, proves
Pending and actual boot-fence scheduling, then requires unchanged-Pod execution
after real capacity becomes available. It separately checks unsupported
architecture-unconstrained pulls and claims no provider autoscaling or alternate
CPU hardware. It does not change architecture labels. This is implemented and
offline-guarded; its initial failed run and subsequent complete pass are below.
`run-native-29` did reach a genuinely Running/Ready native Pod on the real worker,
but the fixture waited for a taint key that Kubernetes 1.35 omits from its generic
`untolerated taint(s)` event. The corrected observer reuses the existing strict
startup-taint evidence guard and briefly delays the actual owned kubelet until
that scheduling decision is observable, always resuming it in `finally`.
No architecture, clock or authority record is forged. The complete corrected native-pull/cleanup case subsequently passed in run 30.

The cold runner now also checks exact rendered five-slot/ten-minute defaults
and creates an actual unready Deployment with a ten-second native progress
deadline under the sixty-second workload bound. Its new case requires the real
`ProgressDeadlineExceeded` condition and slot handoff before the longer bound;
offline evidence guards passed before the actual run 31 below.

`run-native-30` passed all **8** selected assertions, and `run-cold-31` passed
all **13**, both at `2f2957dba3e474370194dbfb88502a01e9a81619` with production
manifest `dc20e47ecf37994e46e531f3c3015243ca6a446aeec2496ddbaabbe34ac2f0c4`.
The native case preserved the original Pending Pod and architecture selector,
observed the actual startup fence, executed on the real newly eligible worker,
rejected the architecture-unconstrained unsupported image at pull, and completed
production cleanup. The cold case additionally observed an actual ten-second
Deployment `ProgressDeadlineExceeded`, freeing the global slot after **11
seconds**, before the configured sixty-second workload deadline.
[`results/native-capacity.json`](results/native-capacity.json) and
[`results/cold-and-native-deadlines.json`](results/cold-and-native-deadlines.json)
preserve sanitized results; both fixture destructions separately passed.
These results verify E2E-05, SCHED-01 and ROT-09. They do not claim actual provider
autoscaling, an alternate-hardware join or elapsed ten-minute-default timing.

The next actual case, `pinned_uninstall.py`, creates a genuinely Helm-owned
profile, exercises real failed pre-delete hooks while tasks and stale provider
pins remain, and requires operator/RBAC/finalizer/claim/fence retention until
normal pool correction permits successful uninstall. It is implemented and
offline-guarded; its actual-node outcome is still pending, and no uninstall
requirement is promoted from those offline tests.
The first actual `run-uninstall-32` execution found a production gap before
uninstall: injecting a genuinely pinned stale capability onto an otherwise
healthy node caused repeated publication errors, but no startup fence was
installed. Earlier coverage introduced stale pins only after another fault had
already fenced the node. Reconciliation and final publication now attempt fresh
UID/owner-bound withdrawal; provider refusal must confirm the node-wide fence
before any host work, and fenced retries retain stale evidence. New contracts
cover both mutable withdrawal and pinned fallback with no host mutation. The
original exact-owned fixture was destroyed; complete corrected uninstall
execution remains pending.
`run-uninstall-33` confirmed the worker now withdraws mutable stale labels
promptly: that correctly raced the fixture's old add-label-then-enable-pin
sequence. The provider interceptor now supports only an exact authenticated
pool owner's initial absent-to-configured-value assignment; worker initialization,
subsequent owner changes and removals still receive real API refusals. The test
arms that policy before adding the stale label, eliminating the unpinned window
without pausing the writer or forging authority. The complete uninstall result
remains pending.
`run-uninstall-34` passed real pin initialization and healthy-node fencing, then
found the actual pre-delete Job unschedulable behind the very startup fence
uninstall must retain. The Job expired without ever requesting profile deletion;
this was not profile/finalizer loss. The chart now gives only the unprivileged
system coordinator an exact `Equal/provisioning/NoSchedule` Brewlet-fence
toleration. Rendered contracts verify the exact toleration and absence of
application RuntimeClass, host namespaces, privilege escalation and capabilities.
Application toleration rejection is unchanged. Corrected complete actual
uninstall remains unverified.
`run-uninstall-35` passed all **9** selected assertions at
`a37f8f8564777a2f639b97d55e0948831043c7ed`, production manifest
`2c419e9611fd1111468da1cbb4eddd7e7543574c689f2584b4ebd593f7e20943`.
The actual failed Helm hooks retained the operator, cleanup RBAC, profile
finalizer, UID claim, active leased root and confirmed startup fence. After real
task drain, the stale provider pin still blocked uninstall. Only explicit pool
correction enabled normal controller/worker teardown and unforced successful
Helm removal. Fixture destruction separately passed.
[`results/pinned-uninstall.json`](results/pinned-uninstall.json) preserves the
sanitized result. Together with prior actual OnDemand/pin recovery and scoped
coordinator contracts, this verifies E2E-06, FENCE-04 and OWN-04.

The next `retirement.py` case implements real exact-owned worker destruction,
fresh replacement serving before evidence, preserved missing-host obligations,
explicit attestor RBAC and immutable history across profile deletion/uninstall.
Its safety/identity guards pass offline; actual execution is pending, so it
does not yet verify detached physical-host retirement or returning-host fencing.
`run-retirement-36` actually removed its exact worker, but Docker's lowercase
`error: no such object` response tripped a case-sensitive confirmation guard.
The acknowledged removal had not yet been checkpointed in fixture ownership,
so conservative destruction refused the now-one-node inventory. Recovery
verified the original successful removal acknowledgement, exact missing worker
ID, sole remaining original container/Node UID and cluster label, then removed
only that owned fixture. The original run remains failed; this recovery is not
production cleanup proof. Removal bookkeeping now checkpoints the successful
external action before bounded case-insensitive, exact-ID absence confirmation,
so a later inspection error cannot lose fixture identity history. A reduced
actual two-node removal/survivor-destruction probe passed after the correction.

## Reference and branch

Reference main: `fa7085335b220e29df6affbece4558c5f1259ecb`.
Remote branch: `brewlet-2.0`.
Parentless root commit: `5d9071f6ad2544121c2d41fd2a19db92d8f0be37`.
The initial tracked index was checked empty before preserving documentation.
No implementation ancestry was inherited, and no existing remote branch was
overwritten.

## Executed successfully

Expanded actual activation/cache companion `run-local-cache-lifetime-68`
passed **11/11**, source `b97d0c63`, with normal production and fixture cleanup.
The real task's protected initial state supplied canonical persistent layer
paths. Original publication receipts aged beyond positive production GC grace
while the application and mounts remained live, and those layers stayed intact.
After normal Pod/task/lease drain, the normal fenced worker removed them.
A fresh task using the **same application digest** recreated the identical
digest/policy paths with new, verified digest/diff-ID receipts and served again.
No clocks, leases, receipts or publication times were edited. Together with the
native actual-bind/flock/ENOSPC negatives this closes RUN-06:
**83 verified, 18 not fully verified**.
[Actual cache-lifetime receipt](results/current-real-cache-lifetime-arm64.json).

Actual activation companion `run-local-activation-67` passed **9/9**, source
`85110e56`. A real fresh-node `none` worker rejected the explicit unsafe
`discard_unpacked_layers=true` fixture without changing the original config
SHA-256, installing a shim/journal/current root, advertising readiness or
releasing the startup fence. Normal profile spec correction to `validated`
repaired effective packed retention before a genuine cold application started.
Normal task drain and production finalization restored the **exact original
configuration SHA-256** and live containerd service, with owned shim and claim
removed. Fixture destruction passed; no activation or cleanup receipt was
invented. Both config-version/native rollback cases remain separately covered.
This closes RUN-10: **82 verified, 19 not fully verified**.
[Actual retention/activation receipt](results/current-actual-retention-activation-arm64.json).

Current-source native-capacity companion
`run-local-native-capacity-66` passed **8/8**, source `41d625e4`.
An architecture-constrained native executable's original Pending Pod started
unchanged only after actual eligible worker capacity, baseline and installation
appeared. The worker's architecture was not relabeled. A separate unconstrained
unsupported-architecture image genuinely failed to pull on that node and was
not counted as autoscaling success. Production profile/root/inventory cleanup,
exact claim release and fixture destruction passed.
[Current native-capacity receipt](results/current-local-native-capacity-arm64.json).
This is native arm64 scheduling proof, not new amd64 hardware execution or
cloud-provider scale-from-zero proof.

The expanded current-source retirement companion
`run-local-returning-retirement-65` passed **9/9**, source `ef74b02a`.
The original worker container actually stopped, its original Node UID was
deleted, and that same container re-registered with a different kubelet-created
UID while preserving its runtime filesystem and physical provider/system
identity. The real controller withdrew newly templated capabilities without
granting ownership or discarding the old obligation; a real scheduler canary
remained blocked. Fixture bookkeeping acknowledged the observed new UID, but
no production ownership, lease, receipt or status was fabricated.

The test then permanently destroyed that exact host and its new registration.
A different node served before any evidence existed. Real uninstall remained
blocked until an initially denied actor received the explicit attestor role
and supplied original-identity permanent-decommission evidence. That immutable
independent history survived profile deletion and actual uninstall. Exact-owned
fixture destruction passed. This closes OWN-03 with actual returning-host
evidence, not only an API fixture:
**81 verified, 20 not fully verified**.
[Physical-return/retirement receipt](results/current-physical-returning-retirement-arm64.json).
This verifies kind-host lifecycle, not cloud hardware autoscaling.

The corrected current-source cold companion
`run-local-cold-64` passed **13/13 selected assertions**, source `763ff160`.
Both real nodes finalized and released their original UID-bound claims, and
exact-owned fixture destruction passed. The actual held authenticated layer
transfer crossed the real 30-second Create bound and 60-second workload
deadline; the global slot handed off, release allowed real installation and
recovery without repatching, and an earlier native Deployment failure freed
its slot. Rendered five-slot/10-minute defaults are configuration checks, not
a claim of ten minutes' elapsed live timing.
[Current cold/deadline receipt](results/current-local-cold-deadlines-arm64.json).

The complete local run's actual deletion/revocation, role-by-role artifact
denials, capability/startup behavior and real TLS provider fencing now reconcile
INV-05, RUN-01, LABEL-01 and FENCE-03 alongside their targeted contracts:
**80 verified, 21 not fully verified**. Running leases survived revocation,
both refused fencing writes blocked deletion, and no fake API result is being
substituted for these actual-node assertions. Remaining detailed obligations
and companion scopes still gate whole-proposal acceptance.

Current-source two-node companion `run-local-cold-63` failed before the held
transfer/deadline case because the shared controller diagnostic helper assumed
the full callback had initialized `controlAcceptance`. The focused caller had
not selected that callback. Its
[failed receipt](results/local-cold-diagnostic-wiring-failure-arm64.json) and
successful exact-owned destruction are preserved; bootstrap success is not
cold-install proof. Diagnostic-state initialization now belongs to the shared
helper constructor, without inventing a selected control suite or pass verdict.
A faithful companion regression first reproduced the `KeyError`; all **307**
offline live-driver safeguards now pass. The actual two-node case must be
repeated before reporting new deadline/recovery evidence.

Rotation completion now has an explicit late-membership regression: after a
target completes, a newly eligible installed/retrying node clears current
completion until validation recovers, without rewriting the once-per-target
timestamp. Existing exact epoch/generation/digest conditions, newer-target
invalidation, retained missing-epoch exclusions and schema-pruning contracts
also pass. ROT-03 is reconciled as controller/schema contract evidence:
**76 verified, 25 not fully verified**. This does not assert that participant
completion patches every running container or proves provider autoscaling.

Request-marker reconciliation exposed a remaining normative omission: the
current owner deleted a non-offered marker without logging that decision.
It now reports the removed ID and literal profile UID/generation after the
fresh authority check. Native tests prove both current-owner removal/logging
and stale-owner non-mutation/no processed-request log. Marker checks also
verify empty/private file mode, stable inode on repeated first use, and no
new marker after failed Create. Complete native suites pass **71 node and
115 provisioner cases/subcases with zero skips**.

An explicit controller eligibility matrix now covers ready eager, cold
positive-generation OnDemand, installed retrying OnDemand, NotReady, startup
fence, retiring profile, stale Node claim and stale profile generation. Existing
cordon/confirmed-fence/deadline cases also pass. Fixtures persist Node and profile
status through separate API subresources; those are reconciliation contracts,
not real scheduling or provider-scaling claims. Go race tests, host/Linux vet
and both Linux builds pass. OD-02 and ROT-02 are reconciled using these contracts
and the separately recorded physical cold-install proof:
**75 verified, 26 not fully verified**. The new combined current-source node run
is independent evidence and is not assumed successful before it finishes.

Even the smaller Java root's first node repeat ran out of available Docker
space before its volume-write assertion. The BusyBox shell returned a failed
write without the GNU shell's I/O message; separate checks confirmed identity,
read-only mounts and empty capabilities were still correct. The
[failed compact-root receipt](results/local-java-alpine-disk-exhaustion-arm64.json)
remains failed, with exact-owned fixture destruction and no rotation claim.

Capacity recovery then used retained task build logs where old builder history
had expired. Fourteen unused compiler snapshots were individually correlated with
their unique completed command interval, frozen controller Dockerfile, exact
branch ancestor revision, recorded owned image identity and successfully
destroyed fixture. Only those fourteen anchored cache IDs were reclaimed, in
separately verified groups of eight and six.
A complete before/after cache inventory confirmed **every other cache ID was
preserved**, including shared compiler-cache mounts and unrelated resources.
Available space recovered to **19,993,698,304 bytes** without global pruning,
root-reserve changes or workload privilege changes. Private provenance and
reclamation audits are retained; recovery alone is not acceptance evidence.

Fresh normal provisioning at `8d16da75` now passed the complete selected Python
runtime scenario: **20/20**, including actual source rotation, strict/security
barriers, retained task/exec leases, events/metrics, and normal production
root/finalizer/claim cleanup. Exact-owned fixture destruction also passed.
The [clean current Python receipt](results/clean-local-current-python-arm64.json)
has production-source manifest
`a6b8e45cbf7f67f4620284f27df9c20e3bb0533c50a58f094c52d76744fe1d00`.
This verifies the CNI-parser correction through normal installed code rather
than the temporary diagnostic helper.

The subsequent larger Ubuntu-based Java JRE fixture again exhausted available
Docker space at its mandatory writable-volume check; the
[failed Java receipt](results/local-java-noble-disk-exhaustion-arm64.json) stays
failed. Its exact-owned resources were destroyed; Node.js/.NET queued behind
it did not execute. Rather than prune unrelated cache or bypass non-root
storage, the explicit Java fixture inputs now use smaller Temurin Alpine JRE
roots for the **same 21.0.7+6 to 21.0.8+9 update**. Proposal 0009 explicitly
permits JRE golden roots. No production runtime inference, launch rewriting,
validation relaxation or application-rotation assertion changed.

Both immutable parent indexes have Linux amd64 and arm64 descriptors. Actual
unprivileged, read-only native arm64 probes confirmed both versions, and both
derived platform-owned golden roots built with the same `65532:65532` account
and all declared PATH directories. Account preparation now supports the
explicit shadow-utils or BusyBox toolset; all **306** offline live-driver
safeguards passed. These are input/pipeline checks, not the pending full
Java node run or new native amd64 execution proof. Host Java/Maven builds
continue to use their JDK, and every complete-suite assertion remains required.

The controller build-cache change was exercised by
`run-local-current-python-58` at `ccff9f2e`: its compiler snapshot was **105.5 MB**
instead of the preceding **1.325 GB**. Actual Python HTTP/dependency execution,
exec/probes, identity/read-only/writable mounts, Kubernetes overrides, custom
UID/HOME, security and strict-barrier denials, source rotation without app
rebuild, old task/exec leases, and runtime events/metrics all passed.

The run nevertheless **failed production cleanup**, and is preserved as
[a failed receipt](results/local-python-nsfs-cleanup-failure-arm64.json).
The new mount parser incorrectly required every kernel mount root to be an
absolute filesystem path. Real CNI namespace binds instead report roots such
as `net:[4026535640]` on `nsfs`, causing conservative inspection failure and a
retained cleanup finalizer. This was a regression introduced by the new parser,
not a disk failure or a successful whole-suite run.

The correction keeps unrelated opaque kernel roots while still rejecting an
ambiguous mapping on the protected filesystem. The native cache regression now
creates an actual namespace bind alongside the actual cache bind; the namespace
survives both retention and successful post-unmount collection. All **71 node**
and **112 provisioner** native cases/subcases pass without skips. A temporary
read-only corrected inspector on the retained real node returned `gone=true`
for runtime mounts where the installed old inspector returned the parser error.
It did not replace the installed shim or perform production cleanup. Temporary
helpers and then only the failed fixture's exact-owned resources were removed.
Go race tests, host/Linux vet and both Linux builds pass. A fresh normal install
and complete production cleanup remain required before claiming this fix passes
the selected node suite; no requirement is promoted from the failed run.

The smallest current-source actual-node repeat,
`run-local-current-python-57` at `9a693616`, still failed on its mandatory
writable-volume check. The application served HTTP and completed an exec, but
the real UID **65532** write returned **errno 28** with **zero available bytes**;
the remaining **5,778,169,856 bytes** were root-reserved, not usable capacity.
This is a failed run, not a full execution/rotation/cleanup pass. Only its
UID/label-verified cluster, registry, helper containers and recorded image tags
were destroyed; that fixture destruction is not production cleanup evidence.
The [failed receipt](results/local-python-disk-exhaustion-arm64.json) preserves
the boundary.

Before/after builder inventories and completed build provenance then identified
two specific **1.325 GB** controller compiler snapshots belonging to this
session's runs 57 and 55. Their creation intervals matched exactly one build
each, with the expected local context, Dockerfile, branch revision, unique
run label and single usage. Exact anchored-ID pruning reclaimed only those
records; no pre-existing record was lost during the first removal, and no
unproven image, volume, compiler-cache mount or other project's cache was
pruned. Available Docker space recovered to **3,381,813,248 bytes**.
The private provenance audits and cache inventories are retained.

The controller Dockerfile was keeping its compiler cache in each source-specific
build snapshot. It now uses the same reusable BuildKit cache-mount mechanism as
the provisioner, with `-trimpath` for both binaries. An architecture contract
guards both builders. This addresses repeated build-storage growth without
changing runtime storage, relaxing non-root enforcement or consuming reserved
space. The next actual-node repeat must verify the changed build and current
production behavior; no success is inferred from reclaiming build cache.

Local kernel-level cache checks found and reproduced a production collection
bug: with cache data on a separate filesystem, a live bind mount did not protect
its cache entry. `mountinfo` printed its source relative to that filesystem,
while the previous inspector searched only for the absolute host path. The
collector actually deleted the fixture entry before the fix. Production mount
evidence now maps host paths through the most-specific host mountpoints and
checks device plus filesystem-relative roots in every inspected namespace.
Equally specific stacked mappings remain conservative; missing/malformed source
evidence blocks collection. The same actual bind now retains its entry, and
normal collection succeeds only after actual unmount.

Independent bounded extraction tests mounted a private **4 MiB tmpfs** and
genuinely exhausted it with an approximately **9 MB** payload. The resulting
`ENOSPC` exposed no partial entry or stage. Raising only that disposable mount
to **16 MiB** let the identical source publish with the expected receipt.
An actual shared consumer flock also prevented collection until it drained.
Gzip and zstd checks verify normalized `0:0` ownership from adversarial header
IDs, exact modes/nanosecond mtimes and forward-hardlink inode identity.
Incompressible payloads are delivered before all compressed bytes have been
read, while complete compressed/diff identities are still verified.

Full isolated native Linux/arm64 suites passed with zero skips:
**artifact 97, node 70, provisioner 112, shim 29 cases/subcases**.
An initial shim invocation omitted the required shared conformance bind and
failed exactly that fixture lookup; its failed log is retained separately.
Repeating with the fixture at the package-relative path passed all 29 cases.
These are actual Linux filesystem/overlay/flock checks, not a claim that a
fresh Kubernetes lifecycle or arbitrary killed-shim recovery passed.
RUN-04 and RUN-05 are reconciled; RUN-06 remains partial pending its full
current-source real-node lifetime reconciliation:
**73 verified, 28 not fully verified**.

The native runs use the same pinned Python userland and isolation flags as the
recipe below, with `CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go test -c
-mod=readonly` for each package. They use `/work` and `/tmp` tmpfs, no network,
a read-only container root, dropped capabilities and `no-new-privileges`.
All receive `SYS_ADMIN`/`MKNOD` and isolated `apparmor=unconfined`; node also
receives `SETFCAP`, and shim also receives `DAC_OVERRIDE`.
Artifact and shim mount repository `conformance/` read-only at
`/work/conformance` and run from `/work/pkg/artifact` and
`/work/shim/cmd/containerd-shim-brewlet-v2`, respectively. No shared Docker
cache or volume is resized or pruned. Detailed outputs are preserved as
`artifact-cache-native.log`, `node-mount-native.log`,
`provisioner-cache-native.log` and `shim-mount-native.log` in session artifacts.

Local retained-profile reconciliation found another production inconsistency:
admission accepted self-referential mirrors and malformed/duplicate mirror
allowlists that source resolution subsequently rejected. A permanent regression
first failed on all eight such cases. One shared policy now validates complete
allowlists and rejects self mirrors before admission, ownership or pull-reference
creation; duplicate worker-only checks were removed. Valid registry authorities,
including ports, IPv6 and Docker Hub, remain supported, and digest-preserving
mirror substitution is unchanged.

Retained pool-conflict/catch-all rules, explicit tolerations, default/validated/
none rollout policy, exact worker policy preservation and fail-closed
admission/controller paths now have direct contract coverage. Required handler
validation runs under both restart policies; `none` is not a validation bypass.
The complete isolated native Linux/arm64 provisioner suite passed **110
cases/subcases with zero skips**. This reconciles PROF-02:
**71 verified, 30 not fully verified**. Fake-API ownership assertions are
contract evidence, not real scheduling or provider-scaling proof. No new
real-node suite is claimed while local disk capacity remains insufficient.

```sh
go test -count=1 -race -mod=readonly ./...
go vet ./...
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -mod=readonly ./...
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -mod=readonly ./...
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go test -c -mod=readonly \
  -o "$ARTIFACTS/provisioner-mirror.test" ./provisioner
docker run --rm --network none --read-only --cap-drop ALL \
  --cap-add SYS_ADMIN --cap-add MKNOD --security-opt no-new-privileges \
  --security-opt apparmor=unconfined --tmpfs /work:rw,exec,size=768m \
  --tmpfs /tmp:rw,exec,size=256m --workdir /work \
  --mount "type=bind,source=$ARTIFACTS/provisioner-mirror.test,target=/provisioner.test,readonly" \
  docker.io/library/python@sha256:34386ef0cb081344d7ec1c103ba398e6e9f64e9ab3a1509accc92a4e24a07258 \
  /provisioner.test -test.v
```

All commands passed; native output is preserved as
`provisioner-mirror-native.log` in the session artifacts. `ARTIFACTS` is the
absolute session artifacts directory, not a shared or repository-root target.
Only the isolated native kernel tests receive the listed extra capabilities
and AppArmor setting; application confinement is unchanged. A backed-up,
completed task-owned build-history reference was released without recovering
disk capacity. No shared build caches, images or volumes were pruned. Local
commits remain unpublished to avoid triggering the deferred GitHub Actions.

With shared-cache cleanup not authorized, no further disk-heavy local repeats
are started while Docker capacity is insufficient. Offline implementation
hardening found an additional observability gap: successful initial Start
silently ignored application-state loading errors and omitted its runtime
event. Such errors now produce explicit task/reason diagnostics, including
failed protected-kind lookup; only protected containerd infrastructure-sandbox
metadata suppresses expected application-state absence. The actual successful
runc result is preserved and no event identity is invented.
The full native Linux/arm64 shim suite passes **29 cases/subcases without
skips**, including application, infrastructure and metadata-error cases. Go
race tests, vet and both Linux builds pass. OBS-04 is partial, not verified:
the complete reason matrix and new physical fault checks still need proof.
GitHub Actions remains deferred; no shared resources were pruned.

The strict JSON fix passed physical bound-Pod verification:
`run-local-strict-control-55` at `1fb1b6df` completed **29/29 selected assertions**
and production/fixture cleanup. Duplicate ID and duplicate generation fields
were rejected by the real shim with `InvalidRuntimeGeneration`; ordinary
epoch/future barriers, runtime rotation and controller behavior still passed.
[`Strict-barrier receipt`](results/clean-local-strict-barriers-arm64.json).
Together with exact-int64/parser contracts and the real recreated-inventory
epoch refusal in lifecycle run 51, this closes ROT-07:
**70 verified, 31 not fully verified**.

The following Java repeat stopped at a writable-volume I/O error before
rotation. Live inspection confirmed Docker's Linux filesystem again had
**zero unreserved available bytes**, with plentiful inodes. This is not
reclassified as a parser failure or a successful runtime repeat. Only the
failed invocation's exact-owned resources were removed; available space
recovered to **2,441,465,856 bytes**, still insufficient for a safe Java repeat.
The [failed Java receipt](results/local-java-disk-exhaustion-arm64.json) is
preserved; Node.js/.NET queued behind it did not execute. No shared build cache,
images or volumes were pruned, and no non-root or storage safeguards changed.

**The worker-health fix is verified by real local accounting.** Run 53 passed
**19/19** selected assertions with unchanged safeguards: CRI writable snapshot
usage grew from **8,192** to **16,789,504 bytes** after the bounded 16 MiB write;
the later 80 MiB allocation triggered actual kubelet `Evicted` status against
the configured limit. Safe available space was **1,599,021,056 bytes**, above
the unchanged **1,157,627,904-byte** requirement. Unrelated-workload health
and identity checks passed, as did production and exact-owned fixture cleanup.
The same production source then passed controller **27/27**, container roles
**23/23**, and delivery **27/27** regressions. Receipts:
[`accounting`](results/clean-local-accounting-worker-health-arm64.json),
[`control`](results/clean-local-control-worker-health-arm64.json),
[`roles`](results/clean-local-roles-worker-health-arm64.json), and
[`delivery`](results/clean-local-delivery-worker-health-arm64.json).
The pre-fix health failure remains failed; no writes or guards were fabricated.

A separate strict-barrier audit found ambiguous duplicate JSON was accepted by
the node decoder: duplicate generation/ID members silently used their last
value. A targeted Go regression reproduced acceptance before the fix.
Node barriers and protected records now reuse the existing generic artifact
strict decoder, preserving the object boundary and rejecting duplicate/escaped
members, unknown fields and trailing documents. Negative numeric cases and
exact counters above 2^53 through max int64 pass. Host Go race tests, vet, both
Linux builds and full native node/provisioner/shim suites pass with no skips
(**63/103/25 cases/subcases**, respectively).
Bound-Pod duplicate-ID/generation denials are now mandatory selected cases;
their physical verification is pending. ROT-07 is not promoted before that run.

Local container-role run 52 passed **23/23**, covering actual main/init/native
sidecar/ephemeral execution, role-specific malformed-artifact refusal and
production cleanup. Formal delivery run 52 passed **27/27**, including actual
TLS/auth pulls, malformed/unsafe/mirrored/corrupt/untrusted/unauthorized start
refusals, native execution and unsupported-platform pull failure. Their
classified receipts are
[`roles`](results/clean-local-roles-arm64.json) and
[`delivery`](results/clean-local-delivery-arm64.json).

The accounting run stopped **before writes** at its unchanged healthy-unrelated
workload guard. Direct inspection established another production bug: the
healthy provisioner was Running with zero restarts, but `--health-check` failed
because `/run/brewlet` did not exist. The main loop wrote the readiness marker
inside the *reconciliation error branch* and never on ordinary success.
The fix synchronizes readiness after every outcome: success requires fresh
authority before atomic publication; failure clears prior readiness; marker
removal errors are surfaced. Four native Linux/arm64 command tests pass with no
skips, including success/failure, lost authority, failed removal and protected
fresh/read-only checking. Go race tests, vet and both Linux builds pass.
Physical health/accounting recovery with the fix remains pending, and no
accounting safety reserve or workload-health requirement has been relaxed.
The pre-write failure is retained
[here](results/local-accounting-worker-health-failure-arm64.json); fixture
destruction afterward was exact-owned only and is not production-cleanup proof.

The optional Maven producer and lifecycle surfaces now pass against the same
fixed production implementation on local native arm64. Maven run 50 passed
**20/20** selected assertions: both bundled and BOM-managed artifacts actually
launched, probed and changed vendor runtime without republishing application
bytes. Lifecycle run 51 passed **32/32**, including real authorization expiry
and recovery, inventory deletion/epoch refusal, worker restart/containerd
reconnect, unavailable-task-service retention, positive GC grace, cold
OnDemand retargeting, mode/idle transitions, actual task/exec/mount retention
and normal delegated CRI drain. Both completed production and exact-owned
fixture cleanup with no manual intervention.
[`Maven`](results/clean-local-maven-arm64.json) and
[`lifecycle`](results/clean-local-lifecycle-arm64.json) preserve selected scope.
Arbitrarily killed shim-server reconstruction and forced orphan-exec survival
are still not claimed; these successes do not erase those remaining gaps.

The next local runs select the existing container-role, formal delivery-negative
and CRI accounting callbacks independently to fit current Docker capacity.
Default complete execution remains unchanged. All **305** driver safeguards
and **9** acceptance/export checks pass; no new physical results for those
three cases are claimed before execution. Accounting's reserve, bounded writes,
actual usage/eviction and unrelated-workload checks are unchanged.

Provider operations documentation now has explicit v2 EKS simulation-only
tags, a matching validated NodeProfile, a Karpenter real-node/startup-taint
example, and AKS managed-cluster initialization, pool-scope, feature-registration
and node-image/reimage requirements. The guidance was reconciled with Proposal
0009, the pinned capability reference and official provider documentation.
The preserved Pages workflow was read as the site build reference; no workflow
was changed or executed and no 1.x publication pipeline was activated.
`go test -count=1 -race -mod=readonly ./architecture` validates the example YAML,
inventory/label agreement, exact startup toleration/taint and explicit
unverified-cloud boundaries. This closes documentation requirement LABEL-02:
**69 verified, 32 not fully verified**. It is offline contract evidence, not a
claim of EKS/Karpenter/AKS scale-from-zero or live cloud initialization.

Local work now selects the already implemented Maven-producer and lifecycle
callbacks independently, with their required Java/Python fixture respectively,
so these implementation surfaces can be checked within current Docker
capacity. Each still requires the real base runtime/rotation/observability and
production cleanup; missing suites remain explicit and complete/default
execution retains every callback. All **304** local safeguards and **9**
acceptance/export checks pass. New physical Maven/lifecycle outcomes are
pending execution, not inferred from driver tests.

**All four runtime paths also pass against the Eager-readiness production
fix.** Node.js and .NET retries each passed **18/18** local assertions and
production/fixture cleanup; Java's fixed-source run passed **18/18**, and
Python's Eager/controller run passed **27/27**. All four share production-source
manifest `3bbae3930aa226899c3b33542a2863d5f752b0e667c8fd08748e71208afaa7bb`.
The new receipts are
[`fixed-source Node.js`](results/clean-local-node-readiness-fix-arm64.json) and
[`.NET`](results/clean-local-dotnet-readiness-fix-arm64.json).
The earlier webhook timeout remains a failed startup result; it is not rewritten
as a pass. These are selected serial local results, not full-proposal
acceptance. GitHub Actions remains deferred.

**The production Eager fix is physically verified locally.** At `5c96c75c`,
`run-local-eager-control-47` passed **27/27 selected assertions**, including
actual incompatible-account installation failure, effective readiness
withdrawal, real scheduler non-placement, same-Pod recovery after correction,
unchanged running leases, failed-inventory finalization and subsequent
controller/provider/production-cleanup checks. The fixed-source Java run
also passed **18/18** runtime assertions and cleanup.
[`Eager recovery`](results/clean-local-eager-recovery-arm64.json) and
[`fixed-source Java`](results/clean-local-java-readiness-fix-arm64.json)
retain the explicit selected scopes. This reconciles PROF-03:
**68 verified, 33 not fully verified**.

The serial Node.js run stopped before application execution because the
owned inventory webhook's first API-server request exceeded its five-second
transport deadline. Live Pods/endpoints were healthy and the exact same
dry-run request succeeded afterward without changing the admission server.
The bounded 60-second startup wait now recognizes that specific owned
webhook transport failure, while still immediately rejecting admission
denials, foreign webhook destinations and certificate-identity errors.
No deadline was increased, no failed application assertion is retried, and the
failed startup receipt is preserved
[here](results/local-node-webhook-startup-failure-arm64.json).
All **301** local safeguards pass. Node.js/.NET fixed-source execution remains
pending retry; GitHub Actions remains deferred.

The new local approved-Eager case found a **production bug** in
`run-local-eager-control-46`: after rejecting the actual golden account mismatch
and marking its authorization failed, the worker left the old aggregate
`brewlet.sh/runtime=ready` advertisement intact without a startup fence.
It withdrew only the failed ID's capability. The same defect reproduced in
native deterministic regressions for mutable and provider-pinned baselines.
[`results/local-eager-readiness-failure-arm64.json`](results/local-eager-readiness-failure-arm64.json)
retains the failed physical result.

The worker now withholds stale aggregate readiness under fresh ownership
checks before an unfulfilled required Eager installation, and on failed
required baseline/authorization checks. It reuses confirmed capability
withdrawal/provider-taint fallback, preserves validated previous roots during
compatible rotation, and leaves optional OnDemand failures isolated.
The full native Linux/arm64 provisioner suite passes **103 cases/subcases with
zero skips**, including both reproduced failures, compatible-current rotation
and metadata refresh, optional OnDemand isolation, unverified required checks,
and stale-owner non-mutation. All Go race tests, `go vet ./...`, and Linux
amd64/arm64 builds pass locally. Physical Eager recovery with the fix remains
pending; PROF-03 is not promoted before that run.

The next local control execution adds a physical approved-Eager failure case
for the still-partial PROF-03 requirement: the source is valid and pinned, but
its declared non-root account deliberately differs from the actual golden
`app` account. Selected checks require an actual failed current-owner
authorization and account-validation error, no published root/capability,
effective baseline withdrawal/fencing, and real scheduler non-placement of a
good-runtime canary. Recovery must release the same canary after the required
entry is removed and preserve existing leases; only normal inventory cleanup
may finalize the failed entry. All **299** local safeguards and **9**
acceptance/export checks pass. This new physical case is not yet verified.

**All four language runtime paths now pass locally.** At `2d59927b`, serial
native arm64 Java, Node.js and .NET runs each passed **18/18 selected assertions**
with clean production cleanup and exact-owned fixture teardown. Each exercised
actual TLS acquisition, first-use installation, explicit launch and
dependencies, command/cwd/identity/exec/probe behavior, readonly confinement,
negative security/generation starts, real vendor update with unchanged
application bytes, held-exec/runtime-lease retention, and old/new event/metric
joins. Combined with Python/controller run 43, all four used the exact same
production-source manifest
`2812f5294a5873b6695b4ecd305256b5377e4442a6126fa0d124208dc9b64352`.
The three public receipts are
[`Java`](results/clean-local-java-runtime-arm64.json),
[`Node.js`](results/clean-local-node-runtime-arm64.json), and
[`.NET`](results/clean-local-dotnet-runtime-arm64.json).
These are independent serial local results, not a simultaneous four-runtime
or complete-proposal acceptance pass; their omitted callbacks remain explicit.
GitHub Actions work stays deferred.

The first serial Java attempt (`run-local-runtime-java-44`) stopped during
delivery bootstrap: ancillary negative OCI graphs still used a literal
`oci-python` seed even though only Java was selected. No Java runtime failure
is inferred from that setup error. Delivery now uses the explicit probe fixture
for those graphs and related Pod templates. A complete mocked bootstrap wiring
test covers positive and negative graph selection with Java and the two vendor
revisions; all **297** local safeguards pass. Exact-owned failed bootstrap
resources were removed. The actual Java/Node.js/.NET serial execution remains
pending retry.

The local driver now supports serial, single-fixture runtime execution for
Java, Node.js and .NET as well as Python. This keeps current disk constraints
from forcing simultaneous golden-root copies. It changes no production
language-neutral execution behavior. Each explicit runtime-only run retains
real acquisition, negative starts/barriers, vendor rotation/held exec,
observability and production cleanup; controller/provider and complete-suite
omissions remain recorded. Complete/default runs still require all four
languages, and controller-only fixture selection still requires Python.
All **296** local driver safeguards and **9** acceptance/export checks pass.
No new runtime-only actual-node outcomes are claimed before execution.

**Clean local implementation milestone:** native arm64
`run-local-implementation-43` at `076fd7fe` passed **all 26 selected assertions**
and exact-owned fixture teardown, with no manual intervention. It exercised
real TLS acquisition, on-demand installation, application launch/exec/probes,
non-root/readonly enforcement, vendor rotation without app rebuild, old/new
runtime event/metric joins, native controller strategies and barriers, global
restart pacing/deadlines, all-ID provider-fence recovery, active-lease cleanup
blocking, actual production root/finalizer/claim release, and a real release
PUT resourceVersion conflict followed by fresh revalidation.
This is a clean **Python/controller** local result, not a four-language or
complete Proposal 0009 acceptance claim. The receipt preserves explicit
omissions and `fullAcceptance: false`.
[`results/clean-local-python-control-arm64.json`](results/clean-local-python-control-arm64.json)
is the classified public record. Together with six targeted restart-selection
and durable-intent contracts, it reconciles ROT-06: **67 verified,
34 not fully verified**. GitHub Actions remains deferred and new milestones
remain local.

Smaller local run `run-local-implementation-42` successfully completed Python
startup, explicit overrides/security, vendor rotation with the same application
artifact, held-exec preservation, generation barriers, and actual old/new
containerd-event/metric joins. It then exposed an integration bug in the new
debugging mode: control entry still required the literal four-language set.
The guard now requires exactly the explicitly selected language set; default
four-language runs still reject an incomplete base. A direct control-entry
regression covers both modes, and all **294** local driver safeguards pass.
This run is retained as a failed run, not claimed as completed control
acceptance. Only its exact-owned resources were destroyed before retry.

With no user available to change Docker capacity, local work continues through
an explicitly smaller Python/controller execution rather than pruning shared
resources or switching back to Actions. `--local-python-only` requires the
existing partial control scenario and explicit pinned-case omission. Only the
selected application and runtime roots are imported/published/offered; the
normal four-language path is unchanged. Real source acquisition, launch,
exec/rotation, event/metric joins, control/provider cases and production cleanup
remain mandatory for that selected scope. Results/export retain
`selectedLanguages` and the missing-language gaps. All **293** driver safeguards
and **9** acceptance/export checks pass; no smaller actual-node pass is yet
claimed.

Local retained run `run-local-implementation-41` reproduced the Python held-exec
failure and established its filesystem cause directly: UID 65532 saw
**zero available bytes**, while roughly 6.5 GiB remained reserved for root.
A bounded write in that same owned application volume returned
**errno 28, `No space left on device`**. Inodes were plentiful. This explains
the shell's generic `printf: I/O error`; it does not establish the cause of the
earlier remote controller-observation stalls. The post-failure probe and its
removal are recorded as manual diagnosis, not a clean acceptance pass.
After recording the evidence, exact container IDs and labels were checked and
only this invocation's cluster, registries and image tags were removed.
Unprivileged Docker filesystem headroom recovered to 4,635,127,808 bytes.
No shared build cache, unrelated images, containers or volumes were pruned.
Further full local execution needs more Docker filesystem capacity; neither
root execution nor root-reserved blocks are a valid application workaround.

**Local-first implementation work is now authoritative; GitHub Actions work is
deferred by user direction.** Local native arm64 run `run-local-implementation-40`
at production source `34ddf959` passed initial execution for Java, Python,
Node.js and .NET, including dependencies, exec/probes, Kubernetes overrides,
non-root identity, readonly application/runtime mounts, conflicting ignored
runtime config defaults, and actual admitted RuntimeClass overhead. Java also
completed its real vendor rotation without rebuilding the application. These
selected results plus the six platform contracts and shared RuntimeClass
contract reconcile ROOT-01 and RC-01: **66 verified, 35 not fully verified**.
The overall run **failed** when Python's held exec reported a `printf` I/O error.
No filesystem or Brewlet root cause is yet established, and the selected run is
not a successful whole-suite acceptance result. Exact-owned fixture destruction
passed; shared Docker resources were not pruned.
[`results/local-startup-and-java-rotation-arm64.json`](results/local-startup-and-java-rotation-arm64.json)
preserves the classified failed result and actual selected passes.

For direct local investigation, the driver now has opt-in
`--retain-on-failure`: helper processes still stop, but failed owned resources
remain available through the private kubeconfig. Failure remains nonzero,
destruction is explicitly `retained`, and normal/success cleanup behavior is
unchanged. Dead held-exec processes now fail immediately with their exit status
instead of polling the Pod marker for a minute. All **290** offline safeguards
passed. No workflow changes or new remote runs are part of this work.

Reconciling RC-01 found a genuine implementation omission independent of the
observation stall: both RuntimeClass manifests omitted the retained pod
reservation entirely. Raw and Helm installs now retain SPEC section 7's
`64Mi`/`50m` reservation exclusively for shim/staging, not application or
language-runtime memory. Contract checks assert the matching shared
handler/selector/reservation and absence of startup-fence tolerations.
`go test -count=1 -race -mod=readonly ./architecture` and rendering the Helm
RuntimeClass passed. The live driver now requires that exact overhead on each
actual admitted initial application Pod. RC-01 remains partial until a run
containing this manifest change passes; the currently running diagnostic CI
source predates it.

Run `39044794472` failed after 34 selected assertions at source `fac84659`.
The new owner-scoped diagnostics show both selected roots eventually advertised
at generation 2 without a startup fence, while the retained binding observation
was older than two minutes. Collection's host task/mount inspection was killed
at its existing budget, and subsequent binding inspections reported deadline
expiry. The native Deployment was available at generation 2; its restart record
correctly remained Deferred rather than treating stale old-task observations as
absence. Transient Node resourceVersion conflicts and an expired earlier
authorization were also recorded. These facts narrow the failure to delayed
provisioning and incomplete timely observation, but do not yet identify the
expensive or blocked observation operation.

Binding inspection errors now retain their exact stage, containerd namespace
and task identity while preserving wrapped error identity. There is no change
to budgets, collection authority or stale-observation retention. The full
native Linux/arm64 provisioner suite passed **92 cases/subcases without skips**,
including deterministic namespace/container deadline-error checks;
`go test -count=1 -race -mod=readonly ./provisioner` also passed.
[`results/scoped-provisioner-failure-amd64.json`](results/scoped-provisioner-failure-amd64.json)
preserves the failed run's classified outcome, not private logs or credentials.

The four-language live fixture builder now makes the proposal-listed ignored
runtime config fields explicitly conflict with application execution: nonexistent entrypoint
and healthcheck, unrelated arguments/workdir/port/volume, and root `User`.
All eight before/after Dockerfiles are checked by the **287** passing offline
safeguards. ROOT-01 remains pending until these inputs execute successfully
on a real node; earlier runs are not retroactively evidence for these defaults.

Failure diagnostics now include only bounded provisioner error-marker lines
from the exact profile owner's worker on the recorded Node. Existing registry
password/basic-auth/bearer-token redaction is applied; ordinary log lines,
foreign workers and raw Pod environments are excluded. The selected live
binding snapshot carries its genuine publication timestamp and known binding
fields. Diagnostic collection errors are recorded separately and never replace
the original failed wait. All **287** offline safeguards pass. These additions
are operational evidence capture, not a claimed fix for the unresolved
provisioning/fresh-observation stall.

Run `39040234666` established a more specific failure: the Deployment itself
was available at its patched generation, but its restart status remained
`Deferred` with `StaleOlderRuntimeTaskObservation` for an old Pod UID.
Both runtime rotations had also retained correctly reported `RotationStalled`
exclusions. The restart controller did not falsely claim completion or occupy a
slot while this genuine/stale older-task uncertainty remained.

Neither bounded source review established a production deadlock or a contract
requiring parallel installs. No old-Pod UID pruning, uncertainty bypass or
arbitrary install-duration cap was added. Provisioning snapshots now run on
every control failure, not only rotation timeouts, and retain the selected
Node binding-observation identity/timestamp and known binding fields alongside
claim, fence and profile state. Raw specs, environments, unrelated IDs and
unknown binding fields are excluded. All **285** offline safeguards pass.
Actual worker-side progress/observation failure diagnosis remains unresolved.

Run `39036910264` retained a genuinely approved generation-two target while the
participant continued reporting generation one throughout the rotation wait;
the existing generation-one workloads remained healthy. This is a provisioning
progress failure, not evidence that stale task tuples should be discarded.
Bounded source reviews found no demonstrated lock-order cycle or restart-slot
defect. Sequential installation alone does not violate the proposal and is not
established as this run's cause; no parallel-install rewrite or arbitrary install
timeout was introduced.

Rotation failures now also retain live Node UID/claim identifiers, selected
runtime-inventory entries, startup fences and bounded profile progress/conditions.
Unrelated runtime entries, profile specs and other fields are excluded, and the
original rotation failure remains an error. All **285** offline safeguards pass.
Actual source-pull/publication failure diagnosis remains outstanding.

Focused run `39033035280` failed waiting for the strategy matrix's
`rolling-deployment` restart record to reach `Complete`. The retained rotation
snapshots alone do not establish why that workload outcome was missing.
Readiness and strategy waits now also retain the dedicated workload's UID,
generation, native status and selected restart record; readiness captures only
Pod UID/phase/readiness summaries, never raw Pod specs or environments. All
**284** offline safeguards pass. No further deadline adjustment or production
restart fix is inferred from this timeout without those observations.

Complete run `39028815384` passed **69** assertions, then timed out waiting for
the pre-pacing `pace-1` Deployment to become ready. Retained rotation diagnostics
show both dedicated runtimes' generation-three targets completed with the node
excluded as `RotationStalled` after **31 seconds**, still reporting generation
two. Thus `RotationComplete` did not imply available target-generation capacity.
Exact-owned fixture destruction passed.

The controller fixture had accidentally configured a **30-second node-install**
deadline along with its short workload deadlines. It now preserves the proposal's
**10-minute node progress default**, independently of the **60-second workload**
pacing bound. Restart disablement moves before all pre-pacing source rotations,
and positive pacing setup requires actual latest-generation `ready` capacity
with no startup fence before creating its workload. No exclusion diagnostic,
status, receipt or clock is overwritten. All **284** offline safeguards pass;
the corrected real-node run remains required.

Focused native amd64 CI run
[`39026251031`](https://github.com/microsoft/brewlet/actions/runs/39026251031)
at `cdad031f7221f593099a2cbb8d8136917af2705c` passed all **44** selected
assertions, including controller strategies, actual global pacing, provider
refusals, production cleanup and real operator claim-release CAS recovery.
The 60-second global pacing case recorded **135** samples, two genuinely failed
unready rollouts and **64 seconds** between their slot starts. Exact-owned
fixture destruction passed. The receipt preserves `selectedScenario: control`
and `fullAcceptance: false`:
[`results/clean-control-amd64.json`](results/clean-control-amd64.json).
Accounting, formal delivery negatives, Maven-node, container roles, lifecycle
and the companion suites were intentionally omitted from this focused run.

This verifies the explicit **resource-version race or recreated Node**
alternative in §13.1 for E2E-09: the real operator's first release hit API CAS,
persisted `CleanupBlocked`, and retried with a fresh request/version only after
original Node/claim/Teardown/no-state checks. It does not verify physical
recreated-host or returning-host cleanup; OWN-02 and OWN-03 remain partial.
Complete CI run `39028815384` was dispatched at the later workload-CAS fixture
fix `1293ed60ae61bbdf15a52948e0f3eb81279d6802`; its result is pending.

Complete-scope CI run `39023985022` passed **66** assertions, including the
strict healthy-baseline accounting check and the selected lifecycle cases.
It failed on an actual API `409 Conflict` while conditionally patching the
`old-epoch` Deployment; exact-owned fixture destruction passed. Fixture workload
patching now retries only conflicts, at most six times, using freshly read
UID/resourceVersion while requiring unchanged spec, generation and targeted
metadata. Replacement/foreign ownership, concurrent spec/target-metadata edits
and non-conflict errors still fail without another patch. All **284** offline
safeguards passed. This does not convert the failed CI run into a complete pass.

Focused control run `39023983077` failed waiting for a dedicated runtime's
`RotationComplete` condition before reaching the pacing case. It passed **32**
earlier selected assertions, not the focused suite. The original receipt did
not retain the incomplete controller status, so no cause is inferred from the
timeout alone. Rotation waits now retain bounded copies of the relevant UID,
object/target generations, digest, rotation details and conditions in the
classified result. Raw objects and unrelated fields are excluded. All **281**
offline safeguards passed. The independent complete run `39023985022` remains
separate evidence and is not assumed to have this same failure.

CI run `39022648917` did not reach the staged-pacing case: it failed earlier in
CRI accounting when the provisioner Pod's captured state changed. Exact-owned
fixture destruction passed. No cause is asserted without the missing original
before/after fields. Accounting now captures only an initially healthy baseline
and retains exact before/after container identities, restart counts and readiness
on mismatch; its post-baseline guard and disk/write safety thresholds are
unchanged. A healthy baseline is not a waiver for later readiness loss.

An explicitly partial `--scenario control` driver/workflow input now isolates
controller and cleanup debugging while retaining genuine startup, applications,
rotations, observability, provider refusals and production cleanup. The complete
scenario remains the default and still executes every original callback and
companion case. Public receipts retain the selected scope and omission list;
focused passes cannot be described as complete acceptance.

LEASE-01 is verified by direct serialization contracts plus actual task-generation
evidence. A native filesystem test races **32** Create resolutions with runtime
publication and collection under the production per-ID flock, verifies all
**33** durable leases and exact generation/root pairing, and retains the old
leased root after the new generation becomes current. The complete native arm64
node suite passed **61** cases/subcases without skips; the contention case also
passed **10** repeated runs. Collection in this isolated test uses explicit fake
absence evidence and is not represented as CRI/mount inspection. Actual
old/new generation selection, recovery and post-drain collection come from the
selected real-node assertions in run `39019782549`, whose later pacing failure
is preserved.

Native amd64 CI run
[`39019782549`](https://github.com/microsoft/brewlet/actions/runs/39019782549)
at `f09e41fb303042b9d1a46a7ca845f100ac6bccd0` passed **69** selected assertions,
including the previously blocked cold installation and all selected lifecycle
cases: genuine first-use latest-target installation, lease-protected idle TTL,
mode transitions, task/exec/mount retention and post-drain release. It also passed
native controller strategies, epoch barriers, idempotency and supersession.
The sanitized receipt is
[`results/lifecycle-and-controller-amd64.json`](results/lifecycle-and-controller-amd64.json).
The command **failed** when it could not establish both a globally queued rollout
and deadline slot handoff. Exact-owned fixture destruction passed.

The pacing fixture previously waited for both runtime roots to finish installing
before sampling, although an earlier completed target could already have opened
its restart window. Installation and restart enablement are now separate real
chart phases: both targets finish under disabled restarts, then the actual
controller is enabled with a 60-second workload deadline and global limit one.
This isolates application-rollout pacing from runtime-copy duration; neither
status nor timestamps are fabricated, and slot handoff must still take at least
the configured deadline. Bounded classified pacing samples are retained even
on failure. All **277** offline safeguards passed; corrected actual pacing
remains unverified until a fresh real-node run.

PROF-01 is reconciled against its entry-validation and unknown-ID isolation
contracts. The explicit install/TTL matrix, shared API/admission/controller
checks and race tests passed. The complete native arm64 provisioner suite now
passes **89** cases/subcases without skips, including both Eager and OnDemand
approved siblings beside an unknown ID. Unknown-ID tests use a fake API and
actual local filesystem state; they are not presented as real scheduling or
startup-fence proof.

Native amd64 CI run
[`39017459558`](https://github.com/microsoft/brewlet/actions/runs/39017459558)
at `854aad1c0afb094f2ab598bd4c0731b185947c96` passed **61** selected assertions,
including the new actual containerd event and worker-metric joins for **eight**
old/new tasks across four languages. All runtime identities matched actual Pod
UIDs and durable leases. First-use wait events, request counters, durations and
current/retired gauges passed; separate shared stores measured 1,683,734,607
runtime bytes and 62,091 layer bytes. Native shim and metric contracts also
cover failed-start non-publication, lossless generations, failed install counts
and explicit event delivery errors. This verifies OBS-01 and OBS-03.
The sanitized receipt is
[`results/observability-and-recovery-amd64.json`](results/observability-and-recovery-amd64.json).

The same run proved real authorization expiry, exact expired bound-start
rejection, fresh renewal, worker/containerd recovery and lease-preserving
inspection uncertainty. It then **failed** on a distinct cold lifecycle Pod:
the allowed bounded Create timeout produced a never-started `StartError`, but
the fixture's `restartPolicy: Never` made the Pod terminal rather than allowing
the expected kubelet retry. Exact-owned fixture destruction passed. Long-running
acceptance Pods now use `Always`; readiness still rejects a prior **actual**
application exit, including one hidden by a subsequent successful restart.
All **276** offline safeguards passed. The cold retry correction still requires
a complete fresh real-node run; these selected results are not full acceptance.

Complete compiled native Linux/arm64 component suites were also executed:
**90 artifact, 25 node, 83 provisioner and 20 shim cases/subcases passed with
zero skips and zero failures**. These are kernel/component contracts, not
fake-client scheduling, provider autoscaling or a replacement for the real-node
runs. [`results/native-linux-arm64-contracts.json`](results/native-linux-arm64-contracts.json)
preserves the bounded public summary; the exact reproducible build/container
commands are the `linux-overlay-contract` steps in `.github/workflows/v2-ci.yml`.

The full suites now run in native amd64 and arm64 CI rather than a small name
filter. Skip guards include nested subtests. Initial full-shim execution exposed
two capability skips: the writable overlay-scaffolding contracts additionally
need test-container `DAC_OVERRIDE`. This was isolated experimentally; relaxing
seccomp/AppArmor did not resolve the failure, while adding only that capability
did. The final successful commands keep default syscall policies, network off,
read-only root, dropped unrelated capabilities and `no-new-privileges`.
No application or production Pod gains that test capability. The new actual
kernel whiteout/opaque composition test proves lower hiding, surviving upper/
sibling bytes, marker absence and inherited-symlink isolation.

The first expanded GitHub CI runs failed: ordinary unprivileged Linux execution
could not materialize the new whiteout fixture, and Ubuntu's overlay mounts
returned `EACCES` in the isolated artifact test container.
The whiteout test now reports the same explicit missing-capability skip as its
existing companion during ordinary unit execution; native CI still rejects
every skip. Only the artifact test container receives the additional capability.
The complete arm64 artifact suite was rerun successfully without skips using the
corrected container policy, and a separate non-root, capability-free execution
confirmed the explicit prerequisite skip. Adding that capability alone did not
resolve Ubuntu's mount denial. Native CI now explicitly exempts only these
isolated kernel-test containers from Docker's AppArmor mount policy and reports
kernel AppArmor/overlay diagnostics on failure. Default seccomp,
`no-new-privileges`, network isolation, read-only roots and narrow capability
sets remain enforced. GitHub CI run
[`39014892780`](https://github.com/microsoft/brewlet/actions/runs/39014892780)
passed all four jobs at `25b475a8b5e4d4af1ad7932fbdba12eb3e337cb4`,
including complete native amd64 and arm64 suites, host contracts and producer
integration. Native amd64 success is established by that runner, not inferred
from the local arm64 result.

Observability hardening adds native shim regressions for exact start-event
identity, no resolution event on failed/exec starts, and explicit serialization/
publisher failure logs without changing an already-successful runc start.
Event `generation` is a decimal string so protobuf's floating-point number
representation cannot round a valid 64-bit generation. The complete updated
native arm64 shim suite passed **25** cases/subcases without skips.
`go test -count=1 -race -mod=readonly ./...`, `go vet -mod=readonly ./...`
and `GOOS=linux GOARCH=amd64|arm64 go build -mod=readonly ./...` passed.
The actual-node driver now requires a bounded, exact-owned containerd event
observer and enabled worker metrics. It joins old/new task events and metrics
to durable runtime leases and actual Pod UIDs, checks cold-install wait events,
retained generation gauges, install counters/durations and separate shared-store
measurements. All **273** offline safeguards passed; those are evidence-parser
and lifecycle contracts, not proof that actual event delivery has run.
Actual execution of this new callback remains required.

The pinned kind image's `ctr` binary does not link the protobuf `Struct` type,
so its generic `events` command cannot decode the shim payload. This was checked
against the actual pinned binary before accepting any event evidence.
The callback now builds a test-only native Go subscriber that registers that
type, filters both namespace and topic, decodes the original protobuf envelope
and emits strict JSON without numeric generation conversion. Wire/envelope
contract tests, full Go race/vet checks and both Linux builds passed.
The superseded first observer CI run `39015465171` was cancelled, not counted
as passed; corrected actual-node delivery still requires execution.

Runtime-root validation was reconciled separately against §7.2. The complete
updated native arm64 node suite passed **48** cases/subcases without skips:
PATH presence/uniqueness and root-relative directory/link validation, `/app`
absence/emptiness, account/group/home mismatches and actual set-ID/file-capability
rejection. The isolated node test container alone gains `SETFCAP` to construct a
genuine Linux `security.capability` negative fixture; this does not grant any
application or production worker a new privilege. Ordinary unprivileged Linux
unit execution reports an explicit prerequisite skip for that fixture, while
native CI's existing no-skip gate requires it to execute. ROOT-02 and ROOT-03
are verified by these reviewed filesystem contracts; this is not additional
real scheduling or fencing evidence.

Direct native publication regressions exposed a production ordering defect:
an unsafe previous `current` symlink was rejected only **after** the new pointer
had replaced it. All four malformed-pointer cases reproduced that mutation.
Previous-pointer validation now occurs under the per-ID lock before publication
mutations; rejection retains the original durable pointer. The complete updated
native arm64 node suite passed **60** cases/subcases without skips, including
fresh-fence failures, exact root/metadata/source binding, immutable generation
collision, untrusted readback permissions and normal previous-root retirement.
The complete native provisioner suite passed **88** cases/subcases without skips,
including actual image-copy symlink isolation and FIFO/set-ID rejection.
Full Go race/vet checks and both Linux builds passed. ROOT-04 remains partial
until complete acquisition/publication evidence is reconciled; these component
results are not fabricated source-pull or scheduling evidence.

Fresh amd64 CI run `39014792810` passed **54** assertions, including actual CRI
accounting/eviction, authenticated delivery, Maven execution and all container
roles, then timed out waiting for the exact expired-authorization rejection.
Fixture destruction passed. That timeout is not accepted as expiry proof.
The original private Pod diagnostics were not exported, so its exact cause is
not asserted. Negative-start results now retain only UID-scoped classified
reason/deadline diagnostics, not arbitrary messages. To remove a known fixture
ambiguity, stopping real renewal now requires confirming that the suspended
worker holds neither the shared-cache exclusion lock nor the target ID lock;
a busy/failed probe resumes the exact worker before retry/failure. Real time
and authorization bytes remain unchanged. All **275** offline safeguards pass.

Observer CI run `39016172961` failed before application execution because Docker
archive transfer did not appear in the node's live `/tmp` mount. Transfer now
uses checked stdin into an invocation-owned executable path, not `docker cp`.
A separate pinned-node probe also confirmed that default tmpfs is non-executable;
the fixture does not weaken that policy. Checksum-identical stdin transfer and
native observer execution passed in the pinned node userland at an executable
test path. Updated actual-node event delivery and expiry execution remain
unverified pending another clean run.

`run-complete-39` reached **30** passing assertions but again refused the CRI
accounting write: available disk was **263,868,416** bytes against the unchanged
**1,157,627,904**-byte minimum. Exact-owned fixture destruction passed.
Removing redundant runtime preloads did not resolve the shared Docker VM's
capacity limit. Real-node CI run
[`39014792810`](https://github.com/microsoft/brewlet/actions/runs/39014792810)
was dispatched at `5673b4e93509265eb0798c31b19245f9c62ca78a` on a fresh runner;
its result is pending, and it predates the new observability callback.

This verifies ART-05 and complements real run 28 to verify PROC-02, PROC-03 and
SEC-02/03/04. Admission additionally tests explicit pod/container nonroot=false
and root primary GID; native node contracts reject every capability set, and
native shim contracts verify terminal/argv/cwd preservation and missing-state
non-delegation. No still-unverified requirement is auto-promoted by package
success alone.

`run-retirement-37` passed all **8** selected assertions at
`7107e88e26dced07a9aee58ddb1fd91975977d51`, production manifest
`2c419e9611fd1111468da1cbb4eddd7e7543574c689f2584b4ebd593f7e20943`.
Actual worker destruction preserved a detached original-UID obligation while a
different real Node served before evidence. Even after replacement task drain,
uninstall could not discard that history. An initially denied actor required
explicit attestor RBAC, after which original-identity external decommission
evidence resolved and remained immutable/retained across profile deletion and
successful actual uninstall. Survivor-only fixture destruction passed.
[`results/physical-retirement.json`](results/physical-retirement.json) preserves
the sanitized result. OWN-03 remains partial: this is not returning-physical-host
or literal recreated-Node cleanup proof.
`run-complete-38` passed the first 30 assertions but correctly refused the CRI
accounting write: measured node free space was 257,613,824 bytes, below the
unchanged 1,157,627,904-byte safety requirement. Its fixture destruction passed.
No shared Docker image, volume or builder cache was pruned. Inspection found
that the mandatory TLS-delivery driver still pre-imported all eight unused
import-only runtime copies before pulling freshly published golden targets.
Those redundant node imports are removed; the host producer images, explicit
vendor pins, exact golden bytes, fresh remote descriptors and mandatory
authenticated pulls remain. Installation now requires complete published
before/after source maps. This reduces test-node footprint rather than weakening
the headroom gate; the corrected actual combined run still needs to pass.

```sh
go test -count=1 -race -mod=readonly ./...
go vet -mod=readonly ./...
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -mod=readonly ./...
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -mod=readonly ./...
mvn -q -B -f maven-plugin/pom.xml verify
PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s integration-tests/live-v2 -p 'test_*.py' -q
PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s acceptance -p '*_test.py' -q
helm lint kubernetes/charts/brewlet
python3 acceptance/check.py
git diff --check
```

Go race tests and vet passed across all packages. Both Linux targets build.
The live-driver safeguards currently include 265 passing offline tests; those
are not real-node proof. Maven tests cover the optional Java producer and
BOM/scoped-GAV layer-set contracts.

Producer integration passed with the Linux CLI wrapper documented in
`integration-tests/fixtures/README.md`:

```sh
PYTHONDONTWRITEBYTECODE=1 python3 integration-tests/fixtures/check.py \
  --brewlet ./integration-tests/linux-cli.sh \
  --work-parent "$BREWLET_TEST_WORK_PARENT"

PYTHONDONTWRITEBYTECODE=1 python3 integration-tests/registry.py \
  --brewlet ./integration-tests/linux-cli.sh \
  --producer-registry-host host.docker.internal \
  --registry-platform linux/arm64 \
  --work-parent "$BREWLET_TEST_WORK_PARENT"
```

These runs built and host-served four real applications, packaged each twice
for two explicit platforms, checked deterministic targets and shared layer
digests, published Java through Maven, and executed 30 shared raw launch/ID/
platform cases through Maven and CLI. Java additionally published and consumed
its BOM-governed layer set without duplicate dependency layers and rejected
scope drift with unchanged dependency bytes. Real Distribution publication
verified signed layer-set provenance, rejected an untrusted key, and reused
dependency descriptors byte-for-byte.

The latest producer rerun additionally passed **all 65** published cases
through Maven's `conformance` goal and the actual Linux CLI with identical
normalized JSON, followed by the 30 applicable actual Maven `push` cases.
The shim-linked published-fixture test passed natively on Linux/arm64 using
the same read-only mounts and security shape now wired into the native CI
matrix. These are contract/integration proofs, not scheduling evidence.
With `ARTIFACTS` set to the session artifact directory, the native invocation
was:

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go test -mod=readonly -c \
  -o "$ARTIFACTS/shim-conformance-current/shim.test" \
  ./shim/cmd/containerd-shim-brewlet-v2
docker run --rm --platform linux/arm64 --network none --user 0:0 \
  --cap-drop ALL --cap-add SYS_ADMIN --cap-add MKNOD \
  --security-opt no-new-privileges --read-only --tmpfs /tmp \
  --tmpfs /src/shim/cmd/containerd-shim-brewlet-v2:rw,mode=755 \
  --mount "type=bind,source=$PWD,target=/src,readonly" \
  --mount "type=bind,source=$ARTIFACTS/shim-conformance-current,target=/tests,readonly" \
  --workdir /src/shim/cmd/containerd-shim-brewlet-v2 \
  --entrypoint /tests/shim.test \
  docker.io/library/python@sha256:34386ef0cb081344d7ec1c103ba398e6e9f64e9ab3a1509accc92a4e24a07258 \
  -test.v -test.run '^TestPublishedProducerShimConformance$'
```

The artifact test directory contains a byte-identical
`conformance/artifact-v2.json`; the actual CI invocation reads the checkout's
published file directly.

The public branch CI run for `09ac69428b41622f71ff6209e8c36132f8b72650`
[passed](https://github.com/microsoft/brewlet/actions/runs/38934713032).
That is contract/producer/native-overlay CI, not full real-node acceptance.

## Real-node evidence

The previous selected suite completed **cleanly**, without manual intervention:

```sh
PYTHONDONTWRITEBYTECODE=1 python3 integration-tests/live-v2/harness.py \
  --work integration-tests/live-v2/run-complete-05
```

The expanded `run-complete-06`, from published
`c212a96369ef7dd7505953c583452cc4791e3bd1`, **failed before installation** at
the TLS registry readiness probe. It provides no new runtime/lifecycle proof.
A focused reproduction identified missing X.509 authority-key identifiers in
the generated leaf certificate under Python's strict verification, and a
second defect where two stdin lines were passed to noninteractive `htpasswd`.
Certificate key identifiers/usages and single-line password input were fixed,
with regression tests and retained redacted registry diagnostics. A fresh
owned-registry probe then passed strict X.509/SAN verification, anonymous HTTP
401 and authenticated HTTP 200; no TLS checks were disabled. Expanded node
acceptance must still be rerun.

`run-complete-07`, from `13d9b752af439546c863c71ab2761dc1cab62490`,
passed that TLS/auth startup boundary but also failed before installation:
the pinned kind node does not enable CRI's registry host directory by default.
The fresh-cluster configuration now explicitly sets that prerequisite before
node startup. A separate real-node setup probe passed effective containerd
configuration readback, strict TLS registry readiness and UID-recorded
workload pull-secret creation, then destroyed its owned cluster and registries.
This is prerequisite proof, **not kubelet/runtime pull or lifecycle proof**.

`run-complete-08` reached installation but failed its cold baseline deadline.
A reduced one-ID reproduction, with newly retained worker logs, identified
empty node-side CA files: the harness's owned-node `docker exec` helper did not
forward stdin. The helper now uses `-i` for supplied input, including an empty
payload; registry file transfers verify SHA-256 readback without printing
credentials. The focused real-node rerun passed metadata-only authenticated
TLS validation, cold OnDemand `available` publication and genuine startup-fence
release. Its lease-recovery marker came from the actual worker. This is not
four-language application execution or full lifecycle acceptance; the expanded
suite still requires a fresh pass.

`run-complete-09` passed the four-ID cold baseline and reached actual Java HTTP
and exec probing. It then failed a misplaced harness assertion: the new vendor
version comparison was running during initial execution, before any rotation,
against the same old Pod twice. The comparison now runs only after the
replacement Pod is ready and rejects reuse of the original Pod as the
replacement. A focused regression checks distinct old/new exec targets. This
was a test defect, not evidence that a rotated runtime failed; no vendor-update
pass is claimed from that run.

`run-complete-10` passed actual Java remote-pull startup, HTTP/dependency,
exec/probe, identity and read-only/security assertions. Its second start of the
same digest failed because the final launch-layer blob was absent from
containerd after unpacking. The pinned node defaults to
`discard_unpacked_layers=true`. Initially treating that setting as immutable
was incorrect: the retained cold-start contract requires false, as the later
run12 investigation below established.
This exposes a **production warm-cache gap**, not an override or vendor-version
failure. Protected, digest-verified original-byte resolution caching is now
implemented without shim registry access or weaker CRI identity checks.
Native Linux/arm64 tests passed cold-to-discarded-launch and all-content-missing
warm resolution, absent/tampered/rejected cache denial, and root-only
lease/mount/grace/fresh-fence collection contracts. Root race/vet and both Linux
builds passed. These are contract tests; real repeat-start acceptance remains
pending. Positive readiness
checks now report terminal Pod errors immediately instead of waiting ten
minutes for a Pod that cannot become ready.

`run-complete-11`, from `cc984dc1`, passed all four languages' actual startup,
repeat starts/overrides, dependencies, env/identity, exec/probes and read-only
checks. All four real vendor-version updates then passed without republishing
the application artifacts; old tasks/exec retained their old runtime leases.
[`results/vendor-runtime-updates.json`](results/vendor-runtime-updates.json)
exports those 16 selected successes with the full command explicitly failed.
The later accounting fixture retained its old `Never`/no-secret delivery
settings and received `ErrImageNeverPull`. It now inherits only the selected
image, pull policy and pull secrets from the workload factory, preserving its
separate no-volume, writable-root and bounded-storage safeguards. Formal
delivery and new lifecycle/controller callbacks were not reached in this run.

`run-complete-12` repeated all language/vendor-update successes, passed actual
CRI growth/eviction again, and verified four positive authenticated TLS
application/runtime-source pulls plus unauthorized, untrusted-certificate,
corrupt and unsafe-layer denials.
[`results/authenticated-pulls-and-accounting.json`](results/authenticated-pulls-and-accounting.json)
preserves those nine selected assertions, not a full-suite pass.
The fresh malformed-artifact case then lost its launch blob **before first
Create**, so no validated warm cache could exist. Proposal 0009 section 8.2
retains SPEC section 6.4, whose explicit cold-start requirement is
`discard_unpacked_layers=false` (`specs/SPECIFICATION.md:378-382`).
That required effective setting was missing from managed host activation.
Production activation now sets it at the correct v2/v3 plugin paths and gates
baseline readiness on effective configuration; exposed live CRI fields are
checked without inventing absent values. Native Linux/arm64 root contracts
passed exact original-byte restoration/rollback, idempotency and non-mutating
`none`-policy denial. Root race/vet passed. This is not merely a test-fixture
setting; fresh-node confirmation remains pending. Warm-cache retention remains
useful after original-byte verification but is not a substitute for this
first-validation prerequisite. Terminal negative cases now surface unexpected
reasons immediately.

`run-complete-13` passed all selected remote delivery cases, including fresh
malformed/config-mismatch rejection, real native binary execution and wrong-
platform pull rejection. Inventory deletion remained blocked by a real lease,
then drained/finalized; same-name recreation got a new UID/generation one and
the shim rejected the old-epoch barrier. Actual five-minute stopped renewal,
authorization-expired bound rejection and latest-target renewal also passed.
[`results/delivery-and-authorization.json`](results/delivery-and-authorization.json)
exports those 19 selected successes; full acceptance remains false.
The run then stopped in controlled worker restart because successor discovery
read Pod state twice and raced predecessor termination. It now uses one
identity-checked snapshot, retries only transient absence/non-running state and
excludes the known predecessor CID before PID inspection. No recovery success
is claimed yet. The separate persistent failed-Create lease was traced to
`privilege-bound`: security validation failed after leasing but before initial
state publication, and generation-wide mount evidence confused healthy
siblings with that failed task. New leases now bind protected task-specific
bundle/snapshot provenance under Create/recovery locking, security preflight
precedes leasing, and failed Create/Delete cleanup remains gated on complete
actual task/exec/mount absence. Legacy unscoped leases stay conservative.
Root Go race/vet, native Linux evidence/mount-scope/warm-cache contracts and both
Linux builds passed. Real recovery/GC confirmation remains pending.

`run-complete-14` repeated the 50 preceding assertions and successfully selected
the actual replacement worker container. It then timed out reading recovery
markers because the fixture consumed only `crictl logs` stdout, while Go's
worker logger writes stderr. Retained diagnostics contained repeated complete
18-task recovery markers and no prior missing-initial-state warning.
Worker-log observation now combines both streams under the same CID/Pod UID
checks, including uncertainty/resume prefix comparisons; a regression verifies
stderr markers and rejects stale-prefix-only observations. This is a fixture
read defect, not proof that production recovery failed or that the full
recovery/GC scenario passed.

`run-complete-15` encountered an earlier fixture conflict while rotating the
Python inventory: generic `apply` replayed a GET snapshot's status, controller
journal and resourceVersion while status was changing. Inventory spec edits
now submit only `/spec`, with atomic UID and spec-generation tests; they never
replay status, finalizers or controller annotations. The same helper is used
for base rotations, delivery profile entries, pin-mode changes and control
inventory edits. Generic `apply` rejects observed inventory snapshots.
A reduced real-API probe verified successful updates across non-spec
resourceVersion churn while preserving concurrent metadata, and rejection of
stale spec intent without changes. It wrote no status, claims or authorization
and is not a scheduling/recovery proof.

`run-complete-16` passed actual worker replacement, fresh exact-task lease
recovery, containerd daemon PID replacement and unchanged application
task/lease/Exec state.
[`results/worker-recovery-selected.json`](results/worker-recovery-selected.json)
records that assertion while preserving the failed full command.
The next fixture overconstrained paused-shim behavior to one error-log string:
known-active task evidence can legitimately retain a lease without that error.
Focused one-task probes confirmed this distinction rather than weakening the
collector. The uncertainty case now temporarily pauses the exact owned
containerd PID/start identity, proves its previously valid read-only
task/mount query actually times out, checks the original lease and kernel
runtime mount remain, resumes in `finally`, and requires fresh complete
production recovery. The reduced real-node probe passed with timeout exit124,
unchanged task PID, retained lease/root and fresh recovery after CONT; no
protected records or clocks were edited. Full retired-generation/GC lifecycle
acceptance must still run.

`run-complete-17` passed 55 assertions, including real inspection uncertainty/
recovery, positive retired-generation grace and actual post-drain collection,
cold OnDemand rotation, lease-protected idle TTL and eager reinstallation.
The remaining direct-CRI fixture failed before application start because its
sandbox omitted the pinned node's systemd cgroup parent. Reduced probes also
found missing explicit CRI client labels, unresolved image-config identity,
and kubelet's legitimate garbage collection of an unmanaged CRI sandbox.
The fixture now declares the actual resolved config identity plus unchanged
requested artifact digest, uses a systemd parent and suitable RPC budget, and
temporarily pauses only the identity-checked owned kubelet, always restoring
it before API cleanup. A focused real-CRI run passed active main/exec/mount
lease and revocation-finalizer protection followed by complete normal
runc/CRI process/mount drain and root reclamation.

**Proof boundary:** delegated runc/CRI normally drains detached exec processes
with main exit on this backend. The fixture must not require or manufacture
orphan survival, change the backend lifecycle, or label that absent behavior
as verified. Its receipt explicitly records whether an exec was observed
after main exit; forced orphan survival remains unverified. Native conditional
task/exec/mount uncertainty contracts and real active-reference/drain evidence
remain separate. No complete lifecycle/controller-suite pass is claimed yet.

`run-complete-18` passed 57 assertions and completed the selected lifecycle
callback, including actual authorization expiry/recovery, worker and daemon
recovery, uncertain-inspection retention, retired-generation collection,
OnDemand mode/idle behavior, and direct-CRI active-reference protection and
normal drain. It then failed in `control-edges`: the positive setup write for
the API CAS primitive probe raced a legitimate Node-controller update. This
was not the intentionally stale write being tested. Setup now re-reads and
revalidates the exact owned synthetic Node before retrying only conflicts,
within a 30-second bound; the negative stale write is never retried.
A fresh isolated actual-API probe passed both stale-resourceVersion and
recreated-UID rejection after this correction, with fixture destruction
successful. Controller restart scenarios and final production cleanup have
not passed as one complete expanded run.

`run-complete-19` again passed 57 assertions through the selected lifecycle
callback. It then encountered another ordinary Node-controller update during
the CAS probe's guarded fixture deletion. Deletion now retries only conflicts,
re-reading and revalidating ownership and UID on each attempt while preserving
both UID and resourceVersion preconditions. Replacement or foreign objects and
unrelated API failures abort rather than retry. The ownership tracker is cleared
only after confirmed deletion. Five successive complete synthetic-Node
create/CAS/delete/recreate/old-UID/delete cycles passed on a fresh actual API,
with successful fixture destruction. This remains API-primitives evidence,
not proof of production claim release.

The focused `probe-control-20` run passed 31 assertions, including the complete
real API CAS primitive probe, then timed out waiting for its first controller
workload. Actual Pod events showed `FailedToRetrieveImagePullSecret` and an
unauthorized registry pull: the dedicated controller namespace did not contain
the base namespace's pull secret. Delivery now creates a namespace-local secret
only in a recorded, UID-validated owned namespace, with name-only output and a
metadata-only UID read. It never copies server Secret objects or overwrites an
existing secret. The controller callback installs that secret and preserves the
canonical Pod factory's pull policy and secret references. Offline safeguards
cover destination ownership, replacement, duplicate creation and secret-output
boundaries; the corrected cross-namespace controller launch still needs actual
node validation.

The corrected `probe-control-21` run passed 36 assertions, including actual
authenticated cross-namespace launch, double opt-in/default-deny behavior,
native Deployment/StatefulSet/DaemonSet strategy outcomes, epoch/idempotency
barriers, superseded deferred targets, global pacing and real application
deadlines, and bound-node exclusion/defer evidence. It timed out during recovery
because the fixture counted 356 terminal kubelet `NodeAffinity` rejections as
live replicas alongside the actual Running/Ready replacement. Readiness now
excludes only terminal history, still requiring native-controller completion
and exactly one nonterminal Running/Ready Pod. Pending or terminating replicas
still block success. The recovered Pod snapshot, not historical rejected Pods,
is used for actual generation verification.
[`results/controller-strategies-selected.json`](results/controller-strategies-selected.json)
preserves sanitized assertion outcomes and the failed overall outcome.
Excluded-node recovery verification, provider-pin cases and final production
cleanup remain pending a corrected actual-node run.

`probe-control-22` completed successfully against
`d7d8f2d1d465e121a0e25e1a461344c6341f0ac2`: **43 assertions passed**,
including excluded-node recovery, real TLS provider/admission refusal,
OnDemand/stale-capability all-ID fencing/recovery, and actual production
cleanup. The claim-release admission interceptor committed a harmless annotation
during the real operator's release PUT, causing a genuine API CAS conflict;
the operator recorded `CleanupBlocked`, re-read and revalidated ownership and
teardown state, then released the claim successfully. Active roots/leases and
finalizers remained protected until actual drain; fixture destruction also
passed independently.
[`results/controller-cleanup-selected.json`](results/controller-cleanup-selected.json)
contains only whitelisted public evidence.

This was the focused driver
`PYTHONDONTWRITEBYTECODE=1 python3 "$SESSION_ARTIFACTS/probe-control-acceptance.py"`,
using the existing harness preflight, cluster/build, delivery bootstrap,
install/startup, four-language execution/rotation, `control_edges.execute`,
`pinned_revocation`, and `before_cleanup`/`production_cleanup`/`after_cleanup`
sequence. Accounting, full delivery and lifecycle callbacks were explicitly
omitted and recorded as gaps, not claimed as passing in this invocation.
The complete reproducible repository entrypoint remains
`python3 integration-tests/live-v2/harness.py --work integration-tests/live-v2/NEW_RUN`.
Physical recreated-Node cleanup, cold-install deadline/recovery, stale pins
through production uninstall, and genuine late native capacity remain separate
unverified obligations. The strict ledger now has 39 verified and 62 not fully
verified requirements; successful focused cleanup is not full acceptance.

`run-complete-23` passed 43 assertions through accounting and the complete
selected remote-delivery callback, then failed on the first lifecycle OnDemand
installation. Production worker logs exposed a staging-monitor shutdown race:
successful acquisition canceled the monitor's in-flight approval API read,
and that internally induced `context canceled` was reported as an installation
failure, withdrawing the capability. Acquisition now uses a distinct local
completion cancellation cause and joins the monitor without treating only
that shutdown-induced read cancellation as authority loss. Real monitor errors,
monitor deadlines, installer failures and parent cancellation remain failures;
the fresh publication fences are unchanged.
Deterministic synchronized race tests exercise the in-flight read at stage
completion rather than relying on timing; actual-node confirmation remains
pending.

[`results/clean-real-node.json`](results/clean-real-node.json) records all 36
selected assertions passing against checkout
`f70a2c40fa1139fbc38ef0d4da4e964428bf5164`, with production-source manifest
`7c286c5590d9e8c604fe64c6cd95aa474972b3f6cf98026d6f498a05acf9bc0b`.
This includes four-language execution and rotation, fail-closed root/privilege/
barrier/epoch checks, actual CRI usage and eviction, real admission-interceptor
double refusal and node-wide fallback, revoked bound starts, active-lease
cleanup protection and genuine drained-root/finalizer/claim cleanup. Owned
fixture clusters and image tags were removed.

The suite still explicitly reports `fullAcceptance: false`: its published gaps
are preserved, and imported execution does not prove kubelet pull or provider
autoscaling. The diagnostic runs below remain useful regression history, not
the final clean-suite verdict.

[`results/four-language-execution.json`](results/four-language-execution.json)
records actual Linux arm64 Java, Python, Node.js and .NET startup, HTTP,
dependencies, exec, exec probes, Kubernetes overrides, identity/HOME/PATH,
read-only root and packaged files, and writable nested volumes. The run failed
later on a test-driver webhook-name assumption before rotation. Discovery was
corrected; this failed run is not reported as a complete suite pass.

[`results/four-language-rotation.json`](results/four-language-rotation.json)
records the later run's successful source rotations for all four languages.
Initial tasks and long-running execs kept their original leased runtime, while
new barrier-bearing pods used generation 2 with unchanged application digests.
Root, privilege, malformed-barrier, wrong-epoch and future-generation starts
were denied with their required reasons. This run failed later because the
test interceptor omitted a typed Forbidden response. The wire response was
corrected; provider-refusal success is not inferred from that failed run.

[`results/cri-accounting.json`](results/cri-accounting.json) records a separate
successful native-node accounting callback:

* real CRI writable-layer usage: 8,192 to 16,789,504 bytes;
* measured growth: 16,781,312 bytes, exceeding the 16 MiB threshold;
* a bounded 80 MiB snapshot-upper write exceeded the 48 MiB limit;
* kubelet reported `Failed/Evicted` for local ephemeral-storage excess;
* no `emptyDir` accounting proxy or kubelet configuration change;
* no global DiskPressure or change to unrelated running workloads.

The callback used the exact component image digests in its receipt. It is not
evidence that the entire current-checkout acceptance matrix passed.

Separate private-node checks also proved profile deletion retained its
finalizer, claim and runtime root while two real applications remained alive.
After UID-preconditioned application deletion, normal workers removed the root
and shim, restored the non-Brewlet handler configuration, terminated cleanup,
and released the claim. Fixture cluster destruction is never counted as that
production-cleanup proof.

All application/runtime delivery in these node tests used verified local OCI
imports with `imagePullPolicy: Never`. It does not prove kubelet registry pull,
registry TLS/authentication, or provider autoscaling.

## Remaining proof and implementation boundaries

The selected four-language rotation/barrier/pinned-label/revocation suite now
passes, but the remaining ledger requirements still need evidence. In particular:

* real alternate-architecture capacity and unsupported-platform pulls;
* actual provider scale-from-zero, or its explicitly distinguished API proof;
* OnDemand pinned-label rejection and stale-pin cleanup/recovery;
* epoch delete/recreate with retained roots and old workload barriers;
* resource-version/recreated-Node claim-release races;
* shim restart, orphan exec, offline authorization expiry and idle collection;
* real automatic-restart strategies, pacing and StatefulSet constraints;
* real-node telemetry scraping and Kubernetes-native event mirroring.

The node implementation currently requires conventional systemd/containerd
paths and the overlayfs snapshotter. Runtime platform variants and pool-wide
architecture coverage remain limited as documented in `provisioner/README.md`.
Kubernetes-native node event mirroring is still an implementation gap.

```sh
python3 acceptance/check.py --require-complete
```

This release gate is expected to fail until the remaining requirements have
appropriate successful evidence. A green build, a partial real-node run or an
offline fake-client test does not override it.
