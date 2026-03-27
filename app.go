//go:build !notray

package main

import (
	"context"
	"crypto/tls"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/lib/pq"
	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/smnhffmnn/mux/internal/config"
	"github.com/smnhffmnn/mux/internal/erp"
	"github.com/smnhffmnn/mux/internal/proxy"
	"github.com/smnhffmnn/mux/internal/tools"
)

// App is the Wails v3 service struct. All exported methods become frontend bindings.
type App struct {
	ctx       context.Context
	cfg       *config.Config
	version   string
	buildTime string
	startTime time.Time
	port      int
	mcpServer *server.MCPServer
	tm        *tunnelManager

	oauthMu      sync.Mutex
	pendingOAuth map[string]*pendingOAuthFlow

	deviceMu      sync.Mutex
	pendingDevice map[string]*pendingDeviceFlow

	// emitEvent sends a named event to the Wails frontend (set by main before app.Run).
	// Nil in stdio mode.
	emitEvent func(name string, data any)

	// toolsMu protects registeredTools and closers from concurrent access.
	toolsMu sync.Mutex

	// registeredTools tracks which MCP tool names are registered per connection,
	// so they can be removed on hot-reload.
	registeredTools map[string][]string

	// closers tracks io.Closers (e.g. sql.DB pools) per connection for cleanup on hot-reload.
	closers map[string][]io.Closer
}

type pendingOAuthFlow struct {
	service      string
	handler      *transport.OAuthHandler
	codeVerifier string
	tokenStore   *config.KeychainTokenStore
}

type pendingDeviceFlow struct {
	connName   string
	deviceCode string
	clientID   string
	scopes     string
	expiresAt  time.Time
}

// NewApp creates the App instance. Called before wails.Run().
func NewApp(cfg *config.Config, version, buildTime string, port int, mcpServer *server.MCPServer, tm *tunnelManager) *App {
	return &App{
		cfg:             cfg,
		version:         version,
		buildTime:       buildTime,
		startTime:       time.Now(),
		port:            port,
		mcpServer:       mcpServer,
		tm:              tm,
		pendingOAuth:    make(map[string]*pendingOAuthFlow),
		pendingDevice:   make(map[string]*pendingDeviceFlow),
		registeredTools: make(map[string][]string),
		closers:         make(map[string][]io.Closer),
	}
}


// --- Hot-reload: ToolReloader interface ---

// ReloadConnection re-registers MCP tools for a connection (implements tools.ToolReloader).
func (a *App) ReloadConnection(conn config.Connection) {
	a.registerConnectionTools(conn)
}

// UnloadConnection removes MCP tools for a connection (implements tools.ToolReloader).
func (a *App) UnloadConnection(name string) {
	a.unregisterConnectionTools(name)
}

// unregisterConnectionTools removes all MCP tools previously registered for a connection
// and closes any associated resources (e.g. sql.DB pools).
func (a *App) unregisterConnectionTools(connName string) {
	if a.mcpServer == nil {
		return
	}
	a.toolsMu.Lock()
	defer a.toolsMu.Unlock()
	if names, ok := a.registeredTools[connName]; ok && len(names) > 0 {
		a.mcpServer.DeleteTools(names...)
		log.Printf("[mux] Unregistered %d tools for %s", len(names), connName)
	}
	delete(a.registeredTools, connName)
	if closers, ok := a.closers[connName]; ok {
		for _, c := range closers {
			c.Close()
		}
		delete(a.closers, connName)
	}
}

// registerConnectionTools registers (or re-registers) MCP tools for a connection.
func (a *App) registerConnectionTools(conn config.Connection) {
	if a.mcpServer == nil {
		return
	}

	// Always unregister first to avoid duplicates
	a.unregisterConnectionTools(conn.Name)

	if !conn.Enabled() {
		return
	}

	if config.IsProxyType(conn.Type) {
		a.registerProxyConnection(conn)
		return
	}

	// Resolve tunnel dialer (fail-closed)
	var dialer tools.Dialer
	if conn.Tunnel != "" {
		t := a.tm.Get(conn.Tunnel)
		if t == nil {
			log.Printf("[mux] Skipping %q: tunnel %q not available", conn.Name, conn.Tunnel)
			return
		}
		dialer = t
	}

	names, closer, err := tools.RegisterConnection(a.mcpServer, conn, dialer)
	if err != nil {
		log.Printf("[mux] Warning: %s not available: %v", conn.Name, err)
		return
	}
	a.toolsMu.Lock()
	a.registeredTools[conn.Name] = names
	if closer != nil {
		a.closers[conn.Name] = append(a.closers[conn.Name], closer)
	}
	a.toolsMu.Unlock()
}

// registerProxyConnection handles hot-reload for proxy-type connections.
func (a *App) registerProxyConnection(conn config.Connection) {
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
				RedirectURI:  fmt.Sprintf("http://localhost:%d/oauth/callback", a.port),
				TokenStore:   adapter,
				PKCEEnabled:  true,
			},
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := proxy.RegisterMount(ctx, a.mcpServer, mount); err != nil {
			log.Printf("[mux] Warning: proxy %s not available: %v", conn.Name, err)
			return
		}
		// Track the registered tool names
		a.trackProxyTools(conn.Name)
	} else if conn.Token != "" {
		mount := proxy.Mount{
			Name:  conn.Name,
			URL:   conn.URL,
			Token: proxy.NewTokenProvider(conn.Token),
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := proxy.RegisterMount(ctx, a.mcpServer, mount); err != nil {
			log.Printf("[mux] Warning: proxy %s not available: %v", conn.Name, err)
			return
		}
		// Track the registered tool names
		a.trackProxyTools(conn.Name)
	}
}

// trackProxyTools discovers which tools were registered for a proxy connection
// by scanning the MCPServer's tool list for the connection prefix.
func (a *App) trackProxyTools(connName string) {
	prefix := connName + "_"
	var names []string
	for name := range a.mcpServer.ListTools() {
		if strings.HasPrefix(name, prefix) {
			names = append(names, name)
		}
	}
	if len(names) > 0 {
		a.toolsMu.Lock()
		a.registeredTools[connName] = names
		a.toolsMu.Unlock()
	}
}

// --- Connection type registry ---
// The single source of truth for connection types lives in config.AllTypes.
// See internal/config/types.go.

func allTypes() []TypeListEntry {
	var types []TypeListEntry
	for _, td := range config.AllTypes {
		types = append(types, TypeListEntry{Type: td.Type, Label: td.Label})
	}
	sort.Slice(types, func(i, j int) bool { return types[i].Label < types[j].Label })
	return types
}

