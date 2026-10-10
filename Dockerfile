# syntax=docker/dockerfile:1
FROM node:24-trixie-slim@sha256:173f125896c3b47ddf056734c7ea789d04595a6a08769a8f78e0df642781fb66 AS frontend
ENV PLAYWRIGHT_SKIP_BROWSER_DOWNLOAD=1
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /src/frontend
COPY frontend/package.json frontend/package-lock.json ./
RUN npm ci
COPY frontend/ ./
COPY document/format_metadata.json /src/document/format_metadata.json
RUN npm run build

# Keep the Go version aligned with go.mod: source-metadata qualification
# includes the exact toolchain version.
FROM golang:1.27.0-trixie@sha256:df98008ecd2b0ecc9f0a94d1b07e3564a9c92b555369b33d9b5f60d0765b2db7 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=frontend /src/frontend/dist/ ./internal/web/dist/
ARG VERSION=dev
ARG COMMIT=unknown
RUN CGO_ENABLED=1 go build -tags fts5 \
    -ldflags="-X go.kenn.io/docbank/internal/version.Version=${VERSION} -X go.kenn.io/docbank/internal/version.Commit=${COMMIT}" \
    -o /out/docbank ./cmd/docbank

FROM debian:trixie-slim@sha256:a29215f6a35e51e22adffa17f89e9d2ef06214e64a2bad10d765c46aea49f11f
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --gid 1000 docbank \
    && useradd --uid 1000 --gid 1000 --create-home docbank \
    && mkdir -p /data /var/lib/docbank-locks \
    && chown 1000:1000 /data /var/lib/docbank-locks \
    && chmod 0700 /data /var/lib/docbank-locks
COPY --from=build /out/docbank /usr/local/bin/docbank
ENV DOCBANK_HOME=/data \
    DOCBANK_LOCK_DIR=/var/lib/docbank-locks \
    DOCBANK_BIND_ADDR=0.0.0.0 \
    DOCBANK_API_PORT=8485
USER 1000:1000
EXPOSE 8485
ENTRYPOINT ["docbank"]
CMD ["daemon", "run"]
