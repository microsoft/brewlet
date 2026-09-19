# Observability & day‑2 operations

The runc-backed shim uses Kubernetes mechanisms for networking, logs, probes,
and resource accounting. This page covers those interfaces, the remaining
autoscaling validation limits, and Brewlet-specific day‑2 tasks.

See also [SPECIFICATION §12](https://github.com/microsoft/brewlet/blob/main/specs/SPECIFICATION.md).

---

## Networking

- **Normal pod IP via CNI** — runc owns the netns the kubelet/containerd provide.
- **Services, Ingress, NetworkPolicy** all behave exactly as for any pod.
- No special CNI configuration is required.

```bash
kubectl get pod <pod> -o wide          # real pod IP
kubectl expose deployment hello --port 80 --target-port 8080
```

---

## Logs

The JVM's stdout/stderr flow through containerd just like any container:

```bash
kubectl logs -l app=hello
kubectl logs -f <pod>
kubectl logs <pod> --previous          # after a restart
```

---

## Metrics & tracing

- **JMX, Micrometer, OpenTelemetry** work as usual — the JVM is a normal process in
  a normal sandbox.
- **JFR (Java Flight Recorder)** can be enabled via `jvm.args`
  (e.g. `-XX:StartFlightRecording=...`).
- **CPU HPA** requires Kubernetes resource metrics from metrics-server.
  [Live validation is scoped to the fixed candidate](#autoscaling).

These are application and Kubernetes resource signals. Brewlet's own
control-plane, launch-path, and node inventory telemetry is a separate,
default-off feature. See
[Runtime metrics and Grafana dashboards](runtime-metrics.md) for Helm
enablement, Prometheus scrape surfaces, the metric catalog, PromQL examples,
and the bundled dashboard.

```yaml
jvm:
  args:
    - "-XX:StartFlightRecording=filename=/tmp/app.jfr,dumponexit=true"
```

---

## Probes & exec

Probe execution and `kubectl exec` use the normal containerd/runc mechanisms:

- readiness / liveness / startup probes: `httpGet`, `tcpSocket`, `exec`;
- `kubectl exec` into the JVM sandbox, using tools installed in its runtime.

Ordinary-image ephemeral debug containers are not supported by the Brewlet
handler. Use a separate ordinary-runtime Pod when those tools are needed.

`brewlet:manifest` does not infer health probes from a port or framework.
Configure them explicitly for endpoints the application exposes; the following
Actuator example requires those health endpoints to be enabled:

```yaml
readinessProbe: { httpGet: { path: /actuator/health/readiness, port: 8080 } }
livenessProbe:  { httpGet: { path: /actuator/health/liveness,  port: 8080 } }
```

```bash
kubectl exec -it <pod> -- jcmd 1 VM.flags      # inspect the running JVM
```

---

## Signals & graceful shutdown

- `SIGTERM` is forwarded to the JVM (PID 1) → **shutdown hooks run**; Brewlet honors
  `terminationGracePeriodSeconds` and `preStop`.
- The `java` process's exit code is the container exit code, which drives the pod's
  `restartPolicy` (so `-XX:+ExitOnOutOfMemoryError` → clean restart).

---

## Autoscaling

The `JavaApplication` controller creates an `autoscaling/v1`
`HorizontalPodAutoscaler` for CPU utilization and preserves the HPA-owned
Deployment replica count. It requires metrics-server and CPU requests on the
workload. Brewlet's runtime exporter is not a replacement for that resource
metrics path.

Two fresh local arm64 clusters passed real CPU scale-up/down with a fixed-shim
candidate over 0.5.0 components. Unmodified 0.5.0 exposed a packed-layer GC
scale-out failure; the candidate fixes verified warm reuse, not cold startup
with missing source bytes. See [issue #94](https://github.com/microsoft/brewlet/issues/94)
and the [live-validation runbook](live-validation.md).
See [Autoscaling configuration](deploying-workloads.md#autoscaling) and use a
disposable evaluation cluster. Memory or custom-metric HPAs require separately
managed ordinary Deployments with `runtimeClassName: brewlet`, HPA resources
and the appropriate metrics providers; they are not
configured by `JavaApplication` or covered by that validation milestone.

---

## Day‑2: JDK upgrades

JDK roots on nodes are **versioned and additive**. The upgrade choreography — add a
new root, migrate workloads, retire the old root — is covered in detail in
[JDK management → Patching & upgrading](jdk-management.md#patching-upgrading-jdks).

Key properties:

- New roots install **additively**; running pods keep their JDK until they restart.
- Old roots are retained until no workload references them, then GC'd by the
  provisioner *(GC is a provisioner responsibility; today, retire by removing from
  the inventory and cleaning the node)*.
- Patching one node JDK patches every workload that uses it — centralized CVE
  management.

---

## Day‑2: multi-arch fleets

- Install one JDK root per node architecture (amd64/arm64).
- The **OCI artifact is arch-independent** — the same artifact runs on any
  provisioned arch, so multi-arch is transparent to developers.
- The provisioner image and shim are compiled per-arch; use the multi-arch
  `*-image-push` build targets. See [JDK management → multi-arch](jdk-management.md#architecture-mapping-multi-arch).
- **Non-portable (JNI) JARs** and arch-coupled accelerators (AppCDS archives) are the
  exception — see the [multi-arch note](multi-arch.md) for the optional `arch`
  scheduling constraint.

---

## Watching the fleet

```bash
# Node readiness and operator state:
kubectl get nodes -L brewlet.sh/runtime
kubectl get node <n> -o jsonpath='{.metadata.annotations.brewlet\.sh/provision-state}{"\n"}'

# What each node offers:
kubectl get node <n> -o jsonpath='{.metadata.annotations.brewlet\.sh/jdks}{"\n"}'
kubectl get node <n> -o jsonpath='{.metadata.annotations.brewlet\.sh/launchers}{"\n"}'

# Operator/provisioner events:
kubectl get events --field-selector reason=NodeReady
kubectl get events --field-selector reason=ProvisionFailed

# Component health:
kubectl get pods -n brewlet
```

The operator's health/readiness endpoint is available at
`--health-probe-bind-address` (default `:8081`). Its Prometheus listener, the
admission listener, and the node exporter are enabled through
`metrics.enabled`; see
[Runtime metrics and Grafana dashboards](runtime-metrics.md).

## Next steps

- **[Troubleshooting](troubleshooting.md)** — when something's wrong.
- **[Security](security.md)** — hardening and supply chain.
- **[Runtime metrics and Grafana dashboards](runtime-metrics.md)** — operate the
  shipped Brewlet-specific telemetry.
