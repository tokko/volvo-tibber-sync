#!/usr/bin/env bash
# volvo-tibber-sync — Raspberry Pi installer
#
# Usage (on the Pi):
#   curl -fsSL https://raw.githubusercontent.com/tokko/volvo-tibber-sync/main/scripts/install.sh | bash
#
# Pins a specific release with env:
#   curl -fsSL https://raw.githubusercontent.com/tokko/volvo-tibber-sync/main/scripts/install.sh | RELEASE_TAG=v0.1.0 bash
#
# What it does:
#   1. Downloads the latest release's arm64 binaries + Dockerfile + compose +
#      .env.example into ~/volvo-tibber-sync (override with INSTALL_DIR=…).
#   2. Prompts for any missing Volvo/Tibber credentials and writes them to
#      .env (existing values are kept — re-running the script is safe).
#   3. Runs the bundled `oauth` helper to complete the Volvo PKCE flow.
#   4. Runs the bundled `tibber-discover` helper to pick the mock-car vehicle.
#   5. Offers to bring the stack up via `docker compose up -d --build`.

set -euo pipefail

# --- Config ---
REPO="${REPO:-tokko/volvo-tibber-sync}"
INSTALL_DIR="${INSTALL_DIR:-$HOME/volvo-tibber-sync}"
RELEASE_TAG="${RELEASE_TAG:-latest}"

# --- Pretty output ---
if [[ -t 1 ]]; then
  B=$(tput bold); N=$(tput sgr0)
  G=$(tput setaf 2); Y=$(tput setaf 3); R=$(tput setaf 1)
else
  B=""; N=""; G=""; Y=""; R=""
fi
say()  { printf "%s==>%s %s\n" "$B$G" "$N" "$*"; }
warn() { printf "%s!!%s  %s\n" "$B$Y" "$N" "$*" >&2; }
die()  { printf "%sxx%s  %s\n" "$B$R" "$N" "$*" >&2; exit 1; }

# --- Pre-flight ---
case "$(uname -m)" in
  aarch64|arm64) PLATFORM=linux-arm64 ;;
  x86_64|amd64)  PLATFORM=linux-amd64 ;;
  *) die "unsupported arch: $(uname -m) (need aarch64 or x86_64)" ;;
esac

command -v curl >/dev/null || die "need 'curl' on PATH"

if ! command -v docker >/dev/null; then
  warn "docker not installed — you'll need it to run the compose stack."
  warn "install with: curl -fsSL https://get.docker.com | sh && sudo usermod -aG docker \$USER"
fi

# --- Download ---
mkdir -p "$INSTALL_DIR"
cd "$INSTALL_DIR"

if [[ "$RELEASE_TAG" == "latest" ]]; then
  BASE="https://github.com/$REPO/releases/latest/download"
else
  BASE="https://github.com/$REPO/releases/download/$RELEASE_TAG"
fi

fetch() {
  local remote="$1" local="${2:-$1}"
  say "downloading $remote"
  curl -fsSL "$BASE/$remote" -o "$local" || die "failed to download $remote from $BASE"
}

fetch "monitor-$PLATFORM"         monitor
fetch "oauth-$PLATFORM"           oauth
fetch "tibber-discover-$PLATFORM" tibber-discover
fetch "Dockerfile"
fetch "docker-compose.yml"
fetch ".env.example"

chmod +x monitor oauth tibber-discover

# --- .env helpers ---
if [[ ! -f .env ]]; then
  cp .env.example .env
  chmod 600 .env
fi

