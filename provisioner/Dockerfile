# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

# brewlet-node-provisioner image.
#
# Multi-stage build:
#   1. Compile the containerd-shim-brewlet-v2 binary from the core module for the
#      target architecture (so the shim installed on the node always matches the
#      node arch — the JAR is arch-neutral, the shim is not).
#   2. Download and checksum-verify target-architecture host tools and notices.
#   3. Re-verify immediately before extracting the tools.
#   4. Assemble a small runtime image containing only the verified tools plus the
#      host-mutation dependencies needed by the entrypoint.
#
# Build (single arch, native):   docker build -t ghcr.io/microsoft/brewlet-node-provisioner:dev -f provisioner/Dockerfile .
# Build (multi-arch, buildx):    docker buildx build --platform linux/amd64,linux/arm64 ...
#
# The build context is the repository root; the Go module lives in core/.

ARG KUBECTL_VERSION=v1.31.0
ARG CONTAINERD_VERSION=2.0.5
ARG CRICTL_VERSION=v1.31.1
ARG KUBECTL_SOURCE_COMMIT=9edcffcde5595e8a5b1a35f88c421764e575afce
ARG CONTAINERD_SOURCE_COMMIT=fb4c30d4ede3531652d86197bf3fc9515e5276d9
ARG CRICTL_SOURCE_COMMIT=ea669ca726b21585ee9c94276391eea5b486da46

# ---- stage 1: build the shim for $TARGETARCH -------------------------------
FROM --platform=$BUILDPLATFORM golang:1.26-bookworm@sha256:9fdc884aacc3bec89b20ffc69f4bb369c78210e3e4f600387b5128b12c199f81 AS shim
ARG TARGETARCH
WORKDIR /src
COPY core/go.mod core/go.sum ./
RUN --mount=type=secret,id=additional-ca,required=false \
    if [ -s /run/secrets/additional-ca ]; then \
      install -m 0644 /run/secrets/additional-ca /usr/local/share/ca-certificates/additional-ca.crt; \
      update-ca-certificates; \
    fi
RUN go mod download
COPY core/. .
COPY scripts/generate-go-notice.sh /usr/local/bin/generate-go-notice
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags='-s -w' \
    -o /out/containerd-shim-brewlet-v2 ./shim/cmd/containerd-shim-brewlet-v2 \
 && CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags='-s -w' \
    -o /out/brewlet-metrics-exporter ./cmd/brewlet-metrics-exporter \
 && CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags='-s -w' \
    -o /out/brewlet-source-policy ./cmd/brewlet-source-policy \
 && GOOS=linux GOARCH=${TARGETARCH} \
    /usr/local/bin/generate-go-notice /src /out/NOTICE.txt \
      ./shim/cmd/containerd-shim-brewlet-v2 \
      ./cmd/brewlet-metrics-exporter \
      ./cmd/brewlet-source-policy

# ---- stage 2: download and verify remote assets ----------------------------
FROM --platform=$BUILDPLATFORM debian:bookworm-slim@sha256:88200866dfff7ea7f5cbcb6ec7c8a701889efe6fe859fe64d6990e4b07ea4171 AS downloaded-assets
ARG TARGETARCH
ARG KUBECTL_VERSION
ARG CONTAINERD_VERSION
ARG CRICTL_VERSION
ARG KUBECTL_SOURCE_COMMIT
ARG CONTAINERD_SOURCE_COMMIT
ARG CRICTL_SOURCE_COMMIT

