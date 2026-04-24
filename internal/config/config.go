package config

import (
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/BurntSushi/toml"
)

// legacyProvisioningHeader matches a standalone `[provisioning]` header on its
// own line (with optional surrounding whitespace). Used to migrate the old
// single-endpoint schema to the new array-of-tables form.
var legacyProvisioningHeader = regexp.MustCompile(`(?m)^(\s*)\[provisioning\]\s*$`)

// migrateLegacyProvisioningHeader rewrites `[provisioning]` (singular table)
// to `[[provisioning]]` (array of tables) in-place. Leaves files that already
// use the array form untouched. Operates only on header lines — string
// occurrences inside values or comments are not affected because table
// headers must be on their own line.
func migrateLegacyProvisioningHeader(content []byte) []byte {
	if bytes := content; len(bytes) > 0 {
		// Skip migration if the file already uses `[[provisioning]]` anywhere —
		// mixed formats would be ambiguous.
		if regexp.MustCompile(`(?m)^\s*\[\[provisioning\]\]\s*$`).Match(bytes) {
			return bytes
		}
	}
	return legacyProvisioningHeader.ReplaceAll(content, []byte("${1}[[provisioning]]"))
}

const (
	ServiceName      = "mux"
	DefaultPort      = 7700
	DefaultConfigDir = ".mux"
	ConfigFileName   = "config.toml"
)

// --- Core types ---

// Connection represents a database or proxy backend.
type Connection struct {
	Name         string `toml:"name" json:"name"`
	Type         string `toml:"type" json:"type"` // "postgresql", "clickhouse", "mariadb", "proxy"
	Host         string `toml:"host,omitempty" json:"host,omitempty"`
	Port         int    `toml:"port,omitzero" json:"port,omitempty"`
	User         string `toml:"user,omitempty" json:"user,omitempty"`
	Password     string `toml:"-" json:"password,omitempty"`
	Database     string `toml:"database,omitempty" json:"database,omitempty"`
	ReadOnly     bool   `toml:"read_only,omitempty" json:"readOnly,omitempty"`
	Secure       bool   `toml:"secure,omitempty" json:"secure,omitempty"`
	URL          string `toml:"url,omitempty" json:"url,omitempty"`
	Token        string `toml:"-" json:"token,omitempty"`
	OAuth        bool   `toml:"oauth,omitempty" json:"oauth,omitempty"` // proxy: use OAuth instead of bearer token
	Scopes       string `toml:"scopes,omitempty" json:"scopes,omitempty"`
	Instructions string `toml:"instructions,omitempty" json:"instructions,omitempty"`
	TokenHeader  string `toml:"token_header,omitempty" json:"tokenHeader,omitempty"` // custom header name for token (default: "Authorization: Bearer {token}")
	Tunnel       string `toml:"tunnel,omitempty" json:"tunnel,omitempty"`             // name of a defined tunnel
	MonthlyLimit int    `toml:"monthly_limit,omitzero" json:"monthlyLimit,omitempty"` // optional request limit per month
	Source       string `toml:"-" json:"source,omitempty"`                             // "local" or "provisioning"
}

// Enabled reports whether the connection has enough config to attempt a connection.
func (c *Connection) Enabled() bool {
	switch {
	case IsProxyType(c.Type), c.Type == "http":
		return c.URL != ""
	case c.Type == "microsoft-graph":
		return true // auth is interactive via tools
	case c.Type == "firecrawl", c.Type == "brave", c.Type == "google-tagmanager",
		c.Type == "openai", c.Type == "elevenlabs", c.Type == "recraft", c.Type == "ideogram",
		c.Type == "asana", c.Type == "gemini", c.Type == "fal-ai":
		return c.Token != ""
	case c.Type == "meilisearch":
		return c.Host != "" && c.Database != ""
	case c.Type == "imap":
		return c.Host != "" && c.User != "" && c.Password != ""
	case c.Type == "git":
		// Token is fetched from vault at request time, not at startup.
		// Only require host and user for the connection to be considered enabled.
		return c.Host != "" && c.User != ""
	default:
		return c.Host != "" && c.User != ""
	}
}

// IsProxyType reports whether a connection type proxies an upstream MCP server.
func IsProxyType(typ string) bool {
	switch typ {
	case "proxy", "youtrack", "sentry", "netdata", "notion", "asana-mcp", "google-workspace":
		return true
	}
	return false
}

