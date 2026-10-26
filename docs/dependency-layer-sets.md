# Dependency layer sets and updates

A layer set is a dependency graph published independently of any application.
`brewlet build --layer-set` copies its layers into your application image when
you build it. The kubelet still pulls only that final image; the shim never
pulls a layer set itself.

For release-time composition, signed final images, dependency replacement and
read-only fleet export for external scanners, use the
[governed composition workflow](managed-composition.md).

## Publish and consume a small local example

This example uses local OCI layouts instead of a registry. It shows how
composition works; it does not deploy a workload or test library
compatibility. Use a new directory and ordinary world-readable files.

1. Create `dependencies/message.py` containing `MESSAGE = "shared dependency"`.

2. Create `layerset.json`:

    ```json
    {
      "schemaVersion": 1,
      "name": "example-dependencies",
      "version": "1",
      "ecosystem": "python",
      "layers": []
    }
    ```

    Leave `layers` empty; `brewlet layer-set` fills in the layer digests from
    the files you supply.

3. Publish the layer set to a local layout and inspect it:

    ```sh
    brewlet layer-set example.invalid/dependencies:v1 \
      --store ./dependency-oci --config layerset.json \
      --layer dependency=./dependencies:/app/dependencies
    brewlet inspect ./dependency-oci
    ```

4. Create an application under `./src` that imports `message`, and a
   `launch.json` whose `process.env` sets `PYTHONPATH=/app/dependencies`.

5. Build the application with the layer set and inspect the result:

    ```sh
    brewlet build example.invalid/app:v1 --store ./application-oci \
      --platform linux/arm64 --launch launch.json \
      --layer-set ./dependency-oci --layer app=./src:/app/src
    brewlet inspect ./application-oci
    ```

Declare your actual platforms. The compressed dependency layers are reused
byte for byte. The image's `brewlet.sh/layerset-evidence` annotation records
the layer set's manifest, config, lock, SBOM, provenance and layer digests.

To use a registry instead, omit `--store` from `brewlet layer-set` so it
publishes to the reference, and pass that reference to `--layer-set`.

## Locks, provenance and signing

An optional lock (`--lock`) records the ecosystem, name, version, `/app` path
and SHA-256 of each dependency file. Generate it from the files you actually
package, not from guessed package versions. SBOM, signing and trust options
are listed in the [CLI reference](cli-reference.md#command-specific-options).

Sign with a separately managed PKCS#8 ECDSA-P256 key and an explicit
`--builder-identity`. Brewlet does not generate keys; use your existing key
management. Whether a set is trusted is decided by the consumer's trust flags,
independently of whether it is valid OCI.

For Java, the optional [Maven plugin](applications.md#optional-maven-producer) can resolve project
dependencies or consume one verified, complete Java layer set for a thin JAR.
The `brewlet` CLI does not interpret Maven BOMs or resolve missing
dependencies.

## Respond to a dependency CVE

1. Find the affected dependency file and version, and the layer-set and
   application image digests that contain it.
2. Publish a reviewed replacement layer set with an updated lock, SBOM and
   provenance.
3. Rebuild the affected applications from unchanged app inputs and the new
   layer set.
4. Publish the new application digests and update your workload manifests
   through your normal delivery process.
5. Verify the rollout and application behavior. Keep the previous digests for
   rollback.

This changes the application's dependency layers. Updating the runtime image on
nodes cannot fix a vulnerable copy under `/app`, and neither update modifies a
running process. If you use governed bundles, follow the
[dependency remediation runbook](dependency-remediation.md) instead.
