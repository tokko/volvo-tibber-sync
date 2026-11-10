// Command monitor polls the Volvo Energy API on an interval, logs charge state
// as JSON to stdout, and optionally mirrors the battery percentage into Tibber
// via the undocumented app API (so Tibber smart charging sees the real SoC).
//
// Volvo access token refresh is automatic; rotations persist to
// TOKEN_STORE_PATH so restarts don't force re-OAuth. Tibber session JWTs are
// cached the same way.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	_ "time/tzdata" // embed zoneinfo so LoadLocation works in distroless (no OS tzdata)

	"github.com/tokko/volvo-tibber-sync/internal/config"
	"github.com/tokko/volvo-tibber-sync/internal/deco"
	"github.com/tokko/volvo-tibber-sync/internal/tibber"
	"github.com/tokko/volvo-tibber-sync/internal/volvo"
)

var (
	flagOnce    = flag.Bool("once", false, "run one poll cycle and exit (useful for testing or cron)")
	flagDryRun  = flag.Bool("dry-run", false, "fetch state and log intended actions; do not mutate Tibber or send a lock command")
	flagLockNow = flag.Bool("lock-now", false, "run one lock-check cycle (lock the car if unlocked) and exit; does not poll or push to Tibber")
)

// logRing is a global capped ring buffer of emitted log lines, so the /logs
// HTTP endpoint can return recent entries without shelling into Docker.
var logRing = newLogRingBuffer(500)

func main() {
	flag.Parse()

	w := io.MultiWriter(os.Stdout, logRing)
	logger := slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	_ = config.LoadDotEnv(".env")

	if err := run(); err != nil {
		slog.Error("monitor exited with error", "err", err)
		os.Exit(1)
	}
}

func run() error {
	clientID, err := config.Require("VOLVO_CLIENT_ID")
	if err != nil {
		return err
	}
	clientSecret, err := config.Require("VOLVO_CLIENT_SECRET")
	if err != nil {
		return err
	}
	apiKey, err := config.Require("VOLVO_API_KEY")
	if err != nil {
		return err
	}
	vin, err := config.Require("VOLVO_VIN")
	if err != nil {
		return err
	}
	refreshToken, err := config.Require("VOLVO_REFRESH_TOKEN")
	if err != nil {
		return err
	}

	pollInterval, err := time.ParseDuration(config.Optional("POLL_INTERVAL", "3h"))
	if err != nil {
		return fmt.Errorf("invalid POLL_INTERVAL: %w", err)
	}
	tokenStore := config.Optional("TOKEN_STORE_PATH", "/data/token.json")
	tibberStore := config.Optional("TIBBER_TOKEN_STORE_PATH", "/data/tibber-token.json")
	httpAddr := config.Optional("HTTP_ADDR", "")

	// Use stored token if present (survives restarts and keeps any rotated refresh
	// token), otherwise bootstrap from env.
	initial := volvo.Token{RefreshToken: refreshToken}
	if stored, ok := loadStoredToken(tokenStore); ok {
		initial = stored
		slog.Info("loaded token from store", "path", tokenStore, "expires_at", initial.ExpiresAt)
	} else {
		// Volvo rotates refresh tokens on use — if this env token has already
		// been consumed by a prior run, the first refresh will fail. The oauth
		// helper should seed token.json so we never take this path on a
		// healthy setup.
		slog.Warn("no stored token — bootstrapping from env VOLVO_REFRESH_TOKEN (one-shot; re-run ./oauth if first refresh fails with invalid_grant)", "path", tokenStore)
	}

	latest := &latestState{}

	ts := volvo.NewTokenSource(clientID, clientSecret, initial, func(t volvo.Token) {
		slog.Info("volvo token refreshed", "expires_at", t.ExpiresAt)
		if err := saveStoredToken(tokenStore, t); err != nil {
			slog.Error("could not persist refreshed volvo token — next restart will fall back to env token and may fail with invalid_grant", "path", tokenStore, "err", err)
		}
	})
	client := volvo.NewClient(ts, apiKey, vin)

	tbrClient, tbrConf := buildTibberClient(tibberStore)
	presence := buildPhonePresenceConfig(tbrClient, tbrConf)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if httpAddr != "" {
		go serveHTTP(ctx, httpAddr, latest)
	}

	slog.Info("starting monitor",
		"vin", vin,
		"poll_interval", pollInterval.String(),
		"http_addr", httpAddr,
		"token_store", tokenStore,
		"tibber_enabled", tbrClient != nil,
		"once", *flagOnce,
		"lock_now", *flagLockNow,
		"dry_run", *flagDryRun,
		"phone_presence_enabled", presence != nil,
	)

	pollOnce := func() {
		slog.Info("poll starting", "vin", vin)
		pCtx, pCancel := context.WithTimeout(ctx, 2*time.Minute)
		defer pCancel()
		state := client.FetchChargeState(pCtx)
		latest.Lock()
		latest.state = &state
		latest.Unlock()
		logState(state)
		if tbrClient == nil {
			return
		}
		if *flagDryRun {
			logDryRunPush(tbrConf, state)
		} else {
			pushToTibber(pCtx, tbrClient, tbrConf, state)
		}
		if tbrConf.chargerID != "" && *flagOnce {
			reconcileTibberChargerCar(pCtx, tbrClient, tbrConf, tbrConf.preferredVehicleName, *flagDryRun)
		}
	}

	if *flagLockNow {
		lockCheckOnce(ctx, client, *flagDryRun)
		slog.Info("--lock-now: exiting after single lock check")
		return nil
	}

	pollOnce()
	if *flagOnce {
		slog.Info("--once: exiting after single poll")
		return nil
	}
	if presence != nil {
		go runPhonePresenceSchedule(ctx, presence, *flagDryRun)
	}

	startLockScheduler(ctx, client)

	t := time.NewTicker(pollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			slog.Info("shutting down")
			return nil
		case <-t.C:
			pollOnce()
		}
	}
}

