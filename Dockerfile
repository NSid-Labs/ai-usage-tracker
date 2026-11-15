# Cross-compiles on the build machine's platform, so multi-arch builds are fast.
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
ARG TARGETOS TARGETARCH
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
      -ldflags="-s -w -X main.version=${VERSION}" -o /out/ai-usage-tracker . \
 && mkdir -p /out/data

FROM gcr.io/distroless/static-debian12:nonroot
ARG VERSION=dev
LABEL org.opencontainers.image.title="ai-usage-tracker" \
      org.opencontainers.image.description="Self-hosted dashboard for AI subscription limits and API spend" \
      org.opencontainers.image.source="https://github.com/NSid-Labs/ai-usage-tracker" \
      org.opencontainers.image.upstream="https://github.com/Dukko/ai-usage-tracker" \
      org.opencontainers.image.licenses="MIT" \
      org.opencontainers.image.version="${VERSION}"
COPY --from=build /out/ai-usage-tracker /ai-usage-tracker
COPY --from=build --chown=65532:65532 /out/data /data
ENV TRACKER_DATA_DIR=/data TRACKER_ADDR=:8080
VOLUME /data
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s CMD ["/ai-usage-tracker", "healthcheck"]
ENTRYPOINT ["/ai-usage-tracker"]
