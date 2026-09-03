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

Tools:
- get   — GET requests (e.g. /v1/models)
- request — POST/PUT/PATCH/DELETE with a JSON body, or a multipart file upload

Common POST endpoints (use the request tool):
- /v1/chat/completions — Chat completions (gpt-4o, gpt-4o-mini, o1, o3-mini, …)
- /v1/responses — Responses API
- /v1/embeddings — Text embeddings (text-embedding-3-small/-large)
- /v1/images/generations — Image generation (DALL·E, gpt-image-*)

Multipart endpoints take a local file: pass file_path (absolute path, sent as
form field "file") and the remaining fields as form_fields, e.g.
- /v1/audio/transcriptions, /v1/audio/translations — Whisper: form_fields {"model": "whisper-1"}
- /v1/files — File upload: form_fields {"purpose": "assistants"}
- /v1/images/edits — Image edit: file_field "image", form_fields {"prompt": "..."}
The file is streamed from disk and never enters the conversation. One file per
request.

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

	requestOpts := []mcp.ToolOption{
		mcp.WithDescription(fmt.Sprintf(
			"Make an HTTP request to %s with a JSON body, or upload a local file as multipart/form-data. Supports POST, PUT, PATCH, and DELETE.\n\n"+
				"Auth: Bearer token (automatic).\n\n"+
				"Common POST endpoints:\n"+
				"- POST /v1/chat/completions — Chat completions (gpt-4o, gpt-4o-mini, o1, o3-mini, …)\n"+
				"- POST /v1/responses — Responses API\n"+
				"- POST /v1/embeddings — Text embeddings\n"+
				"- POST /v1/images/generations — Image generation (DALL·E, gpt-image-*)\n"+
				"- POST /v1/audio/transcriptions, /v1/audio/translations — Whisper (file_path + form_fields {\"model\": \"whisper-1\"})\n"+
				"- POST /v1/files — File upload (file_path + form_fields {\"purpose\": \"assistants\"})",
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
			mcp.Description("JSON request body (optional). "+jsonBodyDesc),
		),
		mcp.WithString("output_file", mcp.Description(outputFileDesc)),
	}
	requestOpts = append(requestOpts, uploadParams("file")...)

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
			Tool:    mcp.NewTool("request", requestOpts...),
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

	// Check the destination before anything is sent: a request that fails on
	// "output_file already exists" afterwards has already happened on the
	// server (a POST may have written) or has moved a whole file for nothing.
	// saveResponseToFileWithNote stays the authority (its O_EXCL decides the
	// race); this is only the early exit. It runs before newBodyRequest opens
	// the upload file, so the early return leaks no file handle.
	if outputFile := req.GetString("output_file", ""); outputFile != "" {
		if _, err := validateOutputFile(outputFile); err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
	}

	httpReq, upload, err := newBodyRequest(ctx, method, o.baseURL+path, req, "file")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+o.apiKey)

	resp, err := clientFor(o.client, upload).Do(httpReq)
	if err != nil {
		return mcp.NewToolResultError(requestError(ctx, err, httpReq, upload)), nil
	}
	defer resp.Body.Close()

	if outputFile := req.GetString("output_file", ""); outputFile != "" && resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return saveResponseToFileWithNote(resp, resp.Header.Get("Content-Type"), outputFile, upload.note())
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, openaiMaxBody))
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("read response: %v", err)), nil
	}

	result := statusLine(resp, upload) + "\n\n" + string(body)
	return mcp.NewToolResultText(result), nil
}