func logDryRunPush(cfg tibberConfig, s volvo.ChargeState) {
	if s.BatteryChargeLevelPct == nil {
		slog.Info("tibber push (dry-run skipped — no battery level this poll)",
			"vehicle", cfg.vehicleName, "vehicle_id", cfg.vehicleID)
		return
	}
	pct := int(*s.BatteryChargeLevelPct + 0.5)
	slog.Info("tibber push (dry-run — NOT sent)",
		"vehicle", cfg.vehicleName, "vehicle_id", cfg.vehicleID,
		"home_id", cfg.homeID, "battery_pct", pct,
		"mutation", "setVehicleSettings", "key", "offline.vehicle.batteryLevel",
	)
}

func logState(s volvo.ChargeState) {
	attrs := []any{
		"vin", s.VIN,
		"observed_at", s.ObservedAt.Format(time.RFC3339),
	}
	if s.BatteryChargeLevelPct != nil {
		attrs = append(attrs, "battery_pct", *s.BatteryChargeLevelPct)
	}
	if s.ElectricRangeKm != nil {
		attrs = append(attrs, "range_km", *s.ElectricRangeKm)
	}
	if s.EstimatedChargingTimeMin != nil {
		attrs = append(attrs, "est_charging_min", *s.EstimatedChargingTimeMin)
	}
	if s.ChargingSystemStatus != "" {
		attrs = append(attrs, "charging_system", s.ChargingSystemStatus)
	}
	if s.ChargingConnectionStatus != "" {
		attrs = append(attrs, "charging_connection", s.ChargingConnectionStatus)
	}
	if len(s.Errors) > 0 {
		attrs = append(attrs, "errors", s.Errors)
		if anyInvalidGrant(s.Errors) {
			attrs = append(attrs, "action_required", "run ./oauth on the host and restart the container (refresh token was rejected by Volvo)")
			slog.Error("volvo refresh token rejected (invalid_grant) — re-auth required", attrs...)
			return
		}
		slog.Warn("charge state (partial)", attrs...)
		return
	}
	slog.Info("charge state", attrs...)
}

func anyInvalidGrant(errs []string) bool {
	for _, e := range errs {
		if strings.Contains(e, "invalid_grant") {
			return true
		}
	}
	return false
}

// anyForbiddenScope reports whether an error is Volvo's "not allowed to access
// the API" 403 — a scope/subscription problem that needs re-auth. It must NOT
// match the *other* 403 Volvo returns ("out of call volume quota"), which is
// transient and must not trigger a re-auth prompt.
func anyForbiddenScope(errs []string) bool {
	for _, e := range errs {
		if strings.Contains(e, "403") && strings.Contains(strings.ToLower(e), "not allowed to access") {
			return true
		}
	}
	return false
}

// startLockScheduler launches the nightly auto-lock goroutine if LOCK_SCHEDULE
// is set. Timezone is resolved explicitly and fail-closed: an invalid LOCK_TZ
// disables the feature rather than silently firing in UTC.
func startLockScheduler(ctx context.Context, client *volvo.Client) {
	schedule := config.Optional("LOCK_SCHEDULE", "")
	if schedule == "" {
		slog.Info("nightly auto-lock disabled (set LOCK_SCHEDULE=20:00 to enable)")
		return
	}
	at, err := time.Parse("15:04", schedule)
	if err != nil {
		slog.Error("invalid LOCK_SCHEDULE (want HH:MM) — nightly auto-lock disabled", "value", schedule, "err", err)
		return
	}
	tzName := config.Optional("LOCK_TZ", "Europe/Stockholm")
	loc, err := time.LoadLocation(tzName)
	if err != nil {
		slog.Error("invalid LOCK_TZ — nightly auto-lock disabled (refusing to fall back to UTC)", "value", tzName, "err", err)
		return
	}
	slog.Info("nightly auto-lock enabled", "at", schedule, "tz", tzName, "scopes", volvo.LockScopes)
	go runLockScheduler(ctx, client, at.Hour(), at.Minute(), loc)
}

