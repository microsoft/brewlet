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
