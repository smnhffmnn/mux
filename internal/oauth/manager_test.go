package oauth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/smnhffmnn/mux/internal/config"
)

func testManager() *Manager {
	cfg := &config.Config{
		Connections: []config.Connection{
			{Name: "notion", Type: config.TypeNotion, URL: "https://mcp.notion.example/mcp", OAuth: true},
			{Name: "plain-proxy", Type: config.TypeProxy, URL: "https://mcp.example/mcp"},
			{Name: "no-url", Type: config.TypeProxy, OAuth: true},
		},
	}
	return NewManager(cfg, 7700, nil)
}

func TestStartValidation(t *testing.T) {
	m := testManager()

	if _, err := m.Start("unknown"); err == nil {
		t.Error("Start(unknown) succeeded, want error")
	}
	if _, err := m.Start("plain-proxy"); err == nil {
		t.Error("Start on a non-OAuth connection succeeded, want error")
	}
	if _, err := m.Start("no-url"); err == nil {
		t.Error("Start without URL succeeded, want error")
	}
}

func TestTakeFlowExpiresAbandonedAttempts(t *testing.T) {
	m := testManager()
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return now }

	// Two attempts started directly, as Start would after a successful
	// discovery and registration (which need a provider).
	m.pending["fresh"] = &flow{service: "notion", started: now}
	m.pending["stale"] = &flow{service: "notion", started: now.Add(-flowTTL - time.Second)}

	if _, ok := m.takeFlow("stale"); ok {
		t.Error("takeFlow(stale) redeemed a flow older than flowTTL")
	}
	if _, ok := m.takeFlow("fresh"); !ok {
		t.Error("takeFlow(fresh) did not find a flow within flowTTL")
	}
	if _, ok := m.takeFlow("fresh"); ok {
		t.Error("takeFlow(fresh) redeemed the same state twice")
	}
	if len(m.pending) != 0 {
		t.Errorf("pending = %d flows after both were taken, want 0", len(m.pending))
	}

	// An abandoned attempt is also dropped when a different state is looked
	// up, so a map of never-completed flows cannot grow unbounded.
	m.pending["stale"] = &flow{service: "notion", started: now.Add(-flowTTL - time.Second)}
	m.takeFlow("never-issued")
	if _, ok := m.pending["stale"]; ok {
		t.Error("stale flow survived a lookup for another state")
	}

	// Start sweeps as well (sweepLocked before it records the new flow), so
	// an instance that only ever starts flows does not keep the abandoned ones.
	m.pending["stale"] = &flow{service: "notion", started: now.Add(-flowTTL - time.Second)}
	m.mu.Lock()
	m.sweepLocked(now)
	m.mu.Unlock()
	if _, ok := m.pending["stale"]; ok {
		t.Error("stale flow survived sweepLocked")
	}
}

func TestCallbackRejectsUnknownState(t *testing.T) {
	m := testManager()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/oauth/callback?code=abc&state=never-issued", nil)

	m.CallbackHandler()(rec, req)

	if !strings.Contains(rec.Body.String(), "Unknown or expired OAuth state") {
		t.Errorf("body = %q, want unknown-state error page", rec.Body.String())
	}
}

func TestCallbackReportsProviderError(t *testing.T) {
	m := testManager()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/oauth/callback?error=access_denied&error_description=User+denied", nil)

	m.CallbackHandler()(rec, req)

	if !strings.Contains(rec.Body.String(), "User denied") {
		t.Errorf("body = %q, want provider error message", rec.Body.String())
	}
}

func TestCallbackEscapesMessages(t *testing.T) {
	m := testManager()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/oauth/callback?error_description=%3Cscript%3Ealert(1)%3C/script%3E", nil)

	m.CallbackHandler()(rec, req)

	if strings.Contains(rec.Body.String(), "<script>") {
		t.Error("callback page did not escape the provider-supplied message")
	}
}

func TestRoutesMountBothEndpoints(t *testing.T) {
	m := testManager()
	mux := http.NewServeMux()
	m.Routes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/oauth/start", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("/oauth/start without ?connection= = %d, want 400", rec.Code)
	}

	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/oauth/callback", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Missing code or state") {
		t.Errorf("/oauth/callback empty = %d %q, want error page", rec.Code, rec.Body.String())
	}
}
