# Local Kubernetes

Try Brewlet with Spring PetClinic in a **disposable kind cluster**. The demo
creates its own two-node cluster using your local Docker engine, runs the app,
and removes that cluster when you press **Ctrl+C**.

**Your existing Kubernetes cluster is not used.** If Docker Desktop already
runs Kubernetes with the kind engine, leave it as it is. Do not reset it,
change its provisioning method, or uninstall anything.

## Before you begin

- Use a normal terminal on **macOS**, **Linux**, or **Windows with WSL 2**.
  On Windows, run the commands inside your WSL distribution, not PowerShell.
- Start Docker Desktop with Linux containers and enable its integration with
  your WSL distribution when applicable. A local Docker Engine with Buildx
  also works on Linux. Remote Docker engines are not supported by this demo.
- Have `curl`, Bash, `tar`, and `sha256sum` or `shasum` available. These are
  standard on macOS and most Linux/WSL installations.
- Allow at least **4 CPUs and about 8 GiB of Docker memory**, with additional
  headroom if other workloads are running. The script checks Docker's allocated
  CPU and memory, not how much is currently free; it never changes the settings.
- Allow downloads from GitHub, Kubernetes and Helm distribution sites, Docker
  Hub, GHCR, and Maven Central. The first run downloads tools and build images
  and can take several minutes.

You do **not** need to install Java, Maven, kind, kubectl, Helm, or Brewlet.
The demo downloads checksum-verified tools into a private directory and builds
PetClinic with JDK 21 in a temporary container. Docker Desktop's built-in
Kubernetes does not need to be enabled.

!!! warning "Local evaluation only"
    Brewlet is preproduction software. Its privileged provisioner changes
    containerd and installs a JDK inside the disposable worker node.
    The demo shares Docker Desktop's CPU, memory, disk, and Docker daemon with
    your other containers; a separate kind cluster is not a separate machine
    or a security boundary. Run it only where local development containers
    and privileged kind nodes are permitted.

## 1. Run the demo

Copy this block into your existing terminal:

```sh
curl -fL https://brewlet.sh/try-brewlet.sh -o brewlet-demo.sh &&
  bash ./brewlet-demo.sh
```

This saves `brewlet-demo.sh` in the current directory. Use a directory where
that filename is available. You can also [read the script first](https://brewlet.sh/try-brewlet.sh)
or download it and inspect it before running `bash ./brewlet-demo.sh`.

**Do not source the script.** `bash ./brewlet-demo.sh` runs a child process;
it does not switch your terminal's shell, enable shell options in it, edit
startup files, or change your `PATH`. A failed download does not run the script,
and a failed demo returns you to your terminal rather than closing it.

The script prints its private workspace and unique cluster name, then:

1. Downloads the released CLI and matching chart, plus private helper tools.
2. Builds the pinned PetClinic example without using your source checkouts or
   Maven cache.
3. Creates a separate native-architecture kind cluster with a private kubeconfig.
4. Installs Brewlet on its worker, loads the JAR-only image, and starts PetClinic.
5. Checks application health before printing a browser URL.

All Kubernetes and Helm operations explicitly target that private kubeconfig.
The script never switches your current kubectl context or changes your existing
cluster's namespaces, Helm releases, node labels, JDKs, or registry configuration.
On Apple Silicon it selects ARM64 nodes, even if your terminal has an AMD64
Docker platform override.

## 2. Open PetClinic

Wait for:

```text
PetClinic is ready: http://127.0.0.1:<port>
```

Open the **actual URL printed by the script** and choose **Find Owners**.
The port is chosen automatically, so the demo does not need to stop another
application using port 8080 or 18080. Keep the terminal running while browsing.

PetClinic uses an in-memory H2 database. No external database, ingress controller,
registry account, or application Dockerfile is needed. The JAR-only application
image uses the worker's Temurin 21 JDK rather than carrying a JDK of its own.

## 3. Stop and clean up

Press **Ctrl+C** in the terminal running the demo. It stops port forwarding,
saves diagnostics, and deletes **only the kind cluster it created**. Your
terminal stays open and your existing Kubernetes configuration is unchanged.

The same cleanup runs after an ordinary setup failure. Diagnostics and private
tool binaries remain in the printed workspace; temporary source/build output
and the local OCI layout are removed. Docker may retain downloaded images in
its normal cache. The script never runs `docker system prune` or removes
unrelated containers or images.

If Docker becomes unavailable during cleanup, the script reports the failure
and prints an exact, run-specific cleanup command. Restore Docker and use that
command. The command is also saved as `cleanup-command.txt` in the private
workspace before cluster creation. A forced kill, machine shutdown, or terminal
crash can prevent any script from running cleanup: retain that workspace and
inspect the recorded resources before removing them. Do not reset Docker Desktop's
Kubernetes or delete a cluster based only on a guessed name.

## If the demo fails

Read the printed error and the workspace's `demo.log`. Cluster diagnostics are
saved under `cluster-logs/` when available; port-forward output is in
`port-forward.log`. A retry creates a new private workspace and cluster.

| Symptom | Next step |
|---|---|
| Docker cannot be reached | Start Docker Desktop or your local Docker Engine; on Windows, check WSL integration. |
| CPU or memory check fails | Make room for an additional cluster. The demo does not change Docker's settings automatically. |
| A download, checksum, or image pull fails | Check the reported endpoint and your network/proxy policy. Do not disable verification or alter your existing cluster's mirror configuration. |
| Brewlet or PetClinic does not become ready | Keep `demo.log` and `cluster-logs/` when reporting the failure. The disposable cluster is cleaned up automatically. |
| Cleanup fails | Use the exact recovery command printed by this run after restoring Docker. |

The demo avoids Docker Desktop's existing Kubernetes node image store and
registry-mirror configuration, including the GHCR mirror failure that affected
the previous walkthrough.

## Where to go next

- [Explore the CLI without Kubernetes](getting-started.md).
- [Build and publish your own application](building-and-publishing.md).
- [Install Brewlet in an administrator-managed cluster](installation.md).
- [Split platform and developer responsibilities](workshops/index.md).
