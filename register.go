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
		}
	}

	if len(mounts) > 0 {
		proxy.RegisterMounts(ctx, s, mounts)
	}
}

// retryAfterVaultUnlock re-resolves secrets from the now-unlocked vault
// and registers connections that were skipped at startup.
// Runs in a goroutine — must be safe for concurrent access.
var retryMu sync.Mutex

func retryAfterVaultUnlock(ctx context.Context, s *server.MCPServer, cfg *config.Config, tm *tunnelManager) {
	// Prevent concurrent retries (e.g. rapid lock/unlock cycles)
	if !retryMu.TryLock() {
		log.Printf("[vault] Retry already in progress, skipping")
		return
	}
	defer retryMu.Unlock()

	// Build set of already-registered tool prefixes
	registered := make(map[string]bool)
	for name := range s.ListTools() {
		if idx := strings.Index(name, "_"); idx > 0 {
			registered[name[:idx]] = true
		}
	}

	count := 0

	// Retry bearer-token proxy connections (not OAuth — OAuth tokens are
	// stored in the keychain token store, not the vault, so they don't
	// benefit from vault unlock)
	for _, conn := range cfg.AllConnections() {
		if !config.IsProxyType(conn.Type) || !conn.Enabled() {
			continue
		}
		if registered[conn.Name] {
			continue
		}
		if conn.OAuth {
			continue
		}

		// Resolve token from vault without mutating the shared config
		token, _ := config.GetSecret(conn.Name + "-token")
		if token == "" {
			continue
		}

		mount := proxy.Mount{
			Name:  conn.Name,
			URL:   conn.URL,
			Token: proxy.NewTokenProvider(token),
		}
		mountCtx, mountCancel := context.WithTimeout(ctx, 30*time.Second)
		if err := proxy.RegisterMount(mountCtx, s, mount); err != nil {
			log.Printf("[vault] Retry: proxy %s failed: %v", conn.Name, err)
		} else {
			log.Printf("[vault] Retry: proxy %s connected", conn.Name)
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
	}
}

func (r *simpleReloader) trackProxyTools(connName string) {
	prefix := connName + "_"
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
