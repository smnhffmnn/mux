package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/smnhffmnn/mux/internal/config"
	"github.com/smnhffmnn/mux/internal/proxy"
)

// ToolReloader triggers hot-reload of MCP tools when connections change.
type ToolReloader interface {
	ReloadConnection(conn config.Connection)
	UnloadConnection(name string)
}

// ConfigTools holds MCP tools for querying and managing mux connections and secrets.
type ConfigTools struct {
	cfg      *config.Config
	reloader ToolReloader
}

// NewConfigTools creates the config management tool set.
// The reloader is called when connections are added, deleted, or become enabled via secret_set.
// Pass nil if hot-reload is not needed.
func NewConfigTools(cfg *config.Config, reloader ToolReloader) *ConfigTools {
	return &ConfigTools{cfg: cfg, reloader: reloader}
}

// Tools returns all config management tool definitions.
func (ct *ConfigTools) Tools() []ToolDef {
	return []ToolDef{
		ct.typeListTool(),
		ct.connectionListTool(),
		ct.connectionAddTool(),
		ct.connectionDeleteTool(),
		ct.tunnelAddTool(),
		ct.tunnelDeleteTool(),
		ct.secretSetTool(),
		ct.secretCheckTool(),
		ct.provisioningSetTool(),
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
	// Build type list dynamically from config.AllTypes
	var typeNames []string
	for _, td := range config.AllTypes {
		typeNames = append(typeNames, td.Type)
	}
	sort.Strings(typeNames)
	typeDesc := "Connection type: " + strings.Join(typeNames, ", ") + "."

	return ToolDef{
		Tool: mcp.NewTool("connection_add",
			mcp.WithDescription("Add a new connection. After creating, use secret_set to store the API key or password."),
			mcp.WithString("name", mcp.Required(),
				mcp.Description("Connection name (lowercase, hyphens allowed, e.g. 'my-firecrawl')."),
			),
			mcp.WithString("type", mcp.Required(),
				mcp.Description(typeDesc),
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
				mcp.Description("Name of a tunnel (WireGuard or SSH) to route this connection through."),
			),
			mcp.WithString("instructions",
				mcp.Description("Instructions for AI agents describing when/how to use this connection."),
			),
			mcp.WithString("token_header",
				mcp.Description("Custom header name for the API token, sent verbatim (http and proxy types), e.g. 'x-goog-api-key'. Default: sends as 'Authorization: Bearer {token}'. For Basic auth on a proxy, set this to 'Authorization' and store the token as 'Basic <base64>'."),
			),
			mcp.WithString("headers",
				mcp.Description("Extra HTTP headers sent with every request (http and proxy types), one 'Name: Value' per line (e.g. 'Notion-Version: 2022-06-28')."),
			),
		),
		Handler: ct.handleConnectionAdd,
	}
}

func (ct *ConfigTools) connectionDeleteTool() ToolDef {
	return ToolDef{
		Tool: mcp.NewTool("connection_delete",
			mcp.WithDescription("Delete a local connection. Provisioned connections cannot be deleted."),
			mcp.WithString("name", mcp.Required(),
				mcp.Description("Connection name to delete."),
			),
		),
		Handler: ct.handleConnectionDelete,
	}
}

func (ct *ConfigTools) tunnelAddTool() ToolDef {
	return ToolDef{
		Tool: mcp.NewTool("tunnel_add",
			mcp.WithDescription("Add a new tunnel (WireGuard or SSH). After creating, use secret_set to store the private key (tunnel-{name}-private-key) and optionally the preshared key (tunnel-{name}-preshared-key)."),
			mcp.WithString("name", mcp.Required(),
				mcp.Description("Tunnel name (lowercase, hyphens allowed, e.g. 'office-vpn')."),
			),
			mcp.WithString("type",
				mcp.Description("Tunnel type: 'wireguard' (default) or 'ssh'."),
			),
			// WireGuard fields
			mcp.WithString("peer_public_key",
				mcp.Description("WireGuard peer public key (base64)."),
			),
			mcp.WithString("peer_endpoint",
				mcp.Description("WireGuard peer endpoint (host:port)."),
			),
			mcp.WithString("allowed_ips",
				mcp.Description("WireGuard allowed IPs (CIDR, e.g. '10.100.0.0/16')."),
			),
			mcp.WithString("tunnel_address",
				mcp.Description("WireGuard local tunnel IP (CIDR, e.g. '10.100.0.42/32')."),
			),
			mcp.WithString("dns",
				mcp.Description("WireGuard DNS server (optional)."),
			),
			// SSH fields
			mcp.WithString("host",
				mcp.Description("SSH host (for ssh type)."),
			),
			mcp.WithNumber("port",
				mcp.Description("SSH port (default: 22)."),
			),
			mcp.WithString("user",
				mcp.Description("SSH user (for ssh type)."),
			),
			mcp.WithString("key_file",
				mcp.Description("Path to SSH private key file (optional, alternative to keychain)."),
			),
		),
		Handler: ct.handleTunnelAdd,
	}
}

func (ct *ConfigTools) tunnelDeleteTool() ToolDef {
	return ToolDef{
		Tool: mcp.NewTool("tunnel_delete",
			mcp.WithDescription("Delete a local tunnel. Provisioned tunnels cannot be deleted."),
			mcp.WithString("name", mcp.Required(),
				mcp.Description("Tunnel name to delete."),
			),
		),
		Handler: ct.handleTunnelDelete,
	}
}

func (ct *ConfigTools) secretSetTool() ToolDef {
	return ToolDef{
		Tool: mcp.NewTool("secret_set",
			mcp.WithDescription("Store a secret securely (OS keychain with file fallback). Write-only — the value can never be read back. Use '{connection-name}-token' for API keys or '{connection-name}-password' for database passwords."),
			mcp.WithString("key", mcp.Required(),
				mcp.Description("Keychain key, e.g. 'my-firecrawl-token', 'production-password', 'provisioning-token'."),
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
			mcp.WithDescription("Check which secrets are set (OS keychain with file fallback). Returns true/false per key — never the actual values."),
			mcp.WithString("connection",
				mcp.Description("Check secrets for a specific connection name. If omitted, checks all."),
			),
		),
		Handler: ct.handleSecretCheck,
	}
}

func (ct *ConfigTools) provisioningSetTool() ToolDef {
	return ToolDef{
		Tool: mcp.NewTool("provisioning_set",
			mcp.WithDescription("Set or add a remote provisioning endpoint. Multiple endpoints can be configured — use 'name' to distinguish them. The token is stored separately via secret_set (key 'provisioning-token' for the default/unnamed endpoint, or 'provisioning-<name>-token' for named endpoints). After setting both, mux fetches tunnels and connections from the endpoint on next startup."),
			mcp.WithString("endpoint", mcp.Required(),
				mcp.Description("Provisioning API URL (e.g. 'https://api.example.com/api/mux/provision')."),
			),
			mcp.WithString("name",
				mcp.Description("Optional name to identify this endpoint (required when configuring multiple endpoints). Leave empty for a single/default endpoint."),
			),
		),
		Handler: ct.handleProvisioningSet,
	}
}

// --- Handlers ---

func (ct *ConfigTools) handleTypeList(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var types []map[string]any
	for _, td := range config.AllTypes {
		fields := make([]string, len(td.Fields))
		for i, f := range td.Fields {
			fields[i] = f.Key
		}
		types = append(types, map[string]any{
			"type":   td.Type,
			"label":  td.Label,
			"fields": fields,
		})
	}
	return jsonResult(types)
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
		entry := map[string]any{
			"name":   t.Name,
			"source": t.Source,
		}
		if t.IsSSH() {
			entry["type"] = config.TunnelTypeSSH
			entry["host"] = t.Host
			entry["port"] = t.Port
			entry["user"] = t.User
			if t.KeyFile != "" {
				entry["key_file"] = t.KeyFile
			}
			entry["private_key_set"] = t.PrivateKey != ""
		} else {
			entry["type"] = config.TunnelTypeWireGuard
			entry["peer_endpoint"] = t.PeerEndpoint
			entry["tunnel_address"] = t.TunnelAddress
			if t.AllowedIPs != "" {
				entry["allowed_ips"] = t.AllowedIPs
			}
			entry["private_key_set"] = t.PrivateKey != ""
			entry["preshared_key_set"] = t.PresharedKey != ""
		}
		tunnels = append(tunnels, entry)
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

	// Validate type
	if !config.ValidType(typ) {
		return mcp.NewToolResultError(fmt.Sprintf("unknown connection type: %s", typ)), nil
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
		Source: config.SourceLocal,
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
	if v := req.GetString("token_header", ""); v != "" {
		if !config.ValidHeaderName(v) {
			return mcp.NewToolResultError(fmt.Sprintf("invalid token_header %q: must be a valid HTTP header name (e.g. 'x-goog-api-key')", v)), nil
		}
		conn.TokenHeader = v
	}
	if v := req.GetString("headers", ""); v != "" {
		headers, err := config.ParseHeaderLines(v)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("invalid headers: %v", err)), nil
		}
		conn.Headers = headers
	}

	config.ApplyConnectionDefaults(&conn)
	ct.cfg.Connections = append(ct.cfg.Connections, conn)

	if err := ct.cfg.Save(); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to save config: %v", err)), nil
	}

	if ct.reloader != nil {
		ct.reloader.ReloadConnection(conn)
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
			if c.Source == config.SourceProvisioning {
				return mcp.NewToolResultError(fmt.Sprintf("connection %q is provisioned and cannot be deleted", name)), nil
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

	if ct.reloader != nil {
		ct.reloader.UnloadConnection(name)
	}

	return mcp.NewToolResultText(fmt.Sprintf("connection %q deleted", name)), nil
}

func (ct *ConfigTools) handleTunnelAdd(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	name, err := req.RequireString("name")
	if err != nil {
		return mcp.NewToolResultError("missing required parameter: name"), nil
	}

	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return mcp.NewToolResultError("name cannot be empty"), nil
	}

	if ct.cfg.FindAnyTunnel(name) != nil {
		return mcp.NewToolResultError(fmt.Sprintf("tunnel %q already exists", name)), nil
	}

	typ := req.GetString("type", config.TunnelTypeWireGuard)
	if typ != config.TunnelTypeWireGuard && typ != config.TunnelTypeSSH {
		return mcp.NewToolResultError("type must be 'wireguard' or 'ssh'"), nil
	}

	t := config.TunnelConfig{
		Name:   name,
		Type:   typ,
		Source: config.SourceLocal,
	}

	if typ == config.TunnelTypeSSH {
		t.Host = req.GetString("host", "")
		if v, ok := req.GetArguments()["port"].(float64); ok && v > 0 {
			t.Port = int(v)
		}
		if t.Port == 0 {
			t.Port = 22
		}
		t.User = req.GetString("user", "")
		t.KeyFile = req.GetString("key_file", "")
	} else {
		t.PeerPublicKey = req.GetString("peer_public_key", "")
		t.PeerEndpoint = req.GetString("peer_endpoint", "")
		t.AllowedIPs = req.GetString("allowed_ips", "")
		t.TunnelAddress = req.GetString("tunnel_address", "")
		t.DNS = req.GetString("dns", "")
		t.MTU = 1420
		t.KeepAlive = 25
	}

	ct.cfg.Tunnels = append(ct.cfg.Tunnels, t)

	if err := ct.cfg.Save(); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to save config: %v", err)), nil
	}

	hint := fmt.Sprintf("tunnel %q (%s) created", name, typ)
	if typ == config.TunnelTypeSSH {
		hint += ". Use secret_set with key 'tunnel-" + name + "-private-key' to store the SSH private key (or set key_file)."
	} else {
		hint += ". Use secret_set with key 'tunnel-" + name + "-private-key' to store the WireGuard private key."
	}
	return mcp.NewToolResultText(hint), nil
}

