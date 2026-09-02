# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

# brewlet-operator / brewlet-admission image.
#
# Multi-stage: build one of the module's binaries for the target arch, then drop
# it into a distroless static base. Which command is built is selected by the CMD
# build-arg (default: manager). Build context is the repository root.
#
#   docker build -f kubernetes/Dockerfile -t ghcr.io/microsoft/brewlet-operator:dev .
#   docker build -f kubernetes/Dockerfile --build-arg CMD=admission \
#     -t ghcr.io/microsoft/brewlet-admission:dev .

FROM --platform=$BUILDPLATFORM golang:1.26-bookworm AS build
ARG TARGETARCH
ARG CMD=manager
WORKDIR /src
COPY kubernetes/go.mod kubernetes/go.sum ./
RUN go mod download
COPY kubernetes/. .
COPY scripts/generate-go-notice.sh /usr/local/bin/generate-go-notice
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags='-s -w' -o /out/entry ./cmd/${CMD} \
 && GOOS=linux GOARCH=${TARGETARCH} \
    /usr/local/bin/generate-go-notice /src /out/NOTICE.txt ./cmd/${CMD}

FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=build /out/entry /entry
COPY --from=build /out/NOTICE.txt /NOTICE.txt
COPY LICENSE.txt /LICENSE.txt
COPY legal/CONTAINER-LEGAL-NOTICE.txt /CONTAINER-LEGAL-NOTICE.txt
USER 65532:65532
ENTRYPOINT ["/entry"]
