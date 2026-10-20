# Proposal 0010: governed dependency composition and remediation

- **Status:** implemented; accepted on 2026-10-20. See the [separate qualification ledger](../../acceptance/composition-requirements.json).
- **Scope:** Brewlet 2.0 producer, release and policy workflows.
- **Extends:** [Proposal 0009 section 6.5](0009-language-agnostic-runtime-images.md#65-dependencies-as-layers-or-independently).
- **Does not change:** runtime-image selection, node provisioning, runc execution,
  startup fencing, leases or host cleanup.
- **Reference main:** `fa7085335b220e29df6affbece4558c5f1259ecb`.
- **Reference 2.0:** `7077c1d3b7c2019ad10b8d3160f60a15dac9c930`.

The requirements below are the approved implementation contract. Qualification
must be recorded separately; code presence is not acceptance evidence.
This approval does not authorize a built-in vulnerability scanner, a runtime
dependency resolver or a new workload CRD.

## 1. Recommendation

Complete the platform-managed dependency experience around the existing generic
layer-set model:

1. The platform publishes an immutable dependency payload with a complete lock,
   SBOM and verifiable publisher provenance.
2. Application producers retain normal development workflows and publish
   explicit application artifacts.
3. Release tooling composes pinned application and dependency artifacts into
   one final application image, preserving application-layer bytes.
4. A separate release-builder identity attests the exact final composition.
5. Fleet inventory traces actual running image digests to dependency evidence.
6. Remediation replaces the dependency input, tests the new composition, and
   rolls native workloads to a new application digest.

The developer-facing delivery configuration can therefore name the application
and approved dependency set. The rendered Kubernetes workload still names
only the final application image and its explicit approved runtime ID.

An SBOM is an inventory, not executable content. The published dependency
artifact MUST contain the actual files; no package-manager resolution from
SBOM entries occurs in the shim, operator or generic composer.

## 2. Existing foundations and actual gaps

This table records the pinned reference-2.0 baseline above. Implemented release
interfaces and their qualified scopes are documented in the
[composition guide](../../docs/managed-composition.md) and the separate ledger.

| Concern | Already in 2.0 | Proposed work |
| --- | --- | --- |
| Dependency publication | Generic OCI layer sets, complete lock validation when a lock is supplied, mandatory SBOM, optional signed provenance. | A clear managed-publication workflow and release policy. |
| Java project intelligence | Optional Maven producer checks the explicit source BOM and resolved scoped graph, rejects nested JARs and generates argv. | Improve platform/application examples; retain these checks outside core. |
| Consumption | `push --layer-set` consumes verified dependency layers with exact digest/diff-ID reuse. | Consume an already-published application artifact without needing its original directories. |
| Composition evidence | `brewlet.sh/layerset-evidence`. | Final-image-bound, signed composition evidence and verification. |
| Operations | Native Kubernetes workloads and scoped execution evidence. | Running-fleet inventory, dependency remediation and end-to-end qualification. |

Main's managed bundle implementation is the behavioral reference, not a
scaffold to copy wholesale. In particular, main also composes before deployment;
its tier-19 update scenario starts from retained thin JARs, not a general
two-published-artifact composition command.

## 3. Goals and non-goals

### Goals

- Make approved dependency consumption practical for platform and application
  teams without turning generic core into a language toolchain.
- Preserve compressed application and dependency layer descriptors and diff IDs
  across composition and dependency-only updates.
- Make the selected dependencies, producer identities and actual deployed
  image traceable and independently verifiable.
- Separate content integrity, publisher authorization, composition and
  application compatibility; none implies the others.
- Qualify Java, Python, Node.js and .NET with real independently published sets
  and compatible unchanged upstream runtime images.
- Qualify WebAssembly through both Wasmtime and Spin, preserving explicit guest
  capabilities and actual platform-built host provenance in a separate companion
  scope (§11.1); do not treat native-language receipts as implicit Wasm proof.

### Non-goals

- A dependency reference that causes a node/shim to fetch another OCI artifact.
- A dependency inventory CRD, workload CRD, admission-time image rewrite or
  dependency restart controller inside the Brewlet operator.
- Maven/npm/pip/NuGet resolution or version-range interpretation in generic core.
- Hot replacement of libraries in running processes.
- Claiming binary/API compatibility from an SBOM, a version number or a signature.
- Restoring 1.x artifact formats or JVM-specific launch rewriting.
- A new generic producer plugin framework.

## 4. Roles and trust

| Role | Responsibility |
| --- | --- |
| Platform dependency publisher | Select and test the dependency closure, publish payload/lock/SBOM, sign its provenance. |
| Application producer | Build application bytes and explicit launch metadata; preserve producer-specific build evidence. |
| Release composer | Authorize pinned inputs, compose the output, sign what was actually selected. |
| Application deployment workflow | Approve/test the candidate and update its ordinary workload manifest. |
| Cluster policy | Verify required final-image evidence and identities before permitting deployment. |

These are artifact/publication roles, not two owners of the application
workload. Runtime infrastructure approval remains a separate platform concern.

Bundle-publisher keys and release-builder keys MUST represent distinct trust
decisions. A bundle signature does not authorize every application using it;
a composition signature does not prove that the application passed its tests.
Private keys stay in CI secret delivery, never in descriptors or app layers.

Managed composition MUST require an explicit trust policy. The production
policy requires trusted dependency publisher provenance and a signed output.
An explicitly selected development policy MAY permit unsigned inputs/outputs,
but MUST NOT label them production-verified.
Present invalid/untrusted provenance MUST NOT be treated as unsigned.
Key rotation MAY accept multiple keys under explicit identity/subject rules.

## 5. Initial bounded contract

Version 1 supports **one complete managed dependency set and one explicit
Linux platform per composition**. This matches the current Maven managed
workflow and keeps replacement unambiguous.

This restriction applies to the new composition operation, not to existing
generic `push` capabilities. Merging partial dependency catalogs, resolving
overlaps between independently governed sets and multi-platform composition
are not silently inferred. They require a subsequent explicit contract.

### 5.1 Inputs

All remote inputs MUST be canonical digest-pinned references. Mutable tags
must be resolved and approved by the delivery workflow before invocation.

- An application input is a valid version-2 application OCI graph with
  explicit app layers, launch metadata and platform/config consistency.
- Initial composition accepts application inputs with no dependency-role
  layers. Structural validity does not mean that this payload image is a
  ready-to-run workload before dependency composition.
- Replacement from a previously composed image requires supported, verified
  composition evidence identifying its application and dependency descriptors.
  An unsigned annotation or a layer-role label alone is insufficient to decide
  which layers may be removed.
- Arbitrary previously bundled dependencies MUST be rejected rather than
  guessed, stripped or silently carried forward.
- Managed layer sets MUST provide a complete verified lock, exactly one valid
  SBOM and the provenance required by the selected policy. This is stricter
  than treating every otherwise-valid unlocked generic layer set as managed.
- Input platform selection MUST be explicit and unambiguous. Selecting a
  member of an index MUST verify its descriptor and relationship to that index.

An application payload can be built against dependency set A and later
delivered with set B. Original build provenance MUST continue to say A;
the composer MUST NOT rewrite it to claim the application was built or tested
against B.

### 5.2 Layout and launch

The composer MUST:

- validate all input descriptors, sizes, hashes, diff IDs and current artifact
  constraints before producing a deployable output;
- use explicit layer roles and validated composition evidence, not filename,
  extension or language detection;
- preserve app-layer compressed bytes, order and diff IDs;
- reuse the selected dependency payload layers byte-for-byte;
- preserve launch-layer bytes by default and regenerate the OCI config's
  rootfs diff-ID list/config mirror for the actual output graph;
- keep optional top-level `runtimeImages` semantics unchanged;
- require an explicit `--launch` input for any requested launch replacement,
  validating and recording that replacement rather than inferring argv.

The composer MUST NOT move files to make a dependency layout fit a launch
descriptor automatically. For example, `/app/lib` is a producer contract, not
a Java-specific extraction rule in the composer.

Managed dependency paths MUST NOT be shadowed, removed by whiteouts or
redirected through symlinks by an application layer. The same existing safe
composition/path-resolution mechanisms SHOULD be reused to compare the
effective managed file inventory and hashes with the selected set.
Ambiguous collisions fail closed.

This is filesystem-level protection, not a language semantic guarantee.
It cannot prove that a JAR contains no shaded duplicate classes, that an
application's loader uses only approved files, or that a dynamic package is
API-compatible. Relevant producer validation and testing remain necessary.

### 5.3 Output and publication

The output is an ordinary version-2 application OCI image: dependency payload,
preserved app payload and exactly one launch layer. Kubelet still pulls one
application image per container.

Equivalent pinned inputs and explicit options SHOULD produce the same output
manifest/config digests. Timestamps and signatures belong in referrers rather
than forcing otherwise-identical application layers to change.

No output tag, accepted manifest or successful release handoff may be published
after a validation or required-signing failure. Registry uploads of immutable
blobs may precede a failed operation; such unreferenced blobs are not a
successfully published release.
Credentials MUST remain origin-scoped during cross-registry copies/mounts.

## 6. Producer interface

The release interface is:

```sh
brewlet compose registry.example.com/apps/orders:candidate \
  --application registry.example.com/builds/orders@sha256:<application-digest> \
  --layer-set registry.example.com/platform/java-web@sha256:<set-digest> \
  --platform linux/arm64 \
  --policy release-policy.yaml \
  --signing-key "$COMPOSER_SIGNING_KEY_FILE" \
  --builder-identity https://ci.example.com/release-composer \
  --push-result composed-image.json
```

`--application` is either an initial application payload or a previously
composed image satisfying section 5.1. The supplied set is the complete desired
dependency closure, not an implicit subset update.
The signing environment variable contains a file path, not private key bytes.

The existing `push` and Maven workflows remain valid. Maven can continue to
derive the graph and launch from project inputs; the new operation is for
artifact-to-artifact release composition, not a replacement Maven resolver.

In a GitOps/release system, the two pinned references are pipeline inputs.
After verification and testing, it updates an ordinary Deployment image digest.
No proposed fields in this section are Kubernetes/Brewlet Pod annotations.

## 7. Final composition attestation

Define a versioned generic predicate, emitted as a signed OCI referrer and
bound to the exact output image manifest digest.
It MUST bind at least:

- application source root and selected platform-manifest digests;
- platform and preserved application-layer descriptors/diff IDs;
- selected dependency-set, config, lock and SBOM digests;
- dependency descriptors/diff IDs and their effective ordering;
- original/effective launch digests and output config digest;
- parent composition digest for replacement, where applicable;
- release-builder identity and predicate version.

This supplements existing layer-set publisher provenance. A generic verifier
must not need Maven GAVs, JAR inspection, Python package resolution or CLR
understanding to verify the final composition.
Java-specific graph/thin-JAR evidence can be bound as producer-owned evidence,
without becoming mandatory language-specific core fields.

An `inspect`/verification interface MUST distinguish unsigned structural
evidence, verified publisher provenance, verified composition provenance and
any separately verified test/release approval. It MUST NOT collapse these into
one ambiguous `verified` flag.

The verifier SHOULD be callable by external admission policy. Initial delivery
MUST NOT claim main's Ratify parity until a concrete integration is implemented,
its policy is documented and real accept/reject cases are exercised.
The inventory-only Brewlet admission path does not become a registry resolver.

## 8. Compatibility and dependency-only remediation

Layer replacement changes the deployed dependency closure, even when no
application code is recompiled. Therefore:

- initial Maven publication retains current exact graph/BOM/scoped-hash checks;
- generic replacement records old and new materials honestly;
- a dependency-only release MUST run its configured compatibility/integration
  checks against the final candidate image before deployment;
- changing package versions/hashes does not authorize changing runtime IDs,
  executable paths or process security implicitly;
- compatibility failures stop the release; they never fall back to locally
  resolving/bundling another graph or bypassing provenance;
- rollback selects a prior approved final image digest, subject to current
  policy; revocation does not grant an unsigned emergency bypass.

For Java, a retained thin JAR may work with a patched library without
recompilation. That must be tested; the old Maven build graph cannot be relabeled
as the newly tested graph. Equivalent ecosystem-specific responsibilities stay
in application CI/producer tooling for Python, Node and .NET.

## 9. Fleet inventory and remediation workflow

First provide a read-only inventory/export path that:

1. selects explicit namespaces/workloads;
2. records actual Pod UID/container ID and observed running image identity;
3. resolves/verifies the application OCI digest and composition evidence;
4. follows the exact dependency-set/SBOM bindings;
5. exports package/file identities, versions and PURLs with their provenance.

Do not assume every CRI `imageID` is the image manifest digest. Correlate
protected/observed identities and verified OCI membership rather than guessing
from a tag, desired Deployment spec or an unrelated config digest.

Missing evidence, inaccessible registries, invalid signatures and ambiguous
identity MUST be reported as **unverified**, not clean.
Namespace exclusions and stale snapshots must remain visible in the report.

Vulnerability evaluation consumes these facts through an explicitly configured
scanner/advisory source. Generic core MUST NOT apply one naive version comparator
to Maven, PEP 440, npm and NuGet ranges.
Fixture advisories can drive deterministic tests; they are not a production
vulnerability database.

The release workflow then publishes a replacement set, composes candidates,
tests them, updates native workload manifests and observes actual rollout.
Post-remediation inventory MUST show the new running application digests and
dependency identities. It MUST NOT report success from a template patch alone.
No dependency-update loop is added to the node provisioner.

## 10. Implementation milestones

| Milestone | Deliverable | Exit criteria |
| --- | --- | --- |
| P1: contract and producer experience | Review this contract; add concrete platform/application Maven projects and neutral publication examples. | Current mechanisms are documented/tested; new commands remain explicitly proposed until implemented. |
| P2: initial generic composition | App-payload plus one managed set, strict paths/platforms, byte-preserving output and deterministic result handoff. | No language inference; all malformed/collision/unknown-input cases fail; initial composition works locally. |
| P3: composition trust | Versioned predicate, signing, verification, key/identity policy and inspect output. | Subject/digest/order tampering and unsigned downgrade fail; signing failures publish no accepted output. |
| P4: dependency replacement | Accept verified prior compositions; replace the set while preserving app layers/launch unless explicitly changed. | Original build evidence stays intact; compatibility tests run against the new digest; no unverified layer stripping. |
| P5: fleet/remediation qualification | Read-only inventory, scanner integration boundary, release/GitOps example and real-node matrix. | Affected/unaffected/unverified cases, complete rollout observation and rollback/revocation behavior are proven. |

P3 depends on P2; P4 depends on P2/P3; P5's release scenario depends on P4.
Read-only inventory prototyping and P1 documentation can be developed separately.
No parallel orchestration or credit budget is authorized by this plan.

## 11. Required acceptance matrix

The final feature is not qualified by separate tests of its constituent parts.
For **each** Java, Python, Node.js and .NET case:

1. Publish an independent locked dependency set, SBOM and publisher provenance.
2. Publish an immutable application payload through its normal external
   toolchain/producer.
3. Compose and verify the final image; prove exact app/dependency layer and
   diff-ID reuse, launch/config consistency and conflict rejection.
4. Run on a pinned compatible **unchanged upstream runtime image** with actual
   HTTP/dependency, probes/Exec, identity, filesystem and resource observations.
5. Publish a patched set, recompose the unchanged application layers, test the
   new candidate and roll a native workload with controlled availability.
6. Observe the actual updated dependencies and image digests in running Pods;
   verify old processes are replaced normally rather than hot-patched.
7. Complete normal workload/profile cleanup and exact-owned fixture destruction.

Native arm64 local proof and native amd64 CI proof are separate scopes.
Cross-builds do not substitute for either. The unchanged .NET runtime input
must be discovered and policy-qualified (for example, a reviewed chiseled
candidate); an adapted root must not be relabeled unchanged.
An unavailable compliant input remains an explicit matrix blocker.

### 11.1 WebAssembly composition companion

WebAssembly is a required additional workload category, not implicitly covered
by the four native-language cases. Qualify **both** the Wasmtime WASIp1 command
and the Spin SDK HTTP component on native arm64 and native amd64 separately:

1. Publish a complete locked external dependency payload, CycloneDX SBOM and
   publisher provenance independently of the guest/application artifact.
2. Compose and verify a signed final image. Exercise required-trust and managed
   path-conflict rejection without publishing accepted output after failure.
3. Observe the actual initial guest and Wasmtime guest Exec or Spin HTTP/probe
   behavior, host Exec identity, non-root restrictions, read-only dependencies
   and exclusion of the runtime filesystem and trusted engine cache.
4. After original workloads are running, publish a changed external payload and
   recompose from the verified prior image. Preserve the exact `.wasm` file
   hashes, application/launch descriptors and diff IDs. Verify the old workloads
   retain the old dependency and the candidate actually consumes the new bytes.
5. Roll ordinary native Deployments only after candidate checks. Verify new
   container/Pod identities, unchanged selected host source/epoch/generation,
   real guest-visible dependency changes and pre/post observed inventory/SBOM
   attribution. Controlled zero-failed-request observation applies to Spin HTTP,
   not to a fabricated HTTP endpoint for the Wasmtime command.
6. Complete normal profile/task/root cleanup and exact-owned fixture destruction.

Record the host images' actual provenance. The existing examples use
platform-built roots combining pinned distroless C/C++ userland with unchanged,
checksum-pinned engine binaries; do not call these unchanged upstream engine
OCI images or weaken the four native-language unchanged-root requirement.

Replacing external files/components that a guest explicitly consumes is not a
patch for code statically linked into a `.wasm` file. Compiled-in dependency
updates require a guest rebuild; host-engine/OS updates require runtime-image
rotation. Neither a fixture asset update nor an SBOM proves compiled-in library
remediation or complete application CVE coverage.

Additional mandatory cases:

- wrong BOM/graph/scope/classifier/hash, missing members and thin-JAR failures
  through the optional Maven producer;
- wrong keys/identities/subjects, malformed present provenance, expired/revoked
  trust policy and each allowed/denied signing combination;
- managed-path shadowing, whiteouts, symlink aliases and attempted file injection;
- source/output platform mismatch and ambiguous index membership;
- missing/invalid final composition evidence and no accepted output after failure;
- old/new source references and app-layer digests preserved across replacement;
- affected control/staging workloads and unknown images handled correctly;
- zero dropped requests for the deliberately configured availability scenario,
  not a universal zero-downtime product guarantee.

Keep a requirements-to-tests checklist for these obligations. Publish exact
inputs, commands, results and limitations; preserve failed runs as failed.
The existing 111-requirement baseline does not qualify this feature.

## 12. Documentation and release boundaries

Publish an end-to-end platform-to-application guide, generic composition
reference, trust/attestation reference and remediation runbook.
Examples must use explicit platforms, immutable references and readable
configuration, not an invented runtime dependency annotation.

Document which source/app formats the new composer accepts and which it
rejects. Archive existing receipts rather than rewriting them as evidence for
new semantics. Keep installation/source-revision compatibility explicit.

This approval authorizes the bounded producer/release work,
not changes to Proposal 0009's rejected shim-pulled dependency design.

## References

- [Main managed bundle workflow at the inspected revision](https://github.com/microsoft/brewlet/blob/fa7085335b220e29df6affbece4558c5f1259ecb/docs/managed-dependency-bundles.md)
- [Main bundle validation](https://github.com/microsoft/brewlet/blob/fa7085335b220e29df6affbece4558c5f1259ecb/core/internal/artifact/dependency_bundle.go)
- [Main CVE remediation scenario](https://github.com/microsoft/brewlet/blob/fa7085335b220e29df6affbece4558c5f1259ecb/integration-tests/e2e/tier19-cve-remediation.sh)
- [2.0 Maven managed consumption](https://github.com/microsoft/brewlet/blob/7077c1d3b7c2019ad10b8d3160f60a15dac9c930/maven-plugin/README.md#managed-consumption)
- [2.0 layer-set implementation](https://github.com/microsoft/brewlet/blob/7077c1d3b7c2019ad10b8d3160f60a15dac9c930/pkg/artifact/layerset.go)
