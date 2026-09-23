# Proposal 0007 — Baked golden-image delivery

- **Status:** proposed; not implemented. Recorded as an alternative delivery mode,
  explicitly **not** a replacement for the node-resident JDK model
- **Related roadmap item:** baked golden-image delivery (workload delivery)
- **Current delivery path:** runnable images ([SPECIFICATION §4.4](../SPECIFICATION.md#44-runnable-image-delivery-mode-kubelet-pullable-the-spinkube-style-pull-path)),
  node JDK roots ([§5.3](../SPECIFICATION.md#53-installing-jdk-runtime-roots-on-nodes)),
  and the Runtime v2 shim ([§6](../SPECIFICATION.md#6-the-containerd-shim-containerd-shim-brewlet-v2))
- **Related proposals:** [0001 — node profiles](0001-node-profiles.md),
  [0006 — sandbox isolation tiers](0006-sandbox-isolation-tiers.md)

This design records an alternative in which an operator-approved **golden image**
(a minimal userland plus a JDK) is combined with the developer's application
layers into an ordinary, runc-runnable OCI image — removing the need for
node-installed JDK roots and the Brewlet containerd shim on the execution path.

It does not change shipped behavior, so [`SPECIFICATION.md`](../SPECIFICATION.md)
remains unchanged.

---

## 1. Summary

Brewlet today separates the application from the runtime at **launch time**: the
developer publishes a runnable image containing only their JAR and a launch
descriptor (§4.4), the provisioner installs approved JDK roots on each node
(§5.3), and the shim overlays the two at `Create()` (§6.1).

The alternative evaluated here separates them at **build time** instead:

1. Ops maintains an allow-list of digest-pinned **golden images** — the same
   governance surface `NodeProfile` provides for JDK sources today, expressed as
   full base images rather than JDK roots.
2. A **baker** composes an approved golden image with the application's layers,
   deriving the image config entrypoint from the §4.2 launch config.
3. The result is a plain OCI image that runs under the stock `runc` RuntimeClass.

The developer experience is unchanged — still no Dockerfile, still `brewlet push`
of a JAR. What changes is where composition happens and who signs the result.

## 2. Motivation

Three recurring constraints motivate the question:

- **Clusters that cannot take a custom shim.** Installing
  `containerd-shim-brewlet-v2` and editing containerd configuration requires a
  privileged DaemonSet and node-pool ownership (§11). Some managed or regulated
  environments will not permit it at all.
- **Stronger isolation tiers.** [Proposal 0006](0006-sandbox-isolation-tiers.md)
  defers Kata precisely because a per-pod VM's guest kernel and page cache
  conflict with node-shared JDK bytes, and gates gVisor on proving the shim
  composes with `runsc`. A plain image has neither problem.
- **Ecosystem tooling.** Ordinary images get scanners, SBOM tooling, cosign/SLSA
  attestation, registry mirrors, CRI image GC, and ephemeral debug containers
  (unsupported by the Brewlet handler today, §6.3) without Brewlet-specific work.

## 3. Goals

- Record the design, its tradeoffs, and the conditions under which it is the
  better choice.
- Preserve zero-Dockerfile delivery (**G1**) and canonical invocation (**G2**).
- Keep the golden-image inventory platform-owned and digest-pinned, matching the
  governance properties of §5.3.
- Identify the smallest change that captures most of the benefit without giving
  up supply-chain provenance.

## 4. Non-goals

- Replacing the node-resident JDK model. **G5** (shared, patchable JVM) is the
  project's differentiator; this proposal does not retire it.
- Node-local image synthesis that produces digests no registry can serve.
- Shipping Brewlet-authored golden base images. The inventory is operator-supplied,
  exactly as JDK sources are today.
- Reimplementing a general-purpose image builder. Jib, Paketo, and kpack already
  exist.

## 5. Design sketch

### 5.1 Approved inventory

Golden images are declared the way JDK sources are (§5.3): distribution, feature
version, and a digest-pinned `image:` reference. The additional field a golden
image needs is the `javaHome` inside it, which §5.3 already records.

An approved entry is a *base*, not a runtime root: it is expected to contain a
minimal read-only userland plus the JDK, and nothing application-specific.

### 5.2 Composition

The baker produces a manifest whose layers are the golden image's layers followed
by the application's `app` / `classpath` / `modulepath` layers — which
`brewlet push --format=image` already emits as standard `tar+gzip` with
`brewlet.sh/layer` role annotations (§4.4). The image config entrypoint is derived
from the §4.2 launch config, so `java -jar`, `-cp`, and `-p -m` forms all map
directly.

**Appending, not flattening, is a hard requirement.** Shared golden layers stay
deduplicated in the snapshotter and the JDK's mapped pages stay shared across
JVMs on the node. A squashed image loses both — which is most of the reason the
node-resident model exists.

### 5.3 Where the bake runs

Two placements are possible, and they are not equivalent:

| | CI-side bake | Node-side bake |
|---|---|---|
| Resulting digest | published, pullable, reproducible | exists only in one node's content store |
| Signer | the existing publishing identity | a new privileged node daemon |
| Portability | normal | breaks re-pull, image GC, and `kubectl describe` |
| Trusted computing base | unchanged | grows |

Node-side baking is rejected for those reasons. The rest of this proposal assumes
a CI-side bake.

### 5.4 Admission

Admission validates that the image records the golden base digest it was built
from — as an annotation covered by the publisher's signature — and that the digest
appears in the operator's allow-list. This replaces the `NoCompatibleJDK` /
`NoCompatibleLauncher` node-capability checks (§14) with a single policy check,
since runtime presence is no longer a per-node property.

## 6. Tradeoff analysis

### 6.1 What improves

- **The largest maintenance burden disappears.** The shim is the source of the
  containerd 2.0+ floor, cgroup-v2-only gating, protected-CRI metadata
  verification, overlay `lowerdir` assembly, and packed-layer retention
  requirements (§4.4). None of it is needed on the baked path.
- **Isolation tiers become tractable.** Both gates in
  [proposal 0006](0006-sandbox-isolation-tiers.md) are about the shim and
  node-shared bytes; a plain image under `runsc` or Kata has neither constraint.
- **Privileged surface shrinks to zero on the execution path.** No host mutation,
  no JDK root rotation, no `.retired.<epoch>.<pid>` renames, no grace-period
  reclamation, no finalizer-driven host cleanup (§12).
- **Standard tooling applies unmodified**, including the ordinary-image admission
  policy the roadmap lists as future work.

### 6.2 What regresses

- **Centralized JDK patching is lost in its strongest form.** Today a JDK CVE is
  remediated by installing a new root and rolling workloads; application artifacts
  are untouched (§11). On the baked path every derived image must be rebuilt —
  applications × golden bases × architectures — before anything can roll. **G5**
  weakens from "independent of the artifact" to "independent of the developer's
  source build".
- **Image combinatorics.** The shim resolves JDK distribution, launcher, and CDS
  late and per node. Baking freezes them, so the matrix becomes one image per
  (golden base, architecture, JDK feature/distribution, launcher, CDS) tuple.
- **Cold start on a fresh node regresses.** The provisioner pre-warms JDK roots
  today; a baked image means the first pod on a node pulls the full base. A
  pre-pull DaemonSet recovers this — which is node provisioning under another name.
- **Node-side AppCDS regeneration has no obvious home.** The per-`(namespace,
  verified platform manifest, JDK build, process UID)` cache that self-heals on
  every central JDK patch (§13) cannot be reproduced at bake time without running
  the JVM, and reproducing it at runtime reintroduces a node component.
- **Launch-phase observability moves.** `overlay_setup`, `runc_create`, artifact
  resolution behavior, and CDS decisions (§12) are shim-emitted and would need a
  different source.
- **Overlap with existing tools.** "Approved base plus app layer, composed
  automatically in CI" is what Jib, Paketo, and kpack already do, with mature
  signing. A baked-only Brewlet would have to justify itself against them without
  the node-resident runtime as a differentiator.

### 6.3 Conclusion

Worse as a replacement; potentially useful as an **optional delivery mode**.

The node-resident JDK is what makes Brewlet's fleet economics work: one JDK copy
per node, and CVE remediation that never touches application artifacts. Baking
trades that for operational simplicity and ecosystem compatibility. That trade is
correct for clusters that cannot accept a shim or privileged provisioning, or that
require a VM-isolated sandbox — and incorrect for the fleet-scale case Brewlet
targets.

## 7. Recommended increment

Rather than a redesign, the tractable step builds on `--format=image`, which
already produces standard layers and carries the launch contract:

1. Add an opt-in `--base=<approved-golden-digest>` to the publishing path that
   appends the app layers onto the approved base and sets the entrypoint from the
   launch config.
2. Record the base digest in a manifest annotation covered by the publisher's
   signature.
3. Accept the result on the stock `runc` RuntimeClass, with admission validating
   the recorded base digest against the operator allow-list.
4. Leave the shim path as the default, so workloads that benefit from centralized
   patching keep it.

This keeps provenance in CI, needs no new privileged component, and lets both
paths coexist.

## 8. Open questions

- How is the golden-image allow-list expressed — an extension of `NodeProfile`, or
  a cluster-scoped resource, given runtime presence is no longer node-specific?
- Can a bake be made bit-for-bit reproducible from (base digest, app layers, launch
  config) so the output is independently verifiable?
- What is the supported remediation workflow for a golden-image CVE, and can it be
  driven without a rebake stampede across every derived image?
- Is there an acceptable AppCDS story that does not reintroduce a node component?
- Does a baked workload still report enough for Brewlet's observable contract
  (§14.1), or does that contract become mode-specific?

## 9. Alternatives considered

### Node-side bake

Rejected in §5.3: it produces unservable digests and grows the trusted computing
base with a privileged, signing node daemon.

### Flattened single-layer images

Simplest to produce and the worst fit. Losing layer sharing forfeits both the disk
dedup and the shared JDK page cache that motivate the node-resident model.

### Defer entirely to buildpacks

A legitimate answer for users who want baked images today: kpack or Paketo with an
approved base is mature and needs nothing from Brewlet. The increment in §7 is
worth doing only because Brewlet already owns the launch contract and the app-layer
format, so the marginal cost is small and the result stays consistent with the
shim path.
