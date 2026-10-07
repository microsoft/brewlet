# Custom CA certificates for Java workloads

For shared corporate or private CA roots, **build the certificates into a
platform-owned JDK source image and select that JDK distribution explicitly**.
This works with today's `JavaApplication` API, preserves JAR-only application
images, and uses Brewlet's existing digest-pinned JDK provisioning workflow.

This guide configures the default Java TLS trust used for outbound connections,
such as HTTPS. It does not configure server certificates, mutual-TLS private
keys, containerd registry trust, or the Brewlet admission webhook's certificates.
Libraries that create their own TLS context may need separate configuration.

Related: [JDK management](jdk-management.md) ·
[Deploying workloads](deploying-workloads.md) · [Security](security.md).

## Build a JDK image containing the approved CA roots

Obtain the CA certificate through your organization's trusted distribution
channel and verify its SHA-256 fingerprint out of band before importing it.
Import only approved trust anchors, not a server's leaf certificate as a
shortcut around TLS verification.

The following Dockerfile extends Temurin 21's existing `cacerts`, retaining its
public roots. Replace the base digest placeholder with a reviewed digest:

```dockerfile
FROM docker.io/library/eclipse-temurin@sha256:<reviewed-temurin-21-digest>
COPY corporate-root-ca.pem /tmp/corporate-root-ca.pem
RUN "$JAVA_HOME/bin/keytool" -importcert -noprompt \
      -cacerts -storepass changeit \
      -alias corporate-root-ca \
      -file /tmp/corporate-root-ca.pem \
    && "$JAVA_HOME/bin/keytool" -list -cacerts -storepass changeit \
      -alias corporate-root-ca \
    && rm /tmp/corporate-root-ca.pem
```

Use a unique alias and import each approved root separately. `changeit` is the
upstream example truststore's integrity password, not an application credential;
adapt it if your base uses another password. This image must not contain private
keys or application secrets.

Build and publish this as a **JDK source image**, not a Brewlet application image.
For example, replacing the registry destination with your own:

```bash
docker buildx build --platform linux/amd64,linux/arm64 \
  -t registry.example.com/platform/temurin-corporate:21 --push .
docker buildx imagetools inspect registry.example.com/platform/temurin-corporate:21
```

Publish only the architectures you operate and confirm that the base supports
them. Review the resulting image/index digest and use that digest below.

**Perform certificate installation at build time.** Brewlet copies the JDK
source image's complete userland and launches Java directly; it does not run the
image's normal entrypoint. Entrypoint-based CA import or OS trust-update scripts
will not run. Updating the OS PEM bundle alone also does not guarantee that
Java's default truststore changes.

## Register and select the JDK

Add the derived image to the existing profile that owns your node pool. For a
Helm-managed profile, merge this entry into `provisioner.jdks`, retaining the
other JDKs your workloads need:

```yaml
provisioner:
  jdks:
    - distribution: temurin-corporate
      feature: 21
      source:
        image: registry.example.com/platform/temurin-corporate@sha256:<reviewed-image-digest>
        javaHome: /opt/java/openjdk
```

For a directly managed `NodeProfile`, the same entry goes in `spec.jdks`.
Follow the existing [JDK provisioning workflow](jdk-management.md#source-model);
do not create an overlapping profile or edit provisioned files on the host.

Once the selected nodes advertise the new JDK as ready, select it in your
application descriptor:

```yaml
apiVersion: apps.brewlet.sh/v1alpha1
kind: JavaApplication
metadata:
  name: orders
spec:
  artifact:
    image: registry.example.com/demo/orders@sha256:<application-image-digest>
  jvm:
    version: 21
    distribution: temurin-corporate
```

For a raw Pod or Deployment, use the Pod annotation
`brewlet.sh/jdk: "temurin-corporate-21"` instead.

No `javax.net.ssl.trustStore` flags are needed: Java uses this JDK's default
truststore, provided the application has not selected another one. Check for
existing truststore overrides, a `jssecacerts` file (which takes precedence over
`cacerts`), or framework-specific TLS configuration.

Pin both `version` and `distribution`. A bare `version: 21` may select a different
JDK; adding a distribution can also change selection for other unpinned
workloads. See [JDK selection](jdk-management.md#when-distribution-is-omitted).

## Verify and rotate

Before rollout, inspect the imported aliases/fingerprints and exercise an actual
TLS connection from the application to the intended private endpoint. Confirm
that required public endpoints still work and that an endpoint signed by an
untrusted CA fails. Listing certificates alone does not establish that the
application's HTTP client uses that truststore.

Rotate roots like a [JDK patch](jdk-management.md#patching-upgrading-jdks):
rebuild from the current approved JDK base, publish a new digest, update the
profile, wait for node readiness, and restart the affected workloads. Existing
Pods retain their old JDK root; a source-image update does not change their
running truststore. Default Java TLS contexts should not be assumed to reload
trust automatically.

For a planned CA migration, first distribute both old and new roots, restart
clients, then switch servers. Remove the old root in a subsequent image and
workload rollout after migration. A compromised CA may require immediate
removal instead, following your incident-response policy.

## When trust must be application-specific

Today's `JavaApplication` CRD exposes `spec.jvm.args` but not volumes,
volume mounts, or a truststore field. Do not add those unsupported fields or
patch the operator-owned Deployment as a durable configuration mechanism.

Use a separately managed raw Pod/Deployment if you need a per-application
truststore now: mount a prebuilt JKS or PKCS12 file from a same-namespace Secret
or ConfigMap, read-only at a dedicated path such as `/etc/java-trust`, and pass
`javax.net.ssl.trustStore`, `javax.net.ssl.trustStoreType`, and, when required,
`javax.net.ssl.trustStorePassword` through the `brewlet.sh/jvm-args` annotation.
Build the store outside the Pod; the current handler does not support an
ordinary-image init container that runs `keytool`.

An explicit truststore **replaces**, rather than augments, Java's default
truststore. Include public roots yourself if required, and maintain them as the
JDK's defaults evolve. Never put private-key passwords in JVM annotations or
`JAVA_TOOL_OPTIONS`; those surfaces can expose values through manifests, process
arguments, or logs.

[Proposal 0008](https://github.com/microsoft/brewlet/blob/main/specs/proposals/0008-application-truststores.md)
describes a possible first-class per-application truststore API. It is **not
implemented**.
