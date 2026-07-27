// Package bridge runs mux as a transparent stdio↔HTTP relay in front of an
// instance that is already serving MCP on this machine.
//
// Exactly one mux per machine should own the side-effectful resources: the
// WireGuard tunnels, the config file, and the vault. A second instance that
// builds its own copies does real damage rather than merely wasting memory —
// two userspace tunnels presenting the same WireGuard key fight over the peer's
// endpoint on the server (whichever sent traffic last owns the return path, and
// a persistent keepalive makes them trade it back and forth indefinitely), and
// two config writers overwrite each other last-writer-wins.
//
// So when a stdio invocation finds an instance already listening, it bridges to
// it instead of duplicating that ownership: the bridge fetches no provisioning,
// starts no tunnels, opens no vault, registers no connections and never writes
// config. Tool names and the owning instance's instructions pass through
// unchanged, so a client sees the same surface either way.
package bridge

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// logger writes to stderr rather than through the standard logger, which stdio
// mode deliberately discards to keep stdout clean for the MCP stream. The MCP
// stdio transport reserves stdout for protocol traffic and leaves stderr for
// logging, so the bridge can report what it decided without corrupting it —
// and a silent bridge is precisely what makes duplicate-instance problems hard
// to diagnose.
var logger = log.New(os.Stderr, "[bridge] ", log.LstdFlags)

// resyncTimeout bounds a tool-list refresh. Without it a hung upstream would
// stall refreshes forever, since the transport's HTTP client has no timeout.
const resyncTimeout = 10 * time.Second

// Upstream is an initialized connection to a mux instance serving MCP locally.
type Upstream struct {
	client       *client.Client
	url          string
	instructions string
	serverInfo   mcp.Implementation

	// clientIdentity is how we introduce ourselves, kept for re-handshakes.
	clientIdentity mcp.Implementation

	// resync carries refresh requests to the worker started by Serve. Buffered
	// with room for one: a full buffer means a refresh is already pending and
	// will observe this change too.
	resync chan struct{}

	// reinitMu serializes re-handshakes so a burst of failing calls produces
	// one attempt rather than one per call.
	reinitMu sync.Mutex
}

// DialOptions configures the upstream connection.
type DialOptions struct {
	// URL is the upstream MCP endpoint. It must resolve to a loopback address;
	// the bridge refuses anything else, including via redirect.
	URL string

	// ExpectServerName, when non-empty, requires the upstream to report this
	// name in its initialize response. Speaking MCP is not enough to be trusted
	// with a client's tool calls — some other MCP server on the port must make
	// the caller fall back to standalone rather than silently hand it the
	// client's traffic under mux's name.
	ExpectServerName string

	ClientName    string
	ClientVersion string
}

// Dial connects to the MCP endpoint in opts and completes the initialize
// handshake.
//
// The handshake doubles as the probe. A plain TCP connect would only prove that
// something holds the port; requiring a successful initialize — and, with
// ExpectServerName set, the right server identity — means a foreign occupant
// makes this fail cleanly and the caller runs standalone instead.
//
// Pass a ctx with a short timeout: this sits in the startup path of the calling
// MCP client. The timeout applies to the handshake only, not to the lifetime of
// the returned connection.
func Dial(ctx context.Context, opts DialOptions) (*Upstream, error) {
	t, err := transport.NewStreamableHTTP(opts.URL,
		transport.WithHTTPBasicClient(loopbackClient()),
		// Without this the client opens no standalone GET stream, and
		// server-initiated notifications only arrive piggybacked on a request
		// the bridge happens to make itself — so tool-list changes on the
		// owning instance would go unnoticed until the next tool call.
		transport.WithContinuousListening(),
	)
	if err != nil {
		return nil, fmt.Errorf("create transport: %w", err)
	}

	c := client.NewClient(t)

	// Start owns the notification listener's lifetime, so it must not inherit
	// the handshake deadline — the listener has to outlive it and is stopped by
	// Close instead.
	if err := c.Start(context.WithoutCancel(ctx)); err != nil {
		return nil, fmt.Errorf("start client: %w", err)
	}

	res, err := c.Initialize(ctx, initRequest(clientIdentity(opts)))
	if err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("initialize: %w", err)
	}
	if want := opts.ExpectServerName; want != "" && res.ServerInfo.Name != want {
		_ = c.Close()
		return nil, fmt.Errorf("upstream at %s reports itself as %q, not %q", opts.URL, res.ServerInfo.Name, want)
	}

	return &Upstream{
		client:         c,
		url:            opts.URL,
		instructions:   res.Instructions,
		serverInfo:     res.ServerInfo,
		clientIdentity: clientIdentity(opts),
		resync:         make(chan struct{}, 1),
	}, nil
}

func clientIdentity(opts DialOptions) mcp.Implementation {
	return mcp.Implementation{Name: opts.ClientName, Version: opts.ClientVersion}
}

func initRequest(who mcp.Implementation) mcp.InitializeRequest {
	return mcp.InitializeRequest{
		Params: mcp.InitializeParams{
			ClientInfo:      who,
			ProtocolVersion: mcp.LATEST_PROTOCOL_VERSION,
		},
	}
}

// loopbackClient returns an HTTP client that can only reach loopback addresses
// and never follows redirects.
//
// The endpoint is built from a port number, so the URL itself cannot name a
// foreign host — but Go's default client follows up to ten redirects and
// replays the POST body on 307/308. A local listener answering with a redirect
// could therefore forward every tool call, including vault_unlock's passphrase
// and secret_set's values, to an off-machine host. Pinning the dialer keeps the
// loopback invariant the endpoint is constructed to guarantee.
func loopbackClient() *http.Client {
	dialer := &net.Dialer{}
	return &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				host, _, err := net.SplitHostPort(addr)
				if err != nil {
					return nil, fmt.Errorf("split upstream address %q: %w", addr, err)
				}
				ip := net.ParseIP(host)
				if ip == nil || !ip.IsLoopback() {
					return nil, fmt.Errorf("refusing non-loopback upstream %q", addr)
				}
				return dialer.DialContext(ctx, network, addr)
			},
		},
	}
}

