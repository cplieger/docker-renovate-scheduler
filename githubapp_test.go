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
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/cplieger/forgeapi/v2"
	"github.com/cplieger/forgeapi/v2/creds"
	"github.com/cplieger/go/measure"
	"github.com/cplieger/go/scheduler"
	"github.com/cplieger/go/scheduler/trigger"
	"github.com/cplieger/go/slogx/capture"
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

// verifyJWT checks that token's header names RS256 and that its signature
// verifies against the test key's public half, and returns its claims.
func verifyJWT(t *testing.T, token string) (claims map[string]any) {
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
	var header map[string]any
	for i, out := range []*map[string]any{&header, &claims} {
		raw, err := base64.RawURLEncoding.DecodeString(parts[i])
		if err != nil {
			t.Fatalf("decode JWT part %d: %v", i, err)
		}
		if err := json.Unmarshal(raw, out); err != nil {
			t.Fatalf("parse JWT part %d: %v", i, err)
		}
	}
	if header["alg"] != "RS256" {
		t.Fatalf("JWT header alg = %v, want RS256", header["alg"])
	}
	return claims
}

// handlerTransport answers the App source's requests in process with a
// handler, so a test reads what the source sent and decides GitHub's answer.
type handlerTransport http.HandlerFunc

func (h handlerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	h(rec, r)
	resp := rec.Result()
	resp.Request = r
	return resp, nil
}

// fakeGitHub returns the scheduler's App source for installationID, built as
// production builds it, answered by handler; opts follow the scheduler's.
func fakeGitHub(t *testing.T, installationID int64, handler http.HandlerFunc, opts ...forgeapi.Option) *creds.AppSource {
	t.Helper()
	key, err := creds.NewAppKey(strconv.Itoa(testAppID), testAppKey())
	if err != nil {
		t.Fatalf("NewAppKey: %v", err)
	}
	src, err := newAppSource(&appConfig{key: key, appID: testAppID, installationID: installationID},
		slices.Concat([]forgeapi.Option{forgeapi.WithWireTransport(handlerTransport(handler))}, opts)...)
	if err != nil {
		t.Fatalf("newAppSource: %v", err)
	}
	return src
}

func writeJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

func tokenBody() string {
	return tokenBodyExpiring(testTokenExpiry)
}

func tokenBodyExpiring(expiry string) string {
	return `{"token":"` + testToken + `","expires_at":"` + expiry + `","permissions":{"contents":"write"}}`
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
		{name: "key file that is not PEM", env: map[string]string{envAppID: "1", envAppKeyFile: "@garbage"}, wantErr: envAppKeyFile + ": " + forgeapi.CodeAppKeyInvalid},
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
			if app == nil {
				return
			}
			jwt, err := app.key.JWT(t.Context())
			if err != nil {
				t.Fatalf("app key JWT() error = %v", err)
			}
			if claims := verifyJWT(t, jwt); claims["iss"] != "4242" {
				t.Errorf("app key JWT iss = %v, want %s's value \"4242\"", claims["iss"], envAppID)
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

			_, err := readAppKey(path, testAppID)
			if err == nil {
				t.Fatal("readAppKey() error = nil for an unusable key file, want a failure")
			}
			if strings.Contains(err.Error(), tt.secret) {
				t.Errorf("readAppKey() error = %q, which echoes file content", err)
			}
		})
	}
}

// TestRunDaemon_AppModeStartNeverLogsTheKeyPath boots the daemon in App mode
// with a key file whose path holds a token-shaped string. Not parallel: it
// swaps the global slog default and uses healthMarkerPath.
func TestRunDaemon_AppModeStartNeverLogsTheKeyPath(t *testing.T) {
	const tokenShaped = "ghp_misplaced-secret-value"
	keyPath := filepath.Join(t.TempDir(), tokenShaped)
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
	var buf bytes.Buffer
	var mu sync.Mutex
	capture.SwapDefault(t, slog.New(slog.NewTextHandler(&lockedWriter{w: &buf, mu: &mu}, &slog.HandlerOptions{Level: slog.LevelDebug})))
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

	if !strings.Contains(logged(), `msg="github auth mode" mode=app`) {
		t.Fatalf("log lacks the App auth-mode line, so the test did not reach it:\n%s", logged())
	}
	if strings.Contains(logged(), tokenShaped) {
		t.Errorf("log quotes the key file path, which here holds a credential:\n%s", logged())
	}
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
			var buf bytes.Buffer
			capture.SwapDefault(t, slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))

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

