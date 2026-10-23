# Four-language contract fixtures

Each fixture serves JSON on port 8000 and supports `--probe` to print the same
JSON once. All report runtime version, a value from a separate dependency layer,
argv boundaries, cwd, UID/GID, PATH, HOME and one explicitly named test
environment value. They do not dump arbitrary environment variables.

Every `launch.json` is hand-written. No CLI inference is involved. Application
and dependency source groups are supplied explicitly. The Java Maven producer is
an additional route to an ordinary v1 artifact, not an alternate runtime path.

Prerequisites: Go as pinned in `go.mod`, Maven 3.9+, Java 21+, Python 3.12+,
Node.js 22+ and .NET SDK **10.0.401**. The .NET SDK does not roll forward.
Java compiler/plugin versions and deterministic JAR timestamps are pinned in
the fixture POMs. All four CLI artifacts explicitly declare `linux/amd64` and
`linux/arm64`; the supplied code is managed/neutral and contains no native
dependencies.

```sh
make build
python3 integration-tests/fixtures/check.py --brewlet ./bin/brewlet
```

The harness builds and runs all four on the **host**, packages each twice,
checks deterministic OCI targets and layer deduplication across declared
platforms, and packages Java again through Maven. Java additionally publishes
an explicitly BOM-governed layer set, consumes it without duplicate dependency
layers, and rejects changed Maven scope despite identical dependency bytes.
Shared raw launch/ID/platform fixtures also pass through the actual Maven
adapter. It retains invocation-unique
temporary work and `result.json`. For fixture-only development:

```sh
python3 integration-tests/fixtures/check.py --host-only
```

Neither command is a real-node test. It does not establish sandbox isolation,
admission, Kubernetes overrides, probes, runtime rotation, installation,
startup fencing, exec parity, CRI usage/eviction or provider autoscaling.
Those requirements remain independently tracked in the acceptance ledger.

On macOS, the system may attach an unremovable `com.apple.provenance` xattr to
inputs. The producer correctly rejects it; tests must not weaken that policy.
To exercise packaging on Linux instead, build a Linux arm64 CLI and use
`integration-tests/linux-cli.sh`, setting `BREWLET_TEST_BINARY` to its absolute
path, `BREWLET_TEST_WORK_PARENT` to an invocation-owned work directory, and
`BREWLET_TEST_IMAGE` to the locally built, pinned Python fixture image. Pass the
same work parent to this harness. The wrapper mounts sources read-only, runs
without capabilities or networking, and writes only under the test work parent.
Registry integration separately opts into bridge networking.
