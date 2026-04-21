// Command oauth performs the one-time Volvo OAuth2 PKCE authorization code
// flow, exchanges the returned code for an access + refresh token, and writes
// a .env file the monitor service can consume.
//
// The Volvo developer portal does not allow localhost redirect URIs, so the
// callback goes to a GitHub Pages page that displays the authorization code
// for copy-paste back into this helper.
//
// Usage:
//
//	oauth --client-id ... --client-secret ... --api-key ... --vin ...
//
// Flags can also be supplied via env vars (VOLVO_CLIENT_ID etc.); CLI flags win.
package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/tokko/volvo-tibber-sync/internal/config"
	"github.com/tokko/volvo-tibber-sync/internal/volvo"
)

const defaultRedirectURI = "https://tokko.github.io/volvo-tibber-sync/callback.html"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	_ = config.LoadDotEnv(".env")

	var (
		clientID    = flag.String("client-id", os.Getenv("VOLVO_CLIENT_ID"), "Volvo OAuth2 client id")
		clientSecret = flag.String("client-secret", os.Getenv("VOLVO_CLIENT_SECRET"), "Volvo OAuth2 client secret")
		apiKey      = flag.String("api-key", os.Getenv("VOLVO_API_KEY"), "Volvo VCC API key")
		vin         = flag.String("vin", os.Getenv("VOLVO_VIN"), "Vehicle VIN")
		redirectURI = flag.String("redirect-uri", defaultRedirectURI, "OAuth redirect URI registered in the Volvo developer portal")
		envOut      = flag.String("out", ".env", "path to write the resulting .env file")
		tokenOut    = flag.String("token-out", "./data/token.json", "path to seed the monitor token store (bind-mounted to /data)")
		scopesFlag  = flag.String("scopes", strings.Join(volvo.DefaultScopes, " "), "space-separated OAuth scopes")
	)
	flag.Parse()

	if *clientID == "" || *clientSecret == "" || *apiKey == "" || *vin == "" {
		flag.Usage()
		return errors.New("client-id, client-secret, api-key and vin are required (flags or env)")
	}

	codeVerifier, codeChallenge, err := pkcePair()
	if err != nil {
		return fmt.Errorf("generate PKCE: %w", err)
	}
	state, err := randString(24)
	if err != nil {
		return err
	}

	authURL, err := buildAuthURL(*clientID, *redirectURI, *scopesFlag, state, codeChallenge)
	if err != nil {
		return err
	}

	fmt.Println()
	fmt.Println("Open this URL in your browser to authorize:")
	fmt.Println()
	fmt.Println("  " + authURL)
	fmt.Println()
	_ = openBrowser(authURL)
	fmt.Println("After authorizing, the browser will redirect to a page showing an")
	fmt.Println("authorization code. Copy it and paste it here.")
	fmt.Println()
	fmt.Print("Authorization code: ")

	// Read from /dev/tty so this works when stdin is piped (e.g. install.sh).
	tty, err := os.Open("/dev/tty")
	if err != nil {
		tty = os.Stdin
	} else {
		defer tty.Close()
	}
	sc := bufio.NewScanner(tty)
	sc.Scan()
	code := strings.TrimSpace(sc.Text())
	if code == "" {
		return errors.New("no authorization code entered")
	}

	fmt.Println()
	fmt.Println("Exchanging code for tokens…")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	ts := volvo.NewTokenSource(*clientID, *clientSecret, volvo.Token{}, nil)
	tok, err := ts.ExchangeCode(ctx, code, codeVerifier, *redirectURI)
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

	// Seed the token store the monitor reads at startup. This avoids the
	// fragile one-shot env-bootstrap: the first refresh_token Volvo issued
	// is already persisted, so a container restart never re-consumes a token
	// that was rotated in a prior run.
	if err := writeTokenStore(*tokenOut, tok); err != nil {
		return fmt.Errorf("write token store %s: %w", *tokenOut, err)
	}
	fmt.Printf("Seeded %s — monitor will load this on startup.\n", *tokenOut)
	return nil
}

func writeTokenStore(path string, tok volvo.Token) error {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	b, err := json.MarshalIndent(tok, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
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
