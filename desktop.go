//go:build !notray

package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mark3labs/mcp-go/server"
	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"

	"github.com/smnhffmnn/mux/internal/config"
	"github.com/smnhffmnn/mux/internal/tools"
	"github.com/smnhffmnn/mux/internal/vault"
)

func runDesktop(s *server.MCPServer, cfg *config.Config, tm *tunnelManager, ctx context.Context, cancel context.CancelFunc, vlt *vault.Vault, waServer *vault.WebAuthnServer, approvalQueue *vault.ApprovalQueue) {
	log.Println("[mux] Starting desktop app")
	app := NewApp(cfg, version, buildTime, cfg.Server.Port, s, tm)

	// Register config management tools (with app as hot-reloader)
	configTools := tools.NewConfigTools(cfg, app)
	for _, t := range configTools.Tools() {
		s.AddTool(t.Tool, t.Handler)
		log.Printf("[mux] Registered: %s", t.Tool.Name)
	}

	var vh *vault.VaultHandlers
	if vlt != nil {
		vh = vault.NewVaultHandlers(vlt, waServer, approvalQueue)
		log.Println("[mux] Vault HTTP endpoints registered on /vault/*")

		// Credential socket for git credential helper
		credSock := vault.NewCredentialSocket(vlt, gitHostsFromConfig(cfg))
		go func() {
			if err := credSock.Listen(); err != nil {
				log.Printf("[vault] Credential socket error: %v", err)
			}
		}()
		defer credSock.Close()
	}

	localRoutes := func(mux *http.ServeMux) {
		app.oauth.Routes(mux)
		if vh != nil {
			vh.Mount(mux)
		}
	}
	var tlsRoutes func(*http.ServeMux)
	if vh != nil {
		tlsRoutes = func(mux *http.ServeMux) { vh.Mount(mux) }
	}
	servers := startHTTPServer(s, cfg, localRoutes, tlsRoutes)

	// Create Wails v3 application
	wailsApp := application.New(application.Options{
		Name: "mux",
		Mac: application.MacOptions{
			ApplicationShouldTerminateAfterLastWindowClosed: false,
		},
		Services: []application.Service{
			application.NewService(app),
		},
		Assets: application.AssetOptions{
			Handler: application.AssetFileServerFS(assets),
		},
	})

	// Wire up event emission from App to Wails
	app.emitEvent = func(name string, data any) {
		wailsApp.Event.Emit(name, data)
	}

	// Signal handling (os.Interrupt + SIGTERM for consistency with headless)
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
		<-sigCh
		log.Println("[mux] Shutting down...")
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		servers.Shutdown(shutdownCtx)
		cancel()
		wailsApp.Quit()
	}()

	// Create the main window
	window := wailsApp.Window.NewWithOptions(application.WebviewWindowOptions{
		Title:  "mux — MCP Unified Exchange",
		Width:  900,
		Height: 680,
		Mac: application.MacWindow{
			InvisibleTitleBarHeight: 50,
			Backdrop:                application.MacBackdropTranslucent,
		},
		URL: "/",
	})

	// Hide window on close (red button) instead of destroying it.
	// Wails' built-in ApplicationShouldHandleReopen handler shows it again on dock click.
	window.RegisterHook(events.Common.WindowClosing, func(e *application.WindowEvent) {
		e.Cancel()
		window.Hide()
	})

	// Start Wails app (blocks — owns main thread)
	if err := wailsApp.Run(); err != nil {
		log.Fatalf("Wails error: %v", err)
	}
}
