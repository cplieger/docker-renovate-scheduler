package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/cplieger/forgeapi/v2"
	"github.com/cplieger/forgeapi/v2/creds"
	"github.com/cplieger/go/envx"
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
	githubWebBase = "https://github.com"
	// githubAPIHost is where forgeapi sends github.com's token requests.
	githubAPIHost   = "api.github.com"
	maxKeyFileBytes = 64 << 10
	// A mint gets up to four attempts, the last ending within two minutes.
	tokenMintBudget  = 2 * time.Minute
	tokenMintRetries = 3
	// tokenExpiryMargin is how long before its token's expiry a run is stopped.
	tokenExpiryMargin = 2 * time.Minute
	// installationTokenLife is GitHub's documented token life. Measured on the
	// host's clock it bounds expires_at, which is on GitHub's, so a host clock
	// behind GitHub's cannot stretch a run past its token:
	// https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/generating-an-installation-access-token-for-a-github-app
	installationTokenLife = time.Hour
)

// appConfig is the validated GitHub App configuration. A nil *appConfig means
// the App settings are absent and Renovate uses its own token.
type appConfig struct {
	key            *creds.AppKey
	proxy          string // the proxy the token requests leave through, "" for a direct dial
	appID          int
	installationID int64 // 0 resolves the App's single installation at first use
}

// loadAppConfig reads the App settings. Absent settings return (nil, nil);
// partial or unusable settings return an error so the daemon stops at start.
// No error quotes a setting's value: a miswired container can hold a token or
// the key itself in any of them.
// appConfigured reports whether any GitHub App setting is set, the test
// loadAppConfig makes before it validates them. The health probe uses it to
// size the lease without reading the key: a set but invalid configuration
// stops the daemon before it writes a health file.
func appConfigured() bool {
	return strings.TrimSpace(envx.String(envAppID)) != "" ||
		strings.TrimSpace(envx.String(envAppInstallationID)) != "" ||
		envx.String(envAppKeyFile) != ""
}

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
	if !appConfigured() {
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
	}
	key, err := readAppKey(keyPath, appID)
	if err != nil {
		return nil, err
	}
	proxy, err := githubAPIProxy()
	if err != nil {
		return nil, err
	}
	return &appConfig{key: key, proxy: proxy, appID: appID, installationID: int64(instID)}, nil
}

// githubAPIProxy answers the proxy the token requests leave through, "" for a
// direct dial: HTTPS_PROXY unless NO_PROXY exempts GitHub's API, as net/http's
// default transport reads them, since forgeapi reads neither. A value that is
// no proxy URL is refused, where net/http would dial directly or misread it.
// net/http reads the variables once per process.
func githubAPIProxy() (string, error) {
	raw := cmp.Or(envx.String("HTTPS_PROXY"), envx.String("https_proxy"))
	if raw == "" {
		return "", nil
	}
	// No error quotes the value, which can hold the proxy's password.
	refused := errors.New("HTTPS_PROXY (or https_proxy) does not hold a proxy URL")
	if !isProxyURL(raw) {
		return "", refused
	}
	u, err := http.ProxyFromEnvironment(&http.Request{URL: &url.URL{Scheme: "https", Host: githubAPIHost}})
	if err != nil {
		return "", refused
	}
	if u == nil {
		return "", nil
	}
	return u.String(), nil
}

// isProxyURL reports whether raw names a proxy forgeapi dials: an http,
// https or SOCKS5 URL with a host, a port or none (its scheme's default),
// and nothing after it, or net/http's bare host[:port] form, read as http.
func isProxyURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		u, err = url.Parse("http://" + raw)
	}
	if err != nil || u.Hostname() == "" || strings.TrimPrefix(u.Path, "/") != "" || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	if port := u.Port(); port != "" {
		if _, err := strconv.ParseUint(port, 10, 16); err != nil {
			return false
		}
	}
	return slices.Contains([]string{"http", "https", "socks5", "socks5h"}, u.Scheme)
}

func notPositiveInt(key string) error {
	return fmt.Errorf("%s must be a positive integer", key)
}