func buildConnInfo(conn config.Connection) ConnInfo {
	td := config.LookupType(conn.Type)

	var summary string
	switch {
	case config.IsProxyType(conn.Type), conn.Type == "http", conn.Type == "firecrawl", conn.Type == "brave", conn.Type == "openai", conn.Type == "elevenlabs", conn.Type == "recraft", conn.Type == "ideogram":
		summary = conn.URL
	case conn.Type == "microsoft-graph":
		summary = "Microsoft Graph API"
	case conn.Type == "google-tagmanager":
		summary = "Google Tag Manager"
	default:
		if conn.Host != "" {
			summary = fmt.Sprintf("%s:%d/%s", conn.Host, conn.Port, conn.Database)
		}
	}

	var fields []FieldInfo
	if td != nil {
		for _, fd := range td.Fields {
			fi := FieldInfo{
				Key:         fd.Key,
				Label:       fd.Label,
				Placeholder: fd.Placeholder,
				Secret:      fd.Secret,
				Small:       fd.Small,
			}
			if !fd.Secret {
				switch fd.Key {
				case "host":
					fi.Value = conn.Host
				case "port":
					if conn.Port > 0 {
						fi.Value = fmt.Sprintf("%d", conn.Port)
					}
				case "user":
					fi.Value = conn.User
				case "database":
					fi.Value = conn.Database
				case "url":
					fi.Value = conn.URL
				case "scopes":
					fi.Value = conn.Scopes
				}
			}
			if fd.Secret {
				switch fd.Key {
				case "password":
					fi.SecretStored = conn.Password != ""
				case "token":
					fi.SecretStored = conn.Token != ""
				}
			}
			fields = append(fields, fi)
		}
	}

	isOAuth := config.IsProxyType(conn.Type) && conn.OAuth
	oauthOK := false
	if isOAuth {
		tokenStore := config.NewKeychainTokenStore(conn.Name)
		oauthOK = tokenStore.HasToken()
	}

	isDeviceAuth := conn.Type == "microsoft-graph"
	deviceAuthOK := false
	if isDeviceAuth {
		if rt, err := config.GetSecret(conn.Name + "-oauth-refresh-token"); err == nil && rt != "" {
			deviceAuthOK = true
		}
	}

	return ConnInfo{
		Name:         conn.Name,
		Type:         conn.Type,
		TypeLabel:    config.TypeLabel(conn.Type),
		Configured:   conn.Enabled(),
		Source:       conn.Source,
		Tunnel:       conn.Tunnel,
		Summary:      summary,
		IsProxy:      config.IsProxyType(conn.Type),
		IsOAuth:      isOAuth,
		OAuthOK:      oauthOK,
		IsERP:        conn.Source == "erp",
		IsDeviceAuth: isDeviceAuth,
		DeviceAuthOK: deviceAuthOK,
		ReadOnly:     conn.ReadOnly,
		Instructions: conn.Instructions,
		Fields:       fields,
	}
}

// ========== Read Bindings ==========

// GetPageData returns the full page state (connections, tunnels, ERP, server info).
func (a *App) GetPageData() PageData {
	data := PageData{
		Server: ServerInfo{
			Version:       a.version,
			Uptime:        time.Since(a.startTime).Round(time.Second).String(),
			Port:          a.port,
			BuildTime:     a.buildTime,
			CanSelfUpdate: selfUpdateURL() != "",
		},
		Types: allTypes(),
	}

	erpT, erpC := a.cfg.ERPStatus()
	data.ERP = ERPInfo{
		Configured:  a.cfg.HasERP(),
		Endpoint:    a.cfg.ERP.Endpoint,
		TokenSet:    a.cfg.ERP.Token != "",
		Tunnels:     erpT,
		Connections: erpC,
	}

	for _, t := range a.cfg.AllTunnels() {
		connected := false
		if a.tm != nil {
			connected = a.tm.IsUp(t.Name)
		}
		typ := "wireguard"
		if t.IsSSH() {
			typ = "ssh"
		}
		data.Tunnels = append(data.Tunnels, TunnelInfo{
			Name:          t.Name,
			Type:          typ,
			PeerEndpoint:  t.PeerEndpoint,
			TunnelAddress: t.TunnelAddress,
			Host:          t.Host,
			Port:          t.Port,
			User:          t.User,
			Source:        t.Source,
			Connected:     connected,
		})
	}

	for _, conn := range a.cfg.AllConnections() {
		data.Connections = append(data.Connections, buildConnInfo(conn))
	}
	sort.Slice(data.Connections, func(i, j int) bool {
		return strings.ToLower(data.Connections[i].Name) < strings.ToLower(data.Connections[j].Name)
	})

	return data
}

// GetServerInfo returns server metadata for the header.
func (a *App) GetServerInfo() ServerInfo {
	return ServerInfo{
		Version:       a.version,
		Uptime:        time.Since(a.startTime).Round(time.Second).String(),
		Port:          a.port,
		BuildTime:     a.buildTime,
		CanSelfUpdate: selfUpdateURL() != "",
	}
}

// GetConnectionTypes returns available connection types for the add dialog.
func (a *App) GetConnectionTypes() []TypeListEntry {
	return allTypes()
}

// GetConnection returns a single connection's info.
func (a *App) GetConnection(name string) *ConnInfo {
	conn := a.cfg.FindAnyConnection(name)
	if conn == nil {
		return nil
	}
	ci := buildConnInfo(*conn)
	return &ci
}

// ========== Write Bindings ==========

// AddConnection creates a new connection and returns its info.
func (a *App) AddConnection(name, typ string) (*ConnInfo, error) {
	name = strings.TrimSpace(strings.ToLower(strings.ReplaceAll(name, " ", "-")))
	if name == "" || typ == "" {
		return nil, fmt.Errorf("name and type are required")
	}

	if !config.ValidType(typ) {
		return nil, fmt.Errorf("unknown connection type: %s", typ)
	}

	if a.cfg.FindAnyConnection(name) != nil {
		return nil, fmt.Errorf("connection already exists: %s", name)
	}

	conn := config.Connection{
		Name:   name,
		Type:   typ,
		Source: "local",
	}
	config.ApplyConnectionDefaults(&conn)
	a.cfg.Connections = append(a.cfg.Connections, conn)

	if err := a.cfg.Save(); err != nil {
		log.Printf("[app] Warning: could not save config file: %v", err)
	}

	a.registerConnectionTools(conn)

	ci := buildConnInfo(conn)
	return &ci, nil
}

