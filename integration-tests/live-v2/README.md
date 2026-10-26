# Private Brewlet real-node execution acceptance

The explicit Wasm companion is separate from the Python qualification suite:

```sh
PYTHONDONTWRITEBYTECODE=1 python3 integration-tests/live-v2/wasm.py \
  --work integration-tests/live-v2/run-wasm-01
```

It uses the existing native runc path, pinned platform-built Wasmtime/Spin roots
and explicit Go WASIp1/Rust Spin SDK HTTP guests with managed dependency layers.
It requires the pinned Rust toolchain/target in addition to Go.
See [Wasm scope and instructions](../../docs/webassembly.md);
the native arm64 companion has its own successful scoped receipt, not proof
inferred from historical receipts or native runwasi/component interfaces generally.

Run from the checkout root, with a **new evidence directory under this folder**:

```sh
PYTHONDONTWRITEBYTECODE=1 python3 integration-tests/live-v2/harness.py \
  --work integration-tests/live-v2/run-20261016-01
```

Nothing is executed at import or during `--help`. The harness never accepts an
existing cluster, kubeconfig, node, registry, provisioner image, or current
context. It creates one invocation-unique single-node kind cluster, its own
private kubeconfig, an owned registry, namespaces and checkout-built images.
Every kubectl/Helm command explicitly selects that kubeconfig and context.
It does **not** touch a running shared cluster or use the tier reset helper.

Inventory webhook startup uses a bounded 60-second dry-run reachability wait.
Only transport failures tied to this invocation's inventory webhook/service
are retried, including an API-server webhook request deadline during startup.
Admission denials, foreign destinations and TLS certificate-identity failures
fail immediately; application assertions and installation deadlines are not
relaxed by this readiness check.

The branch's `.github/workflows/e2e.yml` is reused by PR and full validation and can
also be dispatched manually at an exact `expected_sha`. Its default `pr` scenario
is the real Python baseline (`harness.py --scenario pr`, verified with
`qualify.py --verify-pr`): admission/startup, execution, rotations,
observability, operator restart, protected CRI identity and normal cleanup.
Python is the only qualification fixture; PR, full and promotion validation
never build or install the Java, Node.js, .NET or PHP toolchains. The receipt
records `selectedLanguages` and a gap for the node-wide fallback fence against
another runtime ID (INV-05), which needs a second runtime.

The Python fixture, the Java, Node.js, .NET and PHP example fixtures and the
WebAssembly examples run weekly against `main` in the non-gating
`language-e2e.yml` workflow, one matrix job each. Each fixture job runs
`harness.py --scenario runtime --without-pinned-scenarios --local-language
<name>` (Java uses `--scenario maven`, adding the Maven producer); the
WebAssembly job runs `wasm.py`. It can be dispatched manually for any branch,
either architecture and one or all languages. Their results are never qualification evidence.
It deliberately omits the long fault/accounting/companion matrix, which remains
mandatory before promotion. Neither `PR validation` nor `Full validation`
can pass if a required E2E job fails, cancels or skips; pull requests that do not
impact runtime, fixture or harness code skip it (see `acceptance/README.md`). The
same `pr` scenario is a bounded manual debugging route that actually reaches the
protected-identity phase. Only the `pr` scenario restores Actions caches: the
host Go build/module caches and the image builds' BuildKit Go build cache mount,
seeded before the harness and extracted after a successful run. Promotion
scenarios never use those caches.

The `complete` scope additionally selects the core fault/accounting coverage and
all eight companion suites, with a 180-minute job deadline. Managed native/Wasm
composition remains separate on each architecture. The job exports each private
result through `acceptance/export_result.py` and uploads only whitelisted public
identities/outcomes, never raw observations or private work directories. Generated
receipts are Actions artifacts, not source files; see the
[artifact-index procedure](../../acceptance/README.md).

For focused controller and production-cleanup debugging, use the explicitly
partial scenario:

```sh
PYTHONDONTWRITEBYTECODE=1 python3 integration-tests/live-v2/harness.py \
  --scenario control --work integration-tests/live-v2/run-control-20261017-01
```

The workflow's `scenario` input defaults to `pr`; selecting `control`
retains real TLS bootstrap, startup fencing, Python execution, runtime
rotations, observability, controller/provider-refusal cases and production
cleanup, but omits accounting, formal remote-delivery negatives, Maven-node,
container-role and lifecycle callbacks and the separate companion suites.
Results and public exports retain `selectedScenario` and explicit omissions.
A focused pass is not a complete-suite or full-proposal pass.

Complete qualification additionally selects current assertion coverage, not
historical declarations:

