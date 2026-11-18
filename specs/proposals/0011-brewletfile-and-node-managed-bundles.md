# Proposal 0011: Brewletfile and node-managed dependency bundles

- **Status:** proposed; not approved.
- **Scope:** Brewlet 1.0.0 producers (CLI and Maven plugin), inventory, node
  provisioning, admission, shim and observability.
- **Amends:** §3, §5, §6.3, §6.5, §7, §8.1, §8.2, §9, §11, §12, §13 and §15
  of the [specification](../SPECIFICATION.md).
- **Removes:** Part II (§16) and its
  [composition ledger](../../acceptance/composition-requirements.json).
- **Does not change:** per-container runtime selection (§8.1), runtime
  rotation, ordinary OCI execution of unselected containers, the application
  layer rules (§6.1, §6.2), the image-config mirror (§6.4), user identity
  (§8.4), startup fencing (§9.2) or runc execution.
- **Reference main:** `29159f9d9354578a1dc0d3a4a6722f2f138d11ac`.

`§N` references cite the current specification. This proposal's own sections
are cited as "section N".

## 1. Summary

This proposal makes two changes that fit together:

1. **Brewletfile.** Developers describe an application artifact in a small,
   Dockerfile-like text file instead of hand-writing `launch.json` and passing
   layer flags. `brewlet build` reads it and produces exactly the same artifact
   shape as today.
2. **Dependency bundles are infrastructure, like runtimes.** A platform team
   publishes a dependency bundle (for example, the Spring Boot libraries),
   approves it under a stable ID, and places it on node pools. An application
   names the one bundle it needs. The node mounts the approved bundle next to
   the approved runtime at container start. Patching a bundle is a rotation, the
   same operation as patching a runtime. The application image is never rebuilt.

Today's build-time composition (`brewlet bundle`, `brewlet verify`,
`--layer-set` and Part II) is removed. Dependencies either ship inside the
application as ordinary layers, or are provided by the platform as a bundle.
There is no third path.

### 1.1 Walkthrough: `payroll` needs `spring-boot`

**Platform team, once.** Publish the bundle and approve it under an ID:

```sh
brewlet bundle build registry.example.com/platform/spring-boot:3.3.5 \
  --name spring-boot --version 3.3.5 --ecosystem java \
  --layer ./bundle/lib:lib --lock ./bundle/lock.json
brewlet push registry.example.com/platform/spring-boot:3.3.5

brewlet k8s bundle add spring-boot-3.3 \
  --image registry.example.com/platform/spring-boot@sha256:<digest> \
  --runtime-images java-21
```

and offer it on a pool, next to the runtime:

```yaml
kind: NodeProfile
spec:
  runtimeImages:
    - id: java-21
  bundles:
    - id: spring-boot-3.3
      install: OnDemand
```

**Application team.** Write a `Brewletfile` and build:

```dockerfile
PLATFORM linux/amd64 linux/arm64
RUNTIME java-21
BUNDLE spring-boot-3.3

LAYER app ./target/app /app

ENTRYPOINT ["java"]
CMD ["-cp", "/app/payroll.jar:/bundles/spring-boot-3.3/lib/*", "com.example.payroll.Main"]
```

```sh
brewlet build registry.example.com/apps/payroll:1.4.0
brewlet push registry.example.com/apps/payroll:1.4.0
```

The artifact contains only `payroll.jar` and a `launch.json` that records
`"bundle": "spring-boot-3.3"`.

**Deployment.** The reviewed manifest selects the container's runtime and
bundle by ID, never by digest:

```yaml
kind: Deployment
spec:
  template:
    metadata:
      annotations:
        runtime-image.brewlet.sh/payroll: java-21
        bundle.brewlet.sh/payroll: spring-boot-3.3
    spec:
      runtimeClassName: brewlet
      containers:
        - name: payroll
          image: registry.example.com/apps/payroll@sha256:<app digest>
```

At container start the shim assembles:

```text
/                         java-21 runtime root (read-only)
/app/payroll.jar          application layers (read-only)
/bundles/spring-boot-3.3  current spring-boot-3.3 generation (read-only)
```

**A Spring CVE.** The platform team publishes `3.3.6` and runs
`brewlet k8s bundle rotate spring-boot-3.3 --image …@sha256:<new digest>`.
New containers mount the new generation. Workloads that opted in to rotation
restarts are rolled. Payroll's image, digest and manifest do not change.

## 2. Motivation

- **Hand-written `launch.json` plus layer flags is awkward.** A build is spread
  across a JSON file and a long command line (§11.1). Developers already know the
  Dockerfile shape. A Brewletfile keeps everything in one reviewable file without
  adding build steps.
