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

You need a free developer account to get API access. This is a one-time setup
that takes about 10 minutes.

#### 1 — Create an account

Go to [developer.volvocars.com](https://developer.volvocars.com) and sign in
with your regular Volvo ID (the same account you use in the Volvo Cars app).
If you don't have a Volvo ID, create one at
[volvocars.com](https://www.volvocars.com).

#### 2 — Create an application

1. On your [account page](https://developer.volvocars.com/account/), scroll to
   **Create new application** and give it a name (e.g. `home-charging-sync`).
2. Click **Create**.

The new application appears in the list. Click it to expand.

#### 3 — Get the VCC-API-Key

In the expanded application, under **VCC API key - Primary**, click the eye
icon to reveal the key. Copy it — that is your `VOLVO_API_KEY`.

#### 4 — Get the OAuth client credentials

Still in the expanded application, click **Application Client Details** to
expand that section, then click **Generate new client secret**.

After confirming, a dialog shows:

- **Client ID** → `VOLVO_CLIENT_ID`
- **Client Secret** → `VOLVO_CLIENT_SECRET` (copy it now — it won't be shown
  again in full)

The redirect URI used by the `oauth` helper
(`https://tokko.github.io/volvo-tibber-sync/callback.html`) does not need to
be registered manually; the Volvo ID server accepts it for your application
automatically.

#### 5 — Find your VIN

The VIN is a 17-character code identifying your specific car. Find it in any
of these places:

- **Volvo Cars app** → select your car → Details
- Dashboard sticker (driver's side, visible through the windshield)
- Inside the driver's door frame (sticker)
- Vehicle registration document

Watch for look-alike characters: `0` (zero) vs `O` (letter O), `1` vs `I`.

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
curl -fsSL .../install.sh | RELEASE_TAG=v0.2.0 bash
```

### What the wizard does

1. **Volvo credentials** — prompts for Client ID, Client Secret, VCC-API-Key,
   and VIN (skipped for any value already in `.env`).

2. **Volvo OAuth** — runs the bundled `oauth` helper. It prints an
   authorization URL; open it in any browser (your laptop is fine). After
   you approve, the browser redirects to a GitHub Pages page that displays
   the authorization code. Copy it and paste it back into the terminal when
   prompted. No local server or SSH tunnel needed.

3. **Tibber credentials** — prompts for email and password.

4. **Tibber vehicle discovery** — runs `tibber-discover` to list homes and
   vehicles on your account. You can supply a substring to auto-select your
   mock car (e.g. `Ragnar`), or pick interactively.

5. **Start** — offers to run `docker compose up -d`.

Re-running the installer is safe: any key already present in `.env` is
preserved and its prompt is skipped.

---

## Managing the service

```bash
cd ~/volvo-tibber-sync

# View live logs
docker compose logs -f

# Restart
docker compose restart

# Stop
docker compose down

# Upgrade to a new release
curl -fsSL https://raw.githubusercontent.com/tokko/volvo-tibber-sync/main/scripts/install.sh | bash
docker compose up -d
```

### Poll interval

The default poll interval is **3 hours** (`POLL_INTERVAL=3h` in `.env`). Edit
`.env` and restart the container to change it. Accepts any Go duration string:
`30m`, `1h30m`, etc.

### HTTP status endpoints

The monitor can optionally expose `/healthz` and `/state` endpoints. They are
**disabled by default**. To enable, set `HTTP_ADDR` in `.env`:

```bash
HTTP_ADDR=:8080
```

Then expose the port in `docker-compose.yml` under `ports` and restart. Once
running:

```bash
curl -s http://localhost:8080/state | jq
curl -s http://localhost:8080/healthz
```

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
| `HTTP_ADDR` | no | *(disabled)* | Set to e.g. `:8080` to enable `/healthz` + `/state` |

Tokens are persisted to the `/data` volume so restarts don't require
re-authentication. Volvo refresh tokens are rotated automatically; Tibber JWTs
are refreshed before they expire (~18 h TTL).

If Tibber is not configured (all `TIBBER_*` vars blank) the monitor still runs
and logs Volvo charge state — the Tibber push is simply skipped.

---

## Troubleshooting

**OAuth code rejected** — the code shown on the callback page expires in a
few minutes. If the exchange fails, re-run `./oauth` and complete the flow
without delay.

**`VOLVO_REFRESH_TOKEN` invalid after a long offline period** — Volvo can
expire refresh tokens if unused. Re-run the OAuth step:
```bash
cd ~/volvo-tibber-sync
./oauth
docker compose restart
```

**`VEHICLE_NOT_FOUND` from Volvo API** — the VIN in `.env` doesn't match
any vehicle on your Volvo account. Double-check `VOLVO_VIN` character by
character against the Volvo Cars app (car → Details). Common mistake: `O`
(letter) vs `0` (zero).

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
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o dist/oauth-linux-arm64 ./cmd/oauth
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o dist/tibber-discover-linux-arm64 ./cmd/tibber-discover
```

### Cut a release

Requires `gh` authenticated as a user with push access and the Go toolchain
on `PATH`:

```bash
./scripts/release.sh v0.2.0
```

This cross-compiles all three arm64 binaries and creates a GitHub release with
the binaries, Dockerfile, docker-compose.yml, and .env.example as assets.
