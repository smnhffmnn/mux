package oauth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DiscoverMetadata probes an MCP endpoint for its OAuth authorization-server
// metadata URL: it POSTs to the MCP URL and follows the resource_metadata
// hint from the WWW-Authenticate challenge (RFC 9728); when the server sends
// no usable hint, it falls back to the well-known path on the MCP origin.
func DiscoverMetadata(ctx context.Context, mcpURL string) (string, error) {
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

// extractParam pulls a quoted parameter value out of a WWW-Authenticate
// header, e.g. extractParam(`Bearer resource_metadata="https://x"`, "resource_metadata").
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