func (ct *ConfigTools) handleTunnelDelete(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	name, err := req.RequireString("name")
	if err != nil {
		return mcp.NewToolResultError("missing required parameter: name"), nil
	}

	found := false
	var newTunnels []config.TunnelConfig
	for _, t := range ct.cfg.Tunnels {
		if t.Name == name {
			if t.Source == config.SourceProvisioning {
				return mcp.NewToolResultError(fmt.Sprintf("tunnel %q is provisioned and cannot be deleted", name)), nil
			}
			found = true
			continue
		}
		newTunnels = append(newTunnels, t)
	}

	if !found {
		return mcp.NewToolResultError(fmt.Sprintf("tunnel %q not found", name)), nil
	}

	ct.cfg.Tunnels = newTunnels

	if err := ct.cfg.Save(); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to save config: %v", err)), nil
	}

	// Best-effort cleanup of orphaned secrets
	config.DeleteSecret("tunnel-" + name + "-private-key")
	config.DeleteSecret("tunnel-" + name + "-preshared-key")

	// Warn about connections that still reference this tunnel
	var refs []string
	for _, c := range ct.cfg.AllConnections() {
		if c.Tunnel == name {
			refs = append(refs, c.Name)
		}
	}
	msg := fmt.Sprintf("tunnel %q deleted", name)
	if len(refs) > 0 {
		msg += fmt.Sprintf(". Warning: connections still referencing this tunnel (will be skipped): %s", strings.Join(refs, ", "))
	}

	return mcp.NewToolResultText(msg), nil
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
		return mcp.NewToolResultError("invalid key: must match '{name}-password', '{name}-token', 'provisioning-token', 'tunnel-{name}-private-key', or 'tunnel-{name}-preshared-key'"), nil
	}

	if err := config.SaveSecret(key, value); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to save secret: %v", err)), nil
	}

	// Update in-memory config
	ct.updateInMemorySecret(key, value)

	return mcp.NewToolResultText(fmt.Sprintf("secret %q stored", key)), nil
}

