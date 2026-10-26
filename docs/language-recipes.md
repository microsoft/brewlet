# Workload guides

Brewlet has no built-in language modes. For every language you:

1. Build your program and resolve its dependencies with your usual tools.
2. Write a `launch.json` with explicit executable and file paths.
3. Package and push with `brewlet build` and `brewlet push`, declaring only
   platforms on which your files actually work.
4. Deploy with an approved runtime ID that provides the executable you named.

Brewlet infers no paths. Check that the executable path in your launch
descriptor exists in the runtime image you select.

This page shows a launch descriptor for each language. Use the dedicated guides
for a complete workflow.

| Workload | Guide focus |
| --- | --- |
| [Java](java.md) | Thin JARs, Maven, classpath/module choices and managed dependencies. |
| [Python](python.md) | Import layout, resolved dependencies, interpreter ABI and read-only execution. |
| [Node.js](nodejs.md) | ES modules/CommonJS, dependency paths and native addons. |
| [.NET](dotnet.md) | Framework-dependent publishing, managed/native assets and runtime compatibility. |
| [WebAssembly](webassembly.md) | Explicit Wasmtime/Spin hosts, guest interfaces, capabilities and cache isolation. |

Other languages work the same way. The tested
[fixtures](https://github.com/microsoft/brewlet/tree/main/integration-tests/fixtures)
cover Java, Python, Node.js and .NET. Unmodified distroless runtime images are
also tested for Java, Python, Node.js and static binaries on arm64. For
dependency layer sets shared across applications, see
[managed composition](managed-composition.md).

## Python

Read the [complete Python guide](python.md). For a distroless Python runtime image:

```json
{
  "schemaVersion": 1,
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
brewlet build registry.example.com/apps/python:v1 --platform linux/arm64 \
  --launch launch.json --layer dependency=./dependencies:/app/dependencies \
  --layer app=./src:/app/src
brewlet push registry.example.com/apps/python:v1
```

Resolve compatible dependencies before packaging. A native wheel is not made
portable by repeating platform flags. The [local tutorial](local-kubernetes.md)
uses stdlib Python and needs no host Python package installation.

## Java

Read the [complete Java guide](java.md). For a runtime providing `/usr/bin/java`,
a thin-JAR classpath descriptor is:

```json
{
  "schemaVersion": 1,
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
generates the Java launch arguments; it does not deploy Kubernetes resources.

JPMS modules, AppCDS archives and Java agents work when you package the files
and write the matching JVM arguments in `launch.json`. Brewlet does not rewrite
Java launch arguments for you.

## Node.js

Read the [complete Node.js guide](nodejs.md). In the distroless Node.js image,
`/nodejs/bin/node` is not on the default PATH, so name it explicitly:

```json
{
  "schemaVersion": 1,
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

This example imports its dependencies from `/app/dependencies` by relative
ES-module path; `NODE_PATH` does not apply to ES modules. Resolve dependencies for the
runtime/architecture outside Brewlet and package the actual layout explicitly.

## .NET

Read the [complete .NET guide](dotnet.md). For a runtime explicitly providing
`/usr/bin/dotnet`, the launch descriptor is:

```json
{
  "schemaVersion": 1,
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
required managed/native assets. Map app and dependency layers into `/app` and select an approved runtime with
the matching .NET major version. Check actual file paths and native ABI.

## Static/native binaries

Package the correct Linux binary at an explicit path such as `/app/bin/server`.
A compatible static root may have no shell, account files or PATH.
Dynamically linked native binaries additionally require a compatible loader
and libraries. Static binaries still need correct platform declarations and run as a non-root
numeric user and group.

## Deploy and verify

All recipes use ordinary Kubernetes resources, the `brewlet` RuntimeClass and
an explicit approved runtime ID (see the
[workload example](applications.md#native-workload-example)). After deploying,
check HTTP endpoints and dependencies, `kubectl exec` and probes, the process
identity, writable volumes and [resource limits](resources.md). A successful
`brewlet push` does not prove the workload runs.

For WebAssembly with explicit Wasmtime or Spin hosts, see the
[WebAssembly guide](webassembly.md).
