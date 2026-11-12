FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
COPY web ./web
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/fsdeck ./cmd/fsdeck \
    && mkdir /out/data

FROM scratch
COPY --from=build /out/fsdeck /fsdeck
COPY --from=build --chown=65534:65534 /out/data /data
USER 65534:65534
ENV FSDECK_LISTEN=0.0.0.0:8080 \
    FSDECK_DATA=/data/fsdeck.json
EXPOSE 8080
VOLUME /data
ENTRYPOINT ["/fsdeck"]
