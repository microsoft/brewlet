# Brewlet main vs. Brewlet 2.0: capabilities and equivalent workflows

**Status: discussion draft.** This document compares implementations, not just
proposal terminology. Suggested extensions are explicitly distinguished from
capabilities that exist today. It does not request or implement those extensions.

## Scope and source revisions

| Version | Revision used for this comparison |
| --- | --- |
| Current `main` | [`fa7085335b220e29df6affbece4558c5f1259ecb`](https://github.com/microsoft/brewlet/tree/fa7085335b220e29df6affbece4558c5f1259ecb) |
| Published Brewlet 2.0 | [`7969990b03acca6bbc4c766f5437fe790f92d712`](https://github.com/microsoft/brewlet/tree/7969990b03acca6bbc4c766f5437fe790f92d712) |

Here, **main** means the Java-oriented implementation at the first revision,
not a promise about future default-branch behavior. **2.0** means the clean-slate
implementation governed by [Proposal 0009](../specs/proposals/0009-language-agnostic-runtime-images.md).
The preserved 1.x specification and documentation are reference snapshots;
they do not imply compatibility in 2.0.

## Executive comparison

Both versions implement the same underlying separation: developers publish
application bytes, platform operators govern runtime bytes, and the node
composes them for execution through containerd and runc.

Main specializes this model for Java. Brewlet 2.0 makes the artifact, runtime
inventory, provisioning and execution contracts language-neutral.

**Broader language support does not make 2.0 a feature superset of main.**
Several main features are already expressible through generic 2.0 primitives.
Others need external tooling or permitted producer work. Some are deliberately
excluded from core and should not be restored under a different name.

## 1. Capabilities both versions provide

The rows below compare shared outcomes. They do not claim identical formats,
APIs, implementation maturity or validation coverage.

| Shared capability | Main implementation | Brewlet 2.0 implementation | Meaningful difference |
| --- | --- | --- | --- |
| Separate application and runtime delivery | Java artifact independent of a node-installed JDK image. | Application artifact independent of an approved runtime image. | The separation applies to Java, Python, Node.js, .NET and other explicitly described processes. |
| Platform-controlled runtime sources | Digest-pinned JDK image and `javaHome` declared in a `NodeProfile`. | Digest-pinned `RuntimeImage` approved independently; profiles reference its ID. | Approval is separated from pool placement. |
| Complete runtime userland on the node | Copies the approved JDK image's complete root filesystem, not only its Java directory. | Copies and validates the approved runtime image root. | A full runtime root was already present in main; 2.0 generalizes its contract. |
| Pool-scoped provisioning | `NodeProfile` selects pools, inventory, explicit tolerations and rollout policy. | `NodeProfile` retains pool selection and policy, with per-ID Eager/OnDemand installation. | Sources are no longer repeated inside the placement resource. |
| Standard kubelet image delivery | Runnable OCI application images are pulled through CRI. | Version-2 runnable OCI images are pulled through CRI. | Native 1.x artifact delivery is not a supported 2.0 application format. |
| Digest-bound application execution | Protected CRI requested-image/config identity and verified content select the application. | Retains protected CRI identity checks and strict native-platform/content validation. | Pod hints are not execution authority in either design. |
| Multi-platform publication | Runnable image indexes, explicit architecture options and Java-native-library scanning. | Explicit platform manifests/indexes and strict platform selection. | Automatic application-platform inference is removed. |
| Kubernetes runtime integration | One Brewlet RuntimeClass and runc-backed containerd shim. | The same architectural integration, with generic composition. | The shim executable's `-v2` suffix refers to the containerd interface, not the Brewlet product version. |
| Existing Kubernetes workload controllers | Supports direct Brewlet Pods/workloads in addition to `JavaApplication`. | Uses ordinary Deployments, StatefulSets, DaemonSets and Pods. | Native Kubernetes workloads were not invented by 2.0; they become the only workload API. |
| Application/runtime composition | Shared JDK root as overlay lower, with Java payload and dependency mounts. | Application/dependency layers over a shared runtime lower, with per-container scratch. | Composition is no longer expressed through JAR, classpath or Java-home concepts. |
| Packaged application content protection | Read-only mounts for application JARs and dependency payloads. | Read-only packaged `/app`, with validated confinement of all payload entries. | 2.0 defines one uniform filesystem prefix for every ecosystem. |
| Writable application storage | Kubernetes volumes and container scratch. | Kubernetes volumes and containerd-managed upper/work directories. | Packaged layers remain immutable; volume mounts beneath `/app` are explicit exceptions. |
| Resources, networking, logs and signals | Delegates normal task behavior to runc and preserves CRI resources/CNI namespaces. | Retains runc delegation and the CRI infrastructure while applying generic process policy. | It is not a new sandbox or stronger isolation technology. |
| Java classpath/module execution | Core interprets classpath/module launch metadata. | Maven producer or author writes ordinary Java argv referencing `/app/lib` or `/app/mods`. | Java interpretation moves to the producer. |
| Independent dependency governance | Java dependency bundles with BOM/lock/SBOM/provenance contracts. | Generic layer sets; Maven retains Java graph/BOM verification outside core. | Byte-for-byte reuse remains, but identities and evidence formats differ. |
| Runtime updates without rebuilding application bytes | Replaces the approved JDK source while retaining old mounted roots. | Rotates the digest behind a runtime ID; new starts use current while existing leases retain their generation. | 2.0 adds explicit epoch/generation and restart-barrier semantics. |
| Safe ownership and cleanup | UID-bound claims, durable target/retirement records and conditional release. | Retains those principles and adds per-inventory obligations, leases and revocation receipts. | Main already has substantial safety machinery; the difference is not safe vs. unsafe. |
| Conservative reclamation | Retired-root mount checks and guarded runnable-stage GC. | Durable task/Exec/mount evidence, retirement grace and persistent layer/content caches. | 2.0 makes more of the lifetime relationship explicit and recoverable. |
| Validated host activation | Validates containerd configuration/handler and supports rollback. | Durable activation journal, effective/live checks and exact reversal. | `none` is not permission to bypass mandatory validation. |
| Operational visibility | JDK inventory, provisioner state, shim observations and application readiness. | Runtime/Pod binding metrics, generation/rotation status, restart outcomes and installation metrics. | Signals become generic; the complete main CLI presentation layer is not retained. |
| Automated verification | Unit, integration, tiered and live Java/platform coverage. | Shared producer/shim conformance, four-language fixtures, native contracts and real-node companions. | Neither test counts nor language counts alone establish superiority. |

### Important non-difference: automatic JVM tuning

Current main explicitly avoids injecting heap, GC or CPU tuning automatically.
Its core still constructs Java argv from application metadata and
deployment-supplied `jvm.args`. Brewlet 2.0 removes that Java-specific
construction and delivery mechanism; it is not removing an existing
automatic GC optimizer.

## 2. Main features without a direct 2.0 equivalent

**Available now** means the outcome can be obtained with existing 2.0
primitives, possibly using ordinary Kubernetes or build tools. **Extension**
means additional work is needed and is not shipped by the compared revision.
**Excluded** means the original behavior conflicts with the 2.0 contract.

| Main feature or convenience | Status in 2.0 | How to achieve the outcome |
| --- | --- | --- |
| `JavaApplication` generates Deployment/Service/HPA. | CRD/controller excluded; outcome available now. | Maintain ordinary Kubernetes resources, optionally rendered by separately maintained Helm/Kustomize templates. |
| Maven `manifest` goal generates a development workload. | Absent; excluded from the 2.0 Maven plugin. | A separate deployment stage combines the published application digest with reviewed workload YAML. |
| JDK feature/distribution matching and defaults. | Excluded. | Explicit platform-owned IDs encode the desired compatibility promise; workloads select exactly one. |
| `javaHome` and `/opt/jdk` normalization. | Excluded from core. | The golden image supplies its own stable executable paths and Env; authors invoke `java` through runtime-owned PATH or an explicit absolute path. |
| Structured Java launch knobs and `jvm.args` annotation. | Generic outcome available now. | Put complete explicit argv in `launch.json` or normal Kubernetes command/args overrides; Maven `javaArgs` can generate Java argv. |
| First-class AppCDS archive support. | Generic execution available now; dedicated producer convenience absent. | Publish the archive as an ordinary app file and explicit flags; preserve training paths and mtimes. A Maven-specific attachment convenience is permitted. |
| CLI infers architecture by scanning native libraries in a JAR. | Excluded from generic packaging. | Build/publish explicit platform groups and review matching Kubernetes architecture constraints. |
| Broader automatic Java entry-mode discovery. | Partially retained in Maven, not generic CLI. | Current plugin uses explicit configuration or JAR `Main-Class`; other permitted Java-project analysis could be added only in that producer. |
| `brewlet run` executes locally outside Kubernetes. | Command absent. | Use the isolated local kind path for actual Brewlet execution; direct runtime-image execution is only a separate development check. |
| `brewlet bundle` exports a standalone runc bundle. | Command absent. | Current execution goes through the shim. A new generic execution/export design would need its own authority and lifetime contract. |
| `brewlet k8s` inventory/status/doctor/app-wait helpers. | Commands absent; many observations available now. | Use Kubernetes/Helm/metrics directly, or build a separate generic operations client. |
| `brewlet stage-gc` manual command. | Command absent; reclamation outcome available now. | Use the fenced provisioner's normal GC with configured grace/sweep/idle policy, not ad hoc filesystem deletion. |
| Native 1.x artifact format and old launch schema. | Unsupported; no compatibility path. | Repackage original application inputs as a version-2 runnable image. |
| Old final-application attestation/Ratify policy integration. | Not equivalent to current layer-set signing. | Add an external final-artifact attestation/policy workflow bound to the published v2 digest; integration needs new validation. |
| `brewlet keygen` convenience. | Command absent. | Generate supported signing keys through ordinary key tooling and pass the explicit key path to layer-set publication. |
| Broad registry-login/developer transport conveniences. | Not complete CLI parity. | Current CLI uses explicit credentials/CA settings; kubelet/runtime-source credentials are configured independently. Evaluate any extra integration separately. |
| Public installer, release packaging and full website. | Not carried forward as a complete product surface. | Build a versioned 2.0 release/docs pipeline around the new commands, chart and producer, without republishing old contracts as current. |

## 3. Analysis of equivalent workflows

### 3.1 Replace generated workloads, not the Kubernetes controllers

The value of `JavaApplication` on main is a compact application-oriented API:
artifact, replicas, resources, JVM settings, probes, ports, Service and HPA.

In 2.0, represent those fields in the resources Kubernetes already owns:

| Main field or result | Native 2.0 representation |
| --- | --- |
| `artifact.image` | Container `image`, pinned to the published v2 application digest. |
| `replicas` and resources | Deployment replicas and container requests/limits. |
| JVM version/distribution | Reviewed `brewlet.sh/runtime-image` annotation. |
| JVM arguments | Explicit process argv; no Java-specific annotation. |
| Environment and probes | Ordinary container Env/probe fields. |
| Service and ports | Ordinary Service/container port declarations. |
| Autoscaling | Ordinary HPA referencing the Deployment. |
| Architecture constraints | Standard `kubernetes.io/arch` selectors or affinity. |

A separately maintained chart can restore the concise developer experience
without introducing a replacement workload CRD. That chart is deployment
tooling, not the generic packaging CLI or Maven plugin.

The deployment stage must consume the actual published application digest,
not reconstruct a tag from the project version. Runtime selection remains a
reviewed deployment decision, not an inference from the JAR or launch allowlist.
The existing 2.0 CLI exposes `--push-result` for a machine-readable publication
handoff; its shape is not the old main `push.json` contract.

**Acceptance for additional templates:** render native resources, preserve
user security/affinity/probe settings, bind the exact image digest, and never
infer runtime IDs or grant startup-taint tolerations to application Pods.

### 3.2 Express Java behavior as producer-owned argv

A Java application does not lose the ability to use JPMS, preview features,
system properties, agents or GC flags merely because core no longer models
them.

For example, an author-written descriptor can contain:

```json
{
  "schemaVersion": 2,
  "process": {
    "entrypoint": ["java"],
    "args": [
      "--enable-preview",
      "--add-opens", "java.base/java.lang=ALL-UNNAMED",
      "-Dexample.mode=production",
      "-Xmx512m",
      "-jar", "/app/orders.jar"
    ],
    "workingDir": "/app"
  },
  "requires": {
    "runtimeImages": ["java-21"]
  }
}
```

These arguments belong to the application/deployment author; Brewlet neither
derives nor tunes them. The selected runtime must support them.

Kubernetes `args` replaces the image argument array; it does not append to it.
A template exposing Java tuning must therefore render a complete coherent argv,
including the intended entrypoint arguments. This preserves normal Kubernetes
semantics rather than recreating the main shim's flag-merging rules.

The current Maven producer already supports classpath/module generation,
explicit Java arguments, runtime-ID allowlists and authoritative hand-written
descriptors. Its generated classpath uses `/app/app.jar:/app/lib/*`; module
generation uses `/app/mods`.

**Permitted extension:** producer-side warnings or explicit compiler-release
to runtime-ID mappings, as allowed by Proposal 0009 section 11.2. Such a mapping
may propose an artifact allowlist, not select the workload's actual runtime.
Do not add Java discovery to the generic CLI or shim.

### 3.3 Re-express AppCDS with ordinary files

Main understands a dedicated CDS descriptor and archive payload. It constructs
the sharing flags and prepares the JAR timestamps needed by HotSpot.

The current 2.0 generic path can package:

```text
app/orders.jar
app/orders.jsa
```

with explicit launch arguments:

```text
java -Xshare:auto -XX:SharedArchiveFile=/app/orders.jsa -jar /app/orders.jar
```

This is expressly permitted by Proposal 0009 section 11. The archive is an
ordinary app-layer file, not a new core media type or interpreted hint.

The producer/image pipeline must own archive training, compatible JDK selection,
stable classpath/working-directory layout and source file mtimes. The generic
packager preserves valid modes/mtimes; it does not repair an incompatible
archive or train one.

Runtime rotation may invalidate a prebuilt archive's optimization. Use
best-effort `-Xshare:auto`, and measure both archive use and fallback. A valid
application start alone does not prove the optimization was used.

**Permitted extension:** a separately tested Maven attachment/generation
convenience. **Not proposed:** a Java-aware shim or a guarantee that an archive
survives every runtime digest rotation.

### 3.4 Use explicit runtime IDs instead of matching

Main's feature-only request can select an installed distribution; an exact
distribution request has a stricter match. Recreating that algorithm in 2.0
would undermine explicit runtime authority.

Instead, the platform owns meaningful IDs, for example:

| Desired policy | Example ID policy |
| --- | --- |
| Centrally selected Java 21 userland | `java-21`, with its source controlled by platform administrators. |
| A specific approved distribution | `temurin-21`, explicitly selected by the workload. |
| Hold a validated runtime source | A dedicated administrator-owned ID whose source is intentionally held. |
| Change ABI, libc, runtime line or executable layout incompatibly | A new ID rather than an incompatible rotation. |

The ID is a compatibility promise, not language-version parsing performed by
Brewlet. `spec.compatibility` is informational and does not implement matching.
An artifact allowlist can reject unsuitable IDs but cannot select one.

A runtime digest in a workload annotation is not the substitute. If a workload
requires direct per-workload base-image digest control outside this model, use
an ordinary complete image with the stock runtime.

### 3.5 Preserve managed dependencies without duplicating Maven in core

The existing 2.0 Maven path already provides the principal managed-bundle
outcome: resolve a source BOM, verify exact runtime membership and scoped graph,
compare JAR paths/hashes against a verified layer set, then publish the thin
application with the dependency layers reused unchanged.

The generic layer-set lock and evidence replace the old Java-specific wire
contract. Repackaging requires new manifests/evidence; changing a media-type
string is not sufficient.

Current limitations matter:

- Managed consumption supports one complete Java graph per application.
- Managed consumption rejects nested-JAR applications; it does not unpack or
  rewrite a Spring Boot fat JAR.
- Passing a hand-written launch descriptor does not bypass graph or thin-JAR
  validation.

For an executable fat JAR, use the generic app-layer path and explicit
`java -jar /app/app.jar` launch, treating its bundled dependencies as
application-owned. For independent dependency governance, produce a genuinely
thin application using the existing Maven/layer-set workflow.

### 3.6 Replace operations conveniences with observations first

The absence of `brewlet k8s doctor` does not remove the underlying state, but
`kubectl` commands are not automatically a semantic replacement for that
diagnostic tool.

| Main operation | Existing 2.0 building blocks | Additional UX work if desired |
| --- | --- | --- |
| JDK inventory list | `RuntimeImage` objects, NodeProfile offerings, node runtime inventory and metrics. | Present desired approval separately from installed/current state. |
| Profile inspect/status | Profile targets/conditions and node capabilities/fences. | Join original Node UID, profile UID/generation and per-ID state. |
| Application wait | Native rollout status and actual Pod conditions. | Combine with generation barriers and runtime bindings when waiting for remediation, not just availability. |
| Doctor | CRDs, webhook/controller status, profile policy, effective node readiness and authorization evidence. | A separately maintained generic diagnostic client with explicit error reporting. |
| Inventory/profile edits | Reviewed `kubectl`, Helm or GitOps updates. | Preserve source-of-truth ownership, dry-run semantics and optimistic concurrency. |
| Installation/uninstall | Current Helm chart and cleanup guards. | Provide reviewed wrappers that never force finalizers, claims or fences. |

Useful read-only starting points include:

```sh
kubectl get runtimeimages.node.brewlet.sh
kubectl get nodeprofiles.node.brewlet.sh
kubectl get nodes -L brewlet.sh/runtime
kubectl rollout status deployment/orders --namespace apps
```

The final command establishes native rollout availability, not by itself that
every old-generation runtime task disappeared. For that question, inspect
runtime bindings, restart outcomes and
`brewlet_container_runtime_image_info`.

Any new operations client should consume these public contracts, not read a
JDK directory and recreate removed language matching.

### 3.7 Local run and bundle export need different treatment

For **actual local Brewlet execution today**, use an isolated kind environment,
the current chart/inventory and explicit v2 application descriptors. This
exercises the real shim, authorization, filesystem and Kubernetes overrides.

For **quick application debugging**, invoking the intended program in the
approved runtime image can help. It is not equivalent to Brewlet execution:
it does not prove artifact validation, leases, runtime authorization,
first-use installation or scheduling.

There is no current generic `brewlet bundle` exporter. A future standalone
execution/export design would need an explicit approval source and validated
artifact/runtime/platform selection, plus independent payload ownership or a
lease lifetime protocol. Exporting paths into an evictable live cache would
create a dangling-lifetime hazard.

This is additional design work, not an implicit license to bypass the existing
node authority checks or to restore Java-aware local launching.

### 3.8 Restore application supply-chain policy outside the shim

Layer-set signing in 2.0 does **not** establish parity with every main
final-application provenance and Ratify/Gatekeeper workflow. Runtime source
approval is also not a cryptographic signature-verification policy by itself.

A compatible external pipeline could:

1. Publish a validated v2 application image and obtain its exact digest.
2. Publish an SBOM/attestation whose subject is that image digest.
3. Bind reused layer-set identities and relevant build evidence.
4. Configure a separately maintained verifier/policy service for v2 artifacts.
5. Test tampered/missing/conflicting evidence and actual admission denial.

Keep such registry verification separate from Brewlet's Pod mutator, which
does not fetch artifacts to infer language, platform or runtime selection.
Do not describe current layer-set trust flags as final-application signing.

An implementation must also distinguish packaging failures from policy
fail-open/fail-closed choices, and preserve registry credential-origin boundaries.
This integration is proposed work, not verified functionality in this draft.

### 3.9 Let the provisioner own reclamation

Main's manual stage-GC command addresses its runnable-stage implementation.
2.0 uses a different persistent layer/content cache and explicit task leases.
Operationally, configure the existing provisioner grace/sweep policy and
OnDemand idle TTL instead of porting the old command against the new layout.

Never emulate manual GC by deleting `/opt/brewlet/runtimes` or
`/var/lib/brewlet/layers` entries. An empty Pod list does not prove the absence
of Exec processes, mounts, pending Creates or unresolved cleanup obligations.

If a manual maintenance interface is later needed, it must invoke the same
exclusive guards, fresh authority and complete evidence rules as automatic
collection. A dry-run report is a safer first addition than another writer.

### 3.10 Restore distribution and documentation separately

Main's installer, release workflow, Maven Central workflow and website solve
product-distribution needs, not runtime-composition needs. They can be rebuilt
for 2.0 without changing its architecture, but they are not already restored.

A 2.0 release pipeline should version and publish matching CLI, shim,
controller, worker, CRDs, chart and Maven producer; verify checksums/provenance;
and test fresh installation, upgrade policy and guarded uninstall.
Successful source builds do not establish those public-release workflows.

The preserved [main Pages workflow](../specs/reference/site-pages.yml) is the
publishing reference: it validates site examples, optionally checks public
release artifacts, builds MkDocs strictly and assembles the static site.
Its requirements must be reconciled with the new tree before enabling Pages.
This comparison does not enable a site or release deployment.

## 4. Boundaries that should not be restored in 2.0 core

| Requested behavior | Why it conflicts | Contract-preserving alternative |
| --- | --- | --- |
| Guess a language, entrypoint, dependency layout or platform. | Generic producers must package explicit inputs. | Author/build pipeline supplies descriptors, layer groups and platforms. |
| Infer or default the pod runtime from artifact contents. | Workload runtime selection must be explicit and inventory-controlled. | Reviewed runtime ID in the workload manifest. |
| Add a language-neutral replacement workload CRD. | Proposal 0009 explicitly retains native workload APIs instead. | External templates rendering ordinary resources. |
| Inject JVM/interpreter tuning in the shim. | Execution must be independent of language semantics. | Explicit argv or the platform's runtime image behavior. |
| Give separate containers different runtime IDs in one Brewlet Pod. | Runtime selection is pod-wide. | A platform-composed multi-runtime image, or separate Pods. |
| Pass an ordinary application sidecar through the Brewlet shim. | All application containers must satisfy the v2 artifact contract. | Package that container as v2, or place an ordinary image in a separate stock-runtime Pod. |
| Permit root or privilege escalation as a compatibility setting. | Non-root identity and no privileges are mandatory. | A separately governed ordinary workload using the stock runtime when appropriate. |
| Accept old artifact formats as a fallback. | 2.0 is a breaking contract, not a compatibility mode. | Rebuild/repackage original sources and reviewed deployment definitions. |
| Reclaim uncertain or still-referenced state to unblock cleanup. | Missing evidence cannot authorize deletion. | Restore observation/ownership, drain normally or use the explicit retirement-evidence process. |

An external tool is not a loophole: its output must still satisfy the same
artifact, identity, authorization and execution boundaries.

## 5. Suggested prioritization

| Priority | Candidate | Reason |
| --- | --- | --- |
| First | Native deployment templates and a digest-based publication handoff. | Restores common developer convenience without new execution semantics. |
| First | Generic read-only inventory/status/doctor presentation. | Makes the existing lifecycle model operable without adding another host writer. |
| First | Versioned release/install/docs pipeline. | Makes the implemented system consumable; independent of language support. |
| Next | AppCDS producer convenience and permitted Java requirement warnings. | Restores Java ergonomics while keeping runtime execution generic. |
| Next | External v2 final-application attestation/policy integration. | Restores governance workflows, with explicit additional validation. |
| Separate design | Generic standalone execution/bundle export. | Must resolve authorization and cache/root lifetime rather than copy the old launcher. |
| Do not pursue in core | Inferred runtimes, language tuning, workload CRD replacement, root or old-format fallback. | These reverse the defining choices of Proposal 0009. |

These are recommendations, not an implementation commitment or a statement
that optional producer capabilities are mandatory acceptance gaps.

## Source map

| Topic | Main source | Brewlet 2.0 source |
| --- | --- | --- |
| Java vs. generic launch | [JVMConfig](https://github.com/microsoft/brewlet/blob/fa7085335b220e29df6affbece4558c5f1259ecb/core/internal/artifact/artifact.go), [Java argv](https://github.com/microsoft/brewlet/blob/fa7085335b220e29df6affbece4558c5f1259ecb/core/internal/runtime/launch.go) | [Launch validation](../pkg/artifact/launch.go), [process policy](../pkg/node/process.go) |
| Inventory and workloads | [NodeProfile](https://github.com/microsoft/brewlet/blob/fa7085335b220e29df6affbece4558c5f1259ecb/kubernetes/api/nodeprofile/v1alpha1/nodeprofile_types.go), [JavaApplication](https://github.com/microsoft/brewlet/blob/fa7085335b220e29df6affbece4558c5f1259ecb/kubernetes/api/v1alpha1/javaapplication_types.go) | [Inventory APIs](../kubernetes/api/v2alpha1/types.go), [admission](../kubernetes/internal/admission/pod.go) |
| CLI surface | [CLI](https://github.com/microsoft/brewlet/blob/fa7085335b220e29df6affbece4558c5f1259ecb/core/cmd/brewlet/main.go), [Kubernetes operations](https://github.com/microsoft/brewlet/blob/fa7085335b220e29df6affbece4558c5f1259ecb/core/internal/kube/cli.go) | [Generic CLI](../cmd/brewlet/main.go) |
| Producer | [Maven plugin](https://github.com/microsoft/brewlet/blob/fa7085335b220e29df6affbece4558c5f1259ecb/maven-plugin/README.md) | [Maven producer and limitations](../maven-plugin/README.md) |
| Node execution/lifetime | [Shim](https://github.com/microsoft/brewlet/blob/fa7085335b220e29df6affbece4558c5f1259ecb/core/shim/cmd/containerd-shim-brewlet-v2/service_linux.go), [provisioner](https://github.com/microsoft/brewlet/blob/fa7085335b220e29df6affbece4558c5f1259ecb/provisioner/entrypoint.sh) | [Shim](../shim/cmd/containerd-shim-brewlet-v2/service_linux.go), [node contracts](../pkg/node/types.go), [provisioner](../provisioner/README.md) |
| Scope and evidence | [Main overview](https://github.com/microsoft/brewlet/blob/fa7085335b220e29df6affbece4558c5f1259ecb/README.md) | [Proposal 0009](../specs/proposals/0009-language-agnostic-runtime-images.md), [scoped validation record](../acceptance/RESULTS.md) |
