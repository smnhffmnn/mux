package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/mark3labs/mcp-go/server"

	"github.com/smnhffmnn/mux/internal/bridge"
	"github.com/smnhffmnn/mux/internal/config"
	"github.com/smnhffmnn/mux/internal/logging"
	"github.com/smnhffmnn/mux/internal/oauth"
	"github.com/smnhffmnn/mux/internal/provisioning"
	"github.com/smnhffmnn/mux/internal/tools"
	"github.com/smnhffmnn/mux/internal/vault"
	"github.com/smnhffmnn/mux/internal/wireguard"
)

// mcpServerName is the name this instance reports over MCP, and the name a
// bridge requires its upstream to report. Shared so the two cannot drift.
const mcpServerName = "mux"

// Timeouts for probing an already-running instance. The first is short because
// it sits in the startup path of the calling MCP client and a closed port
// refuses immediately; the second gives a slow-but-live instance a real chance
// before we give up and build a second set of tunnels next to it.
const (
	stdioBridgeDialTimeout      = 3 * time.Second
	stdioBridgeRetryDialTimeout = 15 * time.Second
)

// dialUpstream probes for an instance already serving MCP at endpoint.
//
// A conclusive failure — nothing listening, or something that is not mux — is
// returned as-is so the caller runs standalone. A timeout is not conclusive:
// something holds the port but did not finish the handshake in time, and
// treating that as "nobody home" is the expensive mistake, because an owner
// that is merely slow (cold start, loaded machine, a keychain prompt) would end
// up with a second instance and duplicate tunnels beside it. That case gets one
// more attempt with a longer deadline.
func dialUpstream(endpoint string) (*bridge.Upstream, error) {
	opts := bridge.DialOptions{
		URL:              endpoint,
		ExpectServerName: mcpServerName,
		ClientName:       "mux-bridge",
		ClientVersion:    version,
	}

	ctx, cancel := context.WithTimeout(context.Background(), stdioBridgeDialTimeout)
	upstream, err := bridge.Dial(ctx, opts)
	cancel()
	// Check both shapes: the deadline may surface as context.DeadlineExceeded or,
	// once net/http has wrapped it, as an error that only reports Timeout().
	// Misclassifying here is not fatal — it just means falling back to standalone,
	// which is what happened before this retry existed.
	if err == nil || !(errors.Is(err, context.DeadlineExceeded) || os.IsTimeout(err)) {
		return upstream, err
	}

	log.Printf("[mux] %s did not answer within %s — retrying before falling back",
		endpoint, stdioBridgeDialTimeout)

	ctx, cancel = context.WithTimeout(context.Background(), stdioBridgeRetryDialTimeout)
	defer cancel()
	return bridge.Dial(ctx, opts)
}