// SaveConnection saves connection fields and returns the updated info.
func (a *App) SaveConnection(name string, fields SaveConnectionRequest) (*ConnInfo, error) {
	conn := a.cfg.FindAnyConnection(name)
	if conn == nil {
		return nil, fmt.Errorf("connection not found: %s", name)
	}
	isERP := conn.Source == "erp"

	if !isERP {
		if fields.Host != "" {
			conn.Host = fields.Host
		}
		if fields.Port != "" {
			fmt.Sscanf(fields.Port, "%d", &conn.Port)
		}
		if fields.User != "" {
			conn.User = fields.User
		}
		if fields.Database != "" {
			conn.Database = fields.Database
		}
		if fields.URL != "" {
			conn.URL = fields.URL
		}
		if fields.Scopes != "" {
			conn.Scopes = fields.Scopes
		}
		conn.Tunnel = fields.Tunnel
		conn.Instructions = fields.Instructions
	}

	if fields.Password != "" {
		conn.Password = fields.Password
		if err := config.SaveSecret(conn.Name+"-password", fields.Password); err != nil {
			log.Printf("[app] Warning: could not save to keychain: %v", err)
		}
	}
	if fields.Token != "" {
		conn.Token = fields.Token
		if err := config.SaveSecret(conn.Name+"-token", fields.Token); err != nil {
			log.Printf("[app] Warning: could not save to keychain: %v", err)
		}
		if tp := proxy.GetTokenProvider(conn.Name); tp != nil {
			tp.Set(fields.Token)
		}
	}

	if !isERP {
		if err := a.cfg.Save(); err != nil {
			log.Printf("[app] Warning: could not save config file: %v", err)
		}
	}

	a.registerConnectionTools(*conn)

	ci := buildConnInfo(*conn)
	return &ci, nil
}

// DeleteConnection removes a local connection.
func (a *App) DeleteConnection(name string) error {
	found := false
	var newConns []config.Connection
	for _, c := range a.cfg.Connections {
		if c.Name == name {
			if c.Source == "erp" {
				return fmt.Errorf("ERP-managed connections cannot be deleted")
			}
			found = true
			continue
		}
		newConns = append(newConns, c)
	}

	if !found {
		return fmt.Errorf("connection not found: %s", name)
	}

	a.unregisterConnectionTools(name)
	a.cfg.Connections = newConns
	if err := a.cfg.Save(); err != nil {
		log.Printf("[app] Warning: could not save config file: %v", err)
	}
	return nil
}

