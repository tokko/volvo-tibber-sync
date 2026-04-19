// Command monitor polls the Volvo Energy API on an interval and logs charge state
// as one JSON object per poll to stdout. It refreshes the OAuth access token
// automatically; the refresh token is read from env and rotations are persisted
// to TOKEN_STORE_PATH (default /data/token.json) so restarts don't need re-auth.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/andreasmikaelgustafsson/volvo-charge-monitor/internal/config"
	"github.com/andreasmikaelgustafsson/volvo-charge-monitor/internal/volvo"
)

func main() {
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
	httpAddr := config.Optional("HTTP_ADDR", ":8080")

	// Use stored token if present (survives restarts and keeps any rotated refresh
	// token), otherwise bootstrap from env.
	initial := volvo.Token{RefreshToken: refreshToken}
	if stored, ok := loadStoredToken(tokenStore); ok {
		initial = stored
		slog.Info("loaded token from store", "path", tokenStore, "expires_at", initial.ExpiresAt)
	}

	latest := &latestState{}

	ts := volvo.NewTokenSource(clientID, clientSecret, initial, func(t volvo.Token) {
		if err := saveStoredToken(tokenStore, t); err != nil {
			slog.Warn("could not persist refreshed token", "path", tokenStore, "err", err)
		}
	})
	client := volvo.NewClient(ts, apiKey, vin)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	go serveHTTP(ctx, httpAddr, latest)

	slog.Info("starting monitor",
		"vin", vin,
		"poll_interval", pollInterval.String(),
		"http_addr", httpAddr,
		"token_store", tokenStore,
	)

	// Poll once immediately, then on interval.
	pollOnce := func() {
		pCtx, pCancel := context.WithTimeout(ctx, 2*time.Minute)
		defer pCancel()
		state := client.FetchChargeState(pCtx)
		latest.Lock()
		latest.state = &state
		latest.Unlock()
		logState(state)
	}

	pollOnce()
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
