# Compatibility policy

Brewlet is pre-GA. Use the CLI, Helm chart, CRDs, operator, admission webhook,
node provisioner, shim and optional Maven plugin from the same release
(currently 0.8.1). An unchanged API version or file name does not mean that
components from different releases work together.

## Launch schema

`launch.json` uses `schemaVersion: 1`. Unknown fields, duplicate members and
`null` values are rejected. The optional top-level `runtimeImages` field is an
allowlist of runtime IDs the application accepts: omit it to accept any
approved ID, or set `[]` to deny all. It does not select the Pod's runtime.
See [the artifact contract](applications.md#the-artifact-contract).

## Runtime compatibility is a separate promise

A stable RuntimeImage ID promises compatible runtime line, ABI, executable
layout, numeric defaults, account/NSS behavior and documented tools. Use another
administrator-owned ID for an incompatible source or a separately governed
hold. The workload does not choose a runtime source digest directly.

Deleting and recreating an inventory name creates a new epoch. Old roots,
leases and minimum-generation barriers do not become valid under the new UID.

## Updating the platform

Brewlet does not promise in-place upgrade, downgrade or reuse of persisted
state across releases. The only supported transition is a clean reinstall of
the same release:

1. Drain the affected workloads.
2. Remove Brewlet through the [safe removal](uninstallation.md) procedure.
3. Install the matching components of the target release.

See [bounded transitions and recovery](supported-release.md#bounded-transitions-and-recovery)
for the status of each transition.

Rotating a runtime source within the same ID, or a reviewed Helm values
change, is not a platform-release upgrade. Unsupported old state never
justifies removing finalizers, leases or cleanup records by hand; complete
recovery obligations even when a host is missing. See
[verification and limitations](verification.md).