- **Shared dependencies have the same remediation problem as runtimes.** Under
  Part II, a dependency fix requires recomposing and redeploying every
  application image that consumed the old layer set. Brewlet already solved this
  for runtimes: approve by ID, rotate the digest, and let nodes converge. A
  dependency bundle owned by the platform team should work the same way.
- **One mental model.** Runtimes and bundles share inventory, placement,
  installation, leases, revocation, rotation restarts and observability. Teams
  learn the model once.

## 3. Goals and non-goals

### Goals

- A Brewletfile is the only input `brewlet build` needs to produce a 1.0.0
  application artifact.
- The Brewletfile maps one-to-one to `launch.json`, the image-config mirror and
  the layer table. No inference occurs.
- Platform teams publish, approve, place, rotate and revoke dependency bundles
  exactly as they manage runtime images.
- An application declares the one bundle it requires in the artifact. Each
  workload container selects it by ID in the manifest. The shim enforces both.
- Bundle patches reach workloads without rebuilding or redeploying application
  images.
- Running containers stay traceable to the exact runtime and bundle generations
  they use.

### Non-goals

- **Build steps.** No `FROM`, `RUN` or `COPY --from`. Ecosystem tools produce the
  bytes; the Brewletfile only packages them.
- **Shipping the Brewletfile.** It is a build input, not part of the artifact.
  `launch.json` remains the authoritative contract that the shim reads.
- **Deployment configuration.** The Brewletfile does not select the runtime or
  bundles for a workload and does not generate Kubernetes manifests.
- **Several bundles per container.** A container uses at most one bundle. A
  container that needs several library families uses one bundle that the
  platform team composes and tests as a unit.
- **Dependency resolution.** No Maven, pip, npm or NuGet resolution occurs in
  the CLI, provisioner or shim. A bundle contains the actual files.
- **Automatic lookup paths.** The shim never adds bundle paths to a classpath,
  `PYTHONPATH`, `NODE_PATH` or any other variable. The application's argv and
  environment reference them explicitly.
- **Conflict detection.** Brewlet does not detect duplicate or shaded classes or
  modules between the application, its bundle and its runtime.
- **Compiled-in dependencies.** Libraries statically linked into a binary or a
  `.wasm` module still require rebuilding that binary.

## 4. Brewletfile

### 4.1 Format

A Brewletfile is a UTF-8 text file. `brewlet build REF` reads `./Brewletfile`;
`-f FILE` selects another file.

- Each non-blank line is a comment or an instruction. A comment is a line whose
  first non-blank character is `#`. Trailing comments are not supported.
- An instruction is an uppercase keyword followed by arguments. Keywords are
  case-sensitive.
- A line ending in `\` continues on the next line. Blank and comment lines are
  not allowed inside a continuation.
- Arguments are separated by whitespace, except the JSON forms below.
- There are no variables, `ARG`, substitution, parser directives or includes.
  `$` is an ordinary character.
- Unknown keywords, a missing required instruction, or a repeated
  single-use instruction fail with `InvalidBrewletfile` and the line number.

### 4.2 Instructions

| Instruction | Use | Form | Rules |
|---|---|---|---|
| `PLATFORM` | required, repeatable | `PLATFORM <os/arch[/variant]>...` | At least one platform overall. Duplicates fail with `InvalidPlatform`. More than one platform produces an image index. |
| `LAYER` | required, repeatable | `LAYER [--platform=<p>] <app\|dependency> <src> <dest>` | `src` is a directory relative to the Brewletfile's directory and MUST NOT escape it. `dest` is `/app` or an absolute path beneath it. `--platform` names a declared platform, or fails with `InvalidPlatform`. |
| `RUNTIME` | optional, repeatable | `RUNTIME <id>...` | Runtime image IDs the artifact was tested against. |
| `BUNDLE` | optional, once | `BUNDLE <id>` | The one dependency bundle the artifact requires (section 5). |
| `WORKDIR` | optional, once | `WORKDIR <absolute path>` | |
| `ENV` | optional, repeatable | `ENV <NAME>=<value>` | One variable per instruction, split at the first `=`. `<value>` is a JSON string literal, or a bare word without whitespace that does not start with `"`. Names are unique. `PATH` is forbidden (§8.3). |
| `ENTRYPOINT` | required, once | `ENTRYPOINT ["exe", "arg"...]` | Non-empty JSON array of strings. There is no shell form. |
| `CMD` | optional, once | `CMD ["arg"...]` | JSON array of strings. |

