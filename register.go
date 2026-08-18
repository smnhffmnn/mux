package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/server"

	"github.com/smnhffmnn/mux/internal/config"
	"github.com/smnhffmnn/mux/internal/provisioning"
	"github.com/smnhffmnn/mux/internal/proxy"
	"github.com/smnhffmnn/mux/internal/tools"
	"github.com/smnhffmnn/mux/internal/tunnel"
	"github.com/smnhffmnn/mux/internal/wireguard"
)

var version = "dev"
var buildTime = ""

// tunnelManager wraps WireGuard and SSH tunnels behind a common Dialer interface.
type tunnelManager struct {
	wg  *wireguard.Manager
	ssh map[string]*tunnel.SSHTunnel
}

func newTunnelManager(wgMgr *wireguard.Manager) *tunnelManager {
	return &tunnelManager{wg: wgMgr, ssh: make(map[string]*tunnel.SSHTunnel)}
}

// StartSSH creates and starts SSH tunnels from the given configs.
// Returns errors keyed by tunnel name for any that failed.
func (tm *tunnelManager) StartSSH(tunnels []config.TunnelConfig) map[string]error {
	errs := make(map[string]error)
	for _, cfg := range tunnels {
		if !cfg.IsSSH() || !cfg.Enabled() {
			continue
		}
		t, err := tunnel.NewSSH(cfg)
		if err != nil {
			errs[cfg.Name] = err
			log.Printf("[ssh] Tunnel %q failed: %v", cfg.Name, err)
			continue
		}
		tm.ssh[cfg.Name] = t
		log.Printf("[ssh] Tunnel %q started (%s@%s:%d)", cfg.Name, cfg.User, cfg.Host, cfg.Port)
	}
	return errs
}

// Get returns a Dialer for the named tunnel (WireGuard or SSH), or nil.
func (tm *tunnelManager) Get(name string) tools.Dialer {
	if t := tm.wg.Get(name); t != nil {
		return t
	}
	if t, ok := tm.ssh[name]; ok {
		return t
	}
	return nil
}

// StartMissing starts every tunnel in the list that isn't registered yet
// (WireGuard and SSH alike); already-known tunnels are left untouched. This
// lets a provisioning sync pass the full tunnel list after new entries
// appeared — without it, tunnels provisioned at runtime would only ever
// start on the next full mux restart. Returns errors keyed by tunnel name.
func (tm *tunnelManager) StartMissing(tunnels []config.TunnelConfig) map[string]error {
	var wgNew, sshNew []config.TunnelConfig
	for _, t := range tunnels {
		if tm.Get(t.Name) != nil {
			continue
		}
		if t.IsSSH() {
			sshNew = append(sshNew, t)
		} else {
			wgNew = append(wgNew, t)
		}
	}
	errs := make(map[string]error)
	if len(wgNew) > 0 {
		for name, err := range tm.wg.Start(wgNew) {
			errs[name] = err
		}
	}
	for name, err := range tm.StartSSH(sshNew) {
		errs[name] = err
	}
	return errs
}

// IsUp reports whether the named tunnel is running.
func (tm *tunnelManager) IsUp(name string) bool {
	if tm.wg.IsUp(name) {
		return true
	}
	if t, ok := tm.ssh[name]; ok {
		return t.IsUp()
	}
	return false
}

// Close shuts down all SSH tunnels. WireGuard tunnels are closed via wg.Manager.
func (tm *tunnelManager) Close() {
	for name, t := range tm.ssh {
		log.Printf("[ssh] Closing tunnel %q", name)
		t.Close()
	}
}

// registerConnections registers native database tools for each configured connection.
// Fail-closed: if a connection references a tunnel that isn't available, it's skipped.
func registerConnections(s *server.MCPServer, cfg *config.Config, tm *tunnelManager) {
	for _, conn := range cfg.AllConnections() {
		if config.IsProxyType(conn.Type) {
			continue // handled by registerProxies
		}
		if !conn.Enabled() {
			continue
		}

		// Resolve tunnel dialer (fail-closed)
		var dialer tools.Dialer
		if conn.Tunnel != "" {
			dialer = tm.Get(conn.Tunnel)
			if dialer == nil {
				log.Printf("[mux] Skipping %q: tunnel %q not available", conn.Name, conn.Tunnel)
				continue
			}
		}

		if _, _, err := tools.RegisterConnection(s, conn, dialer); err != nil {
			log.Printf("[mux] Warning: %s not available: %v", conn.Name, err)
		}
	}
}

