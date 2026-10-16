# Brewlet 2.0 validation status

**Incomplete acceptance; not a release claim.** Proposal 0009 remains the
contract. The 100-entry ledger deliberately distinguishes verified requirements
from implemented or partially tested behavior.

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
The live-driver safeguards currently include 199 passing offline tests; those
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
