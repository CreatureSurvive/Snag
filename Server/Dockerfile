# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.23-alpine AS build
ARG TARGETOS TARGETARCH VERSION=dev
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/snag-server ./cmd/snag-server
RUN mkdir -p /out/data

FROM gcr.io/distroless/static-debian12:nonroot
LABEL org.opencontainers.image.title="snag-server" \
      org.opencontainers.image.description="Self-hosted inbox for Snag bug reports, with forwarding to GitHub, Discord, Slack, webhooks and email" \
      org.opencontainers.image.source="https://github.com/CreatureSurvive/Snag" \
      org.opencontainers.image.licenses="MIT"
COPY --from=build /out/snag-server /snag-server
COPY --from=build --chown=nonroot:nonroot /out/data /data
ENV SNAG_ADDR=:8080 SNAG_DATA_DIR=/data
EXPOSE 8080
VOLUME ["/data"]
USER nonroot:nonroot
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s CMD ["/snag-server", "healthcheck"]
ENTRYPOINT ["/snag-server"]
