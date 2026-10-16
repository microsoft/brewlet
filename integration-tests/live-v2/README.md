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

## Prerequisites and immutable inputs

Use a Linux Docker engine (native Linux or Docker Desktop) on amd64 or arm64,
with cgroup v2, at least 4 CPUs, 8 GiB Docker memory, and 25 GiB available disk.
This runs privileged real provisioning and restarts **its private kind node's**
containerd. It requires network access to the pinned upstream images and build
dependencies. An unreachable registry or missing tool is a failure, never a
skip.

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
* Runtime vendor bases are the immutable index digests in
  `integration-tests/fixtures/runtimes/{java,python,node,dotnet}.Dockerfile`.
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
Derived runtime revisions change `BREWLET_RUNTIME_REVISION` from `before` to
`after`, maintaining the account/PATH/ABI. This proves real approved **source
rotation**, not remediation of an actual interpreter CVE or a vendor patch.
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
* First use executes all four real HTTP applications through kubelet/containerd/
  shim/runc, imports their separate dependencies, then passes relative-executable
  `kubectl exec` and actual exec readiness probes.
* Initial and exec JSON reports assert argv, default and overridden cwd,
  environment precedence, runtime-owned PATH, synthesized HOME and UID/GID.
  Explicit command/args/cwd overrides serve real HTTP. Separate custom-UID pods
  verify independently defaulted GID and `/tmp` HOME fallback in initial/exec.
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

## Import proof boundary

This harness deliberately uses **execution-only local imports**, not private
registry/TLS transport acceptance. The owned registry is health-checked but is
not the application/runtime delivery path. Application OCI layouts stream to:

```text
docker exec -i OWNED_NODE ctr -n k8s.io images import --all-platforms -
```

No `--index-name` is used: it would name the *outer* OCI layout index.
Every producer reference is fully qualified. The named import must target the
verified **inner application index digest**, and its canonical `repo@sha256:…`
alias must resolve to the same target. Runtime images are loaded into kind;
their actual **ctr target digests**, not Docker config IDs, become approved
canonical sources. Workloads explicitly set `imagePullPolicy: Never`.

`result.json` therefore always reports `scope: real-node-execution-only`,
`kubeletPullVerified: false`, `tlsTransportVerified: false`, and
`providerScaleFromZeroVerified: false`. Interceptor TLS is not production
registry TLS. A passing selected suite is never labelled full acceptance.

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
* Alternate-architecture capacity actually joining later, unsupported-platform
  pulls, and provider scale-from-zero are not exercised.
* OnDemand pinned-label refusal and stale-pin retention through uninstall,
  same-name inventory recreation with old roots, all-ID recovery checks, and
  actual conditional claim-release resourceVersion races/recreated Nodes
  require additional scenarios.
* Shim restart recovery, orphan exec after initial task exit, idle collection,
  authorization expiry under control-plane outage, and automatic
  rolling-restart controller strategies/pacing remain
  separate proof obligations.
* Maven producer/shared-conformance acceptance is the existing fixture suite;
  this harness executes the four generic-CLI artifacts, not a second Maven
  application on-node.
* Real kubelet registry pulls, authentication, production registry TLS and
  actual vendor-patch remediation remain unproven.

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