// TunnelConfig represents a WireGuard or SSH tunnel definition.
type TunnelConfig struct {
	Name          string `toml:"name" json:"name"`
	Type          string `toml:"type,omitempty" json:"type,omitempty"` // "wireguard" (default) or "ssh"

	// WireGuard fields
	PeerPublicKey string `toml:"peer_public_key,omitempty" json:"peerPublicKey,omitempty"`
	PeerEndpoint  string `toml:"peer_endpoint,omitempty" json:"peerEndpoint,omitempty"`
	AllowedIPs    string `toml:"allowed_ips,omitempty" json:"allowedIPs,omitempty"`
	TunnelAddress string `toml:"tunnel_address,omitempty" json:"tunnelAddress,omitempty"`
	DNS           string `toml:"dns,omitempty" json:"dns,omitempty"`
	PresharedKey  string `toml:"-" json:"-"` // never serialize
	MTU           int    `toml:"mtu,omitzero" json:"mtu,omitempty"`
	KeepAlive     int    `toml:"keepalive,omitzero" json:"keepalive,omitempty"`

	// SSH fields
	Host              string `toml:"host,omitempty" json:"host,omitempty"`
	Port              int    `toml:"port,omitzero" json:"port,omitempty"`
	User              string `toml:"user,omitempty" json:"user,omitempty"`
	KeyFile           string `toml:"key_file,omitempty" json:"keyFile,omitempty"`                         // path to SSH private key file
	InsecureHostKey   bool   `toml:"insecure_host_key,omitempty" json:"insecureHostKey,omitempty"`        // skip host key verification (default: false)

	// Shared
	PrivateKey string `toml:"-" json:"-"` // WG: base64 key; SSH: PEM key content — never serialize
	Source     string `toml:"-" json:"source,omitempty"` // "local" or "provisioning"
}

// IsSSH reports whether the tunnel is an SSH tunnel.
func (t *TunnelConfig) IsSSH() bool {
	return t.Type == "ssh"
}

// Enabled reports whether the tunnel has enough config to be started.
func (t *TunnelConfig) Enabled() bool {
	if t.IsSSH() {
		return t.Host != "" && t.User != "" && (t.PrivateKey != "" || t.KeyFile != "")
	}
	// WireGuard
	return t.PrivateKey != "" && t.PeerPublicKey != "" && t.PeerEndpoint != ""
}

// ProvisioningConfig holds a single remote provisioning endpoint.
// Multiple endpoints can be configured as `[[provisioning]]` blocks in config.toml;
// Name is required when more than one endpoint is configured so tokens and
// provisioned entries can be attributed back to their source.
type ProvisioningConfig struct {
	Name     string `toml:"name,omitempty" json:"name,omitempty"`
	Endpoint string `toml:"endpoint,omitempty" json:"endpoint,omitempty"`
	Token    string `toml:"-" json:"-"`
}

// Enabled reports whether this endpoint is ready to fetch (endpoint + token present).
func (p ProvisioningConfig) Enabled() bool {
	return p.Endpoint != "" && p.Token != ""
}

// SecretKey returns the keychain key under which this endpoint's token is stored.
// Endpoints without a Name fall back to the legacy "provisioning-token" key for
// backward compatibility with single-endpoint configs.
func (p ProvisioningConfig) SecretKey() string {
	if p.Name == "" {
		return "provisioning-token"
	}
	return "provisioning-" + p.Name + "-token"
}

type ServerConfig struct {
	Port    int    `toml:"port"`
	Mode    string `toml:"mode,omitempty" json:"mode,omitempty"`       // "desktop", "headless", or "" (auto-detect)
	TLSCert string `toml:"tls_cert,omitempty" json:"tlsCert,omitempty"` // path to TLS certificate
	TLSKey  string `toml:"tls_key,omitempty" json:"tlsKey,omitempty"`   // path to TLS private key
	TLSPort int    `toml:"tls_port,omitzero" json:"tlsPort,omitempty"` // HTTPS port for WebAuthn/Vault (default: port + 1)
}

// EffectiveTLSPort returns the configured TLS port, defaulting to Port + 1.
func (s ServerConfig) EffectiveTLSPort() int {
	if s.TLSPort != 0 {
		return s.TLSPort
	}
	return s.Port + 1
}

// VaultConfig holds settings for the encrypted secret vault.
type VaultConfig struct {
	Enabled           bool     `toml:"enabled,omitempty" json:"enabled,omitempty"`
	Exclusive         bool     `toml:"exclusive,omitempty" json:"exclusive,omitempty"`                     // when true, vault-stored secrets skip legacy keyring/file
	InactivityTimeout string   `toml:"inactivity_timeout,omitempty" json:"inactivityTimeout,omitempty"`   // e.g. "30m"
	WebAuthnRPID      string   `toml:"webauthn_rp_id,omitempty" json:"webauthnRpId,omitempty"`            // e.g. "mux.local"
	WebAuthnOrigins   []string `toml:"webauthn_origins,omitempty" json:"webauthnOrigins,omitempty"`        // e.g. ["https://mux.local:7700"]
	BaseURL           string   `toml:"base_url,omitempty" json:"baseUrl,omitempty"`                        // public URL for approval links, e.g. "https://mux.example.com:7701"
}

