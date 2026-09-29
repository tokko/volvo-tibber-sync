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
   mock car (e.g. `Ragnar`), or pick interactively. It then lists the selected
   home's `EV_CHARGER` gizmos and each charger's named cars. Pick the charger
   and preferred car by display name; the selected IDs and all linked car
   name-to-ID pairs are saved privately in `.env`.

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

The monitor exposes `/healthz`, `/state`, and `/logs` on the port set by
`HTTP_ADDR` in `.env` (default `:8080` inside the container, mapped to host
**8082** by `docker-compose.yml`). Comment out the `ports:` block to keep
the service fully private.

```bash
curl -s http://localhost:8082/healthz
curl -s http://localhost:8082/state | jq
curl -s 'http://localhost:8082/logs?limit=50'   # recent JSON log lines
```

`/logs` returns up to the last ~500 emitted log entries (newline-delimited
JSON, same format as stdout). `?limit=N` caps the response to the N most
recent entries. Change the left-hand side of `ports:` in compose if 8082 is
already taken on your host.

---

## Nightly auto-lock (optional)

The monitor can lock the car every evening if it was left unlocked. Set
`LOCK_SCHEDULE=20:00` in `.env` (24-hour `HH:MM`; blank disables it) and the
monitor, at that time each day, reads the door/lock state and — if the car is
**unlocked and all doors are closed** — sends a lock command. If the car is
asleep it retries up to 3 times, ~2 minutes apart.

> ⚠️ **This is an unattended physical actuator — accept the failure modes before
> enabling it.** At the scheduled time the car may lock while someone is loading
> it or with a person/pet **inside**. The only situational awareness it has is
> the door-open guard: if any door is open, it skips locking. If door state
> can't be read at all, it also skips (it never locks blind). It requests only
> the lock permission, **never unlock**. Use `LOCK_SCHEDULE` only if that's
> acceptable for your household.

**Prerequisites** (the feature is inert without both):

