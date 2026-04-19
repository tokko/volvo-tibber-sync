// Command oauth performs the one-time Volvo OAuth2 PKCE authorization code
// flow, exchanges the returned code for an access + refresh token, and writes
// a .env file the monitor service can consume.
//
// Usage:
//
//	oauth --client-id ... --client-secret ... --api-key ... --vin ...
//
// Flags can also be supplied via env vars (VOLVO_CLIENT_ID etc.); CLI flags win.
package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/tokko/volvo-tibber-sync/internal/config"
	"github.com/tokko/volvo-tibber-sync/internal/volvo"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	_ = config.LoadDotEnv(".env")

	var (
		clientID     = flag.String("client-id", os.Getenv("VOLVO_CLIENT_ID"), "Volvo OAuth2 client id")
		clientSecret = flag.String("client-secret", os.Getenv("VOLVO_CLIENT_SECRET"), "Volvo OAuth2 client secret")
		apiKey       = flag.String("api-key", os.Getenv("VOLVO_API_KEY"), "Volvo VCC API key")
		vin          = flag.String("vin", os.Getenv("VOLVO_VIN"), "Vehicle VIN")
		port         = flag.Int("port", 8090, "local port for OAuth redirect server")
		envOut       = flag.String("out", ".env", "path to write the resulting .env file")
		scopesFlag   = flag.String("scopes", strings.Join(volvo.DefaultScopes, " "), "space-separated OAuth scopes")
	)
	flag.Parse()

	if *clientID == "" || *clientSecret == "" || *apiKey == "" || *vin == "" {
		flag.Usage()
		return errors.New("client-id, client-secret, api-key and vin are required (flags or env)")
	}

	redirectURI := fmt.Sprintf("http://localhost:%d/callback", *port)
	fmt.Printf("Redirect URI that must be registered in your Volvo app: %s\n", redirectURI)

	codeVerifier, codeChallenge, err := pkcePair()
	if err != nil {
		return fmt.Errorf("generate PKCE: %w", err)
	}
	state, err := randString(24)
	if err != nil {
		return err
	}

	authURL, err := buildAuthURL(*clientID, redirectURI, *scopesFlag, state, codeChallenge)
	if err != nil {
		return err
	}

	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if errStr := q.Get("error"); errStr != "" {
			msg := fmt.Sprintf("authorization error: %s — %s", errStr, q.Get("error_description"))
			http.Error(w, msg, http.StatusBadRequest)
			errCh <- errors.New(msg)
			return
		}
		if got := q.Get("state"); got != state {
			http.Error(w, "state mismatch", http.StatusBadRequest)
			errCh <- fmt.Errorf("state mismatch: got %q", got)
			return
		}
		code := q.Get("code")
		if code == "" {
			http.Error(w, "missing code", http.StatusBadRequest)
			errCh <- errors.New("callback missing code")
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<html><body><h2>Volvo OAuth success</h2><p>You can close this tab.</p></body></html>"))
		codeCh <- code
	})

	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", *port))
	if err != nil {
		return fmt.Errorf("listen on :%d: %w", *port, err)
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := srv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()
	defer srv.Close()

	fmt.Println()
	fmt.Println("Open this URL in your browser to authorize (it should open automatically):")
	fmt.Println()
	fmt.Println("  " + authURL)
	fmt.Println()
	_ = openBrowser(authURL)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	var code string
	select {
	case code = <-codeCh:
	case err := <-errCh:
		return err
	case <-ctx.Done():
		return errors.New("timed out waiting for authorization callback")
	}

	fmt.Println("Got authorization code, exchanging for tokens…")
	ts := volvo.NewTokenSource(*clientID, *clientSecret, volvo.Token{}, nil)
	tok, err := ts.ExchangeCode(ctx, code, codeVerifier, redirectURI)
	if err != nil {
		return err
	}

	fmt.Printf("Success. Access token expires at %s.\n", tok.ExpiresAt.Format(time.RFC3339))
	if err := config.UpdateDotEnv(*envOut, map[string]string{
		"VOLVO_CLIENT_ID":     *clientID,
		"VOLVO_CLIENT_SECRET": *clientSecret,
		"VOLVO_API_KEY":       *apiKey,
		"VOLVO_VIN":           *vin,
		"VOLVO_REFRESH_TOKEN": tok.RefreshToken,
	}); err != nil {
		return fmt.Errorf("write %s: %w", *envOut, err)
	}
	fmt.Printf("Wrote %s — keep this file secret.\n", *envOut)
	return nil
}

func buildAuthURL(clientID, redirectURI, scopes, state, challenge string) (string, error) {
	u, err := url.Parse(volvo.AuthURL)
	if err != nil {
		return "", err
	}
	q := u.Query()
	q.Set("response_type", "code")
	q.Set("client_id", clientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("scope", scopes)
	q.Set("state", state)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func pkcePair() (verifier, challenge string, err error) {
	buf := make([]byte, 64)
	if _, err = rand.Read(buf); err != nil {
		return "", "", err
	}
	verifier = base64.RawURLEncoding.EncodeToString(buf)
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return
}

func randString(nBytes int) (string, error) {
	buf := make([]byte, nBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func openBrowser(u string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", u)
	case "darwin":
		cmd = exec.Command("open", u)
	default:
		cmd = exec.Command("xdg-open", u)
	}
	return cmd.Start()
}