IDs in `RUNTIME` and `BUNDLE` use the ID grammar of §7.1. Malformed or
duplicate runtime IDs fail with `InvalidRuntimeImageID`; a malformed bundle ID
fails with `InvalidBundleID`. A second `BUNDLE`, or a `BUNDLE` with more than
one ID, fails with `InvalidBrewletfile`.

### 4.3 What `brewlet build` produces

| Brewletfile | `launch.json` | Image config |
|---|---|---|
| `ENTRYPOINT` | `process.entrypoint` | `Entrypoint` |
| `CMD` | `process.args` | `Cmd` |
| `WORKDIR` | `process.workingDir` (omitted when absent) | `WorkingDir` (`/app` when absent) |
| `ENV`, in file order | `process.env` | `Env` |
| `RUNTIME`, in file order | `runtimeImages` (omitted when absent) | — |
| `BUNDLE` | `bundle` (omitted when absent) | — |
| `PLATFORM` | — | `os`, `architecture`, `variant`; one manifest per platform |
| `LAYER`, in file order | — | Layers and `rootfs.diff_ids` |

Each platform receives the unprefixed `LAYER`s and its own `--platform` layers,
in file order, followed by the launch layer. Each platform MUST end up with at
least one `app` layer. All §6.2 content rules and §11.1 determinism rules apply
unchanged.

The Brewletfile is not shipped. Comments, whitespace and line continuations
therefore never change an artifact digest. The same instructions and source
bytes always produce the same artifact.

`brewlet build` no longer accepts `--launch`, `--layer`, `--platform` or
`--layer-set`. `brewlet build` remains offline: it never contacts a registry.
`brewlet push`, `brewlet index` and `brewlet inspect` are unchanged.

A Brewletfile equivalent to the §6.3 example:

```dockerfile
PLATFORM linux/amd64 linux/arm64
RUNTIME python-3.12 python-3.12-fips

LAYER dependency ./build/site-packages /app/site-packages
LAYER app        ./src                 /app/src

ENV PYTHONPATH=/app/src:/app/site-packages
ENV PYTHONDONTWRITEBYTECODE=1
ENTRYPOINT ["python"]
CMD ["-m", "gunicorn", "app.wsgi:application", "--bind", "0.0.0.0:8000"]
```

The CLI, the Maven plugin and the shim continue to share the §6.4 conformance
fixtures. Brewletfile fixtures add each valid input with its expected
`launch.json` and config, plus invalid inputs with their reason codes.

## 5. Declaring bundles in the artifact

`launch.json` gains one optional top-level field:

```json
{
  "schemaVersion": 1,
  "process": {
    "entrypoint": ["java"],
    "args": ["-cp", "/app/payroll.jar:/bundles/spring-boot-3.3/lib/*", "com.example.payroll.Main"]
  },
  "runtimeImages": ["java-21"],
  "bundle": "spring-boot-3.3"
}
```

| Field | Required | Rules |
|---|---|---|
| `bundle` | no | One bundle ID (§7.1 grammar). The shim mounts exactly this bundle (section 9). Null and the empty string are invalid. |

The schema version is not incremented. Shims without this proposal reject the
field as unknown, so an artifact that needs a bundle cannot start without it.
`bundle` is not mirrored into the image config.

An application MAY combine its bundle with its own `dependency` layers. Both are
read-only; the application's argv and environment determine lookup order.

## 6. Dependency bundle artifact

The layer set of §6.5 becomes the **dependency bundle**.

### 6.1 Format

| Component | Media type and content |
|---|---|
| Artifact type | `application/vnd.brewlet.bundle.v1+json` |
| Config | `application/vnd.brewlet.bundle.config.v1+json`: `schemaVersion`, `name`, `version`, `ecosystem`, `layers` (`digest`, `diffId`). `name`, `version` and `ecosystem` are informational. |
| Layers | `application/vnd.oci.image.layer.v1.tar+gzip` (or `+zstd`), annotation `brewlet.sh/layer=bundle` |
| Lock (required) | `application/vnd.brewlet.bundle.lock.v1+json`, a manifest layer: `{ecosystem, name, version, path, sha256, group?, purl?}` entries |
| SBOM (required) | One CycloneDX referrer |
| Provenance (optional) | A signed publisher-provenance referrer, as today |

A single manifest is a platform-neutral bundle. An image index provides
per-platform bundles. An index MUST cover every node architecture of the pools
that offer the bundle, as §7.2 requires of runtimes.

### 6.2 Content rules

