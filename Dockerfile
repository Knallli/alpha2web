# syntax=docker/dockerfile:1.7
#   docker build -t alpha2web .
ARG GO_VERSION=1.26

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION} AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY plugin ./plugin
COPY plugins ./plugins
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/alpha2web ./cmd/alpha2web

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/alpha2web /alpha2web
ENV DATA_DIR=/data HTTP_ADDR=:8765
VOLUME /data
EXPOSE 8765
HEALTHCHECK --interval=30s --timeout=10s --start-period=20s --retries=3 CMD ["/alpha2web", "healthcheck"]
ENTRYPOINT ["/alpha2web"]
