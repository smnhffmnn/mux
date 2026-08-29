package health

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/smnhffmnn/mux/internal/config"
)

type fakeRegistry struct{ names []string }

func (f fakeRegistry) RegisteredToolNames() []string { return f.names }

type fakeTunnels struct{ up map[string]bool }

func (f fakeTunnels) IsUp(name string) bool { return f.up[name] }

// isolateSecrets points the config package's secret lookups at a temp dir.
// SetProvisioned supplements missing tokens from the secret store, and without
// this a test would read the developer's real secrets.toml.
func isolateSecrets(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
}

func newChecker(cfg *config.Config, reg Registry, tun Tunnels) *Checker {
	c := NewChecker(cfg, reg, tun, "test", time.Unix(0, 0))
	c.now = func() time.Time { return time.Unix(42, 0) }
	return c
}

func TestCheckAllHealthy(t *testing.T) {
	isolateSecrets(t)

	cfg := &config.Config{
		Provisioning: []config.ProvisioningConfig{
			{Name: "nas", Endpoint: "https://api.example.com/provision", Token: "t"},
		},
		Tunnels:     []config.TunnelConfig{{Name: "wg0", Type: config.TunnelTypeWireGuard}},
		Connections: []config.Connection{{Name: "local1", Type: config.TypeHTTP, URL: "https://a.example.com"}},
	}
	cfg.SetProvisioned("nas", nil, []config.Connection{
		{Name: "prov1", Type: config.TypeHTTP, URL: "https://b.example.com"},
	})

	c := newChecker(cfg,
		fakeRegistry{names: []string{"local1_get", "local1_request", "prov1_get"}},
		fakeTunnels{up: map[string]bool{"wg0": true}})

	rep := c.Check()

	if rep.Status != StatusOK {
		t.Fatalf("status = %q, problems = %v; want ok", rep.Status, rep.Problems)
	}
	if len(rep.Problems) != 0 {
		t.Errorf("problems = %v; want none", rep.Problems)
	}
	if rep.UptimeSecs != 42 {
		t.Errorf("uptime = %d; want 42", rep.UptimeSecs)
	}
	if len(rep.Connections) != 2 {
		t.Errorf("connections = %d; want 2", len(rep.Connections))
	}
	if len(rep.Provisioning) != 1 || !rep.Provisioning[0].Delivered {
		t.Errorf("provisioning = %+v; want one delivered endpoint", rep.Provisioning)
	}
	if rep.Provisioning[0].Connections != 1 {
		t.Errorf("provisioned connection count = %d; want 1", rep.Provisioning[0].Connections)
	}
}

// The case the package exists for: mux started before its network, the
// provisioning fetch failed, and everything else looks fine.
func TestCheckProvisioningNeverDelivered(t *testing.T) {
	isolateSecrets(t)

	cfg := &config.Config{
		Provisioning: []config.ProvisioningConfig{
			{Name: "nas", Endpoint: "https://api.example.com/provision", Token: "t"},
		},
	}

	c := newChecker(cfg, fakeRegistry{}, fakeTunnels{})
	rep := c.Check()

	if rep.Status != StatusDegraded {
		t.Fatalf("status = %q; want degraded", rep.Status)
	}
	if len(rep.Provisioning) != 1 || rep.Provisioning[0].Delivered {
		t.Fatalf("provisioning = %+v; want one undelivered endpoint", rep.Provisioning)
	}
	if len(rep.Problems) != 1 {
		t.Fatalf("problems = %v; want exactly one", rep.Problems)
	}
}

// An endpoint that answered with an empty profile is provisioned. Inferring
// failure from a zero count would report this as broken.
func TestCheckEmptyProfileIsDelivered(t *testing.T) {
	isolateSecrets(t)

	cfg := &config.Config{
		Provisioning: []config.ProvisioningConfig{
			{Name: "nas", Endpoint: "https://api.example.com/provision", Token: "t"},
		},
	}
	cfg.SetProvisioned("nas", nil, nil)

	rep := newChecker(cfg, fakeRegistry{}, fakeTunnels{}).Check()

	if rep.Status != StatusOK {
		t.Fatalf("status = %q, problems = %v; want ok", rep.Status, rep.Problems)
	}
	if !rep.Provisioning[0].Delivered {
		t.Error("delivered = false; want true for an empty but answered profile")
	}
}

