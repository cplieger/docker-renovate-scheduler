package main

import (
	"bytes"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cplieger/scheduler/v4"
	"github.com/cplieger/slogx/capture"
)

const (
	testAppID        = 4242
	testInstallation = 77
	testToken        = "ghs_test-installation-token"
	testTokenExpiry  = "2030-01-02T03:04:05Z"
)

var testAppKey = sync.OnceValue(func() *rsa.PrivateKey {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return key
})

// writeKeyFile writes testAppKey as PEM in GitHub's PKCS#1 form, or PKCS#8.
func writeKeyFile(t *testing.T, pkcs8 bool) string {
	t.Helper()
	block := &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(testAppKey())}
	if pkcs8 {
		der, err := x509.MarshalPKCS8PrivateKey(testAppKey())
		if err != nil {
			t.Fatalf("marshal PKCS#8: %v", err)
		}
		block = &pem.Block{Type: "PRIVATE KEY", Bytes: der}
	}
	path := filepath.Join(t.TempDir(), "app.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatalf("write key file: %v", err)
	}
	return path
}

// verifyJWT checks token's RS256 signature against the test key's public half
// and returns its header and claims.
func verifyJWT(t *testing.T, token string) (header, claims map[string]any) {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("JWT has %d parts, want 3", len(parts))
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("decode JWT signature: %v", err)
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(&testAppKey().PublicKey, crypto.SHA256, digest[:], sig); err != nil {
		t.Fatalf("JWT signature does not verify with the App's public key: %v", err)
	}
	for i, out := range []*map[string]any{&header, &claims} {
		raw, err := base64.RawURLEncoding.DecodeString(parts[i])
		if err != nil {
			t.Fatalf("decode JWT part %d: %v", i, err)
		}
		if err := json.Unmarshal(raw, out); err != nil {
			t.Fatalf("parse JWT part %d: %v", i, err)
		}
	}
	return header, claims
}

func TestSignAppJWT_ClaimsAndSignatureVerifyWithThePublicKey(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)

	token, err := signAppJWT(testAppKey(), testAppID, now)
	if err != nil {
		t.Fatalf("signAppJWT() error = %v", err)
	}

	header, claims := verifyJWT(t, token)
	if header["alg"] != "RS256" || header["typ"] != "JWT" {
		t.Errorf("JWT header = %v, want alg RS256 and typ JWT", header)
	}
	if claims["iss"] != "4242" {
		t.Errorf("JWT iss = %v, want the App ID \"4242\"", claims["iss"])
	}
	if got := claims["iat"]; got != float64(1_800_000_000-60) {
		t.Errorf("JWT iat = %v, want now-60s = %d", got, 1_800_000_000-60)
	}
	if got := claims["exp"]; got != float64(1_800_000_000+540) {
		t.Errorf("JWT exp = %v, want now+9m = %d (GitHub refuses more than 10m)", got, 1_800_000_000+540)
	}
}

// fakeGitHub serves handler and returns a minter aimed at it, with a short
// backoff so retry tests stay fast.
func fakeGitHub(t *testing.T, installationID int, handler http.HandlerFunc) *appTokenMinter {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	m := newAppTokenMinter(&appConfig{key: testAppKey(), appID: testAppID, installationID: installationID})
	m.apiBase = srv.URL
	m.client = srv.Client()
	m.baseDelay = 20 * time.Millisecond
	return m
}

func writeJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

func tokenBody() string {
	return `{"token":"` + testToken + `","expires_at":"` + testTokenExpiry + `","permissions":{"contents":"write"}}`
}

func TestMint_ExchangesASignedJWTForTheInstallationToken(t *testing.T) {
	var gotPath, gotMethod, gotAuth, gotAccept string
	m := fakeGitHub(t, testInstallation, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		gotAuth, gotAccept = r.Header.Get("Authorization"), r.Header.Get("Accept")
		writeJSON(w, http.StatusCreated, tokenBody())
	})

	tok, err := m.mint(t.Context())
	if err != nil {
		t.Fatalf("mint() error = %v", err)
	}

	if tok.value != testToken {
		t.Errorf("mint() token = %q, want %q", tok.value, testToken)
	}
	if want, _ := time.Parse(time.RFC3339, testTokenExpiry); !tok.expiresAt.Equal(want) {
		t.Errorf("mint() expiresAt = %v, want %v", tok.expiresAt, want)
	}
	if gotMethod != http.MethodPost || gotPath != "/app/installations/77/access_tokens" {
		t.Errorf("request = %s %s, want POST /app/installations/77/access_tokens", gotMethod, gotPath)
	}
	if gotAccept != "application/vnd.github+json" {
		t.Errorf("Accept = %q, want application/vnd.github+json", gotAccept)
	}
	jwt, ok := strings.CutPrefix(gotAuth, "Bearer ")
	if !ok {
		t.Fatalf("Authorization = %q, want a Bearer JWT", gotAuth)
	}
	if _, claims := verifyJWT(t, jwt); claims["iss"] != "4242" {
		t.Errorf("bearer JWT iss = %v, want \"4242\"", claims["iss"])
	}
}

