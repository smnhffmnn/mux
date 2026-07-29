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

const openaiMaxBody = 512 * 1024 // 512 KB

// DefaultOpenAIInstructions are used when no custom instructions are set.
const DefaultOpenAIInstructions = `OpenAI API.

Tools:
- get   — GET requests (e.g. /v1/models)
- request — POST/PUT/PATCH/DELETE with a JSON body

Common POST endpoints (use the request tool):
- /v1/chat/completions — Chat completions (gpt-4o, gpt-4o-mini, o1, o3-mini, …)
- /v1/responses — Responses API
- /v1/embeddings — Text embeddings (text-embedding-3-small/-large)
- /v1/images/generations — Image generation (DALL·E, gpt-image-*)

Multipart-upload endpoints (e.g. /v1/audio/transcriptions) are not supported
here — the request tool sends application/json only.

For binary or large responses pass output_file to stream the body to disk
instead of returning it inline.

Auth: Authorization: Bearer {token} (automatic)`

// OpenAI wraps the OpenAI REST API as MCP tools.
type OpenAI struct {
	client  *http.Client
	baseURL string
	apiKey  string
}

// NewOpenAI creates an OpenAI connection from config.
func NewOpenAI(conn config.Connection, dialer Dialer) (*OpenAI, error) {
	if conn.Token == "" {
		return nil, fmt.Errorf("openai: API key is required")
	}

	baseURL := strings.TrimRight(conn.URL, "/")
	if baseURL == "" {
		baseURL = "https://api.openai.com"
	}

	transport := &http.Transport{
		ResponseHeaderTimeout: 30 * time.Second,
		TLSClientConfig:       &tls.Config{},
	}
	if dialer != nil {
		transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, addr)
		}
	}

	return &OpenAI{
		client: &http.Client{
			Transport: transport,
			Timeout:   60 * time.Second,
		},
		baseURL: baseURL,
		apiKey:  conn.Token,
	}, nil
}

// Tools returns the MCP tools for the OpenAI connection.
func (o *OpenAI) Tools() []ToolDef {
	outputFileDesc := "Save response body to this absolute file path instead of returning it inline (supports ~ for home directory). Useful for large or binary responses (audio transcriptions, image bytes). Returns only metadata (status, content-type, path, size). An existing file is never overwritten. Only writes on 2xx responses; errors are returned inline."

	return []ToolDef{
		{
			Tool: mcp.NewTool("get",
				mcp.WithDescription(fmt.Sprintf(
					"Make an HTTP GET request to %s and return the response body.\n\n"+
						"Auth: Bearer token (automatic).\n\n"+
						"Useful endpoints:\n"+
						"- GET /v1/models — List available models\n"+
						"- GET /v1/models/{model_id} — Get model details",
					o.baseURL,
				)),
				mcp.WithString("path", mcp.Required(), mcp.Description("Path to append to the base URL, e.g. /v1/models")),
				mcp.WithString("output_file", mcp.Description(outputFileDesc)),
			),
			Handler: o.handleGet,
		},
		{
			Tool: mcp.NewTool("request",
				mcp.WithDescription(fmt.Sprintf(
					"Make an HTTP request with a JSON body to %s. Supports POST, PUT, PATCH, and DELETE.\n\n"+
						"Auth: Bearer token (automatic).\n\n"+
						"Common POST endpoints:\n"+
						"- POST /v1/chat/completions — Chat completions (gpt-4o, gpt-4o-mini, o1, o3-mini, …)\n"+
						"- POST /v1/responses — Responses API\n"+
						"- POST /v1/embeddings — Text embeddings\n"+
						"- POST /v1/images/generations — Image generation (DALL·E, gpt-image-*)\n"+
						"- POST /v1/audio/transcriptions, /v1/audio/translations — Whisper (multipart, not supported here)",
					o.baseURL,
				)),
				mcp.WithString("method",
					mcp.Required(),
					mcp.Description("HTTP method: POST, PUT, PATCH, or DELETE"),
					mcp.Enum("POST", "PUT", "PATCH", "DELETE"),
				),
				mcp.WithString("path",
					mcp.Required(),
					mcp.Description("Path to append to the base URL, e.g. /v1/chat/completions"),
				),
				mcp.WithString("body",
					mcp.Description("JSON request body (optional)"),
				),
				mcp.WithString("output_file", mcp.Description(outputFileDesc)),
			),
			Handler: o.handleRequest,
		},
	}
}

func (o *OpenAI) handleGet(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return o.doRequest(ctx, http.MethodGet, req)
}

func (o *OpenAI) handleRequest(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	method, _ := req.RequireString("method")
	if method == "" {
		return mcp.NewToolResultError("method is required (POST, PUT, PATCH, or DELETE)"), nil
	}
	method = strings.ToUpper(method)

	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		// valid
	default:
		return mcp.NewToolResultError(fmt.Sprintf("unsupported method: %s (use POST, PUT, PATCH, or DELETE)", method)), nil
	}

	return o.doRequest(ctx, method, req)
}

func (o *OpenAI) doRequest(ctx context.Context, method string, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	path, _ := req.RequireString("path")
	if path == "" {
		return mcp.NewToolResultError("path is required"), nil
	}

	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}

	var bodyReader io.Reader
	if body := req.GetString("body", ""); body != "" {
		bodyReader = bytes.NewBufferString(body)
	}

	httpReq, err := http.NewRequestWithContext(ctx, method, o.baseURL+path, bodyReader)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("invalid request: %v", err)), nil
	}

	httpReq.Header.Set("Accept", "application/json")
	if bodyReader != nil {
		httpReq.Header.Set("Content-Type", "application/json")
	}
	httpReq.Header.Set("Authorization", "Bearer "+o.apiKey)

	resp, err := o.client.Do(httpReq)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("request failed: %v", err)), nil
	}
	defer resp.Body.Close()

	if outputFile := req.GetString("output_file", ""); outputFile != "" && resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return saveResponseToFile(resp, resp.Header.Get("Content-Type"), outputFile)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, openaiMaxBody))
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("read response: %v", err)), nil
	}

	result := fmt.Sprintf("HTTP %d %s\n\n%s", resp.StatusCode, resp.Status, string(body))
	return mcp.NewToolResultText(result), nil
}
