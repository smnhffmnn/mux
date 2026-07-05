package tools

import (
	"bytes"
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
	tokenHeader  string            // custom header name (e.g. "x-goog-api-key"); empty = "Authorization: Bearer"
	headers      map[string]string // extra headers sent with every request (e.g. "Notion-Version")
	readOnly     bool
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
		tokenHeader:  conn.TokenHeader,
		headers:      conn.Headers,
		readOnly:     conn.ReadOnly,
		instructions: conn.Instructions,
	}, nil
}

// Tools returns the MCP tools for this HTTP connection.
func (h *HTTP) Tools() []ToolDef {
	getDesc := fmt.Sprintf("Make an HTTP GET request to %s and return the response body.", h.baseURL)
	if h.instructions != "" {
		getDesc += "\n\n" + h.instructions
	}

	outputFileDesc := "Save response body to this file path instead of returning it inline (supports ~ for home directory). Useful for large responses (images, binary data). Returns only metadata (status, content-type, path, size). Only writes on 2xx responses; errors are returned inline."

	tools := []ToolDef{
		{
			Tool: mcp.NewTool("get",
				mcp.WithDescription(getDesc),
				mcp.WithString("path",
					mcp.Required(),
					mcp.Description("Path to append to the base URL, e.g. /api/products"),
				),
				mcp.WithString("output_file",
					mcp.Description(outputFileDesc),
				),
			),
			Handler: h.handleGet,
		},
	}

	if !h.readOnly {
		writeDesc := fmt.Sprintf("Make an HTTP request with a body to %s. Supports POST, PUT, PATCH, and DELETE.", h.baseURL)

		tools = append(tools, ToolDef{
			Tool: mcp.NewTool("request",
				mcp.WithDescription(writeDesc),
				mcp.WithString("method",
					mcp.Required(),
					mcp.Description("HTTP method: POST, PUT, PATCH, or DELETE"),
					mcp.Enum("POST", "PUT", "PATCH", "DELETE"),
				),
				mcp.WithString("path",
					mcp.Required(),
					mcp.Description("Path to append to the base URL, e.g. /api/users/42"),
				),
				mcp.WithString("body",
					mcp.Description("JSON request body (optional)"),
				),
				mcp.WithString("output_file",
					mcp.Description(outputFileDesc),
				),
			),
			Handler: h.handleRequest,
		})
	}

	return tools
}

func (h *HTTP) handleGet(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return h.doRequest(ctx, http.MethodGet, req)
}

func (h *HTTP) handleRequest(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	method, _ := req.RequireString("method")
	method = strings.ToUpper(method)

	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		// valid
	default:
		return mcp.NewToolResultError(fmt.Sprintf("unsupported method: %s (use POST, PUT, PATCH, or DELETE)", method)), nil
	}

	return h.doRequest(ctx, method, req)
}

func (h *HTTP) doRequest(ctx context.Context, method string, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	path, _ := req.RequireString("path")
	if path == "" {
		return mcp.NewToolResultError("path is required"), nil
	}

	// Ensure path starts with /
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}

	url := h.baseURL + path

	var bodyReader io.Reader
	if body, err := req.RequireString("body"); err == nil && body != "" {
		bodyReader = bytes.NewBufferString(body)
	}

	httpReq, err := http.NewRequestWithContext(ctx, method, url, bodyReader)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("invalid request: %v", err)), nil
	}

	httpReq.Header.Set("Accept", "application/json")
	if bodyReader != nil {
		httpReq.Header.Set("Content-Type", "application/json")
	}
	// Custom headers may override the defaults above; the token header below
	// wins over a custom header of the same name (auth stays vault-managed).
	for name, value := range h.headers {
		httpReq.Header.Set(name, value)
	}
	if h.token != "" {
		if h.tokenHeader != "" {
			httpReq.Header.Set(h.tokenHeader, h.token)
		} else {
			httpReq.Header.Set("Authorization", "Bearer "+h.token)
		}
	}

	resp, err := h.client.Do(httpReq)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("request failed: %v", err)), nil
	}
	defer resp.Body.Close()

	// If output_file is set and response is successful, stream to disk
	if outputFile := req.GetString("output_file", ""); outputFile != "" && resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return saveResponseToFile(resp, outputFile)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("read response: %v", err)), nil
	}

	result := fmt.Sprintf("HTTP %d %s\n\n%s", resp.StatusCode, resp.Status, string(body))
	return mcp.NewToolResultText(result), nil
}
