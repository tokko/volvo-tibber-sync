// Package tibber is a minimal Go client for Tibber's undocumented app API
// (app.tibber.com/v4/gql). The public developer API does not expose a write
// path for vehicle state-of-charge; the app API does. This is unofficial —
// Tibber can change or break the endpoint at any time.
//
// Based on the behavior of github.com/Elibart-home/tibber_soc_updater.
package tibber

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	LoginURL    = "https://app.tibber.com/login.credentials"
	GQLEndpoint = "https://app.tibber.com/v4/gql"

	// Tibber JWTs live ~20h; we refresh early so a long-running process never
	// hits an expired token mid-request.
	tokenTTL          = 18 * time.Hour
	refreshBeforeEnd  = 30 * time.Minute
)

// Session holds credentials and a cached JWT token. The zero value is not
// valid — use NewSession.
type Session struct {
	http      *http.Client
	email     string
	password  string
	onRefresh func(token string, expiresAt time.Time)

	mu        sync.Mutex
	token     string
	expiresAt time.Time
}

func NewSession(email, password string) *Session {
	return &Session{
		http:     &http.Client{Timeout: 30 * time.Second},
		email:    email,
		password: password,
	}
}

// SetOnRefresh registers a callback fired whenever a fresh token is obtained.
// Use it to persist the token to disk so restarts within the 18h window
// don't need to re-authenticate.
func (s *Session) SetOnRefresh(fn func(token string, expiresAt time.Time)) {
	s.onRefresh = fn
}

// Seed installs a previously-obtained token and its expiry so restarts can
// skip login.
func (s *Session) Seed(token string, expiresAt time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.token = token
	s.expiresAt = expiresAt
}

// Invalidate clears the cached token so the next Token() call re-logs in.
// Use this when the server rejects the token (e.g. a 401 — Tibber invalidates
// prior sessions when the user logs in on another device, long before our
// hardcoded TTL would have expired).
func (s *Session) Invalidate() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.token = ""
	s.expiresAt = time.Time{}
}

// Token returns a valid JWT, re-authenticating if the cached one is missing
// or close to expiring.
func (s *Session) Token(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.token != "" && time.Now().Add(refreshBeforeEnd).Before(s.expiresAt) {
		return s.token, nil
	}
	tok, err := s.login(ctx)
	if err != nil {
		return "", err
	}
	s.token = tok
	s.expiresAt = time.Now().Add(tokenTTL)
	if s.onRefresh != nil {
		s.onRefresh(s.token, s.expiresAt)
	}
	return s.token, nil
}

type loginResponse struct {
	Token string `json:"token"`
}

func (s *Session) login(ctx context.Context) (string, error) {
	if s.email == "" || s.password == "" {
		return "", fmt.Errorf("tibber email/password not set; cannot log in")
	}
	form := url.Values{}
	form.Set("email", s.email)
	form.Set("password", s.password)

	req, err := http.NewRequestWithContext(ctx, "POST", LoginURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Origin", "https://app.tibber.com")
	req.Header.Set("Referer", "https://app.tibber.com/")

	resp, err := s.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("tibber login request: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("tibber login returned %d: %s", resp.StatusCode, truncate(string(body), 300))
	}
	var lr loginResponse
	if err := json.Unmarshal(body, &lr); err != nil {
		return "", fmt.Errorf("decode tibber login: %w (body: %s)", err, truncate(string(body), 300))
	}
	if lr.Token == "" {
		return "", fmt.Errorf("tibber login returned no token: %s", truncate(string(body), 300))
	}
	return lr.Token, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