// Config is the application configuration.
type Config struct {
	Server       ServerConfig         `toml:"server"`
	Provisioning []ProvisioningConfig `toml:"provisioning,omitempty"`
	Vault        VaultConfig          `toml:"vault,omitempty"`
	Tunnels      []TunnelConfig       `toml:"tunnels,omitempty"`
	Connections  []Connection         `toml:"connections,omitempty"`

	// Runtime fields (not persisted). Provisioned entries are tracked by
	// the name of the endpoint that delivered them so a sync of a single
	// endpoint only replaces its own entries.
	provisionedByEndpoint map[string]provisionedEntries
	path                  string
}

type provisionedEntries struct {
	Tunnels     []TunnelConfig
	Connections []Connection
}

// ProvisionedTunnels returns all tunnels delivered across all provisioning endpoints.
func (cfg *Config) ProvisionedTunnels() []TunnelConfig {
	var all []TunnelConfig
	for _, p := range cfg.Provisioning {
		entries, ok := cfg.provisionedByEndpoint[p.Name]
		if !ok {
			continue
		}
		all = append(all, entries.Tunnels...)
	}
	return all
}

// ProvisionedConnections returns all connections delivered across all provisioning endpoints.
func (cfg *Config) ProvisionedConnections() []Connection {
	var all []Connection
	for _, p := range cfg.Provisioning {
		entries, ok := cfg.provisionedByEndpoint[p.Name]
		if !ok {
			continue
		}
		all = append(all, entries.Connections...)
	}
	return all
}

// AllTunnels returns local + provisioned tunnels. On name collision, provisioned wins.
func (cfg *Config) AllTunnels() []TunnelConfig {
	provisioned := cfg.ProvisionedTunnels()
	if len(provisioned) == 0 {
		return cfg.Tunnels
	}
	seen := make(map[string]bool)
	var all []TunnelConfig
	for _, t := range provisioned {
		seen[t.Name] = true
		all = append(all, t)
	}
	for _, t := range cfg.Tunnels {
		if !seen[t.Name] {
			all = append(all, t)
		}
	}
	return all
}

// AllConnections returns local + provisioned connections.
func (cfg *Config) AllConnections() []Connection {
	provisioned := cfg.ProvisionedConnections()
	if len(provisioned) == 0 {
		return cfg.Connections
	}
	var all []Connection
	all = append(all, provisioned...)
	all = append(all, cfg.Connections...)
	return all
}

// SetProvisioned stores provisioned tunnels and connections for a single endpoint
// (identified by endpointName — matches ProvisioningConfig.Name). A call only replaces
// the entries that came from that endpoint; others remain untouched.
// HTTP connections without a token inherit the endpoint's bearer token when their URL
// points to the same host as the endpoint (same API, same credentials).
func (cfg *Config) SetProvisioned(endpointName string, tunnels []TunnelConfig, connections []Connection) {
	endpoint := cfg.FindProvisioning(endpointName)
	var provHost, provToken string
	if endpoint != nil {
		provHost = hostFromURL(endpoint.Endpoint)
		provToken = endpoint.Token
	}

	for i := range connections {
		if connections[i].Source == "" {
			connections[i].Source = "provisioning"
		}
		// Provisioned http connections without their own token reuse this endpoint's
		// bearer token if they point to the same host as the endpoint URL.
		if connections[i].Type == "http" && connections[i].Token == "" && provToken != "" {
			if provHost != "" && hostFromURL(connections[i].URL) == provHost {
				connections[i].Token = provToken
			}
		}
		// Supplement with locally stored secrets from keychain/file.
		// Only fills in gaps — if the provisioning server provides a secret, it wins.
		if connections[i].Password == "" {
			if v, err := getSecret(connections[i].Name + "-password"); err == nil {
				connections[i].Password = v
			}
		}
		if connections[i].Token == "" {
			if v, err := getSecret(connections[i].Name + "-token"); err == nil {
				connections[i].Token = v
			}
		}
		ApplyConnectionDefaults(&connections[i])
	}
	for i := range tunnels {
		if tunnels[i].Source == "" {
			tunnels[i].Source = "provisioning"
		}
		if tunnels[i].IsSSH() {
			if tunnels[i].Port == 0 {
				tunnels[i].Port = 22
			}
		} else {
			if tunnels[i].MTU == 0 {
				tunnels[i].MTU = 1420
			}
			if tunnels[i].KeepAlive == 0 {
				tunnels[i].KeepAlive = 25
			}
		}
		// Supplement tunnel secrets from keychain/file
		if tunnels[i].PrivateKey == "" {
			if v, err := getSecret("tunnel-" + tunnels[i].Name + "-private-key"); err == nil {
				tunnels[i].PrivateKey = v
			}
		}
		if tunnels[i].PresharedKey == "" {
			if v, err := getSecret("tunnel-" + tunnels[i].Name + "-preshared-key"); err == nil {
				tunnels[i].PresharedKey = v
			}
		}
	}
	if cfg.provisionedByEndpoint == nil {
		cfg.provisionedByEndpoint = make(map[string]provisionedEntries)
	}
	cfg.provisionedByEndpoint[endpointName] = provisionedEntries{
		Tunnels:     tunnels,
		Connections: connections,
	}
}