Bundle layer entries are relative to the **bundle root**, not to `app/`. The
§6.2 rules otherwise apply unchanged: no absolute paths, no `..`, no escaping
links, no devices, no setuid or setgid bits, no extended attributes,
world-readable content, and whiteouts scoped to the bundle root.

The lock is complete. Every regular file in the bundle appears exactly once,
with a matching SHA-256 hash, and every entry names an existing file.

### 6.3 Producing a bundle

`brewlet bundle build` replaces `brewlet layer-set`:

```sh
brewlet bundle build REF --name N --version V --ecosystem E \
  --layer SRC:DEST... [--platform P] [--lock FILE] [--sbom FILE] \
  [--signing-key FILE --builder-identity URI] [--store DIR]
```

- `DEST` is relative to the bundle root, for example `lib` or `site-packages`.
- Without `--platform`, the result is platform-neutral. With it, the result is
  one platform manifest; `brewlet index` combines several.
- Without `--lock`, the CLI generates a file-level lock (every file, with name
  equal to its path), so every bundle has one.
- The result is written to a local OCI layout. `brewlet push` publishes it
  byte for byte, exactly as for applications.

The Maven plugin's `layer-set` goal becomes a `bundle` goal that publishes a
`java` bundle with complete GAV lock entries.

### 6.4 A bundle ID is a compatibility promise

As for runtime images (§7.2), rotating the digest behind a bundle ID MUST
preserve what applications built against the ID depend on:

- the directories applications reference, such as `lib/` or `site-packages/`;
- the ecosystem and the compatible runtime IDs;
- the API and ABI of every package, which means patch-level updates under the
  ecosystem's conventions only.

File names inside a referenced directory MAY change. Applications therefore
reference directories or wildcards (`lib/*`), not versioned file names. A
breaking change, including removing a package, MUST be published under a new
ID.

## 7. Inventory and placement

### 7.1 `DependencyBundle`

Approval is a cluster-scoped `DependencyBundle` (`node.brewlet.sh/v1alpha1`).
It mirrors `RuntimeImage`:

```yaml
apiVersion: node.brewlet.sh/v1alpha1
kind: DependencyBundle
metadata:
  name: spring-boot-3.3          # the bundle ID
spec:
  source:
    image: registry.example.com/platform/spring-boot@sha256:<64 hex>
  description: Spring Boot 3.3 libraries, patched by the platform team
  runtimeImages: [java-21, java-21-fips]   # optional allowlist
  compatibility:                           # informational
    ecosystem: java
status:
  generation: 4
  digest: sha256:<64 hex>
  conditions: [ { type: Approved, status: "True" } ]
```

- `metadata.name` is the ID, with the §7.1 grammar and at most 55 characters,
  so `brewlet.sh/bundle.<id>` is a valid label key.
- `spec.source.image` follows the `RuntimeImage` source rules: digest-pinned,
  source policy and mirror allowlist.
- `spec.runtimeImages` is an optional allowlist of runtime IDs this bundle may be
  used with. When present, admission and the shim refuse a container whose
  selected runtime is not listed, with `BundleRuntimeIncompatible`. Changes
  apply to new container starts; they are not rotations.
- Changing `spec.source.image` is a rotation. Generation, inventory epoch
  (`metadata.uid`), revocation, node-local authorization and its expiry, and
  deletion finalizers follow §7.1 exactly, with `Bundle*` reason codes.
- Creating, updating or deleting a `DependencyBundle` is an approval act and
  SHOULD be restricted to platform administrators.

### 7.2 `NodeProfile.spec.bundles`

```yaml
spec:
  runtimeImages:
    - id: java-21
  bundles:
    - id: spring-boot-3.3            # install: Eager is the default
    - id: django-5.1
      install: OnDemand
      idleTTL: 72h
```

Entries follow the `runtimeImages` entry rules of §7.3: unique `id`,
`install: Eager | OnDemand`, `idleTTL` only with `OnDemand`. Unknown IDs make
the profile not ready (`UnknownBundle`) without blocking other entries. Eager
bundles gate node readiness like eager runtimes.

### 7.3 Installation, rotation and on-demand use

The provisioner installs bundles with the same pull credentials, mirror policy,
ownership fences, atomic `current` pointer, leases, grace-period collection,
on-demand states and idle reclamation as runtimes (§7.3, §7.5):

- Generations live under
  `/opt/brewlet/bundles/<id>/<inventory-uid>/<generation>/`, with
  `/opt/brewlet/bundles/<id>/current`.
- On-demand requests use markers in `/opt/brewlet/bundles/.requests/<id>`.
- Before publishing `current`, the provisioner selects the node's platform,
  applies the content rules of section 6.2 and verifies every extracted
  file against the lock. Failures are reported as `bundle-platform-unavailable`,
  `bundle-content-violation` or `bundle-lock-mismatch`, and the generation is
  not published.