// TunnelLookup resolves a hostname through a WireGuard tunnel's DNS.
// Only works for WireGuard tunnels (SSH tunnels do not provide DNS resolution).
func (a *App) TunnelLookup(tunnelName, hostname string) *TestResult {
	// DNS lookup is WireGuard-specific (gVisor netstack)
	wgTunnel := a.tm.wg.Get(tunnelName)
	if wgTunnel == nil {
		return &TestResult{Connection: tunnelName, Message: fmt.Sprintf("tunnel %q not available or not a WireGuard tunnel (DNS lookup requires WireGuard)", tunnelName)}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	start := time.Now()
	addrs, err := wgTunnel.LookupHost(ctx, hostname)
	if err != nil {
		return &TestResult{
			Connection: tunnelName,
			Message:    fmt.Sprintf("DNS lookup failed: %v", err),
			Latency:    time.Since(start).Round(time.Millisecond).String(),
		}
	}

	return &TestResult{
		Connection: tunnelName,
		Connected:  true,
		Message:    fmt.Sprintf("Resolved: %s", strings.Join(addrs, ", ")),
		Latency:    time.Since(start).Round(time.Millisecond).String(),
	}
}

// TestConnection tests a connection and returns the result.
func (a *App) TestConnection(name string) *TestResult {
	conn := a.cfg.FindAnyConnection(name)
	if conn == nil {
		return &TestResult{Connection: name, Message: "Connection not found"}
	}

	start := time.Now()
	var result testResponse

	switch conn.Type {
	case "mariadb":
		result = a.testMariaDB(*conn)
	case "clickhouse":
		result = a.testClickHouse(*conn)
	case "postgresql":
		result = a.testPostgreSQL(*conn)
	case "http":
		result = a.testHTTP(*conn)
	case "firecrawl":
		result = a.testFirecrawl(*conn)
	case "brave":
		result = a.testBrave(*conn)
	case "openai":
		result = a.testOpenAI(*conn)
	case "elevenlabs":
		result = a.testElevenLabs(*conn)
	case "recraft":
		result = a.testRecraft(*conn)
	case "ideogram":
		result = a.testIdeogram(*conn)
	case "microsoft-graph":
		result = a.testMicrosoftGraph(*conn)
	case "google-tagmanager":
		result = a.testGoogleTagManager(*conn)
	default:
		if config.IsProxyType(conn.Type) {
			if conn.OAuth {
				result = a.testOAuthProxy(*conn)
			} else {
				result = a.testBearerProxy(*conn)
			}
		} else {
			result = testResponse{Connection: conn.Name, Connected: false, Message: "Unknown type: " + conn.Type}
		}
	}

	return &TestResult{
		Connection: result.Connection,
		Connected:  result.Connected,
		Message:    result.Message,
		Latency:    time.Since(start).Round(time.Millisecond).String(),
	}
}

// SetupERP saves ERP endpoint and token.
func (a *App) SetupERP(endpoint, token string) (*ERPInfo, error) {
	if endpoint == "" && token == "" {
		return nil, fmt.Errorf("enter at least an endpoint or token")
	}

	if endpoint != "" {
		a.cfg.ERP.Endpoint = endpoint
	}
	if token != "" {
		a.cfg.ERP.Token = token
		if err := config.SaveSecret("provisioning-token", token); err != nil {
			log.Printf("[app] Warning: could not save ERP token to keychain: %v", err)
		}
	}
	if err := a.cfg.Save(); err != nil {
		log.Printf("[app] Warning: could not save config file: %v", err)
	}

	erpT, erpC := a.cfg.ERPStatus()
	return &ERPInfo{
		Configured:    a.cfg.HasERP(),
		Endpoint:      a.cfg.ERP.Endpoint,
		TokenSet:      a.cfg.ERP.Token != "",
		Tunnels:       erpT,
		Connections:   erpC,
		ResultMessage: "ERP settings saved.",
		ResultSuccess: true,
	}, nil
}

// SyncERP fetches config from the ERP and returns updated page data.
func (a *App) SyncERP() (*PageData, error) {
	if !a.cfg.HasERP() {
		return nil, fmt.Errorf("ERP not configured (endpoint or token missing)")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	resp, err := erp.Fetch(ctx, a.cfg.ERP.Endpoint, a.cfg.ERP.Token)
	if err != nil {
		return nil, fmt.Errorf("ERP sync failed: %w", err)
	}

	// Collect old ERP connection names before overwriting
	oldERP := make(map[string]bool)
	for _, c := range a.cfg.AllConnections() {
		if c.Source == "erp" {
			oldERP[c.Name] = true
		}
	}

	a.cfg.SetERP(resp.Tunnels, resp.Connections)
	log.Printf("[app] ERP sync: %d tunnels, %d connections", len(resp.Tunnels), len(resp.Connections))

	// Unregister connections that were removed from ERP
	for _, c := range resp.Connections {
		delete(oldERP, c.Name)
	}
	for name := range oldERP {
		a.unregisterConnectionTools(name)
	}

	// Register current connections
	for _, conn := range resp.Connections {
		a.registerConnectionTools(conn)
	}

	data := a.GetPageData()
	data.ERP.ResultMessage = fmt.Sprintf("Synced: %d tunnels, %d connections", len(resp.Tunnels), len(resp.Connections))
	data.ERP.ResultSuccess = true
	return &data, nil
}

// SelfUpdate downloads the platform-specific binary and replaces the current one.
func (a *App) SelfUpdate() *UpdateResult {
	updateURL := selfUpdateURL()
	if updateURL == "" {
		return &UpdateResult{Message: "Self-update is not supported on this platform"}
	}

	exePath, err := os.Executable()
	if err != nil {
		return &UpdateResult{Message: fmt.Sprintf("Cannot determine exe path: %v", err)}
	}
	exePath, _ = filepath.EvalSymlinks(exePath)

	log.Printf("[update] Downloading from %s", updateURL)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, updateURL, nil)
	httpClient := &http.Client{Timeout: 60 * time.Second}
	resp, err := httpClient.Do(req)
	if err != nil {
		return &UpdateResult{Message: fmt.Sprintf("Download failed: %v", err)}
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return &UpdateResult{Message: fmt.Sprintf("Download failed: HTTP %d", resp.StatusCode)}
	}

	newPath := exePath + ".new"
	oldPath := exePath + ".old"

	f, err := os.Create(newPath)
	if err != nil {
		return &UpdateResult{Message: fmt.Sprintf("Cannot write update: %v", err)}
	}

	written, err := io.Copy(f, resp.Body)
	f.Close()
	if err != nil {
		os.Remove(newPath)
		return &UpdateResult{Message: fmt.Sprintf("Download interrupted: %v", err)}
	}

	os.Chmod(newPath, 0755)

	// Swap: current → .old, new → current
	// On Windows the running .exe may be locked, but Rename (MoveFile) typically
	// works for the running executable on Windows (delete doesn't, but rename does).
	os.Remove(oldPath)
	if err := os.Rename(exePath, oldPath); err != nil {
		os.Remove(newPath)
		return &UpdateResult{Message: fmt.Sprintf("Cannot replace binary: %v", err)}
	}
	if err := os.Rename(newPath, exePath); err != nil {
		os.Rename(oldPath, exePath)
		return &UpdateResult{Message: fmt.Sprintf("Cannot rename update: %v", err)}
	}

	log.Printf("[update] Updated %s (%d bytes). Restart to use new version.", exePath, written)
	return &UpdateResult{
		Success: true,
		Message: fmt.Sprintf("Updated (%d KB). Restart mux to use new version.", written/1024),
	}
}

// ========== Auth Bindings ==========

// StartOAuth begins an OAuth flow and returns the auth URL.
func (a *App) StartOAuth(name string) (*OAuthStartResult, error) {
	conn := a.cfg.FindAnyConnection(name)
	if conn == nil || !conn.OAuth {
		return nil, fmt.Errorf("OAuth not supported for: %s", name)
	}

	mcpURL := conn.URL
	if mcpURL == "" {
		return nil, fmt.Errorf("URL not configured")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	metadataURL, err := discoverOAuthMetadata(ctx, mcpURL)
	if err != nil {
		return nil, fmt.Errorf("OAuth discovery failed: %w", err)
	}
	log.Printf("[oauth] Discovered metadata URL for %s: %s", conn.Name, metadataURL)

	redirectURI := fmt.Sprintf("http://localhost:%d/oauth/callback", a.port)
	tokenStore := config.NewKeychainTokenStore(conn.Name)
	adapter := proxy.NewKeychainTokenAdapter(tokenStore)
	clientID, clientSecret := config.LoadOAuthClientID(conn.Name)

	oauthCfg := transport.OAuthConfig{
		ClientID:              clientID,
		ClientSecret:          clientSecret,
		RedirectURI:           redirectURI,
		Scopes:                []string{},
		TokenStore:            adapter,
		PKCEEnabled:           true,
		AuthServerMetadataURL: metadataURL,
	}

	oauthHandler := transport.NewOAuthHandler(oauthCfg)
	oauthHandler.SetBaseURL(mcpURL)

	if clientID == "" {
		if err := oauthHandler.RegisterClient(ctx, "mux"); err != nil {
			return nil, fmt.Errorf("client registration failed: %w", err)
		}
		if err := config.SaveOAuthClient(conn.Name, oauthHandler.GetClientID(), oauthHandler.GetClientSecret()); err != nil {
			log.Printf("[app] Warning: could not save OAuth client to keychain: %v", err)
		}
		log.Printf("[oauth] Registered client for %s: %s", conn.Name, oauthHandler.GetClientID())
	}

	codeVerifier, err := transport.GenerateCodeVerifier()
	if err != nil {
		return nil, fmt.Errorf("failed to generate code verifier")
	}
	codeChallenge := transport.GenerateCodeChallenge(codeVerifier)

	state, err := transport.GenerateState()
	if err != nil {
		return nil, fmt.Errorf("failed to generate state")
	}

	authURL, err := oauthHandler.GetAuthorizationURL(ctx, state, codeChallenge)
	if err != nil {
		return nil, fmt.Errorf("failed to get auth URL: %w", err)
	}

	a.oauthMu.Lock()
	a.pendingOAuth[state] = &pendingOAuthFlow{
		service:      conn.Name,
		handler:      oauthHandler,
		codeVerifier: codeVerifier,
		tokenStore:   tokenStore,
	}
	a.oauthMu.Unlock()

	log.Printf("[oauth] Started OAuth flow for %s", conn.Name)
	return &OAuthStartResult{AuthURL: authURL}, nil
}

// GetOAuthStatus checks if an OAuth flow has completed.
func (a *App) GetOAuthStatus(name string) *OAuthStatus {
	conn := a.cfg.FindAnyConnection(name)
	if conn == nil {
		return &OAuthStatus{Message: "Connection not found"}
	}

	if config.IsProxyType(conn.Type) && conn.OAuth {
		tokenStore := config.NewKeychainTokenStore(conn.Name)
		if tokenStore.HasToken() {
			return &OAuthStatus{Authorized: true, Message: "Authorization successful!"}
		}
	}

	return &OAuthStatus{Message: "Waiting for authorization..."}
}

// StartDeviceAuth begins a device code auth flow (Microsoft Graph).
func (a *App) StartDeviceAuth(name string) (*DeviceAuthStart, error) {
	conn := a.cfg.FindAnyConnection(name)
	if conn == nil || conn.Type != "microsoft-graph" {
		return nil, fmt.Errorf("device auth not supported for: %s", name)
	}

	scopes := conn.Scopes
	if scopes == "" {
		scopes = "https://graph.microsoft.com/Mail.Read https://graph.microsoft.com/Mail.ReadWrite https://graph.microsoft.com/Mail.Send offline_access"
	}

	form := url.Values{
		"client_id": {tools.GraphClientID},
		"scope":     {scopes},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://login.microsoftonline.com/common/oauth2/v2.0/devicecode", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	httpClient := &http.Client{Timeout: 10 * time.Second}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("device code request failed: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("device code error (HTTP %d): %s", resp.StatusCode, string(body))
	}

	var result struct {
		DeviceCode      string `json:"device_code"`
		UserCode        string `json:"user_code"`
		VerificationURI string `json:"verification_uri"`
		ExpiresIn       int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("parse device code failed: %w", err)
	}

	a.deviceMu.Lock()
	a.pendingDevice[name] = &pendingDeviceFlow{
		connName:   name,
		deviceCode: result.DeviceCode,
		clientID:   tools.GraphClientID,
		scopes:     scopes,
		expiresAt:  time.Now().Add(time.Duration(result.ExpiresIn) * time.Second),
	}
	a.deviceMu.Unlock()

	log.Printf("[oauth] Started device code flow for %s", name)
	return &DeviceAuthStart{
		UserCode:        result.UserCode,
		VerificationURI: result.VerificationURI,
	}, nil
}

// GetDeviceAuthStatus checks if a device code auth flow has completed.
func (a *App) GetDeviceAuthStatus(name string) *DeviceAuthStatus {
	a.deviceMu.Lock()
	pending, ok := a.pendingDevice[name]
	a.deviceMu.Unlock()

	if !ok {
		if rt, err := config.GetSecret(name + "-oauth-refresh-token"); err == nil && rt != "" {
			return &DeviceAuthStatus{Completed: true, Message: "Authenticated!"}
		}
		return &DeviceAuthStatus{Message: "No pending authentication"}
	}

	if time.Now().After(pending.expiresAt) {
		a.deviceMu.Lock()
		delete(a.pendingDevice, name)
		a.deviceMu.Unlock()
		return &DeviceAuthStatus{Message: "Device code expired. Try again."}
	}

	form := url.Values{
		"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
		"client_id":   {pending.clientID},
		"device_code": {pending.deviceCode},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	httpClient := &http.Client{Timeout: 10 * time.Second}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://login.microsoftonline.com/common/oauth2/v2.0/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := httpClient.Do(req)
	if err != nil {
		return &DeviceAuthStatus{Message: "Waiting for authorization..."}
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	var tokenResp struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
		Error        string `json:"error"`
	}
	json.Unmarshal(body, &tokenResp)

	switch tokenResp.Error {
	case "authorization_pending":
		return &DeviceAuthStatus{Message: "Waiting for authorization..."}
	case "authorization_declined":
		a.deviceMu.Lock()
		delete(a.pendingDevice, name)
		a.deviceMu.Unlock()
		return &DeviceAuthStatus{Message: "Authorization declined."}
	case "expired_token":
		a.deviceMu.Lock()
		delete(a.pendingDevice, name)
		a.deviceMu.Unlock()
		return &DeviceAuthStatus{Message: "Device code expired. Try again."}
	case "":
		if err := config.SaveSecret(name+"-oauth-refresh-token", tokenResp.RefreshToken); err != nil {
			log.Printf("[oauth] Warning: could not save refresh token: %v", err)
		}
		a.deviceMu.Lock()
		delete(a.pendingDevice, name)
		a.deviceMu.Unlock()
		log.Printf("[oauth] Device code flow successful for %s", name)
		return &DeviceAuthStatus{Completed: true, Message: "Successfully authenticated!"}
	default:
		a.deviceMu.Lock()
		delete(a.pendingDevice, name)
		a.deviceMu.Unlock()
		return &DeviceAuthStatus{Message: fmt.Sprintf("Auth error: %s", tokenResp.Error)}
	}
}

// ========== OAuth Callback HTTP Handler ==========

// oauthCallbackHandler returns an http.HandlerFunc for the /oauth/callback route.
// Unexported so Wails does not expose it as a frontend binding.
func (a *App) oauthCallbackHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		code := r.URL.Query().Get("code")
		state := r.URL.Query().Get("state")

		if code == "" || state == "" {
			errorMsg := r.URL.Query().Get("error_description")
			if errorMsg == "" {
				errorMsg = r.URL.Query().Get("error")
			}
			if errorMsg == "" {
				errorMsg = "Missing code or state parameter"
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprint(w, callbackHTML("Error", "#f87171", errorMsg))
			return
		}

		a.oauthMu.Lock()
		pending, ok := a.pendingOAuth[state]
		if ok {
			delete(a.pendingOAuth, state)
		}
		a.oauthMu.Unlock()

		if !ok {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprint(w, callbackHTML("Error", "#f87171", "Unknown or expired OAuth state"))
			return
		}

		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		if err := pending.handler.ProcessAuthorizationResponse(ctx, code, state, pending.codeVerifier); err != nil {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprint(w, callbackHTML("Error", "#f87171", fmt.Sprintf("Token exchange failed: %v", err)))
			return
		}

		log.Printf("[oauth] Successfully authorized %s", pending.service)

		if a.mcpServer != nil {
			go a.mountOAuthProxy(pending.service, pending.tokenStore)
		}

		// Emit event to Wails frontend
		if a.emitEvent != nil {
			a.emitEvent("oauth:complete", pending.service)
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, callbackHTML("Success", "#34d399", fmt.Sprintf("Successfully authorized %s! You can close this tab.", pending.service)))
	}
}

func callbackHTML(status, color, message string) string {
	return fmt.Sprintf(`<!DOCTYPE html>
<html><head><title>mux - OAuth %s</title>
<style>body{background:#0f1117;color:#e2e4e9;font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',system-ui,sans-serif;display:flex;align-items:center;justify-content:center;height:100vh;margin:0}
.card{text-align:center;padding:2rem;border-radius:12px;border:1px solid #2a2d35}
h2{color:%s}</style></head>
<body><div class="card"><h2>%s</h2><p>%s</p></div></body></html>`, status, color, status, message)
}

func (a *App) mountOAuthProxy(connName string, tokenStore *config.KeychainTokenStore) {
	conn := a.cfg.FindAnyConnection(connName)
	if conn == nil || conn.URL == "" {
		return
	}

	adapter := proxy.NewKeychainTokenAdapter(tokenStore)
	clientID, clientSecret := config.LoadOAuthClientID(connName)

	mount := proxy.Mount{
		Name:       connName,
		URL:        conn.URL,
		TokenStore: tokenStore,
		OAuth: &transport.OAuthConfig{
			ClientID:     clientID,
			ClientSecret: clientSecret,
			RedirectURI:  fmt.Sprintf("http://localhost:%d/oauth/callback", a.port),
			TokenStore:   adapter,
			PKCEEnabled:  true,
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := proxy.RegisterMount(ctx, a.mcpServer, mount); err != nil {
		log.Printf("[oauth] Failed to mount %s after authorization: %v", connName, err)
	}
}

// ========== Test Connection Implementations ==========

type testResponse struct {
	Connection string
	Connected  bool
	Message    string
}

func (a *App) testMariaDB(conn config.Connection) testResponse {
	if !conn.Enabled() {
		return testResponse{Connection: conn.Name, Connected: false, Message: "Not configured"}
	}

	networkName := "tcp"
	if conn.Tunnel != "" {
		t := a.tm.Get(conn.Tunnel)
		if t == nil {
			return testResponse{Connection: conn.Name, Connected: false, Message: fmt.Sprintf("tunnel %q not available", conn.Tunnel)}
		}
		networkName = fmt.Sprintf("wg-test-%d", time.Now().UnixNano())
		mysql.RegisterDialContext(networkName, func(ctx context.Context, addr string) (net.Conn, error) {
			return t.DialContext(ctx, "tcp", addr)
		})
	}

	dsn := fmt.Sprintf("%s:%s@%s(%s:%d)/%s?timeout=5s",
		conn.User, conn.Password, networkName, conn.Host, conn.Port, conn.Database)

	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return testResponse{Connection: conn.Name, Connected: false, Message: err.Error()}
	}
	defer db.Close()

	if err := db.Ping(); err != nil {
		return testResponse{Connection: conn.Name, Connected: false, Message: err.Error()}
	}

	var version string
	db.QueryRow("SELECT VERSION()").Scan(&version)
	return testResponse{Connection: conn.Name, Connected: true, Message: "Connected: " + version}
}

func (a *App) testClickHouse(conn config.Connection) testResponse {
	if !conn.Enabled() {
		return testResponse{Connection: conn.Name, Connected: false, Message: "Not configured"}
	}

	var dialer tools.Dialer
	if conn.Tunnel != "" {
		t := a.tm.Get(conn.Tunnel)
		if t == nil {
			return testResponse{Connection: conn.Name, Connected: false, Message: fmt.Sprintf("tunnel %q not available", conn.Tunnel)}
		}
		dialer = t
	}

	db := tools.OpenClickHouseDB(conn, dialer)
	defer db.Close()

	if err := db.Ping(); err != nil {
		return testResponse{Connection: conn.Name, Connected: false, Message: err.Error()}
	}

	var version string
	db.QueryRow("SELECT version()").Scan(&version)
	return testResponse{Connection: conn.Name, Connected: true, Message: "Connected: ClickHouse " + version}
}

func (a *App) testPostgreSQL(conn config.Connection) testResponse {
	if !conn.Enabled() {
		return testResponse{Connection: conn.Name, Connected: false, Message: "Not configured"}
	}

	var dialer tools.Dialer
	if conn.Tunnel != "" {
		t := a.tm.Get(conn.Tunnel)
		if t == nil {
			return testResponse{Connection: conn.Name, Connected: false, Message: fmt.Sprintf("tunnel %q not available", conn.Tunnel)}
		}
		dialer = t
	}

	dsn := fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable&connect_timeout=5",
		url.QueryEscape(conn.User), url.QueryEscape(conn.Password),
		conn.Host, conn.Port, conn.Database)

	connector, err := pq.NewConnector(dsn)
	if err != nil {
		return testResponse{Connection: conn.Name, Connected: false, Message: err.Error()}
	}
	if dialer != nil {
		connector.Dialer(tools.NewPQDialerAdapter(dialer))
	}

	db := sql.OpenDB(connector)
	defer db.Close()

	if err := db.Ping(); err != nil {
		return testResponse{Connection: conn.Name, Connected: false, Message: err.Error()}
	}

	var version string
	db.QueryRow("SELECT version()").Scan(&version)
	return testResponse{Connection: conn.Name, Connected: true, Message: "Connected: " + version}
}

func (a *App) testHTTP(conn config.Connection) testResponse {
	if conn.URL == "" {
		return testResponse{Connection: conn.Name, Connected: false, Message: "URL not configured"}
	}

	testURL := conn.URL
	if conn.Source == "erp" && a.cfg.ERP.Endpoint != "" {
		testURL = a.cfg.ERP.Endpoint
	}

	httpClient := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}
	req, err := http.NewRequest(http.MethodGet, testURL, nil)
	if err != nil {
		return testResponse{Connection: conn.Name, Connected: false, Message: fmt.Sprintf("Invalid URL: %v", err)}
	}
	req.Header.Set("Accept", "application/json")
	if conn.Token != "" {
		req.Header.Set("Authorization", "Bearer "+conn.Token)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return testResponse{Connection: conn.Name, Connected: false, Message: fmt.Sprintf("Connection failed: %v", err)}
	}
	resp.Body.Close()

	return testResponse{Connection: conn.Name, Connected: true, Message: fmt.Sprintf("Connected: HTTP %d", resp.StatusCode)}
}

func (a *App) testFirecrawl(conn config.Connection) testResponse {
	if conn.Token == "" {
		return testResponse{Connection: conn.Name, Connected: false, Message: "API key not configured"}
	}

	apiURL := conn.URL
	if apiURL == "" {
		apiURL = "https://api.firecrawl.dev"
	}
	apiURL = strings.TrimRight(apiURL, "/")

	httpClient := &http.Client{Timeout: 10 * time.Second}
	reqBody := strings.NewReader(`{"url":"https://example.com","formats":["markdown"],"onlyMainContent":true}`)
	req, err := http.NewRequest(http.MethodPost, apiURL+"/v1/scrape", reqBody)
	if err != nil {
		return testResponse{Connection: conn.Name, Connected: false, Message: fmt.Sprintf("Invalid URL: %v", err)}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+conn.Token)

	resp, err := httpClient.Do(req)
	if err != nil {
		return testResponse{Connection: conn.Name, Connected: false, Message: fmt.Sprintf("Connection failed: %v", err)}
	}
	resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusPaymentRequired {
		return testResponse{Connection: conn.Name, Connected: false, Message: fmt.Sprintf("Authentication failed (HTTP %d) — check your API key", resp.StatusCode)}
	}
	return testResponse{Connection: conn.Name, Connected: true, Message: fmt.Sprintf("Connected: Firecrawl API (HTTP %d)", resp.StatusCode)}
}

func (a *App) testBrave(conn config.Connection) testResponse {
	if conn.Token == "" {
		return testResponse{Connection: conn.Name, Connected: false, Message: "API key not configured"}
	}

	apiURL := conn.URL
	if apiURL == "" {
		apiURL = "https://api.search.brave.com"
	}
	apiURL = strings.TrimRight(apiURL, "/")

	httpClient := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequest(http.MethodGet, apiURL+"/res/v1/web/search?q=test&count=1", nil)
	if err != nil {
		return testResponse{Connection: conn.Name, Connected: false, Message: fmt.Sprintf("Invalid URL: %v", err)}
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Subscription-Token", conn.Token)

	resp, err := httpClient.Do(req)
	if err != nil {
		return testResponse{Connection: conn.Name, Connected: false, Message: fmt.Sprintf("Connection failed: %v", err)}
	}
	resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return testResponse{Connection: conn.Name, Connected: false, Message: fmt.Sprintf("Authentication failed (HTTP %d) — check your API key", resp.StatusCode)}
	}
	return testResponse{Connection: conn.Name, Connected: true, Message: fmt.Sprintf("Connected: Brave Search API (HTTP %d)", resp.StatusCode)}
}

func (a *App) testOpenAI(conn config.Connection) testResponse {
	if conn.Token == "" {
		return testResponse{Connection: conn.Name, Connected: false, Message: "API key not configured"}
	}

	apiURL := conn.URL
	if apiURL == "" {
		apiURL = "https://api.openai.com"
	}
	apiURL = strings.TrimRight(apiURL, "/")

	httpClient := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequest(http.MethodGet, apiURL+"/v1/models", nil)
	if err != nil {
		return testResponse{Connection: conn.Name, Connected: false, Message: fmt.Sprintf("Invalid URL: %v", err)}
	}
	req.Header.Set("Authorization", "Bearer "+conn.Token)

	resp, err := httpClient.Do(req)
	if err != nil {
		return testResponse{Connection: conn.Name, Connected: false, Message: fmt.Sprintf("Connection failed: %v", err)}
	}
	resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return testResponse{Connection: conn.Name, Connected: false, Message: fmt.Sprintf("Authentication failed (HTTP %d) — check your API key", resp.StatusCode)}
	}
	return testResponse{Connection: conn.Name, Connected: true, Message: fmt.Sprintf("Connected: OpenAI API (HTTP %d)", resp.StatusCode)}
}

