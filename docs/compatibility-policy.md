# Compatibility policy

Brewlet is pre-GA. Use matching CLI, chart, CRDs, operator/admission,
provisioner and shim from one release (or one source revision).
The unchanged presence of an API version or filename does not promise arbitrary
mixed-version operation.

## Launch schema

The current launch schema also rejects earlier rewrite snapshots' `requires`
wrapper. Optional `runtimeImages` is now a top-level field, with omission
unrestricted at this gate and an explicit `[]` denying all. The unchanged
`schemaVersion: 1` is not a compatibility alias for the removed wrapper.

## Runtime compatibility is a separate promise

A stable RuntimeImage ID promises compatible runtime line, ABI, executable
layout, numeric defaults, account/NSS behavior and documented tools. Use another
administrator-owned ID for an incompatible source or a separately governed
hold. The workload does not choose a runtime source digest directly.

Deleting and recreating an inventory name creates a new epoch. Old roots,
leases and minimum-generation barriers do not become valid under the new UID.

## Updating the platform

There is no general promise of cross-release in-place upgrade, downgrade,
old persisted-state reuse or a stable release distribution. A supported path
must explicitly name the source/target revisions, state and recovery limits.
Otherwise plan reviewed workload drain and [safe teardown](uninstallation.md)
before installing matching new components.
The [prerelease support table](supported-release.md#bounded-transitions-and-recovery)
names the initial eligible clean-reinstall test pair and its unverified status.

An ordinary same-contract runtime-source rotation or a reviewed Helm values
change is not a platform-release compatibility promise. Nor does unsupported
old state authorize dropping ownership, live-reference or cleanup evidence.

Preserve original recovery obligations even when replacing a missing host.
See [verification limits](verification.md).
