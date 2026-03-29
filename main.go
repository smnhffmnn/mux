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
	"github.com/smnhffmnn/mux/internal/vault"
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

	// --- Vault (encrypted secret store) ---
	var vlt *vault.Vault
	var waServer *vault.WebAuthnServer
	var approvalQueue *vault.ApprovalQueue

	if cfg.Vault.Enabled {
		var vaultOpts []vault.Option
		if cfg.Vault.InactivityTimeout != "" {
			d, err := time.ParseDuration(cfg.Vault.InactivityTimeout)
			if err != nil {
				log.Printf("[vault] Invalid inactivity_timeout %q: %v (using default)", cfg.Vault.InactivityTimeout, err)
			} else {
				vaultOpts = append(vaultOpts, vault.WithInactivityTimeout(d))
			}
		}
		vlt = vault.New(vaultOpts...)

		if err := vlt.Load(); err != nil {
			log.Printf("[vault] Failed to load vault: %v (continuing without vault)", err)
			vlt = nil
		} else {
			config.SetActiveVault(vlt)
			if cfg.Vault.Exclusive {
				config.SetVaultExclusive(true)
				log.Println("[vault] Exclusive mode: secrets only in vault, not in legacy stores")
			}
			log.Printf("[vault] State: %s (%d secrets stored)", vlt.State(), vlt.Status().SecretCount)

			// WebAuthn server (only if RP ID is configured)
			if cfg.Vault.WebAuthnRPID != "" {
				wa, err := vault.NewWebAuthnServer(vault.WebAuthnConfig{
					RPID:          cfg.Vault.WebAuthnRPID,
					RPOrigins:     cfg.Vault.WebAuthnOrigins,
					RPDisplayName: "Mux Vault",
				}, vlt)
				if err != nil {
					log.Printf("[vault] WebAuthn init failed: %v", err)
				} else {
					waServer = wa
					log.Printf("[vault] WebAuthn enabled (RP: %s)", cfg.Vault.WebAuthnRPID)
				}
			}

			// Approval queue + notifier
			var notifier vault.Notifier
			discordWebhook, _ := config.GetSecret("vault-discord-webhook")
			if discordWebhook != "" {
				notifier = vault.NewDiscordWebhookNotifier(discordWebhook)
				log.Println("[vault] Discord webhook notifier configured")
			} else {
				notifier = &vault.LogNotifier{}
			}

			baseURL := cfg.Vault.BaseURL
			if baseURL == "" {
				baseURL = fmt.Sprintf("http://localhost:%d", cfg.Server.Port)
			}
			approvalQueue = vault.NewApprovalQueue(notifier, baseURL)
			log.Println("[vault] Approval queue initialized")
		}
	}

	// Tunnels (WireGuard + SSH)
	wgMgr := wireguard.NewManager()
	defer wgMgr.Close()

	allTunnels := cfg.AllTunnels()

	// Start WireGuard tunnels
	var wgTunnels []config.TunnelConfig
	for _, t := range allTunnels {
		if !t.IsSSH() {
			wgTunnels = append(wgTunnels, t)
		}
	}
	if len(wgTunnels) > 0 {
		for name, err := range wgMgr.Start(wgTunnels) {
			log.Printf("[mux] Tunnel %q failed: %v — connections using it will be skipped", name, err)
		}
	}

	// Combined tunnel manager (WireGuard + SSH)
	tm := newTunnelManager(wgMgr)
	defer tm.Close()

	// Start SSH tunnels
	for name, err := range tm.StartSSH(allTunnels) {
		log.Printf("[mux] Tunnel %q failed: %v — connections using it will be skipped", name, err)
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
	registerConnections(s, cfg, tm)

	// Register proxy mounts
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	registerProxies(ctx, s, cfg)

	// Always register the datetime tool
	s.AddTool(tools.DateTimeTool.Tool, tools.DateTimeTool.Handler)
	log.Println("[mux] Registered: get_datetime")

	// Register vault MCP tools (all modes)
	if vlt != nil {
		vaultTools := tools.NewVaultTools(vlt, cfg)
		for _, t := range vaultTools.Tools() {
			s.AddTool(t.Tool, t.Handler)
			log.Printf("[mux] Registered: %s", t.Tool.Name)
		}
	}

	// --- Stdio mode (Claude Desktop, piped stdin) ---
	if useStdio {
		log.Println("[mux] Starting in stdio mode")
		registerConfigTools(s, cfg, tm)
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

		registerConfigTools(s, cfg, tm)

		httpSrv := startHTTPServer(s, cfg, func(mux *http.ServeMux) {
			if vlt != nil {
				vault.RegisterHandlers(mux, vlt, waServer, approvalQueue)
				log.Println("[mux] Vault HTTP endpoints registered on /vault/*")
			}
		})

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
	runDesktop(s, cfg, tm, ctx, cancel, vlt, waServer, approvalQueue)
}

// registerConfigTools creates a simpleReloader and registers config management tools on the MCP server.
func registerConfigTools(s *server.MCPServer, cfg *config.Config, tm *tunnelManager) {
	reloader := &simpleReloader{mcpServer: s, cfg: cfg, tm: tm, registeredTools: make(map[string][]string), closers: make(map[string][]io.Closer)}
	configTools := tools.NewConfigTools(cfg, reloader)
	for _, t := range configTools.Tools() {
		s.AddTool(t.Tool, t.Handler)
		log.Printf("[mux] Registered: %s", t.Tool.Name)
	}
}

// startHTTPServer creates and starts the MCP HTTP server.
// If TLS cert+key are configured, serves HTTPS on 0.0.0.0 (for Tailscale access).
// Otherwise, serves plain HTTP on localhost only.
func startHTTPServer(s *server.MCPServer, cfg *config.Config, extraRoutes func(*http.ServeMux)) *http.Server {
	mux := http.NewServeMux()

	mcpHandler := server.NewStreamableHTTPServer(s)
	mux.Handle("/mcp", mcpHandler)

	if extraRoutes != nil {
		extraRoutes(mux)
	}

	useTLS := cfg.Server.TLSCert != "" && cfg.Server.TLSKey != ""

	var addr string
	if useTLS {
		// TLS: bind to all interfaces (Tailscale, LAN)
		addr = fmt.Sprintf(":%d", cfg.Server.Port)
	} else {
		// Plain HTTP: localhost only
		addr = fmt.Sprintf("127.0.0.1:%d", cfg.Server.Port)
	}

	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		MaxHeaderBytes:    1 << 20, // 1 MB
	}

	go func() {
		if useTLS {
			certPath := config.ExpandHome(cfg.Server.TLSCert)
			keyPath := config.ExpandHome(cfg.Server.TLSKey)
			log.Printf("[mux] Starting HTTPS server on %s (TLS)", addr)
			if err := httpSrv.ListenAndServeTLS(certPath, keyPath); err != nil && err != http.ErrServerClosed {
				log.Fatalf("HTTPS server error: %v", err)
			}
		} else {
			log.Printf("[mux] Starting HTTP server on %s", addr)
			if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Fatalf("HTTP server error: %v", err)
			}
		}
	}()

	scheme := "http"
	if useTLS {
		scheme = "https"
	}
	log.Printf("[mux] MCP available at %s://localhost:%d/mcp", scheme, cfg.Server.Port)
	return httpSrv
}
