# 2.0 compatibility policy

Brewlet 2.0 is a source-built pre-GA evaluation. Use matching CLI, chart, CRDs,
operator/admission, provisioner and shim from one reviewed source revision.
The unchanged presence of an API version or filename does not promise arbitrary
mixed-revision operation.

## No 1.x compatibility layer

Main's old artifact/launch formats, `JavaApplication`, JDK selection labels,
JVM-specific APIs and removed CLI commands are not supported 2.0 interfaces.
Re-author application descriptors and native workload manifests under the
current contract. The [comparison](brewlet-2-comparison.md) explains equivalents;
it is revision-pinned history, not an in-place migration guarantee.

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

An ordinary same-contract runtime-source rotation or a reviewed Helm values
change is not a platform-release compatibility promise. Nor does unsupported
old state authorize dropping ownership, live-reference or cleanup evidence.

Preserve original recovery obligations even when replacing a missing host.
See [verification limits](verification.md).
