//go:build notray

package main

import (
	"context"
	"log"

	"github.com/mark3labs/mcp-go/server"

	"github.com/smnhffmnn/mux/internal/config"
	"github.com/smnhffmnn/mux/internal/wireguard"
)

func runDesktop(s *server.MCPServer, cfg *config.Config, wgMgr *wireguard.Manager, ctx context.Context, cancel context.CancelFunc) {
	log.Fatal("Desktop mode not available in headless build. This binary was built with -tags notray.")
}
