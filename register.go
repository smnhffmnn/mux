package main

import (
	"context"
	"fmt"
	"log"

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

		if _, err := tools.RegisterConnection(s, conn, dialer); err != nil {
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
