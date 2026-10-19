# Concepts and architecture

Brewlet makes the runtime infrastructure, like the OS or Kubernetes. The
application remains the workload. Sharing immutable files does not share a
process, heap, writable filesystem or resource limit.

## The five decisions

| Decision | Authority | Result |
| --- | --- | --- |
| What to execute | Application producer | Explicit `launch.json`, app/dependency layers and platform declarations. |
| Which runtime bytes are approved | Platform administrator | A digest-pinned `RuntimeImage` and default numeric identity. |
| Where that runtime is offered | Platform administrator | `NodeProfile` pool selection, installation mode and lifecycle policy. |
| Where a workload runs | Kubernetes scheduler | Native workload constraints combined with admitted runtime capabilities. |
| Which generation actually executes | Fenced node provisioner and shim | Validated root, local authorization and a durable per-container lease. |

An ID is a compatibility promise, not a version solver. Neither the CLI nor
the shim detects a language or infers an interpreter. A runtime image's
entrypoint is not an alternative application descriptor.

## From publication to a running process

```text
Producer -> registry: application OCI image and explicit platforms
Administrator -> RuntimeImage: approved source digest and numeric UID/GID
Administrator -> NodeProfile: offer that ID on selected pools
Workload -> admission: RuntimeClass + explicit runtime ID
Admission -> scheduler: preserve author constraints and add runtime capability
Provisioner -> node: validate, authorize and publish an immutable generation
containerd -> Brewlet shim -> runc: lease runtime, isolate /app, run explicit argv
```

Kubelet pulls the application image. The provisioner acquires runtime sources.
The shim verifies application content and resolves trusted local runtime state;
it does not pull runtime images or resolve dependencies.

## Cold first use

An OnDemand-only node can advertise a validated baseline and an `available`
runtime without fetching that root. First use creates a private request marker;
the authorized provisioner installs the root. Create waits for a bounded
interval and fails explicitly if publication has not completed. A marker is
not authority, and a failed request does not authorize repeated marker creation.
Kubelet retries remain ordinary Kubernetes behavior.

Eager instead installs every required root before aggregate readiness.
The [startup fence](providers.md) prevents scheduling through incomplete
provisioning, including when a provider has pre-populated readiness labels.

## Rotation is not a process transplant

```text
Approved ID: digest A -> digest B
Old task:   lease A --------------------------> normal drain
New task:              lease B --------------> normal drain
Old root:  retained until task, Exec and mount references are gone
```

Opted-in native controllers may receive a minimum-generation barrier and a
template update. A patched template is not proof of completed remediation.
See [rotation and cleanup](operations.md).

## Boundaries

Application and dependency bytes live under read-only `/app`. Runtime `/app`
content is hidden, not merged into that prefix. Explicit volume mounts can
provide writable data or secrets. Runtime and application caches are immutable.

Non-root identity, no capabilities, no privilege escalation, runtime-owned PATH
and protected initial state apply to initial processes, Exec, probes and hooks.
This is runc isolation, not a separate hostile-tenant sandbox.

[Try a complete local deployment](local-kubernetes.md) or
[decide whether Brewlet fits](choosing-brewlet.md).