// proxyTunnelClient returns an HTTP client whose connections are dialed through
// the given tunnel dialer. A proxy to an MCP server that is only routable inside
// a tunnel (an internal host) needs this — without it the upstream connection
// uses the default network namespace and never reaches the host. TLS
// verification is skipped, matching the http connection type, because tunneled
// endpoints are internal and often present certificates for private names.
func proxyTunnelClient(dialer tools.Dialer) *http.Client {
	tr := &http.Transport{
		ResponseHeaderTimeout: 30 * time.Second,
		TLSClientConfig:       &tls.Config{InsecureSkipVerify: true},
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, addr)
		},
	}
	// Deliberately no http.Client.Timeout (unlike the http connection type): MCP
	// Streamable-HTTP/SSE connections are long-lived streams that a whole-request
	// deadline would sever. ResponseHeaderTimeout bounds the initial response only.
	return &http.Client{Transport: tr}
}

// oauthProxyMount builds the OAuth proxy mount for a connection. Every path
// that mounts or probes an OAuth proxy — startup registration, vault-unlock
// retry, hot-reload, post-authorization mounting, the test button — must go
// through this builder so the transport construction cannot drift apart.
func oauthProxyMount(name, url string, port int, tokenStore *config.KeychainTokenStore) proxy.Mount {
	adapter := proxy.NewKeychainTokenAdapter(tokenStore)
	clientID, clientSecret := config.LoadOAuthClientID(name)
	return proxy.Mount{
		Name:       name,
		URL:        url,
		TokenStore: tokenStore,
		OAuth: &transport.OAuthConfig{
			ClientID:     clientID,
			ClientSecret: clientSecret,
			RedirectURI:  fmt.Sprintf("http://localhost:%d/oauth/callback", port),
			TokenStore:   adapter,
			PKCEEnabled:  true,
		},
	}
}

// newProxyTokenMount builds a non-OAuth proxy mount for a connection, honouring
// a custom token header and extra static headers, and routing through the
// connection's tunnel when one is set. It is fail-closed on tunnels: if the
// connection references a tunnel that is not available, ok is false and no mount
// should be attempted (a direct connection would bypass the tunnel). token is
// passed explicitly because callers resolve it from different places (in-memory
// config vs. the vault).
func newProxyTokenMount(conn config.Connection, token string, tm *tunnelManager) (proxy.Mount, bool) {
	m := proxy.Mount{
		Name:    conn.Name,
		URL:     conn.URL,
		Headers: conn.Headers,
	}
	if conn.Tunnel != "" {
		dialer := tm.Get(conn.Tunnel)
		if dialer == nil {
			return proxy.Mount{}, false
		}
		m.HTTPClient = proxyTunnelClient(dialer)
	}
	if token != "" {
		if conn.TokenScheme == "basic" {
			m.Token = proxy.NewTokenProviderBasic(token, conn.BasicSuffix)
		} else {
			m.Token = proxy.NewTokenProviderWithHeader(token, conn.TokenHeader)
		}
	}
	return m, true
}

// registerProxies connects to upstream MCP servers and re-exports their tools.
func registerProxies(ctx context.Context, s *server.MCPServer, cfg *config.Config, tm *tunnelManager) {
	var mounts []proxy.Mount

	for _, conn := range cfg.AllConnections() {
		if !config.IsProxyType(conn.Type) || !conn.Enabled() {
			continue
		}

		if conn.OAuth {
			tokenStore := config.NewKeychainTokenStore(conn.Name)
			if tokenStore.HasToken() {
				mounts = append(mounts, oauthProxyMount(conn.Name, conn.URL, cfg.Server.Port, tokenStore))
			}
			continue
		}

		// Token, custom-header, and/or no-auth mounts — all routed through the
		// connection's tunnel when set (fail-closed if it's unavailable).
		m, ok := newProxyTokenMount(conn, conn.Token, tm)
		if !ok {
			log.Printf("[mux] Skipping proxy %q: tunnel %q not available", conn.Name, conn.Tunnel)
			continue
		}
		mounts = append(mounts, m)
	}

	if len(mounts) > 0 {
		proxy.RegisterMounts(ctx, s, mounts)
	}
}

// retryAfterVaultUnlock re-resolves secrets from the now-unlocked vault
// and registers connections that were skipped at startup.
// This includes re-fetching provisioning if it failed due to a sealed vault.
// Runs in a goroutine — must be safe for concurrent access.
var retryMu sync.Mutex

