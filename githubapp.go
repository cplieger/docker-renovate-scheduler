package main

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/cplieger/envx/v2"
	"github.com/cplieger/httpx/v5"
)

// The scheduler's GitHub App settings stay outside RENOVATE_* so Renovate
// never reads them as its own options.
const (
	envAppID             = "GITHUB_APP_ID"
	envAppInstallationID = "GITHUB_APP_INSTALLATION_ID"
	envAppKeyFile        = "GITHUB_APP_PRIVATE_KEY_FILE"
	// envAppKeyInline is refused, never read: the key must come from a file.
	envAppKeyInline = "GITHUB_APP_PRIVATE_KEY"
)

const (
	githubAPIBase   = "https://api.github.com"
	githubAPIVer    = "2022-11-28"
	maxKeyFileBytes = 64 << 10
	maxAPIBodyBytes = 1 << 20
	// GitHub rejects an exp more than 10 minutes ahead of ITS clock; 9 minutes
	// plus the 60 s iat backdate tolerates a minute of skew either way.
	jwtBackdate = 60 * time.Second
	jwtLifetime = 9 * time.Minute
	// installationTokenLife is GitHub's fixed lifetime for an installation token.
	installationTokenLife = time.Hour
	tokenRequestTimeout   = 30 * time.Second
	tokenMintBudget       = 2 * time.Minute
	tokenMintAttempts     = 4
	tokenExpiryMargin     = 2 * time.Minute
)

// appConfig is the validated GitHub App configuration. A nil *appConfig means
// the App settings are absent and Renovate uses its own token.
type appConfig struct {
	key            *rsa.PrivateKey
	appID          int
	installationID int // 0 resolves the App's single installation at first use
}

// loadAppConfig reads the App settings. Absent settings return (nil, nil);
// partial or unusable settings return an error so the daemon stops at start.
// No error quotes a setting's value: a miswired container can hold a token or
// the key itself in any of them.
func loadAppConfig() (*appConfig, error) {
	if envx.String(envAppKeyInline) != "" {
		return nil, fmt.Errorf("%s is not read: put the private key in a file and set %s to its path", envAppKeyInline, envAppKeyFile)
	}
	appID, idSet, err := envx.IntStrict(envAppID)
	if err != nil {
		return nil, notPositiveInt(envAppID)
	}
	instID, instSet, err := envx.IntStrict(envAppInstallationID)
	if err != nil {
		return nil, notPositiveInt(envAppInstallationID)
	}
	keyPath := envx.String(envAppKeyFile)
	if !idSet && !instSet && keyPath == "" {
		return nil, nil
	}
	switch {
	case !idSet:
		return nil, fmt.Errorf("%s is required when %s or %s is set", envAppID, envAppKeyFile, envAppInstallationID)
	case keyPath == "":
		return nil, fmt.Errorf("%s is required when %s is set", envAppKeyFile, envAppID)
	case appID <= 0:
		return nil, notPositiveInt(envAppID)
	case instSet && instID <= 0:
		return nil, notPositiveInt(envAppInstallationID)
	case strings.ContainsAny(keyPath, "\r\n") || strings.Contains(keyPath, "-----BEGIN"):
		return nil, fmt.Errorf("%s holds key material, not a path: put the key in a file and set %s to that file's path", envAppKeyFile, envAppKeyFile)
	}
	key, err := readAppKey(keyPath)
	if err != nil {
		return nil, err
	}
	return &appConfig{key: key, appID: appID, installationID: instID}, nil
}

func notPositiveInt(key string) error {
	return fmt.Errorf("%s must be a positive integer", key)
}

// readAppKey parses an RSA private key from a PEM file in PKCS#1 (GitHub's
// download format) or PKCS#8. Errors name neither the path nor the content.
func readAppKey(path string) (*rsa.PrivateKey, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, keyFileError("open", err)
	}
	defer func() { _ = f.Close() }()
	raw, err := io.ReadAll(io.LimitReader(f, maxKeyFileBytes+1))
	if err != nil {
		return nil, keyFileError("read", err)
	}
	if len(raw) > maxKeyFileBytes {
		return nil, fmt.Errorf("%s: the file is larger than %d bytes, not a private key", envAppKeyFile, maxKeyFileBytes)
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, fmt.Errorf("%s: the file holds no PEM block", envAppKeyFile)
	}
	var key *rsa.PrivateKey
	switch block.Type {
	case "RSA PRIVATE KEY":
		key, err = x509.ParsePKCS1PrivateKey(block.Bytes)
	case "PRIVATE KEY":
		var parsed any
		parsed, err = x509.ParsePKCS8PrivateKey(block.Bytes)
		if err == nil {
			var ok bool
			if key, ok = parsed.(*rsa.PrivateKey); !ok {
				return nil, fmt.Errorf("%s: the file holds a key that is not RSA", envAppKeyFile)
			}
		}
	default:
		// The block label is file content, so the error does not quote it.
		return nil, fmt.Errorf("%s: unsupported PEM block, want \"RSA PRIVATE KEY\" (PKCS#1) or \"PRIVATE KEY\" (PKCS#8)", envAppKeyFile)
	}
	if err != nil {
		return nil, fmt.Errorf("%s: the file is not a valid RSA private key", envAppKeyFile)
	}
	return key, nil
}

