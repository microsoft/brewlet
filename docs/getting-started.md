# Getting started locally

In this local quick start you will install the released Brewlet CLI, build a small Java
application, package only its JAR into an OCI layout, and run it with
your installed JDK. You do not need Docker or Kubernetes.

## Prerequisites

You need:

- JDK 21 or newer;
- Maven 3.9 or newer;
- Bash, Git, `curl`, and `tar`; and
- macOS or Linux on `amd64` or `arm64`.

The optional source-build path also requires Go 1.26+ and `make`.
Use JDK 21 if you also plan to follow [Local Kubernetes](local-kubernetes.md);
that guide's PetClinic build requires it.

Run the commands in one **Bash** terminal unless instructed otherwise. Keep it
open so the variables remain available, and stop at any failed command. If
continuing in the Bash terminal from Local Kubernetes, skip the `bash` command:

```bash
bash
set -euo pipefail
```

??? note "Optional: check installed tools"

    If you are unsure which versions are installed, check them before continuing:

    ```bash
    java -version
    mvn -version
    git --version
    curl --version
    ```

## 1. Obtain Brewlet and the example source

### Install the released CLI (recommended)

The installer requires no Go toolchain. It detects your platform, downloads the
latest release, and verifies its SHA-256 checksum before installing it. Both
local guides use the same CLI in `$HOME/.local/bin`; running this block updates
that CLI to the latest release, even if an earlier shell set `BREWLET_VERSION`.
If you just installed it through [Local Kubernetes](local-kubernetes.md) in
this terminal, keep that CLI and `BREWLET_VERSION` and skip this block:

```bash
curl -fsSL https://brewlet.sh/install.sh | sh -s -- --version latest --install-dir "$HOME/.local/bin"
export PATH="$HOME/.local/bin:$PATH"

BREWLET_VERSION="$(brewlet version)"
export BREWLET_VERSION
printf 'Using Brewlet %s\n' "$BREWLET_VERSION"
```

`BREWLET_VERSION` now contains the installed release number, not a version to
choose manually. New example checkouts and the Kubernetes chart use that same
number. To install elsewhere, change `--install-dir` and the `PATH` entry together.

### Get or reuse the example source

Both guides share **one checkout**, separate from their generated output. If
you already have Brewlet source (including an extracted release archive), set
`BREWLET_SOURCE` to its absolute path before this block. Otherwise, the default
is `$HOME/brewlet-examples`, which is reused even in a new terminal:

```bash
export BREWLET_SOURCE="${BREWLET_SOURCE:-$HOME/brewlet-examples}"
if [ ! -e "$BREWLET_SOURCE" ]; then
  git clone --depth 1 --branch "v${BREWLET_VERSION}" \
    https://github.com/microsoft/brewlet.git "$BREWLET_SOURCE"
fi
if [ ! -f "$BREWLET_SOURCE/integration-tests/fixtures/demo-app/pom.xml" ] ||
   [ ! -f "$BREWLET_SOURCE/integration-tests/fixtures/spring-petclinic/build.sh" ]; then
  printf 'Stop: BREWLET_SOURCE must point to Brewlet source containing both examples.\n' >&2
  exit 1
fi
```

A new checkout uses the installed release's tag. An existing checkout is left
untouched: no second clone, checkout switch, or pull. If you keep source elsewhere,
set the same `BREWLET_SOURCE` in each new terminal. This path builds only the
example application, not Brewlet. Continue at step 2.

### Alternative: build from source

If you want to develop Brewlet itself, use this path instead of the release
installation and example-source steps above. Reuse the same source directory;
a first-time clone on this path uses the default branch:

```bash
export BREWLET_SOURCE="${BREWLET_SOURCE:-$HOME/brewlet-examples}"
if [ ! -e "$BREWLET_SOURCE" ]; then
  git clone https://github.com/microsoft/brewlet.git "$BREWLET_SOURCE"
fi
cd "$BREWLET_SOURCE"
make binaries
export PATH="$BREWLET_SOURCE/bin:$PATH"
brewlet version
```