func main() {
	// Subcommand: git credential helper (must be checked before flag.Parse)
	if len(os.Args) > 1 && os.Args[1] == "git-credential" {
		runGitCredential(os.Args[2:])
		return
	}

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
  mux                          Start desktop app (GUI + MCP server)
  echo '...' | mux             Start in stdio mode (Claude Desktop)
  mux git-credential <op>      Git credential helper (get/store/erase)

Options:
  --config <path>    Config file (default: $XDG_CONFIG_HOME/mux/config.toml or ~/.config/mux/config.toml)
  --port <port>      MCP HTTP port (default: 7700)
  --version          Print version and exit

Transport modes:
  Desktop    Interactive terminal with display — launches GUI + HTTP server
  Headless   Interactive terminal, no display — HTTP server only (servers)
  Stdio      Piped input detected — MCP stdio transport (no UI; stdout carries
             the protocol, diagnostics go to stderr)

Stdio and a running instance:
  When another mux already serves MCP on the configured port, a stdio
  invocation relays to it instead of starting its own tunnels, vault and
  connections — only one instance per machine should own those. Control this
  with [server] stdio_proxy = "auto" (default) | "always" | "never".

More info: https://github.com/smnhffmnn/mux
`)
	}

	flag.Parse()

	if flagVersion {
		fmt.Printf("mux %s\n", version)
		os.Exit(0)
	}

	// Auto-detect transport mode: pipe = stdio, terminal = headless or desktop
	// Check EARLY before any log output: in stdio mode stdout belongs to the MCP
	// protocol, so the standard logger has to be silenced before it writes there.
	// Diagnostics still go to stderr, which the transport leaves free for them.
	fi, err := os.Stdin.Stat()
	useStdio := err == nil && fi != nil && (fi.Mode()&os.ModeCharDevice) == 0

	// In stdio mode, suppress log output until the log file is open — stdout
	// belongs to the MCP protocol, and stderr stays as quiet as it always was.
	if useStdio {
		log.SetOutput(io.Discard)
	}

	// One-time migration of pre-XDG installations (~/.mux → ~/.config/mux).
	// No-op on fresh installs and on Windows; logs and continues on failure.
	config.MigrateLegacyDir()

	// Route the standard logger to <config-dir>/logs/mux.log (after the
	// migration, so the file lands in the final directory). Every mode gets
	// the file; desktop/headless keep stderr alongside it. Without a file the
	// previous behaviour remains: stderr when visible, silence in stdio.
	logPath, logErr := logging.Setup(config.Dir(), useStdio)
	if logErr != nil {
		// Also to stderr: in stdio mode the standard logger is still discarded
		// at this point, and this is the one line explaining why no log exists.
		log.Printf("[mux] file logging unavailable: %v", logErr)
		fmt.Fprintf(os.Stderr, "[mux] file logging unavailable: %v\n", logErr)
	}
	bridge.UseStandardLogger()

	mode := "desktop/headless"
	if useStdio {
		mode = "stdio"
	}
	cfgPath := flagConfig
	if cfgPath == "" {
		cfgPath = filepath.Join(config.Dir(), config.ConfigFileName)
	}
	log.Printf("[mux] mux %s starting (mode %s, config %s, log %s)", version, mode, cfgPath, logPath)

	// Load configuration
	cfg, err := config.Load(flagConfig)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}
	log.Printf("[mux] config loaded: %d tunnels, %d connections, %d provisioning endpoints",
		len(cfg.AllTunnels()), len(cfg.AllConnections()), len(cfg.Provisioning))

	if flagPort > 0 {
		cfg.Server.Port = flagPort
	}

	// Warn about an unrecognized stdio_proxy value: it silently resolves to the
	// default, which is the opposite of what someone typing "nevr" or "of"
	// wanted.
	if raw := strings.TrimSpace(cfg.Server.StdioProxy); raw != "" {
		if effective := cfg.Server.EffectiveStdioProxy(); !strings.EqualFold(raw, effective) {
			log.Printf("[mux] unknown stdio_proxy %q — using %q", raw, effective)
		}
	}

	// --- Stdio bridge to an already-running instance ---
	// This has to sit ahead of provisioning, vault and tunnel startup, because
	// not doing any of that is the entire point: a second instance that builds
	// its own tunnels, vault and config view collides with the running one (see
	// internal/bridge for what breaks). Falls through to a full standalone
	// instance when nothing is serving locally.
	if useStdio && cfg.Server.EffectiveStdioProxy() != config.StdioProxyNever {
		endpoint := cfg.Server.MCPEndpoint()
		upstream, dialErr := dialUpstream(endpoint)

		switch {
		case dialErr == nil:
			info := upstream.ServerInfo()
			log.Printf("[mux] bridging stdio to %s (%s %s)", endpoint, info.Name, info.Version)
			if err := upstream.Serve(context.Background(), mcpServerName, version); err != nil {
				upstream.Close()
				// Fatal exits also go to stderr so the invoking MCP client
				// surfaces them — the log file alone doesn't help someone
				// staring at a dead client.
				log.Printf("[mux] stdio bridge error: %v", err)
				fmt.Fprintf(os.Stderr, "stdio bridge error: %v\n", err)
				os.Exit(1)
			}
			upstream.Close()
			return
		case cfg.Server.EffectiveStdioProxy() == config.StdioProxyAlways:
			log.Printf("[mux] stdio_proxy is %q but no usable instance is serving at %s: %v",
				config.StdioProxyAlways, endpoint, dialErr)
			fmt.Fprintf(os.Stderr, "stdio_proxy is %q but no usable instance is serving at %s: %v\n",
				config.StdioProxyAlways, endpoint, dialErr)
			os.Exit(1)
		default:
			// Standalone means building our own tunnels, which is exactly the
			// collision the bridge exists to avoid — so say so plainly rather
			// than reporting it as routine.
			log.Printf("[mux] no usable instance at %s (%v) — starting standalone with its own tunnels",
				endpoint, dialErr)
		}
	}

	// Remote Provisioning: fetch tunnels + connections from each configured endpoint.
	// Errors on individual endpoints are logged and do not block startup.
	for i := range cfg.Provisioning {
		p := cfg.Provisioning[i]
		if !p.Enabled() {
			continue
		}
		label := p.Name
		if label == "" {
			label = "default"
		}
		log.Printf("[mux] Fetching config from provisioning endpoint %q: %s", label, p.Endpoint)
		provCtx, provCancel := context.WithTimeout(context.Background(), 20*time.Second)
		provResp, provErr := provisioning.Fetch(provCtx, p.Endpoint, p.Token)
		provCancel()
		if provErr != nil {
			log.Printf("[mux] Provisioning from %q failed: %v (continuing)", label, provErr)
			continue
		}
		cfg.SetProvisioned(p.Name, provResp.Tunnels, provResp.Connections)
		log.Printf("[mux] Provisioned from %q: %d tunnels, %d connections", label, len(provResp.Tunnels), len(provResp.Connections))
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
			} else {
				log.Println("[vault] Warning: exclusive mode is off — secrets are also written to plaintext secrets.toml")
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

			// Approval queue + notifier(s)
			var notifiers []vault.Notifier
			tgToken, _ := config.GetSecret("vault-telegram-bot-token")
			tgChatID, _ := config.GetSecret("vault-telegram-chat-id")
			if tgToken != "" && tgChatID != "" {
				notifiers = append(notifiers, vault.NewTelegramNotifier(tgToken, tgChatID))
				log.Println("[vault] Telegram notifier configured")
			}
			discordWebhook, _ := config.GetSecret("vault-discord-webhook")
			if discordWebhook != "" {
				notifiers = append(notifiers, vault.NewDiscordWebhookNotifier(discordWebhook))
				log.Println("[vault] Discord webhook notifier configured")
			}
			var notifier vault.Notifier
			if len(notifiers) > 0 {
				notifier = vault.NewMultiNotifier(notifiers...)
			} else {
				notifier = &vault.LogNotifier{}
			}

			baseURL := cfg.Vault.BaseURL
			if baseURL == "" {
				if cfg.Server.TLSCert != "" && cfg.Server.TLSKey != "" {
					baseURL = fmt.Sprintf("https://localhost:%d", cfg.Server.EffectiveTLSPort())
				} else {
					baseURL = fmt.Sprintf("http://localhost:%d", cfg.Server.Port)
				}
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
	s := server.NewMCPServer(mcpServerName, version,
		server.WithToolCapabilities(true),
		server.WithRecovery(),
		server.WithInstructions(instruction),
	)

	// Register connections (with fail-closed tunnel logic)
	registerConnections(s, cfg, tm)

	// Register proxy mounts
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	registerProxies(ctx, s, cfg, tm)

	// Wire up vault unlock → retry skipped connections
	if vlt != nil {
		vlt.SetOnUnlock(func() {
			retryAfterVaultUnlock(ctx, s, cfg, tm)
		})
	}

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
			log.Printf("[mux] stdio server error: %v", err)
			fmt.Fprintf(os.Stderr, "stdio server error: %v\n", err)
			os.Exit(1)
		}
		return
	}

	// Detect display availability for GUI vs headless decision.
	// Config mode overrides auto-detection: "headless" forces HTTP-only, "desktop" forces GUI.
	var hasDisplay bool
	switch strings.ToLower(cfg.Server.Mode) {
	case "headless":
		hasDisplay = false
		log.Println("[mux] Mode forced to headless via config")
	case "desktop":
		hasDisplay = true
		log.Println("[mux] Mode forced to desktop via config")
	default:
		hasDisplay = runtime.GOOS == "darwin" || runtime.GOOS == "windows" ||
			os.Getenv("DISPLAY") != "" || os.Getenv("WAYLAND_DISPLAY") != ""
	}

	// --- Headless HTTP mode (no display server available) ---
	if !hasDisplay {
		log.Println("[mux] Starting in headless HTTP mode")

		registerConfigTools(s, cfg, tm)

		// OAuth routes for headless mode (browser-based /oauth/start +
		// /oauth/callback). The flow lives in internal/oauth and is available
		// in every build — notray builds included, which previously had no
		// OAuth at all because the code sat in the tag-guarded app.go.
		oauthMgr := oauth.NewManager(cfg, cfg.Server.Port, oauthAuthorizedMount(cfg, s))
		oauthRoutes := oauthMgr.Routes

		var localRoutes, tlsRoutes func(*http.ServeMux)
		if vlt != nil {
			vh := vault.NewVaultHandlers(vlt, waServer, approvalQueue)
			// Local: vault + SSH endpoints + OAuth. TLS: vault only (SSH is localhost-only for security).
			localRoutes = func(mux *http.ServeMux) {
				vh.Mount(mux)
				vh.MountSSH(mux)
				oauthRoutes(mux)
			}
			tlsRoutes = func(mux *http.ServeMux) { vh.Mount(mux) }
			log.Println("[mux] Vault HTTP endpoints registered on /vault/*")

			// Credential socket for git credential helper (Unix domain socket, localhost-only)
			credSock := vault.NewCredentialSocket(vlt, gitHostsFromConfig(cfg))
			go func() {
				if err := credSock.Listen(); err != nil {
					log.Printf("[vault] Credential socket error: %v", err)
				}
			}()
			defer credSock.Close()
		} else {
			// OAuth routes on local HTTP only — redirect_uri is always localhost.
			localRoutes = oauthRoutes
		}
		servers := startHTTPServer(s, cfg, localRoutes, tlsRoutes)

		// Block until SIGINT/SIGTERM
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
		<-sigCh
		log.Println("[mux] Shutting down...")
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		servers.Shutdown(shutdownCtx)
		cancel()
		return
	}

	// --- Desktop mode (Wails v3 + MCP HTTP) ---
	runDesktop(s, cfg, tm, ctx, cancel, vlt, waServer, approvalQueue)
}

// gitHostsFromConfig extracts git connection configs for the credential socket.
func gitHostsFromConfig(cfg *config.Config) []vault.GitHost {
	var hosts []vault.GitHost
	for _, c := range cfg.AllConnections() {
		if c.Type == config.TypeGit && c.Host != "" && c.User != "" {
			hosts = append(hosts, vault.GitHost{
				Host:      c.Host,
				Username:  c.User,
				SecretKey: c.Name + "-token",
			})
		}
	}
	return hosts
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

// httpServers holds one or two HTTP servers for coordinated shutdown.
type httpServers struct {
	http  *http.Server // always present: plain HTTP on localhost
	https *http.Server // nil when TLS is not configured
}

// Shutdown gracefully shuts down all servers in parallel.
func (s *httpServers) Shutdown(ctx context.Context) {
	var wg sync.WaitGroup
	n := 1
	if s.https != nil {
		n = 2
	}
	wg.Add(n)
	go func() {
		defer wg.Done()
		if err := s.http.Shutdown(ctx); err != nil {
			log.Printf("[mux] HTTP shutdown error: %v", err)
		}
	}()
	if s.https != nil {
		go func() {
			defer wg.Done()
			if err := s.https.Shutdown(ctx); err != nil {
				log.Printf("[mux] HTTPS shutdown error: %v", err)
			}
		}()
	}
	wg.Wait()
}

// startHTTPServer creates and starts the MCP HTTP server(s).
//
// Plain HTTP always listens on localhost:port for local MCP clients (including /mcp).
// When TLS is configured, a second HTTPS server listens on 0.0.0.0:tls_port
// for external endpoints (reachable via Tailscale/LAN). /mcp is NOT exposed externally.
//
// localRoutes are mounted on the HTTP server (localhost only).
// tlsRoutes are mounted on the HTTPS server (all interfaces). May be nil.
func startHTTPServer(s *server.MCPServer, cfg *config.Config, localRoutes, tlsRoutes func(*http.ServeMux)) *httpServers {
	httpMux := http.NewServeMux()

	mcpHandler := server.NewStreamableHTTPServer(s)
	httpMux.Handle("/mcp", mcpHandler)

	if localRoutes != nil {
		localRoutes(httpMux)
	}

	timeouts := func() (time.Duration, time.Duration, time.Duration) {
		return 10 * time.Second, 30 * time.Second, 60 * time.Second
	}

	// Plain HTTP: always localhost-only (MCP + local routes).
	//
	// Read and write deadlines are deliberately off here, unlike on the TLS
	// server. Both are absolute from the start of the request, and MCP does not
	// fit that shape: a tool call can legitimately run for minutes (a crawl, a
	// browser agent), and the notification stream is a GET that stays open for
	// the life of the session. A 60s write deadline silently killed both. This
	// listener is bound to loopback, and ReadHeaderTimeout still bounds a client
	// that connects and then dawdles over its headers.
	httpAddr := fmt.Sprintf("127.0.0.1:%d", cfg.Server.Port)
	rht, _, _ := timeouts()
	httpSrv := &http.Server{
		Addr:              httpAddr,
		Handler:           httpMux,
		ReadHeaderTimeout: rht,
		MaxHeaderBytes:    1 << 20,
	}
	go func() {
		log.Printf("[mux] Starting HTTP server on %s", httpAddr)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("[mux] HTTP server error: %v", err)
			p, _ := os.FindProcess(os.Getpid())
			p.Signal(syscall.SIGTERM)
		}
	}()
	log.Printf("[mux] MCP available at http://localhost:%d/mcp", cfg.Server.Port)

	servers := &httpServers{http: httpSrv}

	// HTTPS: only when TLS is configured, on all interfaces.
	// Separate mux with tlsRoutes only — /mcp is NOT exposed.
	if cfg.Server.TLSCert != "" && cfg.Server.TLSKey != "" {
		tlsPort := cfg.Server.EffectiveTLSPort()

		httpsMux := http.NewServeMux()
		if tlsRoutes != nil {
			tlsRoutes(httpsMux)
		}

		httpsAddr := fmt.Sprintf(":%d", tlsPort)
		rht, rt, wt := timeouts()
		httpsSrv := &http.Server{
			Addr:              httpsAddr,
			Handler:           httpsMux,
			ReadHeaderTimeout: rht,
			ReadTimeout:       rt,
			WriteTimeout:      wt,
			MaxHeaderBytes:    1 << 20,
			TLSConfig: &tls.Config{
				MinVersion: tls.VersionTLS12,
			},
		}
		go func() {
			certPath := config.ExpandHome(cfg.Server.TLSCert)
			keyPath := config.ExpandHome(cfg.Server.TLSKey)
			log.Printf("[mux] Starting HTTPS server on %s (TLS)", httpsAddr)
			if err := httpsSrv.ListenAndServeTLS(certPath, keyPath); err != nil && err != http.ErrServerClosed {
				log.Printf("[mux] HTTPS server error: %v", err)
				p, _ := os.FindProcess(os.Getpid())
				p.Signal(syscall.SIGTERM)
			}
		}()
		servers.https = httpsSrv
	}

	return servers
}
