# Proposal 0010: governed dependency composition and remediation

- **Status:** draft for review; not accepted or implemented.
- **Scope:** Brewlet 2.0 producer, release and policy workflows.
- **Extends:** [Proposal 0009 section 6.5](0009-language-agnostic-runtime-images.md#65-dependencies-as-layers-or-independently).
- **Does not change:** runtime-image selection, node provisioning, runc execution,
  startup fencing, leases or host cleanup.
- **Reference main:** `fa7085335b220e29df6affbece4558c5f1259ecb`.
- **Reference 2.0:** `7077c1d3b7c2019ad10b8d3160f60a15dac9c930`.

The requirements below describe the proposed feature, not current guarantees.
CLI examples in this document are proposed interfaces unless marked existing.
Implementation requires a separate approval; this proposal does not authorize
a runtime dependency resolver or a new workload CRD.

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
5. Operators can scan published bundles and answer both "what dependencies does
   this application contain?" and "which applications contain this dependency
   or CVE?" from actual running image digests.
6. Remediation replaces the dependency input, tests the new composition, and
   rolls native workloads to a new application digest.

The developer-facing delivery configuration can therefore name the application
and approved dependency set. The rendered Kubernetes workload still names
only the final application image and its explicit approved runtime ID.

An SBOM is an inventory, not executable content. The published dependency
artifact MUST contain the actual files; no package-manager resolution from
SBOM entries occurs in the shim, operator or generic composer.

## 2. Existing foundations and actual gaps

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
- Make CVE scanning and bidirectional dependency-to-application attribution
  first-class platform/operator workflows, not an exercise in manually joining
  registry manifests and Kubernetes output.
- Separate content integrity, publisher authorization, composition and
  application compatibility; none implies the others.
- Qualify Java, Python, Node.js and .NET with real independently published sets
  and compatible unchanged upstream runtime images.

### Non-goals

- A dependency reference that causes a node/shim to fetch another OCI artifact.
- A dependency inventory CRD, workload CRD, admission-time image rewrite or
  dependency restart controller inside the Brewlet operator.
- Maven/npm/pip/NuGet resolution or version-range interpretation in generic core.
- Hot replacement of libraries in running processes.
- Claiming binary/API compatibility from an SBOM, a version number or a signature.
- Restoring 1.x artifact formats or JVM-specific launch rewriting.
- A new generic producer plugin framework.
- Proving arbitrary application code contains no embedded third-party code.
  Strict build-content controls remain an optional external policy, not this
  feature's default guarantee.

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

## 6. Proposed producer interface

The following command **does not exist today**:

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

## 9. Operator dependency inventory, CVE scanning and remediation

### 9.1 Required operator questions

The initial feature MUST make these questions directly answerable:

- What packages and versions are in this published managed bundle, and which
  known CVEs apply to its verified SBOM?
- Which applications, namespaces and actual running containers contain package
  X, package/version X, bundle digest B or a finding for CVE Y?
- Which dependency sets, packages and CVE findings belong to application A?
- During a rollout, which old/new image digests still contain the affected
  dependency, and how many containers of each remain?
- What could not be verified or scanned, and why?

Here, "contains"/"uses" means the dependency is included in the verified
composition of the observed running image. It MUST NOT imply that the process
has loaded that file, that the vulnerable code is reachable/exploitable, or
that the app contains no other copies.

### 9.2 Scan once, attribute across applications

Platform users MUST be able to scan an approved published dependency set before
any application uses it. Operators MUST be able to scan a selected fleet without
separately learning registry traversal or stitching reports together manually.

Inventory and vulnerability analysis are distinct operations:

1. Capture the explicitly selected cluster/namespace/workload scope.
2. Record actual Pod UID, container ID and observed running application identity.
3. Verify the OCI graph, composition evidence and exact set/SBOM bindings.
4. Extract normalized package/file identities, versions, PURLs and provenance.
5. Scan each distinct verified SBOM document once per applicable scanner/DB
   snapshot, then join findings back to every observed application/container.

Do not assume every CRI `imageID` is the image manifest digest. Correlate
protected/observed identities and verified OCI membership rather than guessing
from a tag, desired Deployment spec or an unrelated config digest.

Workload attribution MUST follow controller owner references and UIDs, such as
Pod -> ReplicaSet -> Deployment or Pod -> Job -> CronJob; matching labels alone
are not ownership. Standalone Pods and unavailable owner chains remain explicit.
The application identity includes cluster, namespace, workload kind/name/UID,
not just a display name shared across namespaces.

Rolling workloads MUST retain separate old/new digest and dependency cohorts.
A single desired template or a newly published bundle cannot make old running
containers appear remediated.

### 9.3 Scanner boundary and freshness

Provide at least one documented, pinned scanner profile accepting the verified
CycloneDX SBOM and returning normalized findings. Reuse established scanner
tooling rather than implementing a vulnerability database or package resolver
inside Brewlet.
Generic core MUST NOT apply one naive version comparator to Maven, PEP 440,
npm and NuGet ranges. Findings must preserve ecosystem/PURL qualifiers.

A scan result MUST identify the SBOM document digest, scanner/version or binary
digest, scanner configuration/policy digest, advisory database identity and
timestamp, scan time and any matching limitations. Cache keys include these
identities; a newer database or changed policy invalidates a prior scan verdict.
Publisher/composer trust must be reevaluated under current policy separately
from reuse of cached vulnerability facts.

Missing evidence, inaccessible registries, invalid signatures, scanner failure,
ambiguous identity and disallowed staleness MUST be **unverified**, not a
successful empty scan. Preserve error reasons per artifact/workload.
Reports MUST distinguish known findings, no known findings under the stated
coverage/DB, unverified inputs and explicitly excluded/out-of-scope objects.
One status must not erase another: a workload can have known findings and
additional unverified coverage at the same time.

Fixture advisories drive deterministic tests; they are not a production
vulnerability database. No known findings is not proof that a package is safe.

### 9.4 Coverage must be visible

| Content | Required reporting boundary |
| --- | --- |
| Managed dependency set | Mandatory verified inventory and scan coverage for this feature. |
| Application-embedded/unmanaged content | Not exhaustively discovered by composition. Mark coverage unavailable unless a separately validated app SBOM/scanner supplies it. |
| Infrastructure runtime packages | Separate coverage. If included, correlate its SBOM with the actual leased inventory UID/generation/source digest, not the ID's current target. Missing runtime SBOM is unknown coverage. |

A patched managed bundle MUST NOT cause an entire application to be labeled
"CVE-free" or "clean." Keep managed, application and runtime coverage separate.
Including additional app/runtime SBOMs must not turn their absence into a zero
dependency count. The initial managed feature need not implement every
additional scanner path to report these boundaries accurately.

### 9.5 Proposed operator experience

These interfaces **do not exist today**; names/flags are proposed alongside the
composition interface. One operation should capture inventory, scan and join
the result, with report reuse for offline queries:

```sh
brewlet dependencies scan \
  --kubeconfig "$KUBECONFIG" --context production \
  --namespace payments --scanner scanner-profile.yaml \
  --output dependency-report.json

brewlet dependencies where-used --report dependency-report.json \
  --purl pkg:maven/org.apache.logging.log4j/log4j-core@2.14.1

brewlet dependencies where-used --report dependency-report.json \
  --cve CVE-2021-44228

brewlet dependencies list --report dependency-report.json \
  --workload Deployment/payments/orders
```

An equivalent scan input for a pinned published `--layer-set` reference MUST
work without a Kubernetes cluster. `where-used` MUST also support exact bundle
digest queries and unversioned package identity queries without conflating
distinct versions. CVE identifiers above are illustrative queries, not current
scan results.

Default human output MUST show package/version, CVE/severity when present,
namespace/workload, observed image digest/cohort, affected container count and
coverage warnings. Structured output MUST be schema-versioned and contain the
full evidence relationships for CI, fleet reporting and external dashboards.
Exit behavior MUST support explicit severity thresholds and fail-on-unverified
policy without discarding either findings or uncertainty from the report.

Snapshots MUST state collection start/end times, cluster/scope, omissions and
freshness. Multiple API reads are not an atomic cluster snapshot; changes
during capture must not silently attribute a replacement object to an old UID.

The tool is read-only: minimum Kubernetes inventory permissions plus explicitly
configured registry/scanner access. Never export registry credentials, pull
Secrets, kubeconfigs or signing keys in reports. Scanning MUST NOT modify
workloads, approve dependencies or request privileged node changes.

### 9.6 Remediation and verification

The release workflow publishes a replacement set, composes candidates, tests
them, updates native workload manifests and observes actual rollout.
Post-remediation inventory MUST show the new running application digests and
dependency identities, including any old/terminating containers still present.
It MUST NOT report success from a template patch alone.
No dependency-update loop is added to the node provisioner.

## 10. Implementation milestones

| Milestone | Deliverable | Exit criteria |
| --- | --- | --- |
| P1: contract, producer and operator experience | Review this contract; add concrete platform/application Maven projects and neutral publication examples; define inventory/scan/where-used report contracts with existing evidence fixtures. | Current mechanisms are documented/tested; operator questions, output coverage and failure states are specified before implementation. |
| P2: initial generic composition | App-payload plus one managed set, strict paths/platforms, byte-preserving output and deterministic result handoff. | No language inference; all malformed/collision/unknown-input cases fail; initial composition works locally. |
| P3: composition trust | Versioned predicate, signing, verification, key/identity policy and inspect output. | Subject/digest/order tampering and unsigned downgrade fail; signing failures publish no accepted output. |
| P4: dependency replacement | Accept verified prior compositions; replace the set while preserving app layers/launch unless explicitly changed. | Original build evidence stays intact; compatibility tests run against the new digest; no unverified layer stripping. |
| P5: fleet/remediation qualification | Read-only inventory, published-bundle/fleet scanning, bidirectional where-used queries, release/GitOps example and real-node matrix. | One scan joins shared dependencies to every actual application cohort; known findings/no-known-findings/unverified coverage, rollout and rollback/revocation behavior are proven. |

P3 depends on P2; P4 depends on P2/P3; P5's release scenario depends on P4.
Read-only inventory/scanning/where-used implementation can start from existing
layer-set evidence in parallel with composition work. Legacy evidence confidence
must remain distinct from the proposed verified final-composition provenance.
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
- one shared dependency/SBOM mapped to multiple applications and namespaces,
  with both package-to-workload and workload-to-package queries;
- bundle-only scans without a cluster, deduplicated SBOM scans across a fleet,
  and cache invalidation on advisory database/scanner/policy changes;
- mixed old/new rollout cohorts with accurate per-digest/container attribution,
  UID-safe owner-chain traversal, standalone Pods and partial RBAC access;
- scanner outages, stale DB/reports, missing SBOM/signatures, registry failures
  and unknown application/runtime coverage never producing a false clean result;
- package/version/PURL qualifiers, CVE queries, severity thresholds and
  fail-on-unverified behavior retained in both human and structured reports;
- zero dropped requests for the deliberately configured availability scenario,
  not a universal zero-downtime product guarantee.

Keep a requirements-to-tests checklist for these obligations. Publish exact
inputs, commands, results and limitations; preserve failed runs as failed.
The existing 111-requirement baseline does not qualify this proposed feature.

## 12. Documentation and release boundaries

Publish an end-to-end platform-to-application guide, generic composition
reference, trust/attestation reference, operator quickstart and remediation
runbook. The operator quickstart MUST demonstrate scan, where-used and
application-dependency listing without manual manifest/SBOM joins.
Examples must use explicit platforms, immutable references and readable
configuration, not an invented runtime dependency annotation.

Document which source/app formats the new composer accepts and which it
rejects. Archive existing receipts rather than rewriting them as evidence for
new semantics. Keep installation/source-revision compatibility explicit.

Approval of this proposal would authorize its bounded producer/release work,
not changes to Proposal 0009's rejected shim-pulled dependency design.

## References

- [Main managed bundle workflow at the inspected revision](https://github.com/microsoft/brewlet/blob/fa7085335b220e29df6affbece4558c5f1259ecb/docs/managed-dependency-bundles.md)
- [Main bundle validation](https://github.com/microsoft/brewlet/blob/fa7085335b220e29df6affbece4558c5f1259ecb/core/internal/artifact/dependency_bundle.go)
- [Main CVE remediation scenario](https://github.com/microsoft/brewlet/blob/fa7085335b220e29df6affbece4558c5f1259ecb/integration-tests/e2e/tier19-cve-remediation.sh)
- [2.0 Maven managed consumption](https://github.com/microsoft/brewlet/blob/7077c1d3b7c2019ad10b8d3160f60a15dac9c930/maven-plugin/README.md#managed-consumption)
- [2.0 layer-set implementation](https://github.com/microsoft/brewlet/blob/7077c1d3b7c2019ad10b8d3160f60a15dac9c930/pkg/artifact/layerset.go)
