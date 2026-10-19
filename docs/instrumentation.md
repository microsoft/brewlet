# Application instrumentation

Brewlet does not auto-inject agents, detect language runtimes or reinterpret
tracing configuration. Instrumentation follows ordinary application/runtime
mechanisms under the same explicit descriptor and security contract.

## Choose a delivery mechanism

| Approach | Use when | Boundary |
| --- | --- | --- |
| Application SDK/framework integration | The application already supports tracing/metrics. | Dependencies are application bytes and need their own updates. |
| Agent packaged under `/app` | An explicit agent is application-specific. | Producer authors paths and invocation; packaged bytes remain read-only. |
| Agent in an approved runtime | The platform deliberately standardizes an agent for that ID. | Runtime compatibility and source rotation govern the shared bytes. |
| Reviewed volume | Environment-specific agent/trust material is required. | Explicit mount and runtime-compatible configuration; no hidden discovery. |

Use standard environment such as `OTEL_SERVICE_NAME`,
`OTEL_EXPORTER_OTLP_ENDPOINT` and `OTEL_RESOURCE_ATTRIBUTES` only where the
selected SDK/agent actually consumes them. These variables alone do not
instrument an otherwise uninstrumented process.

## Java example

A Java producer can explicitly author an entrypoint prefix such as
`["/usr/bin/java", "-javaagent:/app/agents/otel.jar", "-jar"]` and app arguments
such as `["/app/app.jar"]`. Package a reviewed agent separately under `/app`,
or deliberately use a compatible platform runtime with its own documented
agent path. Verify compatibility and overhead.

The optional Maven producer owns Java-specific argv knowledge; generic core
does not add `-javaagent`. A Pod `args` override replaces arguments and must
preserve the complete intended launch semantics.

## Python, Node and .NET

Use application SDKs or each ecosystem's documented explicit
launcher/environment/profiler mechanisms. Package dependencies or select a
runtime that provides them; Brewlet does not install packages at startup.
Prefer recipes that work non-root and without a shell.

## Verify before rollout

Confirm a real exported trace/metric reaches the intended collector, service
identity is correct, secrets are scoped and resource limits leave agent
headroom. An environment variable or an available collector is not export proof.
Choose agent/SDK versions through the application's own governance process.

See [resource behavior](resources.md), [certificate trust](registry-trust.md)
and [node/control-plane metrics](observability.md).
