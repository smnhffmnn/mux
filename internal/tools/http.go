package tools

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/smnhffmnn/mux/internal/config"
)

const maxResponseBody = 100 * 1024 // 100 KB

// HTTP wraps an HTTP API as MCP tools.
type HTTP struct {
	client       *http.Client
	baseURL      string
	token        string
	instructions string
}

// NewHTTP creates an HTTP connection from config.
func NewHTTP(conn config.Connection, dialer Dialer) (*HTTP, error) {
	if conn.URL == "" {
		return nil, fmt.Errorf("http: URL is required")
	}

	// Normalize base URL (strip trailing slash)
	baseURL := strings.TrimRight(conn.URL, "/")

	transport := &http.Transport{
		ResponseHeaderTimeout: 30 * time.Second,
		TLSClientConfig:       &tls.Config{InsecureSkipVerify: true},
	}
	if dialer != nil {
		transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, addr)
		}
	}

	return &HTTP{
		client: &http.Client{
			Transport: transport,
			Timeout:   30 * time.Second,
		},
		baseURL:      baseURL,
		token:        conn.Token,
		instructions: conn.Instructions,
	}, nil
}

// Tools returns the MCP tools for this HTTP connection.
func (h *HTTP) Tools() []ToolDef {
	desc := fmt.Sprintf("Make an HTTP GET request to %s and return the response body.", h.baseURL)
	if h.instructions != "" {
		desc += "\n\n" + h.instructions
	}

	return []ToolDef{
		{
			Tool: mcp.NewTool("get",
				mcp.WithDescription(desc),
				mcp.WithString("path",
					mcp.Required(),
					mcp.Description("Path to append to the base URL, e.g. /api/products"),
				),
			),
			Handler: h.handleGet,
		},
	}
}

func (h *HTTP) handleGet(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	path, _ := req.RequireString("path")
	if path == "" {
		return mcp.NewToolResultError("path is required"), nil
	}

	// Ensure path starts with /
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}

	url := h.baseURL + path

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("invalid request: %v", err)), nil
	}

	httpReq.Header.Set("Accept", "application/json")
	if h.token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+h.token)
	}

	resp, err := h.client.Do(httpReq)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("request failed: %v", err)), nil
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("read response: %v", err)), nil
	}

	result := fmt.Sprintf("HTTP %d %s\n\n%s", resp.StatusCode, resp.Status, string(body))
	return mcp.NewToolResultText(result), nil
}