// nextOccurrence returns the next wall-clock HH:MM in loc strictly after now.
// Computing in loc each time (never now.Add(24h)) keeps it DST-correct.
func nextOccurrence(now time.Time, hour, min int, loc *time.Location) time.Time {
	n := now.In(loc)
	next := time.Date(n.Year(), n.Month(), n.Day(), hour, min, 0, 0, loc)
	if !next.After(n) {
		next = next.AddDate(0, 0, 1)
	}
	return next
}

func runLockScheduler(ctx context.Context, client *volvo.Client, hour, min int, loc *time.Location) {
	for {
		next := nextOccurrence(time.Now(), hour, min, loc)
		slog.Info("next lock check scheduled", "at", next.Format(time.RFC3339))
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			lockCheckOnce(ctx, client, *flagDryRun)
		}
	}
}

// lockCheckOnce runs one lock-check cycle: read lock/door state and, if the car
// is unlocked and safe to lock, send the lock command. It retries only the
// asleep/timeout case (a garaged car is often asleep at the scheduled hour);
// every other outcome returns immediately. Log-and-continue throughout.
func lockCheckOnce(ctx context.Context, client *volvo.Client, dryRun bool) {
	const maxAttempts = 3
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if lockCheckAttempt(ctx, client, dryRun, attempt) {
			return
		}
		if attempt < maxAttempts {
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Minute):
			}
		}
	}
	slog.Error("could not lock car — gave up after retries (car may be unreachable)", "attempts", maxAttempts)
}

// lockCheckAttempt runs a single attempt. It returns true when the cycle is
// done (success, or a terminal condition that must not be retried) and false
// only for the asleep/timeout case that warrants a retry.
func lockCheckAttempt(ctx context.Context, client *volvo.Client, dryRun bool, attempt int) bool {
	aCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	st := client.FetchLockStatus(aCtx)
	if len(st.Errors) > 0 {
		logLockError("read lock status failed", st.Errors, attempt)
		return true // never retry HTTP / scope / quota failures
	}
	if !st.DoorsKnown {
		slog.Warn("no door data — cannot verify it is safe to lock; skipping", lockAttrs(st, attempt)...)
		return true
	}
	if st.AnyDoorOpen {
		slog.Info("car in use (a door is open); skipping lock", lockAttrs(st, attempt)...)
		return true
	}
	switch st.CentralLock {
	case "LOCKED":
		slog.Info("car already locked; nothing to do", lockAttrs(st, attempt)...)
		return true
	case "UNLOCKED":
		// proceed to lock
	default:
		slog.Warn("unknown lock state — check scopes/model; not locking", lockAttrs(st, attempt)...)
		return true
	}

	if dryRun {
		slog.Info("car is unlocked — would send lock command (dry-run, NOT sent)", lockAttrs(st, attempt)...)
		return true
	}

	res := client.Lock(aCtx)
	if len(res.Errors) > 0 {
		logLockError("lock command failed", res.Errors, attempt)
		return true // HTTP failure — don't retry
	}
	switch strings.ToUpper(res.InvokeStatus) {
	case "COMPLETED":
		slog.Info("car locked", "attempt", attempt, "invoke_status", res.InvokeStatus, "message", res.Message)
		return true
	case "RUNNING", "DELIVERED", "WAITING":
		slog.Info("lock command accepted; completing asynchronously", "attempt", attempt, "invoke_status", res.InvokeStatus, "message", res.Message)
		return true
	default:
		// TIMEOUT / VEHICLE_IN_SLEEP / etc. — the car is likely asleep; retry.
		slog.Warn("lock not confirmed; will retry if attempts remain", "attempt", attempt, "invoke_status", res.InvokeStatus, "message", res.Message)
		return false
	}
}

func lockAttrs(st volvo.LockStatus, attempt int) []any {
	attrs := []any{
		"attempt", attempt,
		"central_lock", st.CentralLock,
		"any_door_open", st.AnyDoorOpen,
	}
	if len(st.OpenParts) > 0 {
		attrs = append(attrs, "open_parts", st.OpenParts)
	}
	if !st.ObservedAt.IsZero() {
		attrs = append(attrs, "observed_at", st.ObservedAt.Format(time.RFC3339))
	}
	return attrs
}

func logLockError(msg string, errs []string, attempt int) {
	attrs := []any{"attempt", attempt, "errors", errs}
	switch {
	case anyForbiddenScope(errs):
		attrs = append(attrs, "action_required", "re-run ./oauth with lock scopes (openid energy:state:read conve:lock_status conve:lock) and restart — the token is not authorized for the Connected Vehicle API")
		slog.Error(msg+" — re-auth required", attrs...)
	case anyInvalidGrant(errs):
		attrs = append(attrs, "action_required", "run ./oauth on the host and restart the container (refresh token was rejected by Volvo)")
		slog.Error(msg+" — re-auth required", attrs...)
	default:
		// Includes quota-403 and transient network/5xx — do not prompt re-auth.
		slog.Warn(msg, attrs...)
	}
}

