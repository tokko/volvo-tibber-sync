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
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	_ "time/tzdata" // embed zoneinfo so LoadLocation works in distroless (no OS tzdata)

	"github.com/tokko/volvo-tibber-sync/internal/config"
	"github.com/tokko/volvo-tibber-sync/internal/tibber"
	"github.com/tokko/volvo-tibber-sync/internal/volvo"
)

var (
	flagOnce    = flag.Bool("once", false, "run one poll cycle and exit (useful for testing or cron)")
	flagDryRun  = flag.Bool("dry-run", false, "fetch Volvo state and log the intended action, but do not call Tibber or send a lock command")
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
			return
		}
		pushToTibber(pCtx, tbrClient, tbrConf, state)
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
	homeID      string
	vehicleID   string
	vehicleName string
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

	if email == "" && password == "" && homeID == "" && vehicleID == "" {
		return nil, tibberConfig{}
	}
	if email == "" || password == "" || homeID == "" || vehicleID == "" {
		slog.Warn("tibber partially configured; skipping push — need TIBBER_EMAIL, TIBBER_PASSWORD, TIBBER_HOME_ID, TIBBER_VEHICLE_ID")
		return nil, tibberConfig{}
	}

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
		homeID:      homeID,
		vehicleID:   vehicleID,
		vehicleName: vehicleName,
	}
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
