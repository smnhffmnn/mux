package bridge

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

const testServerName = "mux"

// newUpstream starts an in-process MCP server behind an HTTP test server,
// mounted at /mcp exactly as mux mounts it in production. The returned server
// handle lets a test change the upstream's tool set mid-flight.
func newUpstream(t *testing.T, name, instructions string) (*server.MCPServer, string) {
	t.Helper()

	s := server.NewMCPServer(name, "9.9.9",
		server.WithToolCapabilities(true),
		server.WithInstructions(instructions),
	)
	s.AddTool(
		mcp.NewTool("echo", mcp.WithDescription("echoes msg"), mcp.WithString("msg", mcp.Required())),
		func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return mcp.NewToolResultText("echo:" + req.GetString("msg", "")), nil
		},
	)

	mux := http.NewServeMux()
	mux.Handle("/mcp", server.NewStreamableHTTPServer(s))
	httpSrv := httptest.NewServer(mux)
	t.Cleanup(httpSrv.Close)

	return s, httpSrv.URL + "/mcp"
}

func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func dialTest(t *testing.T, url string) *Upstream {
	t.Helper()
	up, err := Dial(testCtx(t), DialOptions{
		URL:              url,
		ExpectServerName: testServerName,
		ClientName:       "mux-bridge",
		ClientVersion:    "test",
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = up.Close() })
	return up
}

// toolNames returns the tool names currently registered on a server.
func toolNames(s *server.MCPServer) map[string]bool {
	out := map[string]bool{}
	for name := range s.ListTools() {
		out[name] = true
	}
	return out
}

// waitForTools polls until the server's tool set matches want, or fails.
// The mirror updates asynchronously via a notification, so a test cannot read
// it immediately after changing the upstream.
func waitForTools(t *testing.T, s *server.MCPServer, want map[string]bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var got map[string]bool
	for time.Now().Before(deadline) {
		got = toolNames(s)
		if len(got) == len(want) {
			match := true
			for name := range want {
				if !got[name] {
					match = false
					break
				}
			}
			if match {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("mirrored tools = %v, want %v (timed out)", got, want)
}

func TestDial(t *testing.T) {
	t.Run("carries the upstream's instructions and identity", func(t *testing.T) {
		// The instructions are the reason the bridge cannot just mirror tools:
		// mux builds them at runtime from its connections, and a client that
		// loses them loses its per-connection usage guidance.
		_, url := newUpstream(t, testServerName, "upstream instructions")
		up := dialTest(t, url)

		if up.instructions != "upstream instructions" {
			t.Fatalf("instructions = %q, want the upstream's own instructions", up.instructions)
		}
		if got := up.ServerInfo().Name; got != testServerName {
			t.Fatalf("ServerInfo().Name = %q, want %q", got, testServerName)
		}
		if up.URL() != url {
			t.Fatalf("URL() = %q, want %q", up.URL(), url)
		}
	})

	t.Run("rejects an MCP server that is not the expected one", func(t *testing.T) {
		// Speaking MCP is not enough. Some other gateway on the port must make
		// the caller run standalone, not silently receive the client's tool
		// calls — including vault passphrases — under mux's name.
		_, url := newUpstream(t, "some-other-gateway", "")

		_, err := Dial(testCtx(t), DialOptions{
			URL:              url,
			ExpectServerName: testServerName,
			ClientName:       "mux-bridge",
			ClientVersion:    "test",
		})
		if err == nil {
			t.Fatal("Dial accepted a foreign MCP server, want an error so the caller runs standalone")
		}
	})

	t.Run("accepts any server when no name is expected", func(t *testing.T) {
		_, url := newUpstream(t, "some-other-gateway", "")

		up, err := Dial(testCtx(t), DialOptions{URL: url, ClientName: "mux-bridge", ClientVersion: "test"})
		if err != nil {
			t.Fatalf("Dial with no ExpectServerName failed: %v", err)
		}
		_ = up.Close()
	})

	t.Run("rejects a non-MCP service holding the port", func(t *testing.T) {
		// This is why the probe is a full initialize handshake and not a TCP
		// connect: an unrelated service on the port must fail cleanly.
		foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("I am not an MCP server"))
		}))
		t.Cleanup(foreign.Close)

		if _, err := Dial(testCtx(t), DialOptions{URL: foreign.URL + "/mcp", ClientName: "c", ClientVersion: "t"}); err == nil {
			t.Fatal("Dial succeeded against a non-MCP HTTP service, want an error")
		}
	})

	t.Run("refuses to follow a redirect off the machine", func(t *testing.T) {
		// A local listener that answers with a redirect would otherwise relay
		// every tool call — vault passphrases included — to a remote host,
		// because Go replays the POST body on 307/308.
		_, realURL := newUpstream(t, testServerName, "")
		redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, realURL, http.StatusTemporaryRedirect)
		}))
		t.Cleanup(redirector.Close)

		if _, err := Dial(testCtx(t), DialOptions{URL: redirector.URL + "/mcp", ClientName: "c", ClientVersion: "t"}); err == nil {
			t.Fatal("Dial followed a redirect, want an error — the upstream must stay on loopback")
		}
	})

	t.Run("fails when nothing is listening", func(t *testing.T) {
		probe := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		closedURL := probe.URL
		probe.Close()

		if _, err := Dial(testCtx(t), DialOptions{URL: closedURL + "/mcp", ClientName: "c", ClientVersion: "t"}); err == nil {
			t.Fatal("Dial succeeded against a closed port, want an error")
		}
	})
}