func TestRunTimeoutFor_StopsTheRunBeforeTheTokenExpires(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		runTimeout time.Duration
		life       time.Duration
		want       time.Duration
		wantErr    bool
	}{
		{name: "an hour's token caps a two-hour timeout 2m before it expires", runTimeout: 2 * time.Hour, life: time.Hour, want: 58 * time.Minute},
		{name: "a shorter timeout stays", runTimeout: 30 * time.Minute, life: time.Hour, want: 30 * time.Minute},
		{name: "a token expiring inside the margin leaves no run", runTimeout: time.Hour, life: 90 * time.Second, wantErr: true},
		{name: "a token leaving under a second past the margin leaves no run", runTimeout: time.Hour, life: tokenExpiryMargin + 500*time.Millisecond, wantErr: true},
		{name: "a token leaving a second past the margin leaves a one-second run", runTimeout: time.Hour, life: tokenExpiryMargin + time.Second, want: time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			// The bubble's clock stands still, so the expiry is exactly tt.life away.
			synctest.Test(t, func(t *testing.T) {
				got, err := runTimeoutFor(tt.runTimeout, time.Now().Add(tt.life))
				if (err != nil) != tt.wantErr {
					t.Fatalf("runTimeoutFor(%s, now+%s) error = %v, want error %v", tt.runTimeout, tt.life, err, tt.wantErr)
				}
				if err != nil && !strings.Contains(err.Error(), "under 2m1s from now") {
					t.Errorf("runTimeoutFor(%s, now+%s) error = %q, want it to name the refusal's bound, 2m1s", tt.runTimeout, tt.life, err)
				}
				if got != tt.want {
					t.Errorf("runTimeoutFor(%s, now+%s) = %s, want %s", tt.runTimeout, tt.life, got, tt.want)
				}
			})
		})
	}
}

// appModeDaemon returns a daemon whose App source is answered by handler.
func appModeDaemon(t *testing.T, runner scheduler.CommandRunner, handler http.HandlerFunc, opts ...forgeapi.Option) *daemon {
	t.Helper()
	d, _ := newBareDaemon(t, runner)
	d.tokens = fakeGitHub(t, testInstallation, handler, opts...)
	d.installationID = testInstallation
	return d
}

// TestExecute_CapsTheRunTimeoutAtTheMintedTokensExpiry is not parallel: it
// swaps the global slog default.
func TestExecute_CapsTheRunTimeoutAtTheMintedTokensExpiry(t *testing.T) {
	const capped = "run timeout capped to the installation token's life"
	tests := []struct {
		name       string
		runTimeout time.Duration
		life       time.Duration
		want       time.Duration
		wantCapped int
	}{
		{name: "an hour outlasts a 30-minute token", runTimeout: time.Hour, life: 30 * time.Minute, want: 28 * time.Minute, wantCapped: 1},
		{name: "ten minutes fits inside it", runTimeout: 10 * time.Minute, life: 30 * time.Minute, want: 10 * time.Minute, wantCapped: 0},
		// A host clock 5 minutes behind GitHub's reads a fresh token's hour as 65 minutes.
		{name: "a token stating 65 minutes is held to GitHub's hour on the host's clock", runTimeout: 2 * time.Hour, life: 65 * time.Minute, want: 58 * time.Minute, wantCapped: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("RENOVATE_BASE_DIR", t.TempDir())
			rec := capture.Default(t)
			expiry := time.Now().Add(tt.life).UTC().Truncate(time.Second)
			d := appModeDaemon(t, recordingRunner("true", nil), func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, http.StatusCreated, tokenBodyExpiring(expiry.Format(time.RFC3339)))
			})
			d.timeout = tt.runTimeout
			var got time.Duration
			d.runOnce = func(_ context.Context, timeout time.Duration, _ string, _ runPayload, _ scheduler.CommandRunner) runResult {
				got = timeout
				return runResult{outcome: runComplete}
			}

			j := newJob("interval", nil, nil)
			d.execute(t.Context(), t.Context().Done(), j)
			<-j.Result()

			if got > tt.want || got < tt.want-5*time.Second {
				t.Errorf("run timeout = %s, want %s: the shortest of RUN_TIMEOUT, the token's expiry and an hour from the mint, less %s", got, tt.want, tokenExpiryMargin)
			}
			if n := rec.CountLevel(slog.LevelInfo, capped); n != tt.wantCapped {
				t.Errorf("%q logged %d times, want %d", capped, n, tt.wantCapped)
			}
		})
	}
}

