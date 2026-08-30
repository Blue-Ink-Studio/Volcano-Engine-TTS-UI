FROM golang:1.26-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

# VERSION 由 CI/CD 传入,通常为 `git describe --tags --always --dirty` 的输出
# COMMIT 为 `git rev-parse --short HEAD`
# 本地默认 dev
ARG VERSION=dev
ARG COMMIT=dev
RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags "-X github.com/volcano-tts/tts-api/version.Version=${VERSION} \
              -X github.com/volcano-tts/tts-api/version.Commit=${COMMIT}" \
    -o tts-api .

FROM alpine:3.21

RUN apk --no-cache add ca-certificates tzdata \
    && addgroup -S appgroup && adduser -S appuser -G appgroup

WORKDIR /app

COPY --from=builder /app/tts-api .
# health.html 已通过 //go:embed 嵌入 binary,无需单独复制

# 准备 /data 目录存 SQLite (tts.db + installed.lock)
# appuser 必须可写,否则启动后无法创建 DB
RUN mkdir -p /data \
    && chown -R appuser:appgroup /app /data

USER appuser

EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD wget -qO- http://localhost:8080/health || exit 1

ENTRYPOINT ["./tts-api"]