- The shim never pulls a bundle. The provisioner is the only writer of bundle
  roots.

## 8. Workload selection and admission

### 8.1 Container selection

Each container selects at most one bundle through a Pod annotation whose suffix
is the exact container name, next to its runtime selection (§8.1):

```yaml
metadata:
  annotations:
    runtime-image.brewlet.sh/orders: java-21
    bundle.brewlet.sh/orders: spring-boot-3.3
    runtime-image.brewlet.sh/telemetry: java-21
    bundle.brewlet.sh/telemetry: otel-java-2
```

- The key MUST contain a declared container name and the value MUST be one ID
  of the §7.1 grammar. Lists, empty values and malformed IDs fail closed with
  `InvalidBundleID`.
- Only a container with a runtime selection may select a bundle. A bundle
  selection for an unselected (ordinary OCI) container fails with
  `InvalidBundleSelection`.
- An absent selection means the container uses no bundle. Two containers of one
  Pod may select different bundles or the same bundle.
- Bundle selections follow the immutability and ephemeral-reservation rules of
  runtime selections (§8.1).

### 8.2 Admission and scheduling

For every `runtimeClassName: brewlet` pod on CREATE, `brewlet-admission` also:

- validates each bundle selection against a declared container with a runtime
  selection, and that each selected `DependencyBundle` exists and is approved
  (`InvalidBundleID`, `InvalidBundleSelection`, `UnknownBundle`);
- rejects a selection whose bundle's `spec.runtimeImages` excludes that
  container's selected runtime (`BundleRuntimeIncompatible`);
- ANDs `brewlet.sh/bundle.<id> In ["ready", "available"]` for each distinct
  selected bundle into every required `nodeAffinity` term, following §9's
  rules, and appends one preferred `In ["ready"]` term (weight 100) per bundle;
- denies with `NoCompatibleBundle` when valid profiles offer every selected
  runtime but none also offers every selected bundle. As for runtimes (§9), one
  profile MUST offer the complete set; profiles offering disjoint subsets do not
  jointly authorize a Pod.

Admission still never reads the registry. It cannot compare a selection with
the artifact's `bundle` field; the shim does (section 9.1). Fail-open admission
therefore never weakens bundle enforcement.

### 8.3 Capability labels and node annotation

| Label | Value | Meaning |
|---|---|---|
| `brewlet.sh/bundle.<id>` | `ready` | A validated `current` generation is installed. |
| `brewlet.sh/bundle.<id>` | `available` | An uninstalled `OnDemand` entry of the node's profile. |

Generation detail is published in the node annotation `brewlet.sh/bundle-states`,
with the same JSON shape as `brewlet.sh/runtime-images`. Revocation, expiry and
quarantine withdraw labels, or establish the §9.2 fence, as for runtimes.

Provisioning adds `bundle.brewlet.sh/*` and `bundle-generation.brewlet.sh/*`
(section 10) to containerd's forwarded pod annotations, next to the runtime
prefixes. Readiness validation checks this.

## 9. Sandbox assembly

### 9.1 Shim `Create`

Unselected containers delegate unchanged to runc (§8.2 step 1) and never
resolve a bundle. For a selected container, a new step runs after runtime
resolution (§8.2 step 4) and before layer materialization (§8.2 step 5). It
compares the container's forwarded `bundle.brewlet.sh/<container>` selection
with its `launch.json` `bundle`:

1. If the artifact declares a bundle and the container has no selection, fail
   with `BundleNotSelected`. If the selection differs from the declaration, or
   the artifact declares no bundle, fail with `BundleSelectionMismatch`. If
   neither exists, the step ends.
2. Check the bundle's runtime allowlist against the container's selected runtime
   from the node-local authorization record (`BundleRuntimeIncompatible`), then
   authorization, revocation and epoch (`BundleAuthorizationExpired`,
   `BundleRevoked`).
3. If the bundle is `available`, request installation. The runtime and the
   bundle share one §7.5 wait bound (`BundleInstalling`, `BundleInstallFailed`).
   If the node's profile does not offer the ID, fail with `BundleNotInstalled`.
4. Enforce any restart barrier for the bundle's ID (section 10).

The shim then takes the runtime lease and the bundle lease atomically, runtime
first, within one durable lease record. If any step fails, it holds no lease.
Exec uses the leased generations recorded in the task state, never `current`.

### 9.2 `/bundles` boundary and mount policy

