package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/server"

	"github.com/smnhffmnn/mux/internal/config"
)

// TestHeadlessLocalRoutesServeOAuth checks that the route set headless mode
// hands to startHTTPServer contains the OAuth endpoints. The build this test
// exists for is the notray one (CI runs it with -tags notray): before the
// flow moved to internal/oauth it sat in the tag-guarded app.go, and a notray
// binary mounted nothing under /oauth/* — every request there was a 404 and
// an OAuth proxy could never leave "awaiting oauth login".
func TestHeadlessLocalRoutesServeOAuth(t *testing.T) {
	cfg := &config.Config{}
	cfg.Server.Port = 7700
	s := server.NewMCPServer("mux-test", "0.0.0")

	mux := http.NewServeMux()
	headlessLocalRoutes(cfg, newSimpleReloader(s, cfg, nil), nil)(mux)

	cases := []struct {
		name   string
		path   string
		status int
		body   string
	}{
		// The handler answers, not the mux: a 404 here would mean the route is
		// not mounted at all.
		{"start without connection", "/oauth/start", http.StatusBadRequest, "missing ?connection= parameter"},
		{"start for unknown connection", "/oauth/start?connection=nope", http.StatusInternalServerError, "OAuth not supported for: nope"},
		{"callback without code", "/oauth/callback", http.StatusOK, "Missing code or state"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))
			if rec.Code != tc.status {
				t.Fatalf("GET %s = %d, want %d (body %q)", tc.path, rec.Code, tc.status, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.body) {
				t.Fatalf("GET %s body = %q, want it to contain %q", tc.path, rec.Body.String(), tc.body)
			}
		})
	}
}
