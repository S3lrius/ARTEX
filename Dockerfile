# syntax=docker/dockerfile:1
#
# Multi-stage build: frontend static export → single Go binary (frontend embedded) → runtime image.
# This fork builds the image from source. It does not pull a published ARTEX image.
#   docker compose build        # or docker build -t artex:local .
# TARGETARCH is injected by buildx (amd64/arm64). Go cross-compiles without QEMU.
# The runtime stage only installs tools; compilation finishes entirely in the build stages.
# Defaults use the public Go and npm registries. On a network that needs a mirror:
#   --build-arg GOPROXY=https://goproxy.cn,direct
#   --build-arg NPM_REGISTRY=https://registry.npmmirror.com

########## Stage 1: frontend static export ##########
FROM node:20-bookworm AS web
ARG NPM_REGISTRY=https://registry.npmjs.org
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
ARG GOPROXY=https://proxy.golang.org,direct
ENV GOPROXY=$GOPROXY
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# Copy the frontend static output from the web stage (.dockerignore already excludes the host dist, so they do not collide)
COPY --from=web /web/out/ server/webui/dist/
RUN CGO_ENABLED=0 GOARCH="${TARGETARCH}" go build -tags embedui -trimpath -o /out/artex ./cmd/artex

########## Stage 3: runtime image (same as the original: tools + Playwright preinstalled) ##########
FROM python:3.12-slim-bookworm
ARG NPM_REGISTRY=https://registry.npmjs.org
# Runtime npm installs use the same registry as the frontend stage.
ENV npm_config_registry=$NPM_REGISTRY
# Common tools: ripgrep / curl / vim, plus a set of recon utilities (add or remove as needed).
# Node 20.x comes from NodeSource: bookworm's apt nodejs is 18, and Playwright requires >=20.
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates ripgrep curl wget vim git jq unzip \
      dnsutils iputils-ping netcat-openbsd inetutils-telnet whois nmap \
    && curl -fsSL https://deb.nodesource.com/setup_20.x | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && rm -rf /var/lib/apt/lists/*
# Preinstall pinned Playwright packages so runtime does not npx-download them.
# Versions checked 2026-10-14 against the public compromised-package list; none of these are on it.
# --ignore-scripts skips package lifecycle hooks. Chromium is installed by the next command.
RUN npm install -g --ignore-scripts @playwright/mcp@0.0.83 @playwright/cli@0.1.22 playwright@1.64.1 \
    && playwright-cli --help \
    && playwright install --with-deps chromium \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /app
# Single binary produced by the build stage (frontend embedded)
COPY --from=gobuild /out/artex /app/artex
# Supervisor: after a crash it starts the process again.
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
