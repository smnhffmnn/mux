package tools

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/smnhffmnn/mux/internal/config"
)

const meilisearchMaxBody = 1024 * 1024 // 1 MB (read returns full documents)

// Meilisearch exposes a Meilisearch index as MCP tools (search + read).
type Meilisearch struct {
	client       *http.Client
	baseURL      string
	apiKey       string
	index        string
	instructions string
}

// NewMeilisearch creates a Meilisearch connection from config.
// The index name is read from conn.Database; the API key from conn.Token.
// Token is optional — Meilisearch can run without authentication.
func NewMeilisearch(conn config.Connection, dialer Dialer) (*Meilisearch, error) {
	if conn.Host == "" {
		return nil, fmt.Errorf("meilisearch: host is required")
	}
	if conn.Database == "" {
		return nil, fmt.Errorf("meilisearch: index (database) is required")
	}

	scheme := "http"
	if conn.Secure {
		scheme = "https"
	}
	baseURL := fmt.Sprintf("%s://%s:%d", scheme, conn.Host, conn.Port)

	transport := &http.Transport{
		ResponseHeaderTimeout: 30 * time.Second,
		TLSClientConfig:       &tls.Config{},
	}
	if dialer != nil {
		transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, addr)
		}
	}

	return &Meilisearch{
		client: &http.Client{
			Transport: transport,
			Timeout:   30 * time.Second,
		},
		baseURL:      baseURL,
		apiKey:       conn.Token,
		index:        conn.Database,
		instructions: conn.Instructions,
	}, nil
}

// Tools returns the MCP tools for this Meilisearch connection.
func (m *Meilisearch) Tools() []ToolDef {
	searchDesc := fmt.Sprintf("Search the Meilisearch index %q using hybrid search (keyword + semantic). Returns matching documents with title, path, and content snippet.", m.index)
	if m.instructions != "" {
		searchDesc += "\n\n" + m.instructions
	}

	readDesc := fmt.Sprintf("Read a full document from the Meilisearch index %q by its path.", m.index)

	return []ToolDef{
		{
			Tool: mcp.NewTool("search",
				mcp.WithDescription(searchDesc),
				mcp.WithString("query", mcp.Required(),
					mcp.Description("Search query (keywords or natural language)."),
				),
				mcp.WithNumber("limit",
					mcp.Description("Maximum number of results to return (default: 5)."),
				),
			),
			Handler: m.handleSearch,
		},
		{
			Tool: mcp.NewTool("read",
				mcp.WithDescription(readDesc),
				mcp.WithString("path", mcp.Required(),
					mcp.Description("Document path as returned by the search tool."),
				),
			),
			Handler: m.handleRead,
		},
	}
}

func (m *Meilisearch) handleSearch(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	query, err := req.RequireString("query")
	if err != nil || query == "" {
		return mcp.NewToolResultError("query parameter is required"), nil
	}

	limit := 5
	if v, ok := req.GetArguments()["limit"].(float64); ok && v > 0 {
		limit = int(v)
	}
	if limit > 20 {
		limit = 20
	}

	body := map[string]any{
		"q":     query,
		"limit": limit,
		"hybrid": map[string]any{
			"embedder":      "openai",
			"semanticRatio": 0.7,
		},
	}

	respBody, status, err := m.doJSON(ctx, fmt.Sprintf("/indexes/%s/search", m.index), body)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Meilisearch request failed: %v", err)), nil
	}
	if status < 200 || status >= 300 {
		return mcp.NewToolResultError(fmt.Sprintf("Meilisearch API error (HTTP %d): %s", status, string(respBody))), nil
	}

	// Parse response and extract hits
	var result struct {
		Hits []map[string]any `json:"hits"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("parse response: %v", err)), nil
	}

	if len(result.Hits) == 0 {
		return mcp.NewToolResultText("No results found."), nil
	}

	// Format hits as readable output
	var sb strings.Builder
	for i, hit := range result.Hits {
		title, _ := hit["title"].(string)
		path, _ := hit["path"].(string)
		content, _ := hit["content"].(string)

		if title == "" {
			title = path
		}

		// Truncate content to a snippet (rune-safe for multi-byte UTF-8)
		snippet := content
		if runes := []rune(snippet); len(runes) > 500 {
			snippet = string(runes[:500]) + "..."
		}

		if i > 0 {
			sb.WriteString("\n---\n\n")
		}
		fmt.Fprintf(&sb, "### %s\n", title)
		if path != "" {
			fmt.Fprintf(&sb, "**Path:** %s\n\n", path)
		}
		if snippet != "" {
			sb.WriteString(snippet)
			sb.WriteString("\n")
		}
	}

	return mcp.NewToolResultText(sb.String()), nil
}

func (m *Meilisearch) handleRead(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	path, err := req.RequireString("path")
	if err != nil || path == "" {
		return mcp.NewToolResultError("path parameter is required"), nil
	}

	// Use a filter for deterministic exact-match lookup.
	// Requires "path" to be configured as a filterable attribute in the index.
	escaped := strings.ReplaceAll(path, `\`, `\\`)
	escaped = strings.ReplaceAll(escaped, "'", `\'`)
	body := map[string]any{
		"q":      "",
		"limit":  1,
		"filter": fmt.Sprintf("path = '%s'", escaped),
	}

	respBody, status, err := m.doJSON(ctx, fmt.Sprintf("/indexes/%s/search", m.index), body)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Meilisearch request failed: %v", err)), nil
	}
	if status < 200 || status >= 300 {
		return mcp.NewToolResultError(fmt.Sprintf("Meilisearch API error (HTTP %d): %s", status, string(respBody))), nil
	}

	var result struct {
		Hits []map[string]any `json:"hits"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("parse response: %v", err)), nil
	}

	if len(result.Hits) == 0 {
		return mcp.NewToolResultError(fmt.Sprintf("document not found: %s", path)), nil
	}

	hit := result.Hits[0]
	title, _ := hit["title"].(string)
	content, _ := hit["content"].(string)

	var sb strings.Builder
	if title != "" {
		fmt.Fprintf(&sb, "# %s\n\n", title)
	}
	sb.WriteString(content)

	return mcp.NewToolResultText(sb.String()), nil
}

// doJSON sends a JSON POST request to the Meilisearch API and returns the response body.
func (m *Meilisearch) doJSON(ctx context.Context, path string, body any) ([]byte, int, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, 0, fmt.Errorf("marshal request: %w", err)
	}

	url := m.baseURL + path
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, 0, fmt.Errorf("create request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	if m.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+m.apiKey)
	}

	resp, err := m.client.Do(httpReq)
	if err != nil {
		return nil, 0, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, meilisearchMaxBody))
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("read response: %w", err)
	}

	return respBody, resp.StatusCode, nil
}
