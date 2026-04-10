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

const elevenlabsMaxBody = 512 * 1024 // 512 KB

// DefaultElevenLabsInstructions are used when no custom instructions are set.
const DefaultElevenLabsInstructions = `ElevenLabs Audio API — Text-to-Speech, Voice Cloning, Sound Effects.

Endpoints:
- POST /v1/text-to-speech/{voice_id} — TTS (returns audio stream)
  Models: eleven_flash_v2_5 (fast, cost-efficient), eleven_multilingual_v2 (highest quality)
  Body: { "text": "...", "model_id": "eleven_flash_v2_5" }
  Response: Audio-Stream (mp3_44100_128 default)

- POST /v1/text-to-speech/{voice_id}/stream — Streaming TTS (chunked audio)

- POST /v1/sound-generation — Sound Effects Generation
  Body: { "text": "description of sound", "duration_seconds": 5 }

- GET /v1/voices — List available voices (premade + custom)
- GET /v1/models — List available TTS models
- GET /v1/user/subscription — Subscription status, remaining credits

Auth: xi-api-key: {token} (automatic)`

// ElevenLabs wraps the ElevenLabs REST API as MCP tools.
type ElevenLabs struct {
	client  *http.Client
	baseURL string
	apiKey  string
}

// NewElevenLabs creates an ElevenLabs connection from config.
func NewElevenLabs(conn config.Connection, dialer Dialer) (*ElevenLabs, error) {
	if conn.Token == "" {
		return nil, fmt.Errorf("elevenlabs: API key is required")
	}

	baseURL := strings.TrimRight(conn.URL, "/")
	if baseURL == "" {
		baseURL = "https://api.elevenlabs.io"
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

	return &ElevenLabs{
		client: &http.Client{
			Transport: transport,
			Timeout:   60 * time.Second,
		},
		baseURL: baseURL,
		apiKey:  conn.Token,
	}, nil
}

// Tools returns the MCP tools for the ElevenLabs connection.
func (e *ElevenLabs) Tools() []ToolDef {
	return []ToolDef{
		{
			Tool: mcp.NewTool("get",
				mcp.WithDescription(fmt.Sprintf(
					"Make an HTTP GET request to %s and return the response body.\n\n"+
						"Auth: xi-api-key header (automatic).\n\n"+
						"Useful endpoints:\n"+
						"- GET /v1/voices — List available voices\n"+
						"- GET /v1/user/subscription — Subscription status and remaining credits\n"+
						"- GET /v1/models — List available TTS models",
					e.baseURL,
				)),
				mcp.WithString("path", mcp.Required(), mcp.Description("Path to append to the base URL, e.g. /v1/voices")),
			),
			Handler: e.handleGet,
		},
	}
}

func (e *ElevenLabs) handleGet(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	path, _ := req.RequireString("path")
	if path == "" {
		return mcp.NewToolResultError("path is required"), nil
	}

	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, e.baseURL+path, nil)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("invalid request: %v", err)), nil
	}

	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("xi-api-key", e.apiKey)

	resp, err := e.client.Do(httpReq)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("request failed: %v", err)), nil
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, elevenlabsMaxBody))
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("read response: %v", err)), nil
	}

	result := fmt.Sprintf("HTTP %d %s\n\n%s", resp.StatusCode, resp.Status, string(body))
	return mcp.NewToolResultText(result), nil
}