func loadStoredToken(path string) (volvo.Token, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return volvo.Token{}, false
	}
	var t volvo.Token
	if err := json.Unmarshal(b, &t); err != nil {
		return volvo.Token{}, false
	}
	if t.RefreshToken == "" {
		return volvo.Token{}, false
	}
	return t, true
}

func saveStoredToken(path string, t volvo.Token) error {
	b, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	// Best-effort mkdir of parent.
	if dir := dirOf(path); dir != "" {
		_ = os.MkdirAll(dir, 0o700)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func dirOf(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' || path[i] == '\\' {
			return path[:i]
		}
	}
	return ""
}

type latestState struct {
	sync.RWMutex
	state *volvo.ChargeState
}

type tibberConfig struct {
	homeID               string
	vehicleID            string
	vehicleName          string
	chargerName          string
	chargerID            string
	preferredVehicleName string
	chargerVehicles      map[string]string
}

type phonePresenceConfig struct {
	client       *deco.Client
	mac          string
	tibberClient *tibber.Client
	tibber       tibberConfig
	presentName  string
	absentName   string
	statePath    string
}

type phonePresenceState struct {
	PendingDeparture bool `json:"pending_departure"`
}

func buildPhonePresenceConfig(tbrClient *tibber.Client, tbrConf tibberConfig) *phonePresenceConfig {
	enabledSetting := strings.ToLower(strings.TrimSpace(config.Optional("PHONE_PRESENCE_ENABLED", "false")))
	if enabledSetting == "false" || enabledSetting == "" {
		slog.Info("phone presence disabled; set PHONE_PRESENCE_ENABLED=true to enable")
		return nil
	}
	if enabledSetting != "true" {
		slog.Warn("phone presence disabled; PHONE_PRESENCE_ENABLED must be true or false")
		return nil
	}
	if tbrClient == nil {
		slog.Warn("phone presence disabled; Tibber is not configured")
		return nil
	}
	presentName := config.Optional("PHONE_PRESENCE_CAR_PRESENT", "pappas bil")
	absentName := config.Optional("PHONE_PRESENCE_CAR_ABSENT", "Ragnar")
	presentID, presentOK := tbrConf.chargerVehicles[presentName]
	absentID, absentOK := tbrConf.chargerVehicles[absentName]
	if tbrConf.chargerID == "" || tbrConf.chargerName == "" || !presentOK || !absentOK || presentName == absentName || presentID == absentID {
		slog.Warn("phone presence disabled; Tibber charger and two distinct mapped car names are required")
		return nil
	}
	mac, err := net.ParseMAC(config.Optional("DECO_ENROLLED_PHONE_MAC", ""))
	if err != nil || !validPhoneMAC(mac) {
		slog.Warn("phone presence disabled; DECO_ENROLLED_PHONE_MAC must be an enrolled unicast MAC address")
		return nil
	}
	decoClient, err := deco.NewClient(
		config.Optional("DECO_ROUTER_URL", "https://192.168.68.1"),
		config.Optional("DECO_OWNER_PASSWORD", ""),
		config.Optional("DECO_TLS_PUBLIC_KEY_SHA256", ""),
	)
	if err != nil {
		slog.Warn("phone presence disabled; invalid Deco configuration", "err", err)
		return nil
	}
	return &phonePresenceConfig{
		client: decoClient, mac: strings.ToLower(mac.String()),
		tibberClient: tbrClient, tibber: tbrConf,
		presentName: presentName, absentName: absentName,
		statePath: config.Optional("PHONE_PRESENCE_STATE_PATH", "/data/phone-presence.json"),
	}
}

