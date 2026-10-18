package provisioning

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/smnhffmnn/mux/internal/config"
)

// ProvisionResponse is the parsed provisioning payload.
type ProvisionResponse struct {
	Tunnels     []config.TunnelConfig
	Connections []config.Connection
}

// wireTunnel captures tunnel fields from the provisioning JSON that
// TunnelConfig deliberately refuses to (de)serialize. PrivateKey is tagged
// `json:"-"` on TunnelConfig so the key can never leak through outbound
// serialization (UI page data, config writes) — but the provisioning
// endpoint is the one legitimate inbound path that may carry it (e.g.
// managed tunnels whose key material is relayed live from the VPN
// provisioner and never stored anywhere). The outer field shadows the
// embedded one during unmarshal; Fetch copies it over explicitly.
type wireTunnel struct {
	config.TunnelConfig
	PrivateKey string `json:"privateKey,omitempty"`
	Passphrase string `json:"passphrase,omitempty"`
}

// wireConnection does the same for ssh connections: privateKey and
// passphrase are `json:"-"` on Connection and only this path may carry them.
type wireConnection struct {
	config.Connection
	PrivateKey string `json:"privateKey,omitempty"`
	Passphrase string `json:"passphrase,omitempty"`
}

// wireResponse is the raw JSON shape of the provisioning endpoint.
type wireResponse struct {
	Tunnels     []wireTunnel     `json:"tunnels,omitempty"`
	Connections []wireConnection `json:"connections"`
}

// Request headers that carry the installation's identity (config.Instance).
// A server that keys tunnel credentials per instance reads them; one that
// does not simply ignores them.
const (
	HeaderInstance = "X-Mux-Instance"
	HeaderHostname = "X-Mux-Hostname"
)

// Fetch retrieves the provisioning config from the provisioning endpoint.
// It sends a GET request with the bearer token and parses the JSON response.
// All returned tunnels and connections are marked with Source = "provisioning".
//
// instance identifies this installation; with an empty ID no identity headers
// are sent and the server serves the user's shared configuration.
func Fetch(ctx context.Context, endpoint, token string, instance config.Instance) (*ProvisionResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if instance.ID != "" {
		req.Header.Set(HeaderInstance, instance.ID)
		if instance.Hostname != "" {
			req.Header.Set(HeaderHostname, instance.Hostname)
		}
	}

	// Don't follow redirects — a redirect to a login page means the token is invalid.
	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	// Redirect = auth problem
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		location := resp.Header.Get("Location")
		return nil, fmt.Errorf("provisioning server returned redirect (%d → %s) — token is likely expired or invalid", resp.StatusCode, location)
	}

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("provisioning authentication failed (HTTP %d) — check your token", resp.StatusCode)
	}

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1 MB

	if resp.StatusCode != http.StatusOK {
		var jsonErr struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(body, &jsonErr) == nil && jsonErr.Error != "" {
			return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, jsonErr.Error)
		}
		return nil, fmt.Errorf("provisioning server returned HTTP %d", resp.StatusCode)
	}

	// HTML instead of JSON = auth gateway intercepted the request
	if len(body) > 0 && body[0] != '{' && body[0] != '[' {
		return nil, fmt.Errorf("provisioning server returned HTML instead of JSON — token is likely expired or invalid")
	}

	var wire wireResponse
	if err := json.Unmarshal(body, &wire); err != nil {
		return nil, fmt.Errorf("invalid JSON from %s: %w", endpoint, err)
	}

	// Mark all entries as provisioning-sourced and carry over the inbound-only
	// secret fields (see wireTunnel).
	var result ProvisionResponse
	for _, wt := range wire.Tunnels {
		t := wt.TunnelConfig
		t.PrivateKey = wt.PrivateKey
		t.Passphrase = wt.Passphrase
		t.Source = config.SourceProvisioning
		result.Tunnels = append(result.Tunnels, t)
	}
	for _, wc := range wire.Connections {
		c := wc.Connection
		c.PrivateKey = wc.PrivateKey
		c.Passphrase = wc.Passphrase
		c.Source = config.SourceProvisioning
		result.Connections = append(result.Connections, c)
	}

	return &result, nil
}