// ClearProvisioned drops all provisioned entries for an endpoint (used when an
// endpoint is removed from config).
func (cfg *Config) ClearProvisioned(endpointName string) {
	delete(cfg.provisionedByEndpoint, endpointName)
}

// FindProvisioning returns the ProvisioningConfig for a given name, or nil.
func (cfg *Config) FindProvisioning(name string) *ProvisioningConfig {
	for i := range cfg.Provisioning {
		if cfg.Provisioning[i].Name == name {
			return &cfg.Provisioning[i]
		}
	}
	return nil
}

// HasProvisioning reports whether at least one provisioning endpoint is fully configured.
func (cfg *Config) HasProvisioning() bool {
	for _, p := range cfg.Provisioning {
		if p.Enabled() {
			return true
		}
	}
	return false
}

// ProvisioningStatus reports the aggregate count of provisioned tunnels and connections
// across all endpoints.
func (cfg *Config) ProvisioningStatus() (tunnels, connections int) {
	for _, p := range cfg.Provisioning {
		entries, ok := cfg.provisionedByEndpoint[p.Name]
		if !ok {
			continue
		}
		tunnels += len(entries.Tunnels)
		connections += len(entries.Connections)
	}
	return
}

func DefaultConfigPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, DefaultConfigDir, ConfigFileName)
}

// --- Legacy types for backward-compatible config loading ---

type legacyProxyConfig struct {
	URL   string `toml:"url"`
	Token string `toml:"-"`
}

type legacyDBConfig struct {
	Host     string `toml:"host"`
	Port     int    `toml:"port"`
	User     string `toml:"user"`
	Password string `toml:"-"`
	Database string `toml:"database"`
	ReadOnly bool   `toml:"read_only"`
	Secure   bool   `toml:"secure"`
}

type legacyConfigFile struct {
	YouTrack   *legacyProxyConfig `toml:"youtrack"`
	Sentry     *legacyProxyConfig `toml:"sentry"`
	MariaDB    *legacyDBConfig    `toml:"mariadb"`
	ClickHouse *legacyDBConfig    `toml:"clickhouse"`
	PostgreSQL *legacyDBConfig    `toml:"postgresql"`
}

// migrateFromLegacy converts old-format config sections into Connections.
// Only migrates if no connections with the same name already exist.
func migrateFromLegacy(cfg *Config, legacy *legacyConfigFile) {
	existing := make(map[string]bool)
	for _, c := range cfg.Connections {
		existing[c.Name] = true
	}

	if legacy.YouTrack != nil && legacy.YouTrack.URL != "" && !existing["youtrack"] {
		cfg.Connections = append(cfg.Connections, Connection{
			Name:   "youtrack",
			Type:   "proxy",
			URL:    legacy.YouTrack.URL,
			Token:  legacy.YouTrack.Token,
			Source: "local",
		})
	}

	if legacy.Sentry != nil && legacy.Sentry.URL != "" && !existing["sentry"] {
		cfg.Connections = append(cfg.Connections, Connection{
			Name:   "sentry",
			Type:   "proxy",
			URL:    legacy.Sentry.URL,
			OAuth:  true,
			Source: "local",
		})
	}

	if legacy.MariaDB != nil && legacy.MariaDB.Host != "" && !existing["mariadb"] {
		port := legacy.MariaDB.Port
		if port == 0 {
			port = 3306
		}
		cfg.Connections = append(cfg.Connections, Connection{
			Name:     "mariadb",
			Type:     "mariadb",
			Host:     legacy.MariaDB.Host,
			Port:     port,
			User:     legacy.MariaDB.User,
			Password: legacy.MariaDB.Password,
			Database: legacy.MariaDB.Database,
			ReadOnly: legacy.MariaDB.ReadOnly,
			Source:   "local",
		})
	}

	if legacy.ClickHouse != nil && legacy.ClickHouse.Host != "" && !existing["clickhouse"] {
		port := legacy.ClickHouse.Port
		if port == 0 {
			port = 8123
		}
		db := legacy.ClickHouse.Database
		if db == "" {
			db = "default"
		}
		cfg.Connections = append(cfg.Connections, Connection{
			Name:     "clickhouse",
			Type:     "clickhouse",
			Host:     legacy.ClickHouse.Host,
			Port:     port,
			User:     legacy.ClickHouse.User,
			Password: legacy.ClickHouse.Password,
			Database: db,
			Secure:   legacy.ClickHouse.Secure,
			Source:   "local",
		})
	}

	if legacy.PostgreSQL != nil && legacy.PostgreSQL.Host != "" && !existing["postgresql"] {
		port := legacy.PostgreSQL.Port
		if port == 0 {
			port = 5432
		}
		cfg.Connections = append(cfg.Connections, Connection{
			Name:     "postgresql",
			Type:     "postgresql",
			Host:     legacy.PostgreSQL.Host,
			Port:     port,
			User:     legacy.PostgreSQL.User,
			Password: legacy.PostgreSQL.Password,
			Database: legacy.PostgreSQL.Database,
			Source:   "local",
		})
	}
}