func runPhonePresenceSchedule(ctx context.Context, cfg *phonePresenceConfig, dryRun bool) {
	loc, err := time.LoadLocation("Europe/Stockholm")
	if err != nil {
		slog.Error("phone presence schedule disabled; Europe/Stockholm timezone unavailable", "err", err)
		return
	}
	state, err := loadPhonePresenceState(cfg.statePath)
	if err != nil {
		slog.Warn("phone presence state could not be read; retaining pending departure until a confirmed absence", "path", cfg.statePath, "err", err)
		state.PendingDeparture = true
	}
	for {
		next := nextPhonePresenceCheck(time.Now(), loc, state.PendingDeparture)
		delay := time.Until(next)
		if delay > 0 {
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
		cycleCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
		clients, pollErr := cfg.client.ConnectedClients(cycleCtx)
		cancel()
		observedAt := time.Now().In(loc)
		if pollErr != nil {
			nextCheck := nextPhonePresenceCheck(time.Now(), loc, state.PendingDeparture)
			slog.Warn("phone presence observation unknown", "observed_at", observedAt.Format(time.RFC3339), "next_check", nextCheck.In(loc).Format(time.RFC3339), "err", pollErr)
			continue
		}
		present, matches := false, 0
		for _, client := range clients {
			if client.MAC == cfg.mac {
				matches++
				present = client.Online
			}
		}
		if matches > 1 {
			nextCheck := nextPhonePresenceCheck(time.Now(), loc, state.PendingDeparture)
			slog.Warn("phone presence observation unknown; enrolled MAC matched multiple router records", "observed_at", observedAt.Format(time.RFC3339), "next_check", nextCheck.In(loc).Format(time.RFC3339))
			continue
		}
		daytime := phonePresenceDaytime(next.In(loc))
		if !daytime && !state.PendingDeparture {
			continue
		}
		target := cfg.absentName
		stateName := "absent"
		if present {
			target = cfg.presentName
			stateName = "present"
			if !state.PendingDeparture {
				state.PendingDeparture = true
				if err := savePhonePresenceState(cfg.statePath, state); err != nil {
					slog.Error("phone presence selection skipped; pending departure could not be persisted", "path", cfg.statePath, "err", err)
					continue
				}
			}
		}
		cycleCtx, cancel = context.WithTimeout(ctx, 90*time.Second)
		selection := reconcileTibberChargerCar(cycleCtx, cfg.tibberClient, cfg.tibber, target, dryRun)
		cancel()
		if state.PendingDeparture && !present {
			state.PendingDeparture = false
			if err := savePhonePresenceState(cfg.statePath, state); err != nil {
				slog.Error("phone presence pending departure could not be persisted as cleared; polling stops until restart", "path", cfg.statePath, "err", err)
			}
		}
		nextCheck := nextPhonePresenceCheck(time.Now(), loc, state.PendingDeparture)
		slog.Info("phone presence reconciliation", "observed_at", observedAt.Format(time.RFC3339), "state", stateName, "vehicle", target, "selection", selection, "next_check", nextCheck.In(loc).Format(time.RFC3339), "dry_run", dryRun)
	}
}

func validPhoneMAC(mac net.HardwareAddr) bool {
	if len(mac) != 6 || mac[0]&1 != 0 {
		return false
	}
	for _, octet := range mac {
		if octet != 0 {
			return true
		}
	}
	return false
}

func nextPhonePresenceCheck(now time.Time, loc *time.Location, pending bool) time.Time {
	candidate := now.Truncate(time.Minute)
	if candidate.Before(now) {
		candidate = candidate.Add(time.Minute)
	}
	for i := 0; i < 48*60; i++ {
		local := candidate.In(loc)
		minute := local.Hour()*60 + local.Minute()
		if local.Minute()%15 == 0 && (pending || (minute >= 8*60 && minute <= 17*60)) {
			return candidate
		}
		candidate = candidate.Add(time.Minute)
	}
	return candidate
}

func phonePresenceDaytime(t time.Time) bool {
	minute := t.Hour()*60 + t.Minute()
	return minute >= 8*60 && minute <= 17*60
}

func loadPhonePresenceState(path string) (phonePresenceState, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return phonePresenceState{}, nil
		}
		return phonePresenceState{}, err
	}
	var raw struct {
		PendingDeparture *bool `json:"pending_departure"`
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&raw); err != nil {
		return phonePresenceState{}, err
	}
	if raw.PendingDeparture == nil {
		return phonePresenceState{}, errors.New("pending_departure is missing")
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		if err == nil {
			return phonePresenceState{}, errors.New("phone presence state has trailing data")
		}
		return phonePresenceState{}, err
	}
	return phonePresenceState{PendingDeparture: *raw.PendingDeparture}, nil
}

