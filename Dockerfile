FROM golang:1.27.1-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=0.1.0
RUN CGO_ENABLED=0 GOMAXPROCS=1 GOMEMLIMIT=512MiB go build -p=1 -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/youtube-tv ./cmd/youtube-tv

FROM node:22-bookworm-slim AS javascript

FROM python:3.12-slim-bookworm
ARG YTDLP_VERSION=2026.8.19
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates libstdc++6 \
    && rm -rf /var/lib/apt/lists/* \
    && pip install --no-cache-dir "yt-dlp[default]==${YTDLP_VERSION}" \
    && groupadd --gid 10001 youtube-tv \
    && useradd --uid 10001 --gid youtube-tv --no-create-home youtube-tv \
    && mkdir -p /data && chown 10001:10001 /data
COPY --from=javascript /usr/local/bin/node /usr/local/bin/node
COPY --from=build /out/youtube-tv /usr/local/bin/youtube-tv
ENV DATA_DIR=/data LISTEN_ADDR=0.0.0.0:9000 JS_RUNTIME=node PYTHONDONTWRITEBYTECODE=1 \
    GOMEMLIMIT=192MiB GOMAXPROCS=1
USER 10001:10001
WORKDIR /data
VOLUME ["/data"]
EXPOSE 9000
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 CMD ["youtube-tv", "-healthcheck"]
ENTRYPOINT ["youtube-tv"]
