# Security and hardening

Brewlet preserves runc container isolation. It is not gVisor, Kata, a shared
process model or an additional hostile-tenant boundary.

## Trust boundaries

| Boundary | Trusted authority | What an application cannot authorize |
| --- | --- | --- |
| Runtime source | Reviewed RuntimeImage, digest and inventory UID. | A Pod-supplied runtime digest or a label alone. |
| Host mutation | UID-bound NodeProfile, node claim and fenced provisioner. | A first-use request marker or generated worker edit. |
| Application publication | Verified OCI graph, strict launch/config mirror and `/app` confinement. | Runtime path replacement through app layers. |
| Initial/Exec process | Non-root numeric identity and protected initial state. | Root groups, capabilities, privilege escalation or a CRI PATH fallback. |
| Reclamation | Complete task, Exec and cross-process mount evidence. | A Pod list that merely appears empty. |

The provisioner is privileged and changes host containerd configuration.
Restrict who can approve runtime sources, create profiles and edit offering
pools. A minimally packaged but malicious approved runtime is still trusted
code; image minimalism does not replace approval.

## Immutable code, explicit writable data

Application/dependency files are confined to read-only, world-readable `/app`.
Never bake credentials into them. Mount scoped Secrets and separate writable
volumes. Runtime `/app` content is hidden, not inherited by the application.
The entire runtime-visible root is also required to be read-only. Admission
defaults and validates this; the shim refuses a writable root independently.
There is no writable-root escape hatch based on ordinary file permissions.

Workload volume/device destinations outside `/app/` require the immutable,
platform-owned `RuntimeImage.spec.volumeMountPaths` policy. Both read-only and
read-write overlays are constrained; symlink aliases and overlapping workload
mounts are rejected. Narrow CRI/system mount exceptions remain. Administrators
must designate data/configuration zones, not executable/library installations.
The protection excludes those intentionally mounted data/system zones and does
not prevent the application from executing its own code.
Dynamic-library environment and explicit volumes remain application behavior;
confinement does not sanitize malicious application code.

Optional account databases and PATH do not weaken numeric non-root enforcement.
Runtime set-ID binaries and file capabilities remain prohibited. Incompatible
sources need a reviewed derived image or a different runtime, not a policy bypass.

## Supply-chain scope

Digests provide integrity, not a statement about who built an image or whether
it is safe. Layer-set lock/SBOM/provenance binds dependency content and signer
policy. It does not automatically attest every final application.

The [governed composer](managed-composition.md) separately signs its exact
final image and preserves original application build inputs. Its verifier
distinguishes publisher evidence from release-composition signatures and
does not claim application tests passed or hidden code was discovered.

2.0 does not currently restore main's complete application Ratify integration.
Enforce any additional application admission/signature policy through separately
reviewed platform tooling; do not describe it as built into the shim.

## Operational checklist

- Review source provenance, platforms, numeric identity and compatibility IDs.
- Limit profile pools and component/recovery RBAC; separate developer publishing from runtime approval.
- Preserve runtime-owned PATH, non-root groups, no capabilities and no privilege escalation.
- Configure registry and application trust independently; do not disable TLS verification.
- Verify actual bindings and completed rollouts rather than desired targets.
- Keep startup fences until independently validated readiness.
- Preserve original identities, leases, journals and receipts during recovery.

Runtime updates only remediate runtime-root content. For libraries packaged
under `/app`, follow the [dependency update workflow](dependency-layer-sets.md).
For missing hosts and unresolved cleanup, use [safe removal](uninstallation.md).