1. **Add the Connected Vehicle API** to your app on
   [developer.volvocars.com](https://developer.volvocars.com) — it's a separate
   product from the Energy API this project uses by default. Command scopes may
   require the app to be published, not draft.
2. **Re-authorize with the lock scopes, container stopped.** OAuth scopes are
   fixed at authorization time, so the existing token can't lock the car. Run,
   **in this order** (running `oauth` while the monitor is live races the shared
   `token.json` and can lock you out into an `invalid_grant` loop):
   ```bash
   cd ~/volvo-tibber-sync
   docker compose down
   ./oauth --scopes "openid energy:state:read conve:lock_status conve:lock"
   docker compose up -d
   ```

**Timezone.** `LOCK_SCHEDULE` is interpreted in `LOCK_TZ` (default
`Europe/Stockholm`), DST-aware. An invalid `LOCK_TZ` **disables** the feature
rather than silently firing in UTC — check the startup log line
(`nightly auto-lock enabled … tz=…`) to confirm.

**Test it without waiting for 20:00:**
```bash
docker compose run --rm monitor --lock-now --dry-run   # read state, log intended action, send nothing
docker compose run --rm monitor --lock-now             # actually lock, if unlocked
```

**Known limitation.** If the container is restarted across the scheduled time,
that night is skipped — there's no catch-up. The lock only fires when the
process is running at exactly `HH:MM`.

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
| `TIBBER_CHARGER_NAME` | no | — | Selected charger display name from `tibber-discover` |
| `TIBBER_CHARGER_ID` | no | — | ID of that `EV_CHARGER` in `TIBBER_HOME_ID` |
| `TIBBER_CHARGER_VEHICLES` | no | — | URL-encoded name-to-ID pairs of cars linked to that charger; written by `tibber-discover` |
| `TIBBER_PREFERRED_VEHICLE_NAME` | no | — | Named charger car to select; must match one mapped car exactly |
| `POLL_INTERVAL` | no | `3h` | How often to read charge state |
| `TOKEN_STORE_PATH` | no | `/data/token.json` | Where Volvo token is persisted |
| `TIBBER_TOKEN_STORE_PATH` | no | `/data/tibber-token.json` | Where Tibber JWT is persisted |
| `HTTP_ADDR` | no | `:8080` | HTTP listen address inside the container (leave empty to disable) |
| `LOCK_SCHEDULE` | no | — | Daily `HH:MM` to lock the car if unlocked; blank disables (see [Nightly auto-lock](#nightly-auto-lock-optional)) |
| `LOCK_TZ` | no | `Europe/Stockholm` | Timezone for `LOCK_SCHEDULE`; invalid value disables the feature |
| `TZ` | no | — | Log-timestamp timezone (readability only) |

Tokens are persisted to `./data/` on the host (bind-mounted into the
container at `/data`) so restarts don't require re-authentication. Volvo
refresh tokens are rotated automatically; Tibber JWTs are refreshed before
they expire (~18 h TTL). `./oauth` seeds `./data/token.json` directly so the
monitor never has to fall back to the one-shot `VOLVO_REFRESH_TOKEN` from
`.env`.

If Tibber is not configured (all `TIBBER_*` vars blank) the monitor still runs
and logs Volvo charge state — the Tibber push is simply skipped.

The charger selection uses the configured home's `EV_CHARGER` gizmo and its
`vehicles { id name }` list. Before changing selection, the monitor verifies
that charger and the target car still match their configured name/ID pairs.
It reads both the saved `userSettings.preferredVehicleId` and the current
vehicle's `isPreferred` flag. If either differs, it sends one
`setVehicleChargerSettings` request with the `preferredVehicleId` key, then a
separate subsequent request with the `selectPreferredVehicle` key. It confirms
both fields with a fresh read.
If Tibber has not propagated the selection yet, the monitor waits briefly,
then performs one bounded recovery after reading actual state. A remaining
mismatch is logged for the next poll. `--dry-run` still reads the charger and
reports the intended selection, while suppressing Tibber mutations.

The charger car mapping does not change the battery-sync vehicle. Keep
`TIBBER_VEHICLE_ID` set to Ragnar's mock car so Volvo battery updates continue
to target Ragnar even when the charger selects another car.

---

## Troubleshooting

**OAuth code rejected** — the code shown on the callback page expires in a
few minutes. If the exchange fails, re-run `./oauth` and complete the flow
without delay.

**`VOLVO_REFRESH_TOKEN` invalid / `invalid_grant` in logs** — Volvo rotates
refresh tokens on every use, so the token the monitor last used is persisted
to `./data/token.json` (bind-mounted into the container at `/data`). If that
file is deleted, or if a prior run consumed the env's one-shot bootstrap
token, the next refresh will fail. Re-authenticate:
```bash
cd ~/volvo-tibber-sync
./oauth                    # writes both .env and ./data/token.json
docker compose restart
```

Starting in v0.3, `./oauth` seeds `./data/token.json` directly, so the
monitor never has to fall back to the env bootstrap on a healthy setup. The
monitor logs at `ERROR` level when it sees `invalid_grant` so the condition
is visible in `curl http://<host>:8082/logs`.

**Auto-lock logs `re-auth required` / `not allowed to access` (403)** — the
token isn't authorized for the Connected Vehicle API. Either the app isn't
subscribed to that product, or you haven't re-authorized with the lock scopes.
Follow the [Nightly auto-lock](#nightly-auto-lock-optional) prerequisites. (A
403 that mentions *quota* instead is transient — the daily call budget was hit;
it is logged at `WARN`, not treated as a re-auth condition.)

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

`--once` exits after one poll. `--dry-run` logs the intended Tibber push and
charger selection without sending mutations; it reads Tibber state to report
the current selection. `--lock-now` runs a single lock-check cycle (locking the
car if unlocked) and exits without polling or pushing to Tibber; combine with
`--dry-run` to read state without sending a lock command.

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

---

## License

MIT — see [LICENSE](LICENSE). Not affiliated with Volvo Cars or Tibber; the
Tibber integration uses the undocumented app API and may break when Tibber
updates their app.