func retryAfterVaultUnlock(ctx context.Context, s *server.MCPServer, cfg *config.Config, tm *tunnelManager) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[vault] Retry panicked: %v", r)
		}
	}()

	// Prevent concurrent retries (e.g. rapid lock/unlock cycles)
	if !retryMu.TryLock() {
		log.Printf("[vault] Retry already in progress, skipping")
		return
	}
	defer retryMu.Unlock()

	// Re-fetch provisioning for each endpoint whose fetch failed at startup
	// (typically because the token was still in a sealed vault).
	for i := range cfg.Provisioning {
		p := &cfg.Provisioning[i]
		if p.Endpoint == "" {
			continue
		}
		// Skip if this endpoint already delivered its share.
		if t, c := cfg.ProvisionedCountFor(p.Name); t > 0 || c > 0 {
			continue
		}

		token := p.Token
		if token == "" {
			token, _ = config.GetSecret(p.SecretKey())
		}
		if token == "" {
			continue
		}
		p.Token = token

		label := p.Name
		if label == "" {
			label = "default"
		}
		log.Printf("[vault] Retry: fetching provisioning %q from %s", label, p.Endpoint)
		provCtx, provCancel := context.WithTimeout(ctx, 20*time.Second)
		provResp, provErr := provisioning.Fetch(provCtx, p.Endpoint, token)
		provCancel()
		if provErr != nil {
			log.Printf("[vault] Retry: provisioning %q failed: %v", label, provErr)
			continue
		}
		cfg.SetProvisioned(p.Name, provResp.Tunnels, provResp.Connections)
		log.Printf("[vault] Retry: provisioned %q: %d tunnels, %d connections", label, len(provResp.Tunnels), len(provResp.Connections))
	}

	// Build set of already-registered tool prefixes
	registered := make(map[string]bool)
	for name := range s.ListTools() {
		if idx := strings.Index(name, "_"); idx > 0 {
			registered[name[:idx]] = true
		}
	}

	count := 0

	// Retry proxy connections (bearer token and OAuth)
	for _, conn := range cfg.AllConnections() {
		if !config.IsProxyType(conn.Type) || !conn.Enabled() {
			continue
		}
		if registered[conn.Name] {
			continue
		}

		var mount proxy.Mount

		if conn.OAuth {
			tokenStore := config.NewKeychainTokenStore(conn.Name)
			if !tokenStore.HasToken() {
				continue
			}
			mount = oauthProxyMount(conn.Name, conn.URL, cfg.Server.Port, tokenStore)
		} else {
			// Resolve token from vault without mutating the shared config
			token, _ := config.GetSecret(conn.Name + "-token")
			// A no-auth mount with no tunnel and no headers has nothing that
			// depends on the vault — it was already attempted at startup.
			if token == "" && conn.Tunnel == "" && len(conn.Headers) == 0 {
				continue
			}
			m, ok := newProxyTokenMount(conn, token, tm)
			if !ok {
				// Tunnel still not up — a later unlock/retry may resolve it.
				continue
			}
			mount = m
		}

		mountCtx, mountCancel := context.WithTimeout(ctx, 30*time.Second)
		if err := proxy.RegisterMount(mountCtx, s, mount); err != nil {
			log.Printf("[vault] Retry: proxy %s failed: %v", conn.Name, err)
		} else {
			log.Printf("[vault] Retry: proxy %s connected", conn.Name)
			registered[conn.Name] = true
			count++
		}
		mountCancel()
	}

	// Retry database connections that need a password from the vault
	for _, conn := range cfg.AllConnections() {
		if config.IsProxyType(conn.Type) || !conn.Enabled() {
			continue
		}
		if registered[conn.Name] {
			continue
		}

		// Resolve password from vault into a local copy
		if conn.Password == "" {
			pw, _ := config.GetSecret(conn.Name + "-password")
			if pw != "" {
				conn.Password = pw
			}
		}

		var dialer tools.Dialer
		if conn.Tunnel != "" {
			dialer = tm.Get(conn.Tunnel)
			if dialer == nil {
				continue
			}
		}
		if _, _, err := tools.RegisterConnection(s, conn, dialer); err != nil {
			log.Printf("[vault] Retry: %s failed: %v", conn.Name, err)
		} else {
			log.Printf("[vault] Retry: %s connected", conn.Name)
			count++
		}
	}

	if count > 0 {
		log.Printf("[vault] Registered %d connections after unlock", count)
	}
}

