# JDK management

The JDK is the part Brewlet moves **off** application images and **onto** nodes.
The platform team chooses every JDK build, pins it by digest, and declares where
its Java home lives inside the source image.

Related: [Configuration](configuration.md) ·
[Capability labels and autoscaling](capability-labels-and-autoscaling.md) ·
[Launchers](launchers.md) · [Security](security.md).

---

## Source model

Brewlet has no built-in JDK catalog and does not map distribution names to
images. Every `spec.jdks[]` entry requires:

- a stable lowercase `distribution` name used in the node inventory (a DNS-1123
  label, at most 48 characters);
- a positive Java `feature` version;
- a fully qualified, tagless
  `source.image` in `registry/repository@sha256:<64 lowercase hex>` form; and
- a clean absolute `source.javaHome` inside that image.

The provisioner validates all entries before installing the shim or accessing
host containerd. It then pulls the image through host containerd, mounts its
root filesystem, and copies that complete userland to:

```text
/opt/brewlet/jdks/<distribution>-<feature>/
```

The root records the selected Java home in `.brewlet-java-home` and the exact
image/path pair in `.brewlet-source`. The shim uses the complete copied root as
the sandbox filesystem and exposes the selected Java home at `/opt/jdk`.

After installing the roots, the provisioner writes the set of active
distributions to `/opt/brewlet/jdks/.brewlet-active`. Selection is fail-closed on
that inventory: a root that is present on disk but absent from the inventory —
and any request made on a node with no inventory at all — is refused with
`NoCompatibleJDK`. A requested `distribution` must also be a lowercase DNS-1123
token, and the shim verifies after symlink resolution that the selected root is a
direct child of `/opt/brewlet/jdks` before mounting it. Launchers follow the
identical rules (see [Launchers](launchers.md)).

Mutable tags are intentionally rejected. A tag may resolve to different bytes
between reviews or nodes; a digest identifies the exact OCI manifest or
multi-platform index the administrator approved.

---

## Helm examples: Temurin and Microsoft

The chart ships editable examples for Eclipse Temurin 21 and Microsoft Build of
OpenJDK 25:

```yaml
provisioner:
  jdks:
    - distribution: temurin
      feature: 21
      source:
        image: docker.io/library/eclipse-temurin@sha256:85f00967bcc624fc19fa9c2cf124ea426a5363898e267141726f31f358c2e14b
        javaHome: /opt/java/openjdk
    - distribution: microsoft
      feature: 25
      source:
        image: mcr.microsoft.com/openjdk/jdk@sha256:bfde2ed613f4c67c112d1592452575d3a1dc9ce5f7d75821bb7752aa786fa575
        javaHome: /usr/lib/jvm/msopenjdk-25
```

These are ordinary chart values, not privileged Brewlet defaults. Review and
replace the digests according to your patch policy before production rollout.

For a direct `NodeProfile`, use the same shape:

```yaml
apiVersion: node.brewlet.sh/v1alpha1
kind: NodeProfile
metadata:
  name: java-platform
spec:
  nodePool:
    names: ["java-workers"]
  jdks:
    - distribution: temurin
      feature: 21
      source:
        image: docker.io/library/eclipse-temurin@sha256:85f00967bcc624fc19fa9c2cf124ea426a5363898e267141726f31f358c2e14b
        javaHome: /opt/java/openjdk
  rollout:
    validate: true
    containerdRestart: validated
```

---

## Multiple JDKs of the same feature version

A node pool can have multiple JDK 21 installations **on each node**, not just
different feature versions. Each installation is identified by its
`<distribution>-<feature>` inventory token. For example, `temurin-21` and
`microsoft-21` can coexist.

The `distribution` field is an administrator-defined inventory name, not a fixed
vendor enum. To install two builds of the same vendor's JDK 21, give them distinct
names such as `temurin-stable` and `temurin-canary`. Two entries with the same
`distribution` and `feature` are rejected, even if their source digests differ.

Configure both entries in the pool's `NodeProfile.spec.jdks`. This example uses
two Temurin builds; replace each digest placeholder with the reviewed
64-character lowercase SHA-256 digest for that build before applying it:

```yaml
apiVersion: node.brewlet.sh/v1alpha1
kind: NodeProfile
metadata:
  name: java-platform
spec:
  nodePool:
    names: ["java-workers"]
  jdks:
    - distribution: temurin-stable
      feature: 21
      source:
        image: docker.io/library/eclipse-temurin@sha256:<stable-64-lowercase-hex>
        javaHome: /opt/java/openjdk
    - distribution: temurin-canary
      feature: 21
      source:
        image: docker.io/library/eclipse-temurin@sha256:<canary-64-lowercase-hex>
        javaHome: /opt/java/openjdk
  rollout:
    validate: true
    containerdRestart: validated
```

Add these entries to the profile that owns the pool, rather than creating
overlapping profiles. For Helm's default profile, the equivalent names go in
`provisioner.jdks[].distribution` in your values file. The resulting roots are
`/opt/brewlet/jdks/temurin-stable-21/` and
`/opt/brewlet/jdks/temurin-canary-21/`.

### Selecting an installation from an application

In the application's `JavaApplication` descriptor, set both the feature version
and the **same inventory name**:

```yaml
spec:
  jvm:
    version: 21
    distribution: temurin-canary
```

This selects `temurin-canary-21`; change `distribution` to `temurin-stable` to
select the other installation. For a raw `Deployment`, the equivalent Pod
template annotation is `brewlet.sh/jdk: "temurin-canary-21"`.

An explicit distribution request does not fall back to another distribution.
Admission rejects a request with no compatible ready node as `NoCompatibleJDK`,
and the shim independently refuses an unavailable or inactive installation.

