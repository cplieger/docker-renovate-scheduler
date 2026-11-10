package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cplieger/forgeapi/v2"
	"github.com/cplieger/forgeapi/v2/creds"
	"github.com/cplieger/go/slogx/capture"
)

// proxyScenarioEnv names the scenario TestProxyScenario runs in a re-executed
// test binary. net/http reads the proxy variables once per process, so each
// scenario needs a process of its own.
const proxyScenarioEnv = "SCHEDULER_TEST_PROXY_SCENARIO"

const proxyPassword = "proxy-pass"

func TestLoadAppConfig_TheMintLeavesThroughHTTPSProxy(t *testing.T) {
	inProxyScenario(t, "through the proxy")
}

func TestLoadAppConfig_NoProxyNamingTheAPIDialsDirectly(t *testing.T) {
	inProxyScenario(t, "no proxy for the api")
}

func TestLoadAppConfig_WithoutAProxyPrivateAddressesStayRefused(t *testing.T) {
	inProxyScenario(t, "no proxy variable")
}

func TestLoadAppConfig_ProxyCredentialsNeverReachALogOrError(t *testing.T) {
	for _, scenario := range []string{"credentials on a failed mint", "credentials in an unparseable proxy"} {
		t.Run(scenario, func(t *testing.T) { inProxyScenario(t, scenario) })
	}
}

func TestLoadAppConfig_AProxyWithNoPortIsDialledOnItsSchemesDefault(t *testing.T) {
	for _, scenario := range []string{"a port-less http proxy", "a port-less bare host", "a port-less socks5 proxy"} {
		t.Run(scenario, func(t *testing.T) { inProxyScenario(t, scenario) })
	}
}

// TestIsProxyURL holds the accepted forms to what forgeapi's Connection.Proxy
// dials, a port-less URL on its scheme's default port, and net/http's bare
// host[:port] form.
func TestIsProxyURL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		raw  string
		want bool
	}{
		{name: "an http proxy", raw: "http://proxy.example:3128", want: true},
		{name: "credentials, a private address and a trailing slash", raw: "http://user:pass@10.0.0.1:3128/", want: true},
		{name: "an https proxy on its default port", raw: "https://proxy.example", want: true},
		{name: "a SOCKS5 proxy", raw: "socks5://proxy.example:1080", want: true},
		{name: "a SOCKS5 proxy resolving names", raw: "socks5h://proxy.example:1080", want: true},
		{name: "a bare host and port", raw: "proxy.example:3128", want: true},
		{name: "an http proxy on its default port", raw: "http://proxy.example", want: true},
		{name: "a SOCKS5 proxy on its default port", raw: "socks5://proxy.example", want: true},
		{name: "a bare host", raw: "proxy.example", want: true},
		{name: "a scheme forgeapi does not proxy over", raw: "ftp://proxy.example:21"},
		{name: "a named port", raw: "http://proxy.example:port"},
		{name: "a port past 65535", raw: "http://proxy.example:99999"},
		{name: "a broken escape", raw: "http://proxy%zz.example:3128"},
		{name: "a path", raw: "http://proxy.example:3128/path"},
		{name: "a query", raw: "http://proxy.example:3128?q=1"},
		{name: "no host", raw: "http://:3128"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := isProxyURL(tt.raw); got != tt.want {
				t.Errorf("isProxyURL(%q) = %v, want %v", tt.raw, got, tt.want)
			}
		})
	}
}

// inProxyScenario runs scenario in a child test process and fails t with the
// child's output unless the child ran it and passed.
func inProxyScenario(t *testing.T, scenario string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestProxyScenario$", "-test.count=1", "-test.v",
		"-test.timeout=60s")
	cmd.Env = append(os.Environ(), proxyScenarioEnv+"="+scenario)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("scenario %q failed in its child process: %v\n%s", scenario, err, out)
	}
	if !bytes.Contains(out, []byte("--- PASS: TestProxyScenario")) {
		t.Fatalf("scenario %q did not run in its child process:\n%s", scenario, out)
	}
}

