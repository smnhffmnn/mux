package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/smnhffmnn/mux/internal/config"
)

// TokenProvider holds an auth token that can be updated at runtime and knows
// which header to send it under. Safe for concurrent use (lock-free reads via
// atomic.Value). The header name is fixed at construction; only the token value
// is mutable.
type TokenProvider struct {
	token  atomic.Value // stores string
	header string       // header name; empty => "Authorization: Bearer <token>"
}

// NewTokenProvider creates a TokenProvider that sends the token as a bearer
// credential ("Authorization: Bearer <token>").
func NewTokenProvider(token string) *TokenProvider {
	return NewTokenProviderWithHeader(token, "")
}

// NewTokenProviderWithHeader creates a TokenProvider that sends the token under
// a custom header, verbatim (no scheme prefix). When header is empty it falls
// back to the bearer default. This mirrors the http connection type: a custom
// header carries the token as-is, so schemes other than Bearer (e.g. Basic) can
// be expressed by baking the scheme into the token value.
func NewTokenProviderWithHeader(token, header string) *TokenProvider {
	tp := &TokenProvider{header: header}
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
	tok := tp.token.Load().(string)
	if tp.header != "" {
		return map[string]string{tp.header: tok}
	}
	return map[string]string{"Authorization": "Bearer " + tok}
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
	Headers map[string]string // extra static headers sent with every request
	Token   *TokenProvider    // dynamic auth token (bearer by default, or a custom header)

	// HTTPClient, when set, is used for the upstream connection instead of the
	// default client. This is how a proxy reaches an MCP server that is only
	// routable through a tunnel: the client carries the tunnel's dialer.
	HTTPClient *http.Client

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
	} else {
		// Token and/or static-header transport. A dynamic token is read on each
		// request (so secret_set can rotate it live); extra static headers are
		// merged in. Register the provider so secret_set can find it.
		if m.Token != nil {
			registerProvider(m.Name, m.Token)
		}
		t, terr := buildTransport(m)
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
		// Mount names are human-readable display names — sanitize so the
		// result satisfies the strictest MCP client pattern
		// (^[a-zA-Z0-9_-]{1,64}$).
		prefixedName := config.SanitizeToolName(m.Name + "_" + tool.Name)

		// Create a copy of the tool with the prefixed name
		tool.Name = prefixedName

		// Capture the client and original name in the closure
		upstream := c
		origName := originalName

		s.AddTool(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			log.Printf("[tool] Call: %s → %s", prefixedName, m.URL)
			// Forward the call to the upstream server
			upstreamReq := mcp.CallToolRequest{}
			upstreamReq.Params.Name = origName
			upstreamReq.Params.Arguments = req.GetArguments()

			result, err := upstream.CallTool(ctx, upstreamReq)
			if err != nil {
				log.Printf("[tool] Error: %s — %v", prefixedName, err)
			}
			return result, err
		})
		count++
	}

	log.Printf("[proxy] mounted %s: %d tools from %s", m.Name, count, m.URL)
	return nil
}

// mergeHeaders returns a header func that overlays static extra headers onto the
// dynamic token header produced by base. The token header wins over a static
// header of the same name so auth stays token-managed; names are compared
// canonically (http.CanonicalHeaderKey) so a differently-cased duplicate (e.g.
// a static "authorization" against the token's "Authorization") can't slip
// through and win nondeterministically once net/http canonicalizes both. The
// result is computed per request so the token can be rotated live via secret_set.
func mergeHeaders(base transport.HTTPHeaderFunc, extra map[string]string) transport.HTTPHeaderFunc {
	return func(ctx context.Context) map[string]string {
		h := base(ctx)
		if len(extra) == 0 {
			return h
		}
		taken := make(map[string]struct{}, len(h))
		for k := range h {
			taken[http.CanonicalHeaderKey(k)] = struct{}{}
		}
		for k, v := range extra {
			if _, ok := taken[http.CanonicalHeaderKey(k)]; !ok {
				h[k] = v
			}
		}
		return h
	}
}

// buildTransport creates the appropriate MCP client transport for a mount,
// wiring auth (dynamic token and/or static headers) and an optional custom HTTP
// client (e.g. one carrying a tunnel dialer). URLs ending in /sse use SSE
// transport, everything else uses Streamable HTTP.
func buildTransport(m Mount) (transport.Interface, error) {
	var headerFunc transport.HTTPHeaderFunc
	if m.Token != nil {
		headerFunc = mergeHeaders(m.Token.HeaderFunc, m.Headers)
	}

	if strings.HasSuffix(strings.TrimRight(m.URL, "/"), "/sse") {
		var opts []transport.ClientOption
		if m.HTTPClient != nil {
			opts = append(opts, transport.WithHTTPClient(m.HTTPClient))
		}
		if headerFunc != nil {
			opts = append(opts, transport.WithHeaderFunc(headerFunc))
		} else if len(m.Headers) > 0 {
			opts = append(opts, transport.WithHeaders(m.Headers))
		}
		return transport.NewSSE(m.URL, opts...)
	}

	var opts []transport.StreamableHTTPCOption
	if m.HTTPClient != nil {
		opts = append(opts, transport.WithHTTPBasicClient(m.HTTPClient))
	}
	if headerFunc != nil {
		opts = append(opts, transport.WithHTTPHeaderFunc(headerFunc))
	} else if len(m.Headers) > 0 {
		opts = append(opts, transport.WithHTTPHeaders(m.Headers))
	}
	return transport.NewStreamableHTTP(m.URL, opts...)
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
