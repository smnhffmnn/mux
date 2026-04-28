package config

// Canonical string identifiers used throughout the config package and beyond.
//
// The string values are part of the public wire format (TOML config, JSON
// provisioning payloads, persisted state) — never change them. The constants
// exist purely for in-code type safety: typos become compile errors, and
// usage sites are discoverable via "go to definition".
//
// Naming rule: hyphens in the wire value become word boundaries in the
// identifier. Each word starts uppercase. Recognised initialisms
// (HTTP, URL, ID, MCP, AI, SQL, IMAP, SSH, DB) stay all-caps.

// Connection.Type values. Every entry must have a matching TypeDef in
// AllTypes (see types.go) and a handler in internal/tools.
const (
	TypeMariaDB          = "mariadb"
	TypePostgreSQL       = "postgresql"
	TypeClickHouse       = "clickhouse"
	TypeProxy            = "proxy"
	TypeYouTrack         = "youtrack"
	TypeYouTrackAgile    = "youtrack-agile"
	TypeSentry           = "sentry"
	TypeNetdata          = "netdata"
	TypeNotion           = "notion"
	TypeAsana            = "asana"
	TypeAsanaMCP         = "asana-mcp"
	TypeGoogleWorkspace  = "google-workspace"
	TypeGoogleTagManager = "google-tagmanager"
	TypeHTTP             = "http"
	TypeMicrosoftGraph   = "microsoft-graph"
	TypeOpenAI           = "openai"
	TypeElevenLabs       = "elevenlabs"
	TypeBrave            = "brave"
	TypeFirecrawl        = "firecrawl"
	TypeGemini           = "gemini"
	TypeFalAI            = "fal-ai"
	TypeIMAP             = "imap"
	TypeMeilisearch      = "meilisearch"
	TypeRecraft          = "recraft"
	TypeIdeogram         = "ideogram"
	TypeGit              = "git"
)

// Connection.Source / Tunnel.Source values. SourceLocal marks entries
// loaded from the local config (TOML, env vars, secret store);
// SourceProvisioning marks entries fetched from a remote provisioning
// endpoint and never persisted to disk.
const (
	SourceLocal        = "local"
	SourceProvisioning = "provisioning"
)

// TunnelConfig.Type values. WireGuard is the default and ssh is the
// alternative for environments that already have ssh access available.
const (
	TunnelTypeWireGuard = "wireguard"
	TunnelTypeSSH       = "ssh"
)