// URL returns the upstream endpoint this bridge is attached to.
func (u *Upstream) URL() string { return u.url }

// ServerInfo returns the upstream's reported name and version.
func (u *Upstream) ServerInfo() mcp.Implementation { return u.serverInfo }

// Close releases the upstream connection, which also terminates the upstream's
// session for it rather than leaving it parked in the long-lived instance.
func (u *Upstream) Close() error { return u.client.Close() }

// Serve mirrors the upstream's tools onto a stdio MCP server and blocks until
// stdin closes.
func (u *Upstream) Serve(ctx context.Context, name, version string) error {
	mirrorCtx, stopMirror := context.WithCancel(ctx)
	defer stopMirror()

	s, err := u.mirror(mirrorCtx, name, version)
	if err != nil {
		return err
	}
	return server.ServeStdio(s)
}

// mirror builds the local MCP server, populates it from the upstream and starts
// tracking upstream changes. Split out from Serve so the wiring can be exercised
// without a stdio transport. Tracking stops when ctx is cancelled.
func (u *Upstream) mirror(ctx context.Context, name, version string) (*server.MCPServer, error) {
	s := server.NewMCPServer(name, version,
		server.WithToolCapabilities(true),
		server.WithRecovery(),
		server.WithInstructions(u.instructions),
	)

	syncCtx, cancelSync := context.WithTimeout(ctx, resyncTimeout)
	err := u.syncTools(syncCtx, s)
	cancelSync()
	if err != nil {
		return nil, err
	}

	go u.resyncWorker(ctx, s)

	// The upstream's tool set changes while we are attached — connection_add, a
	// provisioning sync, or a vault unlock that retries skipped connections all
	// add or remove tools. Mirror those changes instead of serving the startup
	// snapshot for the life of the process.
	u.client.OnNotification(func(n mcp.JSONRPCNotification) {
		if n.Method != string(mcp.MethodNotificationToolsListChanged) {
			return
		}
		u.requestResync()
	})

	return s, nil
}

// requestResync asks the worker for a tool-list refresh without blocking.
//
// Notification handlers run inline on the transport's read loop, so doing the
// round-trip here would stall the very stream a concurrent tool call is waiting
// on. Handing it to the worker also serializes refreshes, which keeps two
// overlapping list responses from landing out of order and pinning a stale set.
func (u *Upstream) requestResync() {
	select {
	case u.resync <- struct{}{}:
	default:
	}
}

func (u *Upstream) resyncWorker(ctx context.Context, s *server.MCPServer) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-u.resync:
			syncCtx, cancel := context.WithTimeout(ctx, resyncTimeout)
			if err := u.syncTools(syncCtx, s); err != nil {
				logger.Printf("tool re-sync failed: %v", err)
			}
			cancel()
		}
	}
}

// syncTools replaces the local tool set with the upstream's current one.
// Names are passed through verbatim: a client must see identical tool names
// whether it reached mux through a bridge or through its own instance.
func (u *Upstream) syncTools(ctx context.Context, s *server.MCPServer) error {
	res, err := u.client.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		return fmt.Errorf("list tools: %w", err)
	}
	mirrored := make([]server.ServerTool, 0, len(res.Tools))
	for _, tool := range res.Tools {
		mirrored = append(mirrored, server.ServerTool{Tool: tool, Handler: u.forward(tool.Name)})
	}
	// SetTools replaces the set rather than adding to it, so a tool removed
	// upstream disappears here too instead of lingering as a dead entry.
	s.SetTools(mirrored...)
	logger.Printf("mirroring %d tools from %s", len(mirrored), u.url)
	return nil
}

// forward relays a call to the upstream instance under its original name.
func (u *Upstream) forward(name string) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		res, err := u.client.CallTool(ctx, u.upstreamCall(name, req))
		if err == nil {
			return res, nil
		}

		// A restarted owner leaves us holding a session it has forgotten. The
		// transport drops the stale ID but only re-initializes for initialize
		// itself, so without this every later call fails for the life of the
		// process while the mirrored tool list still looks healthy.
		if errors.Is(err, transport.ErrSessionTerminated) {
			logger.Printf("upstream session gone, re-initializing")
			if reErr := u.reinitialize(ctx); reErr != nil {
				logger.Printf("re-initialize failed: %v", reErr)
				return res, err
			}
			u.requestResync()
			res, err = u.client.CallTool(ctx, u.upstreamCall(name, req))
			if err == nil {
				return res, nil
			}
		}

		logger.Printf("call %s failed: %v", name, err)
		return res, err
	}
}

// upstreamCall builds the outgoing request for a mirrored tool.
//
// Arguments are forwarded raw rather than through GetArguments, which yields
// nil for anything that is not a JSON object and would drop such a call's
// arguments silently. Meta rides along so progress tokens keep working.
func (u *Upstream) upstreamCall(name string, req mcp.CallToolRequest) mcp.CallToolRequest {
	var out mcp.CallToolRequest
	out.Params.Name = name
	out.Params.Arguments = req.GetRawArguments()
	out.Params.Meta = req.Params.Meta
	return out
}

// reinitialize re-runs the handshake after the upstream forgot our session.
func (u *Upstream) reinitialize(ctx context.Context) error {
	u.reinitMu.Lock()
	defer u.reinitMu.Unlock()

	_, err := u.client.Initialize(ctx, initRequest(u.clientIdentity))
	return err
}
