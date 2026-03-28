//go:build !notray

package main

// Shared data types for Wails bindings.
// All types use json tags for automatic serialization to the Svelte frontend.

// PageData is the top-level structure returned by GetPageData().
type PageData struct {
	Server      ServerInfo      `json:"server"`
	ERP         ERPInfo         `json:"erp"`
	Tunnels     []TunnelInfo    `json:"tunnels"`
	Connections []ConnInfo      `json:"connections"`
	Types       []TypeListEntry `json:"types"`
}

// ServerInfo contains version/uptime metadata shown in the header.
type ServerInfo struct {
	Version        string `json:"version"`
	Uptime         string `json:"uptime"`
	Port           int    `json:"port"`
	BuildTime      string `json:"buildTime"`
	CanSelfUpdate  bool   `json:"canSelfUpdate"`
}

// ERPInfo describes ERP provisioning status.
type ERPInfo struct {
	Configured    bool   `json:"configured"`
	Endpoint      string `json:"endpoint"`
	TokenSet      bool   `json:"tokenSet"`
	Tunnels       int    `json:"tunnels"`
	Connections   int    `json:"connections"`
	ResultMessage string `json:"resultMessage,omitempty"`
	ResultSuccess bool   `json:"resultSuccess,omitempty"`
}

// TunnelInfo describes a WireGuard or SSH tunnel.
type TunnelInfo struct {
	Name            string `json:"name"`
	Type            string `json:"type"`                              // "wireguard" or "ssh"
	PeerEndpoint    string `json:"peerEndpoint,omitempty"`            // WireGuard
	TunnelAddress   string `json:"tunnelAddress,omitempty"`           // WireGuard
	PeerPublicKey   string `json:"peerPublicKey,omitempty"`           // WireGuard (display only, not secret)
	AllowedIPs      string `json:"allowedIPs,omitempty"`              // WireGuard
	DNS             string `json:"dns,omitempty"`                     // WireGuard
	MTU             int    `json:"mtu,omitempty"`                     // WireGuard
	KeepAlive       int    `json:"keepAlive,omitempty"`               // WireGuard
	Host            string `json:"host,omitempty"`                    // SSH
	Port            int    `json:"port,omitempty"`                    // SSH
	User            string `json:"user,omitempty"`                    // SSH
	KeyFile         string `json:"keyFile,omitempty"`                 // SSH
	InsecureHostKey bool   `json:"insecureHostKey,omitempty"`         // SSH
	Source          string `json:"source"`
	Connected       bool   `json:"connected"`
	PrivateKeySet   bool   `json:"privateKeySet"`                     // whether a private key is stored
	PresharedKeySet bool   `json:"presharedKeySet,omitempty"`         // WireGuard: whether a preshared key is stored
}

// ConnInfo describes a connection with its current field values.
type ConnInfo struct {
	Name         string      `json:"name"`
	Type         string      `json:"type"`
	TypeLabel    string      `json:"typeLabel"`
	Configured   bool        `json:"configured"`
	Source       string      `json:"source"`
	Tunnel       string      `json:"tunnel"`
	Summary      string      `json:"summary"`
	IsProxy      bool        `json:"isProxy"`
	IsOAuth      bool        `json:"isOAuth"`
	OAuthOK      bool        `json:"oauthOK"`
	IsERP        bool        `json:"isERP"`
	IsDeviceAuth bool        `json:"isDeviceAuth"`
	DeviceAuthOK bool        `json:"deviceAuthOK"`
	ReadOnly     bool        `json:"readOnly"`
	Instructions string      `json:"instructions"`
	Fields       []FieldInfo `json:"fields"`
}

// FieldInfo describes a single form field for a connection.
type FieldInfo struct {
	Key          string `json:"key"`
	Label        string `json:"label"`
	Value        string `json:"value"`
	Placeholder  string `json:"placeholder"`
	Secret       bool   `json:"secret"`
	Small        bool   `json:"small"`
	SecretStored bool   `json:"secretStored"`
}

// TypeListEntry is a single entry in the connection type picker.
type TypeListEntry struct {
	Type  string `json:"type"`
	Label string `json:"label"`
}

// TestResult is returned by TestConnection().
type TestResult struct {
	Connection string `json:"connection"`
	Connected  bool   `json:"connected"`
	Message    string `json:"message"`
	Latency    string `json:"latency,omitempty"`
}

// UpdateResult is returned by SelfUpdate().
type UpdateResult struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
}

// OAuthStartResult is returned by StartOAuth().
type OAuthStartResult struct {
	AuthURL string `json:"authURL"`
}

// OAuthStatus is returned by GetOAuthStatus().
type OAuthStatus struct {
	Authorized bool   `json:"authorized"`
	Message    string `json:"message"`
}

// DeviceAuthStart is returned by StartDeviceAuth().
type DeviceAuthStart struct {
	UserCode        string `json:"userCode"`
	VerificationURI string `json:"verificationURI"`
}

// DeviceAuthStatus is returned by GetDeviceAuthStatus().
type DeviceAuthStatus struct {
	Completed bool   `json:"completed"`
	Message   string `json:"message"`
}

// SaveTunnelRequest contains the fields to save for a tunnel.
type SaveTunnelRequest struct {
	// WireGuard fields
	PeerPublicKey string `json:"peerPublicKey,omitempty"`
	PeerEndpoint  string `json:"peerEndpoint,omitempty"`
	AllowedIPs    string `json:"allowedIPs,omitempty"`
	TunnelAddress string `json:"tunnelAddress,omitempty"`
	DNS           string `json:"dns,omitempty"`
	MTU           string `json:"mtu,omitempty"`
	KeepAlive     string `json:"keepAlive,omitempty"`
	PrivateKey    string `json:"privateKey,omitempty"`
	PresharedKey  string `json:"presharedKey,omitempty"`

	// SSH fields
	Host            string `json:"host,omitempty"`
	Port            string `json:"port,omitempty"`
	User            string `json:"user,omitempty"`
	KeyFile         string `json:"keyFile,omitempty"`
	InsecureHostKey *bool  `json:"insecureHostKey,omitempty"` // pointer to distinguish unset from false
}

// SaveConnectionRequest contains the fields to save for a connection.
type SaveConnectionRequest struct {
	Host         string `json:"host,omitempty"`
	Port         string `json:"port,omitempty"`
	User         string `json:"user,omitempty"`
	Password     string `json:"password,omitempty"`
	Database     string `json:"database,omitempty"`
	URL          string `json:"url,omitempty"`
	Token        string `json:"token,omitempty"`
	Scopes       string `json:"scopes,omitempty"`
	Tunnel       string `json:"tunnel,omitempty"`
	Instructions string `json:"instructions,omitempty"`
}
