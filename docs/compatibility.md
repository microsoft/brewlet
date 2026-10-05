# Pre-GA compatibility policy

Brewlet is pre-GA. The specification describes the contract implemented by the
current release, not a blanket promise that earlier Brewlet interfaces or state
will work with later releases. This policy does not remove any feature or weaken
any operational safeguard.

## Brewlet interfaces and support decisions

Superseded Brewlet interfaces have **no automatic pre-GA compatibility
obligation**. This includes CLI aliases, API fields, configuration, artifact
formats, and alternate implementations. Prior existence alone is not a reason
to retain them.

Retaining an alternate interface or format requires a current use case and a
documented maintainer support decision in the relevant contract documentation,
linked to its reviewed issue or pull request. Record the use case, supported
scope, authoritative contract, limits, and review or removal conditions.
Describing an existing code path is not itself a cross-release support decision.

Removing a superseded interface requires a reviewed change that identifies
affected users and operational impact, checks the exceptions below, and updates
the implementation, specification, tests, and user guidance together. Release
notes must identify incompatible changes and required operator actions.
Feature-level removals are separate from adoption of this policy.

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
use teardown/reinstallation. Generic migration instructions, successful Helm
rendering, or an available recovery code path are not a release-pair exception.
This policy introduces no supported in-place release pairs.

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

For example, containerd configuration formats 2 and 3 use different plugin
namespaces; supporting the former does not mean supporting containerd 1.x.
Brewlet still requires containerd 2.0+ and cgroup v2. Conditional requirements,
such as scheduling-gate support for ownership migration, also remain in force.

Add future exceptions to the relevant contract and link them here with their
scope and support decision. Do not silently broaden an exception into a general
compatibility promise.

## Safety is not old-format support

Refusing unsafe old state is a safety obligation, not a promise to run it.
Unsupported or unverifiable resources must fail explicitly rather than be
silently accepted, adopted by name, or treated as an empty installation.

Ownership fences, live-reference checks, integrity verification, and cleanup
obligations remain mandatory. Preserve evidence needed to recover or clean old
state even if the runtime no longer consumes its format. Do not delete in-use
files, abandon host state, bypass migration gates or finalizers, discard
ownership records, or acknowledge stage-GC migration before retiring unguarded
consumers. A blocked cleanup requires investigation and recovery, not forced
deletion. See [Uninstall](installation.md#uninstall) and
[runnable-stage cleanup](runnable-image.md#reclaiming-unused-stages).

Describe current capabilities by function (for example, standalone provisioning,
in-place SIGHUP activation, or Prometheus textfile collection). Reserve historical
terms such as "legacy" for precise old-state or migration descriptions; neither
that label nor its removal decides whether an interface is supported.