`spec.jvm.version` is the integer **feature version**, not a patch version.
There is no separate patch/build or source-digest selector in `spec.jvm`. The
selected inventory entry's `source.image` controls the exact build. Updating its
digest replaces that installation for future launches; use distinct inventory
names when builds must coexist. See [Patching & upgrading JDKs](#patching-upgrading-jdks).

### When distribution is omitted

With only `spec.jvm.version: 21`, any active, compatible JDK 21 is acceptable.
After scheduling, the shim selects the **lexicographically first matching
inventory name on that node**, not the newest patch or a preferred vendor.
The same rule applies to a bare `brewlet.sh/jdk: "21"` annotation; with no JDK
request, the shim defaults to feature 21 and uses that rule.

| Active, compatible installations on the node | Selected for version 21 alone |
|---|---|
| `microsoft-21`, `temurin-21` | `microsoft-21` |
| `temurin-canary-21`, `temurin-stable-21` | `temurin-canary-21` |

**`stable` and `canary` have no special meaning to Brewlet.** Adding a
lexicographically earlier name can change the JDK selected by newly launched or
restarted workloads that omit `distribution`; already-running JVMs are not
switched. Replicas on nodes with different inventories can select different
distributions.

Pin both `version` and `distribution` for predictable selection, and keep the
source digest consistent across the targeted nodes when the exact build matters.
See [Deploying workloads](deploying-workloads.md#jdk-selection).

---

## Arbitrary JDKs

Any image-packaged OpenJDK distribution works when the selected path contains
`bin/java` and the image includes the native loader and libraries it needs. For
example, Azul Zulu 21:

```yaml
spec:
  jdks:
    - distribution: zulu
      feature: 21
      source:
        image: docker.io/library/azul-zulu@sha256:2e230d906cffcc7bb7360ce82836f2ff0e0be74a1d5ebaf929e4e6ac99d61bf2
        javaHome: /usr/lib/jvm/zulu21
```

### Shared jlink runtimes

The same contract supports a platform-owned jlink runtime:

```dockerfile
FROM eclipse-temurin:21-jdk AS build
COPY platform-modules/ /platform-modules/
RUN jlink \
    --module-path "$JAVA_HOME/jmods:/platform-modules" \
    --add-modules java.base,java.logging,java.net.http,com.example.platform \
    --strip-debug --no-man-pages --no-header-files --compress=zip-6 \
    --output /runtime

FROM debian:bookworm-slim
COPY --from=build /runtime /opt/java/runtime
```

Publish each required architecture under one reviewed multi-platform digest:

```yaml
spec:
  jdks:
    - distribution: platform
      feature: 21
      source:
        image: registry.example.com/java/platform-runtime@sha256:<64-lowercase-hex>
        javaHome: /opt/java/runtime
```

The distribution name is an inventory identifier, not a registry lookup key.
There are no `lts`, `latest`, or vendor aliases.

---

## Selecting and reviewing a digest

Resolve the digest with your registry tooling and review its platform coverage.
For example:

```bash
docker buildx imagetools inspect docker.io/library/eclipse-temurin:21-jdk
docker buildx imagetools inspect mcr.microsoft.com/openjdk/jdk:25
```

Use the top-level multi-platform index digest when the selected node pool mixes
architectures. Otherwise, pin the reviewed platform manifest digest. Confirm
that every selected variant exposes the same `source.javaHome`.

Before rollout, verify:

1. the reference is fully qualified and contains no tag;
2. the digest is SHA-256 lowercase hex;
3. every target architecture is present;
4. `<javaHome>/bin/java` exists and is executable; and
5. your organization accepts the distribution's license and update policy.

---

## Architecture mapping (multi-arch)

The provisioner runs on the target node and lets containerd select that node's
platform from the pinned OCI image or image index:

| Node (`uname -m`) | OCI architecture |
|---|---|
| `x86_64` / `amd64` | `amd64` |
| `aarch64` / `arm64` | `arm64` |

Use one reviewed multi-platform index digest for mixed-architecture pools, or
separate profiles with platform-manifest digests when architecture-specific
approval is required. The application JAR remains architecture-neutral.

---

## Registry mirrors and air-gapped clusters

Mirror destinations are operator policy, separate from each `NodeProfile`.
Allowlist exact destination hosts, including explicit ports:

```yaml
security:
  allowedSourceMirrorHosts:
    - registry.internal.example.com

provisioner:
  registry:
    mirrors:
      docker.io: registry.internal.example.com/dockerhub
      mcr.microsoft.com: registry.internal.example.com/mcr
```

Admission, reconciliation, and the privileged provisioner all reject malformed,
duplicate, self-referential, or unapproved mappings. A rewrite changes only the
registry/repository prefix and preserves the original `@sha256:` digest, so the
mirror must expose identical manifest or index bytes under that digest.
Credentials remain a node/containerd concern and are not stored in
`NodeProfile`.

---

## Inspecting the JDKs available on the cluster

With `spec.rollout.validate=true`, each copied JDK must contain an executable
`<javaHome>/bin/java`, and `java -version` must succeed inside the staged root
before Brewlet publishes readiness.

Nodes advertise the coarse inventory:

```bash
kubectl get node node-1 \
  -o jsonpath='{.metadata.annotations.brewlet\.sh/jdks}{"\n"}'
# temurin-21,microsoft-25
```

They also publish `brewlet.sh/jdks-info`, populated from the installed JDK:

```json
[
  {"distribution":"temurin","vendor":"Eclipse Adoptium","feature":21,"version":"21.0.5","arch":"amd64"},
  {"distribution":"microsoft","vendor":"Microsoft","feature":25,"version":"25","arch":"amd64"}
]
```

Use `brewlet jdks`, `brewlet jdks --output wide`, or
`brewlet jdks --output json` to aggregate this data across the fleet.

---

## Patching & upgrading JDKs

To patch a JDK within the same feature version:

1. review the new image or multi-platform index;
2. replace `source.image` with its new digest;
3. apply the Helm values or `NodeProfile`; and
4. restart workloads after nodes report ready.

The changed `.brewlet-source` causes an atomic replacement of the shared root.
Running pods retain their existing mount until they restart.

For a feature upgrade, declare both roots during migration:

```yaml
provisioner:
  jdks:
    - distribution: temurin
      feature: 17
      source:
        image: docker.io/library/eclipse-temurin@sha256:<temurin-17-digest>
        javaHome: /opt/java/openjdk
    - distribution: temurin
      feature: 21
      source:
        image: docker.io/library/eclipse-temurin@sha256:<temurin-21-digest>
        javaHome: /opt/java/openjdk
```

Move workloads to feature 21, then remove the feature-17 entry after nothing
requests it.

### Node ownership and retargeting

Each managed node is reserved to one profile UID and one node UID before
provisioning can start. Runtime readiness labels are separate from this
ownership: removing readiness does not release a node to another profile.
There may be one manually authored catch-all, excluding named-profile targets.
Named profiles must use disjoint pool names and the same key configuration:
either the same explicit key everywhere or auto-detection everywhere.

Changing a selector, pool label, or node role triggers retirement of previously
recorded targets. All provisioner and node-metrics workers for the profile pause
while the operator cleans departing nodes and waits for their cleanup workers
to terminate. Retained nodes' JDK/launcher roots and capability advertisements
are preserved. Only then are claims released and the latest desired targets
provisioned. Repeated edits or a controller restart do not cancel a frozen
retirement episode.

The status ledger records node identities and per-node containerd cleanup
policy. A later label-only `containerdRestart: none` rollout does not erase an
earlier obligation to reverse Brewlet's config changes on that node; newly added
`none` nodes keep their independently managed configuration.

### Provisioning completion and deletion

Provisioner pods stay NotReady until installation, containerd activation, and
readiness publication succeed. The readiness probe checks a container-local
completion marker, not just whether the script is running. This keeps rolling
updates from advancing past nodes whose provisioning is still in progress.

Deleting a valid NodeProfile first stops its provisioner and waits for those pods to
terminate, then starts cleanup. The finalizer remains while cleanup is pending
or failing. After successful completion on all recorded targets, the operator
remembers completion and tears down cleanup; the finalizer remains until that
DaemonSet and its pods are gone. This also prevents a replacement profile from
provisioning alongside an old cleanup container. Stale DaemonSet readiness from
an earlier template does not count.

Move or drain affected workloads before deleting a profile: completion ordering
prevents that profile's provisioner and cleanup from racing, but it does not
migrate workloads away from the runtimes being removed. Keep the operator running
until profile deletion completes. Apply the same workload precautions before
retargeting or changing node labels/roles.

Invalid deleting profiles with possible host state retain both claims and
finalizers with `CleanupBlocked`. Repair the spec/source policy or pool conflict
to resume cleanup; a missing ledger must be restored, not interpreted as an
empty installation. Unavailable nodes and replaced node UIDs also block cleanup.
Never clear ownership labels, status, or finalizers as a workaround.
Missing/reused UIDs before proven cleanup have no supported in-place recovery.
Coordinate [node decommissioning and autoscaler scale-in](capability-labels-and-autoscaling.md#scale-in-consolidation-and-replacement)
before removing a Node or VM.

For a fresh Helm installation, the chart installs the CRDs; no prior upgrade or
legacy migration is needed. Only when updating an existing installation must
the NodeProfile CRD be upgraded before the operator/provisioner pair. Legacy
migration can recover surviving worker/advertisement evidence, but cannot prove
the history of vanished hosts. It inventories pending-node affinity and old-pod
cleanup policy, even when the current DaemonSet template has changed. Temporary
`node.brewlet.sh/migration` scheduling gates prevent replacement workers from
racing that inventory; do not remove them manually. Unverified legacy evidence
remains blocked rather than being adopted by profile name.
Inspect `kubectl get nodeprofile <name> -o yaml`
and operator logs for `OwnershipMigration`, `OwnershipConflict`, `Retargeting`,
or `CleanupBlocked`. For control-plane removal, follow
[the gated uninstall procedure](installation.md#uninstall).

---

## Known limitations

- One source entry must use the same Java-home path on every architecture in its
  selected multi-platform image.
- The copied image must contain the operating-system loader and native libraries
  required by `java`.
- The default validated activation expects a `containerd` systemd service. Use
  `sighup` only for the legacy reload path, or `none` when another system owns
  runtime registration.
