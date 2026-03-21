package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"time"

	"github.com/mark3labs/mcp-go/server"
	"github.com/wailsapp/wails/v3/pkg/application"

	"github.com/smnhffmnn/mux/internal/config"
	"github.com/smnhffmnn/mux/internal/erp"
	"github.com/smnhffmnn/mux/internal/tools"
	"github.com/smnhffmnn/mux/internal/wireguard"
)

func main() {
	var (
		flagPort    int
		flagConfig  string
		flagVersion bool
	)

	flag.IntVar(&flagPort, "port", 0, "HTTP port (default: 7700)")
	flag.StringVar(&flagConfig, "config", "", "Config file path (default: ~/.mux/config.toml)")
	flag.BoolVar(&flagVersion, "version", false, "Print version and exit")
	flag.Parse()

	if flagVersion {
		fmt.Printf("mux %s\n", version)
		os.Exit(0)
	}

	// Auto-detect transport mode: pipe = stdio, terminal = desktop
	// Check EARLY before any log output (stdio mode must not pollute stdout/stderr)
	fi, err := os.Stdin.Stat()
	useStdio := err == nil && fi != nil && (fi.Mode()&os.ModeCharDevice) == 0

	// In stdio mode, suppress all log output to avoid polluting the MCP stream
	if useStdio {
		log.SetOutput(io.Discard)
	}

	// Load configuration
	cfg, err := config.Load(flagConfig)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	if flagPort > 0 {
		cfg.Server.Port = flagPort
	}

	// ERP Provisioning: fetch tunnels + connections from ERP API
	if cfg.HasERP() {
		log.Printf("[mux] Fetching config from ERP: %s", cfg.ERP.Endpoint)
		erpCtx, erpCancel := context.WithTimeout(context.Background(), 20*time.Second)
		erpResp, erpErr := erp.Fetch(erpCtx, cfg.ERP.Endpoint, cfg.ERP.Token)
		erpCancel()
		if erpErr != nil {
			log.Printf("[mux] ERP provisioning failed: %v (continuing with local config)", erpErr)
		} else {
			cfg.SetERP(erpResp.Tunnels, erpResp.Connections)
			log.Printf("[mux] ERP provisioned: %d tunnels, %d connections", len(erpResp.Tunnels), len(erpResp.Connections))
		}
	}

	// WireGuard tunnels
	wgMgr := wireguard.NewManager()
	defer wgMgr.Close()

	allTunnels := cfg.AllTunnels()
	if len(allTunnels) > 0 {
		errs := wgMgr.Start(allTunnels)
		for name, err := range errs {
			log.Printf("[mux] Tunnel %q failed: %v — connections using it will be skipped", name, err)
		}
	}

	// Build MCP instructions from connection descriptions
	instruction := "mux (MCP Unified Exchange) — a unified MCP gateway providing access to configured database and proxy connections."
	for _, conn := range cfg.AllConnections() {
		instr := conn.Instructions
		if instr == "" {
			instr = tools.DefaultInstructions(conn.Type)
		}
		if instr != "" {
			instruction += fmt.Sprintf("\n\n%s: %s", conn.Name, instr)
		}
	}

	// Create MCP server
	s := server.NewMCPServer("mux", version,
		server.WithToolCapabilities(true),
		server.WithRecovery(),
		server.WithInstructions(instruction),
	)

	// Register connections (with fail-closed tunnel logic)
	registerConnections(s, cfg, wgMgr)

	// Register proxy mounts
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	registerProxies(ctx, s, cfg)

	// Always register the datetime tool
	s.AddTool(tools.DateTimeTool.Tool, tools.DateTimeTool.Handler)
	log.Println("[mux] Registered: get_datetime")

	// --- Stdio mode (Claude Desktop, piped stdin) ---
	if useStdio {
		reloader := &stdioReloader{mcpServer: s, cfg: cfg, wgMgr: wgMgr, registeredTools: make(map[string][]string), closers: make(map[string][]io.Closer)}
		configTools := tools.NewConfigTools(cfg, reloader)
		for _, t := range configTools.Tools() {
			s.AddTool(t.Tool, t.Handler)
			log.Printf("[mux] Registered: %s", t.Tool.Name)
		}
		if err := server.ServeStdio(s); err != nil {
			fmt.Fprintf(os.Stderr, "stdio server error: %v\n", err)
			os.Exit(1)
		}
		return
	}

	// --- Desktop mode (Wails v3 + MCP HTTP) ---
	app := NewApp(cfg, version, buildTime, cfg.Server.Port, s, wgMgr)

	// Register config management tools (with app as hot-reloader)
	configTools := tools.NewConfigTools(cfg, app)
	for _, t := range configTools.Tools() {
		s.AddTool(t.Tool, t.Handler)
		log.Printf("[mux] Registered: %s", t.Tool.Name)
	}

	// Start MCP HTTP server on localhost only (avoids firewall popups)
	addr := fmt.Sprintf("127.0.0.1:%d", cfg.Server.Port)
	mux := http.NewServeMux()

	mcpHandler := server.NewStreamableHTTPServer(s)
	mux.Handle("/mcp", mcpHandler)
	mux.HandleFunc("/oauth/callback", app.oauthCallbackHandler())

	httpSrv := &http.Server{Addr: addr, Handler: mux}

	go func() {
		log.Printf("[mux] Starting MCP HTTP server on %s", addr)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("HTTP server error: %v", err)
		}
	}()

	log.Printf("[mux] MCP available at http://localhost:%d/mcp", cfg.Server.Port)

	// Signal handling (os.Interrupt works on all platforms, including Windows)
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, os.Interrupt)
		<-sigCh
		log.Println("[mux] Shutting down...")
		httpSrv.Shutdown(ctx)
		cancel()
		os.Exit(0)
	}()

	// Create Wails v3 application
	wailsApp := application.New(application.Options{
		Name: "mux",
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

	// Create the main window
	wailsApp.Window.NewWithOptions(application.WebviewWindowOptions{
		Title:  "mux — MCP Unified Exchange",
		Width:  900,
		Height: 680,
		Mac: application.MacWindow{
			InvisibleTitleBarHeight: 50,
			Backdrop:               application.MacBackdropTranslucent,
		},
		URL: "/",
	})

	// Start Wails app (blocks — owns main thread)
	if err := wailsApp.Run(); err != nil {
		log.Fatalf("Wails error: %v", err)
	}
}