func (a *App) testElevenLabs(conn config.Connection) testResponse {
	if conn.Token == "" {
		return testResponse{Connection: conn.Name, Connected: false, Message: "API key not configured"}
	}

	apiURL := conn.URL
	if apiURL == "" {
		apiURL = "https://api.elevenlabs.io"
	}
	apiURL = strings.TrimRight(apiURL, "/")

	httpClient := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequest(http.MethodGet, apiURL+"/v1/user/subscription", nil)
	if err != nil {
		return testResponse{Connection: conn.Name, Connected: false, Message: fmt.Sprintf("Invalid URL: %v", err)}
	}
	req.Header.Set("xi-api-key", conn.Token)

	resp, err := httpClient.Do(req)
	if err != nil {
		return testResponse{Connection: conn.Name, Connected: false, Message: fmt.Sprintf("Connection failed: %v", err)}
	}
	resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return testResponse{Connection: conn.Name, Connected: false, Message: fmt.Sprintf("Authentication failed (HTTP %d) — check your API key", resp.StatusCode)}
	}
	return testResponse{Connection: conn.Name, Connected: true, Message: fmt.Sprintf("Connected: ElevenLabs API (HTTP %d)", resp.StatusCode)}
}

func (a *App) testRecraft(conn config.Connection) testResponse {
	if conn.Token == "" {
		return testResponse{Connection: conn.Name, Connected: false, Message: "API key not configured"}
	}

	apiURL := conn.URL
	if apiURL == "" {
		apiURL = "https://external.api.recraft.ai/v1"
	}
	apiURL = strings.TrimRight(apiURL, "/")

	httpClient := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequest(http.MethodGet, apiURL+"/users/me", nil)
	if err != nil {
		return testResponse{Connection: conn.Name, Connected: false, Message: fmt.Sprintf("Invalid URL: %v", err)}
	}
	req.Header.Set("Authorization", "Bearer "+conn.Token)

	resp, err := httpClient.Do(req)
	if err != nil {
		return testResponse{Connection: conn.Name, Connected: false, Message: fmt.Sprintf("Connection failed: %v", err)}
	}
	resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return testResponse{Connection: conn.Name, Connected: false, Message: fmt.Sprintf("Authentication failed (HTTP %d) — check your API key", resp.StatusCode)}
	}
	return testResponse{Connection: conn.Name, Connected: true, Message: fmt.Sprintf("Connected: Recraft API (HTTP %d)", resp.StatusCode)}
}

