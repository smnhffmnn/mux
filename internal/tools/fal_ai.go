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
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/smnhffmnn/mux/internal/config"
)

const falAIMaxBody = 1024 * 1024 // 1 MB

// Model identifiers accept alphanumerics, slash, dot, dash, underscore — e.g. "bytedance/seedance-2.0/text-to-video".
// Path-traversal segments (`..`, `//`) are rejected separately to prevent URL smuggling.
var falModelRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9/_.-]{0,255}$`)

// FalAI wraps the fal.ai queue REST API as MCP tools.
type FalAI struct {
	client  *http.Client
	baseURL string
	apiKey  string
}

// NewFalAI creates a FalAI connection from config.
func NewFalAI(conn config.Connection, dialer Dialer) (*FalAI, error) {
	if conn.Token == "" {
		return nil, fmt.Errorf("fal-ai: API key is required")
	}

	baseURL := strings.TrimRight(conn.URL, "/")
	if baseURL == "" {
		baseURL = "https://queue.fal.run"
	}

	transport := &http.Transport{
		ResponseHeaderTimeout: 60 * time.Second,
		TLSClientConfig:       &tls.Config{},
	}
	if dialer != nil {
		transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, addr)
		}
	}

	return &FalAI{
		client: &http.Client{
			Transport: transport,
			Timeout:   120 * time.Second,
		},
		baseURL: baseURL,
		apiKey:  conn.Token,
	}, nil
}

// Tools returns the MCP tools for the fal.ai connection.
func (f *FalAI) Tools() []ToolDef {
	return []ToolDef{
		{
			Tool: mcp.NewTool("submit",
				mcp.WithDescription("Submit an async job to a fal.ai model. Returns request_id and status/response URLs for polling. Use the status tool to check progress."),
				mcp.WithString("model", mcp.Required(), mcp.Description("Model endpoint, e.g. bytedance/seedance-2.0/text-to-video")),
				mcp.WithString("input", mcp.Required(), mcp.Description("JSON string with model input parameters")),
			),
			Handler: f.handleSubmit,
		},
		{
			Tool: mcp.NewTool("status",
				mcp.WithDescription("Check the status of a fal.ai queue request. Returns status (IN_QUEUE, IN_PROGRESS, COMPLETED), queue position, and logs. When COMPLETED, use the result tool to fetch the full response."),
				mcp.WithString("status_url", mcp.Required(), mcp.Description("The status_url from the submit response")),
			),
			Handler: f.handleStatus,
		},
		{
			Tool: mcp.NewTool("result",
				mcp.WithDescription("Fetch the result of a completed fal.ai request. Only call after status shows COMPLETED."),
				mcp.WithString("response_url", mcp.Required(), mcp.Description("The response_url from the submit response")),
			),
			Handler: f.handleResult,
		},
	}
}

// doJSON performs an HTTP request with Key auth against a path relative to baseURL.
func (f *FalAI) doJSON(ctx context.Context, method, path string, body any) ([]byte, int, error) {
	var reqBody io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, 0, fmt.Errorf("marshal request: %w", err)
		}
		reqBody = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, f.baseURL+path, reqBody)
	if err != nil {
		return nil, 0, fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Key "+f.apiKey)

	resp, err := f.client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, falAIMaxBody))
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("read response: %w", err)
	}

	return data, resp.StatusCode, nil
}

// validateQueueURL ensures a status/response URL points to the configured fal.ai host
// over http(s). Prevents SSRF if the API response contains manipulated URLs.
// Fails closed: an unparseable or host-less configured baseURL rejects all queue URLs.
func (f *FalAI) validateQueueURL(fullURL string) error {
	u, err := url.Parse(fullURL)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return fmt.Errorf("URL scheme must be http or https, got %q", u.Scheme)
	}
	base, err := url.Parse(f.baseURL)
	if err != nil || base.Hostname() == "" {
		return fmt.Errorf("configured fal.ai baseURL is invalid or empty")
	}
	if !strings.EqualFold(u.Hostname(), base.Hostname()) {
		return fmt.Errorf("URL host %q does not match configured fal.ai host %q", u.Host, base.Host)
	}
	return nil
}

// doFullURL performs an HTTP request with Key auth against an absolute URL.
func (f *FalAI) doFullURL(ctx context.Context, method, fullURL string) ([]byte, int, error) {
	if err := f.validateQueueURL(fullURL); err != nil {
		return nil, 0, err
	}

	req, err := http.NewRequestWithContext(ctx, method, fullURL, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Authorization", "Key "+f.apiKey)

	resp, err := f.client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, falAIMaxBody))
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("read response: %w", err)
	}

	return data, resp.StatusCode, nil
}

func (f *FalAI) handleSubmit(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	model, _ := req.RequireString("model")
	if model == "" {
		return mcp.NewToolResultError("model is required"), nil
	}
	if !falModelRe.MatchString(model) || strings.Contains(model, "..") || strings.Contains(model, "//") {
		return mcp.NewToolResultError("model must match /^[a-zA-Z0-9][a-zA-Z0-9/_.-]{0,255}$/ without `..` or `//` (e.g. bytedance/seedance-2.0/text-to-video)"), nil
	}

	inputStr, _ := req.RequireString("input")
	if inputStr == "" {
		return mcp.NewToolResultError("input is required"), nil
	}

	// Validate that input is valid JSON
	var input json.RawMessage
	if err := json.Unmarshal([]byte(inputStr), &input); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("input must be valid JSON: %v", err)), nil
	}

	data, status, err := f.doJSON(ctx, http.MethodPost, "/"+model, input)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	if status != http.StatusOK {
		return mcp.NewToolResultError(fmt.Sprintf("fal.ai API error (HTTP %d): %s", status, string(data))), nil
	}

	return mcp.NewToolResultText(string(data)), nil
}

func (f *FalAI) handleStatus(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	statusURL, _ := req.RequireString("status_url")
	if statusURL == "" {
		return mcp.NewToolResultError("status_url is required"), nil
	}

	data, status, err := f.doFullURL(ctx, http.MethodGet, statusURL)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	if status != http.StatusOK {
		return mcp.NewToolResultError(fmt.Sprintf("fal.ai API error (HTTP %d): %s", status, string(data))), nil
	}

	return mcp.NewToolResultText(string(data)), nil
}

func (f *FalAI) handleResult(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	responseURL, _ := req.RequireString("response_url")
	if responseURL == "" {
		return mcp.NewToolResultError("response_url is required"), nil
	}

	data, status, err := f.doFullURL(ctx, http.MethodGet, responseURL)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	if status != http.StatusOK {
		return mcp.NewToolResultError(fmt.Sprintf("fal.ai API error (HTTP %d): %s", status, string(data))), nil
	}

	return mcp.NewToolResultText(string(data)), nil
}
