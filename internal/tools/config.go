package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/smnhffmnn/mux/internal/config"
	"github.com/smnhffmnn/mux/internal/proxy"
)

// ConfigTools holds MCP tools for querying and managing mux connections and secrets.
type ConfigTools struct {
	cfg *config.Config
}

// NewConfigTools creates the config management tool set.
func NewConfigTools(cfg *config.Config) *ConfigTools {
	return &ConfigTools{cfg: cfg}
}

// Tools returns all config management tool definitions.
func (ct *ConfigTools) Tools() []ToolDef {
	return []ToolDef{
		ct.typeListTool(),
		ct.connectionListTool(),
		ct.connectionAddTool(),
		ct.connectionDeleteTool(),
		ct.secretSetTool(),
		ct.secretCheckTool(),
	}
}

// --- Tool definitions ---

func (ct *ConfigTools) typeListTool() ToolDef {
	return ToolDef{
		Tool: mcp.NewTool("type_list",
			mcp.WithDescription("List all available connection types with their required fields."),
		),
		Handler: ct.handleTypeList,
	}
}

func (ct *ConfigTools) connectionListTool() ToolDef {
	return ToolDef{
		Tool: mcp.NewTool("connection_list",
			mcp.WithDescription("List all mux connections and tunnels. Shows type, source, and whether secrets are set — never the secret values themselves."),
		),
		Handler: ct.handleConnectionList,
	}
}

func (ct *ConfigTools) connectionAddTool() ToolDef {
	return ToolDef{
		Tool: mcp.NewTool("connection_add",
			mcp.WithDescription("Add a new connection. After creating, use secret_set to store the API key or password."),
			mcp.WithString("name", mcp.Required(),
				mcp.Description("Connection name (lowercase, hyphens allowed, e.g. 'my-firecrawl')."),
			),
			mcp.WithString("type", mcp.Required(),
				mcp.Description("Connection type: brave, clickhouse, firecrawl, http, mariadb, microsoft-graph, netdata, notion, postgresql, proxy, sentry, youtrack."),
			),
			mcp.WithString("url",
				mcp.Description("URL for the connection (required for proxy/http/api types, optional for types with defaults like firecrawl/brave)."),
			),
			mcp.WithString("host",
				mcp.Description("Database host (for mariadb, postgresql, clickhouse)."),
			),
			mcp.WithNumber("port",
				mcp.Description("Database port (defaults applied per type if omitted)."),
			),
			mcp.WithString("user",
				mcp.Description("Database user (for mariadb, postgresql, clickhouse)."),
			),
			mcp.WithString("database",
				mcp.Description("Database name (for mariadb, postgresql, clickhouse)."),
			),
			mcp.WithString("tunnel",
				mcp.Description("Name of a WireGuard tunnel to route this connection through."),
			),
			mcp.WithString("instructions",
				mcp.Description("Instructions for AI agents describing when/how to use this connection."),
			),
		),
		Handler: ct.handleConnectionAdd,
	}
}

func (ct *ConfigTools) connectionDeleteTool() ToolDef {
	return ToolDef{
		Tool: mcp.NewTool("connection_delete",
			mcp.WithDescription("Delete a local connection. ERP-managed connections cannot be deleted."),
			mcp.WithString("name", mcp.Required(),
				mcp.Description("Connection name to delete."),
			),
		),
		Handler: ct.handleConnectionDelete,
	}
}

func (ct *ConfigTools) secretSetTool() ToolDef {
	return ToolDef{
		Tool: mcp.NewTool("secret_set",
			mcp.WithDescription("Store a secret in the OS keychain. Write-only — the value can never be read back. Use '{connection-name}-token' for API keys or '{connection-name}-password' for database passwords."),
			mcp.WithString("key", mcp.Required(),
				mcp.Description("Keychain key, e.g. 'my-firecrawl-token', 'production-password', 'erp-token'."),
			),
			mcp.WithString("value", mcp.Required(),
				mcp.Description("Secret value to store."),
			),
		),
		Handler: ct.handleSecretSet,
	}
}

func (ct *ConfigTools) secretCheckTool() ToolDef {
	return ToolDef{
		Tool: mcp.NewTool("secret_check",
			mcp.WithDescription("Check which secrets are set in the OS keychain. Returns true/false per key — never the actual values."),
			mcp.WithString("connection",
				mcp.Description("Check secrets for a specific connection name. If omitted, checks all."),
			),
		),
		Handler: ct.handleSecretCheck,
	}
}

// --- Handlers ---

