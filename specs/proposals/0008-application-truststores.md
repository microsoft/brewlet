# Proposal 0008 - Per-application Java truststores

- **Status:** proposed; not implemented
- **Related roadmap item:** per-application truststores (workload delivery)
- **Current guidance:** [Custom CA certificates](../../docs/custom-ca-certificates.md)
- **Current API:** [`JavaApplicationSpec` and `JVMSpec`](../../kubernetes/api/v1alpha1/javaapplication_types.go)

This proposal adds a narrow truststore reference to `JavaApplication`, not a
general Pod-template escape hatch. It does not change shipped behavior or the
current [specification](../SPECIFICATION.md).

## 1. Summary and motivation

Platform-owned corporate roots can already be installed in a custom node JDK
image. That is the recommended shared-trust model, but application-specific
trust should not require a new JDK distribution or application artifact for each
service or environment.

Today the CRD accepts JVM flags but cannot mount the file those flags reference.
Application teams must manage raw Deployments to mount a truststore. Add an
optional `spec.jvm.trustStore` that references a prebuilt, same-namespace Secret
or ConfigMap. The controller mounts the selected key read-only and supplies the
standard JSSE system properties through the existing JVM-argument path.

## 2. Goals and non-goals

Goals:

- Keep application artifacts and shared node JDK roots unchanged.
- Support an explicit, application-owned replacement truststore.
- Preserve existing behavior when the field is absent.
- Use standard Kubernetes volume delivery, with no new privileged node work,
  ordinary-image init containers, or operator access to Secret contents.
- Make rotation, precedence, errors, and the public-root tradeoff explicit.

Non-goals for the first increment:

- Importing PEM certificates or merging them with the selected JDK's roots.
- Hot reload, automatic restart on Secret/ConfigMap changes, or CA issuance.
- Server identity, mutual-TLS private keys, PKCS11, or password-secret delivery.
- Registry or webhook TLS configuration.
- General `volumes`, `volumeMounts`, `initContainers`, or arbitrary mount paths.
- Forcing application code or custom TLS providers to use JSSE's default context.
  This is configuration convenience, not an enforceable egress security policy.

## 3. Proposed API

The following is illustrative proposed syntax, **not valid shipped configuration**:

```yaml
apiVersion: apps.brewlet.sh/v1alpha1
kind: JavaApplication
metadata:
  name: orders
spec:
  artifact:
    image: registry.example.com/demo/orders@sha256:<application-image-digest>
  jvm:
    version: 21
    distribution: temurin
    trustStore:
      secretKeyRef:
        name: orders-trust-v1
        key: truststore.p12
      type: PKCS12
      password: changeit
```

| Field | Proposed contract |
|---|---|
| `secretKeyRef` | Object with required `name` and `key`; Secret in the application's namespace. |
| `configMapKeyRef` | Alternative object with required `name` and `key`; ConfigMap in the same namespace. Binary stores normally use `binaryData`. |
| `type` | Required enum: `JKS` or `PKCS12`; no guessing from filenames or JDK defaults. |
| `password` | Required non-empty literal integrity password for a CA-only store; no default. Explicitly non-confidential. |

Exactly one source must be set. Selectors do not expose `namespace` or
`optional`: cross-namespace and optional references are unsupported. Validate
names and keys using Kubernetes' corresponding constraints. A key is a data
selector, never a filesystem path.

The store must contain trusted certificates only, not private-key entries.
The password is intentionally visible in the CR and generated JVM annotation;
it protects store integrity, not certificate confidentiality. Kubernetes access
control and the read-only mount protect delivery. Organizations that classify
even a CA-store password as confidential must not use this initial API.
Secret-backed password support requires a separate design that does not leak
the value into annotations, argv, or `JAVA_TOOL_OPTIONS` startup logs.

An explicit store replaces the JDK default trust. There is no implicit merge
or fallback if a connection is not trusted. Teams that need public roots must
include and maintain them in their supplied store. Absence of `trustStore`
retains today's behavior, including any user-supplied TLS flags.

## 4. Controller and runtime behavior

For each configured truststore, the generated Deployment would contain:

- One volume named `brewlet-truststore`, backed by the selected Secret or
  ConfigMap with `optional: false`.
- An `items` mapping from the selected key to the fixed filename `truststore`,
  exposing no other keys from that object.
- A read-only directory mount at `/etc/brewlet/trust`, without `subPath`, on
  the application container. Use file mode `0444` so the existing non-root
  workload UID can read the CA-only store.
- These arguments in the existing JSON-array `brewlet.sh/jvm-args` annotation:

```text
-Djavax.net.ssl.trustStore=/etc/brewlet/trust/truststore
-Djavax.net.ssl.trustStoreType=PKCS12
-Djavax.net.ssl.trustStorePassword=changeit
```

The last two values come from the descriptor. Preserve argument boundaries
through JSON serialization; do not shell-interpolate, join into an environment
variable, or interpret Kubernetes `$(VAR)` substitutions in these values.
Reserve the volume name and mount path for controller use.