COPY --chmod=0755 provisioner/download-verified.sh /usr/local/bin/download-verified
COPY provisioner/checksums/ /checksums/
RUN apt-get update \
 && apt-get install -y --no-install-recommends bash ca-certificates coreutils curl gzip tar \
 && apt-get clean \
 && rm -rf /var/lib/apt/lists/*
RUN --mount=type=secret,id=additional-ca,required=false \
    if [ -s /run/secrets/additional-ca ]; then \
      install -m 0644 /run/secrets/additional-ca /usr/local/share/ca-certificates/additional-ca.crt; \
      update-ca-certificates; \
    fi

RUN set -eu; \
    case "$TARGETARCH" in amd64|arm64) ;; *) echo "unsupported target architecture: $TARGETARCH" >&2; exit 1 ;; esac; \
    checksum_for() { \
      file="$1"; manifest="$2"; \
      awk -v file="$file" '$2 == file { print $1 }' "$manifest"; \
    }; \
    tool_checksums="/checksums/${TARGETARCH}.sha256"; \
    download-verified kubectl \
      "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/${TARGETARCH}/kubectl" \
      "$(checksum_for kubectl "$tool_checksums")" \
      /downloads/kubectl; \
    download-verified containerd \
      "https://github.com/containerd/containerd/releases/download/v${CONTAINERD_VERSION}/containerd-${CONTAINERD_VERSION}-linux-${TARGETARCH}.tar.gz" \
      "$(checksum_for containerd.tar.gz "$tool_checksums")" \
      /downloads/containerd.tar.gz; \
    download-verified crictl \
      "https://github.com/kubernetes-sigs/cri-tools/releases/download/${CRICTL_VERSION}/crictl-${CRICTL_VERSION}-linux-${TARGETARCH}.tar.gz" \
      "$(checksum_for crictl.tar.gz "$tool_checksums")" \
      /downloads/crictl.tar.gz; \
    download-verified kubectl-license \
      "https://raw.githubusercontent.com/kubernetes/kubernetes/${KUBECTL_SOURCE_COMMIT}/LICENSE" \
      "$(checksum_for kubectl.LICENSE /checksums/licenses.sha256)" \
      /downloads/kubectl.LICENSE; \
    download-verified containerd-license \
      "https://raw.githubusercontent.com/containerd/containerd/${CONTAINERD_SOURCE_COMMIT}/LICENSE" \
      "$(checksum_for containerd.LICENSE /checksums/licenses.sha256)" \
      /downloads/containerd.LICENSE; \
    download-verified crictl-license \
      "https://raw.githubusercontent.com/kubernetes-sigs/cri-tools/${CRICTL_SOURCE_COMMIT}/LICENSE" \
      "$(checksum_for crictl.LICENSE /checksums/licenses.sha256)" \
      /downloads/crictl.LICENSE

# ---- stage 3: re-verify and extract trusted assets -------------------------
FROM downloaded-assets AS verified-assets
ARG TARGETARCH
ARG KUBECTL_VERSION
ARG CONTAINERD_VERSION
ARG CRICTL_VERSION
ARG KUBECTL_SOURCE_COMMIT
ARG CONTAINERD_SOURCE_COMMIT
ARG CRICTL_SOURCE_COMMIT
ARG BREWLET_TEST_CORRUPT_DOWNLOAD=

COPY --from=shim /out/NOTICE.txt /verified/NOTICE.txt
RUN set -eu; \
    case "$BREWLET_TEST_CORRUPT_DOWNLOAD" in \
      "") ;; \
      kubectl|containerd.tar.gz|crictl.tar.gz|kubectl.LICENSE|containerd.LICENSE|crictl.LICENSE) \
       printf '\ncorrupted by integrity test\n' >> "/downloads/${BREWLET_TEST_CORRUPT_DOWNLOAD}" ;; \
      *) echo "unsupported corruption target: $BREWLET_TEST_CORRUPT_DOWNLOAD" >&2; exit 2 ;; \
    esac; \
    cd /downloads; \
    sha256sum -c "/checksums/${TARGETARCH}.sha256"; \
    sha256sum -c /checksums/licenses.sha256; \
    install -m 0755 kubectl /verified/kubectl; \
    mkdir -p /tmp/containerd /tmp/crictl; \
    tar -xzf containerd.tar.gz -C /tmp/containerd bin/ctr; \
    tar -xzf crictl.tar.gz -C /tmp/crictl crictl; \
    install -m 0755 /tmp/containerd/bin/ctr /verified/ctr; \
    install -m 0755 /tmp/crictl/crictl /verified/crictl; \
    for component in \
      "Kubernetes kubectl ${KUBECTL_VERSION#v}|https://github.com/kubernetes/kubernetes|${KUBECTL_SOURCE_COMMIT}|kubectl.LICENSE" \
      "containerd ctr ${CONTAINERD_VERSION}|https://github.com/containerd/containerd|${CONTAINERD_SOURCE_COMMIT}|containerd.LICENSE" \
      "Kubernetes CRI tools crictl ${CRICTL_VERSION#v}|https://github.com/kubernetes-sigs/cri-tools|${CRICTL_SOURCE_COMMIT}|crictl.LICENSE"; do \
      name="${component%%|*}"; rest="${component#*|}"; source="${rest%%|*}"; rest="${rest#*|}"; \
      commit="${rest%%|*}"; license="${rest##*|}"; \
      printf '\n================================================================================\nComponent: %s\nSource: %s/tree/%s\n\n--- LICENSE ---\n\n' \
       "$name" "$source" "$commit" >> /verified/NOTICE.txt; \
      cat "$license" >> /verified/NOTICE.txt; \
      printf '\n' >> /verified/NOTICE.txt; \
    done

# ---- stage 4: runtime image ------------------------------------------------
FROM debian:bookworm-slim@sha256:88200866dfff7ea7f5cbcb6ec7c8a701889efe6fe859fe64d6990e4b07ea4171

RUN apt-get update \
 && apt-get install -y --no-install-recommends bash ca-certificates procps util-linux \
 && apt-get clean \
 && rm -rf /var/lib/apt/lists/*

COPY --from=verified-assets /verified/kubectl /usr/local/bin/kubectl
COPY --from=verified-assets /verified/ctr /usr/local/bin/ctr
COPY --from=verified-assets /verified/crictl /usr/local/bin/crictl
COPY --from=verified-assets /verified/NOTICE.txt /NOTICE.txt

# The shim binary the provisioner installs onto the node.
COPY --from=shim /out/containerd-shim-brewlet-v2 /opt/brewlet-dist/containerd-shim-brewlet-v2
COPY --from=shim /out/brewlet-metrics-exporter /opt/brewlet-dist/brewlet-metrics-exporter
COPY --from=shim /out/brewlet-source-policy /opt/brewlet-dist/brewlet-source-policy
COPY LICENSE.txt /LICENSE.txt
COPY legal/CONTAINER-LEGAL-NOTICE.txt /CONTAINER-LEGAL-NOTICE.txt
COPY provisioner/entrypoint.sh /usr/local/bin/brewlet-provision
RUN chmod 0755 /usr/local/bin/brewlet-provision

ENTRYPOINT ["/usr/local/bin/brewlet-provision"]