// A provisioning endpoint without a token is not configured, so it is not a fault.
func TestCheckDisabledProvisioningIgnored(t *testing.T) {
	isolateSecrets(t)

	cfg := &config.Config{
		Provisioning: []config.ProvisioningConfig{
			{Name: "nas", Endpoint: "https://api.example.com/provision"},
		},
	}

	rep := newChecker(cfg, fakeRegistry{}, fakeTunnels{}).Check()

	if rep.Status != StatusOK {
		t.Fatalf("status = %q, problems = %v; want ok", rep.Status, rep.Problems)
	}
	if len(rep.Provisioning) != 0 {
		t.Errorf("provisioning = %+v; want empty", rep.Provisioning)
	}
}

func TestCheckTunnelDown(t *testing.T) {
	isolateSecrets(t)

	cfg := &config.Config{
		Tunnels: []config.TunnelConfig{{Name: "wg0", Type: config.TunnelTypeWireGuard}},
	}

	rep := newChecker(cfg, fakeRegistry{}, fakeTunnels{up: map[string]bool{"wg0": false}}).Check()

	if rep.Status != StatusDegraded {
		t.Fatalf("status = %q; want degraded", rep.Status)
	}
	if len(rep.Tunnels) != 1 || rep.Tunnels[0].Up {
		t.Fatalf("tunnels = %+v; want one down", rep.Tunnels)
	}
}

func TestCheckConnectionNotRegistered(t *testing.T) {
	isolateSecrets(t)

	cfg := &config.Config{
		Connections: []config.Connection{
			{Name: "youtrack-index", Type: config.TypeHTTP, URL: "https://a.example.com"},
		},
	}

	rep := newChecker(cfg, fakeRegistry{}, fakeTunnels{}).Check()

	if rep.Status != StatusDegraded {
		t.Fatalf("status = %q; want degraded", rep.Status)
	}
	if len(rep.Connections) != 1 || rep.Connections[0].Registered {
		t.Fatalf("connections = %+v; want one unregistered", rep.Connections)
	}
	if rep.Connections[0].Detail != "no tools registered" {
		t.Errorf("detail = %q; want %q", rep.Connections[0].Detail, "no tools registered")
	}
}

// An OAuth proxy nobody has logged into registers nothing. That is reported,
// but it must not hold the whole instance down — no restart fixes it.
func TestCheckOAuthProxyAwaitingLoginIsNotDegraded(t *testing.T) {
	isolateSecrets(t)

	cfg := &config.Config{
		Connections: []config.Connection{
			{Name: "higgsfield", Type: config.TypeHiggsfield, URL: "https://mcp.example.com/mcp", OAuth: true},
		},
	}

	rep := newChecker(cfg, fakeRegistry{}, fakeTunnels{}).Check()

	if rep.Status != StatusOK {
		t.Fatalf("status = %q, problems = %v; want ok", rep.Status, rep.Problems)
	}
	if len(rep.Connections) != 1 {
		t.Fatalf("connections = %+v; want one", rep.Connections)
	}
	if rep.Connections[0].Registered {
		t.Error("registered = true; want false")
	}
	if rep.Connections[0].Detail != "awaiting oauth login" {
		t.Errorf("detail = %q; want %q", rep.Connections[0].Detail, "awaiting oauth login")
	}
}

