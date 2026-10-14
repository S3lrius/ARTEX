#!/usr/bin/env bash
# Dev mode: backend (:8787) + traffic proxy (:8788) together with the frontend next dev (:5173).
# The frontend proxies /api to the backend. Ctrl-C stops both.
#
# For the single binary (frontend embedded), see the "single binary" section of the README. This script does not do that.
set -euo pipefail
cd "$(dirname "$0")"

# On exit, stop every child in this process group (backend + frontend).
cleanup() { kill 0 2>/dev/null || true; }
trap cleanup EXIT INT TERM

# Backend (plain go run, frontend not embedded). Work-agent concurrency is set in System settings.
go run ./cmd/artex -addr :8787 -proxy 127.0.0.1:8788 &

# Frontend hot reload (Vite/Next dev server; /api is proxied to :8787).
( cd web && npm run dev ) &

echo "[dev] backend :8787 / proxy :8788 / frontend http://localhost:5173  (Ctrl-C to quit)"
wait
