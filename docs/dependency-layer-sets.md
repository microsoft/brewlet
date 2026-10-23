# Dependency layer sets and updates

A layer set is an independently published dependency graph, not a second
container that the shim pulls. The producer verifies and composes its layers
into one application image. Kubelet still pulls that final image.

For artifact-to-artifact release composition, signed final-image evidence,
dependency replacement and read-only fleet export for external scanners, use
the [governed composition workflow](managed-composition.md).

## Publish and consume a small local example

Use a new work directory and ordinary world-readable files. These commands
illustrate producer composition only; they do not deploy a workload or prove
library compatibility.

Create `dependencies/message.py` containing `MESSAGE = "shared dependency"`.
Create `layerset.json`:

```json
{
  "schemaVersion": 1,
  "name": "example-dependencies",
  "version": "1",
  "ecosystem": "python",
  "layers": []
}
```

The producer populates layer digest/diff-ID bindings from the supplied files:

```sh
brewlet layer-set example.invalid/dependencies:v1 \
  --store ./dependency-oci --config layerset.json \
  --layer dependency=./dependencies:/app/dependencies
brewlet inspect ./dependency-oci
```

Create an app importing `message`, and set explicit descriptor environment
`PYTHONPATH=/app/dependencies`. Then compose:

```sh
brewlet build example.invalid/app:v1 --store ./application-oci \
  --platform linux/arm64 --launch launch.json \
  --layer-set ./dependency-oci --layer app=./src:/app/src
brewlet inspect ./application-oci
```

Declare your actual platforms. The consumed compressed dependency layers are
reused byte-for-byte. `layerset-evidence` binds the selected manifest/config,
lock/SBOM/provenance and layer identities. Remote publication uses the same
explicit process with your registry and without `--store`.

## Locks, provenance and signing

A neutral lock may record ecosystem/name/version, `/app` path and SHA-256 for
each dependency file. Build it from the actual producer inputs, not guessed
package versions. SBOM/provenance and trust options are described in the
[CLI reference](cli-reference.md#layer-set-governance).

Use a separately managed signing key and explicit builder identity. The current
CLI does not supply main's `keygen`; do not add a fabricated command to a recipe.
Trusted signer policy is independent of whether an unsigned graph is valid OCI.

For Java, the optional Maven producer can resolve project dependencies or consume
one verified complete Java layer set for a genuinely thin JAR. Generic core does
not interpret a Maven BOM or resolve missing dependencies.

## Respond to a dependency CVE

1. Identify the affected dependency file/version and the layer-set/final image digests that contain it.
2. Publish a reviewed replacement layer set with updated lock/SBOM/provenance.
3. Recompose affected applications from unchanged app inputs and the new dependency graph.
4. Publish new application digests and update native workload manifests through their normal delivery process.
5. Verify the rollout and actual application behavior; retain previous digests for controlled rollback.

This changes the application's dependency layers, unlike a runtime-only source
rotation. Updating the node runtime cannot repair an affected copy under `/app`.
Neither workflow justifies rewriting a running process or bypassing an epoch barrier.
