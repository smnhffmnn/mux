//go:build !notray

package main

import (
	"context"
	"crypto/tls"
	"database/sql"
	"encoding/json"
	"fmt"
	"html"
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

	"github.com/smnhffmnn/mux/docs/setup"
	"github.com/smnhffmnn/mux/internal/config"
	"github.com/smnhffmnn/mux/internal/provisioning"
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
	} else {
		// Token, custom-header, and/or no-auth mount — routed through the
		// connection's tunnel when set (fail-closed if it's unavailable).
		mount, ok := newProxyTokenMount(conn, conn.Token, a.tm)
		if !ok {
			log.Printf("[mux] Skipping proxy %q: tunnel %q not available", conn.Name, conn.Tunnel)
			return
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
// by scanning the MCPServer's tool list for the connection prefix. Registered
// tool names are sanitized (see config.SanitizeToolName), so the prefix must
// be sanitized the same way.
func (a *App) trackProxyTools(connName string) {
	prefix := config.SanitizeToolName(connName) + "_"
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
	sort.Slice(types, func(i, j int) bool {
		return strings.ToLower(types[i].Label) < strings.ToLower(types[j].Label)
	})
	return types
}

func buildConnInfo(conn config.Connection) ConnInfo {
	td := config.LookupType(conn.Type)

	var summary string
	switch {
	case config.IsProxyType(conn.Type), conn.Type == config.TypeHTTP, conn.Type == config.TypeFirecrawl, conn.Type == config.TypeBrave, conn.Type == config.TypeOpenAI, conn.Type == config.TypeElevenLabs, conn.Type == config.TypeRecraft, conn.Type == config.TypeIdeogram, conn.Type == config.TypeGemini, conn.Type == config.TypeHyperbrowser:
		summary = conn.URL
	case conn.Type == config.TypeMicrosoftGraph:
		summary = "Microsoft Graph API"
	case conn.Type == config.TypeGoogleTagManager:
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
				Multiline:   fd.Multiline,
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
				case "client_id":
					fi.Value = conn.ClientID
				case "scopes":
					fi.Value = conn.Scopes
				case "token_header":
					fi.Value = conn.TokenHeader
				case "token_scheme":
					fi.Value = conn.TokenScheme
				case "basic_suffix":
					fi.Value = conn.BasicSuffix
				case "headers":
					fi.Value = config.FormatHeaderLines(conn.Headers)
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

	isDeviceAuth := conn.Type == config.TypeMicrosoftGraph
	deviceAuthOK := false
	if isDeviceAuth {
		if rt, err := config.GetSecret(conn.Name + "-oauth-refresh-token"); err == nil && rt != "" {
			deviceAuthOK = true
		}
	}

	return ConnInfo{
		Name:          conn.Name,
		Type:          conn.Type,
		TypeLabel:     config.TypeLabel(conn.Type),
		Configured:    conn.Enabled(),
		Source:        conn.Source,
		Tunnel:        conn.Tunnel,
		Summary:       summary,
		IsProxy:       config.IsProxyType(conn.Type),
		IsOAuth:       isOAuth,
		OAuthOK:       oauthOK,
		IsProvisioned: conn.Source == config.SourceProvisioning,
		IsDeviceAuth:  isDeviceAuth,
		DeviceAuthOK:  deviceAuthOK,
		ReadOnly:      conn.ReadOnly,
		Instructions:  conn.Instructions,
		Fields:        fields,
	}
}

// ========== Read Bindings ==========

// GetPageData returns the full page state (connections, tunnels, provisioning, server info).
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
		// Initialize as empty (non-nil) slices: a nil Go slice marshals to JSON
		// `null`, which crashes the frontend's `.length`/`.map` access on a fresh
		// install with no tunnels/connections configured.
		Tunnels:     []TunnelInfo{},
		Connections: []ConnInfo{},
	}

	data.Provisioning = a.buildProvisioningInfo("", false)

	for _, t := range a.cfg.AllTunnels() {
		connected := false
		if a.tm != nil {
			connected = a.tm.IsUp(t.Name)
		}
		data.Tunnels = append(data.Tunnels, buildTunnelInfo(t, connected))
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

// GetSetupDoc returns the setup documentation markdown for a connection type.
// Returns an empty string if no documentation exists for the given type.
func (a *App) GetSetupDoc(typ string) string {
	return setup.Get(typ)
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
		Source: config.SourceLocal,
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

// invalidateOAuthTokens drops stored OAuth tokens for a connection. Called
// when a setting changes that makes existing tokens unusable (e.g. the Azure
// App Registration ID for microsoft-graph). Matches the keys actually used
// by the codebase — OAuth-proxy flows write "<name>-oauth-token" (the full
// token blob) and microsoft-graph / device-code flows write
// "<name>-oauth-refresh-token". Errors from DeleteSecret are ignored because
// the secret may not have been stored at all for a given connection.
func (a *App) invalidateOAuthTokens(connName string) {
	config.DeleteSecret(connName + "-oauth-token")
	config.DeleteSecret(connName + "-oauth-refresh-token")
	log.Printf("[app] Invalidated OAuth tokens for %q (configuration changed)", connName)
}

// SaveConnection saves connection fields and returns the updated info.
func (a *App) SaveConnection(name string, fields SaveConnectionRequest) (*ConnInfo, error) {
	conn := a.cfg.FindAnyConnection(name)
	if conn == nil {
		return nil, fmt.Errorf("connection not found: %s", name)
	}
	isProvisioned := conn.Source == config.SourceProvisioning

	if !isProvisioned {
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
		// Client ID change invalidates existing OAuth tokens (they were issued
		// against the old Azure app). Drop them proactively so the UI surfaces
		// the Reauthorize path instead of a zombie "token present, all calls 401".
		if fields.ClientID != "" && fields.ClientID != conn.ClientID {
			a.invalidateOAuthTokens(conn.Name)
			conn.ClientID = fields.ClientID
		}
		conn.Tunnel = fields.Tunnel
		conn.Instructions = fields.Instructions
		if fields.TokenHeader != "" && !config.ValidHeaderName(fields.TokenHeader) {
			return nil, fmt.Errorf("invalid token_header %q: must be a valid HTTP header name", fields.TokenHeader)
		}
		conn.TokenHeader = fields.TokenHeader
		if fields.TokenScheme != "" && fields.TokenScheme != "bearer" && fields.TokenScheme != "basic" {
			return nil, fmt.Errorf("invalid token_scheme %q: must be 'bearer' or 'basic'", fields.TokenScheme)
		}
		if fields.TokenScheme == "basic" && fields.TokenHeader != "" {
			return nil, fmt.Errorf("token_scheme=basic ignores token_header (Basic always uses the Authorization header) — set only one")
		}
		conn.TokenScheme = fields.TokenScheme
		conn.BasicSuffix = fields.BasicSuffix
		headers, err := config.ParseHeaderLines(fields.Headers)
		if err != nil {
			return nil, fmt.Errorf("invalid headers: %w", err)
		}
		conn.Headers = headers
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

	if !isProvisioned {
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
			if c.Source == config.SourceProvisioning {
				return fmt.Errorf("provisioned connections cannot be deleted")
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

// AddTunnel creates a new tunnel and returns its info.
func (a *App) AddTunnel(name, typ string) (*TunnelInfo, error) {
	name = strings.TrimSpace(strings.ToLower(strings.ReplaceAll(name, " ", "-")))
	if name == "" {
		return nil, fmt.Errorf("name is required")
	}

	if typ != config.TunnelTypeWireGuard && typ != config.TunnelTypeSSH {
		return nil, fmt.Errorf("type must be 'wireguard' or 'ssh'")
	}

	if a.cfg.FindAnyTunnel(name) != nil {
		return nil, fmt.Errorf("tunnel already exists: %s", name)
	}

	t := config.TunnelConfig{
		Name:   name,
		Type:   typ,
		Source: config.SourceLocal,
	}
	if typ == config.TunnelTypeSSH {
		t.Port = 22
	} else {
		t.MTU = 1420
		t.KeepAlive = 25
	}
	a.cfg.Tunnels = append(a.cfg.Tunnels, t)

	if err := a.cfg.Save(); err != nil {
		log.Printf("[app] Warning: could not save config file: %v", err)
	}

	info := buildTunnelInfo(t, false)
	return &info, nil
}

// SaveTunnel saves tunnel fields and returns the updated info.
func (a *App) SaveTunnel(name string, fields SaveTunnelRequest) (*TunnelInfo, error) {
	t := a.cfg.FindAnyTunnel(name)
	if t == nil {
		return nil, fmt.Errorf("tunnel not found: %s", name)
	}
	isProvisioned := t.Source == config.SourceProvisioning

	if !isProvisioned {
		// WireGuard fields
		if fields.PeerPublicKey != "" {
			t.PeerPublicKey = fields.PeerPublicKey
		}
		if fields.PeerEndpoint != "" {
			t.PeerEndpoint = fields.PeerEndpoint
		}
		if fields.AllowedIPs != "" {
			t.AllowedIPs = fields.AllowedIPs
		}
		if fields.TunnelAddress != "" {
			t.TunnelAddress = fields.TunnelAddress
		}
		if fields.DNS != "" {
			t.DNS = fields.DNS
		}
		if fields.MTU != "" {
			fmt.Sscanf(fields.MTU, "%d", &t.MTU)
		}
		if fields.KeepAlive != "" {
			fmt.Sscanf(fields.KeepAlive, "%d", &t.KeepAlive)
		}

		// SSH fields
		if fields.Host != "" {
			t.Host = fields.Host
		}
		if fields.Port != "" {
			fmt.Sscanf(fields.Port, "%d", &t.Port)
		}
		if fields.User != "" {
			t.User = fields.User
		}
		if fields.KeyFile != "" {
			t.KeyFile = fields.KeyFile
		}
		if fields.InsecureHostKey != nil {
			t.InsecureHostKey = *fields.InsecureHostKey
		}
	}

	// Secrets — always saveable (even for provisioned tunnels, to allow local key override)
	if fields.PrivateKey != "" {
		t.PrivateKey = fields.PrivateKey
		if err := config.SaveSecret("tunnel-"+name+"-private-key", fields.PrivateKey); err != nil {
			log.Printf("[app] Warning: could not save tunnel private key: %v", err)
		}
	}
	if fields.PresharedKey != "" {
		t.PresharedKey = fields.PresharedKey
		if err := config.SaveSecret("tunnel-"+name+"-preshared-key", fields.PresharedKey); err != nil {
			log.Printf("[app] Warning: could not save tunnel preshared key: %v", err)
		}
	}

	if !isProvisioned {
		if err := a.cfg.Save(); err != nil {
			log.Printf("[app] Warning: could not save config file: %v", err)
		}
	}

	connected := false
	if a.tm != nil {
		connected = a.tm.IsUp(name)
	}
	info := buildTunnelInfo(*t, connected)
	return &info, nil
}

// DeleteTunnel removes a local tunnel.
func (a *App) DeleteTunnel(name string) error {
	found := false
	var newTunnels []config.TunnelConfig
	for _, t := range a.cfg.Tunnels {
		if t.Name == name {
			if t.Source == config.SourceProvisioning {
				return fmt.Errorf("provisioned tunnels cannot be deleted")
			}
			found = true
			continue
		}
		newTunnels = append(newTunnels, t)
	}

	if !found {
		return fmt.Errorf("tunnel not found: %s", name)
	}

	a.cfg.Tunnels = newTunnels
	if err := a.cfg.Save(); err != nil {
		log.Printf("[app] Warning: could not save config file: %v", err)
	}

	// Best-effort cleanup of orphaned secrets
	config.DeleteSecret("tunnel-" + name + "-private-key")
	config.DeleteSecret("tunnel-" + name + "-preshared-key")

	return nil
}

// buildTunnelInfo creates a TunnelInfo from a TunnelConfig.
func buildTunnelInfo(t config.TunnelConfig, connected bool) TunnelInfo {
	typ := config.TunnelTypeWireGuard
	if t.IsSSH() {
		typ = config.TunnelTypeSSH
	}
	return TunnelInfo{
		Name:            t.Name,
		Type:            typ,
		PeerEndpoint:    t.PeerEndpoint,
		TunnelAddress:   t.TunnelAddress,
		PeerPublicKey:   t.PeerPublicKey,
		AllowedIPs:      t.AllowedIPs,
		DNS:             t.DNS,
		MTU:             t.MTU,
		KeepAlive:       t.KeepAlive,
		Host:            t.Host,
		Port:            t.Port,
		User:            t.User,
		KeyFile:         t.KeyFile,
		InsecureHostKey: t.InsecureHostKey,
		Source:          t.Source,
		Connected:       connected,
		PrivateKeySet:   t.PrivateKey != "",
		PresharedKeySet: t.PresharedKey != "",
	}
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
	case config.TypeMariaDB:
		result = a.testMariaDB(*conn)
	case config.TypeClickHouse:
		result = a.testClickHouse(*conn)
	case config.TypePostgreSQL:
		result = a.testPostgreSQL(*conn)
	case config.TypeHTTP:
		result = a.testHTTP(*conn)
	case config.TypeFirecrawl:
		result = a.testFirecrawl(*conn)
	case config.TypeBrave:
		result = a.testBrave(*conn)
	case config.TypeOpenAI:
		result = a.testOpenAI(*conn)
	case config.TypeElevenLabs:
		result = a.testElevenLabs(*conn)
	case config.TypeRecraft:
		result = a.testRecraft(*conn)
	case config.TypeIdeogram:
		result = a.testIdeogram(*conn)
	case config.TypeMicrosoftGraph:
		result = a.testMicrosoftGraph(*conn)
	case config.TypeGoogleTagManager:
		result = a.testGoogleTagManager(*conn)
	case config.TypeMeilisearch:
		result = a.testMeilisearch(*conn)
	case config.TypeYouTrackAgile:
		result = a.testYouTrackAgile(*conn)
	case config.TypeAsana:
		result = a.testAsana(*conn)
	case config.TypeGemini:
		result = a.testGemini(*conn)
	case config.TypeHyperbrowser:
		result = a.testHyperbrowser(*conn)
	case config.TypeIMAP:
		result = a.testIMAP(*conn)
	case config.TypeGit:
		result = a.testGit(*conn)
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

// SetupProvisioning saves provisioning endpoint and token.
func (a *App) SetupProvisioning(endpoint, token string) (*ProvisioningInfo, error) {
	if endpoint == "" && token == "" {
		return nil, fmt.Errorf("enter at least an endpoint or token")
	}
	if endpoint != "" {
		u, err := url.Parse(endpoint)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, fmt.Errorf("endpoint must be a valid http(s) URL")
		}
	}

	// Upsert the default (unnamed) endpoint. Multi-endpoint setups are configured
	// via config.toml or MCP provisioning_set tool with a name.
	defaultEp := a.cfg.FindProvisioning("")
	if defaultEp == nil {
		a.cfg.Provisioning = append(a.cfg.Provisioning, config.ProvisioningConfig{})
		defaultEp = &a.cfg.Provisioning[len(a.cfg.Provisioning)-1]
	}
	if endpoint != "" {
		defaultEp.Endpoint = endpoint
	}
	if token != "" {
		defaultEp.Token = token
		if err := config.SaveSecret(defaultEp.SecretKey(), token); err != nil {
			log.Printf("[app] Warning: could not save provisioning token to keychain: %v", err)
		}
	}
	if err := a.cfg.Save(); err != nil {
		log.Printf("[app] Warning: could not save config file: %v", err)
	}

	info := a.buildProvisioningInfo("Provisioning settings saved.", true)
	return &info, nil
}

// SyncProvisioning fetches config from every configured endpoint and returns updated page data.
func (a *App) SyncProvisioning() (*PageData, error) {
	if !a.cfg.HasProvisioning() {
		return nil, fmt.Errorf("provisioning not configured (no endpoint with token)")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Collect old provisioned connection names before overwriting (per endpoint).
	oldProvisioned := make(map[string]bool)
	for _, c := range a.cfg.AllConnections() {
		if c.Source == config.SourceProvisioning {
			oldProvisioned[c.Name] = true
		}
	}

	var totalTunnels, totalConns int
	var errs []string
	var toRegister []config.Connection
	for i := range a.cfg.Provisioning {
		p := a.cfg.Provisioning[i]
		if !p.Enabled() {
			continue
		}
		label := p.Name
		if label == "" {
			label = "default"
		}
		resp, err := provisioning.Fetch(ctx, p.Endpoint, p.Token)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", label, err))
			continue
		}
		a.cfg.SetProvisioned(p.Name, resp.Tunnels, resp.Connections)
		totalTunnels += len(resp.Tunnels)
		totalConns += len(resp.Connections)
		log.Printf("[app] Provisioning sync %q: %d tunnels, %d connections", label, len(resp.Tunnels), len(resp.Connections))

		for _, c := range resp.Connections {
			delete(oldProvisioned, c.Name)
		}
		toRegister = append(toRegister, resp.Connections...)
	}

	// Start any provisioned tunnels that aren't running yet BEFORE registering
	// connections — a freshly provisioned connection may reference a tunnel mux
	// has never started, and registration is fail-closed on missing tunnels.
	// Read the list back from cfg (not resp) so keychain-supplemented secrets
	// from SetProvisioned are in effect.
	for name, err := range a.tm.StartMissing(a.cfg.AllTunnels()) {
		log.Printf("[app] Provisioned tunnel %q failed to start: %v — connections using it will be skipped", name, err)
	}

	for _, conn := range toRegister {
		a.registerConnectionTools(conn)
	}

	// Unregister connections that were removed from any provisioning source.
	for name := range oldProvisioned {
		a.unregisterConnectionTools(name)
	}

	if len(errs) > 0 && totalConns == 0 && totalTunnels == 0 {
		return nil, fmt.Errorf("all provisioning endpoints failed: %s", strings.Join(errs, "; "))
	}

	data := a.GetPageData()
	msg := fmt.Sprintf("Synced: %d tunnels, %d connections", totalTunnels, totalConns)
	if len(errs) > 0 {
		msg += fmt.Sprintf(" (errors: %s)", strings.Join(errs, "; "))
	}
	data.Provisioning.ResultMessage = msg
	data.Provisioning.ResultSuccess = len(errs) == 0
	return &data, nil
}

// buildProvisioningInfo assembles the UI-facing view of provisioning status
// across all configured endpoints.
func (a *App) buildProvisioningInfo(resultMsg string, resultSuccess bool) ProvisioningInfo {
	totalT, totalC := a.cfg.ProvisioningStatus()
	info := ProvisioningInfo{
		Configured:    a.cfg.HasProvisioning(),
		Tunnels:       totalT,
		Connections:   totalC,
		ResultMessage: resultMsg,
		ResultSuccess: resultSuccess,
		// Non-nil so it marshals to JSON `[]`, not `null`. Without provisioning
		// endpoints configured the loop below never runs, leaving Endpoints nil,
		// which crashes the frontend's `endpoints.find`/`.length` access.
		Endpoints: []ProvisioningEndpointInfo{},
	}
	for _, p := range a.cfg.Provisioning {
		tc, cc := a.cfg.ProvisionedCountFor(p.Name)
		info.Endpoints = append(info.Endpoints, ProvisioningEndpointInfo{
			Name:        p.Name,
			Endpoint:    p.Endpoint,
			TokenSet:    p.Token != "",
			Tunnels:     tc,
			Connections: cc,
		})
	}
	return info
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

	var scopes []string
	if conn.Scopes != "" {
		for _, s := range strings.Split(conn.Scopes, " ") {
			s = strings.TrimSpace(s)
			if s != "" {
				scopes = append(scopes, s)
			}
		}
	}

	oauthCfg := transport.OAuthConfig{
		ClientID:              clientID,
		ClientSecret:          clientSecret,
		RedirectURI:           redirectURI,
		Scopes:                scopes,
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
	if conn == nil || conn.Type != config.TypeMicrosoftGraph {
		return nil, fmt.Errorf("device auth not supported for: %s", name)
	}
	if conn.ClientID == "" {
		return nil, fmt.Errorf("microsoft-graph connection %q: client_id (Azure App Registration ID) is required", name)
	}

	scopes := conn.Scopes
	if scopes == "" {
		scopes = tools.GraphDefaultScopes
	}

	form := url.Values{
		"client_id": {conn.ClientID},
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
		clientID:   conn.ClientID,
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
<body><div class="card"><h2>%s</h2><p>%s</p></div></body></html>`,
		html.EscapeString(status), color, html.EscapeString(status), html.EscapeString(message))
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
	if conn.Source == config.SourceProvisioning {
		if epName, ok := a.cfg.ConnectionEndpointName(conn.Name); ok {
			if ep := a.cfg.FindProvisioning(epName); ep != nil && ep.Endpoint != "" {
				testURL = ep.Endpoint
			}
		} else if len(a.cfg.Provisioning) > 0 && a.cfg.Provisioning[0].Endpoint != "" {
			testURL = a.cfg.Provisioning[0].Endpoint
		}
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

	if conn.ClientID == "" {
		return testResponse{Connection: conn.Name, Connected: false, Message: "client_id (Azure App Registration ID) not configured"}
	}
	httpClient := &http.Client{Timeout: 10 * time.Second}
	scopes := conn.Scopes
	if scopes == "" {
		scopes = tools.GraphDefaultScopes
	}

	form := url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {conn.ClientID},
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

func (a *App) testAsana(conn config.Connection) testResponse {
	if conn.Token == "" {
		return testResponse{Connection: conn.Name, Connected: false, Message: "Personal access token not configured"}
	}

	httpClient := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequest(http.MethodGet, "https://app.asana.com/api/1.0/users/me", nil)
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
		return testResponse{Connection: conn.Name, Connected: false, Message: fmt.Sprintf("Authentication failed (HTTP %d) — check your personal access token", resp.StatusCode)}
	}
	return testResponse{Connection: conn.Name, Connected: true, Message: fmt.Sprintf("Connected: Asana API (HTTP %d)", resp.StatusCode)}
}

func (a *App) testGemini(conn config.Connection) testResponse {
	if conn.Token == "" {
		return testResponse{Connection: conn.Name, Connected: false, Message: "API key not configured"}
	}

	apiURL := conn.URL
	if apiURL == "" {
		apiURL = "https://generativelanguage.googleapis.com/v1beta"
	}
	apiURL = strings.TrimRight(apiURL, "/")

	httpClient := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequest(http.MethodGet, apiURL+"/models", nil)
	if err != nil {
		return testResponse{Connection: conn.Name, Connected: false, Message: fmt.Sprintf("Invalid URL: %v", err)}
	}
	req.Header.Set("x-goog-api-key", conn.Token)

	resp, err := httpClient.Do(req)
	if err != nil {
		return testResponse{Connection: conn.Name, Connected: false, Message: fmt.Sprintf("Connection failed: %v", err)}
	}
	resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusBadRequest {
		return testResponse{Connection: conn.Name, Connected: false, Message: fmt.Sprintf("Authentication failed (HTTP %d) — check your API key", resp.StatusCode)}
	}
	return testResponse{Connection: conn.Name, Connected: true, Message: fmt.Sprintf("Connected: Gemini API (HTTP %d)", resp.StatusCode)}
}

func (a *App) testHyperbrowser(conn config.Connection) testResponse {
	if conn.Token == "" {
		return testResponse{Connection: conn.Name, Connected: false, Message: "API key not configured"}
	}

	apiURL := conn.URL
	if apiURL == "" {
		apiURL = "https://api.hyperbrowser.ai"
	}
	apiURL = strings.TrimRight(apiURL, "/")

	httpClient := &http.Client{Timeout: 10 * time.Second}
	// /api/sessions lists active sessions — auth check without spending scrape credits.
	req, err := http.NewRequest(http.MethodGet, apiURL+"/api/sessions", nil)
	if err != nil {
		return testResponse{Connection: conn.Name, Connected: false, Message: fmt.Sprintf("Invalid URL: %v", err)}
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("x-api-key", conn.Token)

	resp, err := httpClient.Do(req)
	if err != nil {
		return testResponse{Connection: conn.Name, Connected: false, Message: fmt.Sprintf("Connection failed: %v", err)}
	}
	resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return testResponse{Connection: conn.Name, Connected: false, Message: fmt.Sprintf("Authentication failed (HTTP %d) — check your API key", resp.StatusCode)}
	}
	return testResponse{Connection: conn.Name, Connected: true, Message: fmt.Sprintf("Connected: Hyperbrowser API (HTTP %d)", resp.StatusCode)}
}

func (a *App) testIMAP(conn config.Connection) testResponse {
	if conn.Host == "" || conn.User == "" {
		return testResponse{Connection: conn.Name, Connected: false, Message: "Host or user not configured"}
	}
	if conn.Password == "" {
		return testResponse{Connection: conn.Name, Connected: false, Message: "Password not configured"}
	}

	port := conn.Port
	if port == 0 {
		port = 993
	}
	addr := net.JoinHostPort(conn.Host, fmt.Sprintf("%d", port))

	d := &net.Dialer{Timeout: 10 * time.Second}
	rawConn, err := d.Dial("tcp", addr)
	if err != nil {
		return testResponse{Connection: conn.Name, Connected: false, Message: fmt.Sprintf("Connection failed: %v", err)}
	}

	tlsConn := tls.Client(rawConn, &tls.Config{ServerName: conn.Host})
	tlsConn.SetDeadline(time.Now().Add(10 * time.Second))
	if err := tlsConn.Handshake(); err != nil {
		rawConn.Close()
		return testResponse{Connection: conn.Name, Connected: false, Message: fmt.Sprintf("TLS handshake failed: %v", err)}
	}
	tlsConn.Close()

	return testResponse{Connection: conn.Name, Connected: true, Message: fmt.Sprintf("Connected: IMAP TLS (%s) — credentials not verified", addr)}
}

func (a *App) testGit(conn config.Connection) testResponse {
	if conn.Host == "" || conn.User == "" {
		return testResponse{Connection: conn.Name, Connected: false, Message: "Host or user not configured"}
	}
	// Git credentials are passive — no network call needed.
	// Just verify the token is stored in the secret store.
	if conn.Token == "" {
		return testResponse{Connection: conn.Name, Connected: false, Message: "Personal access token not configured in secret store"}
	}
	return testResponse{Connection: conn.Name, Connected: true, Message: fmt.Sprintf("Configured: %s@%s (token stored)", conn.User, conn.Host)}
}

func (a *App) testMeilisearch(conn config.Connection) testResponse {
	scheme := "http"
	if conn.Secure {
		scheme = "https"
	}
	baseURL := fmt.Sprintf("%s://%s:%d", scheme, conn.Host, conn.Port)

	httpClient := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequest(http.MethodGet, baseURL+"/health", nil)
	if err != nil {
		return testResponse{Connection: conn.Name, Connected: false, Message: fmt.Sprintf("Invalid URL: %v", err)}
	}
	if conn.Token != "" {
		req.Header.Set("Authorization", "Bearer "+conn.Token)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return testResponse{Connection: conn.Name, Connected: false, Message: fmt.Sprintf("Connection failed: %v", err)}
	}
	resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return testResponse{Connection: conn.Name, Connected: false, Message: fmt.Sprintf("Authentication failed (HTTP %d) — check your API key", resp.StatusCode)}
	}
	return testResponse{Connection: conn.Name, Connected: true, Message: fmt.Sprintf("Connected: Meilisearch (HTTP %d)", resp.StatusCode)}
}

func (a *App) testYouTrackAgile(conn config.Connection) testResponse {
	if conn.URL == "" || conn.Token == "" || conn.Database == "" {
		return testResponse{Connection: conn.Name, Connected: false, Message: "Not configured (URL, token, or board ID missing)"}
	}

	baseURL := strings.TrimRight(conn.URL, "/")
	httpClient := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequest(http.MethodGet, baseURL+"/api/agiles/"+conn.Database+"?fields=id,name", nil)
	if err != nil {
		return testResponse{Connection: conn.Name, Connected: false, Message: fmt.Sprintf("Invalid URL: %v", err)}
	}
	req.Header.Set("Authorization", "Bearer "+conn.Token)
	req.Header.Set("Accept", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return testResponse{Connection: conn.Name, Connected: false, Message: fmt.Sprintf("Connection failed: %v", err)}
	}
	resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return testResponse{Connection: conn.Name, Connected: false, Message: fmt.Sprintf("Authentication failed (HTTP %d) — check your permanent token", resp.StatusCode)}
	}
	if resp.StatusCode == http.StatusNotFound {
		return testResponse{Connection: conn.Name, Connected: false, Message: fmt.Sprintf("Board %q not found (HTTP 404) — check the board ID", conn.Database)}
	}
	return testResponse{Connection: conn.Name, Connected: true, Message: fmt.Sprintf("Connected: YouTrack Agile board %s (HTTP %d)", conn.Database, resp.StatusCode)}
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

// headlessOAuthRoutes returns a function that mounts OAuth routes (/oauth/start, /oauth/callback)
// for headless mode. In headless mode there is no Wails UI, so /oauth/start provides a browser-based
// entry point: GET /oauth/start?connection=<name> redirects to the provider's authorization page.
func headlessOAuthRoutes(cfg *config.Config, port int, mcpServer *server.MCPServer, tm *tunnelManager) func(*http.ServeMux) {
	app := NewApp(cfg, version, buildTime, port, mcpServer, tm)
	return func(mux *http.ServeMux) {
		mux.HandleFunc("/oauth/callback", app.oauthCallbackHandler())
		mux.HandleFunc("/oauth/start", func(w http.ResponseWriter, r *http.Request) {
			name := r.URL.Query().Get("connection")
			if name == "" {
				http.Error(w, "missing ?connection= parameter", http.StatusBadRequest)
				return
			}
			result, err := app.StartOAuth(name)
			if err != nil {
				log.Printf("[oauth] StartOAuth error for %q: %v", name, err)
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			http.Redirect(w, r, result.AuthURL, http.StatusFound)
		})
		log.Printf("[oauth] Headless OAuth routes mounted (/oauth/start, /oauth/callback)")
	}
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
