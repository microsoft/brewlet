# Governed dependency composition

Platform teams publish approved dependency payloads once. Application teams
publish application payloads. A release pipeline composes their **immutable
digests** into an ordinary Brewlet application image before Kubernetes sees it.
There is no deployment-time dependency resolver, second shim pull, workload
CRD or hot replacement of libraries.

This implements the producer/release boundary of Proposal 0010. See the
[verification page](verification.md) for qualified execution scopes; the
presence of a command is not evidence that every runtime/platform is qualified.

## Platform: publish the complete dependency closure

Publish the actual dependency files, a complete file lock, a CycloneDX 1.5
SBOM and signed publisher provenance:

```sh
brewlet layer-set registry.example.com/platform/dependencies:2026.10 \
  --config layerset.json --lock dependencies.lock.json \
  --layer dependency=./dependencies:/app/dependencies \
  --signing-key "$PUBLISHER_KEY_FILE" \
  --builder-identity https://ci.example.com/platform
```

`layer-set` computes compressed descriptors and diff IDs. The lock covers every
payload file with its actual hash and explicit package identity; supply
ecosystem-specific PURLs so external scanners can match advisories accurately.
The generic producer never guesses package versions or resolves dependencies.

Use the [optional Maven producer](https://github.com/microsoft/brewlet/blob/brewlet-2.0/maven-plugin/README.md) for a source-BOM
and scoped Maven graph rather than putting Java knowledge in the composer.
The checked-in `integration-tests/fixtures/java/bom`, `dependency` and `app`
projects provide a concrete platform BOM, library and consuming thin-JAR app.

## Application: retain an app-only immutable input

Build with the normal external toolchain, then package only the application
contribution and its matching explicit launch descriptor:

```sh
brewlet push registry.example.com/builds/orders:payload \
  --platform linux/arm64 --launch launch.json \
  --layer app=./application:/app
```

This payload can be structurally valid without being runnable alone. Its
dependencies are added at release time. Initial composition rejects
dependency-role layers; it does not guess which arbitrary bundled files to
strip out.

For an app-only Maven publication:

```sh
mvn package sh.brewlet:brewlet-maven-plugin:2.0.0-alpha.1:push \
  -Dbrewlet.applicationOnly=true \
  -Dbrewlet.sourceBom=sh.brewlet.fixture:fixture-bom:2.0.0 \
  -Dbrewlet.image=registry.example.com/builds/orders:payload \
  -Dbrewlet.platforms=linux/arm64
```

The producer validates the actual source BOM, requires a thin JAR, excludes
dependency payloads and retains the original scoped graph as the ordinary
application file `/app/.brewlet/application-build.json`. Release composition
cannot rewrite that build history to pretend the app was compiled against a
new bundle. Existing direct Maven managed-consumption checks remain applicable
to that publication workflow.

## Release: compose, verify, test, deploy

Choose one explicit Linux platform and one complete managed set. Resolve tags
in the delivery workflow first; `compose` accepts only canonical digest-pinned
remote inputs or explicit local `LAYOUT@sha256:...` inputs:

```sh
brewlet compose registry.example.com/releases/orders:candidate \
  --application "$APPLICATION_REF_AT_DIGEST" \
  --layer-set "$DEPENDENCY_SET_REF_AT_DIGEST" \
  --platform linux/arm64 --policy release-policy.yaml \
  --signing-key "$COMPOSER_KEY_FILE" \
  --builder-identity https://ci.example.com/release \
  --push-result candidate.json

brewlet verify "$CANDIDATE_REF_AT_DIGEST" \
  --platform linux/arm64 --policy release-policy.yaml
```

`--store DIRECTORY` publishes a local OCI layout instead. Application and
dependency compressed bytes, descriptors, order and diff IDs are preserved.
Launch-layer bytes remain unchanged unless an explicit `--launch FILE`
replacement is requested; that replacement is validated and recorded.
The config mirror/rootfs list is regenerated for the actual output graph.

Application layers cannot overwrite managed paths, replace their ancestors
with files/symlinks, hide them with whiteouts, or make them opaque. This is
filesystem integrity, **not** proof that app code contains no shaded/vendored
copies or that it executes only approved code.

The successful receipt contains the exact output digest and separate
`structural`, `publisherProvenance` and `compositionSignature` observations.
`testReleaseApproval: not-evaluated` means this verifier did not verify a test
or release approval. A signature is not a compatibility test.

Run the configured application/integration tests against this exact candidate
digest before updating the ordinary Deployment image. A failed candidate test
must stop that delivery pipeline; do not resolve another dependency graph or
bypass trust as a fallback.

## Trust policy

Policy files accept strict JSON or YAML. Public SPKI PEMs are embedded; private
keys stay in CI secret delivery. A production policy requires active publisher
and composer keys and explicit identities:

```yaml
schemaVersion: 1
mode: production
notAfter: "2027-01-01T00:00:00Z"
publisher:
  identity: https://ci.example.com/platform
  keys:
    - publicKey: |
        -----BEGIN PUBLIC KEY-----
        <publisher SPKI PEM body>
        -----END PUBLIC KEY-----
      notBefore: "2026-01-01T00:00:00Z"
      notAfter: "2027-01-01T00:00:00Z"
composer:
  identity: https://ci.example.com/release
  keys:
    - publicKey: |
        -----BEGIN PUBLIC KEY-----
        <release SPKI PEM body>
        -----END PUBLIC KEY-----
      notBefore: "2026-01-01T00:00:00Z"
      notAfter: "2027-01-01T00:00:00Z"
```

The PEM bodies are placeholders: generate/manage real keys outside Brewlet.
Each key can be marked `revoked: true`; overlapping active keys permit rotation.
Each authority optionally restricts exact `subjects` or denies
`revokedSubjects` (SHA-256 digests). Expired policies/keys and revoked subjects
are not unsigned development inputs.

An explicitly chosen `development` policy may allow unsigned composition.
Its structural evidence is not production-verified and **cannot authorize
dependency stripping for a replacement**. Present invalid/untrusted signatures
never become an unsigned fallback.

The signed, versioned in-toto/DSSE predicate is an OCI referrer to the final
manifest. It binds original and incoming source roots/platform manifests,
application layers, selected set/config/lock/SBOM/publisher evidence, original/
input/effective launch layers, output config, parent composition and release
identity. Supporting immutable graphs and their referrers travel with the
output, so verification does not depend on a mutable source tag.

For cross-registry operations, `compose`, `verify` and `inventory` accept
`--registry-auth FILE`, a JSON map from exact `https://host[:port]` origins to
`username`/`password` objects. Alternatively set `BREWLET_REGISTRY_USERNAME`,
`BREWLET_REGISTRY_PASSWORD` **and** `BREWLET_REGISTRY_ORIGIN`. A source registry
does not receive another origin's credentials.

## Dependency-only replacement and rollback

Pass the prior signed composition as `--application` and the approved
replacement set as `--layer-set`. Brewlet verifies the prior composition before
identifying removable dependency layers. An unsigned annotation alone is not
authority. Original app-layer bytes and build evidence remain intact.

Publish and test the candidate, roll the native Deployment, then inspect actual
running identities. Old processes keep their old dependencies until normal
replacement. A rollback selects a previous approved final image digest under
current trust policy; it is not a trust or revocation bypass.

## Operators: export facts for external scanners

```sh
brewlet inventory --kubeconfig "$KUBECONFIG" --context production \
  --namespace payments --namespace orders \
  --policy release-policy.yaml --output dependencies.json
```

Repeat `--workload Kind/namespace/name` to narrow native workloads.
`--max-age` bounds node observation age (at most two minutes). The reader needs
get/list Pods, get native controller owners, get Nodes and get NodeProfiles.
See `examples/managed-dependencies/inventory-rbac.yaml`; it creates no binding
and grants no Secret, workload-write, host-exec or inventory-write permission.

The exporter correlates Pod UID/container ID, protected shim-recorded
application root/manifest/config identities and fresh worker observations.
It follows owner-reference UIDs, not matching labels or desired image tags.
A CRI config digest is not relabeled as a manifest digest.

The schema-versioned report contains per-container/workload identities and
deduplicated verified artifact facts, complete package locks and the exact
publisher-bound SBOMs. Missing/ambiguous/stale evidence or registry/trust errors
remain `unverified`; partial reports are retained and the command exits nonzero.
Explicit scope and capture times remain visible. These diagnostic observations
are never cleanup or host-mutation authority.

There is **no Brewlet vulnerability scanner or advisory database**. External
automation can trigger on bundle publication, fetch its CycloneDX OCI referrer,
verify digest/provenance bindings, and pass the document to its existing scanner.
It can also read each `artifacts` entry's `sbom` from this export, scan once per
SBOM/scanner/database snapshot, and join package findings back through each
container's `evidenceKey`. Existing scanner tooling owns ecosystem version
matching. Scheduled rescanning can find newly disclosed CVEs in unchanged bytes.

Presence in an observed image does not prove vulnerable-code reachability,
actual module loading or absence of embedded copies. Managed-set coverage is
not exhaustive application or infrastructure-runtime coverage. An unavailable
SBOM or scan result is unknown, never a clean bill of health.

## WebAssembly is a separate required workload scope

The [WebAssembly companion](webassembly.md#governed-external-dependency-replacement)
exercises the same producer, release policy, signed final image, replacement
and fleet-export path for both Wasmtime and Spin. Its native arm64/amd64
receipts are required separately from the four native-language receipts.
Neither language detection nor a Wasm-specific composer is added.

Its dependency update changes a declared external data file that both real
guests consume, preserving the compiled `.wasm` hashes and fixed host images.
It is not evidence that compiled-in libraries can be patched without rebuilding
the guest, or that dynamic component linking has been qualified. Engine/OS
remediation remains a RuntimeImage update. Platform-built host roots must not
be called unchanged upstream engine OCI images.
