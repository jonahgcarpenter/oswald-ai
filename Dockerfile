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
  curl \
  gnupg \
  tzdata \
  libsqlite3-0 \
  sqlite3 \
  libstdc++6 \
  ffmpeg \
  poppler-utils \
  python3 \
  chromium \
  fonts-liberation \
  fonts-noto-color-emoji \
  && rm -rf /var/lib/apt/lists/*

# Node.js 26 for MCP stdio servers (node/npx). The bookworm archive ships
# Node 18, below what current MCP servers require, so install a pinned
# upstream build into /usr/local — on PATH for the service and the agent
# terminal alike.
ARG NODE_VERSION=26.8.1
RUN arch=$(dpkg --print-architecture) \
  && case "$arch" in amd64) node_arch=x64;; arm64) node_arch=arm64;; *) echo "unsupported architecture $arch for Node install" >&2; exit 1;; esac \
  && tarball="node-v${NODE_VERSION}-linux-${node_arch}.tar.xz" \
  && curl -fsSL "https://nodejs.org/dist/v${NODE_VERSION}/SHASUMS256.txt" -o /tmp/SHASUMS256.txt \
  && curl -fsSL "https://nodejs.org/dist/v${NODE_VERSION}/${tarball}" -o "/tmp/${tarball}" \
  && grep "${tarball}" /tmp/SHASUMS256.txt | (cd /tmp && sha256sum -c -) \
  && tar -xJf "/tmp/${tarball}" -C /usr/local --strip-components=1 \
  && rm -f "/tmp/${tarball}" /tmp/SHASUMS256.txt \
  && node -v && npm -v && npx -v

# GitHub CLI for agent-assisted repo workflows; auth comes from GH_TOKEN at
# runtime, never from the image.
RUN mkdir -p /usr/share/keyrings \
  && curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg | dd of=/usr/share/keyrings/githubcli-archive-keyring.gpg status=none \
  && chmod go+r /usr/share/keyrings/githubcli-archive-keyring.gpg \
  && echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" > /etc/apt/sources.list.d/github-cli.list \
  && apt-get update && apt-get install -y --no-install-recommends gh \
  && rm -rf /var/lib/apt/lists/*

RUN groupadd -g 10000 oswald-group && useradd -u 10000 --gid oswald-group --home-dir /home/oswald --no-create-home oswald

# Home directory for the service user; operator data lives in its .oswald root.
RUN install -d -m 0755 -o oswald -g oswald-group /home/oswald
RUN install -d -m 0700 -o oswald -g oswald-group /home/oswald/.oswald

# Binaries live in /opt/oswald, owned by root and read-only to the service user.
COPY --from=builder /app/oswald /opt/oswald/oswald
COPY --from=builder /app/oswald-server /opt/oswald/oswald-server

ENV PATH="/opt/oswald:${PATH}"
ENV HOME=/home/oswald TMPDIR=/tmp

USER oswald
WORKDIR /home/oswald

EXPOSE 8000

CMD ["oswald-server"]
