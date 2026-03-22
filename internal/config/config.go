package config

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/BurntSushi/toml"
	"github.com/zalando/go-keyring"
)

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
	Port         int    `toml:"port,omitempty" json:"port,omitempty"`
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
	Tunnel       string `toml:"tunnel,omitempty" json:"tunnel,omitempty"`             // name of a defined tunnel
	MonthlyLimit int    `toml:"monthly_limit,omitempty" json:"monthlyLimit,omitempty"` // optional request limit per month
	Source       string `toml:"-" json:"source,omitempty"`                             // "local" or "erp"
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
		c.Type == "asana":
		return c.Token != ""
	default:
		return c.Host != "" && c.User != ""
	}
}

// IsProxyType reports whether a connection type proxies an upstream MCP server.
func IsProxyType(typ string) bool {
	switch typ {
	case "proxy", "youtrack", "sentry", "netdata", "notion", "asana-mcp":
		return true
	}
	return false
}

// Tunnel represents a WireGuard tunnel definition.
type TunnelConfig struct {
	Name          string `toml:"name" json:"name"`
	PeerPublicKey string `toml:"peer_public_key" json:"peerPublicKey"`
	PeerEndpoint  string `toml:"peer_endpoint" json:"peerEndpoint"`
	AllowedIPs    string `toml:"allowed_ips" json:"allowedIPs"`
	TunnelAddress string `toml:"tunnel_address" json:"tunnelAddress"`
	DNS           string `toml:"dns,omitempty" json:"dns,omitempty"`
	PrivateKey    string `toml:"-" json:"privateKey,omitempty"`
	PresharedKey  string `toml:"-" json:"presharedKey,omitempty"`
	MTU           int    `toml:"mtu,omitempty" json:"mtu,omitempty"`
	KeepAlive     int    `toml:"keepalive,omitempty" json:"keepalive,omitempty"`
	Source        string `toml:"-" json:"source,omitempty"` // "local" or "erp"
}

// Enabled reports whether the tunnel has enough config to be started.
func (t *TunnelConfig) Enabled() bool {
	return t.PrivateKey != "" && t.PeerPublicKey != "" && t.PeerEndpoint != ""
}

// ERPConfig holds the ERP provisioning endpoint settings.
type ERPConfig struct {
	Endpoint string `toml:"endpoint,omitempty" json:"endpoint,omitempty"`
	Token    string `toml:"-" json:"-"`
}

type ServerConfig struct {
	Port      int  `toml:"port"`
	AutoStart bool `toml:"auto_start"`
}

// Config is the application configuration.
type Config struct {
	Server      ServerConfig   `toml:"server"`
	ERP         ERPConfig      `toml:"erp,omitempty"`
	Tunnels     []TunnelConfig `toml:"tunnels,omitempty"`
	Connections []Connection `toml:"connections,omitempty"`

	// Runtime fields (not persisted)
	erpTunnels     []TunnelConfig
	erpConnections []Connection
	path           string
}

