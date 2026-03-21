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

const ideogramMaxBody = 512 * 1024 // 512 KB

// DefaultIdeogramInstructions are used when no custom instructions are set.
const DefaultIdeogramInstructions = `Ideogram Image Generation API.

Endpoints:
- POST /v1/ideogram-v3/generate — Generate image from text prompt
  Body: { "prompt": "...", "rendering_speed": "DEFAULT", "resolution": "1024x1024", "aspect_ratio": "ASPECT_1_1", "style_type": "AUTO", "magic_prompt": true, "num_images": 1 }
  Rendering speeds: FLASH, TURBO, DEFAULT, QUALITY
  Style types: AUTO, GENERAL, REALISTIC, DESIGN, FICTION

- POST /v1/ideogram-v3/remix — Remix an existing image (multipart: image, prompt)
- POST /v1/ideogram-v3/edit — Edit an image (multipart: image, mask, prompt)
- POST /v1/ideogram-v3/reframe — Reframe/extend an image (multipart: image, resolution)
- POST /v1/ideogram-v3/replace-background — Replace image background (multipart: image, prompt)
- POST /v1/ideogram-v3/describe — Describe an image (multipart: image)

Auth: Api-Key header (automatic)`

// Ideogram wraps the Ideogram REST API as MCP tools.
type Ideogram struct {
	client  *http.Client
	baseURL string
	apiKey  string
}

// NewIdeogram creates an Ideogram connection from config.
func NewIdeogram(conn config.Connection, dialer Dialer) (*Ideogram, error) {
	if conn.Token == "" {
		return nil, fmt.Errorf("ideogram: API key is required")
	}

	baseURL := strings.TrimRight(conn.URL, "/")
	if baseURL == "" {
		baseURL = "https://api.ideogram.ai"
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

	return &Ideogram{
		client: &http.Client{
			Transport: transport,
			Timeout:   60 * time.Second,
		},
		baseURL: baseURL,
		apiKey:  conn.Token,
	}, nil
}

// Tools returns the MCP tools for the Ideogram connection.
func (i *Ideogram) Tools() []ToolDef {
	return []ToolDef{
		{
			Tool: mcp.NewTool("get",
				mcp.WithDescription(fmt.Sprintf(
					"Make an HTTP GET request to %s and return the response body.\n\n"+
						"Auth: Api-Key header (automatic).",
					i.baseURL,
				)),
				mcp.WithString("path", mcp.Required(), mcp.Description("Path to append to the base URL")),
			),
			Handler: i.handleGet,
		},
		{
			Tool: mcp.NewTool("post",
				mcp.WithDescription(fmt.Sprintf(
					"Make an HTTP POST request to %s with a JSON body and return the response.\n\n"+
						"Auth: Api-Key header (automatic).\n\n"+
						"Useful endpoints:\n"+
						"- POST /v1/ideogram-v3/generate — Generate image (body: prompt, rendering_speed, resolution, aspect_ratio, style_type, magic_prompt, num_images)",
					i.baseURL,
				)),
				mcp.WithString("path", mcp.Required(), mcp.Description("Path to append to the base URL, e.g. /v1/ideogram-v3/generate")),
				mcp.WithString("body", mcp.Required(), mcp.Description("JSON request body")),
			),
			Handler: i.handlePost,
		},
	}
}

func (i *Ideogram) handleGet(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	path, _ := req.RequireString("path")
	if path == "" {
		return mcp.NewToolResultError("path is required"), nil
	}

	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, i.baseURL+path, nil)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("invalid request: %v", err)), nil
	}

	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("Api-Key", i.apiKey)

	resp, err := i.client.Do(httpReq)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("request failed: %v", err)), nil
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, ideogramMaxBody))
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("read response: %v", err)), nil
	}

	result := fmt.Sprintf("HTTP %d %s\n\n%s", resp.StatusCode, resp.Status, string(body))
	return mcp.NewToolResultText(result), nil
}

func (i *Ideogram) handlePost(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	path, _ := req.RequireString("path")
	if path == "" {
		return mcp.NewToolResultError("path is required"), nil
	}

	jsonBody, _ := req.RequireString("body")
	if jsonBody == "" {
		return mcp.NewToolResultError("body is required"), nil
	}

	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, i.baseURL+path, strings.NewReader(jsonBody))
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("invalid request: %v", err)), nil
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("Api-Key", i.apiKey)

	resp, err := i.client.Do(httpReq)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("request failed: %v", err)), nil
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, ideogramMaxBody))
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("read response: %v", err)), nil
	}

	result := fmt.Sprintf("HTTP %d %s\n\n%s", resp.StatusCode, resp.Status, string(body))
	return mcp.NewToolResultText(result), nil
}
