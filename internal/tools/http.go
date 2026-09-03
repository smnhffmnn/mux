package tools

import (
	"context"
	"crypto/tls"
	"encoding/base64"
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
	tokenScheme  string            // "" (bearer) or "basic"
	basicSuffix  string            // fixed password component when tokenScheme=basic (token is the Basic username)
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
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 10 {
					return fmt.Errorf("stopped after 10 redirects")
				}
				// Go strips Authorization on a host change but replays custom
				// headers, which would hand a token_header credential to the
				// redirect target.
				if conn.TokenHeader != "" && req.URL.Host != via[0].URL.Host {
					req.Header.Del(conn.TokenHeader)
				}
				return nil
			},
		},
		baseURL:      baseURL,
		token:        conn.Token,
		tokenHeader:  conn.TokenHeader,
		tokenScheme:  conn.TokenScheme,
		basicSuffix:  conn.BasicSuffix,
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

	outputFileDesc := "Save response body to this absolute file path instead of returning it inline (supports ~ for home directory). Useful for large responses (images, binary data). Returns only metadata (status, content-type, path, size). An existing file is never overwritten. Only writes on 2xx responses; errors are returned inline."

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
		writeDesc := fmt.Sprintf("Make an HTTP request with a body to %s. Supports POST, PUT, PATCH, and DELETE. "+
			"The body is JSON by default; with file_path it becomes a multipart/form-data upload streamed from a local file.", h.baseURL)

		opts := []mcp.ToolOption{
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
				mcp.Description(jsonBodyDesc),
			),
			mcp.WithString("output_file",
				mcp.Description(outputFileDesc),
			),
		}
		opts = append(opts, uploadParams("file")...)

		tools = append(tools, ToolDef{
			Tool:    mcp.NewTool("request", opts...),
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

	httpReq, upload, err := newBodyRequest(ctx, method, url, req, "file")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	httpReq.Header.Set("Accept", "application/json")
	// Custom headers may override the defaults above; the token header below
	// wins over a custom header of the same name (auth stays vault-managed).
	for name, value := range h.headers {
		httpReq.Header.Set(name, value)
	}
	if upload != nil {
		// The multipart boundary is per request — a configured Content-Type
		// header would break the upload, so it loses here.
		httpReq.Header.Set("Content-Type", upload.contentTypeHeader)
	}
	if h.token != "" {
		switch {
		case h.tokenScheme == "basic":
			cred := base64.StdEncoding.EncodeToString([]byte(h.token + ":" + h.basicSuffix))
			httpReq.Header.Set("Authorization", "Basic "+cred)
		case h.tokenHeader != "":
			httpReq.Header.Set(h.tokenHeader, h.token)
		default:
			httpReq.Header.Set("Authorization", "Bearer "+h.token)
		}
	}

	resp, err := clientFor(h.client, upload).Do(httpReq)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("request failed: %v", err)), nil
	}
	defer resp.Body.Close()

	// If output_file is set and response is successful, stream to disk
	if outputFile := req.GetString("output_file", ""); outputFile != "" && resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return saveResponseToFileWithNote(resp, resp.Header.Get("Content-Type"), outputFile, upload.note())
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("read response: %v", err)), nil
	}

	result := statusLine(resp, upload) + "\n\n" + string(body)
	return mcp.NewToolResultText(result), nil
}
