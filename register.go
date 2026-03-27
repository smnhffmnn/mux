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
	"github.com/smnhffmnn/mux/internal/wireguard"
)

var version = "dev"
var buildTime = ""

// registerConnections registers native database tools for each configured connection.
// Fail-closed: if a connection references a tunnel that isn't available, it's skipped.
func registerConnections(s *server.MCPServer, cfg *config.Config, wgMgr *wireguard.Manager) {
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
			t := wgMgr.Get(conn.Tunnel)
			if t == nil {
				log.Printf("[mux] Skipping %q: tunnel %q not available", conn.Name, conn.Tunnel)
				continue
			}
			dialer = t
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

// simpleReloader implements tools.ToolReloader for stdio and headless modes (no App/Wails).
type simpleReloader struct {
	mcpServer       *server.MCPServer
	cfg             *config.Config
	wgMgr           *wireguard.Manager
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
		t := r.wgMgr.Get(conn.Tunnel)
		if t == nil {
			log.Printf("[mux] Skipping %q: tunnel %q not available", conn.Name, conn.Tunnel)
			return
		}
		dialer = t
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