func savePhonePresenceState(path string, state phonePresenceState) error {
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// buildTibberClient returns a configured client or (nil, _) if Tibber is not
// configured. Any partial-but-invalid configuration is logged and treated as
// disabled so the core Volvo flow isn't held hostage.
func buildTibberClient(tokenStorePath string) (*tibber.Client, tibberConfig) {
	email := config.Optional("TIBBER_EMAIL", "")
	password := config.Optional("TIBBER_PASSWORD", "")
	homeID := config.Optional("TIBBER_HOME_ID", "")
	vehicleID := config.Optional("TIBBER_VEHICLE_ID", "")
	vehicleName := config.Optional("TIBBER_VEHICLE_NAME", "")
	chargerName := config.Optional("TIBBER_CHARGER_NAME", "")
	chargerID := config.Optional("TIBBER_CHARGER_ID", "")
	preferredVehicleName := config.Optional("TIBBER_PREFERRED_VEHICLE_NAME", "")
	chargerVehicles := map[string]string{}
	vehicleMapValue := config.Optional("TIBBER_CHARGER_VEHICLES", "")
	if vehicleMapValue != "" {
		values, err := url.ParseQuery(vehicleMapValue)
		if err != nil {
			slog.Warn("tibber charger vehicle map is malformed; selection is disabled")
			chargerVehicles = nil
		} else {
			for name, ids := range values {
				if len(ids) != 1 {
					slog.Warn("tibber charger vehicle map has a duplicate name; selection is disabled", "name", name)
					chargerVehicles = nil
					break
				}
				chargerVehicles[name] = ids[0]
			}
		}
		seenIDs := make(map[string]bool, len(chargerVehicles))
		for name, id := range chargerVehicles {
			if strings.TrimSpace(name) == "" || strings.TrimSpace(id) == "" || seenIDs[id] {
				slog.Warn("tibber charger vehicle map has a missing or duplicate name/ID; selection is disabled")
				chargerVehicles = nil
				break
			}
			seenIDs[id] = true
		}
	}

	if email == "" && password == "" && homeID == "" && vehicleID == "" {
		return nil, tibberConfig{}
	}
	if email == "" || password == "" || homeID == "" || vehicleID == "" {
		slog.Warn("tibber partially configured; skipping push — need TIBBER_EMAIL, TIBBER_PASSWORD, TIBBER_HOME_ID, TIBBER_VEHICLE_ID")
		return nil, tibberConfig{}
	}
	if chargerName != "" || chargerID != "" || preferredVehicleName != "" || len(chargerVehicles) > 0 {
		if chargerName == "" || chargerID == "" || preferredVehicleName == "" || len(chargerVehicles) == 0 {
			slog.Warn("tibber charger selection is partially configured; selection is disabled")
			chargerName, chargerID, preferredVehicleName, chargerVehicles = "", "", "", nil
		} else if _, ok := chargerVehicles[preferredVehicleName]; !ok {
			slog.Warn("tibber preferred car has no configured name/ID mapping; selection is disabled", "name", preferredVehicleName)
			chargerName, chargerID, preferredVehicleName, chargerVehicles = "", "", "", nil
		}
	}
	return buildTibberClientWithConfig(tokenStorePath, email, password, homeID, vehicleID, vehicleName, chargerName, chargerID, preferredVehicleName, chargerVehicles)
}

func buildTibberClientWithConfig(tokenStorePath, email, password, homeID, vehicleID, vehicleName, chargerName, chargerID, preferredVehicleName string, chargerVehicles map[string]string) (*tibber.Client, tibberConfig) {

	sess := tibber.NewSession(email, password)
	if stored, ok := loadTibberToken(tokenStorePath); ok {
		sess.Seed(stored.Token, stored.ExpiresAt)
		slog.Info("loaded tibber token from store", "path", tokenStorePath, "expires_at", stored.ExpiresAt)
	}
	sess.SetOnRefresh(func(token string, expiresAt time.Time) {
		slog.Info("tibber token refreshed", "expires_at", expiresAt)
		if err := saveTibberToken(tokenStorePath, tibberToken{Token: token, ExpiresAt: expiresAt}); err != nil {
			slog.Warn("could not persist tibber token", "path", tokenStorePath, "err", err)
		}
	})

	return tibber.NewClient(sess), tibberConfig{
		homeID:               homeID,
		vehicleID:            vehicleID,
		vehicleName:          vehicleName,
		chargerName:          chargerName,
		chargerID:            chargerID,
		preferredVehicleName: preferredVehicleName,
		chargerVehicles:      chargerVehicles,
	}
}

func reconcileTibberChargerCar(ctx context.Context, c *tibber.Client, cfg tibberConfig, targetName string, dryRun bool) string {
	targetID, ok := cfg.chargerVehicles[targetName]
	if !ok || targetName == "" {
		slog.Error("tibber charger selection skipped; target has no configured name/ID mapping", "vehicle", targetName)
		return "incomplete"
	}

	const maxAttempts = 2
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		charger, err := readConfiguredCharger(ctx, c, cfg)
		if err != nil {
			slog.Error("tibber charger selection read failed", "err", err, "attempt", attempt)
			return "incomplete"
		}
		target, err := validateTargetVehicle(charger, targetName, targetID)
		if err != nil {
			slog.Error("tibber charger selection skipped", "err", err)
			return "incomplete"
		}
		aligned, err := chargerSelectionAligned(charger, target.ID)
		if err != nil {
			slog.Error("tibber charger selection skipped", "err", err)
			return "incomplete"
		}
		if aligned {
			slog.Info("tibber charger car already selected", "charger", cfg.chargerName, "vehicle", targetName, "vehicle_id", targetID)
			return "already-correct"
		}
		if dryRun {
			slog.Info("tibber charger selection (dry-run — NOT sent)", "charger", cfg.chargerName, "charger_id", cfg.chargerID, "vehicle", targetName, "vehicle_id", targetID, "saved_vehicle_id", charger.PreferredVehicleID)
			return "dry-run"
		}

		if charger.PreferredVehicleID != target.ID {
			if err := c.SetPreferredVehicleID(ctx, cfg.homeID, cfg.chargerID, target.ID); err != nil {
				slog.Warn("tibber saved charger preference request failed; reading actual state before recovery", "err", err, "attempt", attempt)
			}
			charger, err = readConfiguredCharger(ctx, c, cfg)
			if err != nil {
				slog.Error("tibber charger selection readback failed", "err", err, "attempt", attempt)
				return "incomplete"
			}
			if _, err := validateTargetVehicle(charger, targetName, targetID); err != nil {
				slog.Error("tibber charger selection stopped after saved preference request", "err", err)
				return "incomplete"
			}
			if charger.PreferredVehicleID != target.ID {
				slog.Warn("tibber saved charger preference did not take effect", "wanted_vehicle_id", target.ID, "saved_vehicle_id", charger.PreferredVehicleID, "attempt", attempt)
				continue
			}
		}

		aligned, err = chargerSelectionAligned(charger, target.ID)
		if err != nil {
			slog.Error("tibber charger selection skipped", "err", err)
			return "incomplete"
		}
		if !aligned {
			if err := c.SelectPreferredVehicle(ctx, cfg.homeID, cfg.chargerID, target.ID); err != nil {
				slog.Warn("tibber current charger selection request failed; reading actual state before recovery", "err", err, "attempt", attempt)
			}
		}

		for read := 0; read < 7; read++ {
			if read > 0 {
				select {
				case <-ctx.Done():
					slog.Warn("tibber charger selection readback timed out", "vehicle", targetName, "attempt", attempt)
					return "incomplete"
				case <-time.After(500 * time.Millisecond):
				}
			}
			charger, err = readConfiguredCharger(ctx, c, cfg)
			if err != nil {
				slog.Error("tibber charger selection readback failed", "err", err, "attempt", attempt)
				return "incomplete"
			}
			if _, err := validateTargetVehicle(charger, targetName, targetID); err != nil {
				slog.Error("tibber charger selection stopped during readback", "err", err)
				return "incomplete"
			}
			aligned, err = chargerSelectionAligned(charger, target.ID)
			if err != nil {
				slog.Error("tibber charger selection skipped", "err", err)
				return "incomplete"
			}
			if aligned {
				slog.Info("tibber charger car selection confirmed", "charger", cfg.chargerName, "vehicle", targetName, "vehicle_id", targetID)
				return "changed"
			}
		}
		slog.Warn("tibber charger selection remains mismatched; will reconcile on the next observation", "charger", cfg.chargerName, "vehicle", targetName, "vehicle_id", targetID, "attempt", attempt)
	}
	return "incomplete"
}