The CLI reports the source build's version, which may be `dev`. Continue at
step 2. For Local Kubernetes, use that guide's released CLI and chart rather
than mixing a source-built CLI with released cluster components; keep this
checkout for the examples.

## 2. Build the example

Use the shared source directory. Quick-start output uses its own variables so
it cannot replace a Local Kubernetes run's work directory or cleanup state:

```bash
export BREWLET_QUICKSTART_WORK="$BREWLET_SOURCE/target/brewlet-quickstart"
export BREWLET_QUICKSTART_STORE="$BREWLET_QUICKSTART_WORK/oci"
export BREWLET_REF="demo/hello:local"
mkdir -p "$BREWLET_QUICKSTART_WORK"

mvn -f "$BREWLET_SOURCE/integration-tests/fixtures/demo-app/pom.xml" clean package
```

The result is an ordinary executable JAR. It contains neither Linux nor a JDK.

## 3. Package only the application

Use the CLI to write a native Brewlet artifact to a local OCI layout:

```bash
brewlet push \
  "$BREWLET_SOURCE/integration-tests/fixtures/demo-app/target/app.jar" \
  "$BREWLET_REF" \
  --store "$BREWLET_QUICKSTART_STORE" \
  --format artifact
```

The command reports the manifest digest and confirms that no Dockerfile or base
image was used.

??? note "Optional: inspect the JAR and launch contract"

    ```bash
    test -f "$BREWLET_SOURCE/integration-tests/fixtures/demo-app/target/app.jar"
    brewlet inspect "$BREWLET_REF" --store "$BREWLET_QUICKSTART_STORE"
    ```

    The output contains an OCI manifest and a JVM config whose `mainJar` is
    `app.jar`. The artifact does not choose a JDK or carry one; the runtime
    supplies the JDK when the application starts.

## 4. Run the application

Start the artifact in one terminal:

```bash
brewlet run "$BREWLET_REF" --store "$BREWLET_QUICKSTART_STORE"
```

Brewlet selects the local JDK, extracts the JAR into a sandbox, prints the exact
`java -jar` command, and starts the application on port 8080.

In a second terminal:

```bash
curl -f http://127.0.0.1:8080/hello
```

The response identifies the Brewlet application.

??? note "Optional: check health and runtime details"

    While the application is running, use the second terminal:

    ```bash
    curl -f http://127.0.0.1:8080/healthz
    curl -f http://127.0.0.1:8080/info
    ```

    The health endpoint returns `ok`, and `/info` reports the JDK that supplied
    the runtime.

Return to the first terminal and press **Ctrl+C**.

??? note "Optional: preview the node runtime bundle"

    In the first terminal, generate the OCI runtime bundle that the containerd
    shim would pass to `runc` on a Brewlet node:

    ```bash
    brewlet bundle "$BREWLET_REF" \
      --store "$BREWLET_QUICKSTART_STORE" \
      --cpu 1 \
      --memory 256Mi \
      --out "$BREWLET_QUICKSTART_WORK/bundle"

    test -f "$BREWLET_QUICKSTART_WORK/bundle/config.json"
    ```

    The bundle records the `java -jar /app/app.jar` process, a read-only node
    JDK mount, and the requested CPU and memory limits. Generating it is safe
    on macOS and Linux; executing the bundle with `runc` is a Linux node
    operation.

## What you proved

- Brewlet came from the latest checksum-verified release or your optional
  source build.
- The application payload contains only the JAR and launch metadata.
- A node-resident JDK can run the packaged application directly.

## Next steps

- [Run PetClinic on local Kubernetes with Docker Desktop](local-kubernetes.md),
  reusing this CLI and `BREWLET_SOURCE` checkout.
- [Install Brewlet on a Kubernetes cluster](installation.md).
- [Build and publish your own application](building-and-publishing.md).
- [Complete the role-based workshop](workshops/index.md).
- [Understand the architecture](concepts.md).
