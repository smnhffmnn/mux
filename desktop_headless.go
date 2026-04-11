//go:build notray

package main

import (
	"context"
	"log"
	"net/http"

	"github.com/mark3labs/mcp-go/server"

	"github.com/smnhffmnn/mux/internal/config"
	"github.com/smnhffmnn/mux/internal/vault"
)

func runDesktop(s *server.MCPServer, cfg *config.Config, tm *tunnelManager, ctx context.Context, cancel context.CancelFunc, _ *vault.Vault, _ *vault.WebAuthnServer, _ *vault.ApprovalQueue) {
	log.Fatal("Desktop mode not available in headless build. This binary was built with -tags notray.")
}

func headlessOAuthRoutes(_ *config.Config, _ int, _ *server.MCPServer, _ *tunnelManager) func(*http.ServeMux) {
	return nil
}