// keyFileError drops the path an *fs.PathError carries, since a miswired
// setting can put a credential where the path belongs.
func keyFileError(op string, err error) error {
	if pe, ok := errors.AsType[*fs.PathError](err); ok {
		err = pe.Err
	}
	return fmt.Errorf("%s: cannot %s the file: %w", envAppKeyFile, op, err)
}

// signAppJWT returns the RS256 JSON Web Token that authenticates as the App.
func signAppJWT(key *rsa.PrivateKey, appID int, now time.Time) (string, error) {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, err := json.Marshal(struct {
		Iss string `json:"iss"`
		Iat int64  `json:"iat"`
		Exp int64  `json:"exp"`
	}{
		Iss: strconv.Itoa(appID),
		Iat: now.Add(-jwtBackdate).Unix(),
		Exp: now.Add(jwtLifetime).Unix(),
	})
	if err != nil {
		return "", err
	}
	signingInput := header + "." + base64.RawURLEncoding.EncodeToString(claims)
	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("sign app JWT: %w", err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// installationToken is one minted token. value is a credential: never log it.
type installationToken struct {
	expiresAt time.Time
	value     string
}

// appTokenMinter exchanges the App's JWT for an installation token. Only the
// executor goroutine calls mint, so installationID needs no lock.
type appTokenMinter struct {
	now            func() time.Time
	client         *http.Client
	key            *rsa.PrivateKey
	apiBase        string
	appID          int
	installationID int
	baseDelay      time.Duration
}

func newAppTokenMinter(cfg *appConfig) *appTokenMinter {
	if cfg == nil {
		return nil
	}
	return &appTokenMinter{
		now:            time.Now,
		client:         httpx.NewClient(tokenRequestTimeout),
		key:            cfg.key,
		apiBase:        githubAPIBase,
		appID:          cfg.appID,
		installationID: cfg.installationID,
		baseDelay:      httpx.DefaultBaseDelay,
	}
}

// mint returns a fresh installation token, resolving the installation first
// when GITHUB_APP_INSTALLATION_ID was not set.
func (m *appTokenMinter) mint(ctx context.Context) (installationToken, error) {
	ctx, cancel := context.WithTimeout(ctx, tokenMintBudget)
	defer cancel()
	jwt, err := signAppJWT(m.key, m.appID, m.now())
	if err != nil {
		return installationToken{}, err
	}
	if m.installationID == 0 {
		id, err := m.resolveInstallation(ctx, jwt)
		if err != nil {
			return installationToken{}, err
		}
		m.installationID = id
	}
	var resp struct {
		ExpiresAt time.Time `json:"expires_at"`
		Token     string    `json:"token"`
	}
	path := fmt.Sprintf("/app/installations/%d/access_tokens", m.installationID)
	if err := m.call(ctx, http.MethodPost, path, jwt, &resp); err != nil {
		return installationToken{}, err
	}
	if resp.Token == "" || resp.ExpiresAt.IsZero() {
		return installationToken{}, errors.New("POST " + path + ": response carries no token or expires_at")
	}
	return installationToken{value: resp.Token, expiresAt: resp.ExpiresAt}, nil
}

func (m *appTokenMinter) resolveInstallation(ctx context.Context, jwt string) (int, error) {
	var list []struct {
		Account struct {
			Login string `json:"login"`
		} `json:"account"`
		ID int `json:"id"`
	}
	if err := m.call(ctx, http.MethodGet, "/app/installations?per_page=100", jwt, &list); err != nil {
		return 0, err
	}
	switch len(list) {
	case 0:
		return 0, errors.New("the GitHub App has no installation: install it on the account Renovate serves")
	case 1:
		slog.Info("github app installation resolved",
			"installation_id", list[0].ID, "account", list[0].Account.Login)
		return list[0].ID, nil
	}
	accounts := make([]string, 0, len(list))
	for _, inst := range list {
		accounts = append(accounts, fmt.Sprintf("%s=%d", inst.Account.Login, inst.ID))
	}
	return 0, fmt.Errorf("the GitHub App has %d installations (%s): set %s",
		len(list), strings.Join(accounts, ", "), envAppInstallationID)
}

// call makes one REST request with retries on transport errors and 5xx, and
// decodes a 2xx body into out. Error text carries GitHub's message only,
// never a response body, which for the token endpoint holds the token.
func (m *appTokenMinter) call(ctx context.Context, method, path, jwt string, out any) error {
	_, err := httpx.Do(ctx, func(ctx context.Context) (struct{}, error) {
		return struct{}{}, m.attempt(ctx, method, path, jwt, out)
	}, httpx.WithMaxAttempts(tokenMintAttempts), httpx.WithBaseDelay(m.baseDelay),
		httpx.WithLabel("github app "+method+" "+path))
	return err
}

func (m *appTokenMinter) attempt(ctx context.Context, method, path, jwt string, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, m.apiBase+path, http.NoBody)
	if err != nil {
		return httpx.Permanent(err)
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", githubAPIVer)
	resp, err := m.client.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, httpx.LogSafeError(err))
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := httpx.ReadLimitedBody(resp.Body, maxAPIBodyBytes)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		apiErr := &githubAPIError{method: method, path: path, status: resp.StatusCode, message: githubMessage(body, jwt)}
		if resp.StatusCode >= 500 {
			return httpx.MarkTransient(apiErr)
		}
		return httpx.Permanent(apiErr)
	}
	if err := json.NewDecoder(bytes.NewReader(body)).Decode(out); err != nil {
		return httpx.Permanent(fmt.Errorf("%s %s: malformed response body", method, path))
	}
	return nil
}