// --- Loading ---

func Load(path string) (*Config, error) {
	cfg := &Config{
		Server: ServerConfig{Port: DefaultPort},
	}

	if path == "" {
		path = DefaultConfigPath()
	}
	if _, err := os.Stat(path); err == nil {
		log.Printf("[config] Loading config from %s", path)
		content, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read config %s: %w", path, err)
		}
		// Legacy migration: the old schema wrote a singular `[provisioning]`
		// table. The new schema uses an array-of-tables `[[provisioning]]`.
		// Rewrite a standalone `[provisioning]` header on a line of its own
		// to `[[provisioning]]` so BurntSushi/toml decodes it into the slice.
		migrated := migrateLegacyProvisioningHeader(content)
		if _, err := toml.Decode(string(migrated), cfg); err != nil {
			return nil, fmt.Errorf("parse config %s: %w", path, err)
		}

		// Migrate old-format config sections (best-effort)
		var legacy legacyConfigFile
		toml.Decode(string(migrated), &legacy)
		migrateFromLegacy(cfg, &legacy)
	} else {
		log.Printf("[config] No config file found at %s — using defaults", path)
	}

	// Mark local connections
	for i := range cfg.Connections {
		if cfg.Connections[i].Source == "" {
			cfg.Connections[i].Source = "local"
		}
	}
	for i := range cfg.Tunnels {
		if cfg.Tunnels[i].Source == "" {
			cfg.Tunnels[i].Source = "local"
		}
	}

	// Load secrets from OS keychain (best-effort)
	loadKeychain(cfg)

	// Environment variables override everything
	loadEnv(cfg)

	// Apply defaults for connections
	for i := range cfg.Connections {
		ApplyConnectionDefaults(&cfg.Connections[i])
	}
	for i := range cfg.Tunnels {
		if cfg.Tunnels[i].IsSSH() {
			if cfg.Tunnels[i].Port == 0 {
				cfg.Tunnels[i].Port = 22
			}
			// Load SSH key from file if not already in keychain
			if cfg.Tunnels[i].PrivateKey == "" && cfg.Tunnels[i].KeyFile != "" {
				if data, err := os.ReadFile(ExpandHome(cfg.Tunnels[i].KeyFile)); err == nil {
					cfg.Tunnels[i].PrivateKey = string(data)
				}
			}
		} else {
			if cfg.Tunnels[i].MTU == 0 {
				cfg.Tunnels[i].MTU = 1420
			}
			if cfg.Tunnels[i].KeepAlive == 0 {
				cfg.Tunnels[i].KeepAlive = 25
			}
		}
	}

	// Log summary
	connTypes := make(map[string]int)
	for _, c := range cfg.Connections {
		connTypes[c.Type]++
	}
	if len(cfg.Connections) > 0 {
		parts := make([]string, 0, len(connTypes))
		for t, n := range connTypes {
			parts = append(parts, fmt.Sprintf("%d× %s", n, t))
		}
		sort.Strings(parts)
		log.Printf("[config] %d connections (%s), %d tunnels", len(cfg.Connections), strings.Join(parts, ", "), len(cfg.Tunnels))
	} else {
		log.Printf("[config] No connections configured")
	}

	cfg.path = path
	return cfg, nil
}

func ApplyConnectionDefaults(c *Connection) {
	switch c.Type {
	case "mariadb":
		if c.Port == 0 {
			c.Port = 3306
		}
	case "clickhouse":
		if c.Port == 0 {
			c.Port = 8123
		}
		if c.Database == "" {
			c.Database = "default"
		}
	case "postgresql":
		if c.Port == 0 {
			c.Port = 5432
		}
	case "asana-mcp":
		c.OAuth = true
		if c.URL == "" {
			c.URL = "https://mcp.asana.com/v2/mcp"
		}
	case "notion":
		c.OAuth = true
		if c.URL == "" {
			c.URL = "https://mcp.notion.com/mcp"
		}
	case "gemini":
		if c.URL == "" {
			c.URL = "https://generativelanguage.googleapis.com/v1beta"
		}
	case "fal-ai":
		if c.URL == "" {
			c.URL = "https://queue.fal.run"
		}
	case "imap":
		if c.Port == 0 {
			c.Port = 993
		}
	case "meilisearch":
		if c.Port == 0 {
			c.Port = 7700
		}
	}
}