// TestProxyScenario is the child process's side of inProxyScenario.
func TestProxyScenario(t *testing.T) {
	scenario, ok := proxyScenarios[os.Getenv(proxyScenarioEnv)]
	if !ok {
		t.Skip("runs only in the child process a proxy test starts")
	}
	for _, name := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "NO_PROXY", "no_proxy"} {
		t.Setenv(name, "")
	}
	t.Setenv("RENOVATE_BASE_DIR", t.TempDir())
	t.Setenv(envAppID, "4242")
	t.Setenv(envAppInstallationID, "77")
	t.Setenv(envAppKeyFile, writeKeyFile(t, false))
	t.Setenv(envAppKeyInline, "")
	scenario(t)
}

var proxyScenarios = map[string]func(*testing.T){
	"through the proxy": func(t *testing.T) {
		proxy := newFakeProxy(t)
		t.Setenv("HTTPS_PROXY", proxy.url("renovate:"+proxyPassword))
		t.Setenv("NO_PROXY", "uploads.github.com")
		src := proxiedSource(t)

		if _, err := src.Mint(t.Context()); err == nil {
			t.Fatal("Mint() error = nil through a proxy that refuses every tunnel, want a failure")
		}
		got, auth := proxy.seen()
		if !slices.Equal(got, []string{"CONNECT api.github.com:443"}) {
			t.Errorf("proxy saw %q, want one tunnel to GitHub's API", got)
		}
		if want := "Basic " + base64.StdEncoding.EncodeToString([]byte("renovate:"+proxyPassword)); auth != want {
			t.Errorf("proxy Proxy-Authorization = %q, want the credentials HTTPS_PROXY carries", auth)
		}
	},
	"no proxy for the api": func(t *testing.T) {
		t.Setenv("HTTPS_PROXY", newFakeProxy(t).url(""))
		t.Setenv("NO_PROXY", "api.github.com")
		app := loadProxyConfig(t)

		if app.proxy != "" {
			t.Errorf("app proxy = %q, want none: NO_PROXY names GitHub's API", app.proxy)
		}
		requirePrivateRefused(t, app)
	},
	"no proxy variable": func(t *testing.T) {
		app := loadProxyConfig(t)

		if app.proxy != "" {
			t.Errorf("app proxy = %q, want none with no proxy variable set", app.proxy)
		}
		requirePrivateRefused(t, app)
	},
	"credentials on a failed mint": func(t *testing.T) {
		proxy := newFakeProxy(t)
		t.Setenv("HTTPS_PROXY", proxy.url("renovate:"+proxyPassword))
		var buf bytes.Buffer
		var mu sync.Mutex
		capture.SwapDefault(t, slog.New(slog.NewTextHandler(&lockedWriter{w: &buf, mu: &mu}, &slog.HandlerOptions{Level: slog.LevelDebug})))
		d, _ := newBareDaemon(t, recordingRunner("true", nil))
		d.tokens = proxiedSource(t)
		d.installationID = testInstallation

		_, err := d.tokens.Mint(t.Context())
		j := newJob("interval", nil, nil)
		d.execute(t.Context(), t.Context().Done(), j)
		<-j.Result()

		if _, auth := proxy.seen(); auth == "" {
			t.Fatal("the proxy saw no credentials, so the test did not reach it")
		}
		if err == nil || strings.Contains(err.Error(), proxyPassword) {
			t.Errorf("Mint() error = %v, want a failure that does not quote the proxy's password", err)
		}
		mu.Lock()
		defer mu.Unlock()
		if !strings.Contains(buf.String(), "renovate run skipped: github app token unavailable") {
			t.Fatalf("log lacks the skipped run's line, so the test did not reach it:\n%s", buf.String())
		}
		if strings.Contains(buf.String(), proxyPassword) {
			t.Errorf("log quotes the proxy's password:\n%s", buf.String())
		}
	},
	"a port-less http proxy":   portlessProxyScenario("http://127.0.0.1", "http://127.0.0.1"),
	"a port-less bare host":    portlessProxyScenario("127.0.0.1", "http://127.0.0.1"),
	"a port-less socks5 proxy": portlessProxyScenario("socks5://127.0.0.1", "socks5://127.0.0.1"),
	"credentials in an unparseable proxy": func(t *testing.T) {
		t.Setenv("RUN_INTERVAL", "off")
		for _, value := range []string{
			"http://renovate:" + proxyPassword + "@proxy.example:port",
			"http://renovate:" + proxyPassword + "@proxy%zz.example:3128",
			"ftp://renovate:" + proxyPassword + "@proxy.example:21",
		} {
			t.Setenv("HTTPS_PROXY", value)
			var buf bytes.Buffer
			capture.SwapDefault(t, slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
			// A daemon that accepted the value would serve until this ends.
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)

			err := runDaemon(ctx, testSocketPath(t), recordingRunner("true", nil))
			cancel()

			if err == nil || !strings.Contains(err.Error(), "HTTPS_PROXY") {
				t.Fatalf("runDaemon() error = %v, want a start failure naming HTTPS_PROXY", err)
			}
			if strings.Contains(err.Error(), proxyPassword) {
				t.Errorf("runDaemon() error = %q, which quotes the proxy's password", err)
			}
			if !strings.Contains(buf.String(), `level=ERROR msg="github app configuration invalid"`) {
				t.Fatalf("log lacks the start failure's line, so the test did not reach it:\n%s", buf.String())
			}
			if strings.Contains(buf.String(), proxyPassword) {
				t.Errorf("log quotes the proxy's password:\n%s", buf.String())
			}
		}
	},
}

