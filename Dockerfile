# The build stage runs on the build machine and cross-compiles, so a
# multi-platform build needs no emulation.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
ARG TARGETOS TARGETARCH
ARG VERSION=dev
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
COPY web ./web
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/fsconsole ./cmd/fsconsole \
    && mkdir /out/data

FROM scratch
LABEL org.opencontainers.image.source="https://github.com/juki/freeswitch-console" \
      org.opencontainers.image.description="Web console for operating FreeSWITCH over the Event Socket" \
      org.opencontainers.image.licenses="AGPL-3.0-only"
# Root certificates, for HTTPS connections to fsconsole-agent.
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/fsconsole /fsconsole
COPY --from=build --chown=65534:65534 /out/data /data
USER 65534:65534
ENV FSCONSOLE_LISTEN=0.0.0.0:8080 \
    FSCONSOLE_DATA=/data/fsconsole.json
EXPOSE 8080
VOLUME /data
ENTRYPOINT ["/fsconsole"]