func TestExecute_AppModeMintsWithTheAppIDAndTheFullGrant(t *testing.T) {
	t.Setenv("RENOVATE_BASE_DIR", t.TempDir())
	var gotMethod, gotPath, gotAuth string
	var gotBody []byte
	d := appModeDaemon(t, recordingRunner("true", nil), func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotAuth = r.Method, r.URL.Path, r.Header.Get("Authorization")
		if r.Body != nil {
			gotBody, _ = io.ReadAll(r.Body)
		}
		writeJSON(w, http.StatusCreated, tokenBody())
	})

	j := newJob("interval", nil, nil)
	d.execute(t.Context(), t.Context().Done(), j)
	if out := <-j.Result(); !out.OK {
		t.Fatalf("run outcome ok = false (reason %q), want a minted token and a run", out.Reason)
	}

	if gotMethod != http.MethodPost || gotPath != "/app/installations/77/access_tokens" {
		t.Errorf("request = %s %s, want POST /app/installations/77/access_tokens", gotMethod, gotPath)
	}
	jwt, ok := strings.CutPrefix(gotAuth, "Bearer ")
	if !ok {
		t.Fatalf("Authorization = %q, want a Bearer JWT", gotAuth)
	}
	if claims := verifyJWT(t, jwt); claims["iss"] != "4242" {
		t.Errorf("bearer JWT iss = %v, want %s's value \"4242\"", claims["iss"], envAppID)
	}
	var body map[string]any
	if len(bytes.TrimSpace(gotBody)) > 0 {
		if err := json.Unmarshal(gotBody, &body); err != nil {
			t.Fatalf("token request body %q is not JSON: %v", gotBody, err)
		}
	}
	for _, field := range []string{"permissions", "repositories", "repository_ids"} {
		if _, narrowed := body[field]; narrowed {
			t.Errorf("token request body carries %q, want the installation's full grant", field)
		}
	}
}

func TestExecute_AppModeRunsRenovateWithTheMintedToken(t *testing.T) {
	t.Setenv("RENOVATE_BASE_DIR", t.TempDir())
	d := appModeDaemon(t, shellAssertRunner(`[ "$RENOVATE_TOKEN" = "`+testToken+`" ]`),
		func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, http.StatusCreated, tokenBody()) })

	j := newJob("external", nil, []string{"RENOVATE_TOKEN=pat-from-the-exec", "PATH=" + os.Getenv("PATH")})
	d.execute(t.Context(), t.Context().Done(), j)

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
	d.execute(t.Context(), t.Context().Done(), j)

	if out := <-j.Result(); !out.OK {
		t.Errorf("run outcome ok = false (reason %q), want the child to see the minted token and the daemon's environment", out.Reason)
	}
}

func TestExecute_TokenModeLeavesTheForwardedTokenAlone(t *testing.T) {
	t.Setenv("RENOVATE_BASE_DIR", t.TempDir())
	d, _ := newBareDaemon(t, shellAssertRunner(`[ "$RENOVATE_TOKEN" = "pat-from-the-exec" ]`))

	j := newJob("external", nil, []string{"RENOVATE_TOKEN=pat-from-the-exec", "PATH=" + os.Getenv("PATH")})
	d.execute(t.Context(), t.Context().Done(), j)

	if out := <-j.Result(); !out.OK {
		t.Errorf("run outcome ok = false (reason %q), want the child to see the forwarded PAT unchanged", out.Reason)
	}
}

// TestExecute_ResolvesTheSingleInstallationWhenNoIDIsSet is not parallel: it
// swaps the global slog default.
func TestExecute_ResolvesTheSingleInstallationWhenNoIDIsSet(t *testing.T) {
	t.Setenv("RENOVATE_BASE_DIR", t.TempDir())
	rec := capture.Default(t)
	var posted []string
	d, _ := newBareDaemon(t, recordingRunner("true", nil))
	d.tokens = fakeGitHub(t, 0, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/app/installations" {
			writeJSON(w, http.StatusOK, `[{"id":91,"account":{"login":"octo"}}]`)
			return
		}
		posted = append(posted, r.URL.Path)
		writeJSON(w, http.StatusCreated, tokenBody())
	})

	for range 2 {
		j := newJob("interval", nil, nil)
		d.execute(t.Context(), t.Context().Done(), j)
		if out := <-j.Result(); !out.OK {
			t.Fatalf("run outcome ok = false (reason %q), want the resolved installation's token", out.Reason)
		}
	}

	if want := []string{"/app/installations/91/access_tokens", "/app/installations/91/access_tokens"}; !slices.Equal(posted, want) {
		t.Errorf("token requests = %q, want %q", posted, want)
	}
	if got := rec.CountExact("github app installation resolved"); got != 1 {
		t.Errorf("'github app installation resolved' lines = %d, want 1 for one lookup", got)
	}
}