func (ct *ConfigTools) updateInMemorySecret(key, value string) {
	if strings.HasSuffix(key, "-password") {
		name := strings.TrimSuffix(key, "-password")
		if c := ct.cfg.FindAnyConnection(name); c != nil {
			c.Password = value
			if ct.reloader != nil {
				ct.reloader.ReloadConnection(*c)
			}
		}
	} else if strings.HasPrefix(key, "provisioning-") && strings.HasSuffix(key, "-token") {
		// Provisioning-endpoint token: either legacy "provisioning-token"
		// (unnamed/default endpoint) or named "provisioning-<name>-token".
		name := strings.TrimSuffix(strings.TrimPrefix(key, "provisioning-"), "-token")
		if p := ct.cfg.FindProvisioning(name); p != nil {
			p.Token = value
		} else if name == "" {
			// Legacy "provisioning-token" with no default endpoint configured.
			// If any endpoints exist, prefer writing to the first one (the
			// intuitive "default"); otherwise create a new default.
			if len(ct.cfg.Provisioning) > 0 {
				ct.cfg.Provisioning[0].Token = value
			} else {
				ct.cfg.Provisioning = []config.ProvisioningConfig{{Token: value}}
			}
		}
		// If name != "" and the named endpoint doesn't exist, silently drop —
		// the user needs to add the endpoint first via provisioning_set.
	} else if strings.HasSuffix(key, "-token") {
		name := strings.TrimSuffix(key, "-token")
		if c := ct.cfg.FindAnyConnection(name); c != nil {
			c.Token = value
			// Hot-reload: update the proxy's in-flight token
			if tp := proxy.GetTokenProvider(name); tp != nil {
				tp.Set(value)
			}
			if ct.reloader != nil {
				ct.reloader.ReloadConnection(*c)
			}
		}
	} else if strings.HasSuffix(key, "-oauth-client-id") || strings.HasSuffix(key, "-oauth-client-secret") {
		// OAuth client credentials changed — reload the connection so the proxy picks them up
		var name string
		if strings.HasSuffix(key, "-oauth-client-id") {
			name = strings.TrimSuffix(key, "-oauth-client-id")
		} else {
			name = strings.TrimSuffix(key, "-oauth-client-secret")
		}
		if c := ct.cfg.FindAnyConnection(name); c != nil && ct.reloader != nil {
			ct.reloader.ReloadConnection(*c)
		}
	}
}