func (a *App) testIdeogram(conn config.Connection) testResponse {
	if conn.Token == "" {
		return testResponse{Connection: conn.Name, Connected: false, Message: "API key not configured"}
	}

	apiURL := conn.URL
	if apiURL == "" {
		apiURL = "https://api.ideogram.ai"
	}
	apiURL = strings.TrimRight(apiURL, "/")

	httpClient := &http.Client{Timeout: 10 * time.Second}
	// Ideogram has no simple GET health endpoint. POST an empty body to the
	// generate endpoint: 400 = authenticated (bad request), 401/403 = bad key.
	req, err := http.NewRequest(http.MethodPost, apiURL+"/v1/ideogram-v3/generate", strings.NewReader("{}"))
	if err != nil {
		return testResponse{Connection: conn.Name, Connected: false, Message: fmt.Sprintf("Invalid URL: %v", err)}
	}
	req.Header.Set("Api-Key", conn.Token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return testResponse{Connection: conn.Name, Connected: false, Message: fmt.Sprintf("Connection failed: %v", err)}
	}
	resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return testResponse{Connection: conn.Name, Connected: false, Message: fmt.Sprintf("Authentication failed (HTTP %d) — check your API key", resp.StatusCode)}
	}
	return testResponse{Connection: conn.Name, Connected: true, Message: fmt.Sprintf("Connected: Ideogram API (HTTP %d)", resp.StatusCode)}
}