getenv_val() {
  local key="$1"
  local line
  line=$(grep -E "^$key=" .env 2>/dev/null | head -1 || true)
  [[ -z "$line" ]] && { printf ""; return; }
  local val="${line#*=}"
  # Strip surrounding quotes if present.
  if [[ "$val" == \"*\" ]]; then val="${val#\"}"; val="${val%\"}"; fi
  printf '%s' "$val"
}

set_env_key() {
  local key="$1" val="$2"
  local tmp; tmp=$(mktemp)
  local found=0
  while IFS= read -r line || [[ -n "$line" ]]; do
    if [[ "$line" =~ ^${key}= ]]; then
      printf '%s=%s\n' "$key" "$val" >> "$tmp"
      found=1
    else
      printf '%s\n' "$line" >> "$tmp"
    fi
  done < .env
  [[ $found -eq 0 ]] && printf '%s=%s\n' "$key" "$val" >> "$tmp"
  mv "$tmp" .env
  chmod 600 .env
}

prompt_if_blank() {
  local key="$1" label="$2" silent="${3:-}"
  local cur; cur=$(getenv_val "$key")
  if [[ -n "$cur" ]]; then return; fi
  local val
  if [[ "$silent" == "silent" ]]; then
    read -r -s -p "    $label: " val; echo
  else
    read -r -p "    $label: " val
  fi
  [[ -n "$val" ]] || die "$key is required"
  set_env_key "$key" "$val"
}

# --- Wizard ---
echo
say "Volvo Cars Developer Portal credentials"
echo "   Create an app at https://developer.volvocars.com (subscribe to the"
echo "   Energy API and note both the client id/secret and the VCC-API-Key)."
prompt_if_blank VOLVO_CLIENT_ID     "VOLVO_CLIENT_ID"
prompt_if_blank VOLVO_CLIENT_SECRET "VOLVO_CLIENT_SECRET" silent
prompt_if_blank VOLVO_API_KEY       "VOLVO_API_KEY (VCC-API-Key)"
prompt_if_blank VOLVO_VIN           "VOLVO_VIN"

if [[ -z "$(getenv_val VOLVO_REFRESH_TOKEN)" ]]; then
  echo
  say "Running Volvo OAuth2 PKCE flow"
  echo
  echo "   The oauth helper listens on :8090 for the redirect callback. If you're"
  echo "   SSH'd into a headless Pi, open a second terminal on your laptop first:"
  echo
  printf "       %sssh -L 8090:127.0.0.1:8090 %s@%s%s\n" \
    "$B" "$(whoami)" "$(hostname -I 2>/dev/null | awk '{print $1}')" "$N"
  echo
  echo "   Then paste the URL the helper prints into your laptop browser."
  echo
  read -r -p "   Press Enter when the tunnel is up (or if you're running locally)… " _
  ./oauth || die "oauth helper failed"
else
  say "VOLVO_REFRESH_TOKEN already set — skipping OAuth"
fi

echo
say "Tibber credentials"
prompt_if_blank TIBBER_EMAIL    "TIBBER_EMAIL"
prompt_if_blank TIBBER_PASSWORD "TIBBER_PASSWORD" silent

if [[ -z "$(getenv_val TIBBER_VEHICLE_ID)" ]]; then
  echo
  read -r -p "   Substring to match in your Tibber vehicle name (e.g. Ragnar, blank for picker): " MATCH
  if [[ -n "$MATCH" ]]; then
    ./tibber-discover --match "$MATCH"
  else
    ./tibber-discover
  fi
else
  say "TIBBER_VEHICLE_ID already set — skipping Tibber discovery"
fi

# --- Poll interval (optional tweak) ---
if [[ -z "$(getenv_val POLL_INTERVAL)" ]]; then
  set_env_key POLL_INTERVAL "3h"
fi

# --- Launch ---
echo
say "Configuration complete."
printf "    install dir : %s\n" "$INSTALL_DIR"
printf "    .env        : %s (0600)\n" "$INSTALL_DIR/.env"
echo

if command -v docker >/dev/null && docker compose version >/dev/null 2>&1; then
  read -r -p "Start the service now with 'docker compose up -d --build'? [y/N] " YN
  if [[ "$YN" == "y" || "$YN" == "Y" ]]; then
    docker compose up -d --build
    echo
    say "Service started."
    echo "    logs:  cd $INSTALL_DIR && docker compose logs -f"
    echo "    state: http://localhost:8080/state"
  else
    say "When you're ready: cd $INSTALL_DIR && docker compose up -d --build"
  fi
else
  warn "docker compose not available — install docker, then:"
  warn "   cd $INSTALL_DIR && docker compose up -d --build"
fi