// TestExecute_AReinstallAfterASuspensionLogsTheNewInstallation covers a
// suspended installation the owner replaces with a reinstall: the first run
// after the re-probe wait mints for the new installation, and its lines name
// that one. Not parallel: it swaps the global slog default.
func TestExecute_AReinstallAfterASuspensionLogsTheNewInstallation(t *testing.T) {
	t.Setenv("RENOVATE_BASE_DIR", t.TempDir())
	synctest.Test(t, func(t *testing.T) {
		rec := capture.Default(t)
		var mu sync.Mutex
		installed, minted := "1", ""
		d, _ := newBareDaemon(t, recordingRunner("true", nil))
		d.tokens = fakeGitHub(t, 0, func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			defer mu.Unlock()
			if r.Method == http.MethodGet && r.URL.Path == "/app/installations" {
				writeJSON(w, http.StatusOK, `[{"id":`+installed+`,"account":{"login":"octo"}}]`)
				return
			}
			if r.URL.Path != "/app/installations/2/access_tokens" {
				writeJSON(w, http.StatusForbidden, `{"message":"This installation has been suspended"}`)
				return
			}
			minted = "2"
			writeJSON(w, http.StatusCreated, tokenBody())
		})
		run := func() trigger.Result {
			t.Helper()
			j := newJob("interval", nil, nil)
			d.execute(t.Context(), t.Context().Done(), j)
			return <-j.Result()
		}

		if out := run(); out.OK || out.Reason != mintFailedReason {
			t.Fatalf("run on the suspended installation = ok %v reason %q, want a failure with %q", out.OK, out.Reason, mintFailedReason)
		}
		mu.Lock()
		installed = "2"
		mu.Unlock()
		// forgeapi's re-probe wait after a first terminal refusal is 5 minutes.
		time.Sleep(5*time.Minute + time.Second)
		if out := run(); !out.OK {
			t.Fatalf("run after the reinstall = ok false (reason %q), want a token for the new installation", out.Reason)
		}

		if minted != "2" {
			t.Fatalf("no token minted for installation 2, so the run did not reach the reinstall")
		}
		if got := rec.AttrValuesExact("github app installation token issued", "installation_id"); !slices.Equal(got, []string{minted}) {
			t.Errorf("issued lines' installation_id = %q, want [%q], the installation the token was minted for", got, minted)
		}
		if got := rec.AttrValuesExact("github app installation resolved", "installation_id"); !slices.Equal(got, []string{"2"}) {
			t.Errorf("resolved lines' installation_id = %q, want [\"2\"]: one line, for the installation a token was minted for", got)
		}
	})
}

// TestExecute_TokenFailureFailsTheRunWithoutStartingRenovate covers the
// refusals only a human can repair. Not parallel: it swaps the global slog
// default.
func TestExecute_TokenFailureFailsTheRunWithoutStartingRenovate(t *testing.T) {
	tests := []struct {
		handler  http.HandlerFunc
		name     string
		wantHint string
	}{
		{
			name:     "a rejected key",
			wantHint: envAppID,
			handler: func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, http.StatusUnauthorized, `{"message":"Bad credentials"}`)
			},
		},
		{
			name:     "a missing installation",
			wantHint: envAppInstallationID,
			handler: func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, http.StatusNotFound, `{"message":"Not Found"}`)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("RENOVATE_BASE_DIR", t.TempDir())
			rec := capture.Default(t)
			var started [][]string
			d := appModeDaemon(t, recordingRunner("true", &started), tt.handler)
			d.status.Set(true)

			j := newJob("interval", nil, nil)
			d.execute(t.Context(), t.Context().Done(), j)

			out := <-j.Result()
			if out.OK || out.Reason != mintFailedReason {
				t.Errorf("outcome = ok %v reason %q, want a failure with reason %q", out.OK, out.Reason, mintFailedReason)
			}
			if len(started) != 0 {
				t.Errorf("renovate started %d times, want 0 without a token", len(started))
			}
			if d.status.Healthy() {
				t.Error("health marker healthy after a token failure, want unhealthy")
			}
			if !rec.AttrContains("github app token request failed", "hint", tt.wantHint) {
				t.Errorf("the failure line's hint does not name %s", tt.wantHint)
			}
		})
	}
}