// githubAPIError is a non-2xx answer from GitHub's REST API.
type githubAPIError struct {
	method  string
	path    string
	message string
	status  int
}

func (e *githubAPIError) Error() string {
	msg := fmt.Sprintf("%s %s: HTTP %d", e.method, e.path, e.status)
	if e.message != "" {
		msg += ": " + e.message
	}
	switch e.status {
	case http.StatusUnauthorized:
		msg += " (check " + envAppID + " and that the private key belongs to that App)"
	case http.StatusNotFound:
		msg += " (check " + envAppInstallationID + " and that the App is installed)"
	}
	return msg
}

const maxGitHubMessage = 200

// githubMessage extracts GitHub's error "message" with secret redacted,
// bounded, or "" when the body is not GitHub's error shape. The order is
// redact, normalize, redact, cap: a server can echo the request's JWT.
func githubMessage(body []byte, secret string) string {
	var e struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &e) != nil {
		return ""
	}
	msg := httpx.RedactSecretString(e.Message, httpx.Secret(secret))
	msg = httpx.RedactSecretString(strings.ToValidUTF8(msg, ""), httpx.Secret(secret))
	return strings.ToValidUTF8(msg[:min(len(msg), maxGitHubMessage)], "")
}

// withRenovateToken appends the minted token after env so os/exec's
// last-duplicate-wins dedup lets it replace any forwarded RENOVATE_TOKEN.
func withRenovateToken(env []string, token string) []string {
	if env == nil {
		env = os.Environ()
	}
	return slices.Concat(env, []string{"RENOVATE_TOKEN=" + token})
}

// logAuthMode records at start which credential Renovate runs with. It omits
// the key file path, which a miswired setting can turn into a credential.
func logAuthMode(app *appConfig, runTimeout, effective time.Duration) {
	if app == nil {
		slog.Info("github auth mode", "mode", "token",
			"source", "RENOVATE_TOKEN or Renovate's own configuration")
		return
	}
	installation := "resolved at the first run"
	if app.installationID != 0 {
		installation = strconv.Itoa(app.installationID)
	}
	slog.Info("github auth mode", "mode", "app", "app_id", app.appID,
		"installation_id", installation)
	if effective < runTimeout {
		slog.Info("run timeout capped to the installation token's life",
			"run_timeout", runTimeout, "effective", effective)
	}
}

// appRunCeiling is the longest run a fresh installation token outlives. The
// token lasts installationTokenLife from before the mint request, and the run
// starts at most tokenMintBudget later.
const appRunCeiling = installationTokenLife - tokenMintBudget - tokenExpiryMargin

// runTimeoutFor caps a run in App mode at appRunCeiling: Renovate reads its
// token once at start, so a run cannot switch to a fresh one.
func runTimeoutFor(app *appConfig, runTimeout time.Duration) time.Duration {
	if app == nil {
		return runTimeout
	}
	return min(runTimeout, appRunCeiling)
}
