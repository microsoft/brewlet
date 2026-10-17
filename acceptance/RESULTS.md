# Brewlet 2.0 validation status

**Incomplete acceptance; not a release claim.** Proposal 0009 remains the
contract. The 101-entry ledger deliberately distinguishes verified requirements
from implemented or partially tested behavior.

## Latest combined actual-node result

**`run-complete-24` passed all 70 selected assertions with no failures.**
Tested checkout: `ee166aed665c1317c58fb4f44220699ce7dbab9e`.
Production-source manifest:
`fc993e92cf78fb4796befe1a02bbf5f62eea72ed183f657880490894206592d5`.

```sh
PYTHONDONTWRITEBYTECODE=1 python3 integration-tests/live-v2/harness.py \
  --work integration-tests/live-v2/run-complete-24
```

Exit status was zero. The same fresh native arm64 node completed authenticated
TLS delivery, four-language vendor updates without app rebuilds, actual CRI
accounting/eviction, authorization expiry, worker/daemon recovery, uncertain
inspection retention, retired-generation/idle collection, native controller
strategies/pacing/deadlines, provider-pin fencing, and production cleanup.
Actual operator claim release encountered an injected API resourceVersion
conflict and completed only after fresh ownership/teardown revalidation.
Production cleanup and subsequent disposable-fixture destruction each passed.
No manual intervention was recorded. The staging-monitor correction is now
confirmed through the previously failing actual OnDemand installation and the
complete combined callback chain.

[`results/expanded-real-node.json`](results/expanded-real-node.json) is the
whitelisted public result, not private logs, credentials or kubeconfig.
Its historical `gaps` array retains initial legacy strings, including some
superseded by its passing assertions. The driver now reports the remaining
unverified cases rather than those stale strings; the current ledger below
remains authoritative.

**This is selected-suite success, not complete Proposal 0009 acceptance.**
The ledger has **43 verified and 58 not fully verified requirements**.
Only four additional complete rows were promoted from this run: INV-06,
OD-06, E2E-02 and E2E-08. Worker recovery is not proof of recovery after killing
the shim task-server; normal backend exec drain is not forced orphan survival.
Physical recreated-Node cleanup, genuine late alternate-native capacity/provider
autoscaling, controlled cold-install deadlines, stale pins through production
uninstall and the remaining detailed ledger obligations are still unverified.

## Reference and branch

Reference main: `fa7085335b220e29df6affbece4558c5f1259ecb`.
Remote branch: `brewlet-2.0`.
Parentless root commit: `5d9071f6ad2544121c2d41fd2a19db92d8f0be37`.
The initial tracked index was checked empty before preserving documentation.
No implementation ancestry was inherited, and no existing remote branch was
overwritten.

## Executed successfully

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
The live-driver safeguards currently include 229 passing offline tests; those
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
