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
	"runtime"
	"syscall"
	"time"

	"github.com/mark3labs/mcp-go/server"

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

	flag.IntVar(&flagPort, "port", 0, "HTTP port")
	flag.StringVar(&flagConfig, "config", "", "config file path")
	flag.BoolVar(&flagVersion, "version", false, "print version and exit")

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, `mux — MCP Unified Exchange

Single-binary MCP gateway for databases, APIs, and tunnels.
Provides Claude with unified access to configured connections
via the Model Context Protocol (MCP).

Usage:
  mux                     Start desktop app (GUI + MCP server)
  echo '...' | mux        Start in stdio mode (Claude Desktop)

Options:
  --config <path>    Config file (default: ~/.mux/config.toml)
  --port <port>      MCP HTTP port (default: 7700)
  --version          Print version and exit

Transport modes:
  Desktop    Interactive terminal with display — launches GUI + HTTP server
  Headless   Interactive terminal, no display — HTTP server only (servers)
  Stdio      Piped input detected — MCP stdio transport (no logs, no UI)

More info: https://github.com/smnhffmnn/mux
`)
	}

	flag.Parse()

	if flagVersion {
		fmt.Printf("mux %s\n", version)
		os.Exit(0)
	}

	// Auto-detect transport mode: pipe = stdio, terminal = headless or desktop
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
		log.Println("[mux] Starting in stdio mode")
		registerConfigTools(s, cfg, wgMgr)
		if err := server.ServeStdio(s); err != nil {
			fmt.Fprintf(os.Stderr, "stdio server error: %v\n", err)
			os.Exit(1)
		}
		return
	}

	// Detect display availability for GUI vs headless decision
	hasDisplay := runtime.GOOS == "darwin" || runtime.GOOS == "windows" ||
		os.Getenv("DISPLAY") != "" || os.Getenv("WAYLAND_DISPLAY") != ""

	// --- Headless HTTP mode (no display server available) ---
	if !hasDisplay {
		log.Println("[mux] Starting in headless HTTP mode (no display detected)")

		registerConfigTools(s, cfg, wgMgr)

		httpSrv := startHTTPServer(s, nil, cfg.Server.Port)

		// Block until SIGINT/SIGTERM
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
		<-sigCh
		log.Println("[mux] Shutting down...")
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		httpSrv.Shutdown(shutdownCtx)
		cancel()
		return
	}

	// --- Desktop mode (Wails v3 + MCP HTTP) ---
	runDesktop(s, cfg, wgMgr, ctx, cancel)
}

// registerConfigTools creates a simpleReloader and registers config management tools on the MCP server.
func registerConfigTools(s *server.MCPServer, cfg *config.Config, wgMgr *wireguard.Manager) {
	reloader := &simpleReloader{mcpServer: s, cfg: cfg, wgMgr: wgMgr, registeredTools: make(map[string][]string), closers: make(map[string][]io.Closer)}
	configTools := tools.NewConfigTools(cfg, reloader)
	for _, t := range configTools.Tools() {
		s.AddTool(t.Tool, t.Handler)
		log.Printf("[mux] Registered: %s", t.Tool.Name)
	}
}

// startHTTPServer creates and starts the MCP HTTP server on localhost.
// If app is non-nil (desktop mode), the OAuth callback handler is registered.
func startHTTPServer(s *server.MCPServer, app *App, port int) *http.Server {
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	mux := http.NewServeMux()

	mcpHandler := server.NewStreamableHTTPServer(s)
	mux.Handle("/mcp", mcpHandler)

	if app != nil {
		mux.HandleFunc("/oauth/callback", app.oauthCallbackHandler())
	}

	httpSrv := &http.Server{Addr: addr, Handler: mux}

	go func() {
		log.Printf("[mux] Starting MCP HTTP server on %s", addr)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("HTTP server error: %v", err)
		}
	}()

	log.Printf("[mux] MCP available at http://localhost:%d/mcp", port)
	return httpSrv
}
