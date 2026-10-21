# Go 1.26 builder.
FROM golang:1.26@sha256:37cfcca63222ec499bec5e8ad7aad52dbf7529db36d5812b8eae2f0621aa92d6 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY kubernetes ./kubernetes
COPY pkg ./pkg
COPY scripts/notice ./scripts/notice
COPY LICENSE.txt ./
RUN --mount=type=cache,target=/root/.cache/go-build CGO_ENABLED=0 go build -trimpath -o /out/operator ./kubernetes/cmd/operator && \
    CGO_ENABLED=0 go build -trimpath -o /out/admission ./kubernetes/cmd/admission && \
    CGO_ENABLED=0 go run ./scripts/notice --output /out/NOTICE.txt ./kubernetes/cmd/operator ./kubernetes/cmd/admission
# Debian 12 distroless nonroot.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/operator /operator
COPY --from=build /out/admission /admission
COPY --from=build /src/LICENSE.txt /LICENSE.txt
COPY --from=build /out/NOTICE.txt /NOTICE.txt
USER 65532:65532
ENTRYPOINT ["/operator"]