- Every selected container gets a private, read-only, empty `/bundles`
  directory over the runtime root. Like the `/app` boundary, it hides any
  runtime content at `/bundles`, whether or not the container uses a bundle.
  Unselected OCI containers are unaffected.
- The selected bundle's leased generation is bind-mounted read-only, `nosuid`
  and `nodev`, at `/bundles/<id>`. No other bundle is mounted.
- Application layers cannot reach `/bundles` because §6.2 confines them to
  `/app`.
- `/bundles` and its descendants are added to the non-declarable zones of the
  runtime mount policy. Workload volumes and devices there fail with
  `RuntimeMountNotAllowed`.
- Runtime `PATH` entries and symlink resolution MUST NOT traverse `/bundles`,
  as §7.2 requires for `/app`.

## 10. Rotation, restarts and rollback

- **Rotation.** Re-pinning `spec.source.image` creates a new generation. New
  containers mount it as their nodes converge; running containers keep their
  leased generation.
- **Rollback.** Re-pinning a previous digest is a rotation to a new generation.
  Generations are never reused, and no application changes.
- **Restarts.** The §7.6 opt-in `brewlet.sh/restart-on-rotation: "true"`
  covers bundle rotations too. The operator selects opted-in workloads with a
  declared container selecting the rotated bundle ID, and restarts the whole
  Pod template. It uses the same lease-evidence rules (with
  `DependencyBundle.status.taskObservations`), pacing slots, deferral and
  out-of-scope rules.
- **Barrier.** The operator patches one template annotation per bundle ID, with
  the JSON shape of the runtime barrier:

  ```text
  bundle-generation.brewlet.sh/spring-boot-3.3: {"id":"spring-boot-3.3","inventoryUID":"<uid>","generation":5}
  ```

  The shim enforces it for each container selecting that ID, as §7.6 does for
  runtimes: an epoch mismatch fails with `BundleEpochMismatch`, an older local
  generation fails with `BundleUpdating` after the bounded wait, and a
  malformed value or an `id` that differs from the key fails with
  `InvalidBundleGeneration`. The chart's admission policy lets one operator
  update advance at most one `runtime-generation.brewlet.sh/<id>` or
  `bundle-generation.brewlet.sh/<id>` barrier for an ID selected by a declared
  container.
- **Holding a version.** A team that must not move asks for a separate ID, as
  in §7.4. Because the ID is part of the mount path, adopting a hold ID changes
  the application's `BUNDLE` line and argv; only the launch layer is
  republished.

Platform teams SHOULD rotate a bundle in a staging cluster before production.

## 11. Security

- **Trust roots.** Executable bytes outside `/app` come only from approved,
  digest-pinned runtime roots and bundle roots, apart from explicitly mounted
  Kubernetes volumes. The pod annotation selects among approved bundles; it
  cannot introduce one.
- **Bundle rotation is a privileged act.** It changes code that runs in every
  workload using the ID on its next start. RBAC, signature policy on bundle
  sources and audit logging SHOULD match `RuntimeImage`.
- **Integrity.** The provisioner verifies digest, content rules and every file
  against the complete lock before publishing a generation. Bundle roots are
  read-only to every container.
- **Isolation.** Applications cannot overwrite bundle or runtime files. They can
  still load their own copies of a library first through argv or environment.
  Rotation remediates only what a process actually loads from the bundle.
- **On-demand requests cannot widen the inventory.** They follow §7.5 and §12:
  only IDs that the node's profile lists as `OnDemand` and that are approved.
- **Fail closed.** The shim enforces declaration, selection, authorization,
  revocation and barriers even when admission fails open or a pod is placed
  without admission.

## 12. Observability and tooling

- Shim event `BundleResolved` on each start of a container with a bundle, with
  ID, generation, inventory UID and digest.
- `brewlet_container_bundle_info{namespace,pod,pod_uid,container,container_id,id,inventory_uid,generation,digest}`
  from each node's live leases, alongside the runtime info metric. Together with
  the application digest, these identify a container's full executable content.
- `brewlet_bundle_generations{id,state}`,
  `brewlet_bundle_install_requests_total{id,outcome}` and
  `brewlet_bundle_install_duration_seconds{id}`.
- `DependencyBundle.status.taskObservations`, `OldGenerationTasks`, rotation
  conditions and restart status, as in §13 for `RuntimeImage`.
- `brewlet inventory` is rewritten to report each running container's runtime
  and bundle generations from the same lease snapshots, and links each bundle
  generation to its lock and SBOM. Missing or stale evidence is reported as
  unverified, never as clean.