func (ct *ConfigTools) handleTypeList(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	types := []map[string]any{
		{"type": "brave", "label": "Brave Search", "fields": []string{"url (optional)", "token"}, "default_url": "https://api.search.brave.com"},
		{"type": "clickhouse", "label": "ClickHouse", "fields": []string{"host", "port", "user", "password", "database"}},
		{"type": "firecrawl", "label": "Firecrawl", "fields": []string{"url (optional)", "token"}, "default_url": "https://api.firecrawl.dev"},
		{"type": "google-tagmanager", "label": "Google Tag Manager", "fields": []string{"token (service account JSON key)"}},
		{"type": "http", "label": "HTTP API", "fields": []string{"url", "token (optional)"}},
		{"type": "mariadb", "label": "MariaDB", "fields": []string{"host", "port", "user", "password", "database"}},
		{"type": "microsoft-graph", "label": "Microsoft Graph", "fields": []string{"scopes (optional)"}, "auth": "device-code"},
		{"type": "netdata", "label": "Netdata", "fields": []string{"url", "token"}},
		{"type": "notion", "label": "Notion", "fields": []string{"url"}},
		{"type": "postgresql", "label": "PostgreSQL", "fields": []string{"host", "port", "user", "password", "database"}},
		{"type": "proxy", "label": "MCP Proxy (generic)", "fields": []string{"url", "token"}},
		{"type": "sentry", "label": "Sentry", "fields": []string{"url"}, "auth": "oauth"},
		{"type": "youtrack", "label": "YouTrack", "fields": []string{"url", "token"}},
	}

	data, _ := json.MarshalIndent(types, "", "  ")
	return mcp.NewToolResultText(string(data)), nil
}

func (ct *ConfigTools) handleConnectionList(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	result := make(map[string]any)

	// Connections
	var conns []map[string]any
	for _, c := range ct.cfg.AllConnections() {
		entry := map[string]any{
			"name":   c.Name,
			"type":   c.Type,
			"source": c.Source,
		}
		if c.Host != "" {
			entry["host"] = c.Host
		}
		if c.Port != 0 {
			entry["port"] = c.Port
		}
		if c.User != "" {
			entry["user"] = c.User
		}
		if c.Database != "" {
			entry["database"] = c.Database
		}
		if c.URL != "" {
			entry["url"] = c.URL
		}
		if c.ReadOnly {
			entry["read_only"] = true
		}
		if c.Tunnel != "" {
			entry["tunnel"] = c.Tunnel
		}
		if c.Instructions != "" {
			entry["instructions"] = c.Instructions
		}
		entry["secrets_set"] = map[string]bool{
			"password": c.Password != "",
			"token":    c.Token != "",
		}
		conns = append(conns, entry)
	}
	if conns == nil {
		conns = []map[string]any{}
	}
	result["connections"] = conns

	// Tunnels
	var tunnels []map[string]any
	for _, t := range ct.cfg.AllTunnels() {
		tunnels = append(tunnels, map[string]any{
			"name":           t.Name,
			"peer_endpoint":  t.PeerEndpoint,
			"tunnel_address": t.TunnelAddress,
			"source":         t.Source,
		})
	}
	if tunnels == nil {
		tunnels = []map[string]any{}
	}
	result["tunnels"] = tunnels

	data, _ := json.MarshalIndent(result, "", "  ")
	return mcp.NewToolResultText(string(data)), nil
}

func (ct *ConfigTools) handleConnectionAdd(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	name, err := req.RequireString("name")
	if err != nil {
		return mcp.NewToolResultError("missing required parameter: name"), nil
	}
	typ, err := req.RequireString("type")
	if err != nil {
		return mcp.NewToolResultError("missing required parameter: type"), nil
	}

	// Validate name
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return mcp.NewToolResultError("name cannot be empty"), nil
	}

	// Check duplicate
	if ct.cfg.FindConnection(name) != nil {
		return mcp.NewToolResultError(fmt.Sprintf("connection %q already exists", name)), nil
	}

	conn := config.Connection{
		Name:   name,
		Type:   typ,
		Source: "local",
	}

	// Optional fields
	if v := req.GetString("url", ""); v != "" {
		conn.URL = v
	}
	if v := req.GetString("host", ""); v != "" {
		conn.Host = v
	}
	if v, ok := req.GetArguments()["port"].(float64); ok && v > 0 {
		conn.Port = int(v)
	}
	if v := req.GetString("user", ""); v != "" {
		conn.User = v
	}
	if v := req.GetString("database", ""); v != "" {
		conn.Database = v
	}
	if v := req.GetString("tunnel", ""); v != "" {
		conn.Tunnel = v
	}
	if v := req.GetString("instructions", ""); v != "" {
		conn.Instructions = v
	}

	config.ApplyConnectionDefaults(&conn)
	ct.cfg.Connections = append(ct.cfg.Connections, conn)

	if err := ct.cfg.Save(); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to save config: %v", err)), nil
	}

	return mcp.NewToolResultText(fmt.Sprintf("connection %q (%s) created", name, typ)), nil
}

