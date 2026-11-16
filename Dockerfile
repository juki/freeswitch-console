FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
COPY web ./web
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/fsconsole ./cmd/fsconsole \
    && mkdir /out/data

FROM scratch
COPY --from=build /out/fsconsole /fsconsole
COPY --from=build --chown=65534:65534 /out/data /data
USER 65534:65534
ENV FSCONSOLE_LISTEN=0.0.0.0:8080 \
    FSCONSOLE_DATA=/data/fsconsole.json
EXPOSE 8080
VOLUME /data
ENTRYPOINT ["/fsconsole"]