func (ct *ConfigTools) handleSecretCheck(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	connFilter := req.GetString("connection", "")

	secrets := make(map[string]bool)

	if connFilter == "" {
		_, err := config.GetSecret("provisioning-token")
		secrets["provisioning-token"] = err == nil
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
		if c.Type == config.TypeMicrosoftGraph {
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

func (ct *ConfigTools) handleProvisioningSet(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	endpoint, err := req.RequireString("endpoint")
	if err != nil {
		return mcp.NewToolResultError("missing required parameter: endpoint"), nil
	}

	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return mcp.NewToolResultError("endpoint cannot be empty"), nil
	}

	name := strings.TrimSpace(req.GetString("name", ""))

	// Update existing entry with this name, or append new one.
	if p := ct.cfg.FindProvisioning(name); p != nil {
		p.Endpoint = endpoint
	} else {
		ct.cfg.Provisioning = append(ct.cfg.Provisioning, config.ProvisioningConfig{
			Name:     name,
			Endpoint: endpoint,
		})
	}

	if err := ct.cfg.Save(); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to save config: %v", err)), nil
	}

	p := ct.cfg.FindProvisioning(name)
	hasToken := p != nil && p.Token != ""

	label := "default (unnamed)"
	if name != "" {
		label = fmt.Sprintf("%q", name)
	}
	msg := fmt.Sprintf("provisioning endpoint %s set to %q", label, endpoint)
	if !hasToken {
		secretKey := "provisioning-token"
		if name != "" {
			secretKey = "provisioning-" + name + "-token"
		}
		msg += fmt.Sprintf(". Next: use secret_set with key %q to store the token. Provisioning will activate on next mux restart once both are set.", secretKey)
	} else {
		msg += ". Token is already set — provisioning will activate on next mux restart."
	}
	return mcp.NewToolResultText(msg), nil
}
