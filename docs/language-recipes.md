# Workload guides

These are producer recipes, not language modes in generic core. Compile/resolve
dependencies outside Brewlet, approve a compatible runtime ID and declare only
platforms on which your payload actually works. Test the selected source's
executable layout; no path below is inferred.

Each guide follows the same path: tested runtime inputs, payload build, launch
descriptor, packaging, native deployment, verification and qualification limits.
The launch previews below retain their existing section links; use the
dedicated guides for a complete workflow.

| Workload | Guide focus |
| --- | --- |
| [Java](java.md) | Thin JARs, Maven, classpath/module choices and managed dependencies. |
| [Python](python.md) | Import layout, resolved dependencies, interpreter ABI and read-only execution. |
| [Node.js](nodejs.md) | ES modules/CommonJS, dependency paths and native addons. |
| [.NET](dotnet.md) | Framework-dependent publishing, managed/native assets and runtime compatibility. |
| [WebAssembly](webassembly.md) | Explicit Wasmtime/Spin hosts, guest interfaces, capabilities and cache isolation. |

These are tested workload examples, not an exhaustive list of five built-in
runtime modes. The current [qualification fixtures](https://github.com/microsoft/brewlet/tree/brewlet-2.0/integration-tests/fixtures) exercise
Java, Python, Node and .NET. The unchanged distroless matrix additionally covers
Java/Python/Node/static on arm64; it does not claim unchanged .NET qualification.

## Python

Read the [complete Python guide](python.md). For the qualified distroless layout:

```json
{
  "schemaVersion": 2,
  "process": {
    "entrypoint": [
      "/usr/bin/python3.13"
    ],
    "args": [
      "/app/src/main.py"
    ],
    "workingDir": "/app",
    "env": [
      {
        "name": "PYTHONPATH",
        "value": "/app/dependencies"
      },
      {
        "name": "PYTHONDONTWRITEBYTECODE",
        "value": "1"
      }
    ]
  }
}
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

Read the [complete Java guide](java.md). For a runtime providing `/usr/bin/java`,
the fixture's thin-JAR/classpath descriptor uses:

```json
{
  "schemaVersion": 2,
  "process": {
    "entrypoint": [
      "/usr/bin/java"
    ],
    "args": [
      "--class-path",
      "/app/app.jar:/app/lib/*",
      "sh.brewlet.fixture.Main"
    ],
    "workingDir": "/app"
  }
}
```

Package a directory containing `app.jar` at `/app` and dependency files at
`/app/lib`, or use the optional Maven producer described in
[application packaging](applications.md#optional-maven-producer). The plugin
owns Java-specific launch knowledge; it does not deploy Kubernetes resources.
Do not copy old `JVMConfig`, `jvm.args` or JavaApplication instructions.

Explicit JPMS/AppCDS/agent files and argv can be application recipes where
compatible, but core does not restore structured Java launch rewriting.

## Node.js

Read the [complete Node.js guide](nodejs.md). For the qualified distroless layout,
`/nodejs/bin/node` is outside its ordinary
upstream PATH. Author it explicitly:

```json
{
  "schemaVersion": 2,
  "process": {
    "entrypoint": [
      "/nodejs/bin/node"
    ],
    "args": [
      "/app/src/main.mjs"
    ],
    "workingDir": "/app"
  }
}
```

The fixture preserves its relative ES-module import into `/app/dependencies`;
`NODE_PATH` is not a general ESM resolver. Resolve dependencies for the
runtime/architecture outside Brewlet and package the actual layout explicitly.

## .NET

Read the [complete .NET guide](dotnet.md). For a runtime explicitly providing
`/usr/bin/dotnet`, the fixture launch is:

```json
{
  "schemaVersion": 2,
  "process": {
    "entrypoint": [
      "/usr/bin/dotnet"
    ],
    "args": [
      "/app/App.dll"
    ],
    "workingDir": "/app"
  }
}
```

Publish compatible framework-dependent application bytes and preserve the
required managed/native assets. The complete fixture maps app and dependency
layers into `/app`; select the matching approved runtime line.
Do not infer that a self-contained build needs the same runtime as a
framework-dependent deployment. Check actual source paths and ABI.

## Static/native binaries

Package the correct Linux binary at an explicit path such as `/app/bin/server`.
A compatible static root may have no shell, account files or PATH.
Dynamically linked native binaries additionally require a compatible loader
and libraries. A static artifact does not excuse platform declarations or
numeric process restrictions.

## Deploy and verify

For explicit Wasm hosts on the same runc-backed path, see the
[Wasmtime and Spin guide](webassembly.md). The qualified pair is a Go WASIp1
command and a Rust/Spin SDK 7 HTTP component, not the failed Go/Wagi attempt
or a native SpinKube/runwasi backend.

All recipes use ordinary Kubernetes resources, the Brewlet RuntimeClass and an
explicit approved ID. Check HTTP/dependencies, Exec/probes, identity, writable
volumes and [resource limits](resources.md). A successful producer command is
not deployment proof.
