# alpha2go is still a local replace (../alpha2go), so build with the parent
# directory as context:
#   docker build -f alpha2web/Dockerfile -t alpha2web ..
ARG GO_VERSION=1.26
FROM golang:${GO_VERSION} AS build
WORKDIR /src
COPY alpha2go ./alpha2go
COPY alpha2web ./alpha2web
WORKDIR /src/alpha2web
RUN CGO_ENABLED=0 go build -trimpath -o /alpha2web ./cmd/alpha2web

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /alpha2web /alpha2web
ENV DATA_DIR=/data
VOLUME /data
EXPOSE 8080
ENTRYPOINT ["/alpha2web"]