// ExpandHome replaces a leading ~ with the user's home directory.
func ExpandHome(path string) string {
	if strings.HasPrefix(path, "~/") || path == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, path[1:])
		}
	}
	return path
}

func hostFromURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

// Save writes the non-sensitive config back to the TOML file.
func (cfg *Config) Save() error {
	path := cfg.path
	if path == "" {
		path = DefaultConfigPath()
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create config dir %s: %w", dir, err)
	}

	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create config file %s: %w", path, err)
	}
	defer f.Close()

	enc := toml.NewEncoder(f)
	if err := enc.Encode(cfg); err != nil {
		return fmt.Errorf("write config: %w", err)
	}

	return nil
}

// --- Keychain ---

func loadKeychain(cfg *Config) {
	secretsLoaded := 0

	// Provisioning tokens (one per endpoint, keyed by endpoint name).
	// Unnamed endpoints fall back to the legacy "provisioning-token" key.
	for i := range cfg.Provisioning {
		key := cfg.Provisioning[i].SecretKey()
		if v, err := getSecret(key); err == nil {
			cfg.Provisioning[i].Token = v
			secretsLoaded++
		}
	}

	// Connection secrets (password or token, keyed by connection name)
	for i := range cfg.Connections {
		name := cfg.Connections[i].Name
		if v, err := getSecret(name + "-password"); err == nil {
			cfg.Connections[i].Password = v
			secretsLoaded++
		}
		if v, err := getSecret(name + "-token"); err == nil && cfg.Connections[i].Token == "" {
			cfg.Connections[i].Token = v
			secretsLoaded++
		}
	}

	// Tunnel secrets (private key, preshared key)
	for i := range cfg.Tunnels {
		name := cfg.Tunnels[i].Name
		if v, err := getSecret("tunnel-" + name + "-private-key"); err == nil {
			cfg.Tunnels[i].PrivateKey = v
			secretsLoaded++
		}
		if v, err := getSecret("tunnel-" + name + "-preshared-key"); err == nil {
			cfg.Tunnels[i].PresharedKey = v
			secretsLoaded++
		}
	}

	log.Printf("[secrets] Loaded %d secrets", secretsLoaded)
}

func loadEnv(cfg *Config) {
	// Server port
	if v := os.Getenv("MUX_PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil {
			cfg.Server.Port = p
		}
	}

	// Provisioning — env vars override the first (default, unnamed) endpoint.
	// For multi-endpoint setups, configure additional endpoints via config.toml.
	if endpoint := os.Getenv("MUX_PROVISIONING_ENDPOINT"); endpoint != "" {
		setDefaultProvisioningEndpoint(cfg, endpoint)
	}
	if token := os.Getenv("MUX_PROVISIONING_TOKEN"); token != "" {
		setDefaultProvisioningToken(cfg, token)
	}

	// Legacy env vars: create or update connections by name
	applyLegacyEnv(cfg)
}