// TestExecute_SeveralInstallationsFailTheRunNamingEach is not parallel: it
// swaps the global slog default.
func TestExecute_SeveralInstallationsFailTheRunNamingEach(t *testing.T) {
	t.Setenv("RENOVATE_BASE_DIR", t.TempDir())
	rec := capture.Default(t)
	d, _ := newBareDaemon(t, recordingRunner("true", nil))
	d.tokens = fakeGitHub(t, 0, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, `[{"id":1,"account":{"login":"a"}},{"id":2,"account":{"login":"b"}}]`)
	})

	j := newJob("interval", nil, nil)
	d.execute(t.Context(), t.Context().Done(), j)

	if out := <-j.Result(); out.OK || out.Reason != mintFailedReason {
		t.Errorf("outcome = ok %v reason %q, want a failure with reason %q", out.OK, out.Reason, mintFailedReason)
	}
	for _, account := range []string{"a (1)", "b (2)"} {
		if !rec.AttrContains("github app token request failed", "error", account) {
			t.Errorf("the failure line's error does not name the installation %q", account)
		}
	}
	if !rec.AttrContains("github app token request failed", "hint", envAppInstallationID) {
		t.Errorf("the failure line's hint does not name %s", envAppInstallationID)
	}
}

// TestTransientMintError separates what a GitHub outage causes from a fault
// that waiting does not fix, over the answers GitHub gives a token request.
func TestTransientMintError(t *testing.T) {
	t.Parallel()
	tests := []struct {
		header http.Header
		name   string
		status int
		want   bool
	}{
		{name: "a server error after the retries", status: http.StatusServiceUnavailable, want: true},
		{name: "a secondary rate limit", status: http.StatusTooManyRequests, want: true},
		{name: "a spent primary rate limit", status: http.StatusForbidden, header: http.Header{"X-Ratelimit-Remaining": {"0"}}, want: true},
		{name: "a rejected key", status: http.StatusUnauthorized},
		{name: "a missing installation", status: http.StatusNotFound},
		{name: "a suspended installation", status: http.StatusForbidden},
		{name: "a refused grant", status: http.StatusUnprocessableEntity},
		{name: "an unclassified refusal", status: http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			src := fakeGitHub(t, testInstallation, func(w http.ResponseWriter, _ *http.Request) {
				maps.Copy(w.Header(), tt.header)
				writeJSON(w, tt.status, `{"message":"refused"}`)
			}, forgeapi.WithRetries(0))

			_, err := src.Mint(t.Context())
			if err == nil {
				t.Fatalf("Mint() error = nil for HTTP %d, want a failure", tt.status)
			}
			if got := transientMintError(err); got != tt.want {
				t.Errorf("transientMintError(%v) = %v, want %v", err, got, tt.want)
			}
		})
	}
	t.Run("a GitHub that never answers", func(t *testing.T) {
		t.Parallel()
		src := fakeGitHub(t, testInstallation, nil, forgeapi.WithWireTransport(hangingTransport{}),
			forgeapi.WithRetries(0), forgeapi.WithOperationTimeout(300*time.Millisecond))

		_, err := src.Mint(t.Context())
		if err == nil {
			t.Fatal("Mint() error = nil from a GitHub that never answers, want the budget's end")
		}
		if !transientMintError(err) {
			t.Errorf("transientMintError(%v) = false, want true: the mint's budget ran out", err)
		}
	})
}

// hangingTransport accepts every request and never answers it, as a GitHub
// holding the connection open does.
type hangingTransport struct{}

func (hangingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	<-r.Context().Done()
	return nil, r.Context().Err()
}

// TestExecute_TokenOutageIsToleratedForTheGrace covers a GitHub outage that
// outlasts the mint's retries: like a repository error it passes as a warning
// until it has lasted failureGrace, while a rejected key (above) fails at once.
func TestExecute_TokenOutageIsToleratedForTheGrace(t *testing.T) {
	t.Setenv("RENOVATE_BASE_DIR", t.TempDir())
	var started [][]string
	d := appModeDaemon(t, recordingRunner("true", &started),
		func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusServiceUnavailable, `{"message":"unavailable"}`)
		}, forgeapi.WithRetries(0))
	d.status.Set(true)
	run := func() trigger.Result {
		t.Helper()
		j := newJob("interval", nil, nil)
		d.execute(t.Context(), t.Context().Done(), j)
		return <-j.Result()
	}

	if out := run(); !out.OK || out.Reason != toleratedReason {
		t.Fatalf("token outage = ok %v reason %q, want ok with %q", out.OK, out.Reason, toleratedReason)
	}
	if !d.status.Healthy() {
		t.Error("health unhealthy after a tolerated token outage, want it untouched")
	}

	d.failingSince = d.failingSince.Add(-failureGrace)
	if out := run(); out.OK || out.Reason != mintFailedReason {
		t.Errorf("token outage past the grace = ok %v reason %q, want a failure with %q", out.OK, out.Reason, mintFailedReason)
	}
	if d.status.Healthy() {
		t.Error("health healthy after a token outage lasting the grace, want unhealthy")
	}
	if len(started) != 0 {
		t.Errorf("renovate started %d times, want 0 without a token", len(started))
	}
}