- `brewlet k8s bundle add|rotate|remove|list|status` mirrors `brewlet k8s
  runtime`. `remove` refuses while profiles or workloads reference the ID unless
  forced.
- `brewlet run REF --runtime <id>=<image> --bundle <id>=<image>` mounts the
  bundle locally at `/bundles/<id>`. The artifact's declared bundle MUST be
  given, and any other is an error.

New reason codes: `InvalidBrewletfile`, `InvalidBundleID`,
`InvalidBundleSelection`, `UnknownBundle`, `NoCompatibleBundle`,
`BundleNotSelected`, `BundleSelectionMismatch`, `BundleRuntimeIncompatible`,
`BundleNotInstalled`, `BundleInstalling`, `BundleInstallFailed`,
`BundleRevoked`, `BundleAuthorizationExpired`, `BundleEpochMismatch`,
`BundleUpdating`, `InvalidBundleGeneration`. New provisioner errors:
`bundle-platform-unavailable`, `bundle-content-violation`,
`bundle-lock-mismatch`.

## 13. Trade-offs

- **The application digest no longer identifies every dependency.** A
  container's content is the application digest plus runtime and bundle
  generations. These are recorded per container (section 12). Reproducing an
  old deployment requires the old bundle digest, so platform teams SHOULD
  retain published bundle digests for their rollback window.
- **Dependency patches carry more risk than runtime patches.** Libraries are
  closer to application code. Section 6.4 limits rotations to compatible patches, and
  publishers SHOULD test them against representative applications. Applications
  that need exact versions keep dependencies in their own layers.
- **The ID is part of the path.** This keeps argv explicit and readable, but
  hold IDs require a launch-layer change (section 10).
- **Not every dependency fits.** Compiled-in libraries (Go, Rust, self-contained
  .NET, linked `.wasm`) still require rebuilding the application.

## 14. Removed and changed

Brewlet is pre-1.0, so removals are direct, with no aliases or migration
warnings. No tests assert that removed features stay absent.

| Area | Change |
|---|---|
| Specification | Remove Part II (§16), §6.5 option 2 and `brewlet.sh/layerset-evidence`. Rewrite §6.5 as "dependency layers or dependency bundles". Update §1's diagram, §3 goals, §5 terminology ("Layer set" becomes "Dependency bundle"), §11's Java table and §13.2's reference to §16.11.1. Replace §15 "Shim-pulled dependency artifacts" with section 15's revision. |
| CLI | Remove `bundle` (composition), `verify`, `layer-set` and the `build` flags `--launch`, `--layer`, `--platform` and `--layer-set`. Add `bundle build`, `run --bundle` and `k8s bundle`. Rewrite `inventory`. |
| Maven plugin | Replace the `layer-set` goal with `bundle`. Replace managed composition settings with a `bundle` declaration that writes `launch.json` `bundle`, excludes bundle-provided dependencies from application layers after checking them against the bundle lock, and appends `/bundles/<id>/lib/*` to the generated classpath. |
| Artifacts | Rename layer-set media types to the bundle types of section 6.1. |
| Acceptance | Remove `acceptance/composition-requirements.json` and `composition_check.py`. Add the section 18 scenarios to the baseline ledger. |
| Docs | Rewrite `managed-composition.md`, `dependency-layer-sets.md`, `update-managed-dependency.md` and `dependency-remediation.md` as bundle guides. Update every page that documents `launch.json` authoring, `--layer`, `layer-set`, `bundle` or `verify`, including `applications.md`, `cli-reference.md`, `cheat-sheet.md`, the language pages and `examples/managed-dependencies`. |

## 15. Alternatives considered

### Keep build-time composition alongside bundles

Two managed paths would make every guide explain when to use which, and
build-time composition still requires a new application digest per fix. Node
bundles cover its purpose with the runtime model teams already use.

### Shim-pulled dependency artifacts

This replaces the current §15 entry. Pulling bundles in the shim was rejected
because it needs registry credentials outside kubelet and adds a pull to every
cold start. This proposal instead has the provisioner install approved bundles
through its existing runtime path. The shim still never pulls.

### Bundles inside runtime images

A platform team can already approve a composed runtime such as
`java-21-spring-boot-3.3`. That multiplies IDs for every runtime and bundle
combination, and couples their rotations. Separate bundles keep both
independent.

### Several bundles per container

Lists of bundles would raise ordering and overlap questions between
platform-owned libraries, multiply placement combinations and complicate
runtime allowlists. One bundle per container keeps each container's dependency
source a single approved, tested unit; a platform team that wants several
library families together composes them into one bundle.

### Pod-wide bundle selection

