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

const recraftMaxBody = 512 * 1024 // 512 KB

// DefaultRecraftInstructions are used when no custom instructions are set.
const DefaultRecraftInstructions = `Recraft Image Generation API.

Base URL already includes /v1 — use paths WITHOUT /v1 prefix.

Endpoints:
- POST /images/generations — Generate image from text prompt
  Body: { "prompt": "...", "model": "recraftv4", "size": "1024x1024", "style": "realistic_image", "n": 1, "response_format": "url" }
  Models: recraftv4, recraftv4_vector, recraftv4_pro, recraftv4_pro_vector, recraftv3, recraftv3_vector
  Styles (V3 only): realistic_image, digital_illustration, vector_illustration
  Styles (V4): realistic_image, digital_illustration (V4 does NOT support vector_illustration, icon, or logo)
  Sizes: 1024x1024, 1365x1024, 1024x1365, 1536x1024, 1024x1536, 1820x1024, 1024x1820, 1024x2048, 2048x1024, 1434x1024, 1024x1434, 1024x1280, 1280x1024, 1024x1149, 1149x1024
  Note: response_format "url" returns WebP images (not PNG). Use "b64_json" for base64-encoded PNG.

- GET /users/me — User info and remaining credits

- POST /images/vectorize — Vectorize a raster image (multipart: file)
- POST /images/removeBackground — Remove image background (multipart: file)
- POST /images/crispUpscale — Crisp upscale (multipart: file)
- POST /images/creativeUpscale — Creative upscale with enhancement (multipart: file)
- POST /images/imageToImage — Image-to-image transformation (multipart: file, prompt, strength)
- POST /images/inpaint — Inpainting (multipart: file, mask, prompt)
- POST /images/replaceBackground — Replace image background (multipart: file, prompt)
- POST /styles — Create a style reference (multipart: files)

Multipart endpoints take a local file via the post tool: pass file_path
(absolute path, sent as form field "file") and the text values as form_fields,
e.g. {"prompt": "...", "strength": 0.5}; leave body empty. One file per
request — /images/inpaint needs file and mask together and cannot be served yet.

Auth: Authorization: Bearer {token} (automatic)`

// Recraft wraps the Recraft REST API as MCP tools.
type Recraft struct {
	client  *http.Client
	baseURL string
	apiKey  string
}

// NewRecraft creates a Recraft connection from config.
func NewRecraft(conn config.Connection, dialer Dialer) (*Recraft, error) {
	if conn.Token == "" {
		return nil, fmt.Errorf("recraft: API key is required")
	}

	baseURL := strings.TrimRight(conn.URL, "/")
	if baseURL == "" {
		baseURL = "https://external.api.recraft.ai/v1"
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

	return &Recraft{
		client: &http.Client{
			Transport: transport,
			Timeout:   60 * time.Second,
		},
		baseURL: baseURL,
		apiKey:  conn.Token,
	}, nil
}

// Tools returns the MCP tools for the Recraft connection.
func (r *Recraft) Tools() []ToolDef {
	postOpts := []mcp.ToolOption{
		mcp.WithDescription(fmt.Sprintf(
			"Make an HTTP POST request to %s with a JSON body, or upload a local image as multipart/form-data, and return the response.\n\n"+
				"Auth: Bearer token (automatic).\n\n"+
				"Useful endpoints:\n"+
				"- POST /images/generations — Generate image (body: prompt, model, size, style, n, response_format)\n"+
				"- POST /images/vectorize, /images/removeBackground, /images/crispUpscale, /images/creativeUpscale — file_path only\n"+
				"- POST /images/imageToImage, /images/replaceBackground — file_path + form_fields {\"prompt\": \"...\"}",
			r.baseURL,
		)),
		mcp.WithString("path", mcp.Required(), mcp.Description("Path to append to the base URL, e.g. /images/generations")),
		mcp.WithString("body", mcp.Description("JSON request body (required unless file_path is given). "+jsonBodyDesc)),
	}
	postOpts = append(postOpts, uploadParams("file")...)

	return []ToolDef{
		{
			Tool: mcp.NewTool("get",
				mcp.WithDescription(fmt.Sprintf(
					"Make an HTTP GET request to %s and return the response body.\n\n"+
						"Auth: Bearer token (automatic).\n\n"+
						"Useful endpoints:\n"+
						"- GET /users/me — User info and remaining credits",
					r.baseURL,
				)),
				mcp.WithString("path", mcp.Required(), mcp.Description("Path to append to the base URL, e.g. /users/me")),
			),
			Handler: r.handleGet,
		},
		{
			Tool:    mcp.NewTool("post", postOpts...),
			Handler: r.handlePost,
		},
	}
}

func (r *Recraft) handleGet(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	path, _ := req.RequireString("path")
	if path == "" {
		return mcp.NewToolResultError("path is required"), nil
	}

	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, r.baseURL+path, nil)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("invalid request: %v", err)), nil
	}

	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+r.apiKey)

	resp, err := r.client.Do(httpReq)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("request failed: %v", err)), nil
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, recraftMaxBody))
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("read response: %v", err)), nil
	}

	result := fmt.Sprintf("HTTP %d %s\n\n%s", resp.StatusCode, resp.Status, string(body))
	return mcp.NewToolResultText(result), nil
}

func (r *Recraft) handlePost(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	path, _ := req.RequireString("path")
	if path == "" {
		return mcp.NewToolResultError("path is required"), nil
	}
	if req.GetString("body", "") == "" && req.GetString("file_path", "") == "" {
		return mcp.NewToolResultError("body or file_path is required"), nil
	}

	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}

	httpReq, upload, err := newBodyRequest(ctx, http.MethodPost, r.baseURL+path, req, "file")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+r.apiKey)

	resp, err := clientFor(r.client, upload).Do(httpReq)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("request failed: %v", err)), nil
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, recraftMaxBody))
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("read response: %v", err)), nil
	}

	result := statusLine(resp, upload) + "\n\n" + string(body)
	return mcp.NewToolResultText(result), nil
}