// AllTunnels returns local + ERP tunnels. On name collision, ERP wins.
func (cfg *Config) AllTunnels() []TunnelConfig {
	if len(cfg.erpTunnels) == 0 {
		return cfg.Tunnels
	}
	seen := make(map[string]bool)
	var all []TunnelConfig
	for _, t := range cfg.erpTunnels {
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

// AllConnections returns local + ERP connections.
func (cfg *Config) AllConnections() []Connection {
	if len(cfg.erpConnections) == 0 {
		return cfg.Connections
	}
	var all []Connection
	all = append(all, cfg.erpConnections...)
	all = append(all, cfg.Connections...)
	return all
}

// SetERP stores provisioned tunnels and connections from an ERP response.
// It applies defaults to ERP connections so they are ready to use.
// HTTP connections without a token inherit the ERP bearer token when their URL
// points to the same host as the ERP endpoint (same API, same credentials).
func (cfg *Config) SetERP(tunnels []TunnelConfig, connections []Connection) {
	erpHost := hostFromURL(cfg.ERP.Endpoint)
	for i := range connections {
		if connections[i].Source == "" {
			connections[i].Source = "erp"
		}
		// ERP http connections without their own token reuse the ERP token
		// if they point to the same host as the ERP endpoint
		if connections[i].Type == "http" && connections[i].Token == "" && cfg.ERP.Token != "" {
			if erpHost != "" && hostFromURL(connections[i].URL) == erpHost {
				connections[i].Token = cfg.ERP.Token
			}
		}
		// Supplement with locally stored secrets from keychain.
		// Only fills in gaps — if ERP provides a secret, it wins.
		if connections[i].Password == "" {
			if v, err := keyring.Get(ServiceName, connections[i].Name+"-password"); err == nil {
				connections[i].Password = v
			}
		}
		if connections[i].Token == "" {
			if v, err := keyring.Get(ServiceName, connections[i].Name+"-token"); err == nil {
				connections[i].Token = v
			}
		}
		ApplyConnectionDefaults(&connections[i])
	}
	for i := range tunnels {
		if tunnels[i].Source == "" {
			tunnels[i].Source = "erp"
		}
		if tunnels[i].MTU == 0 {
			tunnels[i].MTU = 1420
		}
		if tunnels[i].KeepAlive == 0 {
			tunnels[i].KeepAlive = 25
		}
		// Supplement tunnel secrets from keychain
		if tunnels[i].PrivateKey == "" {
			if v, err := keyring.Get(ServiceName, "tunnel-"+tunnels[i].Name+"-private-key"); err == nil {
				tunnels[i].PrivateKey = v
			}
		}
		if tunnels[i].PresharedKey == "" {
			if v, err := keyring.Get(ServiceName, "tunnel-"+tunnels[i].Name+"-preshared-key"); err == nil {
				tunnels[i].PresharedKey = v
			}
		}
	}
	cfg.erpTunnels = tunnels
	cfg.erpConnections = connections
}

// HasERP reports whether ERP provisioning is configured (endpoint + token present).
func (cfg *Config) HasERP() bool {
	return cfg.ERP.Endpoint != "" && cfg.ERP.Token != ""
}

// ERPStatus reports whether ERP provisioning has loaded data.
func (cfg *Config) ERPStatus() (tunnels, connections int) {
	return len(cfg.erpTunnels), len(cfg.erpConnections)
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
		if _, err := toml.DecodeFile(path, cfg); err != nil {
			return nil, fmt.Errorf("parse config %s: %w", path, err)
		}

		// Migrate old-format config sections (best-effort)
		var legacy legacyConfigFile
		toml.DecodeFile(path, &legacy)
		migrateFromLegacy(cfg, &legacy)
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
		if cfg.Tunnels[i].MTU == 0 {
			cfg.Tunnels[i].MTU = 1420
		}
		if cfg.Tunnels[i].KeepAlive == 0 {
			cfg.Tunnels[i].KeepAlive = 25
		}
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
	}
}

// hostFromURL extracts the hostname (without port) from a URL string.
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
	// Provisioning token
	if v, err := keyring.Get(ServiceName, "provisioning-token"); err == nil {
		cfg.ERP.Token = v
	}

	// Connection secrets (password or token, keyed by connection name)
	for i := range cfg.Connections {
		name := cfg.Connections[i].Name
		if v, err := keyring.Get(ServiceName, name+"-password"); err == nil {
			cfg.Connections[i].Password = v
		}
		if v, err := keyring.Get(ServiceName, name+"-token"); err == nil && cfg.Connections[i].Token == "" {
			cfg.Connections[i].Token = v
		}
	}

	// Tunnel secrets (private key, preshared key)
	for i := range cfg.Tunnels {
		name := cfg.Tunnels[i].Name
		if v, err := keyring.Get(ServiceName, "tunnel-"+name+"-private-key"); err == nil {
			cfg.Tunnels[i].PrivateKey = v
		}
		if v, err := keyring.Get(ServiceName, "tunnel-"+name+"-preshared-key"); err == nil {
			cfg.Tunnels[i].PresharedKey = v
		}
	}
}

func loadEnv(cfg *Config) {
	// Server port
	if v := os.Getenv("MUX_PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil {
			cfg.Server.Port = p
		}
	}

	// Provisioning
	if v := os.Getenv("MUX_PROVISIONING_ENDPOINT"); v != "" {
		cfg.ERP.Endpoint = v
	}
	if v := os.Getenv("MUX_PROVISIONING_TOKEN"); v != "" {
		cfg.ERP.Token = v
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
// local or ERP connections. Returns a pointer to the actual element
// (not a copy), so mutations persist in memory.
func (cfg *Config) FindAnyConnection(name string) *Connection {
	for i := range cfg.Connections {
		if cfg.Connections[i].Name == name {
			return &cfg.Connections[i]
		}
	}
	for i := range cfg.erpConnections {
		if cfg.erpConnections[i].Name == name {
			return &cfg.erpConnections[i]
		}
	}
	return nil
}

// --- Secrets ---

// SaveSecret stores a credential in the OS keychain.
func SaveSecret(key, value string) error {
	return keyring.Set(ServiceName, key, value)
}

// GetSecret reads a credential from the OS keychain.
func GetSecret(key string) (string, error) {
	return keyring.Get(ServiceName, key)
}

// DeleteSecret removes a credential from the OS keychain.
func DeleteSecret(key string) error {
	return keyring.Delete(ServiceName, key)
}

// ValidSecretKey checks that a keychain key matches known mux patterns.
func ValidSecretKey(key string) bool {
	if key == "provisioning-token" {
		return true
	}
	if strings.HasSuffix(key, "-password") ||
		strings.HasSuffix(key, "-token") ||
		strings.HasSuffix(key, "-oauth-token") ||
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
	if raw, err := keyring.Get(ServiceName, service+"-oauth-token"); err == nil && raw != "" {
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
	if err := keyring.Set(ServiceName, s.service+"-oauth-token", string(data)); err != nil {
		return fmt.Errorf("save token to keychain: %w", err)
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
	if err := keyring.Set(ServiceName, service+"-oauth-client-id", clientID); err != nil {
		return err
	}
	if clientSecret != "" {
		return keyring.Set(ServiceName, service+"-oauth-client-secret", clientSecret)
	}
	return nil
}

// LoadOAuthClientID loads a stored OAuth client ID from the keychain.
func LoadOAuthClientID(service string) (clientID, clientSecret string) {
	clientID, _ = keyring.Get(ServiceName, service+"-oauth-client-id")
	clientSecret, _ = keyring.Get(ServiceName, service+"-oauth-client-secret")
	return
}