One annotation listing the union of every container's bundles cannot express
which container uses which bundle. With per-container runtimes (§8.1),
admission could then not check a bundle's runtime allowlist against the right
container. Bundle selection is therefore keyed by container, like runtime
selection.

### Mount bundles under `/app`

Placing bundles at `/app/bundles/<id>` would let application layers shadow them
and weaken the `/app` ownership rule. A separate `/bundles` boundary keeps each
prefix owned by one party.

### A YAML or JSON build file

A structured file would be equally expressive. The line-oriented format was
chosen because it is familiar from Dockerfiles, diffs cleanly and has no
nesting.

### Reading bundle requirements from the artifact at admission

Admission could pull `launch.json` and default the selection, but admission
never reads the registry (§9). The explicit annotation keeps the reviewed
manifest complete and enables scheduling before any pull.

## 16. Open questions

- **ID-independent mount names.** For example, `BUNDLE spring-boot-3.3 AS spring`
  mounting at `/bundles/spring`, so hold IDs need no argv change. This adds a
  second name to reason about.
- **Bundle authoring file.** Whether `brewlet bundle build` should also read a
  small file instead of flags.
- **Staged rotation.** Whether a rotation can target some pools first, rather
  than relying on staging clusters or separate IDs.
- **Policy visibility.** Whether to mirror `bundle` into an informational image
  label so registry-side policy tools can read it without parsing the launch
  layer.

## 17. Implementation milestones

| Milestone | Deliverable | Exit criteria |
|---|---|---|
| B1: Brewletfile | Parser, `brewlet build -f`, conformance fixtures, removal of `--launch`, `--layer` and `--platform`. The Maven plugin is unaffected. | Every existing artifact fixture is reproducible from a Brewletfile with identical digests; invalid inputs fail with their codes. |
| B2: bundle artifact | `launch.json` `bundle`, `brewlet bundle build`, `inspect`, `run --bundle`, Maven `bundle` goal. | A Python and a Java application run locally against bundles; lock, content and platform violations fail. |
| B3: inventory and nodes | `DependencyBundle`, `NodeProfile.spec.bundles`, provisioner install, labels, authorization, revocation and collection. | Eager and OnDemand installs, rotation, revocation and idle reclamation work on real nodes. |
| B4: workloads | Admission, shim selection, `/bundles` assembly, leases, Exec and mount policy. | The section 18 isolation and selection scenarios pass on real nodes, including fail-open admission. |
| B5: operations | Restart barrier, observability, `inventory`, `k8s bundle`, docs; removal of Part II and `--layer-set`. | End-to-end rotation and rollback without application changes, with traceable running generations. |

B1 is independent and can ship first. B3 depends on B2; B4 depends on B3; B5
depends on B4. Until B5, `--layer-set` keeps working alongside a Brewletfile,
so a managed dependency path always exists.

## 18. Required acceptance matrix

Python is the qualification fixture, as in §13.2. Java is exercised as an
example through both the CLI and the Maven plugin.

| Scenario | Required outcome |
|---|---|
| Brewletfile conformance | Valid fixtures produce the expected `launch.json`, config and digests; invalid fixtures fail with the expected reason and line. |
| End-to-end bundle | Publish and approve a bundle, deploy an application that declares and selects it beside an unselected OCI sidecar, and observe real dependency use, probes, Exec, identity and read-only `/bundles`; the sidecar is unaffected. |
| Rotation | Running containers keep their generation; new ones get the new generation; opted-in workloads roll under the barrier; old generations are collected after leases drain. |
| Rollback | Re-pinning the previous digest creates a new generation and restores behavior without application changes. |
| OnDemand | First use installs and the retried start succeeds; `idleTTL` reclaims; a failed install withdraws the label. |
| Revocation and expiry | New starts fail with `BundleRevoked` or `BundleAuthorizationExpired`; running containers continue. |
| Selection | A declared bundle without a selection fails with `BundleNotSelected`; a different or undeclared selection fails with `BundleSelectionMismatch`; unknown, malformed, runtime-incompatible and runtime-less selections are denied; two containers selecting different bundles each mount only their own; no single profile offering every selected runtime and bundle yields `NoCompatibleBundle`. |
| Isolation | Runtime `/bundles` content is hidden; volumes under `/bundles` are rejected; application layers cannot reach it. |
| Integrity | Lock mismatch, content violations and a missing platform prevent publication of a generation. |
| Traceability | Info metrics, `taskObservations` and `brewlet inventory` name the exact bundle generation and SBOM per running container. |

Native arm64 and native amd64 are separate scopes, as for the existing matrix.
