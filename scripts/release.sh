#!/usr/bin/env bash
# scripts/release.sh — cut a GitHub release with arm64 binaries + deploy files.
#
# Usage:
#   ./scripts/release.sh v0.1.0
#
# Requires: docker buildx, gh (authenticated as a user who can push releases
# to the repo).
#
# The installer (scripts/install.sh) fetches assets by exactly these names:
#   monitor-linux-arm64  oauth-linux-arm64  tibber-discover-linux-arm64
#   Dockerfile  docker-compose.yml  .env.example
# So keep the naming in lock-step if you change either file.

set -euo pipefail

TAG="${1:-}"
[[ -n "$TAG" ]] || { echo "usage: $0 <tag>   (e.g. $0 v0.1.0)" >&2; exit 2; }

ROOT=$(git rev-parse --show-toplevel)
cd "$ROOT"

command -v docker >/dev/null || { echo "need docker on PATH" >&2; exit 1; }
command -v gh     >/dev/null || { echo "need gh on PATH"     >&2; exit 1; }

# Build arm64 binaries via the dedicated build image → host dist/ dir.
rm -rf dist
mkdir  dist
docker buildx build \
  -f Dockerfile.build \
  --platform linux/arm64 \
  --output type=local,dest=./dist \
  .

mv dist/monitor         dist/monitor-linux-arm64
mv dist/oauth           dist/oauth-linux-arm64
mv dist/tibber-discover dist/tibber-discover-linux-arm64

# Verify (at least) the monitor binary is a real arm64 ELF.
file dist/monitor-linux-arm64 | grep -q "ARM aarch64" \
  || { echo "build did not produce an arm64 binary — check docker buildx setup" >&2; exit 1; }

gh release create "$TAG" \
  --title "$TAG" \
  --generate-notes \
  dist/monitor-linux-arm64 \
  dist/oauth-linux-arm64 \
  dist/tibber-discover-linux-arm64 \
  Dockerfile \
  docker-compose.yml \
  .env.example

echo
echo "Released $TAG. Pi install one-liner:"
echo
echo "  curl -fsSL https://raw.githubusercontent.com/tokko/volvo-tibber-sync/main/scripts/install.sh | bash"