func (a *App) testMicrosoftGraph(conn config.Connection) testResponse {
	rt, err := config.GetSecret(conn.Name + "-oauth-refresh-token")
	if err != nil || rt == "" {
		return testResponse{Connection: conn.Name, Connected: false, Message: "Not authenticated — use Authenticate button"}
	}

	httpClient := &http.Client{Timeout: 10 * time.Second}
	scopes := conn.Scopes
	if scopes == "" {
		scopes = "https://graph.microsoft.com/Mail.Read https://graph.microsoft.com/Mail.ReadWrite https://graph.microsoft.com/Mail.Send offline_access"
	}

	form := url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {tools.GraphClientID},
		"refresh_token": {rt},
		"scope":         {scopes},
	}
	tokenResp, err := httpClient.PostForm("https://login.microsoftonline.com/common/oauth2/v2.0/token", form)
	if err != nil {
		return testResponse{Connection: conn.Name, Connected: false, Message: "Token refresh failed: " + err.Error()}
	}
	defer tokenResp.Body.Close()

	body, _ := io.ReadAll(tokenResp.Body)
	var tok struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
	}
	json.Unmarshal(body, &tok)
	if tok.Error != "" || tok.AccessToken == "" {
		return testResponse{Connection: conn.Name, Connected: false, Message: "Token refresh failed — re-authenticate"}
	}

	req, _ := http.NewRequest(http.MethodGet, "https://graph.microsoft.com/v1.0/me?$select=displayName,mail", nil)
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	meResp, err := httpClient.Do(req)
	if err != nil {
		return testResponse{Connection: conn.Name, Connected: false, Message: "Graph API unreachable: " + err.Error()}
	}
	defer meResp.Body.Close()

	meBody, _ := io.ReadAll(meResp.Body)
	var me struct {
		DisplayName string `json:"displayName"`
		Mail        string `json:"mail"`
	}
	json.Unmarshal(meBody, &me)

	return testResponse{Connection: conn.Name, Connected: true, Message: fmt.Sprintf("Authenticated: %s (%s)", me.DisplayName, me.Mail)}
}