func TestMirrorRegistersToolsVerbatim(t *testing.T) {
	// Tool names must pass through unprefixed — unlike proxy mounts, which
	// namespace upstream tools. A client has to see the same names whether it
	// reached mux through a bridge or through its own instance, otherwise
	// switching to the bridge silently renames every tool.
	_, url := newUpstream(t, testServerName, "instructions")
	ctx := testCtx(t)
	up := dialTest(t, url)

	local, err := up.mirror(ctx, "mux", "test")
	if err != nil {
		t.Fatalf("mirror: %v", err)
	}

	got := toolNames(local)
	if len(got) != 1 || !got["echo"] {
		t.Fatalf("mirrored tools = %v, want exactly the unprefixed upstream name {echo}", got)
	}

	// The registered handler — not just forward() in isolation — must reach the
	// upstream tool and carry the arguments there.
	handler := local.ListTools()["echo"].Handler
	var req mcp.CallToolRequest
	req.Params.Name = "echo"
	req.Params.Arguments = map[string]any{"msg": "hi"}

	out, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("mirrored handler failed: %v", err)
	}
	if out == nil || len(out.Content) == 0 {
		t.Fatal("mirrored handler returned no content")
	}
	text, ok := out.Content[0].(mcp.TextContent)
	if !ok {
		t.Fatalf("content type = %T, want mcp.TextContent", out.Content[0])
	}
	if text.Text != "echo:hi" {
		t.Fatalf("result = %q, want %q — arguments did not reach the upstream tool", text.Text, "echo:hi")
	}
}

func TestMirrorTracksUpstreamToolChanges(t *testing.T) {
	// The bridge must not serve a startup snapshot for the life of the process:
	// the owning instance gains and loses tools at runtime (connection_add, a
	// provisioning sync, a vault unlock). This only works because the transport
	// keeps a notification stream open — without it the bridge would never hear
	// about a change it did not itself trigger, which is easy to regress and
	// invisible at runtime.
	upstreamSrv, url := newUpstream(t, testServerName, "")
	ctx := testCtx(t)
	up := dialTest(t, url)

	local, err := up.mirror(ctx, "mux", "test")
	if err != nil {
		t.Fatalf("mirror: %v", err)
	}
	waitForTools(t, local, map[string]bool{"echo": true})

	// A tool appearing upstream must appear here.
	upstreamSrv.AddTool(
		mcp.NewTool("added", mcp.WithDescription("added later")),
		func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return mcp.NewToolResultText("added"), nil
		},
	)
	waitForTools(t, local, map[string]bool{"echo": true, "added": true})

	// And a tool removed upstream must disappear rather than linger as a dead
	// entry that fails when called — this is what SetTools buys over AddTools.
	upstreamSrv.DeleteTools("echo")
	waitForTools(t, local, map[string]bool{"added": true})
}
