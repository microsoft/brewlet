# Test platform-owned golden image pipeline

These four Dockerfiles are test inputs representing the **platform team's**
image pipeline. They are not Brewlet features; neither core nor the provisioner
builds runtime images. All upstream base references are immutable OCI index
digests, resolved during this implementation. Building a derived fixture still
requires recording and approving **its resulting digest**, not the upstream
digest.

The fixture account is `app`, UID/GID `65532:65532`, home `/home/app`. The
pipeline removes setuid/setgid bits and checks that `/app` is absent or empty.
The provisioner must independently validate the final root, including account,
PATH directories, file capabilities, platforms and safe extraction. A
successful Docker build is not that validation, and must not publish Brewlet
readiness or local authorization.

The Java fixture uses the pinned Temurin Alpine **JRE** images for 21.0.7+6 and
21.0.8+9, with both Linux amd64 and arm64 platform descriptors. A full SDK or
Ubuntu userland is not required by the generic runtime contract; using a
smaller explicit golden root reduces local node storage without changing any
launch, dependency, identity, exec, rotation or cleanup assertion. Java/Maven
producer builds still use the host JDK. The account preparation script supports
both shadow-utils and BusyBox account tools, preserving the same app identity
and set-ID removal policy. This is fixture image-pipeline logic, not runtime
detection in Brewlet.

Build explicitly for the architecture under test, using this directory as the
context and an invocation-owned tag, for example:

```sh
docker build --load --platform linux/arm64 \
  --file integration-tests/fixtures/runtimes/python.Dockerfile \
  --tag brewlet-test-python:invocation-id integration-tests/fixtures/runtimes
```

Vendor runtime licenses and redistribution terms remain the platform's
responsibility. These minimal functional fixtures retain package managers and
are not recommended production golden images.
