FROM golang:1.25.7-bookworm AS build-source

ENV GOTOOLCHAIN=local

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
COPY data/workflows/ ./data/workflows/
COPY data/memory/soul/soul.md ./data/memory/soul/soul.md
COPY internal/ ./internal/

FROM build-source AS builder

RUN CGO_ENABLED=1 go build -tags sqlite_fts5 -o oswald-agent ./cmd/agent/main.go

FROM debian:bookworm-slim AS document-runtime

LABEL org.opencontainers.image.source="https://github.com/jonahgcarpenter/oswald-ai"

RUN apt-get update && apt-get install -y --no-install-recommends \
  ca-certificates \
  tzdata \
  libsqlite3-0 \
  sqlite3 \
  libstdc++6 \
  ffmpeg \
  poppler-utils \
  tesseract-ocr \
  tesseract-ocr-eng \
  libreoffice-writer \
  libreoffice-calc \
  libreoffice-impress \
  fonts-dejavu-core \
  fonts-liberation \
  util-linux \
  && rm -rf /var/lib/apt/lists/*

RUN groupadd --system oswald-group && useradd --system --gid oswald-group oswald-ai
RUN mkdir -p /home/oswald-ai/data/database \
  && chown -R oswald-ai:oswald-group /home/oswald-ai \
  && chmod 700 /home/oswald-ai/data/database

WORKDIR /home/oswald-ai/

# Both test and production images inherit exactly the same document parsers.
FROM document-runtime AS document-tests

RUN apt-get update && apt-get install -y --no-install-recommends \
  build-essential \
  libsqlite3-dev \
  pkg-config \
  && rm -rf /var/lib/apt/lists/*

COPY --from=build-source /usr/local/go/ /usr/local/go/
COPY --from=build-source /go/pkg/mod/ /go/pkg/mod/

WORKDIR /app
COPY --from=build-source /app/go.mod /app/go.sum ./
COPY --from=build-source /app/internal/ ./internal/
COPY --from=build-source /app/data/ ./data/

ENV PATH="/usr/local/go/bin:${PATH}" \
  GOTOOLCHAIN=local \
  GOPROXY=off \
  GOSUMDB=off \
  GOMODCACHE=/go/pkg/mod \
  GOCACHE=/tmp/go-build \
  GOTMPDIR=/tmp \
  CGO_ENABLED=1

USER oswald-ai

RUN --network=none go test -count=1 -tags=sqlite_fts5,document_integration ./internal/documents

# Run with docker run --rm --network=none <document-tests image>.
ENTRYPOINT ["go", "test", "-count=1", "-v", "-tags=sqlite_fts5,document_integration", "./internal/documents"]

FROM document-runtime AS runtime

COPY --from=builder --chown=oswald-ai:oswald-group /app/oswald-agent .

RUN chmod +x ./oswald-agent

COPY --from=builder --chown=oswald-ai:oswald-group /app/data/ ./data/

USER oswald-ai

EXPOSE 8000

CMD ["./oswald-agent"]
