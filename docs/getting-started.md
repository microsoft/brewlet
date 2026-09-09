# Getting started locally

In this local quick start you will install the released Brewlet CLI, build a small Java
application, package only its JAR into an OCI layout, inspect it, and run it with
your installed JDK. You do not need Docker or Kubernetes.

## Prerequisites

You need:

- JDK 21 or newer;
- Maven 3.9 or newer;
- `curl` (plus `tar` for release downloads); and
- macOS or Linux on `amd64` or `arm64`.

The optional source-build path also requires Git, Go 1.26+, and `make`.

Confirm the tools before continuing:

```bash
java -version
mvn -version
curl --version
```

## 1. Obtain Brewlet and the example source

### Install the released CLI (recommended)

The installer requires no Go toolchain. It detects macOS or Linux and
`amd64` or `arm64`, downloads
the matching release archive, and verifies its SHA-256 checksum before
installing it:

```bash
export BREWLET_VERSION="0.4.0"
curl -fsSL https://brewlet.sh/install.sh | sh
export PATH="$HOME/.local/bin:$PATH"

brewlet version
```

The last command must print `0.4.0`.

Without `BREWLET_VERSION`, the installer selects the latest release. Set
`BREWLET_INSTALL_DIR` to choose a directory other than `$HOME/.local/bin`.

Download the example source from the matching release tag. This builds the
application only; it does not build Brewlet.

```bash
export BREWLET_WORK="$PWD/brewlet-quickstart-${BREWLET_VERSION}"
export BREWLET_REF="demo/hello:${BREWLET_VERSION}"
mkdir -p "$BREWLET_WORK"

curl -fL \
  "https://github.com/microsoft/brewlet/archive/refs/tags/v${BREWLET_VERSION}.tar.gz" \
  -o "$BREWLET_WORK/source.tar.gz"
tar -xzf "$BREWLET_WORK/source.tar.gz" -C "$BREWLET_WORK"
cd "$BREWLET_WORK/brewlet-${BREWLET_VERSION}"
```

Continue at step 2 from the extracted release source.

### Alternative: build from source

If you want to develop Brewlet itself, use this path instead of the release
installation and source download above. From a directory where you keep projects:

```bash
git clone https://github.com/microsoft/brewlet.git
cd brewlet
make binaries
export PATH="$PWD/bin:$PATH"
export BREWLET_WORK="$PWD/target/brewlet-quickstart"
export BREWLET_REF="demo/hello:local"
mkdir -p "$BREWLET_WORK"
brewlet version
```

The CLI reports the source build's version, which need not be `0.4.0`. Continue
at step 2 from this checkout.

## 2. Build the example

From the source checkout or extracted release source:

```bash
mvn -f integration-tests/fixtures/demo-app/pom.xml clean package
test -f integration-tests/fixtures/demo-app/target/app.jar
```

The result is an ordinary executable JAR. It contains neither Linux nor a JDK.

## 3. Package only the application

Use the CLI to write a native Brewlet artifact to a local OCI layout:

```bash
export BREWLET_STORE="$BREWLET_WORK/oci"

brewlet push \
  integration-tests/fixtures/demo-app/target/app.jar \
  "$BREWLET_REF" \
  --store "$BREWLET_STORE" \
  --format artifact
```

The command reports the manifest digest and confirms that no Dockerfile or base
image was used.

## 4. Inspect the launch contract

```bash
brewlet inspect "$BREWLET_REF" --store "$BREWLET_STORE"
```

The output contains an OCI manifest and a JVM config whose `mainJar` is
`app.jar`. The artifact does not choose a JDK or carry one; the runtime supplies
the JDK when the application starts.

## 5. Run the application

Start the artifact in one terminal:

```bash
brewlet run "$BREWLET_REF" --store "$BREWLET_STORE"
```

Brewlet selects the local JDK, extracts the JAR into a sandbox, prints the exact
`java -jar` command, and starts the application on port 8080.

In a second terminal:

```bash
curl -f http://127.0.0.1:8080/healthz
curl -f http://127.0.0.1:8080/hello
curl -f http://127.0.0.1:8080/info
```

The health endpoint returns `ok`, `/hello` identifies the Brewlet application,
and `/info` reports the JDK that supplied the runtime. Return to the first
terminal and press **Ctrl+C**.

## 6. Preview the node runtime bundle

Generate the OCI runtime bundle that the containerd shim would pass to `runc` on
a Brewlet node:

```bash
brewlet bundle "$BREWLET_REF" \
  --store "$BREWLET_STORE" \
  --cpu 1 \
  --memory 256Mi \
  --out "$BREWLET_WORK/bundle"

test -f "$BREWLET_WORK/bundle/config.json"
```

The bundle records the `java -jar /app/app.jar` process, a read-only node JDK
mount, and the requested CPU and memory limits. Generating it is safe on macOS
and Linux; executing the bundle with `runc` is a Linux node operation.

## What you proved

- Brewlet came from the checksum-verified v0.4.0 release or your optional
  source build.
- The application payload contains only the JAR and launch metadata.
- A node-resident JDK can run the packaged application directly.
- Brewlet can translate the same artifact into an OCI runtime bundle with
  cgroup limits.

## Next steps

- [Install Brewlet on a Kubernetes cluster](installation.md).
- [Build and publish your own application](building-and-publishing.md).
- [Complete the role-based workshop](workshops/index.md).
- [Understand the architecture](concepts.md).
