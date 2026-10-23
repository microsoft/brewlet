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
RUN mkdir -p /example/app && chmod 0755 /example /example/app
COPY --chmod=0644 examples/local/app/server.py /example/app/server.py
COPY --chmod=0644 examples/local/launch.json /example/launch.json
# Fresh authored input avoids BuildKit's own overlay metadata; validation stays strict.
RUN --mount=type=tmpfs,target=/package chmod 0755 /package && \
    install -m 0644 /example/app/server.py /package/server.py && \
    /brewlet build example.invalid/brewlet/hello:v1 --platform "${TARGETOS}/${TARGETARCH}" \
    --launch /example/launch.json --layer app=/package:/app \
    --store /out/oci --result /out/push.json

FROM scratch AS artifact
COPY --from=producer /out/ /
