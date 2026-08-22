package oauth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
