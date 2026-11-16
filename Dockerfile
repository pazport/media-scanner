# ---- build ----
# Build on the runner's native platform and cross-compile for the target (no emulation needed).
FROM --platform=$BUILDPLATFORM golang:1.24-alpine AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod ./
COPY *.go ./
COPY web ./web
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -tags timetzdata -ldflags="-s -w -X main.version=${VERSION}" -o /media-scanner . \
 && mkdir -p /out/config

# ---- runtime: empty image, just the static binary ----
FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /media-scanner /media-scanner
COPY --from=build --chown=1000:1000 /out/config /config
USER 1000:1000
ENV MEDIA_SCANNER_CONFIG=/config/config.json TZ=UTC
VOLUME /config
EXPOSE 3030
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s CMD ["/media-scanner", "healthcheck"]
ENTRYPOINT ["/media-scanner"]
