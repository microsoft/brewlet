# Measured evaluation and CTO assessment

## Decision summary

**Evaluate Brewlet for centralized runtime governance, not for an assumed
startup, memory, or bandwidth advantage.** In this experiment, startup was
similar to a matched conventional container, optimized transfer payloads were
nearly equal, and Brewlet's native shim used substantially more memory.

The useful distinction was operational: the same application image digest ran
on two different node JDK builds. That separates runtime policy from application
image publication. It does **not** remove workload restarts, application
dependency maintenance, or the need to operate privileged node components.

For a CTO, the current recommendation is a **bounded, trusted internal
evaluation**, not a production-platform standard or a cost-per-workload claim.
The managed-dependency integrity and live admission/autoscaling work from
remediation phases 8 and 9 was explicitly deferred. Those gaps are not closed by
these measurements.

## What was measured

Snapshot: **9 September 2026**, runtime source commit
[`4ffb683`](https://github.com/microsoft/brewlet/tree/4ffb683b5b81e869a3a1a87bcc5a9276f02377a4).

| Control | Configuration |
|---|---|
| Workload | Real Spring PetClinic, pinned upstream commit `b3ee2c53e76e9267f03551a7cd36b0983c859c56`; one prepared application JAR and 93 dependency JARs |
| Paths compared | Real containerd CRI with ordinary runc versus the production Brewlet shim; shipped operator and provisioner |
| Runtime | The same Temurin `21.0.12+8-LTS` full JDK and Ubuntu userland, Linux ARM64 |
| Host | macOS host; Docker Desktop Linux VM, 4 vCPUs and approximately 7.75 GiB RAM; the disposable node was capped at 5 GiB |
| Node software | containerd 2.0.3, runc 1.2.5, Linux 7.0.12-linuxkit |
| Each application container | 1 CPU limit, 512 MiB memory limit, UID 65532, read-only root, writable temporary directory |
| JVM settings | `-Xms128m -Xmx256m -XX:+ExitOnOutOfMemoryError`; identical application flags and ordered classpath |
| Cache state | Fresh JVM on an **image-warm node**; runtime already installed; no global cache eviction |
| Custom AppCDS | Disabled for both variants; default CDS mappings observed for both |
| Placement | Explicit `spec.nodeName`; scheduler queue time was excluded |
| Startup samples | Two warmups per variant, then 12 paired trials per variant; seeded order, six pairs in each order |
| Memory samples | Ten HTTP initialization requests, then ten seconds settling; single-Pod trials and three four-JVM cohorts per variant |

The conventional payload was extracted from the same prepared OCI inputs:
**same payload by construction, with recorded input hashes**, not an independent
hash comparison of application files inside both live containers. Actual process
arguments, JDK executable/release hashes, UIDs, root policy, and cgroup caps were
compared. All 24 measured single-Pod
trials and all six four-JVM cohorts completed; no failed observations or outliers
were silently removed.

"Optimized conventional" here means **layered application packaging**. It does
not mean the smallest possible JRE or jlink image; those alternatives were not
measured. The matched full JDK avoids changing runtime contents between the
primary variants.

This is one workload on one shared developer machine, not a dedicated performance
lab. The provisioner image build moved intermediate extraction of verified
`ctr`/`crictl` downloads from `/tmp` to `/work`, leaving `/verified` outputs and
final-stage destinations unchanged. This was a build-only workaround, not a
runtime/rootfs repair; complete-image equivalence with released components was
not asserted. The production runtime source was not changed. Exact
versions, digests, controls, samples, and cleanup evidence are in the
[raw results](https://github.com/microsoft/brewlet/blob/main/integration-tests/benchmarks/results/2026-09-09-linux-arm64.json).

The native shims are also **not matched builds**: Brewlet embeds containerd
2.2.7 and is built with the pinned Go 1.26 toolchain; the ordinary stack uses
containerd 2.0.3 built with Go 1.23.6. The memory delta compares these stacks,
not an isolated Brewlet-code effect or an unavoidable architectural cost.

## Startup: no demonstrated advantage

Times are seconds. IQR is the inclusive 25th-75th percentile interval; it is
not a confidence interval.

| Metric | Conventional layered | Brewlet layered |
|---|---:|---:|
| Spring-reported startup, median | 6.606 | 6.613 |
| Spring-reported startup, IQR | 6.356-6.865 | 6.381-6.825 |
| Spring-reported startup, range | 6.189-7.401 | 6.207-7.229 |
| Host create-to-observed-ready, median | 8.421 | 8.415 |
| Kubernetes creation-to-ready, median | 8.0 | 8.5 |
| Kubernetes container-start-to-ready, median | 8.0 | 8.0 |

The paired Spring-startup difference, Brewlet minus conventional, had a median
of **-0.054 seconds** and an IQR of **-0.158 to +0.111 seconds**. Differences
went in both directions. This small experiment does not establish a meaningful
startup improvement or statistical equivalence.

The Kubernetes status timestamps and one-second readiness probe are coarse.
Do not interpret the creation-to-ready median difference as a precise half-second
runtime penalty, or the host stopwatch difference as a six-millisecond benefit.
These are not cold-node, registry-download, scheduler-latency, or service
throughput measurements.

## Memory: similar JVMs, larger Brewlet shim

Values are MiB. PSS apportions shared mapped pages among processes; RSS does not.
Combined PSS is summed within each snapshot before taking the median.

### One application Pod

| Median post-startup measurement | Conventional layered | Brewlet layered |
|---|---:|---:|
| JVM RSS | 350.0 | 350.5 |
| JVM PSS | 349.9 | 350.5 |
| Native shim RSS | 12.2 | 80.9 |
| Native shim PSS | 5.9 | 80.8 |
| JVM + associated shim PSS | 356.2 | 431.3 |
| Application container `memory.current` | 329.3 | 329.6 |

The JVM footprints were similar. The measured Brewlet shim accounted for
approximately **75 MiB more PSS** per single-Pod sample. Combined JVM-plus-shim
PSS was approximately **21% higher** in this snapshot.

### Four concurrent JVMs

These are medians of three cohort snapshots per variant, not per-Pod numbers.

| Measurement | Conventional layered | Brewlet layered |
|---|---:|---:|
| Sum of JVM PSS | 1,329.7 | 1,323.8 |
| Sum of associated shim PSS | 25.5 | 285.8 |
| Combined JVM + shim PSS | 1,355.2 | 1,609.6 |

The combined difference was approximately **254 MiB** per four-JVM cohort.
Sharing the JDK installation did not produce a measured heap-sharing benefit.
Conventional containers also share unchanged image layers and mapped files.

A separate idle snapshot of Brewlet's operator, admission, provisioner, and
their associated shims totaled approximately **93.4 MiB PSS**. This fixed
platform overhead is not included in the workload rows above.

These are **summed sampled JVM plus runtime-shim PSS**, not total Pod memory:
sandbox pause processes and the parent Pod cgroup were not collected. They are
post-startup snapshots, not peak, long-running steady-state, or whole-node
capacity measurements. Application cgroup charges and PSS measure
different things: shared file pages may be charged elsewhere, and a native shim
can be outside the application container's cgroup. Do not add `memory.current`
to PSS, sum RSS as physical memory, or extrapolate a production Pod density from
these tables. Common Kubernetes/CNI and sandbox overhead is not fully captured.
Cohort PSS is collected in approximately sequential snapshots, not as one
simultaneous whole-node physical-memory reading.

The shipped RuntimeClass reserves
[64 MiB of memory overhead](https://github.com/microsoft/brewlet/blob/4ffb683b5b81e869a3a1a87bcc5a9276f02377a4/kubernetes/internal/controller/resources.go).
The measured single-Pod shim footprint warrants profiling native allocations,
retention over time, and overhead sizing before making density claims. This
experiment does not establish a memory leak or its cause, and did not tune the
runtime to improve the result.

## Storage and transfer payload: account for caches on both sides

These numbers are **compressed content-addressed payload bytes**, recomputed
from actual OCI manifest, config, and layer descriptors for Linux ARM64. They
are not observed network traffic, transfer duration, or registry billing.
Image-index retrieval is excluded equally. HTTP headers, TLS, retries, and
provider-specific storage behavior were not measured.

| Missing-object scenario | Conventional layered | Brewlet layered |
|---|---:|---:|
| Empty cache: application plus its runtime | 267.355 MiB | 267.333 MiB |
| Runtime objects already cached | 56.565 MiB | 56.531 MiB |
| Another replica of the identical cached image | 0 additional object bytes | 0 additional object bytes |
| Small application-resource revision; dependencies unchanged | 455,044 bytes | 445,430 bytes |

The Brewlet cold-cache row includes its separately delivered JDK/userland,
not just its small application image. Brewlet's benchmark-built operator,
admission, and provisioner add **97.782 MiB** of unique compressed objects beyond
that application/runtime set, bringing this one-node setup to **365.114 MiB**.
Those component images are a platform cost where they are pulled, not a new
transfer for every application.

The fat-JAR conventional control required **59,492,728 bytes** for the same
resource revision. Both layered variants reduced it to roughly 0.43 MiB:
that is a benefit of layering, not a unique Brewlet advantage.
"Zero additional object bytes" for another replica does not mean zero network
requests or zero memory use.

GNU `du -s -B1` reported **522,031,104 allocated-block bytes** (497.85 MiB) for the
installed JDK and source-image userland. The Java-home subtree's 309,055,488 bytes
are a subset, not an additional cost. This is neither apparent file size nor an
exclusive/incremental host-physical storage comparison: shared extents, retained
source-image blobs/snapshots, and other node storage also matter.

### Runtime replacement and efficient conventional rebasing

A separate post-primary demonstration changed the node runtime between actual
Temurin **21.0.11+10** and **21.0.12+8** builds. New Pods used the selected build
while retaining the **same Brewlet application image digest**. Running JVMs
were not patched in place.

For an old-to-new source-image update, the measured missing payload was:

| Update path | Payload |
|---|---:|
| Conventional image with `COPY --link` rebase control | 171.998 MiB |
| Brewlet runtime replacement, unchanged application | 171.990 MiB |
| Conventional image rebuilt with standard `COPY` | 228.544 MiB |

The efficient conventional control also reuses its application/dependency
layers. Comparing only with the standard-`COPY` rebuild would overstate
Brewlet's transfer advantage. These values include any source-image userland
changes; they are not measurements of JDK binaries alone or proof of CVE
remediation.

The operating-model distinction remains: a conventional runtime update produces
a new application image manifest/digest, even if efficiently rebased. Brewlet
can change the runtime policy without changing that application digest.
**Both still require workload rollout/restart to adopt the runtime.**
The policy-transition readiness times are not compared: the alternate JDK
source and original cached source had different cache conditions.

## CTO recommendation

| Decision | Assessment |
|---|---|
| Centralize JDK policy across a controlled Java fleet | Worth a bounded evaluation: independent runtime replacement was demonstrated. |
| Adopt to reduce startup latency | Not justified by this dataset. |
| Adopt to reduce memory or increase Pod density | Not justified; measured native-shim overhead was higher. |
| Adopt to eliminate large application-update transfers | Compare with conventional layered images first; both reused unchanged dependencies effectively. |
| Standardize for production today | Not recommended on this evidence and current operational gaps. |

The business case is strongest when runtime governance is a material problem
and a platform team can own node provisioning, runtime qualification, and
workload rollout. It is weaker when existing container tooling already provides
centralized base-image automation and efficient rebasing.

Before a production decision:

- Complete the deferred managed-tar integrity and signer-rotation work
  (**phase 8**) and real admission/HPA coverage (**phase 9**).
- Address or explicitly accept the
  [node-loss and scale-in restrictions](capability-labels-and-autoscaling.md#scale-in-consolidation-and-replacement).
  Missing/reused Node UIDs before proven cleanup can block an entire profile;
  automatic decommissioning requires external coordination.
- Qualify the [privileged provisioning and trust model](security.md), and
  measure native-shim memory over longer runs and under workload churn.
- Repeat on dedicated representative infrastructure, multiple applications and
  sizes, additional architectures, and JRE/jlink alternatives. Measure sustained
  traffic, cold delivery, and actual operational effort before projecting money.
- Establish ownership and support expectations.
  [Support is best-effort, without an SLA](https://github.com/microsoft/brewlet/blob/main/SUPPORT.md);
  repository/package access and ordinary-image ephemeral debugging remain
  documented limitations.

No production readiness, cloud cost-per-request, hostile-tenant isolation, or
closed-loop autoscaling claim follows from this benchmark.

## Reproduce and inspect

The [benchmark harness](https://github.com/microsoft/brewlet/tree/main/integration-tests/benchmarks)
creates disposable, explicitly scoped resources. It requires an authorized
checkout, an ARM64 Linux Docker VM with adequate capacity, Go, Maven, JDK 21,
Python 3.12+, kubectl, and Helm. The recorded host was macOS ARM64. The script
can obtain its pinned kind binary there; otherwise supply a compatible `KIND`
executable. Run only in a disposable evaluation environment; do not adapt it
to an existing cluster.

The reusable replay harness was hardened after the recorded experiment without
changing its observations. Fresh runs use unique resource identities, reject
fixture overrides and dirty production inputs, and export the detailed idle
sample and cleanup outcomes into a consistent result schema. These replay
safeguards have offline regression coverage; the full timed experiment was not
repeated solely to exercise that plumbing.

```bash
JAVA_HOME=/path/to/JDK21 \
  integration-tests/benchmarks/run.sh integration-tests/benchmarks/work-local

python3 integration-tests/benchmarks/summarize.py \
  integration-tests/benchmarks/work-local/results.json
```

Use a new output directory for each run. The driver writes sanitized
`results.json` after cleanup, including failures rather than silently claiming
success. It does not overwrite the historical result file below. `PETCLINIC_REPO`,
`PETCLINIC_REF`, and `PETCLINIC_JAR` overrides are deliberately rejected for this
pinned-workload protocol.

Recompute the published tables without Docker, downloads, or a cluster:

```bash
python3 integration-tests/benchmarks/summarize.py \
  integration-tests/benchmarks/results/2026-09-09-linux-arm64.json

PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover \
  -s integration-tests/benchmarks -p 'test_*.py' -v
```

The summarizer verifies paired samples, observed controls, distinct process
identities, and OCI object-set accounting. Quartiles use
`statistics.quantiles(..., method="inclusive")`; all measured samples remain
included. The raw results record successful profile finalization, Helm
uninstall, and removal of task-created node/registry resources. Shared base
images and build caches were not globally pruned.
