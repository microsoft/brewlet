# Pre-GA compatibility policy

Brewlet is pre-GA. The specification describes the contract implemented by the
current release, not a blanket promise that earlier Brewlet interfaces or state
will work with later releases.

## Brewlet interfaces and support decisions

Superseded Brewlet interfaces have **no automatic pre-GA compatibility
obligation**. This includes CLI aliases, API fields, configuration, artifact
formats, and alternate implementations. Prior existence alone is not a reason
to retain them.

Retaining an alternate interface or format requires a current use case and a
documented maintainer support decision in the relevant contract documentation,
linked to its reviewed issue or pull request, with its scope and limits.

Removing a superseded interface requires a reviewed change that identifies
affected users and operational impact, checks the exceptions below, and updates
the implementation, specification, tests, and user guidance together. Release
notes must identify incompatible changes and required operator actions.

There is no automatic pre-GA deprecation window or promise of mixed-version
operation, reuse of old Brewlet artifacts, rollback, or in-place release updates.
Any such promise must be explicit and bounded; an unchanged API version or
format name alone does not establish it.

## Release updates

**Safe teardown/reinstallation is the default for pre-GA release updates.**
Save reviewed configuration and workload manifests, drain or move workloads,
and complete the installed release's cleanup before installing the target
release. Follow [Upgrading](installation.md#upgrading), including its guidance
for retained resources and nodes that cannot be safely cleaned.

An in-place upgrade is supported only when a documented support decision names
the **source and target releases**, covered components and persisted state,
prerequisites, validation evidence, and recovery limits. Without that decision,
use teardown/reinstallation. There are no supported in-place release pairs.

Use matching release components and CRDs. Recreate manifests in the target
release's supported format; rebuild or republish artifacts when its contract
requires it. An in-place exception does not imply that downgrades work.

Current-release operations such as rotating JDK roots or changing reviewed Helm
values remain governed by their feature documentation. A `helm upgrade` command
used for configuration maintenance is not permission to replace Brewlet with
another release; keep the installed chart version and component choices pinned.

## Explicit compatibility exceptions

| Contract | Promise and boundary |
| --- | --- |
| [Capability-label contract v1](https://github.com/microsoft/brewlet/blob/main/specs/CAPABILITY_LABELS.md#compatibility-and-versioning) | Preserve the public scheduling key families and matching semantics within contract v1. Breaking changes require a new major capability-label contract, release notes, and a migration period publishing old and new keys together so autoscaler templates and workload policies can move safely. This is not a whole-installation in-place-upgrade guarantee. |
| External platform interoperability | Preserve interoperability required by the [supported platform prerequisites](installation.md#prerequisites) and the specification, including Kubernetes, OCI, containerd, and JVM contracts. The age of an external format is not a reason to remove support required by that matrix. |

## Safety

Refusing unsafe old state is a safety obligation, not a promise to run it.
Unsupported or unverifiable resources must fail explicitly rather than be
silently accepted, adopted by name, or treated as an empty installation.

Ownership fences, live-reference checks, integrity verification, and cleanup
obligations remain mandatory. Preserve evidence needed to recover or clean old
state even if the runtime no longer consumes its format. Do not delete in-use
files, abandon host state, bypass scheduling gates or finalizers, discard
ownership records, or enable stage GC on unverified installations. Blocked
cleanup requires the original installed components to finish safely or reviewed
node replacement, not forced deletion or adoption of unfenced hosts. Replacement
does not erase outstanding cleanup obligations.

Follow [Uninstall](installation.md#uninstall) for ordered teardown and
[runnable-stage cleanup](runnable-image.md#reclaiming-unused-stages) for
safe file reclamation.