func TestMint_FailsWithoutRetryOnClientErrors(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		body     string
		wantText []string
	}{
		{
			"401 bad credentials", http.StatusUnauthorized, `{"message":"A JSON web token could not be decoded"}`,
			[]string{"HTTP 401", "A JSON web token could not be decoded", "GITHUB_APP_ID"},
		},
		{
			"404 unknown installation", http.StatusNotFound, `{"message":"Not Found"}`,
			[]string{"HTTP 404", "Not Found", "GITHUB_APP_INSTALLATION_ID"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls atomic.Int32
			m := fakeGitHub(t, testInstallation, func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				writeJSON(w, tt.status, tt.body)
			})

			_, err := m.mint(t.Context())
			if err == nil {
				t.Fatalf("mint() error = nil, want an HTTP %d failure", tt.status)
			}
			for _, want := range tt.wantText {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("mint() error = %q, want it to contain %q", err, want)
				}
			}
			if got := calls.Load(); got != 1 {
				t.Errorf("requests = %d, want 1 (a client error is not retried)", got)
			}
		})
	}
}

func TestMint_RetriesServerErrorsWithBackoff(t *testing.T) {
	var mu sync.Mutex
	var seen []time.Time
	m := fakeGitHub(t, testInstallation, func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		seen = append(seen, time.Now())
		n := len(seen)
		mu.Unlock()
		if n <= 2 {
			writeJSON(w, http.StatusBadGateway, `{"message":"Server Error"}`)
			return
		}
		writeJSON(w, http.StatusCreated, tokenBody())
	})

	tok, err := m.mint(t.Context())
	if err != nil {
		t.Fatalf("mint() error = %v, want success on the third attempt", err)
	}
	if tok.value != testToken {
		t.Errorf("mint() token = %q, want %q", tok.value, testToken)
	}
	if len(seen) != 3 {
		t.Fatalf("requests = %d, want 3 (two 502s, then 201)", len(seen))
	}
	// Equal jitter waits at least half the delay: 10ms, then 20ms.
	if gap := seen[1].Sub(seen[0]); gap < 10*time.Millisecond {
		t.Errorf("first retry came %v after the failure, want a backoff of at least 10ms", gap)
	}
	if gap := seen[2].Sub(seen[1]); gap < 20*time.Millisecond {
		t.Errorf("second retry came %v after the failure, want a doubled backoff of at least 20ms", gap)
	}
}

func TestMint_GivesUpAfterPersistentServerErrors(t *testing.T) {
	var calls atomic.Int32
	m := fakeGitHub(t, testInstallation, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		writeJSON(w, http.StatusServiceUnavailable, `{"message":"unavailable"}`)
	})
	m.baseDelay = time.Millisecond

	_, err := m.mint(t.Context())
	if err == nil || !strings.Contains(err.Error(), "HTTP 503") {
		t.Fatalf("mint() error = %v, want the final HTTP 503", err)
	}
	if got := calls.Load(); got != 4 {
		t.Errorf("requests = %d, want 4 attempts", got)
	}
}

