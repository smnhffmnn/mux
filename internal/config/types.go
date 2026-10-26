package config

import "sync"

// TypeField defines a field shown in the UI for configuring a connection.
type TypeField struct {
	Key         string
	Label       string
	Placeholder string
	Secret      bool
	Small       bool
	Multiline   bool // render as textarea (e.g. header lines)
	Checkbox    bool // boolean switch; the value is "true" or ""
}

// TypeDef defines a connection type with its UI metadata and fields.
// This is the SINGLE SOURCE OF TRUTH for all known connection types.
type TypeDef struct {
	Type   string      // e.g. "postgresql", "recraft"
	Label  string      // e.g. "PostgreSQL", "Recraft"
	Fields []TypeField // fields shown in the add/edit UI
}

// AllTypes is the single source of truth for all connection types.
// Do not modify at runtime — treat as read-only.
// When adding a new connection type, add it here and implement the
// handler in internal/tools (RegisterConnection switch).
var AllTypes = []TypeDef{
	{Type: TypePostgreSQL, Label: "PostgreSQL", Fields: []TypeField{
		{Key: "host", Label: "Host", Placeholder: "localhost"},
		{Key: "port", Label: "Port", Placeholder: "5432", Small: true},
		{Key: "user", Label: "User", Placeholder: "postgres"},
		{Key: "password", Label: "Password", Placeholder: "password", Secret: true},
		{Key: "database", Label: "Database", Placeholder: "postgres"},
	}},
	{Type: TypeClickHouse, Label: "ClickHouse", Fields: []TypeField{
		{Key: "host", Label: "Host", Placeholder: "localhost"},
		{Key: "port", Label: "Port", Placeholder: "8123", Small: true},
		{Key: "user", Label: "User", Placeholder: "default"},
		{Key: "password", Label: "Password", Placeholder: "password", Secret: true},
		{Key: "database", Label: "Default Database", Placeholder: "default"},
	}},
	{Type: TypeMariaDB, Label: "MariaDB", Fields: []TypeField{
		{Key: "host", Label: "Host", Placeholder: "localhost"},
		{Key: "port", Label: "Port", Placeholder: "3306", Small: true},
		{Key: "user", Label: "User", Placeholder: "root"},
		{Key: "password", Label: "Password", Placeholder: "password", Secret: true},
		{Key: "database", Label: "Database", Placeholder: "mydb"},
	}},
	{Type: TypeProxy, Label: "MCP Proxy (generic)", Fields: []TypeField{
		{Key: "url", Label: "MCP URL", Placeholder: "https://example.com/mcp"},
		{Key: "token", Label: "Token", Placeholder: "perm:... (or your raw token for Basic)", Secret: true},
		{Key: "token_scheme", Label: "Token Scheme (optional)", Placeholder: "bearer (default) | basic"},
		{Key: "basic_suffix", Label: "Basic Suffix (optional)", Placeholder: "non-secret constant after 'token:' when scheme=basic (e.g. 'token')"},
		{Key: "token_header", Label: "Token Header (optional)", Placeholder: "Authorization: Bearer (default)"},
		{Key: "headers", Label: "Headers (optional)", Placeholder: "One \"Name: Value\" per line", Multiline: true},
	}},
	{Type: TypeYouTrack, Label: "YouTrack (MCP proxy)", Fields: []TypeField{
		{Key: "url", Label: "MCP URL", Placeholder: "https://instance.myjetbrains.com/mcp"},
		{Key: "token", Label: "Token", Placeholder: "perm:...", Secret: true},
	}},
	{Type: TypeSentry, Label: "Sentry", Fields: []TypeField{
		{Key: "url", Label: "MCP URL", Placeholder: "https://mcp.sentry.dev/mcp"},
	}},
	{Type: TypeNetdata, Label: "Netdata", Fields: []TypeField{
		{Key: "url", Label: "MCP URL", Placeholder: "https://app.netdata.cloud/api/v1/mcp"},
		{Key: "token", Label: "Token", Placeholder: "ndc.xxx", Secret: true},
	}},
	{Type: TypeNotion, Label: "Notion", Fields: []TypeField{
		{Key: "url", Label: "MCP URL", Placeholder: "https://mcp.notion.com/mcp"},
	}},
	{Type: TypeHTTP, Label: "HTTP API", Fields: []TypeField{
		{Key: "url", Label: "Base URL", Placeholder: "https://api.example.com"},
		{Key: "token", Label: "API Token (optional)", Placeholder: "Bearer token", Secret: true},
		{Key: "token_scheme", Label: "Token Scheme (optional)", Placeholder: "bearer (default) | basic"},
		{Key: "basic_suffix", Label: "Basic Suffix (optional)", Placeholder: "non-secret constant after 'token:' when scheme=basic (e.g. 'token')"},
		{Key: "token_header", Label: "Token Header (optional)", Placeholder: "Authorization: Bearer (default)"},
		{Key: "headers", Label: "Headers (optional)", Placeholder: "One \"Name: Value\" per line, e.g. Notion-Version: 2022-06-28", Multiline: true},
	}},
	{Type: TypeFirecrawl, Label: "Firecrawl", Fields: []TypeField{
		{Key: "url", Label: "API URL", Placeholder: "https://api.firecrawl.dev (default)"},
		{Key: "token", Label: "API Key", Placeholder: "fc-...", Secret: true},
	}},
	{Type: TypeBrave, Label: "Brave Search", Fields: []TypeField{
		{Key: "url", Label: "API URL", Placeholder: "https://api.search.brave.com (default)"},
		{Key: "token", Label: "API Key", Placeholder: "BSA...", Secret: true},
	}},
	{Type: TypeMicrosoftGraph, Label: "Microsoft Graph", Fields: []TypeField{
		{Key: "client_id", Label: "Client ID (required)", Placeholder: "Azure App Registration (Application) ID"},
		{Key: "scopes", Label: "Scopes", Placeholder: "Mail.ReadWrite Mail.Send offline_access (default: Mail-only)"},
	}},
	{Type: TypeGoogleTagManager, Label: "Google Tag Manager", Fields: []TypeField{
		{Key: "token", Label: "Service Account JSON Key", Placeholder: `{"client_email":"...","private_key":"..."}`, Secret: true},
	}},
	{Type: TypeOpenAI, Label: "OpenAI", Fields: []TypeField{
		{Key: "url", Label: "API URL", Placeholder: "https://api.openai.com (default)"},
		{Key: "token", Label: "API Key", Placeholder: "sk-...", Secret: true},
	}},
	{Type: TypeElevenLabs, Label: "ElevenLabs", Fields: []TypeField{
		{Key: "url", Label: "API URL", Placeholder: "https://api.elevenlabs.io (default)"},
		{Key: "token", Label: "API Key", Placeholder: "xi_...", Secret: true},
	}},
	{Type: TypeRecraft, Label: "Recraft", Fields: []TypeField{
		{Key: "url", Label: "API URL", Placeholder: "https://external.api.recraft.ai/v1 (default)"},
		{Key: "token", Label: "API Key", Placeholder: "Recraft API token", Secret: true},
	}},
	{Type: TypeIdeogram, Label: "Ideogram", Fields: []TypeField{
		{Key: "url", Label: "API URL", Placeholder: "https://api.ideogram.ai (default)"},
		{Key: "token", Label: "API Key", Placeholder: "Ideogram API key", Secret: true},
	}},
	{Type: TypeAsana, Label: "Asana", Fields: []TypeField{
		{Key: "token", Label: "Personal Access Token", Placeholder: "0/abc123...", Secret: true},
	}},
	{Type: TypeAsanaMCP, Label: "Asana MCP", Fields: []TypeField{
		{Key: "url", Label: "MCP URL", Placeholder: "https://mcp.asana.com/v2/mcp (default)"},
	}},
	{Type: TypeGemini, Label: "Google Gemini", Fields: []TypeField{
		{Key: "url", Label: "API URL", Placeholder: "https://generativelanguage.googleapis.com/v1beta (default)"},
		{Key: "token", Label: "API Key (x-goog-api-key)", Placeholder: "AIzaSy...", Secret: true},
	}},
	{Type: TypeFalAI, Label: "fal.ai", Fields: []TypeField{
		{Key: "url", Label: "API URL", Placeholder: "https://queue.fal.run (default)"},
		{Key: "token", Label: "API Key", Placeholder: "fal_...", Secret: true},
	}},
	{Type: TypeIMAP, Label: "IMAP", Fields: []TypeField{
		{Key: "host", Label: "IMAP Host", Placeholder: "imap.example.com"},
		{Key: "port", Label: "Port", Placeholder: "993", Small: true},
		{Key: "user", Label: "Email / Username", Placeholder: "user@example.com"},
		{Key: "password", Label: "Password", Placeholder: "password", Secret: true},
		{Key: "from_name", Label: "Sender Name", Placeholder: "Jane Doe (optional)"},
		{Key: "smtp_host", Label: "SMTP Host", Placeholder: "smtp.example.com (optional, for sending)"},
		{Key: "smtp_port", Label: "SMTP Port", Placeholder: "587", Small: true},
		{Key: "allow_send", Label: "Allow sending", Checkbox: true},
		{Key: "smtp_saves_sent", Label: "Server files sent mail itself", Checkbox: true},
	}},
	{Type: TypeGit, Label: "Git Credential", Fields: []TypeField{
		{Key: "host", Label: "Git Host", Placeholder: "gitlab.com"},
		{Key: "user", Label: "Username", Placeholder: "oauth2"},
		{Key: "token", Label: "Personal Access Token", Placeholder: "glpat-... / ghp_...", Secret: true},
	}},
	{Type: TypeMeilisearch, Label: "Meilisearch", Fields: []TypeField{
		{Key: "host", Label: "Host", Placeholder: "localhost"},
		{Key: "port", Label: "Port", Placeholder: "7700", Small: true},
		{Key: "database", Label: "Index", Placeholder: "docs"},
		{Key: "token", Label: "API Key", Placeholder: "master-key", Secret: true},
	}},
	{Type: TypeGoogleWorkspace, Label: "Google Workspace", Fields: []TypeField{
		{Key: "url", Label: "MCP URL", Placeholder: "http://localhost:8000/mcp"},
	}},
	{Type: TypeYouTrackAgile, Label: "YouTrack Agile (superseded by YouTrack REST)", Fields: []TypeField{
		{Key: "url", Label: "Base URL", Placeholder: "https://instance.myjetbrains.com/youtrack"},
		{Key: "token", Label: "Permanent Token", Placeholder: "perm:...", Secret: true},
		{Key: "database", Label: "Board ID", Placeholder: "123-45"},
	}},
	{Type: TypeYouTrackREST, Label: "YouTrack REST", Fields: []TypeField{
		{Key: "url", Label: "Base URL", Placeholder: "https://instance.youtrack.cloud or https://instance.myjetbrains.com/youtrack (no /api, no /mcp)"},
		{Key: "token", Label: "Permanent Token", Placeholder: "perm:...", Secret: true},
		{Key: "protected_fields", Label: "Protected fields (optional)", Placeholder: "One per line, \"Project:Field\" or \"Field\" — never written by this connection, e.g. P:Sprint priority", Multiline: true},
		{Key: "set_once_fields", Label: "Set-once fields (optional)", Placeholder: "One per line, \"Project:Field\" or \"Field\" — written only while still empty, e.g. P:Department", Multiline: true},
	}},
	{Type: TypeHyperbrowser, Label: "Hyperbrowser", Fields: []TypeField{
		{Key: "url", Label: "API URL", Placeholder: "https://api.hyperbrowser.ai (default)"},
		{Key: "token", Label: "API Key (x-api-key)", Placeholder: "hb_...", Secret: true},
	}},
	{Type: TypeHiggsfield, Label: "Higgsfield", Fields: []TypeField{
		{Key: "url", Label: "MCP URL", Placeholder: "https://mcp.higgsfield.ai/mcp (default)"},
	}},
	{Type: TypeSSH, Label: "SSH (commands & files)", Fields: []TypeField{
		{Key: "host", Label: "Host", Placeholder: "server.example.com"},
		{Key: "port", Label: "Port", Placeholder: "22", Small: true},
		{Key: "user", Label: "User", Placeholder: "deploy"},
		{Key: "auth", Label: "Auth", Placeholder: "key (default) | agent — agent uses the ssh-agent of the logged-in user, no key in mux"},
		{Key: "private_key", Label: "Private Key (PEM, optional)", Placeholder: "Paste the private key — may be passphrase-protected; not needed with Key File or auth = agent", Secret: true, Multiline: true},
		{Key: "passphrase", Label: "Key Passphrase (optional)", Placeholder: "Passphrase of the private key, if it has one", Secret: true},
		{Key: "key_file", Label: "Key File (optional)", Placeholder: "~/.ssh/id_ed25519 — read instead of the stored key"},
		{Key: "host_key", Label: "Host Key (optional)", Placeholder: "ssh-ed25519 AAAA… — pins the server's key; empty: known_hosts decides"},
		{Key: "allow_commands", Label: "Allowed commands (optional)", Placeholder: "One per line, e.g. systemctl status — read-only connections use the built-in list when empty", Multiline: true},
		{Key: "deny_commands", Label: "Denied commands (optional)", Placeholder: "One per line, e.g. journalctl --vacuum* — always wins", Multiline: true},
	}},
}

// typeIndex is a lazily-built lookup map from type string to TypeDef.
var (
	typeIndex map[string]*TypeDef
	indexOnce sync.Once
)

func ensureIndex() {
	indexOnce.Do(func() {
		typeIndex = make(map[string]*TypeDef, len(AllTypes))
		for i := range AllTypes {
			typeIndex[AllTypes[i].Type] = &AllTypes[i]
		}
	})
}

// LookupType returns the TypeDef for the given type string, or nil if unknown.
func LookupType(typ string) *TypeDef {
	ensureIndex()
	return typeIndex[typ]
}

// TypeLabel returns the human-readable label for a connection type.
// Falls back to the raw type string if not found.
func TypeLabel(typ string) string {
	if td := LookupType(typ); td != nil {
		return td.Label
	}
	return typ
}

// ValidType reports whether typ is a known connection type.
func ValidType(typ string) bool {
	return LookupType(typ) != nil
}
