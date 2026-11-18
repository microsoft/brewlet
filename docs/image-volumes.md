# Reproducing Brewlet use cases with image volumes

This report tests how far native Kubernetes can go toward Brewlet's use cases
**without Brewlet**. It uses only
[image volumes](https://kubernetes.io/docs/concepts/storage/volumes/#image),
built-in admission policies, ordinary workload controllers and kubelet behavior.
A live harness exercises each use case on a real two-node cluster and records
the evidence.

The tested stack has no Brewlet CLI, shim, operator, provisioner,
RuntimeClass, CRD or webhook. It also has no custom controller, hostPath mount
or init-container copy.

## Summary

The tested native designs reproduce Brewlet's **delivery** model well. They
reproduce its **governance** model only partly, and do not reproduce its
**node-level enforcement** model. Brewlet's added value is integrating runtime
governance and lifecycle enforcement, not making separate delivery possible.

- **Reproduced natively:**
  - publishing runtime and application separately
  - rotating a runtime without rebuilding the application
  - selecting a runtime by ID instead of by digest
  - OnDemand pulls
  - leaving running Pods untouched during rotation and revocation
  - running Python, Java and Node on one shared base image
- **Approximated with extra parts and caveats:**
  - **Approved catalog:** a ConfigMap read by a MutatingAdmissionPolicy and a
    ValidatingAdmissionPolicy.
  - **Pool placement:** static node labels plus an injected `nodeSelector`.
  - **Eager installation:** a pre-pull DaemonSet.
  - **Inventory:** derived from Pod specs.
  - **Restart-time authorization:** `pullPolicy: Always` turns registry
    availability into a restart dependency.
- **Not reproduced:**
  - rollout that follows the catalog automatically
  - generation and lease accounting
  - a startup fence tied to provisioning
  - node-local authorization that survives a registry outage
  - node cleanup with evidence
- **Native advantage:** one Pod can mix runtimes. All application containers in
  a Brewlet Pod must use the same runtime ID.

ABI coupling is a **layout choice**, not an image-volume limitation. Brewlet
and Layout 1 use the complete runtime image as the container root. Layout 2
mounts runtime subtrees into a shared base, coupling their application binary
interface (ABI) to that base; its mismatches surfaced only at `exec`. A native
catalog could instead inject the complete runtime as the container image.
That hybrid is **untested**, not unavailable: the harness verified its
building blocks separately, but not their end-to-end combination.

Selection at Pod admission also changes rollout semantics. When the runtime
digest is injected there rather than recorded in the workload template,
Deployments can mix versions and `rollout undo` does not restore the runtime.
These are findings about the tested catalog design, not all image-volume
deployments. Layout 1 retains explicit references and native rollback.

The final run passed **34 of 34 checks**. See
[Use-case matrix](#use-case-matrix) for the per-check mapping.
These checks include successful reproductions and expected failures that
expose gaps; they are not a score of advantages over either solution. Brewlet
comparisons describe its documented behavior, not a head-to-head run.
Controls not built here are not proven impossible to implement natively.

## Method

### Environment

| Item | Value |
| --- | --- |
| Kubernetes | v1.36.4 (kind node `kindest/node@sha256:099e0493…`), one control-plane plus one worker, both schedulable |
| CRI | containerd v2.3.4 |
| Platform | `linux/arm64`, native engine architecture, no emulation |
| `ImageVolume` kubelet feature | Enabled (GA) |
| `ImageVolumeWithDigest` | Alpha, **disabled** by default |
| `KubeletEnsureSecretPulledImages` | Beta, enabled |
| Admission | `MutatingAdmissionPolicy` and `ValidatingAdmissionPolicy`, `admissionregistration.k8s.io/v1` |
| Pod Security | `restricted` enforced on every experiment namespace |
| Registry | Disposable, digest-pinned `registry` container on a loopback port, reached by nodes through containerd `hosts.toml` |

The harness records the feature gates from the kubelet's
`kubernetes_feature_enabled` metrics.

### Pinned inputs

| Role | Upstream source |
| --- | --- |
| Python runtime | `python` 3.13.10 and 3.13.11, by digest |
| Java runtime | Eclipse Temurin JRE 21.0.7 and 21.0.8, glibc, by digest |
| Musl Java runtime (ABI case) | Eclipse Temurin JRE 21, Alpine, by digest |
| Node runtime | `node` 22.17.0, by digest |
| Shared base | `gcr.io/distroless/cc-debian13` (glibc), by digest |
| Static base (ABI case) | `gcr.io/distroless/static-debian13` (no libc), by digest |
| Java application build | Temurin JDK 21, `javac --release 21` into a `FROM scratch` image |

The harness publishes every runtime, application and base image to the
experiment registry over the OCI Distribution API. Delivered digests therefore
differ from the upstream digests. `result.json` records both.

### Two layouts

The experiment tests two native designs:

1. **Runtime as the container image (Layout 1).** The complete Python image is
   the ordinary container image. The application and its dependency are
   scratch images mounted at `/app` and `/dependencies`. The runtime keeps its
   original root layout, avoiding ABI coupling to a separate shared base.
2. **Shared base plus runtime volume (Layout 2).** Every Pod uses one
   platform-owned `distroless/cc` image. The runtime is an image volume
   mounted with `subPath` at its expected location: `/usr/local` for Python and
   Node, `/opt/java/openjdk` for Java. The application is a second image
   volume at `/app`. A catalog chooses the runtime digest at admission. This
   layout is closer to Brewlet, where the platform owns the runtime and a
   workload names only a runtime ID.

Both layouts launch with an explicit Pod `command`, `env` and `workingDir`.
Image-volume `ENTRYPOINT` and `ENV` are not executed or applied. Layout 1
verifies this with fixtures that contain an invalid entrypoint and a poisoned
environment variable.

## Layout 2 architecture

### Catalog

The platform team owns a ConfigMap in its own namespace. Each key maps a
runtime ID to a digest-pinned runtime image. The `base` key names the shared
base.

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: runtime-catalog
  namespace: platform
data:
  base: registry.example.com/platform/base-cc@sha256:<digest>
  python-3.13: registry.example.com/platform/runtime-python@sha256:<digest>
  java-21: registry.example.com/platform/runtime-java@sha256:<digest>
  node-22: registry.example.com/platform/runtime-node@sha256:<digest>
```

### Workload template

The workload names a runtime ID in a label. It leaves the runtime reference
empty and uses a placeholder container image. A Deployment template may omit
`volumes[].image.reference`, but a Pod may not, so admission must fill it in.

```yaml
template:
  metadata:
    labels:
      app: java-21
      runtime.example.com/id: java-21
  spec:
    containers:
      - name: app
        image: platform-base            # replaced at admission
        command: ["/opt/java/openjdk/bin/java", "-Xmx64m", "-jar", "/app/app.jar"]
        volumeMounts:
          - name: runtime
            mountPath: /opt/java/openjdk
            subPath: opt/java/openjdk
          - name: application
            mountPath: /app
    volumes:
      - name: runtime
        image:
          pullPolicy: IfNotPresent      # reference injected at admission
      - name: application
        image:
          reference: registry.example.com/apps/java@sha256:<digest>
          pullPolicy: IfNotPresent
```

### Mutation

A `MutatingAdmissionPolicy` runs on Pod `CREATE` in namespaces labeled
`runtime-catalog=enabled`, when the runtime-ID label is present. It reads the
catalog as its parameter.

The policy needs two mutations:

- An `ApplyConfiguration` sets the `app` container image and the `runtime`
  volume reference. Both lists merge by `name`.
- A `JSONPatch` adds the pool `nodeSelector`. An ApplyConfiguration cannot set
  it, because `nodeSelector` is an atomic map and apply configurations may not
  modify atomic fields.

```yaml
apiVersion: admissionregistration.k8s.io/v1
kind: MutatingAdmissionPolicy
metadata:
  name: runtime-selection
spec:
  failurePolicy: Fail
  reinvocationPolicy: Never
  paramKind: {apiVersion: v1, kind: ConfigMap}
  matchConstraints:
    resourceRules:
      - apiGroups: [""]
        apiVersions: [v1]
        operations: [CREATE]
        resources: [pods]
  matchConditions:
    - name: runtime-id
      expression: >-
        has(object.metadata.labels) &&
        'runtime.example.com/id' in object.metadata.labels
  mutations:
    - patchType: ApplyConfiguration
      applyConfiguration:
        expression: >-
          Object{spec: Object.spec{
            containers: [Object.spec.containers{name: 'app',
              image: params.data['base']}],
            volumes: [Object.spec.volumes{name: 'runtime',
              image: Object.spec.volumes.image{reference:
                object.metadata.labels['runtime.example.com/id'] in params.data
                  ? params.data[object.metadata.labels['runtime.example.com/id']]
                  : 'unknown-runtime-id'}}]}}
    - patchType: JSONPatch
      jsonPatch:
        expression: >-
          (has(object.spec.nodeSelector) ? [] :
            [JSONPatch{op: 'add', path: '/spec/nodeSelector', value: {}}]) +
          [JSONPatch{op: 'add',
            path: '/spec/nodeSelector/' + jsonpatch.escapeKey('runtime.example.com/' +
              object.metadata.labels['runtime.example.com/id']),
            value: 'offered'}]
```

The `MutatingAdmissionPolicyBinding` sets
`paramRef: {name: runtime-catalog, namespace: platform,
parameterNotFoundAction: Deny}` and the same namespace selector.

### Validation

A `ValidatingAdmissionPolicy` with the same parameter applies to **every** Pod
created in a catalog namespace. Mutating admission runs first, so validation
sees the injected values. The policy has four validations:

| Validation | Purpose |
| --- | --- |
| The label names a non-`base` key present in the catalog | Rejects unlabeled Pods and unknown or removed runtime IDs. |
| Every container and init container uses `params.data['base']` | Prevents a workload from substituting its own root. |
| The `runtime` volume equals the catalog digest for that ID | Defense in depth if the mutation is bypassed or reordered. |
| `nodeSelector` contains `runtime.example.com/<id>: offered` | Keeps placement tied to pools that offer the runtime. |

The policy uses `messageExpression` to produce specific denials, for example
`runtime ID ruby-3 is not offered by the catalog`.

### Pools, Eager and OnDemand

A node offers a runtime when it carries `runtime.example.com/<id>=offered`.
The experiment labels the nodes as follows:

| Node | Offers |
| --- | --- |
| control-plane | `python-3.13`, `node-22` |
| worker | `python-3.13`, `java-21` |

**Eager** delivery is a DaemonSet in the `platform` namespace. It is outside the
catalog namespaces, so the catalog policies do not apply to it. The DaemonSet
selects the offering nodes and mounts the runtime image into a `pause`
container. The kubelet must pull the runtime to start that Pod. While the Pod
holds the image, the kubelet's image garbage collector treats the image as in
use.

```yaml
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: prefetch-java-21
  namespace: platform
spec:
  selector:
    matchLabels: {prefetch: java-21}
  template:
    metadata:
      labels: {prefetch: java-21}
    spec:
      nodeSelector:
        runtime.example.com/java-21: offered
      containers:
        - name: hold
          image: registry.k8s.io/pause:3.10
          volumeMounts:
            - {name: runtime, mountPath: /runtime}
      volumes:
        - name: runtime
          image:
            reference: registry.example.com/platform/runtime-java@sha256:<digest>
```

**OnDemand** delivery is the kubelet default: the first Pod that needs the
runtime pulls it.

## Use-case matrix

Legend:

- **Reproduced:** native behavior matches the Brewlet use case.
- **Partial:** a native construction approximates it with the stated caveats.
- **Not reproduced:** the tested designs do not supply the control; this is
  not a claim that native platform engineering cannot provide it.
- **Native advantage:** native Kubernetes allows something Brewlet does not.

| Brewlet use case | Native mechanism | Result | Evidence (check name) |
| --- | --- | --- | --- |
| Application and runtime are published separately | Separate OCI images; runtime as the container image (Layout 1) or as an image volume (Layout 2) | **Reproduced** | `separate application and dependency image volumes`; `Python, Java and Node apps run on one distroless/cc base with runtime volumes` |
| Rotate the runtime without rebuilding application bytes | Change the runtime reference | **Reproduced** | `runtime rotation without rebuilding application or dependency` |
| Running workloads keep the generation they started with | Pods are immutable, and the kubelet does not re-resolve volumes for a running container | **Reproduced** | `existing Pod keeps its old runtime after Deployment rotation`; `catalog rotation pre-pulls the new runtime and leaves running Pods unchanged` |
| Workload selects a runtime by ID, platform owns the digest | Label plus MAP injection from a ConfigMap | **Reproduced** | `catalog admission injects base image, runtime digest and pool selector` |
| Workload cannot substitute runtime bytes | MAP overwrites forged references, and VAP checks the base and runtime | **Partial**: Pod `CREATE` only | Forged `python:latest` container image and runtime volume were both rewritten to catalog digests |
| Approved runtime IDs only | VAP against the catalog (Layout 2) or a digest allowlist (Layout 1) | **Partial**: no content validation, no epoch | `catalog admission denies unknown runtime IDs and unselected runtimes`; `native admission rejects unapproved runtime and dependency references` |
| Pool placement (`NodeProfile`) | Static node labels plus a JSONPatch-injected `nodeSelector` | **Partial**: labels are not readiness | `runtime ID placement keeps each Pod on nodes offering its runtime` |
| Eager installation | Pre-pull DaemonSet per runtime ID | **Partial**: second source of truth, no startup fence | `Eager: DaemonSet pre-pulls java-21 only on nodes offering it`; `Eager Java Pod starts from the cached runtime; OnDemand Node Pod pulls it` |
| OnDemand installation | Kubelet pull on first use | **Reproduced** | Same check: the Node runtime was absent until first use, then pulled in 2.16 s from the local registry |
| Opt-in restarts after rotation | Manual `kubectl rollout restart` | **Not reproduced**: needs delivery automation or a controller | `rollout restart converges every replica on the rotated runtime` |
| Generation tracking and minimum-generation barriers | Digests are injected per Pod, with no generation recorded in the template | **Not reproduced**; Brewlet does not promise atomic convergence either | `scale-up after rotation mixes runtime versions inside one ReplicaSet` |
| Rollback | Catalog revert plus restart; `rollout undo` is ineffective | **Partial** | `rollout undo keeps the rotated runtime because templates carry no digest`; `catalog rollback plus rollout restart restores the previous runtime` |
| Inventory of running generations (`taskObservations`) | Read digests from Pod specs | **Partial**: spec only, not node-observed | `inventory from Pod specs identifies the stale runtime digest`; `Pod status omits image-volume digests while ImageVolumeWithDigest is off` |
| Revocation blocks new starts, does not kill running work | Remove the catalog key or allowlist entry; VAP denies new Pods | **Reproduced** for new Pods | `removing a runtime ID denies new Pods without stopping running Pods`; `native admission rejects new Pods selecting a revoked runtime digest`; `registry revocation leaves the running container serving`; `recovery onto approved runtime` |
| Authorization on every container Create, including restarts | `IfNotPresent`: none. `Always`: registry lookup on each start | **Partial**: availability tradeoff | `gap reproduced: existing Pod restarts revoked runtime without Pod admission`; `IfNotPresent restarts the revoked runtime from the node cache`; `pullPolicy Always blocks restart after registry revocation`; `restoring the manifest lets the blocked Pod restart in place`; `registry outage blocks pullPolicy Always restarts but not cached ones`; `registry recovery lets the blocked Pod restart in place` |
| Runtime runs from its own complete root, no shared-base ABI coupling | Layout 1: runtime image as the container image. Layout 2: mismatch fails at `exec` | **Reproduced** in Layout 1; combining it with catalog injection is untested | `separate application and dependency image volumes`; `ABI mismatch: glibc Python runtime cannot start on distroless/static`; `ABI mismatch: musl Temurin runtime cannot start on glibc distroless/cc` |
| Startup fence before provisioning completes | None | **Not reproduced** | Not built; placement labels exist before pre-pull completes |
| Read-only runtime and application | Image volumes are always read-only | **Reproduced**; root needs `readOnlyRootFilesystem` | `runtime and application mounts are read-only while the base root is writable` |
| Explicit launch, no image entrypoint | Pod `command`, `env`, `workingDir` | **Reproduced** | Asserted in every Layout 1 phase, for example `separate application and dependency image volumes` |
| Node cleanup of retired runtimes | Kubelet image garbage collection under disk pressure | **Not reproduced**: no evidence or finalizers | `replaced runtime stays cached on the node after its last Pod exits` |
| Multiple language runtimes | Per-language mount layout on a shared glibc base | **Reproduced** for Python, Java and Node | `Python, Java and Node apps run on one distroless/cc base with runtime volumes` |
| One runtime ID per Pod (Brewlet constraint) | Each container mounts its own runtime volume | **Native advantage** | `one Pod runs Python and Node containers from different runtime volumes` |
| Dependency remediation separate from application | Dependency as its own image volume (Layout 1) | **Reproduced** for one pure-Python module | `dependency-only remediation without rebuilding application` |

## Findings

### 1. Delivery: separation and rotation work natively

The harness verifies the following in **Layout 1**:

- Both replicas serve the mounted application.
- Python rotates from **3.13.10 to 3.13.11** while the application and
  dependency digests stay unchanged. The harness checks the application's
  source SHA-256 in every phase.
- A held Pod outside the Deployment keeps running 3.13.10 after the rollout.
- A dependency-only change switches the imported value from `original` to
  `patched`.
- Rolling back restores both the runtime and the dependency.

**Layout 2** runs Python 3.13.10, Temurin 21.0.7 and Node 22.17.0 on one
`distroless/cc` base. The application images are tiny: the Java application
volume is **2,909 bytes**. The Node runtime volume is **78,483,405 bytes**.

### 2. Catalog semantics differ from a recorded runtime generation

Injecting the digest at Pod admission makes the catalog authoritative for new
Pods. The Deployment, however, carries no record of the runtime. The harness
proves three consequences.

**Rotation does not roll out.** After `java-21` changes from 21.0.7 to 21.0.8,
the running Pod keeps the same UID and restart count 0, and still serves
21.0.7. Nothing changes until something creates new Pods.

**One ReplicaSet can run two runtimes.** Scaling the Deployment to two
replicas after rotation produces a 21.0.7 Pod and a 21.0.8 Pod. Both Pods have
the same `pod-template-hash` (`6c798ddc99`). Native rollout tooling therefore
reports the Deployment as fully up to date while it runs mixed versions.

**Rollout history does not include the runtime.** `kubectl rollout restart`
converges both replicas on 21.0.8. `kubectl rollout undo` then returns to
revision 1, but the new Pods still receive **21.0.8**, because the template
never contained the runtime digest. Rolling back the runtime requires
reverting the catalog and restarting again. The harness verifies that this
restores 21.0.7.

Brewlet takes a different approach. A rotation produces a monotonically
numbered generation, and, with rotation restarts enabled, opted-in workloads
get a minimum-generation barrier in their template. Its operator restarts
workloads still running an older generation and treats missing node reports
as stale. This provides visibility and opt-in convergence, not an atomic
switch: different generations can coexist during or after a deferred rollout.
The barrier is not an exact-version pin, so undoing a template is not a runtime
rollback mechanism for Brewlet either.

A native platform could use delivery automation or a controller to compare
running Pods against the catalog and patch templates, plus a convention to
record the expected digest for rollback. These gaps belong to the tested
admission-time catalog, not to image volumes generally. See
[rotation and cleanup](operations.md#rotate-without-rebuilding-application-bytes).

### 3. Placement, Eager and OnDemand

The MAP-injected selector keeps every Pod on a node offering its runtime. In
this run, Java ran on the worker and Node on the control-plane.

**Eager.** Before the DaemonSet existed, neither node had the Java or Node
runtime. The DaemonSet then pulled Java 21.0.7 onto the worker only. When the
Java workload started, the kubelet reported that the image was
`already present on machine` and did not pull it. On rotation, updating the
DaemonSet pre-pulled 21.0.8 before any workload used it.

**OnDemand.** No node had the Node runtime until the first Node Pod pulled it.
In this run, a 78 MB pull from the local registry took 2.16 s. A real registry
adds network latency, and first-use cost is not measured here.

Caveats:

- The catalog ConfigMap and the DaemonSet are **two sources of truth**. The
  harness updates both on every rotation. A controller would have to keep
  them in sync.
- Node labels say a runtime is *offered*, not *installed*. The scheduler can
  place a Pod before the pre-pull finishes. That Pod then simply pulls the
  runtime itself. Brewlet's [startup fence](providers.md) prevents scheduling
  until provisioning completes.
- Image-volume pull events repeat on Pod syncs. The Java Pod's
  `already present` event for its runtime reached count 4 within three
  seconds. Events are therefore weak evidence, so the harness checks node
  image stores with `crictl` instead.

### 4. Admission guarantees and their boundary

Catalog admission behaves as designed on Pod `CREATE`:

- A Pod that labels `python-3.13` but sets `python:latest` as both its image
  and runtime volume is admitted with **both rewritten** to the catalog
  digests. Mutation runs before validation.
- `ruby-3` is denied with `runtime ID ruby-3 is not offered by the catalog`.
- An unlabeled Pod in a catalog namespace that brings its own runtime is denied
  with `Pods in catalog namespaces must select a runtime ID`.
- After `node-22` is removed from the catalog, new Pods are denied. The running
  Node Pod keeps its UID, stays ready and still serves 22.17.0.

The boundary:

- Admission covers Pod creation only. Ephemeral containers arrive through the
  `pods/ephemeralcontainers` subresource, which these policies do not match.
- Anyone who can edit the catalog ConfigMap or the policies controls runtime
  selection for every catalog namespace.
- Admission checks names and digests. It cannot check that a runtime is
  compatible with the base it is mounted into (see finding 5).

Image volumes and their execution are allowed under the Pod Security
`restricted` profile. Every namespace in this experiment enforces it.

### 5. ABI coupling is a shared-base risk

Layout 2 separates the runtime from its original root, so the runtime's
loader and libc must come from the base. The harness ran two mismatches with
`restartPolicy: Never`:

| Case | Base | Runtime | Result |
| --- | --- | --- | --- |
| `python-on-static` | `distroless/static` (no libc) | glibc Python 3.13 | Exit **255**, reason `Error`, log `exec /usr/local/bin/python3: no such file or directory` |
| `musl-java-on-cc` | `distroless/cc` (glibc) | musl Temurin 21 | Exit **255**, reason `Error`, log `exec /opt/java/openjdk/bin/java: no such file or directory` |

The executable exists in both cases. The error refers to the missing ELF
interpreter, not the binary. Admission accepted both Pods, the scheduler placed
them, and the kubelet pulled every image. The incompatibility surfaced only at
`exec`.

Native libraries outside the mounted subtree are a related risk. The Python
volume carries only `/usr/local`. Extension modules that link against
libraries the full Python image installs in `/usr/lib` depend on the base
providing compatible versions. The fixtures used only modules that need glibc,
so this risk was not exercised.

Layout 1 avoids the coupling by keeping the complete runtime image as the
root. Brewlet takes the same approach: its provisioner validates and installs
the complete runtime image as the container root, and the application is
mounted at `/app`. See [concepts](concepts.md#from-publication-to-a-running-process).

Native Kubernetes could combine Layout 1's root with Layout 2's catalog. The
mutation would inject the catalog runtime as the **container image** instead
of a shared base. Both parts were verified separately: Layout 1 runs the
runtime image as the root with the application mounted, and Layout 2's
mutation rewrites the container image. The harness did not run that
combination end to end.

With that combination, the runtime image's `ENV` would apply to the process.
Pull-policy restart behavior (finding 9) would move to the container's
`imagePullPolicy`.

### 6. Mixed runtimes in one Pod are a native advantage

Pod `mixed` runs a Python container on port 8080 and a Node container on port
8081. Each container mounts its own runtime volume and application volume. The
containers report Python 3.13.10 and Node 22.17.0. Brewlet requires every
application container in a Pod to use one runtime ID. See
[coexistence](choosing-brewlet.md#coexistence).

### 7. Mounts are read-only, the root is not

Layout 2 deliberately sets `readOnlyRootFilesystem: false`. Mountinfo from each
language shows the following:

- `/` (the base) is `rw`.
- `/app` and the runtime mount (`/usr/local` or `/opt/java/openjdk`) are `ro`,
  without `readOnly: true` in the Pod.

A read-only runtime volume therefore does not protect the rest of the
container's view. Layout 1 sets `readOnlyRootFilesystem: true` and verifies a
fully read-only root. A native platform must require that setting in policy.
Brewlet enforces it in admission and the shim.

### 8. Inventory has no node-observed truth

Because admission pins every runtime reference by digest, Pod specs are an
inventory. After rotation and scale-up, the harness compared each Pod's
`runtime` volume with the catalog and identified exactly one stale Pod: the
original Java replica.

Pod **status** does not help. It includes the base image's `imageID`, but no
image-volume digest. The alpha `ImageVolumeWithDigest` gate, disabled here,
adds that status field. Without it, nothing observed on the node confirms what
a container actually mounted. Spec-derived inventory also fails when a
reference is not digest-pinned. Brewlet's `RuntimeImage`
`status.taskObservations` comes from node reports of running tasks. See
[find workloads still on an older generation](operations.md#find-workloads-still-on-an-older-generation).

### 9. Restart authorization: revocation versus availability

Pod admission does not run when a container restarts inside an existing Pod.
The harness tested both pull policies for the runtime volume. It used Pod
`always` with `pullPolicy: Always` and Pod `cached` with `IfNotPresent`. Both
Pods used the same base and application.

| Phase | `always` (`pullPolicy: Always`) | `cached` (`IfNotPresent`) |
| --- | --- | --- |
| Manifest deleted from registry, container keeps running | Still serving 3.13.11, restart count 0 | Not affected |
| Container exits after revocation | **Blocked**: same UID, restart count 0, not ready after 40 s; `ErrImagePull`, `NotFound` | **Restarts** from the node cache |
| Manifest restored | Restarts in place (same UID) | — |
| Registry disconnected from the node network, container exits | **Blocked**: restart count 1, reported as `CrashLoopBackOff` (`back-off 2m40s`) with a new pull-failure event | **Restarts** from the node cache |
| Registry reconnected | Restarts in place (same UID) | — |

Observations:

- **`IfNotPresent` restarts revoked runtimes.** Layout 1 reproduced the same
  gap after removing a runtime from the allowlist. The held Pod restarted
  Python 3.13.10 with the same UID, and its restart count went from 0 to 1.
- **`Always` enforces revocation at the registry on every start.** Running
  containers are never stopped. Each successful start re-resolved the
  manifest. With layers already on the node, those pulls of the 43 MB runtime
  took 10–23 ms. The harness did not measure network bytes.
- **The same mechanism blocks restarts during a registry outage.** Revocation
  and unavailability are indistinguishable to the kubelet.
- **Status can hide the cause.** During the outage, the container reported
  `CrashLoopBackOff` rather than an image-pull reason. Only the `Failed`
  pull events identified the runtime volume.
- **Recovery waits for kubelet back-off, not for the registry.** After the
  revocation-related exit, the container restarted 88 s later. After the
  outage-related exit, it restarted 2 min 47 s later, including a 2m40s
  container back-off. Back-off can grow to five minutes.

Brewlet behaves differently. Every container create checks a node-local
authorization, which expires after five minutes unless the node rechecks
approval and ownership. Revocation is enforced on the node without a registry
round trip. A registry outage does not block restarts of installed runtimes.
A disconnected node stops authorizing starts when its authorization expires.
This is still an availability cost: prolonged loss of renewal blocks recovery
starts even with cached bytes. See
[authorization, restarts and recovery](choosing-brewlet.md#authorization-restarts-and-recovery)
and [cleanup and revocation](operations.md#cleanup-and-revocation).
Neither design terminates running processes on revocation.

Registry deletion is also a coarse tool. It removes one repository's manifest
for every consumer. It does not revoke the same content elsewhere. In this
run, the revoked `revocable-runtime` manifest had the same digest as the
Python 3.13.11 runtime published in a separate repository.

### 10. Cleanup is garbage collection, not retirement

After `rollout restart`, the last Pod using Java 21.0.7 exited, but 21.0.7
stayed in the worker's image store. The kubelet removes unused images only
under disk pressure, using its image garbage-collection thresholds. Native
Kubernetes has no equivalent to Brewlet's finalizers and cleanup checkpoints.
Removing a runtime from a pool therefore never produces evidence that its
bytes are gone. On the other hand, image volumes change no host configuration,
so there is nothing to reverse.

## What a native platform would still need

To match Brewlet's guarantees with image volumes, a platform team would build
or adopt the following:

1. **A catalog controller.** It would keep the pre-pull DaemonSets and node
   labels in sync with the catalog.
2. **A rollout controller.** It would compare running Pod digests with the
   catalog, patch workload templates, and record the expected digest so that
   `rollout undo` stays meaningful.
3. **Readiness gating.** Pool labels would appear only after a pre-pull
   completes, or a scheduling gate would hold Pods until their runtime is
   present.
4. **A compatibility decision.** Either use each complete runtime image as the
   container root, or qualify every runtime against the shared base before
   publication: ABI, loader, libc and native library set.
5. **Coverage of all admission paths.** That includes ephemeral containers,
   plus `readOnlyRootFilesystem` and approved mount locations.
6. **A restart-authorization decision.** Either accept that cached runtimes
   restart after revocation, or accept registry availability as a restart
   dependency.
7. **Observed inventory.** Enable `ImageVolumeWithDigest` once available, or
   build a node agent that reports mounted digests.

Each item is ordinary platform engineering, not proof that a custom runtime
shim is required. Brewlet integrates these items instead.

## Not covered

- **No performance comparison.** The experiment did not compare startup time,
  memory, network or density. Image volumes and ordinary images both use
  content-addressed caches.
- **Not tested:**
  - private registries and image-pull credentials
  - signatures and provenance
  - multi-architecture images
  - Windows nodes
  - production-scale clusters
- **Language coverage:** only Python, Java and Node fixtures, each a minimal
  HTTP service. Native extensions, framework workloads, .NET, PHP and
  WebAssembly were not tested.
- **Dependency remediation:** the dependency fixture is one pure-Python
  module, not a real advisory fix.
- **Test registry only:** the local registry used HTTP. Delivered images are
  re-encoded OCI manifests, not preserved upstream signatures.

## Run the experiment

Requirements:

- Python 3, Docker, kind and kubectl
- access to the pinned public images
- an engine architecture for which every pinned image exists (tested on
  `linux/arm64`)

From the repository root:

```sh
PYTHONDONTWRITEBYTECODE=1 python3 integration-tests/live-v2/image_volumes.py \
  --work integration-tests/live-v2/run-image-volumes-01
```

Choose a **new** `run-*` directory for every invocation. The harness has these
isolation and cleanup guarantees:

- It creates a cluster, private kubeconfig and loopback-published registry that
  are unique to the invocation.
- It never uses the current kubectl context or an existing cluster.
- On success or failure, it removes the cluster, the registry container and the
  Docker tags it created.

The work directory keeps the generated fixtures, the kind configuration and
`result.json`. On failure, it also keeps `diagnostics.yaml` with Pods, events,
DaemonSets and Deployments from all namespaces. With upstream images already
cached locally, the final run took about 10 minutes from cluster creation to
the last check. The restart back-off phases took the most time.

See [When to use Brewlet](choosing-brewlet.md) for the broader tradeoff and
the Kubernetes
[image-volume task guide](https://kubernetes.io/docs/tasks/configure-pod-container/image-volumes/)
for native semantics.
