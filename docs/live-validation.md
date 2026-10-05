# Disposable live validation

The independent admission and CPU HPA scenarios in
[`integration-tests/e2e/live/`](https://github.com/microsoft/brewlet/tree/main/integration-tests/e2e/live)
target the existing native managed-dependency and basic CPU-autoscaling
contracts. The [executable workflows](#executable-workflows) scenario covers
`brewlet push`, `brewlet k8s app status|wait`, `mvn brewlet:deploy` and
`brewlet k8s profile delete` through their shipped entry points. They are not
production certification, a performance benchmark, or the broader zero-skip
rewrite tracked in [#13](https://github.com/microsoft/brewlet/issues/13).

**Coverage status:** CPU HPA passed twice on fresh local arm64 clusters and
twice on hosted amd64 with the fixed-shim candidate described below. Admission
passed its 47-assertion matrix twice on fresh local arm64 clusters and twice on
hosted amd64 with that shim and the corrected Verifier manifest. Neither is an
unmodified 0.5.0 pass.
The acceptance work and delivery are tracked in
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
is the hosted-workflow target; the recorded local runs use Linux/arm64 nodes
on macOS Docker Desktop. Other host/architecture combinations are fixture
targets, not additional acceptance claims.
Cross-architecture emulation is deliberately not selected.

Required host tools: Python 3.12+, Docker, **kind 0.30.0**, kubectl, Helm, Go
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
python3 integration-tests/e2e/live/workflows.py
```

The output directory must not exist as an invocation directory: each invocation
creates its own random child. Keep evidence out of commits. No `--reset`, current
kube context, existing cluster, existing registry, or pre-existing runtime is
used. Do not point the tiered suite's reset helper at these fixtures.

The **E2E** workflow remains scheduled/manual-only. Select `suite: tiers` for
tiers 1-19 and the arm64 host-only coverage, `live` for isolated live scenarios,
or `all` (the default) for both. Scheduled runs execute both suites; `scenario`
is ignored when selecting `tiers`. Automation using the former `legacy`
selector must switch to `tiers`.

Its `live` selection runs
two separate jobs, each executing its scenario twice consecutively with fresh
clusters. The first failure stops that job; the other scenario is independent.
The manual `scenario` selector can run only `hpa`, only `admission` or only
`workflows`; the scheduled default (`both`) runs every live scenario. The
`workflows` job runs as two matrix entries, each on a fresh runner and cluster.
Ordinary PR CI executes offline fixture safeguards and suite routing/monitor
contracts (`make e2e-contract-check`), not the live jobs. Saved monitor history
is normalized without renaming run IDs or evidence; see the
[harness runbook](../integration-tests/AGENTS.md).

## Release baseline and reproducibility

Brewlet 0.5.1 ships the verified warm-reuse fix and corrected Verifier manifest
described below. The fixture deliberately retains its 0.5.0 baseline and
candidate modes to reproduce the original defect; these runs do not establish
unmodified 0.5.1 live coverage.

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
I/O errors still fail. The HPA scenario then requests a containerd
source-layer GC and waits for it, and checks retained hashes before applying load.
It never deletes source blobs to manufacture this condition.

The fixture node keeps `discard_unpacked_layers=true` but sets the containerd GC
scheduler's `startup_delay` to `24h`. With the stock `100ms` delay, containerd
can collect the packed layers within about a second of the CRI pull. That races
the first Pod's cold start, which fails closed (see below) and made the scenario
flaky. With the longer delay, the cold start finds its layers. The scenario
asserts this (`cold-start-source-layers-retained`), then triggers containerd's
own collector by creating a lease and deleting it with `ctr leases delete --sync`
(`gcRequestLease`). After that, scale-out exercises only the warm-reuse path.

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

## Native admission contract and configuration

Scenario A replaces only the empty invocation-owned Distribution registry with
**Zot 2.1.8**, pinned to
`sha256:cd2aea942f428630bcb4190542be6abd35e14177aab84fc7ccad0dca8ecb363d`.
Distribution 3.0.0 was demonstrated to return 404 for the native Referrers API;
fallback tags cannot substitute for discovery. Zot uses the same private network
and loopback host port, explicit container identity/ownership and an isolated
0700 data directory owned by the invoking UID/GID.

The released Maven plugin publishes a signed dependency bundle, a runnable
thin-JAR application and its native final-image DSSE/in-toto referrer. Negative
fixtures retain discoverable referrers and exercise wrong key, builder, subject,
malformed, tampered, incomplete and split-across-candidate evidence. Each
denial must be the named Gatekeeper policy's response to a valid API request,
not an invalid object, missing runtime or scheduling failure.

The verifier is compiled from exact Brewlet 0.5.0 source and delivered by the
documented **baked image** route, over digest-pinned Ratify **1.4.5**. Set
`RATIFY_CONFIG=/home/nonroot/.ratify` to discover the baked plugin directory.
Ratify's **1.15.6** chart is read from source commit
`f5fd56fe58ba0a604eca247276acb7899e026435`. The fixture installs
digest-pinned Gatekeeper **3.18.3** from commit
`5be06a95665624a619a8082677dcf942043bf514`. Assertion records capture the exact
base images, generated image and verifier binary digests.

Live deployment found that the pinned chart CRD rejects `Verifier.spec.type`.
The shipped resource is corrected to select the same plugin through `spec.name`.
The scenario starts from the release resources and records that one-field
candidate correction explicitly; trust, predicate and verifier-identity rules
are unchanged.

Gatekeeper has external data enabled, cache TTL 0 and validation
`failurePolicy: Fail`. The Ratify provider timeout is 20 seconds, inside the
30-second validating webhook timeout. Gatekeeper and Ratify each have bounded
240-second rollout waits. Ratify uses a test-only `Recreate` rollout; current
Ready Pods and Service endpoints must agree
before enforcement tests proceed. Report transport uses a unique fixture CA and
verified SANs, never `curl --insecure`.

After Helm installation, the fixture uses a spec-only strategic-merge patch for
Ratify's rollout strategy, named container image, and registry host alias. It
does not reapply a fetched Deployment's resource version or status, avoiding
conflicts with concurrent controller updates while preserving other chart fields.

Provider/discovery caches are disabled. The pinned Ratify version also exposed
concurrent writes to its shared ORAS content-cache index, so this test uses an
empty root-owned read-only OCI layout for that cache. Every verification still
fetches and verifies actual registry data. These runs do not establish
production content-cache concurrency or cache-revocation behavior. Ratify 1.4.5
omits `errorReason` in its aggregated plugin report; the fixture also captures
the unchanged plugin's real subprocess output for rejection diagnostics.
Actual Ratify decisions and actual API admission outcomes remain mandatory.

Current-only and current-plus-obsolete evidence admit the **same subject**;
obsolete-only evidence denies it. Fresh reports must contain every real
candidate. Another verifier's success cannot substitute, its failure cannot
veto a complete valid Brewlet candidate, and partial claims cannot be combined.
Fresh subjects exercise missing evidence, real registry authentication/fetch
failures and unavailable Ratify. Valid CREATE/UPDATE requests cover regular and
init images; ephemeral images use the proper UPDATE subresource. Non-Brewlet
behavior and all namespace exclusions are checked separately.

## Executable workflows

`workflows.py` reuses the strict fixture but provisions it **entirely from the
checkout under test**: it builds `core/cmd/brewlet`, installs the checkout's
Maven plugin into the invocation's private Maven repository (and checks the
installed JAR is byte-identical to the build), builds the operator, admission
and provisioner images with an ownership label, loads them into the kind node
pinned by digest, and installs `kubernetes/charts/brewlet`. No released CLI,
plugin, chart or image is used. It needs `go` and `htpasswd` in addition to the
prerequisites above. Each CI run has a 90-minute job budget; most of the time
is image builds and NodeProfile provisioning/cleanup.

Sections run in this order, each command through the real CLI or Maven goal
with the private kubeconfig. `commands.json` records every command's sanitized
argv, environment *names*, target kubeconfig/context/namespace, exit code and
duration; stdout and stderr are kept in separate `cmd-NNN-*.stdout|stderr`
files. Generated passwords, tokens, Docker auth, Maven cipher text and security
files are redacted and any command that prints one fails.

1. **Remote push.** The demo is pushed anonymously to the invocation's
   Distribution registry; the stdout digest and every `--push-result` field are
   compared with the index fetched from the registry, and every amd64/arm64
   child manifest, config and layer is fetched and hashed. A repeat push must
   reuse existing blobs and leave both digests intact. An explicit `--store`
   stays local; an unqualified ref with `--push-result` is rejected ("Brewlet
   never defaults to Docker Hub") and without it writes only `./oci`. A second
   htpasswd-protected registry proves anonymous rejection, isolated Docker
   config and `BREWLET_REGISTRY_USERNAME/PASSWORD` publication, and wrong-password
   rejection without a handoff or tag.
2. **Application status/wait.** `app wait` starts before the JavaApplication
   exists and must retry `not found` until the app becomes Ready. Same-named
   apps in `wf-alpha` (Ready, using the CLI-pushed digest-pinned image) and
   `wf-beta` (never Ready) distinguish the kubeconfig's default namespace from
   `--namespace`. JSON, YAML and table outputs are parsed from stdout; progress
   stays on stderr. A nonready `wait` must fail within 20s plus the documented
   15s tolerance with describe/status remediation. With the operator paused
   (scaled to zero) a spec change leaves an old Ready condition that must not
   satisfy `wait`; resuming it must. A loopback endpoint that accepts but never
   answers, and a closed port, bound `status`/`wait` without touching the
   cluster.
3. **Maven deploy.** `mvn package brewlet:deploy` uses a kubeconfig whose
   current context is an unreachable decoy, so only `brewlet.kubeContext`
   reaches the cluster (confirmed by the `kube-system` UID) and
   `brewlet.namespace=wf-maven`. The index digest must equal `push.json`, the
   generated manifest and the applied resource, which must be Ready for its
   current generation and serve `/hello` through its Service. Further cases:
   `brewlet.wait=false` with a never-ready probe; a live readiness timeout
   bounded by Maven's own log timestamps; an **injected** stalled `kubectl`
   (test executable, not a rollout) that must be terminated with its
   descendant after `brewlet.waitTimeout`; encrypted `settings.xml` with a
   generated private `settings-security.xml` (success, wrong password and
   undecryptable master all explicit); and dry runs that publish nothing,
   leave `push.json` and the live resourceVersion unchanged, after which
   `brewlet:manifest` still uses the saved digest-pinned image.
4. **Profile deletion.** After all apps are removed, a running and a
   gracefully terminating bare Brewlet Pod (real `preStop` sleep in a 600s
   grace period) block deletion with and without dry runs and `--wait`;
   `--yes --dry-run=server` reports them without mutating. A Helm-managed label
   and a ServiceAccount without Pod list permission are refused (fail-closed).
   The operator is paused while UID, resourceVersion and deletion timestamp are
   compared, so a real controller status write cannot hide or fake a change.
   With the operator still paused, a no-wait deletion is followed by an attached
   `--wait --wait-timeout 10s` that must time out while the finalizer, status
   targets, node ownership label and host JDK roots remain. The operator is
   then resumed and an attached `--wait` follows real cleanup; a watch on the
   profile must show `CleanupComplete=True/CleanupSucceeded` for the current
   generation with claims retained before `DELETED`. The node must then have no
   shim, JDK roots, containerd `brewlet` runtime, `owner-uid` or JDK labels, and
   no profile workers. A second profile is provisioned and deleted with a fresh
   `--wait` to the same host checks. The run ends by asserting no test
   JavaApplications, Brewlet Pods or NodeProfiles remain.

The timeout-vs-cleanup case is controlled by pausing the operator, not by a
race. Deterministic API permutations that need a synchronization seam
(UID/resourceVersion preconditions against an edit or recreation,
`CleanupBlocked`, attach-after-finalizer-removal) run in
`kubernetes/internal/cli/profile_delete_integration_test.go`
(`make -C kubernetes test-cli-integration`, also in PR CI). Registry protocol
permutations (mounts, redirects, references) belong to the shared conformance
tests from [#170](https://github.com/microsoft/brewlet/issues/170); neither is
presented as live evidence here.

Runtime architecture: the index always contains amd64 and arm64 children, which
are verified by registry fetch only. Execution covers just the node's
architecture (amd64 hosted; recorded in `identity.json`). Cross-architecture
runtime is not claimed.

| Acceptance item (#171) | Scenario assertions (`assertions.json`) / test |
|---|---|
| Real CLI/Maven entry points | `commands.json` for every section |
| App namespace, outputs, readiness | `app-status-namespaces-and-outputs`, `app-wait-retries-missing-resource` |
| App current generation and timeouts | `app-wait-requires-current-generation`, `app-wait-nonready-rollout-timeout`, `app-commands-bounded-on-api-failure` |
| Profile workload, terminating Pod, ownership, RBAC | `profile-delete-refuses-running-and-terminating-workloads`, `profile-delete-refuses-helm-managed`, `profile-delete-restricted-identity-fails-closed`; GitOps in envtest |
| Profile dry runs | `profile-delete-dry-runs-nonmutating` |
| Production cleanup and wait modes | `profile-delete-production-cleanup`, `profile-delete-fresh-wait-cleanup`, `profile-delete-timeout-preserves-cleanup` |
| Profile concurrency and blocked cleanup | envtest `profile-deletion/uid-resource-version-preconditions`, `profile-deletion/blocked-timeout-and-attach` |
| Remote push anonymous/authenticated/rejected | `cli-push-anonymous-verifiable-index`, `cli-push-authenticated-and-rejected`, `cli-push-store-and-unqualified-targets`, `cli-push-repeat-publication-intact` |
| Runnable digest-pinned workload | `cli-push-digest-pinned-workload-serves` |
| Maven push→manifest→apply→Ready→response | `maven-deploy-push-manifest-apply-ready-response` |
| Maven wait opt-out, timeouts, stalled process | `maven-deploy-wait-opt-out`, `maven-deploy-live-readiness-timeout`, `maven-deploy-injected-stalled-kubectl` |
| Maven encrypted credentials and dry run | `maven-encrypted-settings-credentials`, `maven-deploy-dry-run-nonmutating` |
| No leaks | `no-leaked-test-resources`, `result.json` cleanup errors |

Additional evidence: `versions.json` (source revision, dirty flag, CLI/plugin
hashes, image IDs, tool versions), `nodeprofile-live-watch.json`,
`nodeprofile-live-cleanup-timeline.json` and `cleanup-*-*.log` (operator and
provisioner logs captured while cleanup workers exist), plus the shared
resource, event, node and registry diagnostics.

## Evidence and failure diagnosis

### Recorded hosted acceptance

Both jobs ran twice consecutively on independent fresh clusters from clean
committed source. The later documentation-only commits do not change the
archived fixture/runtime source hashes.

| Scenario | Source commit | Hosted evidence | Fresh cluster suffixes | Required assertions |
|---|---|---|---|---|
| CPU HPA | `35b7c1c1ee9e7b91ed4665086d8d97a6c09f67e5` | [Run 35419764860](https://github.com/microsoft/brewlet/actions/runs/35419764860), artifact `live-hpa-1` | `b17f0615694a`, `d8482594728a` | 11 per run |
| Native admission | `b2684abda75c35289096c0dafca1939f770ecdd2` | [Run 35423006340](https://github.com/microsoft/brewlet/actions/runs/35423006340), artifact `live-admission-1` | `c47da5e70141`, `c616823c6d60` | 47 per run |

Both pairs reported successful cleanup with no diagnostic-capture errors.
The source archives, per-file manifests and identical verifier/shim binary
hashes within each architecture's pair were checked against the reported
SHA-256 values. Hosted artifacts follow the workflow's 14-day retention;
rerun the pinned scenario rather than treating an expired artifact link as
new evidence.

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
