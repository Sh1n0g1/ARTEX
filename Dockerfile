# syntax=docker/dockerfile:1
#
# Multi-stage build: frontend static export -> single Go binary (frontend embedded) -> runtime image.
# The original Docker Hub image source (autumn27/artex) is gone; this repo now supports self-service
# builds straight from source:
#   docker compose build        # or docker build -t artex:local .
# TARGETARCH is injected automatically by buildx (amd64/arm64), so Go cross-compilation needs no QEMU;
# the runtime layer only installs tools — all compilation happens in the build stages.
# Module sources can be overridden for networks behind the Great Firewall (defaults are already
# friendly to mainland China):
#   --build-arg GOPROXY=https://goproxy.cn,direct
#   --build-arg NPM_REGISTRY=https://registry.npmmirror.com

########## Stage 1: frontend static export ##########
FROM node:20-bookworm AS web
ARG NPM_REGISTRY=https://registry.npmmirror.com
WORKDIR /web
# --ignore-scripts: skip prepare (husky) -- there's no .git in the container, so husky would fail;
# Next.js/Biome platform binaries go through optionalDependencies and don't rely on lifecycle scripts.
COPY web/package.json web/package-lock.json ./
RUN npm config set registry "$NPM_REGISTRY" && npm ci --ignore-scripts
COPY web/ ./
RUN NEXT_EXPORT=1 npm run build:static

########## Stage 2: Go backend (frontend embedded, statically compiled) ##########
FROM golang:1.26-bookworm AS gobuild
ARG TARGETARCH
ARG GOPROXY=https://goproxy.cn,https://proxy.golang.org,direct
WORKDIR /src
COPY go.mod go.sum ./
# go.mod uses a replace directive pointing at local ./norma, so its module files must be in place
# before downloading dependencies
COPY norma/go.mod norma/go.sum ./norma/
RUN go mod download
COPY . .
# Frontend static output is copied in from the web stage (.dockerignore already excludes the host's
# dist, so there's no conflict)
COPY --from=web /web/out/ server/webui/dist/
RUN CGO_ENABLED=0 GOARCH="${TARGETARCH}" go build -tags embedui -trimpath -o /out/artex ./cmd/artex

########## Stage 3: runtime image (same as before: tools + Playwright preinstalled) ##########
FROM python:3.12-slim-bookworm
ARG NPM_REGISTRY=https://registry.npmmirror.com
# Global npm installs and the chromium download in the runtime layer also go through the mirror
# (still reachable from networks outside mainland China)
ENV npm_config_registry=$NPM_REGISTRY \
    PLAYWRIGHT_DOWNLOAD_HOST=https://cdn.npmmirror.com/binaries/playwright
# Common tools: ripgrep / curl / vim, plus a set of recon staples (add/remove as needed).
# Node is installed from NodeSource at 20.x: bookworm's apt nodejs is 18, and Playwright requires >=20.
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates ripgrep curl wget vim git jq unzip \
      dnsutils iputils-ping netcat-openbsd inetutils-telnet whois nmap \
    && curl -fsSL https://deb.nodesource.com/setup_20.x | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && rm -rf /var/lib/apt/lists/*
# Preinstall Playwright MCP and CLI (globally) so there's no npx network download at runtime.
# @playwright/mcp: browser MCP can run `npx @playwright/mcp` directly (already installed globally,
# no need for -y/@latest).
# @playwright/cli: provides playwright-cli; run --help right after install to confirm it works.
# Then install playwright (provides browser management) and use --with-deps to preinstall chromium
# and its system dependencies, so the MCP/CLI works on first launch in the container with no
# browser download needed over the network.
RUN npm install -g @playwright/mcp@latest @playwright/cli@latest playwright@latest \
    && playwright-cli --help \
    && playwright install --with-deps chromium \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /app
# Single binary produced by the build stage (frontend embedded)
COPY --from=gobuild /out/artex /app/artex
# Supervising startup script: after the process exits, it decides whether to relaunch it based on
# the exit code, which is how the one-click update button in the UI completes a swap.
# It's also responsible for forwarding SIGTERM to artex -- docker stop only sends the signal to
# PID 1, and without forwarding, artex never receives it, can't shut down gracefully, and gets
# hard-killed by SIGKILL after 10 seconds.
COPY start.sh /app/start.sh
RUN chmod +x /app/artex /app/start.sh
COPY skills/ /app/skills/
# data/ (SQLite + jwt.key) persistence mount point
VOLUME ["/app/data"]
EXPOSE 8787 8788
ENTRYPOINT ["/app/start.sh"]
CMD ["-addr", ":8787", "-proxy", ":8788"]
