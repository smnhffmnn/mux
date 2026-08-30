package oauth

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestExtractParam(t *testing.T) {
	header := `Bearer realm="mcp", resource_metadata="https://mcp.example/.well-known/oauth-protected-resource"`
	if got := extractParam(header, "resource_metadata"); got != "https://mcp.example/.well-known/oauth-protected-resource" {
		t.Errorf("extractParam = %q", got)
	}
	if got := extractParam(header, "missing"); got != "" {
		t.Errorf("extractParam(missing) = %q, want empty", got)
	}
	if got := extractParam(`Bearer resource_metadata="unterminated`, "resource_metadata"); got != "" {
		t.Errorf("extractParam(unterminated) = %q, want empty", got)
	}
}

func TestDiscoverMetadataFollowsResourceHint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/mcp":
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer resource_metadata="http://%s/meta"`, r.Host))
			w.WriteHeader(http.StatusUnauthorized)
		case "/meta":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"authorization_servers":["https://auth.example"]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	got, err := DiscoverMetadata(context.Background(), srv.URL+"/mcp")
	if err != nil {
		t.Fatal(err)
	}
	want := "https://auth.example/.well-known/oauth-authorization-server"
	if got != want {
		t.Errorf("DiscoverMetadata = %q, want %q", got, want)
	}
}

func TestDiscoverMetadataFallsBackToOrigin(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized) // no WWW-Authenticate hint
	}))
	defer srv.Close()

	got, err := DiscoverMetadata(context.Background(), srv.URL+"/mcp")
	if err != nil {
		t.Fatal(err)
	}
	want := srv.URL + "/.well-known/oauth-authorization-server"
	if got != want {
		t.Errorf("DiscoverMetadata = %q, want %q", got, want)
	}
}
