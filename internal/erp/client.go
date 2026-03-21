package erp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/smnhffmnn/mux/internal/config"
)

// ProvisionResponse is the expected JSON structure from the ERP provisioning endpoint.
type ProvisionResponse struct {
	Tunnels     []config.TunnelConfig `json:"tunnels,omitempty"`
	Connections []config.Connection   `json:"connections"`
}

// Fetch retrieves the provisioning config from the ERP endpoint.
// It sends a GET request with the bearer token and parses the JSON response.
// All returned tunnels and connections are marked with Source = "erp".
func Fetch(ctx context.Context, endpoint, token string) (*ProvisionResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")

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
		return nil, fmt.Errorf("ERP returned redirect (%d → %s) — token is likely expired or invalid", resp.StatusCode, location)
	}

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("ERP authentication failed (HTTP %d) — check your token", resp.StatusCode)
	}

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1 MB

	if resp.StatusCode != http.StatusOK {
		var jsonErr struct{ Error string `json:"error"` }
		if json.Unmarshal(body, &jsonErr) == nil && jsonErr.Error != "" {
			return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, jsonErr.Error)
		}
		return nil, fmt.Errorf("ERP returned HTTP %d", resp.StatusCode)
	}

	// HTML instead of JSON = auth gateway intercepted the request
	if len(body) > 0 && body[0] != '{' && body[0] != '[' {
		return nil, fmt.Errorf("ERP returned HTML instead of JSON — token is likely expired or invalid")
	}

	var result ProvisionResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("invalid JSON from %s: %w", endpoint, err)
	}

	// Mark all entries as ERP-sourced
	for i := range result.Tunnels {
		result.Tunnels[i].Source = "erp"
	}
	for i := range result.Connections {
		result.Connections[i].Source = "erp"
	}

	return &result, nil
}
