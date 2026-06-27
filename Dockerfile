FROM golang:1.25-bookworm AS builder

RUN apt-get update && apt-get install -y --no-install-recommends \
  git \
  build-essential \
  libsqlite3-dev \
  pkg-config \
  && rm -rf /var/lib/apt/lists/*

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY cmd/ ./cmd/
COPY data/tools/ ./data/tools/
COPY data/memory/soul/soul.md ./data/memory/soul/soul.md
COPY internal/ ./internal/

RUN CGO_ENABLED=1 go build -o oswald-agent ./cmd/agent/main.go

FROM debian:bookworm-slim

LABEL org.opencontainers.image.source="https://github.com/jonahgcarpenter/oswald-ai"

RUN apt-get update && apt-get install -y --no-install-recommends \
  ca-certificates \
  tzdata \
  libsqlite3-0 \
  sqlite3 \
  libstdc++6 \
  && rm -rf /var/lib/apt/lists/*

RUN groupadd --system oswald-group && useradd --system --gid oswald-group oswald-ai
RUN mkdir -p /data/database && chown -R oswald-ai:oswald-group /data

WORKDIR /home/oswald-ai/

COPY --from=builder --chown=oswald-ai:oswald-group /app/oswald-agent .

RUN chmod +x ./oswald-agent

COPY --from=builder --chown=oswald-ai:oswald-group /app/data/ ./data/

USER oswald-ai

EXPOSE 8000

CMD ["./oswald-agent"]
