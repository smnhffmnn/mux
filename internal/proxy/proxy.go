package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/smnhffmnn/mux/internal/config"
)

// TokenProvider holds a bearer token that can be updated at runtime.
// Safe for concurrent use (lock-free reads via atomic.Value).
type TokenProvider struct {
	token atomic.Value // stores string
}

// NewTokenProvider creates a TokenProvider with an initial token.
func NewTokenProvider(token string) *TokenProvider {
	tp := &TokenProvider{}
	tp.token.Store(token)
	return tp
}

// Set updates the token. The next request will use the new value.
func (tp *TokenProvider) Set(token string) {
	tp.token.Store(token)
}

// HeaderFunc returns an authorization header map with the current token.
// Signature matches transport.HTTPHeaderFunc.
func (tp *TokenProvider) HeaderFunc(_ context.Context) map[string]string {
	return map[string]string{"Authorization": "Bearer " + tp.token.Load().(string)}
}

// provider registry — allows secret_set to find active TokenProviders by connection name.
var (
	providersMu sync.RWMutex
	providers   = make(map[string]*TokenProvider)
)

// GetTokenProvider returns the active TokenProvider for a connection, or nil.
func GetTokenProvider(name string) *TokenProvider {
	providersMu.RLock()
	defer providersMu.RUnlock()
	return providers[name]
}

func registerProvider(name string, tp *TokenProvider) {
	providersMu.Lock()
	providers[name] = tp
	providersMu.Unlock()
}

// Mount describes a remote MCP server to proxy through the gateway.
type Mount struct {
	Name    string
	URL     string
	Headers map[string]string // static auth headers (legacy)
	Token   *TokenProvider    // dynamic bearer token (preferred for bearer proxies)

	// OAuth fields (mutually exclusive with Headers/Token auth)
	OAuth      *transport.OAuthConfig
	TokenStore *config.KeychainTokenStore // used to persist tokens to keychain
}

// RegisterMounts connects to each upstream MCP server, discovers its tools,
// and re-exports them on the gateway server with a namespace prefix.
func RegisterMounts(ctx context.Context, s *server.MCPServer, mounts []Mount) {
	for _, m := range mounts {
		if err := RegisterMount(ctx, s, m); err != nil {
			log.Printf("[proxy] skipping %s: %v", m.Name, err)
		}
	}
}

// RegisterMount connects to a single upstream MCP server and registers its tools.
func RegisterMount(ctx context.Context, s *server.MCPServer, m Mount) error {
	var c *client.Client
	var err error

	if m.OAuth != nil {
		// OAuth transport — mcp-go handles token refresh automatically
		c, err = client.NewOAuthStreamableHttpClient(m.URL, *m.OAuth)
		if err != nil {
			return fmt.Errorf("create OAuth transport for %s: %w", m.Name, err)
		}
	} else if m.Token != nil {
		// Dynamic bearer token — reads current token on each request
		registerProvider(m.Name, m.Token)
		t, terr := newTransportDynamic(m.URL, m.Token.HeaderFunc)
		if terr != nil {
			return fmt.Errorf("create transport for %s: %w", m.Name, terr)
		}
		c = client.NewClient(t)
	} else {
		// Static header transport
		t, terr := newTransport(m.URL, m.Headers)
		if terr != nil {
			return fmt.Errorf("create transport for %s: %w", m.Name, terr)
		}
		c = client.NewClient(t)
	}

	if err := c.Start(ctx); err != nil {
		return fmt.Errorf("start client for %s: %w", m.Name, err)
	}

	_, err = c.Initialize(ctx, mcp.InitializeRequest{
		Params: mcp.InitializeParams{
			ClientInfo: mcp.Implementation{
				Name:    "mux",
				Version: "1.0.0",
			},
			ProtocolVersion: mcp.LATEST_PROTOCOL_VERSION,
		},
	})
	if err != nil {
		_ = c.Close()
		return fmt.Errorf("initialize %s: %w", m.Name, err)
	}

	toolsResult, err := c.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		_ = c.Close()
		return fmt.Errorf("list tools from %s: %w", m.Name, err)
	}

	count := 0
	for _, tool := range toolsResult.Tools {
		originalName := tool.Name
		prefixedName := m.Name + "_" + tool.Name

		// Create a copy of the tool with the prefixed name
		tool.Name = prefixedName

		// Capture the client and original name in the closure
		upstream := c
		origName := originalName

		s.AddTool(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			// Forward the call to the upstream server
			upstreamReq := mcp.CallToolRequest{}
			upstreamReq.Params.Name = origName
			upstreamReq.Params.Arguments = req.GetArguments()

			return upstream.CallTool(ctx, upstreamReq)
		})
		count++
	}

	log.Printf("[proxy] mounted %s: %d tools from %s", m.Name, count, m.URL)
	return nil
}

// newTransport creates the appropriate MCP client transport based on URL.
// URLs ending in /sse use SSE transport, everything else uses Streamable HTTP.
func newTransport(url string, headers map[string]string) (transport.Interface, error) {
	if strings.HasSuffix(strings.TrimRight(url, "/"), "/sse") {
		return transport.NewSSE(url, transport.WithHeaders(headers))
	}
	return transport.NewStreamableHTTP(url, transport.WithHTTPHeaders(headers))
}

// newTransportDynamic creates a transport that calls headerFunc on each request.
func newTransportDynamic(url string, headerFunc transport.HTTPHeaderFunc) (transport.Interface, error) {
	if strings.HasSuffix(strings.TrimRight(url, "/"), "/sse") {
		return transport.NewSSE(url, transport.WithHeaderFunc(headerFunc))
	}
	return transport.NewStreamableHTTP(url, transport.WithHTTPHeaderFunc(headerFunc))
}

// keychainTokenAdapter wraps KeychainTokenStore to implement transport.TokenStore.
// It bridges between the config package (raw JSON) and the transport package (*transport.Token).
type keychainTokenAdapter struct {
	store *config.KeychainTokenStore
}

// NewKeychainTokenAdapter creates a transport.TokenStore backed by the OS keychain.
func NewKeychainTokenAdapter(store *config.KeychainTokenStore) transport.TokenStore {
	return &keychainTokenAdapter{store: store}
}

func (a *keychainTokenAdapter) GetToken(ctx context.Context) (*transport.Token, error) {
	raw, err := a.store.GetRawToken()
	if err != nil {
		return nil, transport.ErrNoToken
	}
	var token transport.Token
	if err := json.Unmarshal(raw, &token); err != nil {
		return nil, fmt.Errorf("unmarshal token: %w", err)
	}
	return &token, nil
}

func (a *keychainTokenAdapter) SaveToken(ctx context.Context, token *transport.Token) error {
	data, err := json.Marshal(token)
	if err != nil {
		return fmt.Errorf("marshal token: %w", err)
	}
	return a.store.SaveRawToken(data)
}