// A connection missing the fields that make it usable is not configured yet,
// so it is not a fault either.
func TestCheckDisabledConnectionIgnored(t *testing.T) {
	isolateSecrets(t)

	cfg := &config.Config{
		Connections: []config.Connection{{Name: "half-done", Type: config.TypeHTTP}},
	}

	rep := newChecker(cfg, fakeRegistry{}, fakeTunnels{}).Check()

	if rep.Status != StatusOK {
		t.Fatalf("status = %q, problems = %v; want ok", rep.Status, rep.Problems)
	}
	if len(rep.Connections) != 0 {
		t.Errorf("connections = %+v; want empty", rep.Connections)
	}
}

// Connection names where one is a prefix of the other: the tool must be
// credited to the longer match only, or "a" looks registered because "a_b" is.
func TestCheckLongestPrefixWins(t *testing.T) {
	isolateSecrets(t)

	cfg := &config.Config{
		Connections: []config.Connection{
			{Name: "a", Type: config.TypeHTTP, URL: "https://a.example.com"},
			{Name: "a_b", Type: config.TypeHTTP, URL: "https://b.example.com"},
		},
	}

	rep := newChecker(cfg, fakeRegistry{names: []string{"a_b_get"}}, fakeTunnels{}).Check()

	got := map[string]bool{}
	for _, c := range rep.Connections {
		got[c.Name] = c.Registered
	}
	if got["a_b"] != true {
		t.Errorf("a_b registered = %v; want true", got["a_b"])
	}
	if got["a"] != false {
		t.Errorf("a registered = %v; want false — a_b_get belongs to a_b", got["a"])
	}
}

// Connection names are sanitized into tool names, so the health check has to
// look for the sanitized prefix, not the raw name.
func TestCheckSanitizedConnectionName(t *testing.T) {
	isolateSecrets(t)

	cfg := &config.Config{
		Connections: []config.Connection{
			{Name: "MariaDB local dev", Type: config.TypeHTTP, URL: "https://a.example.com"},
		},
	}

	rep := newChecker(cfg,
		fakeRegistry{names: []string{"MariaDB_local_dev_query"}}, fakeTunnels{}).Check()

	if rep.Status != StatusOK {
		t.Fatalf("status = %q, problems = %v; want ok", rep.Status, rep.Problems)
	}
	if !rep.Connections[0].Registered {
		t.Error("registered = false; want true for the sanitized prefix")
	}
}

func TestHandlerStatusCodes(t *testing.T) {
	isolateSecrets(t)

	tests := []struct {
		name string
		cfg  *config.Config
		want int
	}{
		{
			name: "healthy",
			cfg:  &config.Config{},
			want: http.StatusOK,
		},
		{
			name: "degraded",
			cfg: &config.Config{
				Tunnels: []config.TunnelConfig{{Name: "wg0"}},
			},
			want: http.StatusServiceUnavailable,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := newChecker(tc.cfg, fakeRegistry{}, fakeTunnels{})
			rec := httptest.NewRecorder()
			c.Handler()(rec, httptest.NewRequest(http.MethodGet, "/health", nil))

			if rec.Code != tc.want {
				t.Errorf("status = %d; want %d", rec.Code, tc.want)
			}
			if got := rec.Header().Get("Content-Type"); got != "application/json" {
				t.Errorf("content-type = %q; want application/json", got)
			}

			var rep Report
			if err := json.Unmarshal(rec.Body.Bytes(), &rep); err != nil {
				t.Fatalf("body is not a Report: %v", err)
			}
			// The slices are initialised so the JSON carries [] rather than null:
			// a consumer iterating the response should not have to nil-check.
			if rep.Problems == nil || rep.Connections == nil || rep.Tunnels == nil || rep.Provisioning == nil {
				t.Errorf("report has null slices: %+v", rep)
			}
		})
	}
}

func TestMountAnswersOnlyGet(t *testing.T) {
	isolateSecrets(t)

	mux := http.NewServeMux()
	newChecker(&config.Config{}, fakeRegistry{}, fakeTunnels{}).Mount(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("GET status = %d; want 200", rec.Code)
	}

	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/health", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST status = %d; want 405", rec.Code)
	}
}
