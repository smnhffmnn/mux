package tools

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/smnhffmnn/mux/internal/config"
)

// elevenlabsMaxBody caps inlined responses. Binary payload never takes that
// path: it is streamed to a file instead, so a long narration is not truncated.
const elevenlabsMaxBody = 512 * 1024 // 512 KB

// DefaultElevenLabsInstructions are used when no custom instructions are set.
const DefaultElevenLabsInstructions = `ElevenLabs Audio API — Text-to-Speech, Voice Cloning, Sound Effects.

Tools:
- get  — GET requests (e.g. /v1/voices)
- post — POST requests with a JSON body (e.g. text-to-speech)

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

Binary responses (audio and anything else non-textual) are written to disk,
never inlined: pass output_file (absolute path) to choose the destination, or
omit it and the connector writes into the mux output directory. Either way the
result is the file path plus metadata (status, content-type, size). Textual
responses (JSON, XML, text/*) come back inline, truncated at 512 KB with a
marker — endpoints that wrap audio in JSON (e.g. .../with-timestamps) are
better fetched with output_file.

Auth: xi-api-key: {token} (automatic)`

// elevenlabsAPIKeyHeader carries the credential. It is a custom header, so Go
// would replay it to a redirect target on another host — see NewElevenLabs.
const elevenlabsAPIKeyHeader = "xi-api-key"

// ElevenLabs wraps the ElevenLabs REST API as MCP tools.
type ElevenLabs struct {
	client    *http.Client
	baseURL   string
	apiKey    string
	outputDir string
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
		// Generation happens before the first byte arrives; 120s matches the
		// other generative connector (gemini).
		ResponseHeaderTimeout: 120 * time.Second,
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
			// Covers the whole exchange including streaming the audio to disk,
			// so it has to outlast a long narration — not just the API call.
			Timeout: 10 * time.Minute,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 10 {
					return fmt.Errorf("stopped after 10 redirects")
				}
				// Go strips Authorization on a host change but keeps custom
				// headers, which would hand the API key to the redirect target.
				if req.URL.Host != via[0].URL.Host {
					req.Header.Del(elevenlabsAPIKeyHeader)
				}
				return nil
			},
		},
		baseURL:   baseURL,
		apiKey:    conn.Token,
		outputDir: filepath.Join(config.Dir(), "output"),
	}, nil
}

// Tools returns the MCP tools for the ElevenLabs connection.
func (e *ElevenLabs) Tools() []ToolDef {
	outputFileDesc := "Save the response body to this absolute file path instead of returning it inline (supports ~ for home directory). " +
		"Binary responses such as audio are always written to a file — without output_file they go to the mux output directory. " +
		"Returns metadata only (status, content-type, path, size). An existing file is never overwritten. " +
		"Only writes on 2xx responses; errors are returned inline."

	return []ToolDef{
		{
			Tool: mcp.NewTool("get",
				mcp.WithDescription(fmt.Sprintf(
					"Make an HTTP GET request to %s and return the response body. Binary responses "+
						"(e.g. audio downloads from the history endpoints) are written to a file instead — see output_file.\n\n"+
						"Auth: xi-api-key header (automatic).\n\n"+
						"Useful endpoints:\n"+
						"- GET /v1/voices — List available voices\n"+
						"- GET /v1/user/subscription — Subscription status and remaining credits\n"+
						"- GET /v1/models — List available TTS models",
					e.baseURL,
				)),
				mcp.WithString("path", mcp.Required(), mcp.Description("Path to append to the base URL, e.g. /v1/voices")),
				mcp.WithString("output_file", mcp.Description(outputFileDesc)),
			),
			Handler: e.handleGet,
		},
		{
			Tool: mcp.NewTool("post",
				mcp.WithDescription(fmt.Sprintf(
					"Make an HTTP POST request to %s with a JSON body.\n\n"+
						"Auth: xi-api-key header (automatic).\n\n"+
						"Useful endpoints:\n"+
						"- POST /v1/text-to-speech/{voice_id} — Text-to-speech (body: text, model_id)\n"+
						"- POST /v1/text-to-speech/{voice_id}/stream — Streaming text-to-speech\n"+
						"- POST /v1/sound-generation — Sound effects (body: text, duration_seconds)\n\n"+
						"These endpoints answer with raw audio bytes. The audio is written to a file and the "+
						"result carries the path plus metadata — pass output_file to pick the destination, "+
						"otherwise the mux output directory is used.",
					e.baseURL,
				)),
				mcp.WithString("path", mcp.Required(), mcp.Description("Path to append to the base URL, e.g. /v1/text-to-speech/{voice_id}")),
				mcp.WithString("body", mcp.Required(), mcp.Description("JSON request body")),
				mcp.WithString("output_file", mcp.Description(outputFileDesc)),
			),
			Handler: e.handlePost,
		},
	}
}

func (e *ElevenLabs) handleGet(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return e.doRequest(ctx, http.MethodGet, req, false)
}

func (e *ElevenLabs) handlePost(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return e.doRequest(ctx, http.MethodPost, req, true)
}

func (e *ElevenLabs) doRequest(ctx context.Context, method string, req mcp.CallToolRequest, requireBody bool) (*mcp.CallToolResult, error) {
	path, _ := req.RequireString("path")
	if path == "" {
		return mcp.NewToolResultError("path is required"), nil
	}

	jsonBody := req.GetString("body", "")
	if requireBody && jsonBody == "" {
		return mcp.NewToolResultError("body is required"), nil
	}

	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}

	// Check the destination before the call: a generation costs credits, and
	// failing on the path afterwards would throw the paid-for audio away.
	outputFile := req.GetString("output_file", "")
	if outputFile != "" {
		clean, err := validateOutputFile(outputFile)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		outputFile = clean
	}

	var bodyReader io.Reader
	if jsonBody != "" && method != http.MethodGet {
		bodyReader = strings.NewReader(jsonBody)
	}

	httpReq, err := http.NewRequestWithContext(ctx, method, e.baseURL+path, bodyReader)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("invalid request: %v", err)), nil
	}

	// Not "application/json": the audio endpoints answer with audio/mpeg and the
	// history endpoints hand out raw files, so this connector accepts anything
	// and decides by the response's Content-Type.
	httpReq.Header.Set("Accept", "*/*")
	if bodyReader != nil {
		httpReq.Header.Set("Content-Type", "application/json")
	}
	httpReq.Header.Set(elevenlabsAPIKeyHeader, e.apiKey)

	resp, err := e.client.Do(httpReq)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("request failed: %v", err)), nil
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		contentType := sniffContentType(resp)
		if outputFile != "" {
			return saveResponseToFile(resp, contentType, outputFile)
		}
		// A narration track is megabytes of audio; inlining it would blow up the
		// caller's context, and truncating it would corrupt the file. Stream it
		// to the output directory and answer with the path.
		if ext, ok := binaryResponseExt(contentType); ok {
			return saveResponseToGeneratedFile(resp, contentType, e.outputDir, "elevenlabs-*"+ext)
		}
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, elevenlabsMaxBody+1))
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("read response: %v", err)), nil
	}

	var note string
	if len(body) > elevenlabsMaxBody {
		body = body[:elevenlabsMaxBody]
		note = fmt.Sprintf("\n\n[truncated at %d bytes — pass output_file to write the full response to disk]", elevenlabsMaxBody)
	}

	result := fmt.Sprintf("HTTP %d %s\n\n%s%s", resp.StatusCode, resp.Status, string(body), note)
	return mcp.NewToolResultText(result), nil
}
