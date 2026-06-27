FROM golang:1.26-alpine AS builder

RUN apk add --no-cache git build-base sqlite-dev

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY cmd/ ./cmd/
COPY data/tools/ ./data/tools/
COPY data/memory/soul/soul.md ./data/memory/soul/soul.md
COPY internal/ ./internal/

RUN CGO_ENABLED=1 go build -o oswald-agent ./cmd/agent/main.go

FROM alpine:3.23

LABEL org.opencontainers.image.source="https://github.com/jonahgcarpenter/oswald-ai"

RUN apk add --no-cache ca-certificates tzdata libstdc++ sqlite-libs

RUN addgroup -S oswald-group && adduser -S oswald-ai -G oswald-group
RUN mkdir -p /data/database && chown -R oswald-ai:oswald-group /data

WORKDIR /home/oswald-ai/

COPY --from=builder --chown=oswald-ai:oswald-group /app/oswald-agent .

RUN chmod +x ./oswald-agent

COPY --from=builder --chown=oswald-ai:oswald-group /app/data/ ./data/

USER oswald-ai

EXPOSE 8000

CMD ["./oswald-agent"]
