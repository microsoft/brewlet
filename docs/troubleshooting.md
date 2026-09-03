# Troubleshooting

A field guide to what can go wrong, what it looks like, and how to fix it. The
failure-mode summary is from [SPECIFICATION §14](https://github.com/microsoft/brewlet/blob/main/specs/SPECIFICATION.md).

---

## Failure modes at a glance

| Scenario | Behavior you'll see | Where to look |
|---|---|---|
| No compatible JDK on any ready node | Pod stays `Pending`; event `NoCompatibleJDK`; scheduler skips nodes | [→ JDK issues](#pod-is-pending-with-nocompatiblejdk) |
| Requested launcher not installed | Pod stays `Pending`; event `NoCompatibleLauncher`; scheduler skips nodes | [→ launcher issues](#pod-is-pending-with-nocompatiblelauncher) |
| OCI artifact missing/unauthorized | `ImagePull`-style failure on the pod | [→ artifact pull](#imagepull-style-failure) |
| JVM OOM | `ExitOnOutOfMemoryError` → exit → kubelet restart | [→ OOM](#pod-restarts-oomkilled) |
| Node provisioning fails | Node not labeled `ready`; condition/event `ProvisionFailed` | [→ provisioning](#node-never-becomes-ready) |
| NodeProfile is invalid | `Ready=False`, reason `InvalidProfile`; profile DaemonSet is absent | [→ source policy](#nodeprofile-source-policy-failures) |
| Shim crash | containerd reports task failure; pod restarts | [→ shim](#task-shim-failures) |
| cgroup v1-only node | Provisioner refuses; node not marked ready | [→ provisioning](#node-never-becomes-ready) |
| containerd 1.x node | Provisioner refuses; node not marked ready | [→ provisioning](#node-never-becomes-ready) |

---

## Node never becomes ready

**Symptom:** `kubectl get nodes -L brewlet.sh/runtime` shows no `ready`, or
`brewlet.sh/provision-state=Failed`.

```bash
# Is the node opted in?
kubectl get node <n> -o jsonpath='{.metadata.labels.brewlet\.sh/provision}{"\n"}'
# Provisioner pod on that node:
kubectl get pods -n brewlet -o wide | grep <n>
kubectl logs -n brewlet <provisioner-pod>
kubectl get events --field-selector reason=ProvisionFailed
kubectl get node <n> -o jsonpath='{.metadata.annotations.brewlet\.sh/provision-error}{"\n"}'
```

**Common causes & fixes:**

- **cgroup v1-only node** — the provisioner refuses it (cgroup v2 is required). Move
  the node to a cgroup v2 kernel/config, or exclude it.
- **containerd 1.x node** — the provisioner reports
  `unsupported-containerd-version`; upgrade the server to containerd 2.0 or
  newer. Config file `version = 2` remains supported on containerd 2 and is not
  the containerd server version.
- **Runtime source preflight failed** — a required indexed entry is missing, an
  image is not a canonical SHA-256 digest reference, a path/name is malformed,
  an entry is duplicated, or a mirror is malformed/unapproved. The provisioner
  exits before pulling or mounting source content. See
  [NodeProfile source-policy failures](#nodeprofile-source-policy-failures).
- **JDK copy-from-image failed** — the node can't reach the verified digest-pinned
  JDK image. Verify the containerd socket mount and image pull access, or mirror
  the exact digest through an approved destination. See
  [JDK management](jdk-management.md#source-model).
- **can't reach the registry / socket** — verify the containerd
  socket mount and that the node can pull the JDK image.
- **JDK or launcher validation failed** — `brewlet.sh/provision-error`
  identifies the failed component. Confirm each configured JDK has an executable
  `<javaHome>/bin/java` that passes `java -version`; confirm each launcher source
  is a regular non-symlink file and the staged copy is executable. Arbitrary
  launchers are not executed as readiness probes. A failure removes stale
  runtime and capability labels.
- **containerd config validation failed** — in `validated` mode, Brewlet runs
  `containerd config dump` and requires the exact `brewlet` handler before
  activation. Inspect the provisioner log and the effective host configuration;
  Brewlet restores/removes the rejected render automatically.
- **containerd restart or health check failed** — validated mode restores the
  prior primary config or drop-in, restarts containerd, and verifies recovery.
  Reasons such as `restart-failed`, `containerd-health-check-failed`, or
  `runtime-handler-health-check-failed` indicate the failed stage;
  `rollback-failed` means automated recovery also failed and the node needs
  immediate operator attention.
- **RBAC** — the provisioner needs `get`/`patch` on nodes to label them; confirm the
  ServiceAccount/ClusterRole from the manifest/chart are present.

---

## NodeProfile source-policy failures

The admission webhook validates sources and mirrors for immediate feedback, but
the reconciler repeats the same checks. A stored invalid profile therefore fails
closed even if admission is disabled or bypassed: its provisioner DaemonSet is
withheld/deleted and only that profile's owned node advertisements are removed.
Repair an invalid profile before deleting it if you need automatic host
reversal. Its current pool selector is untrusted and may overlap another
profile, so deletion while `InvalidProfile` deliberately skips the cleanup
DaemonSet rather than risk deprovisioning another profile's nodes. The operator
still waits for that profile's provisioner and cleanup pods to terminate before
withdrawing advertisements one final time and releasing the finalizer.

```bash
kubectl get nodeprofile <name> \
  -o jsonpath='{range .status.conditions[*]}{.type}{"="}{.status}{" reason="}{.reason}{" message="}{.message}{"\n"}{end}'
kubectl describe nodeprofile <name>
kubectl logs -n brewlet deploy/brewlet-operator
```

Correct the condition message:

- replace every tagged `spec.jdks[].source.image` with a fully qualified,
  tagless `repository@sha256:<64 lowercase hex>` reference;
- supply `source.image` and `source.javaHome` for every JDK;
- supply `name`, `source.image`, and `source.path` for every optional launcher;
- add the exact mirror destination host, including any explicit port, to
  `security.allowedSourceMirrorHosts`, or remove the mapping; and
- remove duplicate/self mirror mappings and conflicting named-pool ownership.

To inspect a provisioner source declaration directly:

```bash
/opt/brewlet-dist/brewlet-source-policy validate-ref \
  --image docker.io/library/azul-zulu@sha256:2e230d906cffcc7bb7360ce82836f2ff0e0be74a1d5ebaf929e4e6ac99d61bf2
/opt/brewlet-dist/brewlet-source-policy validate-path \
  --path /usr/lib/jvm/zulu21
```

An air-gapped mirror must preserve the referenced OCI manifest/index bytes and
digest.

---

## Pod is Pending with `NoCompatibleJDK`

**Symptom:** the pod won't schedule; a `NoCompatibleJDK` event is recorded.

The pod requested a JDK (`spec.jvm.version` via `JavaApplication`, or raw pod
annotation `brewlet.sh/jdk`) that no ready node advertises.

```bash
# What does the pod ask for?
kubectl get pod <pod> -o jsonpath='{.metadata.annotations.brewlet\.sh/jdk}{"\n"}'
# What do nodes offer?
kubectl get nodes -o custom-columns=NODE:.metadata.name,JDKS:.metadata.annotations.brewlet\\.sh/jdks
```

**Fix:** either request a JDK the fleet has, or add the JDK to the inventory:

```yaml
provisioner:
  jdks:
    - distribution: temurin
      feature: 21
      source:
        image: docker.io/library/eclipse-temurin@sha256:<reviewed-digest>
        javaHome: /opt/java/openjdk
```

Apply the values with `helm upgrade ... -f values.yaml`, wait for
`brewlet.sh/jdks` to update, and re-deploy.

---

## Pod is Pending with `NoCompatibleLauncher`

Same as above, for launchers. The pod requested `spec.jvm.launcher` or
`brewlet.sh/launcher: <name>` that no ready node has.

```bash
kubectl get nodes -o custom-columns=NODE:.metadata.name,LAUNCHERS:.metadata.annotations.brewlet\\.sh/launchers
```

**Fix:** add a structured launcher entry with `name`, digest-pinned
`source.image`, and absolute `source.path`, then re-provision; or drop the
request to use the vanilla `java` launcher (omit the annotation). See
[Launchers](launchers.md#helm-example-jaz).

---

## ImagePull-style failure

**Symptom:** the pod can't fetch the OCI image.

- **Wrong ref / not pushed** — verify the image exists:
  `oras manifest fetch <ref>` (or `brewlet inspect <ref>` for the local layout).
- **Unauthorized** — add/verify `imagePullSecrets` (or `artifact.pullSecrets` in the
  `JavaApplication`).
- **Not a Brewlet artifact** — confirm it was pushed with the brewlet media types
  ([Reference](reference.md#oci-media-types)), not as a plain image.

---

## Pod restarts / OOMKilled

**Symptom:** the pod restarts under load or is `OOMKilled`.

- The heap likely has no non-heap headroom. Set
  `-XX:MaxRAMPercentage=75.0` (not 100%) so Metaspace/threads/code-cache/direct
  buffers fit under `limits.memory`. See [Resource requests, limits & JVM tuning](resource-tuning.md#memory-leave-headroom-for-non-heap).
- Add `-XX:+ExitOnOutOfMemoryError` so an OOM is a clean restart, not a hang.
- Or switch to `jaz`, which sizes the heap from the cgroup automatically.
- Raise `limits.memory` if the workload genuinely needs more.

---

## Task / shim failures

**Symptom:** containerd reports a task failure; the pod restarts or won't start.

```bash
kubectl describe pod <pod>                 # events from kubelet/containerd
# On the node:
journalctl -u containerd | grep -i brewlet
ls -l /opt/brewlet/bin/containerd-shim-brewlet-v2
grep -A2 runtimes.brewlet /etc/containerd/config.toml
test ! -f /etc/containerd/config.toml.d/99-brewlet.toml ||
  cat /etc/containerd/config.toml.d/99-brewlet.toml
containerd --config /etc/containerd/config.toml config dump | grep -A4 runtimes.brewlet
```

- **Shim not on containerd's PATH** — confirm it's installed to `/usr/local/bin`
  and the effective config dump contains the `runtimes.brewlet` block. In
  validated mode the provisioner restarts and probes the handler itself; use its
  logs and `brewlet.sh/provision-error` rather than manually replacing config.
- **JDK root missing** — confirm `/opt/brewlet/jdks/<dist>-<feature>/bin/java`
  exists on the node.
- **Arch mismatch** — the shim is arch-specific; ensure the provisioner image matches
  the node arch (the build compiles it per-arch).

---

## Webhook / admission problems

**Symptom:** compatibility hints aren't stamped, or scheduling isn't steered.

- With `admission.failurePolicy: Ignore` (default), a webhook outage silently lets
  pods through **without** stamping/steering — check the webhook is healthy. If
  the shim still cannot resolve the workload image from containerd metadata, it
  fails closed rather than guessing:
  ```bash
  kubectl get pods -n brewlet -l app=brewlet-admission
  kubectl logs -n brewlet -l app=brewlet-admission
  ```
- **caBundle/cert mismatch** after a `helm upgrade` should self-heal (Helm rotates
  cert + `caBundle` together). In production use cert-manager. See
  [Configuration](configuration.md#admission-webhook).
- Non-brewlet pods are intentionally passed through untouched.

---

## Local development issues

- **The harness cannot find a component directory** — run it from a complete
  `microsoft/brewlet` monorepo checkout. For external component checkouts, set
  `BREWLET_CORE_DIR` and `BREWLET_KUBERNETES_DIR`.
- **Tier 2 cannot build a fixture** — check `JAVA_HOME` points at a full JDK 21+.
- **Core or Kubernetes build fails** — Go 1.26+ is required.
- **Tier 3 skips or fails before runc** — Docker must be installed and reachable;
  the tier runs a privileged Linux container.
- **Non-Linux dev host** — only the portable bundle-assembly core builds locally;
  use integration-test tier 3 to exercise the Linux/runc path. See
  [Getting started](getting-started.md).

---

## Still stuck?

- Re-read the relevant guide: [Installation](installation.md),
  [Configuration](configuration.md), [JDK management](jdk-management.md),
  [Launchers](launchers.md), [Resource requests, limits & JVM tuning](resource-tuning.md).
- The provisioner entrypoint is **idempotent** — safe to let it re-run after you fix
  node state.
- Component logs: `kubectl logs -n brewlet <operator|admission|provisioner pod>`.
