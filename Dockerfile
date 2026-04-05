FROM golang:1.26-alpine AS builder

RUN apk add --no-cache git

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY cmd/ ./cmd/
COPY config/tools/ ./config/tools/
COPY config/soul.md ./config/soul.md
COPY internal/ ./internal/

RUN CGO_ENABLED=0 GOOS=linux go build -o oswald-agent ./cmd/agent/main.go

FROM alpine:3.23

LABEL org.opencontainers.image.source="https://github.com/jonahgcarpenter/oswald-ai"

RUN apk add --no-cache ca-certificates tzdata

RUN addgroup -S oswald-group && adduser -S oswald-ai -G oswald-group

WORKDIR /home/oswald-ai/

COPY --from=builder --chown=oswald-ai:oswald-group /app/oswald-agent .

RUN chmod +x ./oswald-agent

COPY --from=builder --chown=oswald-ai:oswald-group /app/config/ ./config/

USER oswald-ai

EXPOSE 8080

CMD ["./oswald-agent"]
