package deco

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	ErrAuthentication = errors.New("deco session rejected")
	ErrBadCredentials = errors.New("deco owner login rejected; restart after correcting credentials")
)

type Client struct {
	mu         sync.Mutex
	http       *http.Client
	baseURL    *url.URL
	password   string
	pin        []byte
	aesKey     []byte
	aesIV      []byte
	rsaKey     *rsa.PublicKey
	hash       string
	stok       string
	sequence   uint
	authLocked bool
}

type ConnectedClient struct {
	MAC    string
	Online bool
}

func NewClient(routerURL, ownerPassword, publicKeySHA256 string) (*Client, error) {
	u, err := url.Parse(strings.TrimSpace(routerURL))
	if err != nil || u.Scheme != "https" || u.User != nil || u.Host == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("Deco router URL must be an HTTPS origin")
	}
	host, _, err := net.SplitHostPort(u.Host)
	if err != nil {
		host = u.Hostname()
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsPrivate() {
		return nil, errors.New("Deco router URL must use a private IP address")
	}
	if strings.TrimSpace(ownerPassword) == "" {
		return nil, errors.New("Deco owner password is empty")
	}
	pin, err := hex.DecodeString(strings.ReplaceAll(strings.TrimSpace(publicKeySHA256), ":", ""))
	if err != nil || len(pin) != sha256.Size {
		return nil, errors.New("Deco public-key SHA-256 pin must be 64 hexadecimal characters")
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{
			// The enrolled public-key pin is this client’s explicit trust anchor.
			// The global default transport and Volvo/Tibber clients keep normal TLS verification.
			InsecureSkipVerify: true, // #nosec G402 -- VerifyConnection checks the configured router key pin.
			VerifyConnection: func(state tls.ConnectionState) error {
				if len(state.PeerCertificates) == 0 {
					return errors.New("Deco TLS peer sent no certificate")
				}
				cert := state.PeerCertificates[0]
				now := time.Now()
				if now.Before(cert.NotBefore) || now.After(cert.NotAfter) {
					return errors.New("Deco TLS certificate is outside its validity period")
				}
				publicKey, err := x509.MarshalPKIXPublicKey(cert.PublicKey)
				if err != nil {
					return fmt.Errorf("encode Deco TLS public key: %w", err)
				}
				hash := sha256.Sum256(publicKey)
				if !bytes.Equal(hash[:], pin) {
					return errors.New("Deco TLS public-key pin mismatch")
				}
				return nil
			},
		},
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
	}
	return &Client{
		http: &http.Client{
			Transport: transport,
			Jar:       jar,
			Timeout:   15 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		baseURL:  u,
		password: ownerPassword,
		pin:      pin,
	}, nil
}

// ConnectedClients returns a complete mesh list. Calls and reauthentication
// share one lock because Deco binds the encrypted session to sequence state.
func (c *Client) ConnectedClients(ctx context.Context) ([]ConnectedClient, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.authLocked {
		return nil, ErrBadCredentials
	}
	if c.stok == "" {
		if err := c.authenticate(ctx); err != nil {
			if errors.Is(err, ErrBadCredentials) {
				c.authLocked = true
			}
			return nil, err
		}
	}
	clients, err := c.clientList(ctx)
	if !errors.Is(err, ErrAuthentication) {
		return clients, err
	}
	c.clearSession()
	if err := c.authenticate(ctx); err != nil {
		if errors.Is(err, ErrBadCredentials) {
			c.authLocked = true
		}
		return nil, err
	}
	clients, err = c.clientList(ctx)
	if errors.Is(err, ErrAuthentication) {
		c.clearSession()
	}
	return clients, err
}

func (c *Client) authenticate(ctx context.Context) error {
	key, iv, err := newAESKey()
	if err != nil {
		return fmt.Errorf("prepare Deco session: %w", err)
	}
	c.aesKey, c.aesIV = key, iv
	c.hash = fmt.Sprintf("%x", md5.Sum([]byte("admin"+c.password)))

	var passwordKey struct {
		ErrorCode *int `json:"error_code"`
		Result    *struct {
			Password []string `json:"password"`
		} `json:"result"`
	}
	if err := c.postPlain(ctx, "/cgi-bin/luci/;stok=/login", "keys", []byte(`{"operation":"read"}`), &passwordKey); err != nil {
		return fmt.Errorf("read Deco password key: %w", err)
	}
	if err := validateAuthEnvelope(passwordKey.ErrorCode, passwordKey.Result != nil); err != nil {
		return err
	}
	passRSA, err := parseRSAPublicKey(passwordKey.Result.Password)
	if err != nil {
		return fmt.Errorf("decode Deco password key: %w", err)
	}

	var sessionKey struct {
		ErrorCode *int `json:"error_code"`
		Result    *struct {
			Sequence *uint    `json:"seq"`
			Key      []string `json:"key"`
		} `json:"result"`
	}
	if err := c.postPlain(ctx, "/cgi-bin/luci/;stok=/login", "auth", []byte(`{"operation":"read"}`), &sessionKey); err != nil {
		return fmt.Errorf("read Deco session key: %w", err)
	}
	if err := validateAuthEnvelope(sessionKey.ErrorCode, sessionKey.Result != nil); err != nil {
		return err
	}
	if sessionKey.Result.Sequence == nil {
		return errors.New("Deco session key response omitted sequence")
	}
	c.rsaKey, err = parseRSAPublicKey(sessionKey.Result.Key)
	if err != nil {
		return fmt.Errorf("decode Deco session key: %w", err)
	}
	c.sequence = *sessionKey.Result.Sequence

	encryptedPassword, err := rsaEncryptHex(c.password, passRSA)
	if err != nil {
		return fmt.Errorf("encrypt Deco owner password: %w", err)
	}
	loginBody, err := json.Marshal(map[string]any{
		"operation": "login",
		"params":    map[string]string{"password": encryptedPassword},
	})
	if err != nil {
		return err
	}
	response, err := c.postEncrypted(ctx, "/cgi-bin/luci/;stok=/login", "login", loginBody, true)
	if err != nil {
		if errors.Is(err, ErrAuthentication) {
			return ErrBadCredentials
		}
		return fmt.Errorf("Deco login exchange: %w", err)
	}
	var loginResult struct {
		ErrorCode *int `json:"error_code"`
		Result    *struct {
			Stok string `json:"stok"`
		} `json:"result"`
	}
	if err := json.Unmarshal(response, &loginResult); err != nil {
		return fmt.Errorf("decode Deco login response")
	}
	if loginResult.ErrorCode != nil && *loginResult.ErrorCode == -5002 {
		return ErrBadCredentials
	}
	if loginResult.ErrorCode != nil && *loginResult.ErrorCode != 0 {
		return fmt.Errorf("Deco login rejected with error code %d", *loginResult.ErrorCode)
	}
	if loginResult.Result == nil || loginResult.Result.Stok == "" {
		return errors.New("Deco login response omitted session token")
	}
	c.stok = loginResult.Result.Stok
	return nil
}

func (c *Client) clientList(ctx context.Context) ([]ConnectedClient, error) {
	body := []byte(`{"operation":"read","params":{"device_mac":"default"}}`)
	response, err := c.postEncrypted(ctx, "/cgi-bin/luci/;stok="+url.PathEscape(c.stok)+"/admin/client", "client_list", body, false)
	if err != nil {
		return nil, err
	}
	var envelope struct {
		ErrorCode *int `json:"error_code"`
		Result    *struct {
			ClientList *[]struct {
				MAC    *string `json:"mac"`
				Online *bool   `json:"online"`
			} `json:"client_list"`
		} `json:"result"`
	}
	if err := json.Unmarshal(response, &envelope); err != nil {
		return nil, errors.New("malformed Deco client-list response")
	}
	if envelope.ErrorCode == nil || *envelope.ErrorCode != 0 {
		return nil, errors.New("Deco client-list response did not confirm success")
	}
	if envelope.Result == nil || envelope.Result.ClientList == nil {
		return nil, errors.New("Deco client-list response was incomplete")
	}
	clients := make([]ConnectedClient, 0, len(*envelope.Result.ClientList))
	for _, entry := range *envelope.Result.ClientList {
		if entry.MAC == nil || entry.Online == nil {
			return nil, errors.New("Deco client-list entry omitted MAC or online status")
		}
		mac, err := normalizeMAC(*entry.MAC)
		if err != nil {
			return nil, errors.New("Deco client-list contained an invalid MAC address")
		}
		clients = append(clients, ConnectedClient{MAC: mac, Online: *entry.Online})
	}
	return clients, nil
}

func (c *Client) postPlain(ctx context.Context, path, form string, body []byte, out any) error {
	response, status, err := c.post(ctx, path, form, body)
	if err != nil {
		return err
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return ErrBadCredentials
	}
	if status != http.StatusOK {
		return fmt.Errorf("Deco returned HTTP %d", status)
	}
	if err := json.Unmarshal(response, out); err != nil {
		return errors.New("malformed Deco authentication response")
	}
	return nil
}

func (c *Client) postEncrypted(ctx context.Context, path, form string, body []byte, login bool) (json.RawMessage, error) {
	encrypted, err := encryptAES(body, c.aesKey, c.aesIV)
	if err != nil {
		return nil, err
	}
	sign := fmt.Sprintf("h=%s&s=%d", c.hash, int(c.sequence)+len(encrypted))
	if login {
		sign = fmt.Sprintf("k=%s&i=%s&h=%s&s=%d", c.aesKey, c.aesIV, c.hash, int(c.sequence)+len(encrypted))
	}
	signed, err := rsaEncryptChunks(sign, c.rsaKey)
	if err != nil {
		return nil, fmt.Errorf("sign Deco request: %w", err)
	}
	formBody := url.Values{"sign": {signed}, "data": {encrypted}}.Encode()
	response, status, err := c.post(ctx, path, form, []byte(formBody))
	if err != nil {
		return nil, err
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return nil, ErrAuthentication
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("Deco returned HTTP %d", status)
	}
	var envelope struct {
		Data *string `json:"data"`
	}
	if err := json.Unmarshal(response, &envelope); err != nil || envelope.Data == nil {
		return nil, errors.New("malformed encrypted Deco response")
	}
	if *envelope.Data == "" {
		return nil, ErrAuthentication
	}
	decoded, err := decryptAES(*envelope.Data, c.aesKey, c.aesIV)
	if err != nil {
		return nil, errors.New("could not decrypt Deco response")
	}
	return json.RawMessage(decoded), nil
}

func (c *Client) post(ctx context.Context, path, form string, body []byte) ([]byte, int, error) {
	u := *c.baseURL
	u.Path = strings.TrimRight(u.Path, "/") + path
	query := u.Query()
	query.Set("form", form)
	u.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	contentType := "application/json"
	if form == "login" || form == "client_list" {
		contentType = "application/x-www-form-urlencoded"
	}
	req.Header.Set("Content-Type", contentType)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, errors.New("Deco local request failed")
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return data, resp.StatusCode, nil
}

func (c *Client) clearSession() {
	c.aesKey, c.aesIV, c.rsaKey = nil, nil, nil
	c.hash, c.stok = "", ""
	c.sequence = 0
	jar, _ := cookiejar.New(nil)
	c.http.Jar = jar
}

func validateAuthEnvelope(code *int, hasResult bool) error {
	if code == nil {
		return errors.New("Deco authentication response omitted error code")
	}
	if *code != 0 {
		if *code == -5002 {
			return ErrBadCredentials
		}
		return fmt.Errorf("Deco authentication key request rejected with error code %d", *code)
	}
	if !hasResult {
		return errors.New("Deco authentication response omitted result")
	}
	return nil
}

func parseRSAPublicKey(parts []string) (*rsa.PublicKey, error) {
	if len(parts) != 2 {
		return nil, errors.New("RSA key must contain modulus and exponent")
	}
	n := new(big.Int)
	if _, ok := n.SetString(parts[0], 16); !ok || n.Sign() <= 0 {
		return nil, errors.New("invalid RSA modulus")
	}
	e64, err := strconv.ParseUint(parts[1], 16, 32)
	if err != nil || e64 < 3 || e64%2 == 0 {
		return nil, errors.New("invalid RSA exponent")
	}
	return &rsa.PublicKey{N: n, E: int(e64)}, nil
}

func rsaEncryptHex(value string, key *rsa.PublicKey) (string, error) {
	ciphertext, err := rsa.EncryptPKCS1v15(rand.Reader, key, []byte(value))
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(ciphertext), nil
}

func rsaEncryptChunks(value string, key *rsa.PublicKey) (string, error) {
	const chunkSize = 53 // Deco's firmware expects the 512-bit-key signing split used by its app.
	var result strings.Builder
	for len(value) > 0 {
		n := chunkSize
		if n > len(value) {
			n = len(value)
		}
		part, err := rsaEncryptHex(value[:n], key)
		if err != nil {
			return "", err
		}
		result.WriteString(part)
		value = value[n:]
	}
	return result.String(), nil
}

func newAESKey() ([]byte, []byte, error) {
	limit := new(big.Int).Exp(big.NewInt(10), big.NewInt(16), nil)
	newDigits := func() ([]byte, error) {
		n, err := rand.Int(rand.Reader, limit)
		if err != nil {
			return nil, err
		}
		return []byte(fmt.Sprintf("%016d", n.Int64())), nil
	}
	key, err := newDigits()
	if err != nil {
		return nil, nil, err
	}
	iv, err := newDigits()
	return key, iv, err
}

func encryptAES(plaintext, key, iv []byte) (string, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	pad := aes.BlockSize - len(plaintext)%aes.BlockSize
	input := append(append([]byte(nil), plaintext...), bytes.Repeat([]byte{byte(pad)}, pad)...)
	output := make([]byte, len(input))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(output, input)
	return base64.StdEncoding.EncodeToString(output), nil
}

func decryptAES(encoded string, key, iv []byte) ([]byte, error) {
	ciphertext, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(ciphertext) == 0 || len(ciphertext)%aes.BlockSize != 0 {
		return nil, errors.New("invalid AES ciphertext")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	plaintext := make([]byte, len(ciphertext))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(plaintext, ciphertext)
	pad := int(plaintext[len(plaintext)-1])
	if pad == 0 || pad > aes.BlockSize || pad > len(plaintext) {
		return nil, errors.New("invalid AES padding")
	}
	for _, b := range plaintext[len(plaintext)-pad:] {
		if int(b) != pad {
			return nil, errors.New("invalid AES padding")
		}
	}
	return plaintext[:len(plaintext)-pad], nil
}

func normalizeMAC(value string) (string, error) {
	mac, err := net.ParseMAC(strings.TrimSpace(value))
	if err != nil || !validUnicastMAC(mac) {
		return "", errors.New("invalid unicast MAC")
	}
	return strings.ToLower(mac.String()), nil
}

func validUnicastMAC(mac net.HardwareAddr) bool {
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
