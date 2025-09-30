# syntax=docker/dockerfile:1.4
FROM golang:1.25-bookworm AS build
WORKDIR /app
# Tools needed for module fetching and CGO builds (go-fitz)
RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates git build-essential pkg-config \
    libmupdf-dev libjpeg62-turbo-dev libopenjp2-7-dev \
    libjbig2dec0-dev libfreetype6-dev libharfbuzz-dev libmujs-dev \
    && rm -rf /var/lib/apt/lists/*
# Cache Go modules first (copies go.mod and go.sum if present)
COPY go.* ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download all

# Copy the source
COPY . .

# Build with CGO (cache build artifacts)
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=1 GOOS=linux GOARCH=amd64 GOFLAGS=-mod=mod go build -o /app/bin/aidispatcher ./cmd/app

FROM debian:bookworm-slim
WORKDIR /app
COPY --from=build /app/bin /app/bin
COPY --from=build /app/web /app/web
COPY scripts/dev_entrypoint.sh /app/dev_entrypoint.sh
RUN chmod +x /app/dev_entrypoint.sh && \
    mkdir -p /app/logs && apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates mupdf-tools libopenjp2-7 libjbig2dec0 libfreetype6 libharfbuzz0b \
    libreoffice && \
    rm -rf /var/lib/apt/lists/*
ENV PORT=8080
CMD ["/app/dev_entrypoint.sh"]
