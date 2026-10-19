# Explicit language recipes

These are producer recipes, not language modes in generic core. Compile/resolve
dependencies outside Brewlet, approve a compatible runtime ID and declare only
platforms on which your payload actually works. Test the selected source's
executable layout; no path below is inferred.

The current [qualification fixtures](https://github.com/microsoft/brewlet/tree/brewlet-2.0/integration-tests/fixtures) exercise
Java, Python, Node and .NET. The unchanged distroless matrix additionally covers
Java/Python/Node/static on arm64; it does not claim unchanged .NET qualification.

## Python

For the qualified distroless Python layout:

```json
{"schemaVersion":2,"process":{"entrypoint":["/usr/bin/python3.13"],"args":["/app/src/main.py"],"env":[{"name":"PYTHONPATH","value":"/app/dependencies"},{"name":"PYTHONDONTWRITEBYTECODE","value":"1"}]}}
```

```sh
brewlet push registry.example.com/apps/python:v1 --platform linux/arm64 \
  --launch launch.json --layer dependency=./dependencies:/app/dependencies \
  --layer app=./src:/app/src
```

Resolve compatible dependencies before packaging. A native wheel is not made
portable by repeating platform flags. The [local tutorial](local-kubernetes.md)
uses stdlib Python and needs no host Python package installation.

## Java

For a runtime providing `/usr/bin/java`, a thin-JAR/classpath descriptor can use:

```json
{"schemaVersion":2,"process":{"entrypoint":["/usr/bin/java"],"args":["-cp","/app/app.jar:/app/lib/*","example.Main"]}}
```

Package a directory containing `app.jar` at `/app` and dependency files at
`/app/lib`, or use the optional Maven producer described in
[application packaging](applications.md#optional-maven-producer). The plugin
owns Java-specific launch knowledge; it does not deploy Kubernetes resources.
Do not copy old `JVMConfig`, `jvm.args` or JavaApplication instructions.

Explicit JPMS/AppCDS/agent files and argv can be application recipes where
compatible, but core does not restore structured Java launch rewriting.

## Node.js

For the qualified distroless layout, `/nodejs/bin/node` is outside its ordinary
upstream PATH. Author it explicitly:

```json
{"schemaVersion":2,"process":{"entrypoint":["/nodejs/bin/node"],"args":["/app/src/main.js"],"env":[{"name":"NODE_PATH","value":"/app/dependencies/node_modules"}]}}
```

Resolve dependencies for the runtime/architecture outside Brewlet. For
applications whose module resolution depends on physical directory layout
instead of `NODE_PATH`, package that layout explicitly; no resolver is added.

## .NET

For a runtime explicitly providing `/usr/bin/dotnet`:

```json
{"schemaVersion":2,"process":{"entrypoint":["/usr/bin/dotnet"],"args":["/app/publish/Application.dll"]}}
```

Publish compatible framework-dependent application bytes into `publish/` and
package them at `/app/publish`. Select the matching approved runtime line.
Do not infer that a self-contained build needs the same runtime as a
framework-dependent deployment. Check actual source paths and ABI.

## Static/native binaries

Package the correct Linux binary at an explicit path such as `/app/bin/server`.
A compatible static root may have no shell, account files or PATH.
Dynamically linked native binaries additionally require a compatible loader
and libraries. A static artifact does not excuse platform declarations or
numeric process restrictions.

## Deploy and verify

All recipes use ordinary Kubernetes resources, the Brewlet RuntimeClass and an
explicit approved ID. Check HTTP/dependencies, Exec/probes, identity, writable
volumes and [resource limits](resources.md). A successful producer command is
not deployment proof.
