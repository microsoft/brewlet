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
COPY --chmod=0644 examples/local/app/server.py /example/app/server.py
COPY --chmod=0644 examples/local/launch.json /example/launch.json
RUN /brewlet push example.invalid/brewlet/hello:v1 --platform "${TARGETOS}/${TARGETARCH}" \
    --launch /example/launch.json --layer app=/example/app:/app \
    --store /out/oci --push-result /out/push.json

FROM scratch AS artifact
COPY --from=producer /out/ /