// simpleReloader implements tools.ToolReloader for stdio and headless modes (no App/Wails).
type simpleReloader struct {
	mcpServer       *server.MCPServer
	cfg             *config.Config
	tm              *tunnelManager
	toolsMu         sync.Mutex
	registeredTools map[string][]string
	closers         map[string][]io.Closer
}

func (r *simpleReloader) ReloadConnection(conn config.Connection) {
	r.unregisterConnection(conn.Name)

	if !conn.Enabled() {
		return
	}

	// Reload paths (connection_add, secret_set on other fields) hand the
	// connection over as it sits in config.toml — without its secrets. The
	// startup path resolves them via loadKeychain; without the same
	// resolution here the connection runs unauthenticated until the next
	// restart (an http proxy then silently sends no Authorization header).
	config.ResolveConnectionSecrets(&conn)

	if config.IsProxyType(conn.Type) {
		r.registerProxy(conn)
		return
	}

	// Resolve tunnel dialer (fail-closed)
	var dialer tools.Dialer
	if conn.Tunnel != "" {
		dialer = r.tm.Get(conn.Tunnel)
		if dialer == nil {
			log.Printf("[mux] Skipping %q: tunnel %q not available", conn.Name, conn.Tunnel)
			return
		}
	}

	names, closer, err := tools.RegisterConnection(r.mcpServer, conn, dialer)
	if err != nil {
		log.Printf("[mux] Warning: %s not available: %v", conn.Name, err)
		return
	}
	r.toolsMu.Lock()
	r.registeredTools[conn.Name] = names
	if closer != nil {
		r.closers[conn.Name] = append(r.closers[conn.Name], closer)
	}
	r.toolsMu.Unlock()
}

func (r *simpleReloader) UnloadConnection(name string) {
	r.unregisterConnection(name)
}

func (r *simpleReloader) unregisterConnection(name string) {
	r.toolsMu.Lock()
	defer r.toolsMu.Unlock()
	if names, ok := r.registeredTools[name]; ok && len(names) > 0 {
		r.mcpServer.DeleteTools(names...)
		log.Printf("[mux] Unregistered %d tools for %s", len(names), name)
	}
	delete(r.registeredTools, name)
	if closers, ok := r.closers[name]; ok {
		for _, c := range closers {
			c.Close()
		}
		delete(r.closers, name)
	}
}

func (r *simpleReloader) registerProxy(conn config.Connection) {
	if conn.OAuth {
		tokenStore := config.NewKeychainTokenStore(conn.Name)
		if !tokenStore.HasToken() {
			return
		}
		adapter := proxy.NewKeychainTokenAdapter(tokenStore)
		clientID, clientSecret := config.LoadOAuthClientID(conn.Name)
		mount := proxy.Mount{
			Name:       conn.Name,
			URL:        conn.URL,
			TokenStore: tokenStore,
			OAuth: &transport.OAuthConfig{
				ClientID:     clientID,
				ClientSecret: clientSecret,
				RedirectURI:  fmt.Sprintf("http://localhost:%d/oauth/callback", r.cfg.Server.Port),
				TokenStore:   adapter,
				PKCEEnabled:  true,
			},
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := proxy.RegisterMount(ctx, r.mcpServer, mount); err != nil {
			log.Printf("[mux] Warning: proxy %s not available: %v", conn.Name, err)
			return
		}
		r.trackProxyTools(conn.Name)
	} else {
		// Token, custom-header, and/or no-auth mount — routed through the
		// connection's tunnel when set (fail-closed if it's unavailable).
		mount, ok := newProxyTokenMount(conn, conn.Token, r.tm)
		if !ok {
			log.Printf("[mux] Skipping proxy %q: tunnel %q not available", conn.Name, conn.Tunnel)
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := proxy.RegisterMount(ctx, r.mcpServer, mount); err != nil {
			log.Printf("[mux] Warning: proxy %s not available: %v", conn.Name, err)
			return
		}
		r.trackProxyTools(conn.Name)
	}
}

// Registered tool names are sanitized (see config.SanitizeToolName), so the
// prefix must be sanitized the same way.
func (r *simpleReloader) trackProxyTools(connName string) {
	prefix := config.SanitizeToolName(connName) + "_"
	var names []string
	for name := range r.mcpServer.ListTools() {
		if strings.HasPrefix(name, prefix) {
			names = append(names, name)
		}
	}
	if len(names) > 0 {
		r.toolsMu.Lock()
		r.registeredTools[connName] = names
		r.toolsMu.Unlock()
	}
}