// portlessProxyScenario sets HTTPS_PROXY to value, which states no port, and
// requires the App proxy net/http resolves it to (want) and a mint that
// forgeapi's port allow-list lets reach the dial: net/http dials such a
// proxy on its scheme's default port, where nothing in the test listens.
func portlessProxyScenario(value, want string) func(*testing.T) {
	return func(t *testing.T) {
		t.Setenv("HTTPS_PROXY", value)
		if app := loadProxyConfig(t); app.proxy != want {
			t.Errorf("app proxy = %q, want %q", app.proxy, want)
		}

		_, err := proxiedSource(t).Mint(t.Context())
		if err == nil {
			t.Fatal("Mint() error = nil through a proxy address nothing serves, want a failure")
		}
		if fe, ok := errors.AsType[*forgeapi.Error](err); ok && fe.Code == forgeapi.CodeRedirectPortRefused {
			t.Errorf("Mint() error = %v, want a dial to the proxy's default port, not %s", err, forgeapi.CodeRedirectPortRefused)
		}
	}
}

// loadProxyConfig loads the child's App settings.
func loadProxyConfig(t *testing.T) *appConfig {
	t.Helper()
	app, err := loadAppConfig()
	if err != nil || app == nil {
		t.Fatalf("loadAppConfig() = %v, %v, want App settings", app, err)
	}
	return app
}

// proxiedSource is the child's App source, built as production builds it, with
// one attempt so a refused tunnel fails at once.
func proxiedSource(t *testing.T) *creds.AppSource {
	t.Helper()
	src, err := newAppSource(loadProxyConfig(t), forgeapi.WithRetries(0), forgeapi.WithOperationTimeout(10*time.Second))
	if err != nil {
		t.Fatalf("newAppSource() error = %v", err)
	}
	return src
}

// requirePrivateRefused requires app's source options to keep forgeapi's
// refusal of a private address, by building a source on one.
func requirePrivateRefused(t *testing.T, app *appConfig) {
	t.Helper()
	_, err := creds.NewAppSource(forgeapi.Connection{WebBaseURL: "https://10.0.0.1"}, app.key,
		creds.AppSourceConfig{Installation: creds.InstallationByID(testInstallation), Grant: creds.FullGrant()},
		appSourceOptions(app)...)
	if fe, ok := errors.AsType[*forgeapi.Error](err); !ok || fe.Code != forgeapi.CodePrivateAddressRefused {
		t.Errorf("NewAppSource() on a private address with the scheduler's options = %v, want %s", err, forgeapi.CodePrivateAddressRefused)
	}
}

// fakeProxy records each request a proxy is sent and refuses it with 502, as
// a proxy that cannot reach GitHub does.
type fakeProxy struct {
	srv  *httptest.Server
	got  []string
	auth string
	mu   sync.Mutex
}

func newFakeProxy(t *testing.T) *fakeProxy {
	t.Helper()
	p := &fakeProxy{}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		p.got = append(p.got, r.Method+" "+r.Host)
		p.auth = r.Header.Get("Proxy-Authorization")
		p.mu.Unlock()
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(p.srv.Close)
	return p
}

// url is the proxy's URL, carrying userinfo when it is not empty.
func (p *fakeProxy) url(userinfo string) string {
	if userinfo == "" {
		return p.srv.URL
	}
	return "http://" + userinfo + "@" + p.srv.Listener.Addr().String()
}

func (p *fakeProxy) seen() (requests []string, auth string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.got), p.auth
}
