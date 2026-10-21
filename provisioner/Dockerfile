# Go 1.26, pinned multi-platform toolchain.
FROM golang:1.26@sha256:37cfcca63222ec499bec5e8ad7aad52dbf7529db36d5812b8eae2f0621aa92d6 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY pkg/ pkg/
COPY kubernetes/api/ kubernetes/api/
COPY kubernetes/policy/ kubernetes/policy/
COPY provisioner/ provisioner/
COPY cmd/brewlet-node/ cmd/brewlet-node/
COPY shim/ shim/
COPY scripts/notice/ scripts/notice/
COPY LICENSE.txt ./
RUN --mount=type=cache,target=/root/.cache/go-build CGO_ENABLED=0 go build -trimpath -o /brewlet-node ./cmd/brewlet-node && \
    CGO_ENABLED=0 go build -trimpath -o /containerd-shim-brewlet-v2 ./shim/cmd/containerd-shim-brewlet-v2 && \
    CGO_ENABLED=0 go run ./scripts/notice --output /NOTICE.txt ./cmd/brewlet-node ./shim/cmd/containerd-shim-brewlet-v2

# Debian bookworm-slim; no mutable package repository inputs.
FROM debian:bookworm-slim@sha256:7c7b2c966bc9ee8cedfeef67e0e279108992c77681fa595db4a9d65c06ccc587
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
RUN test -x /usr/bin/nsenter && test -s /etc/ssl/certs/ca-certificates.crt
COPY --from=build /brewlet-node /brewlet-node
COPY --from=build /containerd-shim-brewlet-v2 /containerd-shim-brewlet-v2
COPY --from=build /src/LICENSE.txt /LICENSE.txt
COPY --from=build /NOTICE.txt /NOTICE.txt
ENTRYPOINT ["/brewlet-node"]