// applyLegacyEnv handles old-style environment variables for backward compatibility.
func applyLegacyEnv(cfg *Config) {
	// YouTrack
	if url := os.Getenv("YOUTRACK_MCP_URL"); url != "" {
		c := findOrCreateConnection(cfg, "youtrack", "proxy")
		c.URL = url
	}
	if token := os.Getenv("YOUTRACK_MCP_TOKEN"); token != "" {
		c := findOrCreateConnection(cfg, "youtrack", "proxy")
		c.Token = token
	}

	// Sentry
	if url := os.Getenv("SENTRY_MCP_URL"); url != "" {
		c := findOrCreateConnection(cfg, "sentry", "proxy")
		c.URL = url
		c.OAuth = true
	}
	if token := os.Getenv("SENTRY_MCP_TOKEN"); token != "" {
		c := findOrCreateConnection(cfg, "sentry", "proxy")
		c.Token = token
	}

	// MariaDB
	if v := os.Getenv("MARIADB_DB_HOST"); v != "" {
		findOrCreateConnection(cfg, "mariadb", "mariadb").Host = v
	}
	if v := os.Getenv("MARIADB_DB_PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil {
			findOrCreateConnection(cfg, "mariadb", "mariadb").Port = p
		}
	}
	if v := os.Getenv("MARIADB_DB_USER"); v != "" {
		findOrCreateConnection(cfg, "mariadb", "mariadb").User = v
	}
	if v := os.Getenv("MARIADB_DB_PASSWORD"); v != "" {
		findOrCreateConnection(cfg, "mariadb", "mariadb").Password = v
	}
	if v := os.Getenv("MARIADB_DB_NAME"); v != "" {
		findOrCreateConnection(cfg, "mariadb", "mariadb").Database = v
	}

	// ClickHouse
	if v := os.Getenv("CLICKHOUSE_HOST"); v != "" {
		findOrCreateConnection(cfg, "clickhouse", "clickhouse").Host = v
	}
	if v := os.Getenv("CLICKHOUSE_PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil {
			findOrCreateConnection(cfg, "clickhouse", "clickhouse").Port = p
		}
	}
	if v := os.Getenv("CLICKHOUSE_USER"); v != "" {
		findOrCreateConnection(cfg, "clickhouse", "clickhouse").User = v
	}
	if v := os.Getenv("CLICKHOUSE_PASSWORD"); v != "" {
		findOrCreateConnection(cfg, "clickhouse", "clickhouse").Password = v
	}
	if v := os.Getenv("CLICKHOUSE_SECURE"); v != "" {
		findOrCreateConnection(cfg, "clickhouse", "clickhouse").Secure = v == "true" || v == "1"
	}

	// PostgreSQL
	if v := os.Getenv("POSTGRESQL_DB_HOST"); v != "" {
		findOrCreateConnection(cfg, "postgresql", "postgresql").Host = v
	}
	if v := os.Getenv("POSTGRESQL_DB_PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil {
			findOrCreateConnection(cfg, "postgresql", "postgresql").Port = p
		}
	}
	if v := os.Getenv("POSTGRESQL_DB_USER"); v != "" {
		findOrCreateConnection(cfg, "postgresql", "postgresql").User = v
	}
	if v := os.Getenv("POSTGRESQL_DB_PASSWORD"); v != "" {
		findOrCreateConnection(cfg, "postgresql", "postgresql").Password = v
	}
	if v := os.Getenv("POSTGRESQL_DB_NAME"); v != "" {
		findOrCreateConnection(cfg, "postgresql", "postgresql").Database = v
	}
}

// findOrCreateConnection returns a pointer to the connection with the given name,
// creating it if it doesn't exist.
func findOrCreateConnection(cfg *Config, name, typ string) *Connection {
	for i := range cfg.Connections {
		if cfg.Connections[i].Name == name {
			return &cfg.Connections[i]
		}
	}
	cfg.Connections = append(cfg.Connections, Connection{
		Name:   name,
		Type:   typ,
		Source: "local",
	})
	return &cfg.Connections[len(cfg.Connections)-1]
}

// FindTunnel returns the local tunnel with the given name, or nil.
func (cfg *Config) FindTunnel(name string) *TunnelConfig {
	for i := range cfg.Tunnels {
		if cfg.Tunnels[i].Name == name {
			return &cfg.Tunnels[i]
		}
	}
	return nil
}

// FindAnyTunnel returns the tunnel with the given name from
// local or provisioned tunnels. Returns a pointer to the actual element.
func (cfg *Config) FindAnyTunnel(name string) *TunnelConfig {
	for i := range cfg.Tunnels {
		if cfg.Tunnels[i].Name == name {
			return &cfg.Tunnels[i]
		}
	}
	for _, p := range cfg.Provisioning {
		entries, ok := cfg.provisionedByEndpoint[p.Name]
		if !ok {
			continue
		}
		for i := range entries.Tunnels {
			if entries.Tunnels[i].Name == name {
				return &entries.Tunnels[i]
			}
		}
	}
	return nil
}

// FindConnection returns the connection with the given name, or nil.
// Searches local connections only.
func (cfg *Config) FindConnection(name string) *Connection {
	for i := range cfg.Connections {
		if cfg.Connections[i].Name == name {
			return &cfg.Connections[i]
		}
	}
	return nil
}

// FindAnyConnection returns the connection with the given name from
// local or provisioned connections. Returns a pointer to the actual element
// (not a copy), so mutations persist in memory.
func (cfg *Config) FindAnyConnection(name string) *Connection {
	for i := range cfg.Connections {
		if cfg.Connections[i].Name == name {
			return &cfg.Connections[i]
		}
	}
	for _, p := range cfg.Provisioning {
		entries, ok := cfg.provisionedByEndpoint[p.Name]
		if !ok {
			continue
		}
		for i := range entries.Connections {
			if entries.Connections[i].Name == name {
				return &entries.Connections[i]
			}
		}
	}
	return nil
}

