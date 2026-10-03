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
COPY internal/ ./internal/

RUN CGO_ENABLED=1 go build -tags sqlite_fts5 -o oswald ./cmd/oswald
RUN CGO_ENABLED=1 go build -tags sqlite_fts5 -o oswald-server ./cmd/oswald-server

FROM debian:bookworm-slim

LABEL org.opencontainers.image.source="https://github.com/jonahgcarpenter/oswald-ai"

RUN apt-get update && apt-get install -y --no-install-recommends \
  ca-certificates \
  tzdata \
  libsqlite3-0 \
  sqlite3 \
  libstdc++6 \
  ffmpeg \
  && rm -rf /var/lib/apt/lists/*

RUN groupadd --system oswald-group && useradd --system --gid oswald-group oswald-ai

# Binaries live in /opt/oswald, owned by root and read-only to the service user.
COPY --from=builder /app/oswald /opt/oswald/oswald
COPY --from=builder /app/oswald-server /opt/oswald/oswald-server

# Operator data lives at the filesystem-root .oswald, owned by the service user.
RUN install -d -m 0700 -o oswald-ai -g oswald-group /.oswald

ENV PATH="/opt/oswald:${PATH}"

USER oswald-ai
WORKDIR /

EXPOSE 8000

CMD ["oswald-server"]
