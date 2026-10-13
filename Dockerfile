# Multi-stage Pilot image for linux/arm64 and linux/amd64.
# Build: docker buildx build --platform linux/arm64 -t pilot:local .

ARG GO_VERSION=1.24.10

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS builder

ARG TARGETOS=linux
ARG TARGETARCH

WORKDIR /src

RUN apk add --no-cache git ca-certificates

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/pilot ./cmd/main.go \
 && CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/e2echeck ./cmd/e2echeck \
 && CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/checkdeps ./cmd/checkdeps \
 && CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/seedlogs ./cmd/seedlogs \
 && CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/agenteval ./cmd/agenteval \
 && CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/mcpserver ./cmd/mcpserver

FROM alpine:3.20

RUN apk add --no-cache ca-certificates tzdata curl \
 && adduser -D -H -u 10001 pilot

WORKDIR /app

COPY --from=builder /out/pilot /app/pilot
COPY --from=builder /out/e2echeck /app/e2echeck
COPY --from=builder /out/checkdeps /app/checkdeps
COPY --from=builder /out/seedlogs /app/seedlogs
COPY --from=builder /out/agenteval /app/agenteval
COPY --from=builder /out/mcpserver /app/mcpserver
COPY web /app/web
COPY manifest/config /app/manifest/config
COPY internal/prompts/prompts.yaml /app/internal/prompts/prompts.yaml

USER pilot

ENV PILOT_CONFIG=/app/manifest/config/config.docker.yaml
EXPOSE 8080

HEALTHCHECK --interval=10s --timeout=3s --start-period=20s --retries=5 \
  CMD curl -fsS http://127.0.0.1:8080/api/v1/health/live >/dev/null || exit 1

ENTRYPOINT ["/app/pilot"]