// setDefaultProvisioningEndpoint sets the Endpoint URL on the first (default)
// provisioning entry, creating one if none exists. Used by env-var overrides.
func setDefaultProvisioningEndpoint(cfg *Config, endpoint string) {
	if len(cfg.Provisioning) == 0 {
		cfg.Provisioning = []ProvisioningConfig{{Endpoint: endpoint}}
		return
	}
	cfg.Provisioning[0].Endpoint = endpoint
}

// setDefaultProvisioningToken sets the Token on the first (default)
// provisioning entry, creating one if none exists. Used by env-var overrides.
func setDefaultProvisioningToken(cfg *Config, token string) {
	if len(cfg.Provisioning) == 0 {
		cfg.Provisioning = []ProvisioningConfig{{Token: token}}
		return
	}
	cfg.Provisioning[0].Token = token
}

// ConnectionEndpointName returns the Name of the provisioning endpoint that
// delivered the given connection, or "" if the connection is local.
func (cfg *Config) ConnectionEndpointName(connName string) string {
	for _, p := range cfg.Provisioning {
		entries, ok := cfg.provisionedByEndpoint[p.Name]
		if !ok {
			continue
		}
		for i := range entries.Connections {
			if entries.Connections[i].Name == connName {
				return p.Name
			}
		}
	}
	return ""
}

// --- Secrets ---

// SaveSecret stores a credential in the OS keychain (with file fallback).
func SaveSecret(key, value string) error {
	return setSecret(key, value)
}

// GetSecret reads a credential from the OS keychain (with file fallback).
func GetSecret(key string) (string, error) {
	return getSecret(key)
}

// DeleteSecret removes a credential from the OS keychain (and file store).
func DeleteSecret(key string) error {
	return deleteSecret(key)
}

// ValidSecretKey checks that a keychain key matches known mux patterns.
func ValidSecretKey(key string) bool {
	if key == "provisioning-token" {
		return true
	}
	// Vault internal secrets (notification config, etc.)
	if strings.HasPrefix(key, "vault-") {
		return true
	}
	if strings.HasSuffix(key, "-password") ||
		strings.HasSuffix(key, "-token") ||
		strings.HasSuffix(key, "-oauth-token") ||
		strings.HasSuffix(key, "-oauth-refresh-token") ||
		strings.HasSuffix(key, "-oauth-client-id") ||
		strings.HasSuffix(key, "-oauth-client-secret") {
		return len(key) > len("-password") // must have a prefix
	}
	if strings.HasPrefix(key, "tunnel-") &&
		(strings.HasSuffix(key, "-private-key") || strings.HasSuffix(key, "-preshared-key")) {
		return true
	}
	return false
}

// --- OAuth Token Store (backed by OS keychain) ---

// KeychainTokenStore stores and retrieves OAuth tokens as raw JSON in the
// OS keychain. It implements the mcp-go transport.TokenStore interface
// (GetToken/SaveToken with *transport.Token) via the transport package adapter.
type KeychainTokenStore struct {
	service string // e.g. "sentry"
	mu      sync.RWMutex
	cached  json.RawMessage
}

// NewKeychainTokenStore creates a token store for the given service name.
func NewKeychainTokenStore(service string) *KeychainTokenStore {
	s := &KeychainTokenStore{service: service}
	if raw, err := getSecret(service + "-oauth-token"); err == nil && raw != "" {
		s.cached = json.RawMessage(raw)
	}
	return s
}

// GetRawToken returns the stored token as raw JSON.
func (s *KeychainTokenStore) GetRawToken() (json.RawMessage, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.cached) == 0 {
		return nil, fmt.Errorf("no token available")
	}
	return s.cached, nil
}

// SaveRawToken persists a token as raw JSON to the OS keychain.
func (s *KeychainTokenStore) SaveRawToken(data json.RawMessage) error {
	if err := setSecret(s.service+"-oauth-token", string(data)); err != nil {
		return fmt.Errorf("save token: %w", err)
	}
	s.mu.Lock()
	s.cached = data
	s.mu.Unlock()
	return nil
}

// HasToken returns true if a token exists in the store.
func (s *KeychainTokenStore) HasToken() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.cached) > 0
}

// SaveOAuthClient stores OAuth client credentials in the keychain.
func SaveOAuthClient(service, clientID, clientSecret string) error {
	if err := setSecret(service+"-oauth-client-id", clientID); err != nil {
		return err
	}
	if clientSecret != "" {
		return setSecret(service+"-oauth-client-secret", clientSecret)
	}
	return nil
}

// LoadOAuthClientID loads a stored OAuth client ID from the keychain/file store.
func LoadOAuthClientID(service string) (clientID, clientSecret string) {
	clientID, _ = getSecret(service + "-oauth-client-id")
	clientSecret, _ = getSecret(service + "-oauth-client-secret")
	return
}