func TestMint_RejectsAMalformedBodyWithoutEchoingIt(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"truncated JSON", `{"token":"` + testToken},
		{"no token field", `{"expires_at":"` + testTokenExpiry + `"}`},
		{"no expiry", `{"token":"` + testToken + `"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := fakeGitHub(t, testInstallation, func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, http.StatusCreated, tt.body)
			})

			_, err := m.mint(t.Context())
			if err == nil {
				t.Fatalf("mint() error = nil for body %q, want a failure", tt.body)
			}
			if strings.Contains(err.Error(), testToken) {
				t.Errorf("mint() error = %q, which carries the token from the body", err)
			}
		})
	}
}

func TestMint_ResolvesTheSingleInstallationWhenNoIDIsSet(t *testing.T) {
	var posted string
	m := fakeGitHub(t, 0, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/app/installations" {
			writeJSON(w, http.StatusOK, `[{"id":91,"account":{"login":"octo"}}]`)
			return
		}
		posted = r.URL.Path
		writeJSON(w, http.StatusCreated, tokenBody())
	})

	if _, err := m.mint(t.Context()); err != nil {
		t.Fatalf("mint() error = %v", err)
	}
	if posted != "/app/installations/91/access_tokens" {
		t.Errorf("token request path = %q, want the resolved installation 91", posted)
	}
}

func TestMint_RefusesToGuessBetweenSeveralInstallations(t *testing.T) {
	m := fakeGitHub(t, 0, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, `[{"id":1,"account":{"login":"a"}},{"id":2,"account":{"login":"b"}}]`)
	})

	_, err := m.mint(t.Context())
	if err == nil || !strings.Contains(err.Error(), "a=1, b=2") || !strings.Contains(err.Error(), envAppInstallationID) {
		t.Fatalf("mint() error = %v, want one naming both installations and %s", err, envAppInstallationID)
	}
}

func TestLoadAppConfig(t *testing.T) {
	tests := []struct {
		env      map[string]string
		name     string
		wantErr  string
		wantApp  bool
		keyPKCS8 bool
	}{
		{name: "nothing set keeps token mode", env: map[string]string{}},
		{name: "PKCS#1 key", env: map[string]string{envAppID: "4242", envAppInstallationID: "77", envAppKeyFile: "@key"}, wantApp: true},
		{name: "PKCS#8 key", env: map[string]string{envAppID: "4242", envAppKeyFile: "@key"}, wantApp: true, keyPKCS8: true},
		{name: "App ID without key file", env: map[string]string{envAppID: "4242"}, wantErr: envAppKeyFile + " is required"},
		{name: "key file without App ID", env: map[string]string{envAppKeyFile: "@key"}, wantErr: envAppID + " is required"},
		{name: "installation ID alone", env: map[string]string{envAppInstallationID: "77"}, wantErr: envAppID + " is required"},
		{name: "non-numeric App ID", env: map[string]string{envAppID: "my-app", envAppKeyFile: "@key"}, wantErr: envAppID},
		{name: "zero App ID", env: map[string]string{envAppID: "0", envAppKeyFile: "@key"}, wantErr: "positive integer"},
		{name: "negative installation ID", env: map[string]string{envAppID: "1", envAppInstallationID: "-3", envAppKeyFile: "@key"}, wantErr: "positive integer"},
		{name: "inline key is refused", env: map[string]string{envAppKeyInline: "-----BEGIN RSA PRIVATE KEY-----"}, wantErr: "put the private key in a file"}, // gitleaks:allow (PEM header only, no key)
		{name: "missing key file", env: map[string]string{envAppID: "1", envAppKeyFile: "/nonexistent/app.pem"}, wantErr: "no such file"},
		{name: "key file that is not PEM", env: map[string]string{envAppID: "1", envAppKeyFile: "@garbage"}, wantErr: "no PEM block"},
		{name: "PEM pasted as the key file path", env: map[string]string{envAppID: "1", envAppKeyFile: "-----BEGIN RSA PRIVATE KEY-----\nAAAA\n"}, wantErr: "holds key material"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			garbage := filepath.Join(t.TempDir(), "garbage.pem")
			if err := os.WriteFile(garbage, []byte("not a key"), 0o600); err != nil {
				t.Fatalf("write garbage key: %v", err)
			}
			for _, k := range []string{envAppID, envAppInstallationID, envAppKeyFile, envAppKeyInline} {
				t.Setenv(k, "")
			}
			for k, v := range tt.env {
				switch v {
				case "@key":
					v = writeKeyFile(t, tt.keyPKCS8)
				case "@garbage":
					v = garbage
				}
				t.Setenv(k, v)
			}

			app, err := loadAppConfig()

			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("loadAppConfig() error = %v, want one containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("loadAppConfig() error = %v", err)
			}
			if (app != nil) != tt.wantApp {
				t.Fatalf("loadAppConfig() app = %v, want app mode %v", app, tt.wantApp)
			}
			if app != nil && !app.key.Equal(testAppKey()) {
				t.Error("loadAppConfig() parsed a different key from the one written")
			}
		})
	}
}

func TestReadAppKey_ErrorNeverEchoesTheFileContent(t *testing.T) {
	tests := []struct {
		name    string
		content string
		secret  string
	}{
		{
			name:    "corrupt key body",
			content: "-----BEGIN RSA PRIVATE KEY-----\nc2VjcmV0LWtleS1ieXRlcw==\n-----END RSA PRIVATE KEY-----\n", // gitleaks:allow (fake key, error-text test fixture)
			secret:  "c2VjcmV0",
		},
		{
			name:    "secret as the PEM label",
			content: "-----BEGIN ghp_misplaced-secret-value-----\nAAAA\n-----END ghp_misplaced-secret-value-----\n",
			secret:  "ghp_misplaced-secret-value",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "app.pem")
			if err := os.WriteFile(path, []byte(tt.content), 0o600); err != nil {
				t.Fatalf("write key file: %v", err)
			}

			_, err := readAppKey(path)
			if err == nil {
				t.Fatal("readAppKey() error = nil for an unusable key file, want a failure")
			}
			if strings.Contains(err.Error(), tt.secret) {
				t.Errorf("readAppKey() error = %q, which echoes file content", err)
			}
		})
	}
}

// bootAppDaemon starts the daemon in App mode with a valid key at keyPath,
// waits for its start line, stops it, and returns everything it logged. Not
// parallel: it swaps the global slog default and uses healthMarkerPath.
func bootAppDaemon(t *testing.T, keyPath string) string {
	t.Helper()
	block := &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(testAppKey())}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatalf("write key file: %v", err)
	}
	t.Setenv("RENOVATE_BASE_DIR", t.TempDir())
	t.Setenv("RUN_INTERVAL", "off")
	t.Setenv("RUN_TIMEOUT", "1h")
	t.Setenv(envAppID, "4242")
	t.Setenv(envAppInstallationID, "77")
	t.Setenv(envAppKeyFile, keyPath)
	t.Setenv(envAppKeyInline, "")
	t.Cleanup(func() { _ = os.Remove(healthMarkerPath) })
	saveLogGlobals(t)
	var buf bytes.Buffer
	var mu sync.Mutex
	slog.SetDefault(slog.New(slog.NewTextHandler(&lockedWriter{w: &buf, mu: &mu}, &slog.HandlerOptions{Level: slog.LevelDebug})))
	logged := func() string {
		mu.Lock()
		defer mu.Unlock()
		return buf.String()
	}

	cancel, done, runErr := startDaemonForTest(t, recordingRunner("true", nil))
	waitFor(t, 5*time.Second, func() bool { return strings.Contains(logged(), `msg="container started"`) },
		"the App-mode daemon never logged its start line")
	cancel()
	awaitDaemonStopped(t, done)
	if err := *runErr; err != nil {
		t.Fatalf("runDaemon() = %v, want nil", err)
	}
	return logged()
}

func TestRunDaemon_AppModeStartNeverLogsTheKeyPath(t *testing.T) {
	const tokenShaped = "ghp_misplaced-secret-value"

	logged := bootAppDaemon(t, filepath.Join(t.TempDir(), tokenShaped))

	if !strings.Contains(logged, `msg="github auth mode" mode=app`) {
		t.Fatalf("log lacks the App auth-mode line, so the test did not reach it:\n%s", logged)
	}
	if strings.Contains(logged, tokenShaped) {
		t.Errorf("log quotes the key file path, which here holds a credential:\n%s", logged)
	}
}

func TestRunDaemon_AppModeCapsTheRunTimeout(t *testing.T) {
	logged := bootAppDaemon(t, filepath.Join(t.TempDir(), "app.pem"))

	for line := range strings.Lines(logged) {
		if strings.Contains(line, `msg="container started"`) {
			if !strings.Contains(line, " timeout=56m0s ") {
				t.Errorf("start line = %q, want timeout=56m0s for RUN_TIMEOUT=1h in App mode", line)
			}
			return
		}
	}
	t.Fatalf("log lacks the start line:\n%s", logged)
}

// TestRunDaemon_UnreadableKeyFileFailsAtStart pins that a broken App setting
// stops the daemon before it binds the trigger socket. Not parallel: it
// swaps the global slog default.
func TestRunDaemon_UnreadableKeyFileFailsAtStart(t *testing.T) {
	t.Setenv("RENOVATE_BASE_DIR", t.TempDir())
	t.Setenv("RUN_INTERVAL", "off")
	t.Setenv(envAppID, "4242")
	t.Setenv(envAppKeyFile, filepath.Join(t.TempDir(), "missing.pem"))
	rec := capture.Default(t)
	sock := testSocketPath(t)

	err := runDaemon(t.Context(), sock, recordingRunner("true", nil))

	if err == nil {
		t.Fatal("runDaemon() error = nil, want a start failure for the unreadable key file")
	}
	if got := rec.CountLevel(slog.LevelError, "github app configuration invalid"); got != 1 {
		t.Errorf("ERROR 'github app configuration invalid' lines = %d, want 1", got)
	}
	if _, statErr := os.Stat(sock); statErr == nil {
		t.Error("trigger socket was bound, want the daemon to stop before serving")
	}
}

// TestRunDaemon_MisplacedSecretNeverReachesTheLog puts a credential where a
// number or a path belongs and requires that neither the start error nor any
// rendered log line quotes it. Not parallel: it swaps the global slog default.
func TestRunDaemon_MisplacedSecretNeverReachesTheLog(t *testing.T) {
	const tokenShaped = "ghp_misplaced-secret-value"
	pemShaped := "-----BEGIN RSA PRIVATE KEY-----\nbWlzcGxhY2VkLWtleQ==\n-----END RSA PRIVATE KEY-----\n"
	tests := []struct {
		env    map[string]string
		name   string
		secret string
	}{
		{
			name: "token as App ID", secret: tokenShaped,
			env: map[string]string{envAppID: tokenShaped, envAppKeyFile: "@key"},
		},
		{
			name: "token as installation ID", secret: tokenShaped,
			env: map[string]string{envAppID: "4242", envAppInstallationID: tokenShaped, envAppKeyFile: "@key"},
		},
		{
			name: "PEM as key file path", secret: "bWlzcGxhY2VkLWtleQ", // gitleaks:allow (fake key, redaction test fixture)
			env: map[string]string{envAppID: "4242", envAppKeyFile: pemShaped},
		},
		{
			name: "token as key file path", secret: tokenShaped,
			env: map[string]string{envAppID: "4242", envAppKeyFile: tokenShaped},
		},
		{
			name: "token as the key file's PEM label", secret: tokenShaped,
			env: map[string]string{envAppID: "4242", envAppKeyFile: "@label"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("RENOVATE_BASE_DIR", t.TempDir())
			t.Setenv("RUN_INTERVAL", "off")
			for _, k := range []string{envAppID, envAppInstallationID, envAppKeyFile, envAppKeyInline} {
				t.Setenv(k, "")
			}
			for k, v := range tt.env {
				switch v {
				case "@key":
					v = writeKeyFile(t, false)
				case "@label":
					v = filepath.Join(t.TempDir(), "app.pem")
					label := "-----BEGIN " + tokenShaped + "-----\nAAAA\n-----END " + tokenShaped + "-----\n"
					if err := os.WriteFile(v, []byte(label), 0o600); err != nil {
						t.Fatalf("write key file: %v", err)
					}
				}
				t.Setenv(k, v)
			}
			saveLogGlobals(t)
			var buf bytes.Buffer
			slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))

			err := runDaemon(t.Context(), testSocketPath(t), recordingRunner("true", nil))

			if err == nil {
				t.Fatal("runDaemon() error = nil, want a start failure")
			}
			if strings.Contains(err.Error(), tt.secret) {
				t.Errorf("runDaemon() error = %q, which quotes the misplaced secret", err)
			}
			if logged := buf.String(); strings.Contains(logged, tt.secret) {
				t.Errorf("log quotes the misplaced secret:\n%s", logged)
			}
		})
	}
}

func TestRunTimeoutFor_CapsAppRunsBeforeTheTokenExpires(t *testing.T) {
	app := &appConfig{appID: testAppID}
	tests := []struct {
		app  *appConfig
		name string
		in   time.Duration
		want time.Duration
	}{
		{name: "token mode keeps a long timeout", in: 2 * time.Hour, want: 2 * time.Hour},
		{name: "app mode caps the default hour", app: app, in: time.Hour, want: 56 * time.Minute},
		{name: "app mode keeps a shorter timeout", app: app, in: 30 * time.Minute, want: 30 * time.Minute},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := runTimeoutFor(tt.app, tt.in); got != tt.want {
				t.Errorf("runTimeoutFor(app=%v, %s) = %s, want %s", tt.app != nil, tt.in, got, tt.want)
			}
		})
	}
}

// TestLogAuthMode_ReportsTheRunTimeoutCap is not parallel: it swaps the global
// slog default.
func TestLogAuthMode_ReportsTheRunTimeoutCap(t *testing.T) {
	const capped = "run timeout capped to the installation token's life"
	app := &appConfig{appID: testAppID}
	tests := []struct {
		name       string
		runTimeout time.Duration
		want       int
	}{
		{name: "an hour is capped", runTimeout: time.Hour, want: 1},
		{name: "half an hour is not", runTimeout: 30 * time.Minute, want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := capture.Default(t)

			logAuthMode(app, tt.runTimeout, runTimeoutFor(app, tt.runTimeout))

			if got := rec.CountLevel(slog.LevelInfo, capped); got != tt.want {
				t.Errorf("logAuthMode(RUN_TIMEOUT=%s) logged %q %d times, want %d", tt.runTimeout, capped, got, tt.want)
			}
		})
	}
}

// appModeDaemon returns a daemon whose minter is aimed at handler.
func appModeDaemon(t *testing.T, runner scheduler.CommandRunner, handler http.HandlerFunc) *daemon {
	t.Helper()
	d, _ := newBareDaemon(t, runner)
	d.tokens = fakeGitHub(t, testInstallation, handler)
	return d
}

func TestExecute_AppModeRunsRenovateWithTheMintedToken(t *testing.T) {
	t.Setenv("RENOVATE_BASE_DIR", t.TempDir())
	d := appModeDaemon(t, shellAssertRunner(`[ "$RENOVATE_TOKEN" = "`+testToken+`" ]`),
		func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, http.StatusCreated, tokenBody()) })

	j := newJob("external", nil, []string{"RENOVATE_TOKEN=pat-from-the-exec", "PATH=" + os.Getenv("PATH")})
	d.execute(t.Context(), t.Context().Err, j)

	if out := <-j.Result(); !out.OK {
		t.Errorf("run outcome ok = false (reason %q), want the child to see the minted token over the forwarded PAT", out.Reason)
	}
}

// TestExecute_AppModeWithoutAForwardedEnvKeepsTheDaemonEnvironment covers the
// built-in schedule, whose jobs carry a nil env.
func TestExecute_AppModeWithoutAForwardedEnvKeepsTheDaemonEnvironment(t *testing.T) {
	t.Setenv("RENOVATE_BASE_DIR", t.TempDir())
	t.Setenv("RENOVATE_TEST_AMBIENT", "from-the-daemon")
	d := appModeDaemon(t, shellAssertRunner(
		`[ "$RENOVATE_TOKEN" = "`+testToken+`" ] && [ "$RENOVATE_TEST_AMBIENT" = "from-the-daemon" ]`,
	),
		func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, http.StatusCreated, tokenBody()) })

	j := newJob("interval", nil, nil)
	d.execute(t.Context(), t.Context().Err, j)

	if out := <-j.Result(); !out.OK {
		t.Errorf("run outcome ok = false (reason %q), want the child to see the minted token and the daemon's environment", out.Reason)
	}
}

func TestExecute_TokenModeLeavesTheForwardedTokenAlone(t *testing.T) {
	t.Setenv("RENOVATE_BASE_DIR", t.TempDir())
	d, _ := newBareDaemon(t, shellAssertRunner(`[ "$RENOVATE_TOKEN" = "pat-from-the-exec" ]`))

	j := newJob("external", nil, []string{"RENOVATE_TOKEN=pat-from-the-exec", "PATH=" + os.Getenv("PATH")})
	d.execute(t.Context(), t.Context().Err, j)

	if out := <-j.Result(); !out.OK {
		t.Errorf("run outcome ok = false (reason %q), want the child to see the forwarded PAT unchanged", out.Reason)
	}
}

func TestExecute_TokenFailureFailsTheRunWithoutStartingRenovate(t *testing.T) {
	t.Setenv("RENOVATE_BASE_DIR", t.TempDir())
	var started [][]string
	d := appModeDaemon(t, recordingRunner("true", &started),
		func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusUnauthorized, `{"message":"Bad credentials"}`)
		})
	d.health.Set(true)

	j := newJob("interval", nil, nil)
	d.execute(t.Context(), t.Context().Err, j)

	out := <-j.Result()
	if out.OK || out.Reason != mintFailedReason {
		t.Errorf("outcome = ok %v reason %q, want a failure with reason %q", out.OK, out.Reason, mintFailedReason)
	}
	if len(started) != 0 {
		t.Errorf("renovate started %d times, want 0 without a token", len(started))
	}
	if d.marker.Healthy() {
		t.Error("health marker healthy after a token failure, want unhealthy")
	}
}

// TestExecute_TokenNeverReachesTheLog renders every scheduler line at debug,
// through a retried mint, a successful run and a failed one, and requires
// neither the token nor the JWT in the output. Not parallel: it swaps the
// global slog default.
func TestExecute_TokenNeverReachesTheLog(t *testing.T) {
	t.Setenv("RENOVATE_BASE_DIR", t.TempDir())
	saveLogGlobals(t)
	var buf bytes.Buffer
	var mu sync.Mutex
	slog.SetDefault(slog.New(slog.NewTextHandler(&lockedWriter{w: &buf, mu: &mu}, &slog.HandlerOptions{Level: slog.LevelDebug})))

	var calls atomic.Int32
	var jwts []string
	d := appModeDaemon(t, recordingRunner("false", nil), func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		jwts = append(jwts, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		mu.Unlock()
		switch calls.Add(1) {
		case 1:
			writeJSON(w, http.StatusBadGateway, `{"message":"Server Error"}`)
		case 2:
			writeJSON(w, http.StatusCreated, tokenBody())
		default:
			writeJSON(w, http.StatusCreated, `{"token":"`+testToken)
		}
	})

	for range 2 {
		j := newJob("interval", []string{"owner/repo"}, nil)
		d.execute(t.Context(), t.Context().Err, j)
		<-j.Result()
	}

	mu.Lock()
	defer mu.Unlock()
	logged := buf.String()
	if !strings.Contains(logged, "github app installation token issued") {
		t.Fatalf("log lacks the issued line, so the test did not reach a mint:\n%s", logged)
	}
	if strings.Contains(logged, testToken) {
		t.Errorf("log carries the installation token:\n%s", logged)
	}
	for _, jwt := range jwts {
		if jwt != "" && strings.Contains(logged, jwt) {
			t.Errorf("log carries the App JWT:\n%s", logged)
		}
	}
}

