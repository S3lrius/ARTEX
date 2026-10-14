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
FROM node:20-bookworm@sha256:8f693eaa7e0a8e71560c9a82b55fd54c2ae920a2ba5d2cde28bac7d1c01c9ba5 AS web
ARG NPM_REGISTRY=https://registry.npmjs.org
WORKDIR /web
# --ignore-scripts: skip prepare (husky). There is no .git in the container, so husky fails.
# Next.js/Biome platform binaries come from optionalDependencies and do not need lifecycle scripts.
COPY web/package.json web/package-lock.json ./
RUN npm config set registry "$NPM_REGISTRY" && npm ci --ignore-scripts
COPY web/ ./
RUN NEXT_EXPORT=1 npm run build:static

########## Stage 2: Go backend (embedded frontend, static build) ##########
FROM golang:1.26-bookworm@sha256:d9c68c2c51161e12fd77e4c6320687c9cd86e1af1e3ad6e6cd63ff970641453c AS gobuild
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

########## Stage 3: Chromium already installed in the pinned Playwright image ##########
FROM mcr.microsoft.com/playwright:v1.64.1-noble@sha256:7852dc5a7d72f5fe458a597d690f6c42fbc8e9e26ddfa95f8e6ef552558d9bbf AS browsers

########## Stage 4: runtime image (tools + pinned Node and Chromium) ##########
FROM python:3.12-slim-bookworm@sha256:34386ef0cb081344d7ec1c103ba398e6e9f64e9ab3a1509accc92a4e24a07258
ARG NPM_REGISTRY=https://registry.npmjs.org
# Runtime npm installs use the same registry as the frontend stage.
ENV npm_config_registry=$NPM_REGISTRY \
    PLAYWRIGHT_BROWSERS_PATH=/ms-playwright
# Node comes from the official image above. Bookworm's apt nodejs is 18, and Playwright requires >=20.
COPY --from=web /usr/local/bin/node /usr/local/bin/node
COPY --from=web /usr/local/bin/npm /usr/local/bin/npm
COPY --from=web /usr/local/bin/npx /usr/local/bin/npx
COPY --from=web /usr/local/lib/node_modules /usr/local/lib/node_modules
# Chromium, its headless shell, and ffmpeg from the pinned Playwright image. This build does not download them.
COPY --from=browsers /ms-playwright /ms-playwright
# Common tools: ripgrep / curl / vim, plus a set of recon utilities (add or remove as needed).
# libatomic1 is a runtime dependency of the official Node binary.
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates ripgrep curl wget vim git jq unzip libatomic1 \
      dnsutils iputils-ping netcat-openbsd inetutils-telnet whois nmap \
    && rm -rf /var/lib/apt/lists/*
# Preinstall pinned Playwright packages so runtime does not npx-download them.
# Versions checked 2026-10-14 against the public compromised-package list; none of these are on it.
# --ignore-scripts skips package lifecycle hooks. install-deps adds OS libraries only.
RUN npm install -g --ignore-scripts @playwright/mcp@0.0.83 @playwright/cli@0.1.22 playwright@1.64.1 \
    && playwright-cli --help \
    && playwright install-deps chromium \
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