func (ct *ConfigTools) handleConnectionDelete(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	name, err := req.RequireString("name")
	if err != nil {
		return mcp.NewToolResultError("missing required parameter: name"), nil
	}

	found := false
	var newConns []config.Connection
	for _, c := range ct.cfg.Connections {
		if c.Name == name {
			if c.Source == "erp" {
				return mcp.NewToolResultError(fmt.Sprintf("connection %q is ERP-managed and cannot be deleted", name)), nil
			}
			found = true
			continue
		}
		newConns = append(newConns, c)
	}

	if !found {
		return mcp.NewToolResultError(fmt.Sprintf("connection %q not found", name)), nil
	}

	ct.cfg.Connections = newConns

	if err := ct.cfg.Save(); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to save config: %v", err)), nil
	}

	return mcp.NewToolResultText(fmt.Sprintf("connection %q deleted", name)), nil
}

func (ct *ConfigTools) handleSecretSet(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	key, err := req.RequireString("key")
	if err != nil {
		return mcp.NewToolResultError("missing required parameter: key"), nil
	}
	value, err := req.RequireString("value")
	if err != nil {
		return mcp.NewToolResultError("missing required parameter: value"), nil
	}

	if !config.ValidSecretKey(key) {
		return mcp.NewToolResultError("invalid key: must match '{name}-password', '{name}-token', 'erp-token', 'tunnel-{name}-private-key', or 'tunnel-{name}-preshared-key'"), nil
	}

	if err := config.SaveSecret(key, value); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to save secret: %v", err)), nil
	}

	// Update in-memory config
	ct.updateInMemorySecret(key, value)

	return mcp.NewToolResultText(fmt.Sprintf("secret %q stored in keychain", key)), nil
}

func (ct *ConfigTools) updateInMemorySecret(key, value string) {
	if strings.HasSuffix(key, "-password") {
		name := strings.TrimSuffix(key, "-password")
		if c := ct.cfg.FindConnection(name); c != nil {
			c.Password = value
		}
	} else if key != "erp-token" && strings.HasSuffix(key, "-token") {
		name := strings.TrimSuffix(key, "-token")
		if c := ct.cfg.FindConnection(name); c != nil {
			c.Token = value
		}
		// Hot-reload: update the proxy's in-flight token
		if tp := proxy.GetTokenProvider(name); tp != nil {
			tp.Set(value)
		}
	}
}

func (ct *ConfigTools) handleSecretCheck(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	connFilter := req.GetString("connection", "")

	secrets := make(map[string]bool)

	if connFilter == "" {
		_, err := config.GetSecret("erp-token")
		secrets["erp-token"] = err == nil
	}

	for _, c := range ct.cfg.AllConnections() {
		if connFilter != "" && c.Name != connFilter {
			continue
		}
		_, err := config.GetSecret(c.Name + "-password")
		secrets[c.Name+"-password"] = err == nil

		_, err = config.GetSecret(c.Name + "-token")
		secrets[c.Name+"-token"] = err == nil

		if c.OAuth {
			_, err = config.GetSecret(c.Name + "-oauth-token")
			secrets[c.Name+"-oauth-token"] = err == nil
		}
		if c.Type == "microsoft-graph" {
			_, err = config.GetSecret(c.Name + "-oauth-refresh-token")
			secrets[c.Name+"-oauth-refresh-token"] = err == nil
		}
	}

	if connFilter == "" {
		for _, t := range ct.cfg.AllTunnels() {
			_, err := config.GetSecret("tunnel-" + t.Name + "-private-key")
			secrets["tunnel-"+t.Name+"-private-key"] = err == nil

			_, err = config.GetSecret("tunnel-" + t.Name + "-preshared-key")
			secrets["tunnel-"+t.Name+"-preshared-key"] = err == nil
		}
	}

	data, _ := json.MarshalIndent(secrets, "", "  ")
	return mcp.NewToolResultText(string(data)), nil
}