// rateLimitedGitHub answers the first limited token requests 429 with
// Retry-After: 30, and every later one with a token, recording when each
// request arrived.
func rateLimitedGitHub(limited int) (http.HandlerFunc, func() []time.Time) {
	var mu sync.Mutex
	var asked []time.Time
	handler := func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		asked = append(asked, time.Now())
		n := len(asked)
		mu.Unlock()
		if n <= limited {
			w.Header().Set("Retry-After", "30")
			writeJSON(w, http.StatusTooManyRequests, `{"message":"You have exceeded a secondary rate limit"}`)
			return
		}
		writeJSON(w, http.StatusCreated, tokenBody())
	}
	return handler, func() []time.Time {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(asked)
	}
}

// TestExecute_RateLimitedMintDelaysTheRunUntilGitHubsWait covers a
// rate-limited token request: the same run mints again once GitHub's wait has
// passed, sending nothing before it, and then starts Renovate. Not parallel:
// it swaps the global slog default.
func TestExecute_RateLimitedMintDelaysTheRunUntilGitHubsWait(t *testing.T) {
	t.Setenv("RENOVATE_BASE_DIR", t.TempDir())
	synctest.Test(t, func(t *testing.T) {
		rec := capture.Default(t)
		handler, asked := rateLimitedGitHub(1)
		d := appModeDaemon(t, nil, handler)
		var runs int
		d.runOnce = func(context.Context, time.Duration, string, runPayload, scheduler.CommandRunner) runResult {
			runs++
			return runResult{outcome: runComplete}
		}

		j := newJob("interval", nil, nil)
		d.execute(t.Context(), t.Context().Done(), j)

		if out := <-j.Result(); !out.OK || out.Reason != "" {
			t.Errorf("outcome = ok %v reason %q, want the delayed run's success", out.OK, out.Reason)
		}
		if runs != 1 {
			t.Errorf("renovate started %d times, want once after GitHub's wait", runs)
		}
		got := asked()
		if len(got) != 2 {
			t.Fatalf("token requests = %d, want 2: the limited one and one after the wait", len(got))
		}
		if waited := got[1].Sub(got[0]); waited < 30*time.Second {
			t.Errorf("second token request sent %s after the first, want no sooner than GitHub's 30s", waited)
		}
		if wait, ok := rec.Attr("renovate run delayed: github app token rate-limited", "retry_after"); !ok || wait.Duration() != 30*time.Second {
			t.Errorf("delayed line retry_after = %v (found %v), want GitHub's 30s", wait, ok)
		}
		if n := rec.CountExact("renovate run skipped: github app token unavailable"); n != 0 {
			t.Errorf("skipped lines = %d, want 0: a rate limit delays the run", n)
		}
		if !d.failingSince.IsZero() {
			t.Errorf("failingSince = %v, want zero: a wait is no failure", d.failingSince)
		}
	})
}

// TestExecute_ShutdownDuringARateLimitWaitCancelsTheRun is not parallel: it
// swaps the global slog default.
func TestExecute_ShutdownDuringARateLimitWaitCancelsTheRun(t *testing.T) {
	t.Setenv("RENOVATE_BASE_DIR", t.TempDir())
	synctest.Test(t, func(t *testing.T) {
		rec := capture.Default(t)
		handler, asked := rateLimitedGitHub(1)
		d := appModeDaemon(t, nil, handler)
		reg := measure.NewRegistry(metricsPrefix)
		d.metrics = newRunMetrics(reg, "external", "app", scheduler.RunRecord{}, false)
		var runs int
		d.runOnce = func(context.Context, time.Duration, string, runPayload, scheduler.CommandRunner) runResult {
			runs++
			return runResult{outcome: runComplete}
		}
		shutdown, stop := context.WithCancel(t.Context())
		defer stop()

		j := newJob("interval", nil, nil)
		go d.execute(context.WithoutCancel(shutdown), shutdown.Done(), j)
		synctest.Wait()
		stop()

		if out := <-j.Result(); out.OK || out.Reason != shutdownCancelReason {
			t.Errorf("outcome = ok %v reason %q, want the shutdown's cancel %q", out.OK, out.Reason, shutdownCancelReason)
		}
		if runs != 0 || len(asked()) != 1 {
			t.Errorf("renovate started %d times after %d token requests, want none after the limited one", runs, len(asked()))
		}
		if !rec.AttrContains("run cancelled by shutdown", "stage", "token wait") {
			t.Error("no 'run cancelled by shutdown' line with stage \"token wait\"")
		}
		if n := rec.CountExact(runFinishedMsg); n != 0 {
			t.Errorf("%q lines = %d, want 0: a run cancelled during the wait never completed", runFinishedMsg, n)
		}
		for key, v := range series(t, reg) {
			if strings.Contains(key, "_runs_total{") && v != 0 {
				t.Errorf("%s = %v, want 0 after a run cancelled during the wait", key, v)
			}
		}
	})
}

