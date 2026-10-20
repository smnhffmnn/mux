package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/zalando/go-keyring"

	"github.com/smnhffmnn/mux/internal/config"
)

// isolateSecretStore keeps a test's secrets out of the developer's keychain
// and secrets.toml: the keyring becomes an in-memory mock and the config
// directory, where the file store lives, a temporary one.
func isolateSecretStore(t *testing.T) {
	t.Helper()
	keyring.MockInit()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "mux"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", dir)
}

// toolsWithPrefix lists the gateway's tools that belong to a connection.
func toolsWithPrefix(s *server.MCPServer, conn string) []string {
	prefix := config.SanitizeToolName(conn) + "_"
	var names []string
	for name := range s.ListTools() {
		if strings.HasPrefix(name, prefix) {
			names = append(names, name)
		}
	}
	return names
}

// TestOAuthAuthorizedReloadTracksMountedTools covers the step after a
// successful OAuth callback: the proxy is mounted through the reloader, so
// its tools are recorded like every other mount and UnloadConnection (what
// connection_delete calls) removes them again. The old post-authorization
// path registered the mount directly and left the tools in place until a
// restart.
func TestOAuthAuthorizedReloadTracksMountedTools(t *testing.T) {
	isolateSecretStore(t)

	// An upstream MCP server with one tool, served over streamable HTTP the
	// way a provider's server is.
	upstream := server.NewMCPServer("upstream", "0.0.0")
	upstream.AddTool(mcp.NewTool("ping"), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultText("pong"), nil
	})
	upMux := http.NewServeMux()
	upMux.Handle("/mcp", server.NewStreamableHTTPServer(upstream))
	srv := httptest.NewServer(upMux)
	defer srv.Close()

	const name = "oauth-up"
	cfg := &config.Config{Connections: []config.Connection{
		{Name: name, Type: config.TypeProxy, URL: srv.URL + "/mcp", OAuth: true},
	}}
	cfg.Server.Port = 7700

	// The token the callback has stored by the time onAuthorized runs.
	tok, err := json.Marshal(transport.Token{AccessToken: "test", TokenType: "Bearer", ExpiresAt: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if err := config.NewKeychainTokenStore(name).SaveRawToken(tok); err != nil {
		t.Fatalf("store token: %v", err)
	}

	s := server.NewMCPServer("mux-test", "0.0.0")
	reloader := newSimpleReloader(s, cfg, nil)

	oauthAuthorizedReload(cfg, reloader)(name)

	// The reload runs in the background; the callback must not block the
	// provider's redirect.
	deadline := time.Now().Add(10 * time.Second)
	for len(toolsWithPrefix(s, name)) == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	mounted := toolsWithPrefix(s, name)
	if len(mounted) == 0 {
		t.Fatal("no tools mounted after authorization")
	}
	if !strings.Contains(strings.Join(mounted, ","), "ping") {
		t.Fatalf("mounted tools %v, want the upstream's ping", mounted)
	}

	reloader.UnloadConnection(name)
	if left := toolsWithPrefix(s, name); len(left) != 0 {
		t.Fatalf("tools still registered after UnloadConnection: %v", left)
	}
}

type recordingReloader struct{ reloaded chan string }

func (r *recordingReloader) ReloadConnection(c config.Connection) { r.reloaded <- c.Name }
func (r *recordingReloader) UnloadConnection(string)              {}

// TestOAuthAuthorizedReloadIgnoresUnknownConnection: a callback for a name
// the config does not know (the connection was deleted while the flow was
// open) reloads nothing.
func TestOAuthAuthorizedReloadIgnoresUnknownConnection(t *testing.T) {
	cfg := &config.Config{Connections: []config.Connection{
		{Name: "known", Type: config.TypeProxy, URL: "https://mcp.example/mcp", OAuth: true},
	}}
	r := &recordingReloader{reloaded: make(chan string, 1)}
	cb := oauthAuthorizedReload(cfg, r)

	cb("unknown")
	select {
	case got := <-r.reloaded:
		t.Fatalf("reloaded %q for an unknown connection", got)
	case <-time.After(100 * time.Millisecond):
	}

	cb("known")
	select {
	case got := <-r.reloaded:
		if got != "known" {
			t.Fatalf("reloaded %q, want known", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("known connection was not reloaded")
	}
}
