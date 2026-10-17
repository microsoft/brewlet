# Private Brewlet 2.0 real-node execution acceptance

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

The branch's `.github/workflows/e2e.yml` runs this suite by manual dispatch.
Select the `brewlet-2.0` ref on the existing E2E workflow. GitHub schedules run
from the repository's default branch, so this orphan branch does not claim
automatic nightly execution. The job uploads only classified `result.json`
files, never the private work directories.

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

Physical missing-host obligations and independent administrative evidence have
their own actual-node case:

```sh
PYTHONDONTWRITEBYTECODE=1 python3 integration-tests/live-v2/retirement.py \
  --work integration-tests/live-v2/run-retirement-20261016-01
```

It permanently removes only the recorded extra worker's exact Docker container
and volumes, never the control plane, then removes the exact original Node UID.
The replacement must serve on a different real Node before any external evidence
exists. Original missing-host history must block deletion/uninstall even after
replacement tasks drain. A separate initially unbound actor must be denied
retirement creation, then explicitly bound to the chart's attestor role before
attesting the actually destroyed instance. Accepted immutable evidence must
survive profile deletion and real uninstall. This is external decommission
resolution, not invented host-cleanup proof or returning-hardware verification.

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
The IDs are `fixture-java`, `fixture-python`, `fixture-node`, `fixture-dotnet`;
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
* Java, Python, Node and .NET are built and host-probed using the existing
  fixture builder. Each is explicitly packaged by this checkout's generic CLI
  with distinct dependency/application roles and one final launch layer.
* The optional Maven producer separately emits both bundled and BOM-managed
  Java artifacts. Their exact OCI targets are published unchanged through the
  authenticated registry and run through the same RuntimeClass, shim, exec,
  probe and non-root/read-only checks. A further approved Java runtime update
  retains their initial leases and moves replacements to the new generation
  without republishing either application. This adds no Java behavior to core.
* First use executes all four real HTTP applications through kubelet/containerd/
  shim/runc, imports their separate dependencies, then passes relative-executable
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
* Every language rotates its approved source without republishing application
  bytes. The old Pod UID/container ID and durable runtime lease stay unchanged,
  old HTTP and exec retain the old Env, an exec spans the rotation, and the
  old root survives. A new Pod carries an epoch/generation barrier and executes
  the new approved runtime, with the exact same application image digest.
* With mutation bypassed **only for the invocation workload namespace**,
  already-bound non-root pods reach the real shim and must fail on malformed,
  wrong-epoch and future-generation barriers with exact reason codes.
  Separate already-bound root and privilege-escalation requests also bypass
  admission and must fail at the shim, not merely at the webhook.
* A separate, explicitly writable-root accounting container writes first
  16 MiB, then a total of 80 MiB into its `/tmp` snapshot upper, not an
  `emptyDir` or other volume. Actual CRI ContainerStats for its exact Pod UID
  and container ID must report at least 16 MiB growth. Exceeding its 48 MiB
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

* Invalid/deleting/conflicting zero-node profile permutations belong to the
  separate admission suite.
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
* Maven producer/shared-conformance acceptance is the existing fixture suite;
  this harness executes the four generic-CLI artifacts, not a second Maven
  application on-node.
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