// TestExecute_ARateLimitWaitKeepsHealthAsItWas covers a wait that outlasts
// the built-in schedule's lease, then a run lasting most of its timeout:
// /healthz answers through both as before the run, 200 for a healthy verdict
// set near the lease's end and 503 for an unhealthy one.
func TestExecute_ARateLimitWaitKeepsHealthAsItWas(t *testing.T) {
	const (
		interval   = 100 * time.Millisecond
		runTimeout = 1200 * time.Millisecond
		retryAfter = 2 * time.Second
		// age lapses the verdict inside the wait's first half lease, before the
		// first periodic refresh.
		age = time.Second
		// runLasts plus the time since the wait's last half-lease refresh,
		// 600ms before its end, passes the lease, so only a refresh at the
		// wait's end keeps the verdict fresh through the run.
		runLasts = 1100 * time.Millisecond
	)
	lease := runLease(interval, true, runTimeout, false)
	tests := []struct {
		name    string
		healthy bool
		outcome runOutcome
		want    int
	}{
		{name: "a healthy verdict stays fresh", healthy: true, outcome: runComplete, want: http.StatusOK},
		{name: "an unhealthy verdict stays unhealthy", outcome: runFailed, want: http.StatusServiceUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("RENOVATE_BASE_DIR", t.TempDir())
			synctest.Test(t, func(t *testing.T) {
				waited := make(chan struct{})
				var asked atomic.Int32
				d, _ := newBareDaemon(t, nil)
				d.tokens = fakeGitHub(t, testInstallation, func(w http.ResponseWriter, _ *http.Request) {
					switch asked.Add(1) {
					case 1:
						w.Header().Set("Retry-After", strconv.Itoa(int(retryAfter.Seconds())))
						writeJSON(w, http.StatusTooManyRequests, `{"message":"You have exceeded a secondary rate limit"}`)
						return
					case 2:
						close(waited)
					}
					writeJSON(w, http.StatusCreated, tokenBody())
				}, forgeapi.WithRetries(0))
				d.installationID = testInstallation
				d.timeout = runTimeout
				var ran time.Duration
				d.runOnce = func(context.Context, time.Duration, string, runPayload, scheduler.CommandRunner) runResult {
					began := time.Now()
					time.Sleep(runLasts)
					ran = time.Since(began)
					return runResult{outcome: tt.outcome}
				}
				d.lease = lease
				healthz := newHealthz(d.status, lease)
				d.status.Set(tt.healthy)
				time.Sleep(age)
				start := time.Now()

				j := newJob("interval", nil, nil)
				done := make(chan struct{})
				go func() { defer close(done); d.execute(t.Context(), t.Context().Done(), j) }()
				probe := time.NewTicker(50 * time.Millisecond)
				defer probe.Stop()
				var probes, wrong, firstCode int
				var waitedFor, firstAt time.Duration
				var firstBody string
				endOfWait := waited
			probing:
				for {
					select {
					case <-endOfWait:
						waitedFor, endOfWait = time.Since(start), nil
					case <-done:
						break probing
					case <-probe.C:
						rec := httptest.NewRecorder()
						healthz.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
						probes++
						if rec.Code != tt.want {
							if wrong == 0 {
								firstAt, firstCode, firstBody = time.Since(start), rec.Code, rec.Body.String()
							}
							wrong++
						}
					}
				}
				<-j.Result()

				if waitedFor <= lease {
					t.Fatalf("the wait lasted %s, within the %s lease, so it tests nothing", waitedFor, lease)
				}
				if ran >= lease {
					t.Fatalf("the run after the wait lasted %s, past the %s lease, so no refresh could keep the verdict fresh", ran, lease)
				}
				if wrong > 0 {
					t.Errorf("/healthz answered other than %d on %d of %d probes during the wait and the run after it; the first, %s in: %d %s",
						tt.want, wrong, probes, firstAt, firstCode, firstBody)
				}
			})
		})
	}
}

