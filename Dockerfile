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

# Pinned release snapshot for reproducible runtime builds; bump deliberately.
FROM debian:bookworm-20261020-slim

LABEL org.opencontainers.image.source="https://github.com/jonahgcarpenter/oswald-ai"

RUN apt-get update && apt-get install -y --no-install-recommends \
  ca-certificates \
  curl \
  gnupg \
  tzdata \
  libatomic1 \
  libsqlite3-0 \
  sqlite3 \
  libstdc++6 \
  ffmpeg \
  poppler-utils \
  python3 \
  chromium \
  fonts-liberation \
  fonts-noto-color-emoji \
  xz-utils \
  && rm -rf /var/lib/apt/lists/*

# GitHub CLI for agent-assisted repo workflows; auth comes from GH_TOKEN at
# runtime, never from the image.
RUN mkdir -p /usr/share/keyrings \
  && curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg | dd of=/usr/share/keyrings/githubcli-archive-keyring.gpg status=none \
  && chmod go+r /usr/share/keyrings/githubcli-archive-keyring.gpg \
  && echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" > /etc/apt/sources.list.d/github-cli.list \
  && apt-get update && apt-get install -y --no-install-recommends gh \
  && rm -rf /var/lib/apt/lists/*

# Node.js 26 for stdio MCP servers; package auth/config stays manual.
ARG NODE_VERSION=26.9.0
RUN curl -fsSL https://nodejs.org/dist/v${NODE_VERSION}/node-v${NODE_VERSION}-linux-x64.tar.xz \
    | tar -xJ -C /opt/ \
  && mv /opt/node-v${NODE_VERSION}-linux-x64 /opt/node \
  && ln -s /opt/node/bin/node /usr/local/bin/node \
  && ln -s /opt/node/bin/npm /usr/local/bin/npm \
  && ln -s /opt/node/bin/npx /usr/local/bin/npx \
  && node --version && npm --version

RUN groupadd --system oswald-group && useradd --system --gid oswald-group --home-dir /home/oswald --no-create-home oswald

# Home directory for the service user; operator data lives in its .oswald root.
RUN install -d -m 0755 -o oswald -g oswald-group /home/oswald
RUN install -d -m 0700 -o oswald -g oswald-group /home/oswald/.oswald

# Binaries live in /opt/oswald, owned by root and read-only to the service user.
COPY --from=builder /app/oswald /opt/oswald/oswald
COPY --from=builder /app/oswald-server /opt/oswald/oswald-server

ENV PATH="/opt/oswald:${PATH}"

USER oswald
WORKDIR /home/oswald

EXPOSE 8000

CMD ["oswald-server"]