* Real API admission runs the existing security/defaulting/affinity/hint checks
  against an owned zero-node profile. A temporary object-selector exclusion
  applies only to invocation-labeled profiles while constructing invalid mirror
  and conflicting-profile fixtures; Pod admission remains enabled and must reject
  them. A fixture-only finalizer holds genuine deletion long enough to observe
  exclusion. Cleanup first corrects an exact-owned conflicting profile to a
  valid disjoint empty pool and waits for actual current-generation ownership
  initialization, the matching provisioning snapshot and no targets. A spec
  patch alone is not reconciliation proof. Only then may UID/resourceVersion
  preconditions request normal deletion. The same guard covers the original
  invalid-mirror fixture and startup's zero-node fixture. Cleanup removes only
  the test hold finalizer with UID/resourceVersion/value tests, retries only
  freshly revalidated conflicts, and restores the exact owned webhook even when
  cleanup fails. Already-deleting uninitialized profiles retain production
  finalizers and fail explicitly; no status or authority is invented.
  Repeated cleanup first checks authoritative absence and never patches an
  already-equal spec. Immediately before conditional deletion, the harness
  captures an immutable exact-owned witness: UID/name/owner, canonical spec and
  provisioning snapshot, resourceVersion and genuinely initialized current
  provisioning generation with no targets or retirement obligations. Only an
  acknowledged UID/resourceVersion-conditional deletion retains this witness
  for later observations and repeated cleanup; it is never reconstructed from
  an already-deleting object's status.
  Kubernetes [registry `markAsDeleting`](https://github.com/kubernetes/apiserver/blob/v0.37.0/pkg/registry/generic/registry/store.go)
  advances a positive generation once when first marking a non-graceful object
  with pending finalizers for deletion. Metadata-only finalizer updates do not
  advance the [custom-resource spec generation](https://github.com/kubernetes/apiextensions-apiserver/blob/v0.37.0/pkg/registry/customresource/strategy.go).
  Brewlet's no-target production deletion path checks initialized ownership and
  its own current-generation CleanupComplete condition, but does not reprovision
  an object being deleted.
  Accordingly, a deleting fixture must have a deletion timestamp, zero deletion
  grace, an advanced resourceVersion and exactly the witnessed generation plus
  one, while UID/owner/name, canonical spec/snapshot and initialized provisioning
  generation remain witnessed and obligations remain empty. This is not a
  general allowance for generation drift. Missing witnesses, stale pre-delete
  authority, changed identity/spec/snapshot, additional generation changes or new
  obligations fail closed. Hold release revalidates these invariants and the
  empty Node pool before removing only an actually present test hold using fresh
  UID/version/value tests. An absent hold causes no patch. Disappearance proves
  object deletion, not production host cleanup.
  Assertion failures remain the primary exception even when fixture cleanup or
  webhook restoration also fails. Each independent cleanup is attempted; every
  secondary error is recorded explicitly without replacing the original error.
  Private results retain exception text. Public `fixtureLifecycle` diagnostics
  use a second typed allowlist: fixed fixture/phase identifiers, boolean UID/
  owner/spec matches, numeric generation freshness and empty-target/retirement
  predicates, plus primary/secondary stage and bounded error category. No raw
  object, UID, claim, spec/status, credential or exception text is exported.
  Each error retains its own predicate snapshot so later cleanup observations
  cannot overwrite the original failure's state.
  These diagnostics explain the observed predicate, not an inferred cause for
  old attempts whose private snapshots were not uploaded.
* A production worker with deliberately missing authority has real writable host
  access but must exit on its initial fence without changing runtime/cache/shim/
  containerd state. It never edits a claim or a cleanup receipt.
* Actual published root paths, root-owned immutable metadata, native platform,
  source marker and v2 readiness/diagnostics are observed. An actual operator
  restart followed by new source rotations must preserve monotonic journaled
  generations and existing task leases.
* A native probe reads protected containerd CRI metadata and verifies the native
  OCI descriptor graph/config using the production resolver and verified content
  cache. Like the event observer, its invocation-unique mode-0700 binary lives
  in the owned node's `/usr/local/bin`, not `/tmp`: pinned
  [kind Docker provisioning](https://github.com/kubernetes-sigs/kind/blob/v0.33.0/pkg/cluster/internal/providers/docker/provision.go)
  creates `/tmp` as a tmpfs without an exec override, and
  [Docker tmpfs defaults](https://github.com/moby/moby/blob/v28.5.1/daemon/oci_linux.go)
  include noexec. Transfer and cleanup retain collision/digest guards. It still
  uses the same identity-checked kind-node `docker exec`, default configured
  node identity, containerd socket/namespace and protected cache; no host/workload
  container substitution, new capability, chmod of production paths, remount or
  mount/LSM policy change. The pinned node image has no non-root USER override
  on either architecture. This configuration/source diagnosis does not invent
  unexported historical effective UID, mode, mount flags or LSM observations.
  The observer already uses the executable node-local path; cold-RPC JSON and
  observer PID data remain non-executable `/tmp` files.
  Real admission-bypassing forged artifact hints must not change the
  executing artifact. Root primary and supplementary group starts must be
  independently refused by the shim.
* A genuinely installed OnDemand runtime remains a participant during real
  invalid-root retries. The actual configured ten-minute node progress deadline
  must elapse before confirmed withdrawal/quarantine, with Node UID, observed
  generation and reason intact. No status timestamps or clocks are changed.
  Corrected source and actual Eager mode must recover normally.

The cold companion separately clones an actual owned kubelet container/sandbox
configuration for one invocation-owned CRI probe while TLS transfer is held.
It temporarily stops only that owned worker's initially active kubelet while the
probe runs, so Pod reconciliation cannot race or garbage-collect the additional
CRI container. It revalidates the actual sandbox after stopping the service and
restores the kubelet and real Node Ready condition even after a probe/stop
failure. No shared kubelet, clock, or Node status is edited.
Its real start request must return `RuntimeImageInstalling` in the default
thirty-second window (29 to less than 40 seconds including command overhead),
below the observed 120-second kubelet request timeout, without any workload
process or premature current root. Profile-generation replacement must stop the
old installing worker and make its successor revalidate authority before transfer
release. The probe container/config files are removed only after exact identity
checks. These assertions need fresh native execution; offline guard tests are
not proof that the scenarios passed.

Control execution also deliberately offers an approved default-Eager runtime
whose runtime config declares the forbidden `PATH=/app`. The
real worker must reject that root without publishing `current`, keep effective
baseline readiness withdrawn/fenced, and leave a good-runtime canary
unscheduled. Removing the invalid required entry must release that same Pod
without replacing it or disturbing existing application leases; normal
inventory finalization must then clean the failed entry. This is an actual
installation/readiness failure, not a fake-client readiness result.

For hands-on **local** debugging, add `--retain-on-failure`. Failed runs still
exit nonzero and stop their helper processes, but leave the invocation-owned
cluster, registry and images available for inspection through the private
`--kubeconfig WORK/kubeconfig --context kind-CLUSTER` arguments. `result.json`
records destruction as `retained`, never successful cleanup or a clean pass.
Use its exact recorded Docker IDs and ownership labels when explicitly cleaning
up afterward; do not prune shared Docker resources. Successful runs still
perform their normal cleanup even when this option is set. CI does not enable
this debugging option.

When local Docker capacity cannot hold all runtime roots, an explicitly smaller
Python/controller debugging run is available:

```sh
PYTHONDONTWRITEBYTECODE=1 python3 integration-tests/live-v2/harness.py \
  --scenario control --without-pinned-scenarios --local-python-only \
  --retain-on-failure --work integration-tests/live-v2/run-local-python-01
```

This still uses checkout-built components, real TLS delivery, actual runtime
installation, execution/exec/rotation, controller/provider-refusal cases and
production cleanup. It builds and offers only Python runtime roots and imports
only the selected application; the host fixture helper still requires the
documented producer toolchains. Results explicitly retain `selectedLanguages`
and omitted-language/pinned-case gaps. The option is rejected for the complete
scenario or without the explicit pinned-case omission. A smaller local pass is
never full acceptance.

To exercise an example fixture locally (as the weekly language workflow does), run it in
a new owned environment; run several **sequentially**:

```sh
PYTHONDONTWRITEBYTECODE=1 python3 integration-tests/live-v2/harness.py \
  --scenario runtime --without-pinned-scenarios --local-language java \
  --retain-on-failure --work integration-tests/live-v2/run-local-java-01
```

Repeat with `node`, `dotnet` or `php`, after the previous environment is cleaned up.
This fixture selector belongs only to the local driver, not Brewlet's generic
CLI or runtime selection. The runtime-only scenario still requires actual
TLS delivery, first-use installation, launch/exec/probes, identity and mount
checks, invalid/epoch/future barriers, real vendor rotation without app
rebuilding, event/metric joins and production lease/root/claim cleanup.
Bound-Pod probes also reject duplicate generation and ID members with
`InvalidRuntimeGeneration`; successful execution under last-value-wins JSON
is not accepted as barrier conformance.
It explicitly omits controller/provider cases and the complete-suite callbacks;
results preserve those omissions. A runtime-only scenario requires an explicit
fixture and pinned-case omission and cannot silently replace complete coverage.

Additional existing callbacks can run locally with a single runtime fixture:

```sh
PYTHONDONTWRITEBYTECODE=1 python3 integration-tests/live-v2/harness.py \
  --scenario maven --without-pinned-scenarios --local-language java \
  --retain-on-failure --work integration-tests/live-v2/run-local-maven-01
PYTHONDONTWRITEBYTECODE=1 python3 integration-tests/live-v2/harness.py \
  --scenario lifecycle --without-pinned-scenarios --local-language python \
  --retain-on-failure --work integration-tests/live-v2/run-local-lifecycle-01
```

Run these sequentially. Both require the base real runtime execution,
rotation/observability and production cleanup. The Maven case additionally
runs the optional producer's bundled/BOM-managed artifacts on-node and rotates
their runtime without republishing. The lifecycle case runs the unchanged real
authorization-expiry, recovery, revocation, retention and collection callbacks.
Only the selected callback is added; omitted suites and fixture languages
remain explicit in results. Complete/default execution is unchanged.

The existing container-role, authenticated delivery-negative and CRI accounting
callbacks can likewise be selected with `--scenario roles`, `--scenario
delivery` or `--scenario accounting`, each with `--without-pinned-scenarios
--local-language python` and a new work directory. Run them sequentially, not
alongside other owned node runs. Their selected callback is mandatory and
unselected callbacks remain listed as gaps. Accounting retains its real
filesystem-usage/eviction assertions, unchanged disk reserve/write bounds and
unrelated-container safety checks; insufficient capacity fails rather than
reducing those safeguards.

Accounting waits for all unrelated running containers to be healthy before
capturing its baseline. Afterward, disappearance, identity/restart changes or
readiness loss still fail immediately with before/after identity diagnostics;
waiting for a healthy baseline does not relax the disk reserve or write limits.

The separate cold-install case uses two actual native nodes:

```sh
PYTHONDONTWRITEBYTECODE=1 python3 integration-tests/live-v2/cold_install.py \
  --work integration-tests/live-v2/run-cold-20261016-01
```

Its additional worker joins with a real startup taint and no offered pool.
After old workloads are running on the first node, it is enrolled as a genuinely
cold OnDemand node. A registry proxy preserves TLS verification and registry
authentication while withholding one fresh runtime layer. Cordoning the old
host makes native restart-created Pods land on the cold worker. The case holds
that real transfer beyond a 60-second workload deadline (longer than the shim's
30-second Create wait), requires Pod UID/node/barrier installation diagnostics
and concurrency-slot release, then releases the transfer and requires actual
latest-generation execution without another template patch. Neither protected
runtime records nor clocks are edited. This is not alternate-architecture or
provider-autoscaling proof. The manual E2E workflow runs this case after the
combined selected suite. Both exact-owned node/container identities are checked
before host access and fixture destruction.

## Prerequisites and immutable inputs

The unchanged-upstream companion uses the same private cluster/registry and
production builds, but imports native platform manifests directly from
`integration-tests/fixtures/runtimes/upstream.json` through the production OCI
registry importer. Manifest, config and layer bytes are preserved; no Dockerfile
or source container execution derives the runtime images.

```sh
PYTHONDONTWRITEBYTECODE=1 python3 integration-tests/live-v2/upstream.py \
  --work integration-tests/live-v2/run-upstream-01
```

It executes a shell-free Python application and a native application on
unchanged distroless roots, updates each language's approved source without
rebuilding its app, and retains old leases. A detached Python Exec retains its
exact PID/start/executable identity across real worker and containerd restarts,
then completes its own code and drains. This is process/reconnection evidence,
not a guarantee that CRI streaming connections survive daemon restart.
Revocation, lease drain, normal profile cleanup and exact-owned destruction are
mandatory. A separately identified synthetic root tests numeric identities,
missing account files/PATH and populated runtime `/app`; it is **not** presented
as an unchanged third-party source. Offline tar audits inspect verified blobs
without extracting to the host and record actual old-contract blockers.
This companion does not replace the full-userland .NET/security/control suite,
and arm64 execution does not qualify physical amd64 nodes.

Physical missing-host obligations and independent administrative evidence have
their own actual-node case:

```sh
PYTHONDONTWRITEBYTECODE=1 python3 integration-tests/live-v2/retirement.py \
  --work integration-tests/live-v2/run-retirement-20261016-01
```

It first stops the recorded extra worker and removes its exact original Node
UID. The same container, with its original runtime storage, must return under a
genuinely new kubelet-created UID. The controller must withdraw its templated
capabilities without granting a fresh claim or erasing the original obligation;
a real scheduler canary must remain blocked. Only fixture identity bookkeeping
changes to acknowledge the observed UID; no production claim, lease, receipt
or status is fabricated. The test then permanently removes that exact worker
container and volumes, never the control plane, and its returning registration.
The replacement must serve on a different real Node before any external evidence
exists. Original missing-host history must block deletion/uninstall even after
replacement tasks drain. A separate initially unbound actor must be denied
retirement creation, then explicitly bound to the chart's attestor role before
attesting the actually destroyed instance. Accepted immutable evidence must
survive profile deletion and real uninstall. This covers a real returning kind
host and external decommission resolution, not invented host-cleanup proof or
cloud hardware/provider autoscaling verification. A passed fixture build or
offline identity test alone does not verify the physical-return sequence.

The packed-content prerequisite and exact configuration reversal have a focused
actual-node case:

```sh
PYTHONDONTWRITEBYTECODE=1 python3 integration-tests/live-v2/activation.py \
  --work integration-tests/live-v2/run-activation-NEW
```

Before Brewlet owns the node, it records an explicit unsafe
`discard_unpacked_layers=true` fixture configuration. A real `none` worker must
fail without changing those bytes, publishing a shim/root or releasing
readiness. A normal profile spec correction to `validated` must repair effective
retention before a real cold application starts. Normal production cleanup must
restore the exact original SHA-256 and live containerd service, not just an
equivalent rendered config. Offline transform tests are not activation proof.
The same case uses positive `2s`/`1s` production GC settings to retain actual
leased layer entries beyond grace, then observes normal post-drain collection
and verified rematerialization of the unchanged application. It reads original
receipts and real time; it never edits publication times, task leases or clocks.
It also kills one dedicated task's actual Brewlet server after checking its
protected Pod/task, main-process cgroup, parent executable and process start.
The owned containerd daemon is briefly STOP/CONT guarded by its own identity;
unavailable evidence must retain the durable lease/root. Normal backend cleanup,
fresh fenced recovery and a new real task server must then succeed. This is not
a promise to resurrect the old runc task server or preserve a container after
its server is killed; no unrelated server is signaled.
The dedicated crash Pod explicitly shares its sandbox PID namespace. With the
server dead and daemon paused, terminating only its main process must leave a
genuine detached Exec alive with the exact leased upperdir. That surviving
process and mount must retain the lease; only subsequent normal teardown may
release it. The test does not assume default PID-namespace init-exit behavior.
It also rotates the installed OnDemand ID to a separately published fixture
whose explicit `PATH=/app` is invalid, without changing its root layers. Actual
timestamped worker failures must exhibit exponential retry spacing, withdrawn
capability and failed authorization while the old task/lease survives. A normal
source correction must publish a validated target and start the unchanged
application; this does not edit production authorization, roots or receipts.
The same focused suite checks delegated Exec output/exit status and an exact
owned initial task's normal SIGTERM handling, logs and nonzero exit status.
The task ID comes from its protected durable lease; unrelated processes are
never signaled. These lifecycle checks complement the complete Python
suite's CNI, volumes, resources, probes and controller behavior.

The stale-pin uninstall case creates its profile through the actual Helm chart,
not by fabricating release ownership:

```sh
PYTHONDONTWRITEBYTECODE=1 python3 integration-tests/live-v2/pinned_uninstall.py \
  --work integration-tests/live-v2/run-uninstall-20261016-01
```

It uses the real TLS provider-refusal interceptor, first retaining a live task
through a failed pre-delete hook, then draining the task while a stale capability
remains pinned. Operator/RBAC, finalizer, claim and startup fence must survive
failed uninstall. Only explicit pool-owner correction permits normal production
cleanup and successful Helm removal. No finalizer, cleanup receipt or Helm
ownership record is forged.
The pre-delete coordinator uses an explicit, value-scoped startup-fence
toleration so it can run when all Brewlet nodes are fenced. It remains a normal
unprivileged runc Pod, not a Brewlet application or host-namespace worker.

Native scheduling with initially no eligible ready pool capacity has its own
actual-node case:

```sh
PYTHONDONTWRITEBYTECODE=1 python3 integration-tests/live-v2/native_capacity.py \
  --work integration-tests/live-v2/run-native-20261016-01
```

The requested architecture is the runner's actual native architecture. Its pool
initially has no eligible nodes; the existing control plane is outside that
offering. The unchanged native Pod must be genuinely Pending, then remain
blocked by the real worker's boot-time startup taint despite template `ready`
labels, and execute only after authorized provisioning makes capacity available.
No architecture label is changed. An architecture-unconstrained image for the
other CPU must fail at the real kubelet pull rather than being represented as
rescheduling or autoscaling. This covers the §13.1 ready-capacity scenario, not
an alternate-hardware join or actual provider scale-from-zero. The manual
workflow executes it separately from the combined and cold-install cases.
The startup observation briefly pauses only the identity-checked owned worker
so the actual scheduler can observe template labels behind the real boot fence.
It accepts the pinned scheduler's generic `untolerated taint(s)` diagnostic only
with an exact Pod UID and that startup taint as the sole NoSchedule blocker.
The worker is always resumed in `finally`; clocks and authority records are not
changed, and a preexisting user pause is never undone.

Use a Linux Docker engine (native Linux or Docker Desktop) on amd64 or arm64,
with cgroup v2, at least 4 CPUs, 8 GiB Docker memory, and 25 GiB available disk.
This runs privileged real provisioning and restarts **its private kind node's**
containerd. It requires network access to the pinned upstream images and build
dependencies. An unreachable registry or missing tool is a failure, never a
skip.
The fresh kind configuration explicitly enables CRI's trusted-host
`config_path=/etc/containerd/certs.d` before the node starts. Delivery then
installs only invocation-owned CA/host credentials there and creates a
workload-controlled pull secret. This is a test-platform prerequisite, not a
modification of a shared node or proof that an image has been pulled.
Golden runtime builds remain in the host producer store until authenticated
publication; unused import-only copies are not preloaded into the test node.
This avoids duplicating every runtime before the mandatory remote pulls.
Installation refuses to proceed until both approved source revisions have
actually been published. Disk safety thresholds are unchanged, and the driver
never prunes shared images, volumes or builder caches to manufacture headroom.

On macOS the CLI is cross-built for the engine's explicit Linux platform and
runs in the pinned Python fixture userland, with read-only sources and a
writeable invocation work directory. This keeps automatic, unremovable macOS
provenance xattrs out of the Linux source filesystem without weakening the
producer's xattr rejection. An inherited `DOCKER_DEFAULT_PLATFORM` cannot
redirect the node or registry to a different architecture.

* Python 3.12+, Go 1.26+, JDK 21+, Maven 3.9+, Node.js 22+.
* .NET SDK **10.0.401**, as pinned by the fixture `global.json`; no roll-forward.
* Docker with Linux builds, kubectl compatible with the pinned node's Kubernetes
  minor version, Helm 3, and OpenSSL supporting `req -addext`.
* **kind v0.33.0** (checked exactly).
* Node:
  `kindest/node@sha256:4613778f3cfcd10e615029370f5786704559103cf27bef934597ba562b269661`.
* Registry:
  `registry@sha256:6c5666b861f3505b116bb9aa9b25175e71210414bd010d92035ff64018f9457e`.
* Before/after vendor bases and version probes are pinned in
  `integration-tests/fixtures/runtimes/rotation.json`; the runtime Dockerfiles
  accept only the explicitly supplied base used by this fixture pipeline.
  Their exact file hashes, checkout commit, tool versions, resulting approved
  runtime digests and application digests are saved in `result.json`.
* Production images are built from `provisioner/Dockerfile` and
  `kubernetes/Dockerfile` in this checkout. Current `kubernetes/deploy/crds`
  schemas are applied **before** Helm. No old chart schema is assumed current.
  Build contexts contain only the production Dockerfiles' source inputs, not
  this run's kubeconfig/keys. The copied production source snapshot is retained
  with a per-file SHA256 manifest; the built CLI binary is hashed separately.

Only kind and OCI image digests are exact tool/image pins; the other host tools
have the documented minimum versions and recorded actual versions. Dependency
repositories remain network build inputs. This is not a hermetic build claim.
Runtime revisions retain the same account/PATH/ABI and change the pinned vendor
release: Temurin 21.0.7 to 21.0.8, Python 3.13.5 to 3.13.6, Node 22.17.0 to
22.18.0 and .NET 10.0.0 to 10.0.1. Actual old/new exec version output must match
the inputs; `BREWLET_RUNTIME_REVISION` additionally identifies retained initial
and long-running exec environments. These are version-update fixtures, not a
claim that these old releases are currently secure or remediate a specific CVE.
Historical runs through `run-complete-05` changed only Env and are **not** vendor
version-update evidence.
Both revisions deliberately declare unusable runtime `Entrypoint`, `Cmd` and
`Healthcheck`, a different `WorkingDir`, root `User`, an unrelated exposed port
and volume. Successful application launch must still use the explicit
application descriptor, pod overrides and approved non-root account; only the
runtime config's `Env` contributes to execution. Generating these fixtures is
not itself real-node proof: use a successful run at the revision containing
these conflicting defaults.
The IDs are `fixture-python`, `fixture-java`, `fixture-node`, `fixture-dotnet`
and `fixture-php`;
their names intentionally do not invent unverified language-version promises.

## Mandatory selected assertions

The default run fails on any missing protocol, reason, transition or evidence:

* Startup-tainted, template-labelled node does not schedule the application.
  Admission rejects both wildcard and exact startup-taint tolerations.
* A zero-node offering produces admission warnings naming its profile.
  Multi-term affinity, `matchFields`, architecture/node selectors, existing
  preferred rules, and an explicitly empty required selector survive admission.
  This is API/scheduler proof, **not provider scale-from-zero**.
* The actual worker establishes UID-bound ownership, installs the shim/handler,
  forwards annotations, publishes OnDemand-only baseline `available`, and
  releases the fence before **any current runtime root exists**.
* The Python fixture is built and host-probed using the existing fixture
  builder, then explicitly packaged by this checkout's generic CLI with
  distinct dependency/application roles and one final launch layer.
* In the Java job of the weekly language workflow (`--scenario maven`), not the default
  run, the optional Maven producer separately emits both bundled and BOM-managed
  Java artifacts. Their exact OCI targets are published unchanged through the
  authenticated registry and run through the same RuntimeClass, shim, exec,
  probe and non-root/read-only checks. A further approved Java runtime update
  retains their initial leases and moves replacements to the new generation
  without republishing either application. This adds no Java behavior to core.
* First use executes the real Python HTTP application through kubelet/containerd/
  shim/runc, imports its separate dependencies, then passes relative-executable
  `kubectl exec` and actual exec readiness probes.
* Initial and exec JSON reports assert argv, default and overridden cwd,
  environment precedence, runtime-owned PATH, synthesized HOME and UID/GID.
  Explicit command/args/cwd overrides serve real HTTP. Separate custom-UID pods
  verify independently defaulted GID and `/tmp` HOME fallback in initial/exec.
* One real Pod exercises a completed init container, regular and native init
  sidecars, an ephemeral container, named task leases, exec/readiness probes and
  a real postStart hook through the same generic runtime. Malformed artifacts
  are independently rejected in each non-main role. These tasks remain active
  until the final production cleanup; a preallocated CRI ID or epoch-zero
  StartError is never treated as actual application execution.
* Execution fixtures retain `readOnlyRootFilesystem: true`. `/` and `/app` must actually
  appear read-only in `/proc/self/mountinfo`; writes fail. An explicit
  `/app/new-volume-target` emptyDir, **absent from every packaged layer**, remains
  writable. Both initial and exec processes have no effective capabilities and
  `NoNewPrivs=1`; `launch.json` is not mounted.
* The Python fixture rotates its approved source without republishing application
  bytes. The old Pod UID/container ID and durable runtime lease stay unchanged,
  old HTTP and exec retain the old Env, an exec spans the rotation, and the
  old root survives. A new Pod carries an epoch/generation barrier and executes
  the new approved runtime, with the exact same application image digest.
* With mutation bypassed **only for the invocation workload namespace**,
  already-bound non-root pods reach the real shim and must fail on malformed,
  wrong-epoch and future-generation barriers with exact reason codes.
  Separate already-bound root and privilege-escalation requests also bypass
  admission and must fail at the shim, not merely at the webhook.
* The immutable-filesystem companion proves actual EROFS on runtime-provided,
  mode-1777 `/tmp` and packaged `/app`, with writable explicit application
  storage in both the initial process and Exec. Admission must default a
  read-only root and reject writable roots and runtime overlays. With the
  invocation's admission Deployment stopped, already-bound Pods must be
  rejected independently by the shim. Admission is restored before cleanup.
* The accounting container also has an enforced read-only root. It writes first
  16 MiB, then a total of 80 MiB into an explicit disk-backed `emptyDir` beneath
  `/app`. Actual CRI ContainerStats measure its root separately; kubelet
  VolumeStats bound to the exact Pod UID and volume name must report the
  16 MiB data-volume growth, never misreported as root-layer growth.
  Exceeding its 48 MiB
  ephemeral-storage limit must cause real kubelet eviction with the expected
  reason while the node remains free of global DiskPressure. Packaged `/app`
  remains read-only and all non-root/no-capability restrictions remain.
  Before writing, the test reads the actual kubelet eviction/image-GC policy
  and reserves its applicable threshold plus 1 GiB and the entire bounded
  write. It does not change kubelet settings. Initial zero-timestamp CRI
  responses are recorded as pending and bounded-waited, never accepted as
  measured zero usage.
* Fresh application and approved-runtime targets are published through an
  invocation-owned TLS/authenticated registry before installation. Workloads
  use `Always` and a workload pull secret; the worker uses its configured host
  trust/auth. Exact target checks, kubelet pull events and registry blob reads
  distinguish pulls from pre-imported execution. Missing credentials, an
  untrusted certificate, corruption, unsafe layers, malformed launch metadata,
  config mismatch and unsupported native platforms must fail without executing
  application code.
* Dedicated lifecycle IDs exercise same-name inventory recreation, genuine
  five-minute authorization expiry while renewal is stopped, cold rotation,
  idle/mode transitions and task/exec lease retention. Dedicated controller
  workloads exercise restart opt-ins, native strategies, pacing and deadlines.
  Cleanup hooks induce a real resourceVersion conflict during the production
  claim-release update; synthetic Node API tests are not replacement-host proof.
* A real TLS Kubernetes admission interceptor refuses node label
  removal/value-change. Entries are first switched to Eager, since OnDemand
  pins are unsupported. Refusing both withdrawal and taint changes must leave
  inventory deletion blocked and explicitly diagnosed. Permitting only the
  taint must result in a read-back-confirmed node-wide fence with the pinned
  label retained. Other IDs cannot newly schedule; existing tasks and leases
  survive; a bound, admission-bypassing revoked start must fail closed.
* Production profile deletion must retain roots, finalizers and the UID claim
  while actual workloads are active. Only after owned pods drain may root,
  finalizer and claim cleanup complete. No fake receipt, forced finalizer
  removal or invented authorization is used.

The interceptor is part of the default suite, not a mocked API decision. For
initial debugging only, `--without-pinned-scenarios` selects the positive/
barrier/cleanup subset and explicitly records the omitted cases as gaps.
There are no silent skips. `--timeout 600` is the default per-assertion deadline
(range 30–1800 seconds); individual builds and commands also have bounds.

## Delivery and execution proof boundaries

The initial build stage retains local-import execution inputs. Application OCI
layouts stream to:

```text
docker exec -i OWNED_NODE ctr -n k8s.io images import --all-platforms -
```

No `--index-name` is used: it would name the *outer* OCI layout index.
Every producer reference is fully qualified. The named import must target the
verified **inner application index digest**, and its canonical `repo@sha256:…`
alias must resolve to the same target. Initial runtime images are loaded into kind;
their actual **ctr target digests**, not Docker config IDs, become approved
canonical sources.

Before installation, `delivery.bootstrap` publishes fresh selected application
and runtime targets that are not pre-imported, changes the references and sets
workload `imagePullPolicy: Always` with the owned secret. The later
`delivery.execute` callback must complete actual pull/TLS/authentication and
negative-start assertions before remote-delivery evidence is classified as
verified. Merely publishing, importing or configuring trust never sets those
proof flags. Historical results, including `run-complete-05`, predate these
callbacks and remain **local-import-only** evidence.

`providerScaleFromZeroVerified` and `fullAcceptance` remain false. Interceptor
TLS is not registry TLS, and native single-node execution is not proof of
alternate-architecture capacity or provider autoscaling. A clean selected-suite
pass requires successful completion and fixture destruction, not just a
temporarily passing list of assertions during an unfinished run.

## Evidence, failure and cleanup

The work directory is private (0700); kubeconfig and private TLS keys are 0600.
It retains command logs, OCI layouts, builds, host reports, diagnostics,
digests, UID/lease evidence, assertion outcomes and the exact remaining gaps.
**Do not upload the whole directory**: it includes kubeconfig and fixture keys.
`result.json` and selected redacted diagnostics are the intended review inputs.

On failure, diagnostics are collected and the disposable private cluster is
destroyed. This is recorded separately as **fixture destruction, not proof
of production cleanup**. Both ownership labels and recorded Docker container
IDs are checked before deletion. Kubernetes workload deletes include atomic
UID preconditions. Cleanup refuses replaced/foreign resources and image tags;
it never prunes Docker, removes a shared network, or deletes by shared image ID.
Image tags are invocation-scoped. Pinned upstream images/build caches may remain
on the shared Docker engine intentionally. Existing evidence directories cannot
be reused. SIGINT/SIGTERM enter cleanup; SIGKILL cannot, so use the retained
UID/ownership inventory for manual review rather than broad reset/prune commands.

## Explicit remaining gaps

`UNIMPLEMENTED` in `harness.py` is serialized into every result. In particular,
this suite does **not** complete all proposal §13.1 acceptance:

* Invalid/deleting/conflicting zero-node profiles are selected by the complete
  API admission callback, not by partial/debug scopes. Fresh execution is still
  required; the existence of the callback does not qualify a candidate.
* Alternate-architecture capacity actually joining later and provider
  scale-from-zero require genuine additional native infrastructure. Optional
  `begin_native_capacity` / `finish_native_capacity` callbacks do not replace
  that prerequisite with emulation or architecture-label edits.
* Physical recreated-node cleanup refusal, stale-pin retention through
  production uninstall and controlled cold-install deadline recovery remain
  additional proof obligations.
* Newly wired delivery, lifecycle and control-edge callbacks are selected
  assertions, **not verified evidence until a fresh run passes them**. Their
  module-level limitations and result receipts distinguish surviving-shim
  reconnect from killed-shim recovery and API CAS from physical-host proof.
* Maven producer/shared-conformance acceptance also requires the fixed fixture
  producer receipts. Complete selects the additional real Maven-node callback;
  partial/debug scopes still omit it.
* The pinned vendor-version rotation is newly selected, not verified until the
  fresh run executes both versions. No specific CVE remediation is claimed.

## Offline safeguard checks

These do not contact Docker, kind, a registry, or Kubernetes:

```sh
PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover \
  -s integration-tests/live-v2 -p 'test_*.py' -v
PYTHONDONTWRITEBYTECODE=1 python3 integration-tests/live-v2/harness.py --help
```

Tests cover command/context isolation, invocation and Node/resource UIDs,
atomic delete preconditions, foreign/changed cleanup ownership, inner/outer OCI
index identity, readonly/volume/probe construction, negative-event UID scoping,
honest evidence classification, and the actual interceptor decision logic.
