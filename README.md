# volvo-tibber-sync

Reads the battery charge level from a Volvo PHEV via the official Volvo Energy
API and pushes it to a mock-car entry in the Tibber app — so Tibber's smart
charging schedule sees the real state of charge instead of a static value you
have to update by hand.

Runs as a lightweight Docker container on a Raspberry Pi 5 (or any Linux
arm64 host). A single poll is ~6 MB of RAM and a few milliseconds of CPU; the
rest of the time the process sleeps.

---

## Prerequisites

### Volvo Cars Developer Portal

1. Sign in at [developer.volvocars.com](https://developer.volvocars.com).
2. Create an application and subscribe it to the **Volvo Energy API**.
3. Add `http://localhost:8090/callback` as an allowed redirect URI (needed for
   the one-time OAuth step).
4. Note your **Client ID**, **Client Secret**, and **VCC-API-Key**.
5. Find your car's **VIN** (visible in the Volvo Cars app under vehicle details,
   or on the dashboard).

### Tibber mock car

Tibber does not natively support PHEV Volvo models. The workaround is to add a
**generic mock car** in the Tibber app (Settings → EV charger → Add car →
pick any generic model) and let this service keep its battery level in sync.

You'll need your Tibber **email** and **password** (same credentials as the
Tibber mobile app).

### Raspberry Pi

- Raspberry Pi OS (Bookworm or later) — arm64.
- Docker and Docker Compose installed:
  ```
  curl -fsSL https://get.docker.com | sh
  sudo usermod -aG docker $USER   # log out and back in after this
  ```

---

## Install

Run this one-liner on the Pi. It downloads the latest release, walks you
through credential setup, and optionally starts the service.

```bash
curl -fsSL https://raw.githubusercontent.com/tokko/volvo-tibber-sync/main/scripts/install.sh | bash
```

By default files land in `~/volvo-tibber-sync`. Override with `INSTALL_DIR`:

```bash
curl -fsSL .../install.sh | INSTALL_DIR=/opt/volvo-tibber-sync bash
```

Pin a specific release version:

```bash
curl -fsSL .../install.sh | RELEASE_TAG=v0.1.0 bash
```

### What the wizard does

1. **Volvo credentials** — prompts for Client ID, Client Secret, VCC-API-Key,
   and VIN (skipped for any value already in `.env`).

2. **Volvo OAuth** — runs the bundled `oauth` helper. It starts a local callback
   server on `:8090`, prints an authorization URL, and waits for you to open it
   in a browser.

   > **Headless Pi**: the OAuth callback must reach the Pi's port 8090. Before
   > pressing Enter in the wizard, open a second terminal on your laptop and
   > forward the port:
   > ```
   > ssh -L 8090:127.0.0.1:8090 pi@<pi-ip>
   > ```
   > Then open the URL from the wizard in your laptop's browser. The callback
   > will tunnel through to the Pi automatically.
   >
   > The redirect URI you registered in the developer portal must be
   > `http://localhost:8090/callback`.

3. **Tibber credentials** — prompts for email and password.

4. **Tibber vehicle discovery** — runs `tibber-discover` to list homes and
   vehicles on your account. You can supply a substring to auto-select your
   mock car (e.g. `Ragnar`), or pick interactively.

5. **Start** — offers to run `docker compose up -d --build`.

Re-running the installer is safe: any key already present in `.env` is
preserved and its prompt is skipped.

---

## Managing the service

```bash
cd ~/volvo-tibber-sync

# View live logs
docker compose logs -f

# Check charge state
curl -s http://localhost:8080/state | jq

# Health check
curl -s http://localhost:8080/healthz

# Restart
docker compose restart

# Stop
docker compose down

# Upgrade to a new release
curl -fsSL https://raw.githubusercontent.com/tokko/volvo-tibber-sync/main/scripts/install.sh | bash
docker compose up -d --build
```

### Poll interval

The default poll interval is **3 hours** (`POLL_INTERVAL=3h` in `.env`). Edit
`.env` and restart the container to change it. Accepts any Go duration string:
`30m`, `1h30m`, etc.

---

## Configuration reference

All configuration lives in `.env` (created from `.env.example` by the
installer).

| Variable | Required | Default | Description |
|---|---|---|---|
| `VOLVO_CLIENT_ID` | yes | — | OAuth client ID from developer portal |
| `VOLVO_CLIENT_SECRET` | yes | — | OAuth client secret |
| `VOLVO_API_KEY` | yes | — | VCC-API-Key from developer portal |
| `VOLVO_VIN` | yes | — | Vehicle VIN |
| `VOLVO_REFRESH_TOKEN` | yes | — | Written by `oauth` helper; auto-rotated |
| `TIBBER_EMAIL` | no | — | Tibber account email; leave blank to disable push |
| `TIBBER_PASSWORD` | no | — | Tibber account password |
| `TIBBER_HOME_ID` | no | — | Written by `tibber-discover` |
| `TIBBER_VEHICLE_ID` | no | — | Written by `tibber-discover` |
| `TIBBER_VEHICLE_NAME` | no | — | Display name; used in log output only |
| `POLL_INTERVAL` | no | `3h` | How often to read charge state |
| `TOKEN_STORE_PATH` | no | `/data/token.json` | Where Volvo token is persisted |
| `TIBBER_TOKEN_STORE_PATH` | no | `/data/tibber-token.json` | Where Tibber JWT is persisted |
| `HTTP_ADDR` | no | `:8080` | Address for the `/healthz` + `/state` HTTP server |

Tokens are persisted to the `/data` volume so restarts don't require
re-authentication. Volvo refresh tokens are rotated automatically; Tibber JWTs
are refreshed before they expire (~18 h TTL).

If Tibber is not configured (all `TIBBER_*` vars blank) the monitor still runs
and logs Volvo charge state — the Tibber push is simply skipped.

---

## Troubleshooting

**OAuth callback times out** — the `oauth` helper waits 10 minutes for a
browser redirect. If it times out, check that:
- the redirect URI in your Volvo app is exactly `http://localhost:8090/callback`
- the SSH tunnel is open on the right port
- no firewall blocks 8090 on the Pi

**`VOLVO_REFRESH_TOKEN` invalid after a long offline period** — Volvo can
expire refresh tokens if unused. Re-run the OAuth step:
```bash
cd ~/volvo-tibber-sync
./oauth
docker compose restart
```

**Tibber push fails** — the Tibber app API is undocumented. If it stops
working after a Tibber app update, check the [issues](https://github.com/tokko/volvo-tibber-sync/issues)
page. The monitor will continue logging Volvo state even if the Tibber push
errors.

---

## Development

### Run a single poll without Docker

```bash
go run ./cmd/monitor --once --dry-run
```

`--once` exits after one poll. `--dry-run` logs the intended Tibber push
without sending it.

### Build locally

```bash
# Host binaries (native arch)
go build ./cmd/...

# Cross-compile arm64 (requires Go toolchain, no Docker needed)
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o dist/monitor-linux-arm64 ./cmd/monitor
```

### Cut a release

Requires `gh` authenticated as a user with push access:

```bash
./scripts/release.sh v0.2.0
```

This cross-compiles all three arm64 binaries and creates a GitHub release with
the binaries, Dockerfile, docker-compose.yml, and .env.example as assets.
