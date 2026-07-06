package main

import (
	"context"
	"fmt"
	"io"
	"log"
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

// registerProxies connects to upstream MCP servers and re-exports their tools.
func registerProxies(ctx context.Context, s *server.MCPServer, cfg *config.Config) {
	var mounts []proxy.Mount

	for _, conn := range cfg.AllConnections() {
		if !config.IsProxyType(conn.Type) || !conn.Enabled() {
			continue
		}

		if conn.OAuth {
			tokenStore := config.NewKeychainTokenStore(conn.Name)
			if tokenStore.HasToken() {
				adapter := proxy.NewKeychainTokenAdapter(tokenStore)
				clientID, clientSecret := config.LoadOAuthClientID(conn.Name)
				mounts = append(mounts, proxy.Mount{
					Name:       conn.Name,
					URL:        conn.URL,
					TokenStore: tokenStore,
					OAuth: &transport.OAuthConfig{
						ClientID:     clientID,
						ClientSecret: clientSecret,
						RedirectURI:  fmt.Sprintf("http://localhost:%d/oauth/callback", cfg.Server.Port),
						TokenStore:   adapter,
						PKCEEnabled:  true,
					},
				})
			}
		} else if conn.Token != "" {
			mounts = append(mounts, proxy.Mount{
				Name:  conn.Name,
				URL:   conn.URL,
				Token: proxy.NewTokenProvider(conn.Token),
			})
		} else {
			// No-auth mount — upstream server handles its own auth (e.g. google-workspace)
			mounts = append(mounts, proxy.Mount{
				Name: conn.Name,
				URL:  conn.URL,
			})
		}
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
			adapter := proxy.NewKeychainTokenAdapter(tokenStore)
			clientID, clientSecret := config.LoadOAuthClientID(conn.Name)
			mount = proxy.Mount{
				Name:       conn.Name,
				URL:        conn.URL,
				TokenStore: tokenStore,
				OAuth: &transport.OAuthConfig{
					ClientID:     clientID,
					ClientSecret: clientSecret,
					RedirectURI:  fmt.Sprintf("http://localhost:%d/oauth/callback", cfg.Server.Port),
					TokenStore:   adapter,
					PKCEEnabled:  true,
				},
			}
		} else {
			// Resolve token from vault without mutating the shared config
			token, _ := config.GetSecret(conn.Name + "-token")
			if token != "" {
				mount = proxy.Mount{
					Name:  conn.Name,
					URL:   conn.URL,
					Token: proxy.NewTokenProvider(token),
				}
			} else {
				// No-auth connections don't depend on vault secrets —
				// they were already attempted at startup. Skip retry.
				continue
			}
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
	} else if conn.Token != "" {
		mount := proxy.Mount{
			Name:  conn.Name,
			URL:   conn.URL,
			Token: proxy.NewTokenProvider(conn.Token),
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := proxy.RegisterMount(ctx, r.mcpServer, mount); err != nil {
			log.Printf("[mux] Warning: proxy %s not available: %v", conn.Name, err)
			return
		}
		r.trackProxyTools(conn.Name)
	} else {
		// No-auth mount — upstream server handles its own auth
		mount := proxy.Mount{
			Name: conn.Name,
			URL:  conn.URL,
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
