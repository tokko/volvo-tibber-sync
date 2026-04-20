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
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/tokko/volvo-tibber-sync/internal/config"
	"github.com/tokko/volvo-tibber-sync/internal/tibber"
	"github.com/tokko/volvo-tibber-sync/internal/volvo"
)

var (
	flagOnce    = flag.Bool("once", false, "run one poll cycle and exit (useful for testing or cron)")
	flagDryRun  = flag.Bool("dry-run", false, "fetch Volvo state and log the intended Tibber push, but do not call Tibber")
)

func main() {
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
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
	}

	latest := &latestState{}

	ts := volvo.NewTokenSource(clientID, clientSecret, initial, func(t volvo.Token) {
		slog.Info("volvo token refreshed", "expires_at", t.ExpiresAt)
		if err := saveStoredToken(tokenStore, t); err != nil {
			slog.Warn("could not persist refreshed volvo token", "path", tokenStore, "err", err)
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

	pollOnce()
	if *flagOnce {
		slog.Info("--once: exiting after single poll")
		return nil
	}

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
		slog.Warn("charge state (partial)", attrs...)
		return
	}
	slog.Info("charge state", attrs...)
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