Keep the existing user `jvm.args` and append the generated properties after
them. Reject explicit `jvm.args` assignments to any of these three properties
when `trustStore` is set, including bare `-Dproperty` forms, rather than silently
accepting two sources of truth. Apply equivalent CRD validation and controller
validation so admission availability is not the sole guard.

The shim already appends deployment arguments after artifact launch properties;
the Java launcher applies `JAVA_TOOL_OPTIONS` and `JDK_JAVA_OPTIONS` before its
command-line arguments. Thus the generated properties should win over ordinary
duplicate system properties in those sources. Verify that precedence for the
supported launcher paths before shipping; preserve existing environment-overlap
reporting. Application code can still construct a different TLS context.

No truststore bytes enter annotations, status, or the application image. No
node-root mutation or new shim mount mechanism is intended: preserve the
kubelet-supplied volume through the existing OCI mount path.

## 5. Validation and failure reporting

Schema/CEL and controller validation cover source exclusivity, selectors, type,
password presence, and conflicting JVM arguments. Controller validation failures
use the existing `Ready=False` / `ReconcileError` condition and event path; do not
publish a success-shaped result.

The controller does not fetch, cache, or watch the referenced object. This avoids
adding cluster-wide Secret read permissions and leaves namespace-scoped file
delivery to kubelet. Kubernetes events report missing objects/keys or mount
failures; with non-optional volumes, the new container must not start without the
file. The Deployment remains unready for that rollout; existing replicas may
continue serving during a failed rolling update.

Without reading and parsing the store, the controller cannot validate its
password, entries, or certificate validity. Invalid stores and TLS failures are
reported by the JVM/client when it initializes or uses trust, which may be after
startup. Never claim `Ready` proves a successful private-endpoint TLS handshake.
Application health checks and deployment verification must exercise that
dependency when it is a readiness requirement. Do not retry with default roots
or disable certificate verification on failure.

## 6. Rotation and lifecycle

Recommend versioned, immutable Secrets/ConfigMaps such as `orders-trust-v1`.
Build and verify the next store outside the Pod, create `orders-trust-v2` with
`immutable: true`, then change the reference in the JavaApplication. That changes
the Pod template and triggers an ordinary rolling update; no content hash,
Secret watch, or restart controller is needed.

Keep the old object until old replicas are gone and the rollback window has
closed. Rollback selects the old reference. Brewlet does not own or delete either
object.

Mutable objects are not rejected, but updating their contents alone does not
trigger a rollout. Kubelet can eventually update projected files while existing
Java TLS contexts retain cached trust, creating inconsistent behavior. A new
versioned reference is the supported deterministic rotation workflow; hot reload
is not promised.

Use overlapping old/new CA roots for planned migrations, then remove retired
roots in a second rollout. Do not silently add the latest node JDK public roots
to an application-owned store when a JDK is patched.

## 7. Implementation surfaces and acceptance criteria

The implementation must update the Go API and generated deepcopy, both deployed
and chart CRD copies, Deployment rendering and JVM argument validation, relevant
CLI/Maven manifest round-trips, and the user documentation/reference. Audit
update paths so unrelated edits preserve the new field. No new RBAC permissions
should be needed.

Required coverage before treating the proposal as shipped:

- CRD and controller rejection of both/no sources, invalid selectors/types,
  missing passwords, and duplicate truststore flags; an absent field preserves
  existing rendering and argument behavior.
- Exact volume source, single-key mapping, non-optional delivery, fixed path,
  read-only permissions, and generated arguments for both formats and sources.
- Argument boundaries for spaces/special characters, precedence against artifact
  properties and both JVM environment variables, and supported launchers.
- A live non-root Brewlet application connecting to a private-CA TLS endpoint;
  an unrelated CA and hostname mismatch still fail. Test the public-root
  replacement semantics as well as a deliberately combined store.
- Missing objects/keys prevent new-container startup; wrong passwords and corrupt
  stores produce visible client errors without trust fallback.
- Reference rotation creates new Pods using the new roots; rollback works;
  another application and the shared JDK truststore remain unchanged.
- Removing `trustStore` removes its generated mount/properties and returns to
  existing default/user-supplied configuration.

These are future acceptance tests, not evidence of current live support.

## 8. Alternatives and follow-ups

**Custom node JDK image:** remains the recommended fleet-wide approach. It
preserves centralized maintenance of public and corporate roots, but couples
trust changes to JDK provisioning.

**Generic volumes in the CRD:** more flexible but exposes a much broader Pod API
and leaves every application to wire paths and flags correctly. This proposal
deliberately starts with the narrow use case.

**PEM inputs merged at launch:** convenient, but requires store construction,
password handling, writable per-Pod state, and a clear policy for refreshing
JDK defaults. Ordinary-image init containers are unsupported by today's handler.
Defer this rather than hide runtime mutation inside the initial feature.

**Future confidential passwords or automatic rotation:** require separate
decisions about runtime delivery, logging, Secret access, restart ownership,
and availability during rotation. They must not be implied by this first API.
