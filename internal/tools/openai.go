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

const openaiMaxBody = 512 * 1024 // 512 KB

// DefaultOpenAIInstructions are used when no custom instructions are set.
const DefaultOpenAIInstructions = `OpenAI API.

Endpoints:
- POST /v1/audio/transcriptions — Speech-to-Text (Whisper)
  Models: gpt-4o-mini-transcribe ($0.003/min), gpt-4o-transcribe ($0.006/min), whisper-1 ($0.006/min)
  Accepts: mp3, mp4, mpeg, mpga, m4a, wav, webm (max 25MB)

- POST /v1/chat/completions — Chat Completions
  Models: gpt-4o, gpt-4o-mini, o1, o3-mini, etc.

- POST /v1/embeddings — Text Embeddings
  Models: text-embedding-3-small, text-embedding-3-large

- POST /v1/images/generations — Image Generation (DALL-E)
  Models: dall-e-3, dall-e-2

- GET /v1/models — List available models

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
			),
			Handler: o.handleGet,
		},
	}
}

func (o *OpenAI) handleGet(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	path, _ := req.RequireString("path")
	if path == "" {
		return mcp.NewToolResultError("path is required"), nil
	}

	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, o.baseURL+path, nil)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("invalid request: %v", err)), nil
	}

	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+o.apiKey)

	resp, err := o.client.Do(httpReq)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("request failed: %v", err)), nil
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, openaiMaxBody))
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("read response: %v", err)), nil
	}

	result := fmt.Sprintf("HTTP %d %s\n\n%s", resp.StatusCode, resp.Status, string(body))
	return mcp.NewToolResultText(result), nil
}
