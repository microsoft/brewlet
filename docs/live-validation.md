# Disposable live validation

The independent admission and CPU HPA scenarios in
[`integration-tests/e2e/live/`](https://github.com/microsoft/brewlet/tree/main/integration-tests/e2e/live)
target the existing native managed-dependency and basic CPU-autoscaling
contracts. The [executable workflows](#executable-workflows) scenario covers
`brewlet push`, `brewlet k8s app status|wait`, `status`, `doctor`,
`jdk|launcher list|add`, `profile list|inspect|delete`, `inspect app`, the
`install` guards and `mvn package brewlet:push` with a separate kubectl/CLI
deployment handoff through their shipped entry points. They are not
production certification, a performance benchmark, or the broader zero-skip
rewrite tracked in [#13](https://github.com/microsoft/brewlet/issues/13).

The smaller PR smoke, `python3 integration-tests/e2e/live/smoke.py`, runs
`mvn package brewlet:push brewlet:manifest` in its own disposable fixture.
It checks the generated YAML's image, namespace, JDK, ports, and readiness probe
before applying that file separately with kubectl. The CLI then waits for the
application, and the fixture verifies runtime readiness and an HTTP response.
Evidence includes `maven-push-manifest.log`, `javaapplication.yaml`, and
`cli-app-wait.log`. The comprehensive workflows scenario retains its separate
production-style manifest handoff, so both paths remain covered.

The [external retirement scenario](#external-host-retirement) exercises
evidence-backed NodeProfile recovery after destroying a disposable kind worker,
including real Java execution on its replacement.

**Coverage boundary:** All live scenarios build the full current checkout.
The [recorded source-built validation](#recorded-source-built-validation)
identifies the tested revision and its relationship to the corresponding release.
Archived acceptance results used an older mixed-version stack; neither those
results nor source-built runs establish a pass for published release artifacts.
A harness implementation or successful component test is not evidence that the
live matrix passed. See
[preview status](README.md#preview-status-and-validation) for the public
coverage boundary.

## Prerequisites and invocation

Use an otherwise idle Docker engine with at least **4 CPUs and 7 GiB RAM**.
The fixture bounds its single kind node to 3 CPUs/6 GiB and the registry to
0.5 CPU/256 MiB. Retirement instead uses a 3 GiB control plane and two
1.5 GiB workers; each worker is capped at 2 CPUs. Run scenarios sequentially
on that host. Linux amd64
is the hosted-workflow target; the recorded local runs use Linux/arm64 nodes
on macOS Docker Desktop. Other host/architecture combinations are fixture
targets, not additional acceptance claims.
Cross-architecture emulation is deliberately not selected.

Required host tools: Python 3.12+, Docker, **kind 0.33.0**, kubectl, Helm, Go
(the toolchain required by `core/go.mod`), JDK 21 or newer, Maven, Git,
curl, tar, OpenSSL, and `htpasswd` (Apache utilities, for admission's private
registry fixture). The demo is compiled with `--release 21`. Internet
access to GitHub release assets, GHCR, Docker Hub, registry.k8s.io, and Maven
Central is required. Missing prerequisites fail; mandatory cases never skip.

The kind release is pinned exactly, together with its digest-pinned
multi-arch (amd64/arm64) node image `kindest/node:v1.34.11`, so every fresh
cluster is reproducible. Any other kind version fails before a cluster is
created, and the error names the required version and its install command.

Setup steps that reach external infrastructure retry transient network
failures (DNS, connection resets/timeouts, registry 5xx/rate limits) at most
three times with 10/20/40-second backoff. These steps are the checkout's Go,
Docker and Maven builds, pulls of the pinned registry, kind node, Zot and Ratify
base images, and pinned source downloads. Pinned images are pre-pulled once per
invocation before use. Maven also runs with
`-Daether.connector.http.retryHandler.count=5`. A publishing Maven goal reruns
only when it failed during dependency resolution, before anything was pushed.
Exhausted retries raise an `INFRASTRUCTURE ERROR (not an assertion failure)`.
`result.json` records `failureClass` as `infrastructure`, `assertion` or `error`.
Assertions themselves are never retried or skipped.

The executable-workflows stalled-process check supports Linux and macOS hosts
using `/bin/ps` with a bounded timeout, full command arguments, and zombie-state
checks. An absent PID must also be confirmed by a signal-0 probe; unavailable,
denied, or malformed inspection fails rather than reporting no orphans.
Unsupported host platforms fail this mandatory check. The injected stub uses
the fixture's Python interpreter for portable wall-clock timestamps.

From a checkout of the source to validate:

```bash
go install sigs.k8s.io/kind@v0.33.0
export PATH="$(go env GOPATH)/bin:$PATH"
export PYTHONDONTWRITEBYTECODE=1
export BREWLET_LIVE_OUTPUT="$(mktemp -d "${TMPDIR:-/tmp}/brewlet-live-evidence.XXXXXX")"
python3 -m unittest discover -s integration-tests/e2e/live -p '*test*.py' -v
python3 integration-tests/e2e/live/hpa.py
python3 integration-tests/e2e/live/admission.py
python3 integration-tests/e2e/live/workflows.py
python3 integration-tests/e2e/live/retirement.py
```

The output directory must not exist as an invocation directory: each invocation
creates its own random child. Keep evidence out of commits. No `--reset`, current
kube context, existing cluster, existing registry, or pre-existing runtime is
used. Do not point the tiered suite's reset helper at these fixtures.

The **E2E** workflow remains scheduled/manual-only. Select `suite: tiers` for
tiers 1-19 (tier 14 is retired) and the arm64 host-only coverage, `live` for isolated live scenarios,
or `all` (the default) for both. Scheduled runs execute both suites; `scenario`
is ignored when selecting `tiers`.

Its `live` selection runs
three separate jobs, each executing its scenario twice consecutively with fresh
clusters. The first failure stops that job; the other scenario is independent.
The manual `scenario` selector can run only `hpa`, `admission`, `retirement` or
`workflows`; the scheduled default (`both`) runs every live scenario. The
`workflows` job runs as two matrix entries, each on a fresh runner and cluster.
Admission/HPA/retirement jobs have a 180-minute budget for two complete checkout builds
and scenario runs; each workflows job has 90 minutes. Tier jobs have 60 minutes,
and the arm64 host-only job has 30 minutes. Tier jobs retain selected redacted
logs on success or failure; private work directories are never uploaded.
Ordinary PR CI executes offline fixture safeguards and suite routing/monitor
contracts (`make e2e-contract-check`), not the live jobs. Saved monitor runs retain
their metadata, logs, and evidence across reloads; see the
[harness runbook](https://github.com/microsoft/brewlet/blob/main/integration-tests/AGENTS.md).

## Checkout builds and reproducibility

Every invocation builds the CLI, Maven plugin, operator, admission webhook and provisioner from
the checkout and installs its chart. Admission also builds the checkout's
Ratify verifier and uses its shipped policy manifests. `versions.json` records
the source revision and dirty flag, CLI/plugin hashes, chart file hashes,
loaded component digests and tool versions. All scenarios share the
checkout build implementation. There is no runtime version selector or
released-component overlay. External infrastructure and JDK images remain pinned.

The runnable demo is a test fixture, built from the invocation's checkout.
It is not a new Spring Boot application. The opt-in CPU endpoint is disabled
unless the fixture supplies `-Dbrewlet.test.cpuLoad=true`; load expires after
20 seconds unless renewed, and only accepts POST with `seconds=0` or
`seconds=20`.

Record final acceptance on a clean committed fixture revision. If a scenario
exposes a product defect, preserve the failure and record the fix's exact
source revision and image digest in a separate run.

### Packed-layer GC and warm reuse

The runtime retains descriptor-verified compressed layers in an atomically
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
containerd policy is silently changed by this fixture.

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

## External host retirement

Run `python3 integration-tests/e2e/live/retirement.py`, or dispatch E2E with
`suite: live` and `scenario: retirement`. This is explicitly destructive only
inside the invocation-owned kind fixture: it accepts no existing cluster or
node identifiers and does not call Azure APIs.

The fixture provisions its control plane and original worker through the real
NodeProfile controller and provisioner, then serves Java on both. A second,
unprovisioned worker is held outside the profile's pool as the replacement.
The scenario verifies the private cluster UID, original Node UID/provider ID,
and captured Docker container ID/cluster label before removing that worker
container **and its volumes**. Successful Docker inventory reads must prove
their absence before the Kubernetes Node registration is removed.

After introducing the standby into the pool, the scenario requires
real Java serving on a newly claimed replacement **before evidence**, with
`RetirementPending=True` and the exact frozen obligation retained independently.
The survivor's labels, Pod UID, running container and response remain unchanged.
After removing its Java workloads, the fixture requests deletion and verifies
that unresolved history holds the finalizer and causes real Helm uninstall to
fail without removing the operator. An unbound submitter must receive
`Forbidden`; only an explicit binding to `brewlet-retirement-recovery` permits
the identity-bound attestation. The operator must resolve the evidence and
finish profile deletion. A subsequent uninstall must succeed without deleting
or changing the resolved evidence.

`retirement-host-before.json` and `retirement-host.json` retain original host
identity and destruction proof. The attestation references the SHA-256 of the
latter. `retirement-before-evidence.json`, `uninstall-unresolved.log`,
`retirement-evidence-resolved.json`,
`retirement-evidence-retained.json`, and `assertions.json` retain the recovery
checkpoints. Shared fixture cleanup identity-checks remaining containers and
removes only this invocation's resources, including on failure.

This covers provider-neutral recovery and actual runtime provisioning, not
AKS/VMSS permanent-decommission verification, a same-name host replacement, or
all cleanup races. Envtest covers the broader identity and race permutations.
The shared fixture's cold-start containerd GC deferral still applies. Offline
safeguard tests alone do not establish a successful live recovery run.

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

The selected Maven plugin publishes a signed dependency bundle, a runnable
thin-JAR application and its native final-image DSSE/in-toto referrer. Negative
fixtures retain discoverable referrers and exercise wrong key, builder, subject,
malformed, tampered, incomplete and split-across-candidate evidence. Each
denial must be the named Gatekeeper policy's response to a valid API request,
not an invalid object, missing runtime or scheduling failure.

The verifier is compiled from the checkout and delivered by the
documented **baked image** route, over digest-pinned Ratify **1.4.5**. Set
`RATIFY_CONFIG=/home/nonroot/.ratify` to discover the baked plugin directory.
Ratify's **1.15.6** chart is read from source commit
`f5fd56fe58ba0a604eca247276acb7899e026435`. The fixture installs
digest-pinned Gatekeeper **3.18.3** from commit
`5be06a95665624a619a8082677dcf942043bf514`. Assertion records capture the exact
base images, generated image and verifier binary digests.

Live deployment found that the pinned chart CRD rejects `Verifier.spec.type`.
The shipped resource is corrected to select the same plugin through `spec.name`.
The fixture uses the shipped manifest without deleting `spec.type` to hide a
regression; trust, predicate and verifier-identity rules are unchanged.

Gatekeeper has external data enabled, cache TTL 0 and validation
`failurePolicy: Fail`. The Ratify provider timeout is 20 seconds, inside the
30-second validating webhook timeout. Gatekeeper and Ratify each have bounded
240-second rollout waits. Gatekeeper's fixture-only TCP startup probe waits for
the webhook listener before the unchanged `/readyz` probe can mark it Ready.
After enabling fail-closed validation, a server-side dry-run ConfigMap must pass
through the API server before Ratify's CRDs are installed. This readiness wait
is bounded to 120 seconds and retries only Gatekeeper connection-refused or
missing-Service-endpoint errors; policy denials and other failures abort.
`admission-gatekeeper-readiness.log` retains the latest response. Ratify uses a test-only `Recreate` rollout; current
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
3. **Cluster inventory and profile additions.** With `live` provisioned and
   `wf-app` Ready, every read is compared with the cluster itself. `jdk list`
   (json, wide, table, `--selector`) must equal the node's
   `brewlet.sh/jdks-info` and the profile's declared JDKs; `launcher list`
   must equal `brewlet.sh/launchers`. `profile list` must match the live
   generation, Ready condition and node counts, and `profile inspect` (json,
   yaml, default) must list exactly the node carrying the profile's
   `owner-uid`. `status` must auto-discover `brewlet` from a kubeconfig whose
   namespace is `wf-alpha`, report both rollouts as their Deployments do and
   fail for a namespace without a control plane. `doctor` must pass all seven
   checks for the context and an explicit namespace, and must isolate the
   `developer-rbac` failure for a ServiceAccount that cannot create
   JavaApplications. `inspect app` must report the owned Deployment and pods on
   the node, and `--namespace` must reach the unready `wf-beta` app. Cluster-scoped
   commands reject `--namespace`. With the operator paused, `jdk|launcher add`
   client and server dry runs, conflict/`--replace`, invalid sources, offline
   `--file` (JSON and the server dry run's YAML, both left unmodified) and a
   temporary Helm ownership label must leave the profile's UID and
   resourceVersion unchanged. A real `launcher add` and `jdk add` then update a
   disposable profile whose pool matches no node, which is deleted. That
   profile declares `spec.rollout` explicitly: otherwise the operator's
   finalizer update writes `rollout: {}` under its own field manager and the
   add commands refuse the profile as externally managed. `install`
   refuses before Helm runs because Brewlet CRDs exist, reporting its release,
   namespace and context without creating the namespace or a release; missing
   values, version ranges, unreadable values and invalid namespaces fail first.
   `install --dry-run` and a real install are **not** exercised: both fetch the
   released `oci://ghcr.io/microsoft/charts/brewlet` chart from the network
   rather than the checkout chart.
4. **Maven publication and deployment handoff.** `mvn package brewlet:push`
   publishes without Kubernetes options and must not create a manifest or
   application resource. The index digest must equal `push.json`; a separate
   fixture-owned manifest is applied with kubectl using that immutable image.
   CLI readiness checks use an explicit context with an unreachable decoy as
   the current context (the target is confirmed by the `kube-system` UID).
   The applied resource must be Ready for its current generation and serve
   `/hello` through its Service in `wf-maven`. Further cases cover encrypted
   `settings.xml` with a generated private `settings-security.xml` (success,
   wrong password and undecryptable master all explicit), and dry runs that
   publish nothing and leave `push.json` and the live resourceVersion unchanged.
5. **Profile deletion.** After all apps are removed, a running and a
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
| Cluster reads against ground truth | `k8s-jdk-list-node-inventory`, `k8s-launcher-list-node-inventory`, `k8s-profile-list-and-inspect`, `k8s-status-control-plane-and-profiles`, `k8s-inspect-app-workloads`, `k8s-cluster-scoped-commands-reject-namespace` |
| Doctor checks, namespaces, RBAC | `k8s-doctor-checks-and-namespace`, `k8s-doctor-restricted-identity-fails` |
| Profile additions | `k8s-jdk-add-dry-runs-nonmutating`, `k8s-launcher-add-dry-runs-nonmutating`, `k8s-profile-add-offline-file-input`, `k8s-profile-add-refuses-helm-managed`, `k8s-profile-add-live-update` |
| Install guards (no chart render) | `k8s-install-validation-and-fresh-install-guard` |
| Maven push handoff → kubectl apply → CLI Ready → response | `maven-push-kubectl-apply-cli-ready-response` |
| Maven encrypted credentials and dry run | `maven-encrypted-settings-credentials`, `maven-push-dry-run-nonmutating` |
| No leaks | `no-leaked-test-resources`, `result.json` cleanup errors |

Additional evidence: `versions.json` (source revision, dirty flag, CLI/plugin
hashes, image IDs, tool versions), `nodeprofile-live-watch.json`,
`nodeprofile-live-cleanup-timeline.json` and `cleanup-*-*.log` (operator and
provisioner logs captured while cleanup workers exist), plus the shared
resource, event, node and registry diagnostics.

## Evidence and failure diagnosis

### Recorded source-built validation

The [October 7, 2026 E2E run](https://github.com/microsoft/brewlet/actions/runs/37594958441)
passed all live jobs on Linux amd64 using source commit
`1b21f3318aeecef380f7039a0b7d9f889c00cab9`:

| Scenario | Successful jobs |
|---|---|
| Native admission | `Live admission (two fresh clusters)` |
| CPU HPA | `Live hpa (two fresh clusters)` |
| CLI and Maven workflows | `Live workflows (fresh environment 1)` and `Live workflows (fresh environment 2)` |

Admission and HPA each ran twice consecutively on fresh disposable kind
clusters. Each workflows job used a separate fresh environment. All Brewlet
components and the chart were built or installed from that checkout, not
downloaded as published release artifacts.

The `v0.7.1` tag points to `739c4181c61deeb402e20ba2bf44297e58b3de54`,
one commit after the tested revision. The
[release-preparation diff](https://github.com/microsoft/brewlet/compare/1b21f3318aeecef380f7039a0b7d9f889c00cab9...739c4181c61deeb402e20ba2bf44297e58b3de54)
changes documentation, website content and a website contract test, not runtime
code or live fixtures. This provides live validation of the runtime source
shipped in 0.7.1, **not a live test of the published 0.7.1 artifacts**.
The exact release commit has no recorded E2E run as of October 7, 2026.

These disposable-cluster results are not production certification. The
[warm-reuse and cold-start boundary](#packed-layer-gc-and-warm-reuse) and
fixture-specific registry, cache and TLS settings still apply. Hosted evidence
artifacts follow the workflow's 14-day retention.

### Archived hosted acceptance

These older runs used 0.5.0 plus a fixed shim and corrected Verifier manifest,
not the current checkout-built stack. Both jobs ran twice consecutively on independent fresh clusters from clean
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
run the current checkout scenarios rather than treating an expired artifact
link as new evidence.

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