func readConfiguredCharger(ctx context.Context, c *tibber.Client, cfg tibberConfig) (tibber.Charger, error) {
	gizmos, err := c.ListHomeGizmos(ctx, cfg.homeID)
	if err != nil {
		return tibber.Charger{}, fmt.Errorf("list configured home gizmos: %w", err)
	}
	chargerMatches := 0
	for _, g := range gizmos {
		if g.Type == "EV_CHARGER" && (g.ID == cfg.chargerID || g.Name == cfg.chargerName) {
			chargerMatches++
			if g.ID != cfg.chargerID || g.Name != cfg.chargerName {
				return tibber.Charger{}, fmt.Errorf("configured charger name and ID do not match one EV_CHARGER in home %q", cfg.homeID)
			}
		}
	}
	if chargerMatches != 1 {
		return tibber.Charger{}, fmt.Errorf("configured charger %q (%s) matched %d EV_CHARGER gizmos in home %q", cfg.chargerName, cfg.chargerID, chargerMatches, cfg.homeID)
	}
	charger, err := c.GetCharger(ctx, cfg.homeID, cfg.chargerID)
	if err != nil {
		return tibber.Charger{}, fmt.Errorf("read configured charger: %w", err)
	}
	if charger.ID != cfg.chargerID || charger.Name != cfg.chargerName || charger.Type != "EV_CHARGER" {
		return tibber.Charger{}, fmt.Errorf("charger response does not match configured EV_CHARGER %q (%s)", cfg.chargerName, cfg.chargerID)
	}
	if err := validateConfiguredChargerVehicles(charger, cfg.chargerVehicles); err != nil {
		return tibber.Charger{}, err
	}
	return charger, nil
}

func validateTargetVehicle(charger tibber.Charger, name, id string) (tibber.ChargerVehicle, error) {
	nameMatches, idMatches := 0, 0
	var target tibber.ChargerVehicle
	for _, vehicle := range charger.Vehicles {
		if vehicle.Name == name {
			nameMatches++
			target = vehicle
		}
		if vehicle.ID == id {
			idMatches++
		}
	}
	if nameMatches != 1 || idMatches != 1 || target.ID != id {
		return tibber.ChargerVehicle{}, fmt.Errorf("configured car name and ID do not uniquely match a vehicle on charger %q", charger.Name)
	}
	return target, nil
}