// TestExecute_AReflectedJWTNeverReachesTheLog uses a server that echoes the
// bearer JWT in its error message. Not parallel: it swaps the global slog
// default.
func TestExecute_AReflectedJWTNeverReachesTheLog(t *testing.T) {
	tests := []struct {
		name   string
		status int
	}{
		{name: "permanent 401", status: http.StatusUnauthorized},
		{name: "retried 502", status: http.StatusBadGateway},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("RENOVATE_BASE_DIR", t.TempDir())
			saveLogGlobals(t)
			var buf bytes.Buffer
			var mu sync.Mutex
			slog.SetDefault(slog.New(slog.NewTextHandler(&lockedWriter{w: &buf, mu: &mu}, &slog.HandlerOptions{Level: slog.LevelDebug})))
			var jwts []string
			d := appModeDaemon(t, recordingRunner("true", nil), func(w http.ResponseWriter, r *http.Request) {
				jwt := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
				mu.Lock()
				jwts = append(jwts, jwt)
				mu.Unlock()
				writeJSON(w, tt.status, `{"message":"rejected `+jwt+`"}`)
			})
			d.tokens.baseDelay = time.Millisecond

			j := newJob("interval", nil, nil)
			d.execute(t.Context(), t.Context().Err, j)
			<-j.Result()

			mu.Lock()
			defer mu.Unlock()
			logged := buf.String()
			if !strings.Contains(logged, "rejected REDACTED") {
				t.Fatalf("log lacks the redacted server message, so the test did not reach it:\n%s", logged)
			}
			for _, jwt := range jwts {
				// The 200-byte cap alone would keep the header, the claims and
				// part of the signature, so look for a signature fragment.
				sig := jwt[strings.LastIndex(jwt, ".")+1:]
				if strings.Contains(logged, sig[:16]) {
					t.Errorf("log carries the reflected App JWT:\n%s", logged)
				}
			}
		})
	}
}

type lockedWriter struct {
	w  *bytes.Buffer
	mu *sync.Mutex
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}