func (a *App) testGoogleTagManager(conn config.Connection) testResponse {
	if conn.Token == "" {
		return testResponse{Connection: conn.Name, Connected: false, Message: "Service account JSON key not configured"}
	}

	var sa struct {
		ClientEmail string `json:"client_email"`
		PrivateKey  string `json:"private_key"`
	}
	if err := json.Unmarshal([]byte(conn.Token), &sa); err != nil {
		return testResponse{Connection: conn.Name, Connected: false, Message: fmt.Sprintf("Invalid JSON: %v", err)}
	}
	if sa.ClientEmail == "" || sa.PrivateKey == "" {
		return testResponse{Connection: conn.Name, Connected: false, Message: "JSON must contain client_email and private_key"}
	}

	gtm, err := tools.NewGoogleTagManager(conn, nil)
	if err != nil {
		return testResponse{Connection: conn.Name, Connected: false, Message: err.Error()}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	result, err := gtm.Tools()[0].Handler(ctx, mcp.CallToolRequest{})
	if err != nil {
		return testResponse{Connection: conn.Name, Connected: false, Message: "API call failed: " + err.Error()}
	}

	for _, c := range result.Content {
		if tc, ok := c.(mcp.TextContent); ok {
			if strings.Contains(tc.Text, "error") && strings.Contains(tc.Text, "401") {
				return testResponse{Connection: conn.Name, Connected: false, Message: "Authentication failed — check service account permissions"}
			}
			return testResponse{Connection: conn.Name, Connected: true, Message: fmt.Sprintf("Connected: %s", sa.ClientEmail)}
		}
	}

	return testResponse{Connection: conn.Name, Connected: true, Message: fmt.Sprintf("Connected: %s", sa.ClientEmail)}
}

func (a *App) testBearerProxy(conn config.Connection) testResponse {
	if conn.URL == "" || conn.Token == "" {
		return testResponse{Connection: conn.Name, Connected: false, Message: "Not configured (URL or token missing)"}
	}

	headers := map[string]string{"Authorization": "Bearer " + conn.Token}
	t, err := transport.NewStreamableHTTP(conn.URL, transport.WithHTTPHeaders(headers))
	if err != nil {
		return testResponse{Connection: conn.Name, Connected: false, Message: fmt.Sprintf("Transport error: %v", err)}
	}

	c := client.NewClient(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := c.Start(ctx); err != nil {
		return testResponse{Connection: conn.Name, Connected: false, Message: fmt.Sprintf("Connection failed: %v", err)}
	}
	defer c.Close()

	initResult, err := c.Initialize(ctx, mcp.InitializeRequest{
		Params: mcp.InitializeParams{
			ClientInfo:      mcp.Implementation{Name: "mux", Version: "1.0.0"},
			ProtocolVersion: mcp.LATEST_PROTOCOL_VERSION,
		},
	})
	if err != nil {
		return testResponse{Connection: conn.Name, Connected: false, Message: fmt.Sprintf("MCP initialize failed: %v", err)}
	}

	toolsResult, err := c.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		return testResponse{Connection: conn.Name, Connected: true, Message: fmt.Sprintf("Connected (%s) — could not list tools: %v", initResult.ServerInfo.Name, err)}
	}

	return testResponse{Connection: conn.Name, Connected: true, Message: fmt.Sprintf("Connected: %s (%d tools)", initResult.ServerInfo.Name, len(toolsResult.Tools))}
}

func (a *App) testOAuthProxy(conn config.Connection) testResponse {
	if conn.URL == "" {
		return testResponse{Connection: conn.Name, Connected: false, Message: "URL not configured"}
	}

	tokenStore := config.NewKeychainTokenStore(conn.Name)
	if !tokenStore.HasToken() {
		return testResponse{Connection: conn.Name, Connected: false, Message: "Not authorized — click Authorize to connect"}
	}

	adapter := proxy.NewKeychainTokenAdapter(tokenStore)
	oauthCfg := transport.OAuthConfig{
		TokenStore:  adapter,
		PKCEEnabled: true,
	}

	c, err := client.NewOAuthStreamableHttpClient(conn.URL, oauthCfg)
	if err != nil {
		return testResponse{Connection: conn.Name, Connected: false, Message: fmt.Sprintf("Transport error: %v", err)}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if err := c.Start(ctx); err != nil {
		return testResponse{Connection: conn.Name, Connected: false, Message: fmt.Sprintf("Connection failed: %v", err)}
	}
	defer c.Close()

	initResult, err := c.Initialize(ctx, mcp.InitializeRequest{
		Params: mcp.InitializeParams{
			ClientInfo:      mcp.Implementation{Name: "mux", Version: "1.0.0"},
			ProtocolVersion: mcp.LATEST_PROTOCOL_VERSION,
		},
	})
	if err != nil {
		return testResponse{Connection: conn.Name, Connected: false, Message: fmt.Sprintf("MCP initialize failed: %v", err)}
	}

	toolsResult, err := c.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		return testResponse{Connection: conn.Name, Connected: true, Message: fmt.Sprintf("Connected (%s) — could not list tools: %v", initResult.ServerInfo.Name, err)}
	}

	return testResponse{Connection: conn.Name, Connected: true, Message: fmt.Sprintf("Connected: %s (%d tools)", initResult.ServerInfo.Name, len(toolsResult.Tools))}
}

// ========== OAuth Discovery ==========

func discoverOAuthMetadata(ctx context.Context, mcpURL string) (string, error) {
	httpClient := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, mcpURL, nil)
	if err != nil {
		return "", fmt.Errorf("create probe request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("probe MCP endpoint: %w", err)
	}
	defer resp.Body.Close()

	wwwAuth := resp.Header.Get("WWW-Authenticate")
	resourceMetaURL := extractParam(wwwAuth, "resource_metadata")

	if resourceMetaURL != "" {
		req2, _ := http.NewRequestWithContext(ctx, http.MethodGet, resourceMetaURL, nil)
		req2.Header.Set("Accept", "application/json")
		resp2, err := httpClient.Do(req2)
		if err == nil {
			defer resp2.Body.Close()
			if resp2.StatusCode == http.StatusOK {
				var resource struct {
					AuthorizationServers []string `json:"authorization_servers"`
				}
				if json.NewDecoder(resp2.Body).Decode(&resource) == nil && len(resource.AuthorizationServers) > 0 {
					authServer := resource.AuthorizationServers[0]
					return authServer + "/.well-known/oauth-authorization-server", nil
				}
			}
		}
	}

	parsed, err := url.Parse(mcpURL)
	if err != nil {
		return "", fmt.Errorf("parse MCP URL: %w", err)
	}
	origin := parsed.Scheme + "://" + parsed.Host
	return origin + "/.well-known/oauth-authorization-server", nil
}

func extractParam(header, param string) string {
	key := param + `="`
	idx := strings.Index(header, key)
	if idx < 0 {
		return ""
	}
	start := idx + len(key)
	end := strings.Index(header[start:], `"`)
	if end < 0 {
		return ""
	}
	return header[start : start+end]
}