// TestExecute_ARateLimitWaitCountsNothingTowardTheGrace covers a run that
// waits out two hour-long rate limits and then fails in a way an outage
// causes: its failures count from the end of the wait, so a second failing
// run straight after it is still tolerated.
func TestExecute_ARateLimitWaitCountsNothingTowardTheGrace(t *testing.T) {
	tests := []struct {
		after   http.HandlerFunc
		outcome runOutcome
		name    string
	}{
		{
			name: "a token outage after the wait",
			after: func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, http.StatusServiceUnavailable, `{"message":"unavailable"}`)
			},
			outcome: runComplete,
		},
		{
			name:    "a repository error after the wait",
			after:   func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, http.StatusCreated, tokenBody()) },
			outcome: runRepositoryError,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("RENOVATE_BASE_DIR", t.TempDir())
			synctest.Test(t, func(t *testing.T) {
				var mu sync.Mutex
				var asked []time.Time
				d := appModeDaemon(t, nil, func(w http.ResponseWriter, r *http.Request) {
					mu.Lock()
					asked = append(asked, time.Now())
					n := len(asked)
					mu.Unlock()
					if n <= 2 {
						w.Header().Set("Retry-After", "3600")
						writeJSON(w, http.StatusTooManyRequests, `{"message":"You have exceeded a secondary rate limit"}`)
						return
					}
					tt.after(w, r)
				}, forgeapi.WithRetries(0))
				d.runOnce = func(context.Context, time.Duration, string, runPayload, scheduler.CommandRunner) runResult {
					return runResult{outcome: tt.outcome}
				}
				run := func() trigger.Result {
					t.Helper()
					j := newJob("interval", nil, nil)
					d.execute(t.Context(), t.Context().Done(), j)
					return <-j.Result()
				}

				if out := run(); !out.OK || out.Reason != toleratedReason {
					t.Fatalf("first failing run after the wait = ok %v reason %q, want ok with %q", out.OK, out.Reason, toleratedReason)
				}
				mu.Lock()
				got := slices.Clone(asked)
				mu.Unlock()
				if len(got) != 3 || got[2].Sub(got[0]) < 2*time.Hour {
					t.Fatalf("token requests = %v, want 3 spanning GitHub's two hour-long waits", got)
				}
				afterWait := got[2]
				if d.failingSince.Before(afterWait.Add(-time.Second)) {
					t.Errorf("failingSince is %s before the request after the wait, want no earlier than it", afterWait.Sub(d.failingSince))
				}
				if out := run(); !out.OK || out.Reason != toleratedReason {
					t.Errorf("second failing run straight after = ok %v reason %q, want ok with %q: the failures have lasted minutes", out.OK, out.Reason, toleratedReason)
				}
			})
		})
	}
}

// TestExecute_TokenNeverReachesTheLog renders every scheduler line at debug,
// through a retried mint, a successful run and a failed one, and requires
// neither the token nor the JWT in the output. Not parallel: it swaps the
// global slog default.
func TestExecute_TokenNeverReachesTheLog(t *testing.T) {
	t.Setenv("RENOVATE_BASE_DIR", t.TempDir())
	var buf bytes.Buffer
	var mu sync.Mutex
	capture.SwapDefault(t, slog.New(slog.NewTextHandler(&lockedWriter{w: &buf, mu: &mu}, &slog.HandlerOptions{Level: slog.LevelDebug})))

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
	}, forgeapi.WithRetries(1))

	for range 2 {
		j := newJob("interval", []string{"owner/repo"}, nil)
		d.execute(t.Context(), t.Context().Done(), j)
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
		{name: "a rejected key", status: http.StatusUnauthorized},
		{name: "a refused grant", status: http.StatusUnprocessableEntity},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("RENOVATE_BASE_DIR", t.TempDir())
			var buf bytes.Buffer
			var mu sync.Mutex
			capture.SwapDefault(t, slog.New(slog.NewTextHandler(&lockedWriter{w: &buf, mu: &mu}, &slog.HandlerOptions{Level: slog.LevelDebug})))
			var jwts []string
			d := appModeDaemon(t, recordingRunner("true", nil), func(w http.ResponseWriter, r *http.Request) {
				jwt := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
				mu.Lock()
				jwts = append(jwts, jwt)
				mu.Unlock()
				writeJSON(w, tt.status, `{"message":"rejected `+jwt+`"}`)
			})

			j := newJob("interval", nil, nil)
			d.execute(t.Context(), t.Context().Done(), j)
			<-j.Result()

			mu.Lock()
			defer mu.Unlock()
			logged := buf.String()
			if !strings.Contains(logged, "rejected [redacted]") {
				t.Fatalf("log lacks the redacted server message, so the test did not reach it:\n%s", logged)
			}
			for _, jwt := range jwts {
				// A length cap alone would keep the header, the claims and part
				// of the signature, so look for a signature fragment.
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
