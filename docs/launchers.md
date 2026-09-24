# Launchers

A launcher is the program that fronts the Java entrypoint. Brewlet always
provides the selected JDK's stock `java` launcher; administrators may also stage
explicit launcher binaries such as
[`jaz`](https://learn.microsoft.com/java/jaz/overview).

Related: [Resource requests, limits and JVM tuning](resource-tuning.md) ·
[JDK management](jdk-management.md) · [Security](security.md).

---

## `java` is implicit

When a workload omits its launcher or requests `java`, Brewlet executes:

```text
<selected-java-home>/bin/java
```

Do not add `java` to `spec.launchers`. It comes from every declared JDK and is a
reserved launcher name.

Use `brewlet k8s launcher list` to aggregate optional launchers advertised by
nodes, or `--output wide` for per-node rows. The
[`launcher add` workflow](cli-reference.md#safe-jdk-and-launcher-additions)
updates the live NodeProfile by default. `--dry-run` validates locally and prints
the proposal; `--dry-run=server` validates through the API server without saving.
If validation fails, no rendered output is printed. Client dry-run mode also
supports offline `--file` and `--values` inputs. The CLI never installs a
launcher directly onto a node; the operator provisions it after a profile update.

## Launcher names are tokens, not paths

A launcher name identifies a node-installed launcher layer directory
(`/opt/brewlet/launchers/<name>/`), so it must be a lowercase DNS-1123 token: up
to 54 letters, digits and dashes, starting and ending with a letter or digit.
Path separators, `..`, wildcards, surrounding whitespace, absolute paths and
uppercase are rejected — in `spec.jvm.launcher`, in the `brewlet.sh/launcher`
annotation, in `brewlet run --launcher`, and again on the node. The same rule
applies to `spec.jvm.distribution`. The shim additionally requires the name to
appear in the node's active inventory and verifies that the selected directory
really is inside the configured runtime roots before mounting it.

```yaml
jvm:
  version: 21
  launcher: java
  args:
    - "-XX:MaxRAMPercentage=75.0"
    - "-XX:+UseZGC"
```

Brewlet injects no JVM tuning flags. With stock `java`, the deployment's
`jvm.args` owns heap, GC, agent, and other runtime tuning.

---

## Helm example: `jaz`

Launchers are structured sources. Each entry requires a lowercase `name`, a
fully qualified tagless SHA-256 digest reference, and an absolute binary path
inside the image:

```yaml
provisioner:
  launchers:
    - name: jaz
      source:
        image: mcr.microsoft.com/openjdk/jdk@sha256:bfde2ed613f4c67c112d1592452575d3a1dc9ce5f7d75821bb7752aa786fa575
        path: /usr/bin/jaz
```

The Microsoft Build of OpenJDK image in this editable example contains
`/usr/bin/jaz`. Review and update the digest according to your patch policy.

The provisioner pulls and mounts the source image through host containerd,
rejects a missing source file or a symlink in any source-path component, and
copies it to:

```text
/opt/brewlet/launchers/jaz/bin/jaz
```

It does not run the source image, grant it host networking, or expose a writable
host bind mount. The exact image and source path are recorded in
`/opt/brewlet/launchers/jaz/.brewlet-source`. An atomic
`/opt/brewlet/launchers/.brewlet-active` inventory prevents undeclared stale
launcher roots from being selected.

---

## Arbitrary launchers

Brewlet does not maintain a launcher catalog or special launcher allowlist. Any
launcher can use the same shape:

```yaml
apiVersion: node.brewlet.sh/v1alpha1
kind: NodeProfile
metadata:
  name: custom-launcher
spec:
  jdks:
    - distribution: temurin
      feature: 21
      source:
        image: docker.io/library/eclipse-temurin@sha256:85f00967bcc624fc19fa9c2cf124ea426a5363898e267141726f31f358c2e14b
        javaHome: /opt/java/openjdk
  launchers:
    - name: acme-java
      source:
        image: registry.example.com/java/acme-launcher@sha256:<64-lowercase-hex>
        path: /usr/local/bin/acme-java
```

An arbitrary launcher must:

- be a regular, non-symlink file at `source.path`;
- be usable as an executable Linux program on every selected node architecture;
- accept Brewlet's Java-style argument ordering; and
- find the selected JVM through `JAVA_HOME` when it does not embed one.

The readiness gate checks that the staged launcher exists and is executable. It
does not execute arbitrary administrator-provided launcher binaries, because
Brewlet cannot assume a universal safe probe argument. Test launcher/JDK
compatibility in your own image qualification pipeline.

If the launcher needs shared libraries not present in the JDK image, package
them into an image layout compatible with your launcher or use a statically
linked launcher.

---

## Requesting a launcher

In a `JavaApplication`:

```yaml
spec:
  jvm:
    version: 21
    launcher: jaz
```

Or on a raw pod:

```yaml
metadata:
  annotations:
    brewlet.sh/launcher: "jaz"
spec:
  runtimeClassName: brewlet
```

The admission webhook rejects a request with `NoCompatibleLauncher` when no
ready node advertises that launcher. Installed nodes expose:

```bash
kubectl get node node-1 \
  -o jsonpath='{.metadata.annotations.brewlet\.sh/launchers}{"\n"}'
# java,jaz
```

Brewlet pins `JAVA_HOME` to the selected node JDK before invoking a custom
launcher. Launcher arguments precede artifact launch knobs and JVM/application
arguments:

```text
<launcher> <launcher.args...> <artifact launch knobs> <jvm.args...> -jar /app/app.jar
```

Use `jaz` when you want its resource-aware ergonomics. Use stock `java` when you
need explicit JVM tuning or do not want an additional node launcher source.
