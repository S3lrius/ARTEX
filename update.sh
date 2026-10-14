#!/usr/bin/env bash
# ARTEX updater: ① Docker update (rebuild the image and recreate)  ② local rebuild (rebuild the binary)
# Counterpart of install.sh: install does the first setup, update upgrades to a new version.
# Database migrations do not need to be run by hand — artex re-applies schema.sql idempotently
# on every start (including ADD COLUMN / CREATE INDEX IF NOT EXISTS), so a restart is a migration.
# Data (the pgdata volume, ./data, and ./skills) is left alone.
set -euo pipefail
cd "$(cd "$(dirname "$0")" && pwd)"

info(){ printf '\033[36m[*]\033[0m %s\n' "$*"; }
ok(){   printf '\033[32m[+]\033[0m %s\n' "$*"; }
warn(){ printf '\033[33m[!]\033[0m %s\n' "$*"; }
die(){  printf '\033[31m[x]\033[0m %s\n' "$*" >&2; exit 1; }
ask(){  local p="$1" d="${2:-}" a; read -rp "$p${d:+ [$d]}: " a; echo "${a:-$d}"; }

# ── optional: sync the repo to the latest code (compose, scripts, and local build sources all depend on this) ───────
sync_repo(){
  [ -d .git ] && command -v git >/dev/null 2>&1 || { warn "not a git working copy; skipping git pull"; return; }
  [ "$(ask 'Pull the latest code (git pull --ff-only)? (y/n)' y)" = y ] || return
  if ! git pull --ff-only; then
    warn "git pull could not fast-forward (local changes or diverged branches) — fix that by hand and retry; this run keeps the current code"
  fi
}

# ── ① Docker update ───────────────────────────────
update_docker(){
  command -v docker >/dev/null 2>&1 && docker compose version >/dev/null 2>&1 \
    || die "docker / docker compose not found; install with ./install.sh first"
  [ -f .env ] || die ".env not found; run ./install.sh for the first deploy"

  # Touch only artex: postgres is pinned to 16-alpine and does not need to follow
  # (pulling it only wastes bandwidth, and a major-version jump can break compatibility).
  # artex declares depends_on postgres, so `up` with the service name starts postgres
  # if it is down, and leaves an already-running postgres as-is without recreating it.
  info "building ARTEX from this source…"
  docker compose build artex
  info "recreating and starting (artex migrates the schema on restart)…"
  docker compose up -d artex
  ok "update finished → http://localhost:8787"
  info "logs: docker compose logs -f artex"
  info "remove old images (optional): docker image prune -f"
}

# ── ② local rebuild ──────────────────────────────
update_local(){
  command -v go >/dev/null 2>&1 || die "Go not found (>=1.26): https://go.dev/dl/"
  [ -f config.json ] || warn "config.json not found — for a first deploy, use ./install.sh instead"
  ok "Go: $(go version)"

  if command -v npm >/dev/null 2>&1; then
    info "rebuilding frontend static assets…"
    ( cd web && npm ci && npm run build:static )
    rm -rf server/webui/dist && cp -r web/out server/webui/dist
    info "recompiling the single binary with the frontend embedded…"
    CGO_ENABLED=0 go build -tags embedui -trimpath -o artex ./cmd/artex
  else
    warn "npm not found: building a backend **without the embedded frontend** (run the frontend separately with npm run dev)"
    CGO_ENABLED=0 go build -o artex ./cmd/artex
  fi
  ok "build finished → ./artex"
  warn "restart the running artex process for this to take effect (the schema migrates on restart)"
}

echo "=============================="
echo "  ARTEX update"
echo "  1) Docker update (rebuild the image and recreate)"
echo "  2) Local update (rebuild with go)"
echo "=============================="
case "$(ask 'Choice' 1)" in
  1) sync_repo; update_docker ;;
  2) sync_repo; update_local ;;
  *) die "invalid choice" ;;
esac