// readAppKey reads the App's private key file, GitHub's PKCS#1 download or
// PKCS#8, as the key of the App GitHub names by appID. Errors name neither the
// path nor the content.
func readAppKey(path string, appID int) (*creds.AppKey, error) {
	if strings.ContainsAny(path, "\r\n") || strings.Contains(path, "-----BEGIN") {
		return nil, fmt.Errorf("%s holds key material, not a path: put the key in a file and set %s to that file's path", envAppKeyFile, envAppKeyFile)
	}
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
	key, err := creds.ParseAppKeyPEM(strconv.Itoa(appID), raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", envAppKeyFile, err)
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

// newAppSource builds the token source of app's installation on github.com,
// asking for the installation's full grant, since Renovate needs whatever the
// App holds. opts follow appSourceOptions, so they win.
func newAppSource(app *appConfig, opts ...forgeapi.Option) (*creds.AppSource, error) {
	install := creds.OnlyInstallation()
	if app.installationID != 0 {
		install = creds.InstallationByID(app.installationID)
	}
	return creds.NewAppSource(forgeapi.Connection{WebBaseURL: githubWebBase, Proxy: app.proxy}, app.key,
		creds.AppSourceConfig{Installation: install, Grant: creds.FullGrant()},
		slices.Concat(appSourceOptions(app), opts)...)
}

// appSourceOptions is the scheduler's mint budget and, behind a proxy, leave
// to reach a proxy on a private address.
func appSourceOptions(app *appConfig) []forgeapi.Option {
	opts := []forgeapi.Option{
		forgeapi.WithRetries(tokenMintRetries),
		forgeapi.WithOperationTimeout(tokenMintBudget),
	}
	if app.proxy != "" {
		// The operator named the proxy in the environment, and the proxy, not
		// this process, dials GitHub, so the address policy only judges the proxy.
		opts = append(opts, forgeapi.WithPrivateAddresses(true))
	}
	return opts
}

// transientMintError reports whether a failed mint is one a GitHub outage
// causes: a server or network failure that outlasted the retries, a rate
// limit, or the mint's own budget running out. A rejected key, a missing
// installation or a refusal GitHub gives no class stays a hard failure.
func transientMintError(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	fe, ok := errors.AsType[*forgeapi.Error](err)
	return ok && fe.Retryable
}

// rateLimitWait is how long the App source holds mints after a rate limit:
// GitHub's stated wait, or forgeapi's own where GitHub stated none; zero where
// err is no rate limit.
func rateLimitWait(err error) time.Duration {
	if fe, ok := errors.AsType[*forgeapi.Error](err); ok && fe.Kind == forgeapi.KindRateLimited {
		return fe.RetryAfter
	}
	return 0
}

// mintHint is the failure line's pointer for a failed mint: GITHUB_APP_ID for
// a rejected key, GITHUB_APP_INSTALLATION_ID for a missing installation, and
// the GitHub App section of docs/configuration.md for any other failure.
func mintHint(err error) string {
	fe, _ := errors.AsType[*forgeapi.Error](err)
	switch {
	case fe != nil && fe.Kind == forgeapi.KindUnauthorized:
		return "check " + envAppID + " and that the private key belongs to that App"
	case fe != nil && fe.Kind == forgeapi.KindNotFound:
		return "check " + envAppInstallationID + " and that the App is installed"
	}
	return "see docs/configuration.md, 'GitHub App authentication'"
}

// runTimeoutFor caps runTimeout so a run started now stops
// tokenExpiryMargin before its token's expiry: Renovate reads its token once
// at start, so a run cannot switch to a fresh one. It fails where the token
// leaves no time for a run.
func runTimeoutFor(runTimeout time.Duration, expiry time.Time) (time.Duration, error) {
	left := time.Until(expiry)
	if left < tokenExpiryMargin+time.Second {
		return 0, fmt.Errorf("the installation token expires at %s, under %s from now, so no run fits before it",
			expiry.UTC().Format(time.RFC3339), tokenExpiryMargin+time.Second)
	}
	return min(runTimeout, (left - tokenExpiryMargin).Truncate(time.Second)), nil
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
func logAuthMode(app *appConfig) {
	if app == nil {
		slog.Info("github auth mode", "mode", "token",
			"source", "RENOVATE_TOKEN or Renovate's own configuration")
		return
	}
	installation := "resolved at the first run"
	if app.installationID != 0 {
		installation = strconv.FormatInt(app.installationID, 10)
	}
	slog.Info("github auth mode", "mode", "app", "app_id", app.appID,
		"installation_id", installation)
}
