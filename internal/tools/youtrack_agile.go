package tools

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/smnhffmnn/mux/internal/config"
)

const youtrackAgileMaxBody = 4 * 1024 * 1024 // 4 MB

// youtrackAgileListFetchTop is the upper bound we request from YouTrack so we
// can reverse the natural (ascending) order client-side and return the newest
// sprints first. Generously sized — boards rarely accumulate this many sprints.
const youtrackAgileListFetchTop = 10000

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

	// YouTrack returns sprints in ascending order (oldest first) and exposes
	// no $orderBy/$reverse parameter on this endpoint. To honor the documented
	// "newest first" contract we fetch the full list, reverse it, and trim.
	path := fmt.Sprintf("/api/agiles/%s/sprints?fields=id,name,start,finish&$top=%d", y.boardID, youtrackAgileListFetchTop)
	body, err := y.fetch(ctx, path)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	var sprints []json.RawMessage
	if err := json.Unmarshal(body, &sprints); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("decode sprints: %v", err)), nil
	}

	for i, j := 0, len(sprints)-1; i < j; i, j = i+1, j-1 {
		sprints[i], sprints[j] = sprints[j], sprints[i]
	}
	if len(sprints) > top {
		sprints = sprints[:top]
	}

	out, err := json.Marshal(sprints)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("encode sprints: %v", err)), nil
	}
	return mcp.NewToolResultText(string(out)), nil
}

func (y *YouTrackAgile) doGet(ctx context.Context, path string) (*mcp.CallToolResult, error) {
	body, err := y.fetch(ctx, path)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(string(body)), nil
}

func (y *YouTrackAgile) fetch(ctx context.Context, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, y.baseURL+path, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+y.token)

	resp, err := y.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, youtrackAgileMaxBody))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("YouTrack API error (HTTP %d): %s", resp.StatusCode, string(body))
	}

	return body, nil
}
