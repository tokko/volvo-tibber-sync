package volvo

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
	AuthURL  = "https://volvoid.eu.volvocars.com/as/authorization.oauth2"
	TokenURL = "https://volvoid.eu.volvocars.com/as/token.oauth2"
)

// DefaultScopes covers everything the monitor needs from the Energy API.
// Only the Energy API subscription is required in the Volvo developer portal —
// conve:vehicle_relation belongs to the Connected Vehicle API and is not needed.
var DefaultScopes = []string{
	"openid",
	"energy:battery_charge_level",
	"energy:electric_range",
	"energy:estimated_charging_time",
	"energy:charging_system_status",
	"energy:charging_connection_status",
}

type Token struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	ExpiresAt    time.Time `json:"expires_at"`
}

func (t Token) Expired() bool {
	return time.Now().Add(60 * time.Second).After(t.ExpiresAt)
}

type TokenSource struct {
	http         *http.Client
	clientID     string
	clientSecret string
	mu           sync.Mutex
	tok          Token
	onRefresh    func(Token)
}

func NewTokenSource(clientID, clientSecret string, initial Token, onRefresh func(Token)) *TokenSource {
	return &TokenSource{
		http:         &http.Client{Timeout: 30 * time.Second},
		clientID:     clientID,
		clientSecret: clientSecret,
		tok:          initial,
		onRefresh:    onRefresh,
	}
}

func (ts *TokenSource) Access(ctx context.Context) (string, error) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if ts.tok.AccessToken != "" && !ts.tok.Expired() {
		return ts.tok.AccessToken, nil
	}
	if ts.tok.RefreshToken == "" {
		return "", fmt.Errorf("no refresh token; run oauth helper first")
	}
	next, err := ts.refresh(ctx, ts.tok.RefreshToken)
	if err != nil {
		return "", err
	}
	ts.tok = next
	if ts.onRefresh != nil {
		ts.onRefresh(next)
	}
	return ts.tok.AccessToken, nil
}

type tokenResp struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	TokenType    string `json:"token_type"`
	Scope        string `json:"scope"`
}

func (ts *TokenSource) refresh(ctx context.Context, refreshToken string) (Token, error) {
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)
	return ts.postToken(ctx, form)
}

// ExchangeCode swaps an authorization code for an initial token pair.
// Used by the oauth helper.
func (ts *TokenSource) ExchangeCode(ctx context.Context, code, codeVerifier, redirectURI string) (Token, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("code_verifier", codeVerifier)
	form.Set("redirect_uri", redirectURI)
	return ts.postToken(ctx, form)
}

func (ts *TokenSource) postToken(ctx context.Context, form url.Values) (Token, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return Token{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(ts.clientID, ts.clientSecret)

	resp, err := ts.http.Do(req)
	if err != nil {
		return Token{}, fmt.Errorf("token endpoint request: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<15))

	if resp.StatusCode != http.StatusOK {
		return Token{}, fmt.Errorf("token endpoint returned %d: %s", resp.StatusCode, string(body))
	}

	var tr tokenResp
	if err := json.Unmarshal(body, &tr); err != nil {
		return Token{}, fmt.Errorf("decode token response: %w", err)
	}

	next := Token{
		AccessToken:  tr.AccessToken,
		RefreshToken: tr.RefreshToken,
		ExpiresAt:    time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second),
	}
	if next.RefreshToken == "" {
		// Some IdPs don't rotate refresh tokens on refresh_token grant.
		next.RefreshToken = form.Get("refresh_token")
	}
	return next, nil
}
