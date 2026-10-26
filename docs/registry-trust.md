# Registry access and certificate trust

There are four separate trust paths. Configuring one does not configure the
others.

| Client | Purpose | Configuration responsibility |
| --- | --- | --- |
| Brewlet producer | Publish/inspect application and layer-set OCI graphs. | Producer credentials and host/producer CA roots. |
| Kubelet/containerd CRI | Pull application images. | Pod/service-account imagePullSecrets and node registry trust. |
| Node provisioner/containerd | Acquire approved runtime sources. | Reviewed node source-registry authentication, CA roots and mirror policy. |
| Application runtime | Outbound application TLS. | Runtime/application trust store, explicit configuration and mounted trust material. |

## Producer access

The CLI accepts `BREWLET_REGISTRY_USERNAME`, `BREWLET_REGISTRY_PASSWORD` and
additional trust through `SSL_CERT_FILE`. Obtain credentials through your secret
delivery system, keep them out of descriptors and source control, and scope
them to the intended registry. Credential forwarding is origin-bound.

`--registry-scheme https` explicitly selects TLS, including a loopback TLS
registry. `--insecure` permits HTTP; it is not a certificate-verification
bypass. Use HTTP only for a deliberately isolated fixture or an explicitly
reviewed environment, never as the default repair for a bad certificate.

## Application pulls

Use normal Kubernetes imagePullSecrets or your platform's workload registry
integration. A Secret usable by the application namespace is not automatically
available to a privileged runtime-source pull. Prefer digest-pinned application
references and verify actual pull failures/events separately from root installation.

The [local tutorial](local-kubernetes.md) preimports the application image into
its private node and uses `Never`; the node pulls the public runtime images
itself. It intentionally does not qualify production registry authentication or
TLS.

## Runtime sources and mirrors

Configure node registry access before offering private runtime sources. The
node provisioner resolves runtime sources through the node's containerd hosts
directory, `/etc/containerd/certs.d`:

1. Put registry host settings in `/etc/containerd/certs.d/<registry>/hosts.toml`.
   Reference CA files with paths relative to that directory, so they still
   resolve when the provisioner reads the host filesystem.
2. For an authenticated registry, add
   `/etc/containerd/certs.d/<registry>/credentials.json` containing exactly
   `{"username": "...", "password": "..."}`. The file must be a regular file of
   at most 64 KiB, with no group or other permissions (for example mode `0600`).
   Credentials under `_default` are never used.

Credentials are sent only to the registry's own HTTPS origin. Do not distribute
registry credentials through application environment variables.

`NodeProfile.spec.registry.mirrors` is governed by
`security.allowedSourceMirrorHosts`. Destination authorities must be explicit,
different from their source and allowlisted. Mapping a source does not change
the approved digest or excuse missing content. Even unused malformed policies
fail validation.

For an air-gapped deployment, mirror the required platform graphs, configs and
layers, verify digest preservation and platform coverage, and arrange trusted
node access before readiness. A mirror declaration alone is not proof that
offline installation works.

## Application-specific or corporate CAs

If all applications using an ID need shared trust, a platform may derive a
runtime image containing approved CA roots and rotate to its reviewed digest.
If trust is application-specific, mount CA material and select it through the
application/runtime's documented mechanism.

Java truststores, OpenSSL roots and runtime-specific trust behavior differ.
Brewlet does not detect them, rewrite truststores or inject JVM flags.
Adding a public CA is not the same as distributing an mTLS private key.
Do not package private keys into world-readable application layers.
