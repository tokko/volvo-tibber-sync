#!/usr/bin/env bash
# scripts/release.sh — cut a GitHub release with arm64 binaries + deploy files.
#
# Usage:
#   ./scripts/release.sh v0.2.0
#
# Requires: go (1.21+), gh (authenticated as a user who can push releases).
#
# The installer (scripts/install.sh) fetches assets by exactly these names:
#   monitor-linux-arm64  oauth-linux-arm64  tibber-discover-linux-arm64
#   Dockerfile  docker-compose.yml  env.example
# Keep naming in lock-step if you change either file.

set -euo pipefail

TAG="${1:-}"
[[ -n "$TAG" ]] || { echo "usage: $0 <tag>   (e.g. $0 v0.2.0)" >&2; exit 2; }

ROOT=$(git rev-parse --show-toplevel)
cd "$ROOT"

command -v go >/dev/null || { echo "need go on PATH" >&2; exit 1; }
command -v gh >/dev/null || { echo "need gh on PATH" >&2; exit 1; }

# Cross-compile static arm64 binaries.
rm -rf dist
mkdir -p dist

echo "Building arm64 binaries..."
for cmd in monitor oauth tibber-discover; do
  GOOS=linux GOARCH=arm64 CGO_ENABLED=0 \
    go build -trimpath -ldflags="-s -w" \
    -o "dist/${cmd}-linux-arm64" "./cmd/${cmd}"
  echo "  dist/${cmd}-linux-arm64"
done

# Verify the monitor binary is a real arm64 ELF.
file dist/monitor-linux-arm64 | grep -q "ARM aarch64" \
  || { echo "build did not produce an arm64 binary" >&2; exit 1; }

# GitHub renames dotfiles in release assets — upload as env.example.
cp .env.example dist/env.example

gh release create "$TAG" \
  --title "$TAG" \
  --generate-notes \
  dist/monitor-linux-arm64 \
  dist/oauth-linux-arm64 \
  dist/tibber-discover-linux-arm64 \
  Dockerfile \
  docker-compose.yml \
  dist/env.example

echo
echo "Released $TAG. Pi install one-liner:"
echo
echo "  curl -fsSL https://raw.githubusercontent.com/tokko/volvo-tibber-sync/main/scripts/install.sh | bash"
