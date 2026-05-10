FROM golang:1.25-alpine AS builder

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Version/build/commit stamping. Accepts --build-arg overrides; otherwise
# derives from .version + `date -u` + `git rev-parse --short HEAD` at build
# time. Matches the v3 reference pattern (one -ldflags -X per stamped field).
ARG VERSION
ARG BUILD
ARG GIT_COMMIT

RUN apk add --no-cache git && \
    VERSION="${VERSION:-$(awk '/^\[satellites\]/{f=1;next} /^\[/{f=0} f && /^version/{gsub(/version\s*=\s*/,""); gsub(/ /,""); print; exit}' .version 2>/dev/null || echo dev)}" && \
    BUILD="${BUILD:-$(date -u +"%Y-%m-%d-%H-%M-%S")}" && \
    GIT_COMMIT="${GIT_COMMIT:-$(git rev-parse --short HEAD 2>/dev/null || echo unknown)}" && \
    go build -ldflags="-s -w \
        -X 'github.com/bobmcallan/satellites/internal/config.Version=${VERSION}' \
        -X 'github.com/bobmcallan/satellites/internal/config.Build=${BUILD}' \
        -X 'github.com/bobmcallan/satellites/internal/config.GitCommit=${GIT_COMMIT}'" \
        -o /bin/satellites-server ./cmd/satellites-server && \
    go build -ldflags="-s -w \
        -X 'github.com/bobmcallan/satellites/internal/config.Version=${VERSION}' \
        -X 'github.com/bobmcallan/satellites/internal/config.Build=${BUILD}' \
        -X 'github.com/bobmcallan/satellites/internal/config.GitCommit=${GIT_COMMIT}'" \
        -o /bin/satellites-agent ./cmd/satellites-agent && \
    go build -ldflags="-s -w \
        -X 'github.com/bobmcallan/satellites/internal/config.Version=${VERSION}' \
        -X 'github.com/bobmcallan/satellites/internal/config.Build=${BUILD}' \
        -X 'github.com/bobmcallan/satellites/internal/config.GitCommit=${GIT_COMMIT}'" \
        -o /bin/satellites-client ./cmd/satellites-client

FROM alpine:3.21

# Chromium + the freetype/font stack are needed by the portal_replicate
# MCP tool (sty_088f6d5c) — chromedp drives the runtime browser. The
# swiftshader package is the software GPU; without it chromium fails to
# initialise on machines without /dev/dri. nss + freetype + harfbuzz +
# ca-certificates cover TLS + glyph rendering. Adds ~150 MB to the
# image; acceptable for the unlock value (live UI replication).
RUN apk add --no-cache \
    ca-certificates tzdata curl bash git \
    chromium chromium-swiftshader \
    nss freetype freetype-dev harfbuzz \
    ttf-freefont font-noto-emoji

# chromedp picks up CHROME_BIN as the binary path. Alpine's chromium
# package installs to /usr/bin/chromium-browser.
ENV CHROME_BIN=/usr/bin/chromium-browser

WORKDIR /app

COPY --from=builder /bin/satellites-server /app/satellites-server
COPY --from=builder /bin/satellites-agent /app/satellites-agent
COPY --from=builder /bin/satellites-client /app/satellites-client
COPY docker/entrypoint-local.sh /app/docker/
# story_7bfd629c: ship the system-tier configuration markdown into the
# image. The boot path resolves these via SATELLITES_SEED_DIR /
# SATELLITES_HELP_DIR, defaulting to ./config/seed and ./config/help.
COPY config /app/config
RUN chmod +x /app/docker/entrypoint-local.sh

EXPOSE 8080

ENTRYPOINT ["/app/satellites-server"]
