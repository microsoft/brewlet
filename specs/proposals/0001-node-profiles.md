# Proposal 0001 — Node profiles: per-pool cluster preparation

- **Status:** implemented; retained as a design record
- **Specification sections:** **§5.6** (Node profiles), **§8.1**, **§8.3**, and
  **§14**.
- **Related code:** [`kubernetes/`](../../kubernetes):
  `api/nodeprofile/v1alpha1`, `internal/controller/nodeprofile_*.go`,
  `internal/admission/nodeprofile_webhook.go`, the `internal/brewlet` vocabulary,
  `charts/brewlet` (CRD, `nodeprofiles.yaml`, profile values, and validating
  webhook), `deploy/nodeprofile-crd.yaml`, and
  `deploy/sample-nodeprofile.yaml`; [`microsoft/brewlet`](https://github.com/microsoft/brewlet):
  `provisioner/entrypoint.sh`
- **Related proposals (split out of the original draft):**
  - [0002 — Validated containerd reconfiguration & readiness smoke gate](0002-validated-node-reconfig.md)
    (consumed here via `rollout.containerdRestart` / `rollout.validate`).
  - [0003 — Stable capability-label taxonomy for autoscaling](0003-capability-label-taxonomy.md).
  - [0004 — cert-manager integration for the admission webhook](0004-cert-manager-admission.md).

This document preserves the design rationale for the `NodeProfile` API. The
[specification](../SPECIFICATION.md) and shipped API are authoritative.

The implemented security contract requires an administrator-provided,
canonical, tagless SHA-256 digest reference and absolute path for every JDK and
launcher. Brewlet has no runtime-source catalog. Mirror destinations are
separately allowlisted by operator/admission configuration and revalidated by
the reconciler and privileged provisioner. This design record reflects that
fail-closed behavior.

---

## 1. Summary

Make preparing a Kubernetes cluster for Brewlet **configurable for heterogeneous
production fleets** by introducing a cluster-scoped **`NodeProfile`** CRD. A profile
binds a **node pool** to a **JDK/launcher inventory** (plus rollout policy and an
optional registry override). The operator reconciles **one provisioner DaemonSet per
profile**, so GPU pools, arm64 pools, and team-specific JDK sets are first-class
instead of a single global string.

The unit of configuration is the **node pool**, not the individual node. The user says
"provision Brewlet on the `batch` pool"; every node in that pool — including nodes the
autoscaler adds later — is provisioned automatically. Selecting a pool **is** the opt-in,
so there is no separate per-node `kubectl annotate` step. Cloud node pools are disjoint
(a node belongs to exactly one), which removes the per-node ownership/overlap machinery
an arbitrary node-selector model would need.

The one-command path remains: the chart's editable structured JDK and launcher
examples generate a **default profile** that provisions every node pool (the
bare-metal / no-pool fallback).

This proposal is deliberately scoped to the CRD and its reconcile semantics. The
host-hardening work it depends on (validated restart, smoke gate) lives in
[0002](0002-validated-node-reconfig.md); the autoscaling label contract in
[0003](0003-capability-label-taxonomy.md); the webhook cert change in
[0004](0004-cert-manager-admission.md). Each can ship independently.

## 2. Motivation — where the current model gets hard in production

Before `NodeProfile`, inventory was a single global value:
`provisioner.jdks` / `provisioner.launchers` (Helm) → one `buildDaemonSet` whose
`nodeAffinity` was just `brewlet.sh/provision In [true]`
(see [`resources.go`](../../kubernetes/internal/controller/resources.go),
[`node_controller.go`](../../kubernetes/internal/controller/node_controller.go)).

| Gap | Today | Why it hurts in production |
|---|---|---|
| **One global inventory** | `provisioner.jdks` is one comma list applied to every `brewlet.sh/provision=true` node (`Config.JDKs` → one DaemonSet). | Real fleets are heterogeneous (pool, team, cost). You can't say "the batch pool gets `temurin-21`, the edge pool gets `microsoft-25`". |
| **Coarse, per-node opt-in** | `kubectl annotate node --all brewlet.sh/provision=true`. | Manual, node-by-node, and untied to any inventory. Worse, it **breaks under autoscaling**: cluster-autoscaler / Karpenter nodes join un-annotated and get nothing until a human re-annotates them. |
| **No reversal** | Uninstall removes control-plane objects; shim + JDK roots + `config.toml` edits are left on the host (see [installation.md](https://github.com/microsoft/brewlet/blob/main/docs/installation.md) "Uninstall"). | Nodes accumulate drift; no clean way to de-provision without reimaging. |

The opt-in and autoscaling gaps have the same root cause: the unit of configuration is
the **individual node**. Cloud fleets are already organized into **node pools** (GKE
node pools, AKS agent pools, EKS managed node groups, Karpenter NodePools), and that is
the unit autoscalers add/remove capacity in. Targeting the pool instead of the node makes
opt-in declarative ("provision the `batch` pool") and autoscaler-safe (new pool members
are provisioned automatically).

> **Note on host mutation / readiness.** The original draft also listed "fragile host
> mutation" and "readiness ≠ validated" here. Those are accurate but **independent of
> the CRD** and are addressed in [0002](0002-validated-node-reconfig.md). (For the
> record: `entrypoint.sh` already runs `java -version` at install time and writes a
> `config.toml.brewlet.bak` backup — the gap is that nothing *restores* the backup on
> failure and the final `runtime=ready` label isn't gated on a re-run smoke test.)

Architecture is **not** a motivating gap for the inventory: the provisioner already
installs the JDK for the node's own arch automatically (`ctr` selects the matching
image platform), so a mixed amd64/arm64 fleet works today with one inventory. Profiles
add value where the *inventory itself* should differ per pool. Where a fleet organizes
arch into distinct pools (a common arm64 pattern), pinning a profile to that pool
already pins it to the arch.

## 3. How similar projects prepare nodes (prior art)

| Project | Node-prep mechanism | Idea we adopt |
|---|---|---|
| **SpinKube / Runtime Class Manager** | An operator-managed node installer handles shim lifecycle; a **`Shim` CRD** describes the containerd-shim-spin/runwasi integration to install. | CRD-per-capability, targeted by selector. |
| **SpinKube / Spin Operator** | `SpinAppExecutor` CR selects runtime. | RuntimeClass + selection as first-class CRs. |
| **Kata `kata-deploy`** | DaemonSet installs artifacts; targets nodes by label; ships a **`kata-cleanup` DaemonSet**. | Per-capability targeting + an explicit **cleanup** path for reversal. |
| **NVIDIA GPU Operator** | A single **`ClusterPolicy`** CR drives the whole config; **validator pods** gate readiness. | One CR = whole config surface; validation gate. |
| **gVisor on GKE / Bottlerocket / Talos** | Shim baked into the **node image**; pick a "sandbox node pool". | First-class **immutable-image path** — no privileged host mutation. |
| **Cluster Autoscaler / Karpenter** | Scale node groups from pre-baked images on capability labels. | Stable capability-label taxonomy ([0003](0003-capability-label-taxonomy.md)). |

The common thread: **a declarative object per capability, targeted by node selector,
with a reversal path.** `NodeProfile` closes the remaining gap in Brewlet.

## 4. Goals / non-goals

**Goals**
- Per-pool JDK/launcher inventories via a cluster-scoped CRD.
- **Node-pool-level targeting**: name a pool, provision every node in it (present and
  future) — autoscaler-safe, no per-node annotation.
- Keep the one-command install trivially simple (default profile from Helm values).
- Deterministic behaviour when a node matches no pool profile.
- A reversal (cleanup) path driven by a finalizer.
- A per-profile hook for the label-only / immutable-image path.

**Non-goals**
- **Sub-pool (individual-node) targeting.** Pool is the smallest unit by design; "these
  3 nodes of 20" is explicitly out of scope. Split the pool if you need that boundary.
- Changing the artifact format, the shim, or the `JavaApplication` CRD.
- Auto-discovering which JDKs a workload needs (still platform-team-declared).
- Replacing the capability-label / admission mechanism — admission reads per-node
  `brewlet.sh/jdks` annotations and needs **no change** to work across heterogeneous
  pools (§8.3). We extend, not replace it.
- The validated-restart/smoke-gate host hardening ([0002](0002-validated-node-reconfig.md)),
  the label taxonomy ([0003](0003-capability-label-taxonomy.md)), and cert-manager
  ([0004](0004-cert-manager-admission.md)) — referenced, not defined here.

## 5. Design

### 5.1 The `NodeProfile` CRD (new group `node.brewlet.sh/v1alpha1`, cluster-scoped)

```yaml
apiVersion: node.brewlet.sh/v1alpha1
kind: NodeProfile
metadata:
  name: general-pool
spec:
  # Which node pool(s) this profile provisions. Selecting a pool IS the opt-in:
  # every node in the pool (now and as the autoscaler grows it) is provisioned.
  nodePool:
    # One or more pool names. On a recognized cloud the label KEY is auto-detected
    # (see below); `key` overrides it for non-standard / bare-metal clusters.
    names: [general, general-spot]
    # key: cloud.google.com/gke-nodepool   # optional explicit override
  # Declarative inventory. Every runtime source is explicit and digest-pinned.
  jdks:
    - distribution: temurin
      feature: 21
      source:
        image: docker.io/library/eclipse-temurin@sha256:85f00967bcc624fc19fa9c2cf124ea426a5363898e267141726f31f358c2e14b
        javaHome: /opt/java/openjdk
    - distribution: zulu
      feature: 21
      source:
        image: docker.io/library/azul-zulu@sha256:2e230d906cffcc7bb7360ce82836f2ff0e0be74a1d5ebaf929e4e6ac99d61bf2
        javaHome: /usr/lib/jvm/zulu21
  launchers:
    - name: jaz
      source:
        image: mcr.microsoft.com/openjdk/jdk@sha256:bfde2ed613f4c67c112d1592452575d3a1dc9ce5f7d75821bb7752aa786fa575
        path: /usr/bin/jaz
  # Optional per-profile registry override for air-gapped / mirrored clusters.
  # NOTE: copy-from-image pulls via `ctr` against the host containerd, so mirroring
  # is expressed as a containerd registry mirror the provisioner configures for the
  # source image hosts — NOT a naive host-swap and NOT a Kubernetes imagePullSecret
  # (which ctr does not consume). See §5.10.
  registry:
    mirrors:
      "mcr.microsoft.com":   registry.internal.example.com/mcr
      "docker.io":           registry.internal.example.com/dockerhub
  # Rollout policy for the managed DaemonSet + host reconfig. The reconfig/validate
  # behaviour itself is defined in proposal 0002.
  rollout:
    maxUnavailable: 1
    validate: true                 # 0002 smoke gate before marking ready
    containerdRestart: validated   # validated | sighup | none  (0002)
status:
  observedGeneration: 3
  resolvedPoolKey: cloud.google.com/gke-nodepool  # key the operator matched on
  assignedNodes: 12                # nodes in the selected pool(s)
  readyNodes: 12
  conditions:
    - type: Ready
      status: "True"
      reason: AllNodesProvisioned
```

**Pool identification (provider portability).** A "node pool" is a provider-standard
node label. The operator auto-detects the key by probing known ones on the fleet:

| Provider | Pool label key |
|---|---|
| GKE | `cloud.google.com/gke-nodepool` |
| AKS | `kubernetes.azure.com/agentpool` (legacy `agentpool`) |
| EKS (managed node groups) | `eks.amazonaws.com/nodegroup` |
| Karpenter | `karpenter.sh/nodepool` |

For bare-metal / kubeadm clusters that have no pool concept, set `spec.nodePool.key`
explicitly (e.g. to a hand-applied `brewlet.sh/pool` label), or omit `nodePool`
entirely to mean "every node" (the default-profile fallback, §5.3).

**Semantics**
- **Cluster-scoped** (nodes are cluster-scoped; profiles describe them).
- A node is provisioned if and only if it belongs to one of the profile's `nodePool.names`
  (matched on the resolved pool key). **Pool membership is the opt-in** — there is no
  separate `brewlet.sh/provision=true` gate.
- Cloud node pools are **disjoint**: a node belongs to exactly one pool, so at most one
  profile owns a node by construction. Misconfiguration (two profiles naming the same
  pool) is rejected by the webhook (§5.8), not resolved by priority.
- `spec.registry.mirrors` configures digest-preserving copy-from-image rewrites.
  Every destination registry host must exactly match the external
  operator/admission allowlist (§5.10).
- Every JDK and launcher requires a source whose `source.image` is a fully
  qualified, tagless
  `repository@sha256:<64 lowercase hex>` reference.
- A JDK source may be a complete vendor JDK or a jlink runtime containing a
  platform-approved module set. The provisioner copies the source image's complete
  root filesystem and records `source.javaHome` plus the exact source digest;
  the shim uses that root as the sandbox userland and exposes the declared Java
  home at `/opt/jdk`.
- Shared jlink runtimes are node inventory, not application payloads. Applications
  continue to publish JARs and select the inventory token
  `<distribution>-<feature>`.

### 5.2 Operator reconcile: one DaemonSet per profile, selected by pool label

Because pools are disjoint, ownership needs no computation: each profile's DaemonSet
targets its pool label directly, and no node can match two of them. There is no
per-node assignment label and no priority tie-break — the earlier draft's
`brewlet.sh/profile` stamping existed only to make *overlapping arbitrary selectors*
safe, which the pool model removes.

```
NodeProfileReconciler (watches NodeProfile + Node):
  1. validateProfilePolicy(profile)              # sources, mirrors, rollout, pool conflicts
  2. ensureRuntimeClass()                        # cluster singleton, unchanged
  3. resolvePoolKey(profile)                     # auto-detect provider key or use spec override
  4. ds := buildProfileDaemonSet(cfg, profile)   # nodeAffinity =
        #   <poolKey> In [<names...>]
        #   (indexed JDK/launcher source env + mirrors + external allowlist)
     SetControllerReference(profile, ds)        # cluster owner, namespaced dep — OK
     CreateOrUpdate(ds)
  5. updateStatus(profile): assignedNodes / readyNodes / Ready|Degraded
     emit NodeUnmatched only if a named pool resolves to zero nodes

  validation failure:
     delete/withhold profile DaemonSet
     remove profile-owned node readiness/capability advertisements
     set Ready=False, reason=InvalidProfile, with an actionable message
```

```go
// api/v1alpha1 (node.brewlet.sh) — illustrative draft.
type NodeProfileSpec struct {
    NodePool  NodePoolRef   `json:"nodePool,omitempty"`
    JDKs      []JDKRef      `json:"jdks"`
    Launchers []LauncherRef `json:"launchers,omitempty"`
    Registry  *RegistrySpec `json:"registry,omitempty"`
    Rollout   RolloutSpec   `json:"rollout,omitempty"`
}

type NodePoolRef struct {
    // Names of the pool(s) this profile provisions. Empty means "every node"
    // (bare-metal / default-profile fallback).
    Names []string `json:"names,omitempty"`
    // Key is the node label carrying the pool name. Empty => operator auto-detects
    // the provider key (gke-nodepool / agentpool / nodegroup / karpenter).
    Key string `json:"key,omitempty"`
}

type JDKRef struct {
    Distribution string `json:"distribution"`
    Feature      int32  `json:"feature"`
    Source       JDKSource `json:"source"`
}

type JDKSource struct {
    Image    string `json:"image"`
    JavaHome string `json:"javaHome"`
}

type LauncherRef struct {
    Name   string         `json:"name"`
    Source LauncherSource `json:"source"`
}

type LauncherSource struct {
    Image string `json:"image"`
    Path  string `json:"path"`
}

type RegistrySpec struct {
    // Mirrors maps an upstream host to an externally allowlisted mirror
    // host/path while retaining the source digest. See §5.10.
    Mirrors map[string]string `json:"mirrors,omitempty"`
}

type RolloutSpec struct {
    MaxUnavailable    *intstr.IntOrString `json:"maxUnavailable,omitempty"`
    Validate          *bool               `json:"validate,omitempty"`          // default true (0002)
    ContainerdRestart string              `json:"containerdRestart,omitempty"` // validated|sighup|none (0002)
}
```

Key points:
- `buildProfileDaemonSet` is the existing `buildDaemonSet` generalized: the pod
  `nodeAffinity` becomes `<resolvedPoolKey> In [<names...>]`, and
  indexed `JDK_SOURCE_*`/`LAUNCHER_SOURCE_*` and mirror env come from the
  profile while `SOURCE_ALLOWED_MIRROR_HOSTS` comes from operator policy. The
  provisioner derives its inventory strings from those entries. Pool
  disjointness enforces single ownership — no assignment label required.
- Reconciliation shares source and mirror validation with admission and applies
  it before creating or updating the privileged DaemonSet. Invalid profiles fail
  closed with `Ready=False/InvalidProfile`; admission is early feedback, not the
  security boundary.
- The DaemonSet is **owned by the profile** (controller ref). A cluster-scoped owner
  with a namespaced dependent is a valid ownerReference, so deleting the profile GCs
  its DaemonSet — but see §5.7: host cleanup must run *before* that GC, via a finalizer.
- `NodeReconciler` (§8.1) is unchanged in spirit: it still reflects per-node
  `provision-state` and emits `NodeReady` / `ProvisionFailed`. Under 0002 it also reads
  `brewlet.sh/provision-error` so a validation failure surfaces as the node's (and the
  owning profile's) failure reason.

### 5.3 Helm bridge — the default profile

The chart renders a single **default `NodeProfile`** from the structured
`provisioner.jdks` / `provisioner.launchers` values:

```yaml
# templates/nodeprofiles.yaml
apiVersion: node.brewlet.sh/v1alpha1
kind: NodeProfile
metadata:
  name: default
spec:
  nodePool: {}                     # no names => every node (bare-metal fallback)
  jdks: {{ toYaml .Values.provisioner.jdks | nindent 4 }}
  launchers: {{ toYaml .Values.provisioner.launchers | nindent 4 }}
```

The one-command install retains the same targeting behavior: an empty `nodePool`
means "every node." The values are structured because every source and path is
mandatory; legacy comma-separated inventories are rejected. When a user later adds an
explicit pool-scoped profile, they narrow the default (or delete it) — because pools are
disjoint there is no priority to reason about; a node is either in a named pool or falls
through to the empty-pool default. The operator has no JDK or launcher inventory
flags; the CRD is authoritative.

### 5.4 Membership, the catch-all default, and unmatched pools

- **No selector overlap to resolve.** Cloud pools are disjoint, so two *named-pool*
  profiles cannot both claim a node. The webhook (§5.8) rejects two profiles naming the
  same pool, turning what was a runtime tie-break into an up-front validation error.
- **The one precedence rule — catch-all fallthrough.** The only profile that can
  co-select a node with a named-pool profile is the empty-`nodePool` **default**
  (which nominally means "every node"). We define the default as a *catch-all*: it owns
  exactly the nodes **not** claimed by any named-pool profile. This is binary
  ("named pool beats catch-all"), not a numeric priority, and it is the whole of the
  precedence logic. Concretely, the default DaemonSet's nodeAffinity gains a
  `NotIn [<all named pools>]` term on the resolved pool key.
- **Reassignment:** if a node moves pools (rare — usually means the node was recycled),
  or a profile's `nodePool.names` changes, the operator runs cleanup for the *old*
  inventory before applying the new one (§5.7), so the node never accumulates roots from
  a pool that no longer owns it.
- **Unmatched / empty pool:** a `nodePool.name` that resolves to zero nodes (typo, pool
  not yet created, wrong provider key) is surfaced with a `NodeUnmatched` warning event
  and a `Degraded`/`reason: EmptyPool` condition, so a misnamed pool is diagnosable
  rather than silently doing nothing. Bare-metal installs that keep the empty-pool
  default never hit this.

### 5.5 Failure feedback: operator ↔ provisioner

The provisioner self-labels the node; the operator can't observe a bash-level failure
directly. Per [0002](0002-validated-node-reconfig.md) the provisioner writes
`brewlet.sh/provision-error=<reason>` on the node (and withholds `runtime=ready`) on any
reconfig/validation failure. `NodeProfileReconciler` reads it when computing
`readyNodes` and flips the owning profile's `Ready` condition to `Degraded` with that
reason, so a bad rollout is visible at `kubectl get nodeprofile`.

### 5.6 Label-only / immutable-image mode

For regulated / immutable-OS clusters, a profile with `rollout.containerdRestart: none`
(defined in [0002](0002-validated-node-reconfig.md)) runs a **label-only** provisioner:
it skips host mutation entirely (shim + JDK roots + `config.toml` are baked into the
node image), runs the smoke gate, and only advertises capability labels +
`runtime=ready`. The mechanics of baking the runtime into a Talos/Bottlerocket/AMI node
image are a how-to for the docs, not part of this design; the CRD contribution is
simply that the mode is selectable per profile.

### 5.7 Reversal — cleanup via a finalizer

Owner-ref GC alone would delete a profile's DaemonSet the instant the profile is
deleted, leaving nothing to clean host state (and racing the cleanup). So `NodeProfile`
carries a **finalizer** (`node.brewlet.sh/cleanup`):

1. On profile deletion (or pool reassignment), the operator launches a short-lived
   **`brewlet-cleanup`** DaemonSet scoped to the affected nodes (selected on the profile's
   resolved pool label). Following the kata-cleanup pattern it: removes the
   `runtimes.brewlet` block from `config.toml` (restoring the backup), restarts
   containerd, removes the shim from `/usr/local/bin`, optionally prunes
   `/opt/brewlet/jdks/*` not referenced by another profile, and removes the
   `brewlet.sh/runtime` + capability labels.
2. Only once cleanup completes does the operator remove the finalizer, letting owner-ref
   GC drop the managed DaemonSet.

This makes `helm uninstall` + `kubectl delete nodeprofile --all` fully reverse host
state for valid profiles, closing the "uninstall leaves drift" gap. An invalid
profile is the exception: its current selector may overlap another profile and
is not trusted for privileged cleanup. Deletion stops the profile's provisioner
and cleanup pods, waits for them to terminate, withdraws advertisements again,
and removes the finalizer without launching cleanup; repair the profile before
deleting it when automatic reversal is required.

### 5.8 Validating webhook for `NodeProfile`

A small validating admission webhook rejects malformed profiles early: empty
`jdks`, invalid distribution or launcher identifiers, any JDK/launcher missing
its source, any image not using a canonical SHA-256 digest reference, malformed
or non-absolute source paths,
malformed mirror mappings, destinations outside
`--allowed-source-mirror-hosts`, invalid `containerdRestart` values, and **two
profiles naming the same pool** (ambiguous ownership — the pool-model
replacement for the old priority tie-break). It also flags a `nodePool.key` that matches no
known provider on a cluster where auto-detection found a different one. The
reconciler applies the same policy, so the webhook provides immediate feedback
without becoming the security boundary.

### 5.9 Status & events

- **Status:** `observedGeneration`, `resolvedPoolKey`, `assignedNodes`, `readyNodes`, and
  a `conditions` list with `Ready` (`AllNodesProvisioned`) /
  `InvalidProfile` (source, mirror, rollout, or pool-conflict policy) /
  `Degraded` (empty pool or provisioning/reconfig failure — reason propagated
  from `brewlet.sh/provision-error`).
- **Events (extend §14):** `NodeUnmatched` (a named pool resolved to zero nodes),
  plus the existing `Provisioning` / `NodeReady` / `ProvisionFailed`. The reconfig/
  validation events (`ContainerdReconfigFailed`, `ValidationFailed`) are owned by
  [0002](0002-validated-node-reconfig.md).

### 5.10 Registry override for air-gapped copy-from-image

Copy-from-image pulls explicitly digest-pinned images via `ctr` against the
**host containerd**. A Kubernetes `imagePullSecret` does not
apply because host `ctr` does not read pod credentials.
`spec.registry.mirrors` maps each upstream host to a mirror repository prefix,
but the destination registry host must exactly match the separately configured
operator/admission allowlist. The operator passes that same allowlist to the
provisioner as `SOURCE_ALLOWED_MIRROR_HOSTS`.

Admission, reconciliation, and the provisioner reject schemes, whitespace,
empty or malformed hosts, duplicate mappings, self-mappings, and unapproved
destinations. Rewriting retains the original `@sha256:` suffix, so the mirror
must preserve the exact OCI manifest/index bytes and digest. Auth to the mirror
is a node/containerd concern (host `hosts.toml` credentials), not data carried
in the CRD.

## 6. Helm surface (progressive disclosure)

```yaml
# Simple: one structured inventory for every node -> default NodeProfile.
provisioner:
  jdks:
    - distribution: temurin
      feature: 21
      source:
        image: docker.io/library/eclipse-temurin@sha256:85f00967bcc624fc19fa9c2cf124ea426a5363898e267141726f31f358c2e14b
        javaHome: /opt/java/openjdk
  launchers:
    - name: jaz
      source:
        image: mcr.microsoft.com/openjdk/jdk@sha256:bfde2ed613f4c67c112d1592452575d3a1dc9ce5f7d75821bb7752aa786fa575
        path: /usr/bin/jaz

# Mirror authority is operator-level, not profile-level.
security:
  allowedSourceMirrorHosts:
    - registry.internal

# Advanced: per-pool profiles. Pool label key is auto-detected per provider;
# override with `key` on bare-metal / non-standard clusters.
profiles:
  - name: general
    pools: [general]
    jdks:
      - distribution: temurin
        feature: 21
        source:
          image: docker.io/library/eclipse-temurin@sha256:85f00967bcc624fc19fa9c2cf124ea426a5363898e267141726f31f358c2e14b
          javaHome: /opt/java/openjdk
  - name: edge-arm
    pools: [edge-arm64]
    jdks:
      - distribution: microsoft
        feature: 25
        source:
          image: mcr.microsoft.com/openjdk/jdk@sha256:bfde2ed613f4c67c112d1592452575d3a1dc9ce5f7d75821bb7752aa786fa575
          javaHome: /usr/lib/jvm/msopenjdk-25
    registry: { mirrors: { "mcr.microsoft.com": "registry.internal/mcr" } }
```

## 7. RBAC & CRD lifecycle

- **CRD:** add the `nodeprofiles.node.brewlet.sh` CRD to `charts/brewlet/crds/` and
  [`kubernetes/deploy/`](../../kubernetes/deploy/).
  **Helm does not upgrade CRDs placed in `crds/`** — the rollout plan
  must document applying CRD updates out-of-band (or moving CRDs to a templated,
  `helm.sh/hook: crd-install`-style path) so `helm upgrade` picks up schema changes.
- **Operator RBAC:** add `get/list/watch/update/patch` on `nodeprofiles` (+ `/status`,
  `/finalizers`), `get/list/watch` on Nodes (to read pool labels and readiness — no node
  writes are needed since the model no longer stamps a per-node assignment label), and
  `create/update/delete` on DaemonSets — the operator now manages **N** DaemonSets, not one.
- **Validating webhook RBAC/config:** the §5.8 webhook configuration + its serving cert
  (self-signed today, or via [0004](0004-cert-manager-admission.md)).

## 8. Test plan

The implementation is end-to-end-test driven. Additions:

- **Unit (operator):** mandatory digest-only JDK and launcher sources,
  exact-host mirror allowlists, provider pool-key auto-detection
  (GKE/AKS/EKS/Karpenter + explicit-key override), catch-all default `NotIn`
  exclusion of named pools, empty-pool `NodeUnmatched` handling, per-profile
  DaemonSet nodeAffinity/env, fail-closed invalid-profile cleanup, finalizer
  add/remove ordering, and status/condition transitions.
- **Envtest:** `NodeProfileReconciler` against a fake fleet with labeled pools — create a
  named-pool profile plus the catch-all default, assert each node is owned by exactly one
  DaemonSet (named pool excluded from the default); delete a profile and assert the
  finalizer blocks GC until cleanup completes.
- **Webhook unit:** rejects missing sources, mutable/malformed digest
  refs, unauthorized mirrors, empty jdks, two profiles naming the same pool, and
  unknown pool keys; accepts valid arbitrary JDK and launcher names.
- **Provisioner/source policy:** verifies strict digest/path validation,
  missing/duplicate source failures, strict mirror rewrite with digest preservation,
  preflight-before-host-operation, stale-root ordering, safe launcher
  extraction, and stale-label removal.
- **e2e tier:** a new tier proving two pool profiles materialize different inventories on
  their pools, that a node the autoscaler adds to a pool is provisioned without manual
  annotation, and that `kubectl delete nodeprofile` + cleanup reverses host state
  (extends the existing provisioning tiers).

## 9. Risks & mitigations

- **Two ways to configure (values vs. profiles).** The default-profile bridge
  preserves the simple installation path while profiles serve heterogeneous
  fleets.
- **Ownership ambiguity.** Removed structurally: pools are disjoint, so a node belongs to
  exactly one named pool; the catch-all default is excluded from named pools via a
  `NotIn` term (§5.4). Duplicate pool names are rejected by the webhook, not raced at
  runtime.
- **Provider portability of the pool key.** Auto-detect known keys (GKE/AKS/EKS/
  Karpenter) with an explicit `spec.nodePool.key` escape hatch for bare-metal/kubeadm,
  where an empty `nodePool` still means "every node."
- **Cleanup vs. owner-ref GC race.** Finalizer runs host cleanup before GC (§5.7).
- **Mutable host runtime content.** Every source requires a reviewed digest pin
  and absolute path; mirror destinations are externally allowlisted and enforced
  again by reconciliation and the provisioner.
- **CRD adds API surface.** Justified: it is the mechanism NVIDIA GPU Operator /
  runtime-class-manager use for the same heterogeneous-fleet problem.

## Appendix — spec integration points

- **§5.6 (new):** "Node profiles" — the CRD, node-pool targeting, disjoint-pool single
  ownership + catch-all default, empty-pool handling, finalizer cleanup, label-only
  mode, air-gap registry mirrors, and the validating webhook.
- **§8.1:** add `NodeProfileReconciler` alongside `NodeReconciler`; note the DaemonSet
  is now per-profile, owned by the profile, and selected via the resolved node-pool label.
- **§14:** add the `NodeUnmatched` event (reconfig/validation events are owned by 0002).
