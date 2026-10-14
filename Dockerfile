# syntax=docker/dockerfile:1
#
# Multi-stage build: frontend static export → single Go binary (frontend embedded) → runtime image.
# The original Docker Hub image (autumn27/artex) is gone. This repository can now be built from source:
#   docker compose build        # or docker build -t artex:local .
# TARGETARCH is injected by buildx (amd64/arm64). Go cross-compiles without QEMU.
# The runtime stage only installs tools; compilation finishes entirely in the build stages.
# On networks inside China you can override the module sources (the defaults are already China-friendly):
#   --build-arg GOPROXY=https://goproxy.cn,direct
#   --build-arg NPM_REGISTRY=https://registry.npmmirror.com

########## Stage 1: frontend static export ##########
FROM node:20-bookworm AS web
ARG NPM_REGISTRY=https://registry.npmmirror.com
WORKDIR /web
# --ignore-scripts: skip prepare (husky). There is no .git in the container, so husky fails.
# Next.js/Biome platform binaries come from optionalDependencies and do not need lifecycle scripts.
COPY web/package.json web/package-lock.json ./
RUN npm config set registry "$NPM_REGISTRY" && npm ci --ignore-scripts
COPY web/ ./
RUN NEXT_EXPORT=1 npm run build:static

########## Stage 2: Go backend (embedded frontend, static build) ##########
FROM golang:1.26-bookworm AS gobuild
ARG TARGETARCH
ARG GOPROXY=https://goproxy.cn,https://proxy.golang.org,direct
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# Copy the frontend static output from the web stage (.dockerignore already excludes the host dist, so they do not collide)
COPY --from=web /web/out/ server/webui/dist/
RUN CGO_ENABLED=0 GOARCH="${TARGETARCH}" go build -tags embedui -trimpath -o /out/artex ./cmd/artex

########## Stage 3: runtime image (same as the original: tools + Playwright preinstalled) ##########
FROM python:3.12-slim-bookworm
ARG NPM_REGISTRY=https://registry.npmmirror.com
# Global npm installs and the Chromium download in the runtime stage also use China mirrors (reachable from the public internet as well)
ENV npm_config_registry=$NPM_REGISTRY \
    PLAYWRIGHT_DOWNLOAD_HOST=https://cdn.npmmirror.com/binaries/playwright
# Common tools: ripgrep / curl / vim, plus a set of recon utilities (add or remove as needed).
# Node 20.x comes from NodeSource: bookworm's apt nodejs is 18, and Playwright requires >=20.
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates ripgrep curl wget vim git jq unzip \
      dnsutils iputils-ping netcat-openbsd inetutils-telnet whois nmap \
    && curl -fsSL https://deb.nodesource.com/setup_20.x | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && rm -rf /var/lib/apt/lists/*
# Preinstall Playwright MCP and CLI globally so runtime does not npx-download them.
# @playwright/mcp: browser MCP is invoked as `npx @playwright/mcp` (already global; no -y/@latest).
# @playwright/cli: provides playwright-cli; --help after install checks that it runs.
# Then install playwright (browser management) and use --with-deps to prefetch Chromium and its system libraries,
# so MCP/CLI work on first start inside the container without downloading a browser.
RUN npm install -g @playwright/mcp@latest @playwright/cli@latest playwright@latest \
    && playwright-cli --help \
    && playwright install --with-deps chromium \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /app
# Single binary produced by the build stage (frontend embedded)
COPY --from=gobuild /out/artex /app/artex
# Supervisor: after the process exits, the exit code decides whether to start it again.
# The one-click update in the UI depends on this swap.
# It also forwards SIGTERM to artex — docker stop sends the signal only to PID 1.
# Without forwarding, artex never receives it, cannot shut down cleanly, and is SIGKILL'd after 10 seconds.
COPY start.sh /app/start.sh
RUN chmod +x /app/artex /app/start.sh
COPY skills/ /app/skills/
# Persistence point for data/ (SQLite + jwt.key)
VOLUME ["/app/data"]
EXPOSE 8787 8788
ENTRYPOINT ["/app/start.sh"]
CMD ["-addr", ":8787", "-proxy", ":8788"]
