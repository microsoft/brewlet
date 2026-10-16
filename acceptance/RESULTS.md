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
The live-driver safeguards currently include 65 passing offline tests; those
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

The public branch CI run for `09ac69428b41622f71ff6209e8c36132f8b72650`
[passed](https://github.com/microsoft/brewlet/actions/runs/38934713032).
That is contract/producer/native-overlay CI, not full real-node acceptance.

## Real-node evidence

[`results/four-language-execution.json`](results/four-language-execution.json)
records actual Linux arm64 Java, Python, Node.js and .NET startup, HTTP,
dependencies, exec, exec probes, Kubernetes overrides, identity/HOME/PATH,
read-only root and packaged files, and writable nested volumes. The run failed
later on a test-driver webhook-name assumption before rotation. Discovery was
corrected; this failed run is not reported as a complete suite pass.

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

The full four-language rotation/barrier/pinned-label/revocation/recovery suite
must pass, along with the remaining ledger requirements. In particular:

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
