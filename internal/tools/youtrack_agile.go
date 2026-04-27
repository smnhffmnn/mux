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

const youtrackAgileMaxBody = 256 * 1024 // 256 KB

// YouTrackAgile wraps the YouTrack Agile Board REST API as MCP tools.
type YouTrackAgile struct {
	client       *http.Client
	baseURL      string
	token        string
	boardID      string
	instructions string
}

// NewYouTrackAgile creates a YouTrack Agile connection from config.
func NewYouTrackAgile(conn config.Connection, dialer Dialer) (*YouTrackAgile, error) {
	if conn.URL == "" {
		return nil, fmt.Errorf("youtrack-agile: base URL is required")
	}
	if conn.Token == "" {
		return nil, fmt.Errorf("youtrack-agile: token is required")
	}
	if conn.Database == "" {
		return nil, fmt.Errorf("youtrack-agile: board ID is required")
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

	return &YouTrackAgile{
		client: &http.Client{
			Transport: transport,
			Timeout:   30 * time.Second,
		},
		baseURL:      strings.TrimRight(conn.URL, "/"),
		token:        conn.Token,
		boardID:      conn.Database,
		instructions: conn.Instructions,
	}, nil
}

// Tools returns the MCP tools for this YouTrack Agile connection.
func (y *YouTrackAgile) Tools() []ToolDef {
	currentDesc := "Get the current sprint from the agile board. Returns the sprint name, start date, finish date, and goal."
	listDesc := "List sprints from the agile board. Returns sprint names, start dates, and finish dates."
	if y.instructions != "" {
		currentDesc += "\n\n" + y.instructions
		listDesc += "\n\n" + y.instructions
	}

	return []ToolDef{
		{
			Tool: mcp.NewTool("get_current_sprint",
				mcp.WithDescription(currentDesc),
			),
			Handler: y.handleGetCurrentSprint,
		},
		{
			Tool: mcp.NewTool("list_sprints",
				mcp.WithDescription(listDesc),
				mcp.WithNumber("top", mcp.Description("Maximum number of sprints to return (default: 50)")),
			),
			Handler: y.handleListSprints,
		},
	}
}

func (y *YouTrackAgile) handleGetCurrentSprint(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	path := fmt.Sprintf("/api/agiles/%s/sprints/current?fields=id,name,start,finish,goal", y.boardID)
	return y.doGet(ctx, path)
}

func (y *YouTrackAgile) handleListSprints(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	top := 50
	if v, ok := req.GetArguments()["top"].(float64); ok && v > 0 {
		top = int(v)
	}

	path := fmt.Sprintf("/api/agiles/%s/sprints?fields=id,name,start,finish&$top=%d", y.boardID, top)
	return y.doGet(ctx, path)
}

func (y *YouTrackAgile) doGet(ctx context.Context, path string) (*mcp.CallToolResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, y.baseURL+path, nil)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("create request: %v", err)), nil
	}

	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+y.token)

	resp, err := y.client.Do(req)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("request failed: %v", err)), nil
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, youtrackAgileMaxBody))
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("read response: %v", err)), nil
	}

	if resp.StatusCode != http.StatusOK {
		return mcp.NewToolResultError(fmt.Sprintf("YouTrack API error (HTTP %d): %s", resp.StatusCode, string(body))), nil
	}

	return mcp.NewToolResultText(string(body)), nil
}
