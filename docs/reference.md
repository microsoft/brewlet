# Contracts and glossary

This is a reader's map of the implemented contract. The
[specification](../specs/SPECIFICATION.md), current
CRDs and [CLI reference](cli-reference.md) remain authoritative.

## Launch descriptor

| Field | Contract |
| --- | --- |
| `schemaVersion` | Exactly 1. |
| `process.entrypoint` | Required nonempty argv prefix; no language detection. |
| `process.args` | Optional ordered arguments, not shell text. |
| `process.workingDir` | Optional canonical absolute path; default `/app`. |
| `process.env` | Optional ordered `{name,value}` entries with unique names; PATH forbidden. |
| `runtimeImages` | Optional top-level exact-ID allowlist; omitted means unrestricted at this gate, `[]` denies all; does not select the Pod runtime. |

Unknown/duplicate fields, invalid paths and config-mirror drift fail validation.
The final launch layer contains exactly one regular `launch.json`. The OCI
config must canonically mirror argv, Env and working directory, with empty User.
The manifest carries `brewlet.sh/artifact-version=1`.

Brewlet's artifact and launch versions are independent of OCI image manifests
and indexes, whose external `schemaVersion` remains `2`.

Application/dependency layers remain under `/app`; unsafe links, private
permissions, set-ID entries, devices and extended attributes are rejected. On
macOS, `brewlet build` ignores only the host-local `com.apple.provenance` and
`com.apple.quarantine` attributes, which are never written into layers.
Allowed whiteout/opaque semantics are validated as metadata, not ordinary files.
Explicit OCI platforms are producer assertions; they do not schedule Pods.

## Public workload annotations

| Annotation | Location | Meaning |
| --- | --- | --- |
| `brewlet.sh/runtime-image` | Pod metadata | Required explicit approved runtime ID. |
| `brewlet.sh/rotated-to-generation` | Pod/template metadata | JSON `{id,inventoryUID,generation}` minimum-generation barrier. |
| `brewlet.sh/restart-on-rotation` | Native workload controller metadata | Opt-in `"true"` when restart coordination is enabled cluster-wide. |

The barrier's generation is a positive integer and its epoch must match.
Duplicate/escaped duplicate JSON members are rejected; do not construct a
barrier from a mutable tag or a Kubernetes metadata generation.

Node capability labels and the startup fence are described in
[provider configuration](providers.md). Owner claims, runtime-binding
observations and revocation ledgers are protected infrastructure state, not
application inputs to patch.

## Failure boundaries

| Reason/category | Meaning |
| --- | --- |
| `UnsupportedArtifact`, `LaunchConfigMismatch`, `LayerPathViolation` | Invalid app graph, mirror or confinement. |
| `NoCompatibleRuntime` | No valid offered runtime matching the explicit request. |
| `RuntimeImageInstalling`, `RuntimeImageInstallFailed` | Cold publication pending or installation failed. |
| `RuntimePATHRequired` | Relative executable without a runtime PATH. |
| `RootNotSupported`, `PrivilegeEscalationNotSupported` | Prohibited process identity/security. |
| `ReadOnlyRootRequired` | A writable container root was requested or reached the shim. |
| `RuntimeMountNotAllowed` | Mount/device destination is unapproved, aliases runtime paths or overlaps another workload mount. |
| `InvalidRuntimeMountPolicy` | Invalid platform data/configuration mount declarations. |
| `RuntimeImageRevoked`, `RuntimeImageAuthorizationExpired` | New-start authority unavailable. |
| `RuntimeImageEpochMismatch`, `RuntimeImageUpdating`, `InvalidRuntimeGeneration` | Stale/invalid identity or generation boundary. |
| `ExecStateUnavailable` | Protected initial state is missing or unusable. |

Observe stable reasons separately from free-form details. Actual source,
webhook, controller and host failures may carry additional diagnostics.
[Troubleshooting](troubleshooting.md) supplies actions rather than a reason list alone.

## Host and protocol names

| Name/path | Purpose |
| --- | --- |
| `RuntimeClass/brewlet` | Selects containerd handler `brewlet`. |
| `io.containerd.brewlet.v2` | Containerd runtime identifier; not a product release selector. |
| `containerd-shim-brewlet-v2` | Conventional executable discovery name, not a 0.x compatibility layer. |
| `/opt/brewlet/runtimes` | Root-owned runtime generations, authority and task lease state. |
| `/var/lib/brewlet/layers` | Immutable verified application-layer cache. |

Do not expose or modify protected host state through workload mounts.
The executable's `-v2` is a Runtime v2 naming convention, not a claim that the
task wire protocol must be version 2.

## Glossary

**Artifact:** the application OCI graph, including launch metadata and payloads.
**Runtime ID:** administrator-owned compatibility name, not a version range.
**Inventory epoch:** the RuntimeImage's Kubernetes UID.
**Generation:** monotonic allocated target within that epoch.
**Current:** atomically published validated root/metadata, not every task's runtime.
**Authorization:** short-lived separately renewed local permission for new starts.
**Lease:** durable binding protecting a task's exact runtime generation.
**Capability:** scheduling information, not authorization by itself.
**Golden:** approved runtime image, not necessarily custom-built.
