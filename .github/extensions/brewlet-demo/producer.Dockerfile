# Linux producer for the canvas: builds the matching Brewlet CLI from this checkout
# and packages the extracted ShowMyJVM layers. Fresh tmpfs copies avoid host
# (macOS) extended attributes; the application never contains a JDK.
FROM golang:1.26@sha256:37cfcca63222ec499bec5e8ad7aad52dbf7529db36d5812b8eae2f0621aa92d6 AS producer
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY pkg/ pkg/
COPY kubernetes/api/ kubernetes/api/
COPY kubernetes/policy/ kubernetes/policy/
COPY cmd/brewlet/ cmd/brewlet/
RUN --mount=type=cache,target=/root/.cache/go-build CGO_ENABLED=0 go build -trimpath -o /brewlet ./cmd/brewlet
COPY input/ /input/
RUN --mount=type=tmpfs,target=/package \
    mkdir -m 0755 /package/app /package/lib && \
    install -m 0644 /input/app/app.jar /package/app/app.jar && \
    for jar in /input/lib/*.jar; do install -m 0644 "$jar" /package/lib/; done && \
    install -m 0644 /input/launch.json /package/launch.json && \
    /brewlet build example.invalid/brewlet-demo/showmyjvm:demo --platform "${TARGETOS}/${TARGETARCH}" \
      --launch /package/launch.json \
      --layer dependency=/package/lib:/app/lib \
      --layer app=/package/app:/app \
      --store /out/oci --result /out/push.json && \
    /brewlet inspect example.invalid/brewlet-demo/showmyjvm:demo --store /out/oci > /out/inspect.txt 2>&1

FROM scratch AS artifact
COPY --from=producer /out/ /