func validateConfiguredChargerVehicles(charger tibber.Charger, configured map[string]string) error {
	if len(configured) == 0 {
		return fmt.Errorf("no configured charger car mappings")
	}
	seenIDs := make(map[string]bool, len(configured))
	for name, id := range configured {
		if strings.TrimSpace(name) == "" || strings.TrimSpace(id) == "" || seenIDs[id] {
			return fmt.Errorf("configured charger car mappings contain a missing or duplicate name/ID")
		}
		seenIDs[id] = true
		if _, err := validateTargetVehicle(charger, name, id); err != nil {
			return err
		}
	}
	return nil
}

func chargerSelectionAligned(charger tibber.Charger, targetID string) (bool, error) {
	preferred := 0
	targetPreferred := false
	for _, vehicle := range charger.Vehicles {
		if vehicle.ID == "" || vehicle.Name == "" || vehicle.IsPreferred == nil {
			return false, fmt.Errorf("charger returned a vehicle with missing ID, name, or preference status")
		}
		if *vehicle.IsPreferred {
			preferred++
			targetPreferred = vehicle.ID == targetID
		}
	}
	if preferred > 1 {
		return false, fmt.Errorf("charger returned multiple currently preferred vehicles")
	}
	return charger.PreferredVehicleID == targetID && targetPreferred, nil
}

func pushToTibber(ctx context.Context, c *tibber.Client, cfg tibberConfig, s volvo.ChargeState) {
	if s.BatteryChargeLevelPct == nil {
		slog.Warn("no battery level from volvo this poll; skipping tibber push")
		return
	}
	pct := int(*s.BatteryChargeLevelPct + 0.5)
	if err := c.SetBatteryLevel(ctx, cfg.homeID, cfg.vehicleID, pct); err != nil {
		slog.Error("tibber push failed", "err", err, "vehicle", cfg.vehicleName, "battery_pct", pct)
		return
	}
	slog.Info("tibber push ok", "vehicle", cfg.vehicleName, "vehicle_id", cfg.vehicleID, "battery_pct", pct)
}

type tibberToken struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

func loadTibberToken(path string) (tibberToken, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return tibberToken{}, false
	}
	var t tibberToken
	if err := json.Unmarshal(b, &t); err != nil {
		return tibberToken{}, false
	}
	if t.Token == "" || time.Now().After(t.ExpiresAt) {
		return tibberToken{}, false
	}
	return t, true
}

func saveTibberToken(path string, t tibberToken) error {
	b, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	if dir := dirOf(path); dir != "" {
		_ = os.MkdirAll(dir, 0o700)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func serveHTTP(ctx context.Context, addr string, latest *latestState) {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/state", func(w http.ResponseWriter, _ *http.Request) {
		latest.RLock()
		s := latest.state
		latest.RUnlock()
		w.Header().Set("Content-Type", "application/json")
		if s == nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"status":"no poll completed yet"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(s)
	})
	mux.HandleFunc("/logs", func(w http.ResponseWriter, r *http.Request) {
		// Return recent log lines (already JSON-per-line from slog JSONHandler).
		// ?limit=N caps the number of most-recent entries returned.
		limit := 0
		if raw := r.URL.Query().Get("limit"); raw != "" {
			if n, err := strconv.Atoi(raw); err == nil && n > 0 {
				limit = n
			}
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		for _, line := range logRing.Snapshot(limit) {
			_, _ = w.Write(line)
		}
	})

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("http server failed", "err", err)
	}
}

// logRingBuffer is an io.Writer that keeps the last N writes in memory. Each
// Write is treated as one log entry — slog.JSONHandler calls Write exactly
// once per record, so entries do not get split across writes.
type logRingBuffer struct {
	mu   sync.Mutex
	buf  [][]byte
	cap  int
	next int
	size int
}

func newLogRingBuffer(capacity int) *logRingBuffer {
	return &logRingBuffer{cap: capacity, buf: make([][]byte, capacity)}
}

func (r *logRingBuffer) Write(p []byte) (int, error) {
	entry := make([]byte, len(p))
	copy(entry, p)
	r.mu.Lock()
	r.buf[r.next] = entry
	r.next = (r.next + 1) % r.cap
	if r.size < r.cap {
		r.size++
	}
	r.mu.Unlock()
	return len(p), nil
}

// Snapshot returns up to limit of the most recent entries in chronological
// order. limit <= 0 returns all buffered entries.
func (r *logRingBuffer) Snapshot(limit int) [][]byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := r.size
	if limit > 0 && limit < n {
		n = limit
	}
	out := make([][]byte, 0, n)
	// Start index so we walk from oldest-of-n to newest.
	start := (r.next - n + r.cap) % r.cap
	for i := 0; i < n; i++ {
		out = append(out, r.buf[(start+i)%r.cap])
	}
	return out
}
