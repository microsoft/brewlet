# Linux producer for the canvas: packages the extracted ShowMyJVM layers with a
# Brewlet CLI that is either the checksum-verified release binary placed in
# cli/brewlet (BREWLET_CLI=release) or built from this checkout
# (BREWLET_CLI=source). Fresh tmpfs copies avoid host (macOS) extended
# attributes; the application never contains a JDK.
ARG BREWLET_CLI=release

FROM golang:1.26@sha256:37cfcca63222ec499bec5e8ad7aad52dbf7529db36d5812b8eae2f0621aa92d6 AS cli-source
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY pkg/ pkg/
COPY kubernetes/api/ kubernetes/api/
COPY kubernetes/policy/ kubernetes/policy/
COPY cmd/brewlet/ cmd/brewlet/
RUN --mount=type=cache,target=/root/.cache/go-build CGO_ENABLED=0 go build -trimpath -o /brewlet ./cmd/brewlet

FROM scratch AS cli-release
COPY cli/brewlet /brewlet

FROM cli-${BREWLET_CLI} AS cli

FROM debian:bookworm-slim@sha256:7c7b2c966bc9ee8cedfeef67e0e279108992c77681fa595db4a9d65c06ccc587 AS producer
ARG TARGETOS
ARG TARGETARCH
COPY --from=cli /brewlet /usr/local/bin/brewlet
COPY input/ /input/
RUN --mount=type=tmpfs,target=/package \
    mkdir -m 0755 /package/app /package/lib && \
    install -m 0644 /input/app/app.jar /package/app/app.jar && \
    for jar in /input/lib/*.jar; do install -m 0644 "$jar" /package/lib/; done && \
    install -m 0644 /input/launch.json /package/launch.json && \
    brewlet build example.invalid/brewlet-demo/showmyjvm:demo --platform "${TARGETOS}/${TARGETARCH}" \
      --launch /package/launch.json \
      --layer dependency=/package/lib:/app/lib \
      --layer app=/package/app:/app \
      --store /out/oci --result /out/push.json && \
    brewlet inspect example.invalid/brewlet-demo/showmyjvm:demo --store /out/oci > /out/inspect.txt 2>&1

FROM scratch AS artifact
COPY --from=producer /out/ /
