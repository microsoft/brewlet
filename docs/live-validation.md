# Disposable live validation

The independent admission and CPU HPA scenarios in
[`integration-tests/e2e/live/`](https://github.com/microsoft/brewlet/tree/main/integration-tests/e2e/live)
target the existing native managed-dependency and basic CPU-autoscaling
contracts. They are not production certification, a performance benchmark, or
the broader zero-skip rewrite tracked in
[#13](https://github.com/microsoft/brewlet/issues/13).

**Coverage status:** CPU HPA passed twice on fresh local arm64 clusters with the
fixed-shim candidate described below. Admission remains pending. The consolidated
acceptance work remains tracked in
[#93](https://github.com/microsoft/brewlet/issues/93),
[#94](https://github.com/microsoft/brewlet/issues/94), and
[#95](https://github.com/microsoft/brewlet/issues/95). A harness implementation or
successful component test is not evidence that the live matrix passed. See
[preview status](README.md#preview-status-and-validation) for the public
coverage boundary.

## Prerequisites and invocation

Use an otherwise idle Docker engine with at least **4 CPUs and 7 GiB RAM**.
The fixture bounds its single kind node to 3 CPUs/6 GiB and the registry to
0.5 CPU/256 MiB. Run the two scenarios sequentially on that host. Linux amd64
is the hosted-workflow target; local native Linux/arm64 and macOS Docker Desktop
are supported fixture targets, not additional architecture acceptance claims.
Cross-architecture emulation is deliberately not selected.

Required host tools: Python 3, Docker, **kind 0.30.0**, kubectl, Helm, Go
(the toolchain required by the pinned verifier), JDK 21 or newer, Maven, Git,
curl, tar, OpenSSL, and `htpasswd` (Apache utilities, for admission's private
registry fixture). The demo is compiled with `--release 21`. Internet
access to GitHub release assets, GHCR, Docker Hub, registry.k8s.io, and Maven
Central is required. Missing prerequisites fail; mandatory cases never skip.

From a full checkout containing release commit
`f0b9334f7b29177d2ba4b49b044163ef69e16af7`:

```bash
go install sigs.k8s.io/kind@v0.30.0
export PATH="$(go env GOPATH)/bin:$PATH"
export PYTHONDONTWRITEBYTECODE=1
export BREWLET_LIVE_OUTPUT="$(mktemp -d "${TMPDIR:-/tmp}/brewlet-live-evidence.XXXXXX")"
export BREWLET_LIVE_CANDIDATE=shim
python3 -m unittest discover -s integration-tests/e2e/live -p '*test*.py' -v
python3 integration-tests/e2e/live/hpa.py
python3 integration-tests/e2e/live/admission.py
```

The output directory must not exist as an invocation directory: each invocation
creates its own random child. Keep evidence out of commits. No `--reset`, current
kube context, existing cluster, existing registry, or pre-existing runtime is
used. Do not point the legacy tier reset helper at these fixtures.

The **E2E** workflow remains scheduled/manual-only. Its `live` selection runs
two separate jobs, each executing its scenario twice consecutively with fresh
clusters. The first failure stops that job; the other scenario is independent.
The manual `scenario` selector can run only `hpa` or only `admission`; the
scheduled default is both.
Ordinary PR CI executes only offline fixture safeguards, not the live jobs.

## Release baseline and reproducibility

Both scenarios begin with the **published 0.5.0** CLI, Maven plugin, chart and
component images. CLI/plugin/chart bytes are checked against fixed SHA-256
checksums, and component, kind-node, registry and JDK images are digest-pinned.
The external Brewlet verifier is built from the matching release source, not
silently from the current checkout. `versions.json` records the baseline,
component digests, host Java, and fixture revision. The public fixture source
archive and its per-file hash manifest identify the exact executed fixture
even for diagnostic runs from an uncommitted checkout.

The runnable demo is a test fixture, built from the invocation's checkout.
It is not a new Spring Boot application. The opt-in CPU endpoint is disabled
unless the fixture supplies `-Dbrewlet.test.cpuLoad=true`; load expires after
20 seconds unless renewed, and only accepts POST with `seconds=0` or
`seconds=20`.

Record final acceptance on a clean committed fixture revision. If a scenario
exposes a product defect, preserve the release failure and record any patched
candidate's exact source revision and image digest separately. A candidate run
must not be called unmodified 0.5.0.

### Demonstrated 0.5.0 scale-out defect and candidate

On the stock pinned kind configuration (`discard_unpacked_layers=true`),
unmodified 0.5.0 served the first Pod and real CPU caused an HPA recommendation
from 1 to 3. Additional Pods failed to create their tasks: cached runnable-image
verification tried to read a packed layer already garbage-collected by
containerd. No replica writes or synthetic metrics produced that transition.

`BREWLET_LIVE_CANDIDATE=release` preserves that baseline; it is expected to fail
this configuration's scale-out regression. The documented invocation and hosted
live jobs explicitly select `shim`: they compile the current shim and overlay
**only that binary** on the digest-pinned released provisioner image, keeping
the released operator, admission webhook, Maven plugin, CLI and chart.
`versions.json` identifies the candidate image manifest, binary SHA-256, Go
version and complete source snapshot. These runs are **0.5.0 plus the fixed
shim**, not unmodified release validation.

The candidate retains descriptor-verified compressed layers in an atomically
published `immutable-v2` stage and verifies them on reuse. Only missing source
bytes can use the retained evidence; present-but-corrupt source bytes and other
I/O errors still fail. The HPA scenario explicitly waits for natural
containerd source-layer GC and checks retained hashes before applying load.
It never deletes source blobs to manufacture this condition.

This is a **warm-reuse fix**, not a complete packed-layer retention policy.
Cold startup when the source has already disappeared and no verified v2 stage
exists still fails closed with re-pull guidance. Upgrades leave in-use older
stages untouched and build a separate v2 stage when source bytes are available.
For reliable cold starts, operators must retain packed layers in their effective
containerd configuration and re-pull already affected images. No node-wide
containerd policy is silently changed by this candidate.

## Isolation, trust, and cleanup

Every invocation creates a uniquely named Docker network, registry and kind
cluster. All Kubernetes commands carry the private kubeconfig and explicit
context. Cleanup checks immutable container IDs and ownership labels, removes
only those containers and their anonymous volumes, and removes only the
identified network. It does not clear production finalizers or call the old
reset helper. Destroying the disposable node is **not** proof of the production
NodeProfile cleanup lifecycle.

Kubeconfig, Maven settings/repository, signing keys and source/build scratch
files live outside the uploaded evidence tree and are removed at teardown.
Do not enable shell tracing around credentials. Fixture keys are generated per
invocation and never committed.

The registry uses plain HTTP **only inside the owned fixture network and on a
loopback-bound host port**, with exact containerd host mappings. This is not a
production registry configuration. The Brewlet compatibility webhook boots
with the chart's default `Ignore`, then switches to `Fail` after readiness and
before any workload or admission assertion. Signature enforcement is a separate
Ratify/Gatekeeper policy and must not be weakened to make a test pass.

## CPU HPA contract and timing

CPU metrics come from pinned **metrics-server 0.8.0** and the real kubelet
resource-metrics API, not the Brewlet runtime exporter. Only this disposable
kind fixture passes `--kubelet-insecure-tls` for its self-signed kubelet
certificate. The metrics-server deployment is digest-pinned on its first apply.

The JavaApplication requests 100m CPU/128 MiB, limits each Pod to 500m/256 MiB,
targets 50% CPU, and bounds replicas to 1-3. Kind's controller-manager is
explicitly tuned for this test: 10-second HPA synchronization, 60-second
downscale stabilization and 30-second CPU initialization. Production defaults
and Brewlet's autoscaling API are unchanged.

The scenario waits up to 180 seconds for usable per-Pod CPU samples, 420 seconds
for idle minimum convergence, 600 seconds for load-driven scale-up, 120 seconds
per ownership reconciliation and 420 seconds for scale-down. Assertions use
observed conditions, not a fixed sleep. Requests to the real Service are
recorded throughout both transitions, including failures; no zero-downtime
guarantee is inferred from eventual readiness.

While loaded, the test changes `JavaApplication.spec.replicas` to stale values
7, 8 and 9 and waits for each observed generation. The managed Deployment must
remain within the HPA's 1-3 bounds. No manual Deployment scaling or synthetic
metric can substitute for the Kubernetes HPA decision.

The existing CPU HPA convenience remains optional: Brewlet creates an
`autoscaling/v1` HPA and stops writing Deployment replicas while enabled.
Disabling it resumes ownership of `spec.replicas` (default 1); it is **not**
an external-HPA ownership mode. Advanced scaling uses separately managed
ordinary Deployments with `runtimeClassName: brewlet`. Custom metrics, KEDA,
scale-to-zero, JVM scaling algorithms and node/cluster autoscaling are outside
this validation.

## Evidence and failure diagnosis

The printed per-invocation directory contains `versions.json`, owned resource
identities, `assertions.json`, `result.json`, and failure diagnostics.
`result.json` reports success only after the scenario and cleanup succeed.
Hosted jobs upload this directory on both success and failure.

For CPU scaling, inspect `scaling-samples.json` and `requests.json`: they retain
raw metrics, HPA conditions, selected/ready replica counts, JavaApplication
status and individual serving/load outcomes. `events.json`, per-Pod logs and
`node.log` retain controller, kubelet, containerd and workload context.

Required evidence for admission includes actual API responses and real
candidate-verification reports, not only successful subprocess exit codes.
Negative candidates must remain registry-discoverable. Key rotation and outage
checks must account for both Ratify and Gatekeeper provider caches; a cached
admission decision is not evidence that a new candidate was processed.

| Acceptance group | Required evidence |
|---|---|
| #94 metrics and capacity | Real per-Pod CPU samples plus node allocatable capacity |
| #94 scaling up/down | Low baseline, CPU above target, extra Ready Brewlet Pods, low CPU and minimum convergence |
| #94 ownership/status/serving | Three observed reconciliations, exact ready endpoints, transition request outcomes |
| #94 bounded cleanup | Expiring CPU leases, stopped clients, identity-checked fixture teardown |
| #95 trusted deployment | Digest-pinned JavaApplication-generated Pods Ready and serving |
| #95 negative evidence | Discoverable unsigned/wrong-key/builder/subject/malformed/tampered/incomplete cases and API denials |
| #95 rotation and verifier identity | Current-only/current+obsolete admission, obsolete-only denial, real candidate reports, other-verifier isolation |
| #95 fail-closed dependencies | Fresh subjects denied on missing evidence, registry auth/fetch failure and unavailable provider |
| #95 API scope | Valid regular/init/ephemeral image requests, CREATE/UPDATE, non-Brewlet and namespace exclusions |
| #93 repeatability | Two consecutive successful fresh-cluster results per scenario on an identified revision |

Admission coverage does not imply support for executing ordinary-image ephemeral
debug containers through Brewlet. Neither scenario covers broader signature
formats, keyless identity, production hardening or performance claims.
