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
COPY --from=builder /app/health.html .

RUN chown -R appuser:appgroup /app

USER appuser

EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
    CMD wget -qO- http://localhost:8080/health || exit 1

ENTRYPOINT ["./tts-api"]
