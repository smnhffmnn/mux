package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/smnhffmnn/mux/internal/config"
)

const (
	asanaBaseURL = "https://app.asana.com/api/1.0"
	asanaMaxBody = 512 * 1024
)

// DefaultAsanaInstructions are shown to AI clients for the Asana REST API type.
const DefaultAsanaInstructions = `Asana REST API — project and task management.

Auth: Personal Access Token (Bearer). Base URL: https://app.asana.com/api/1.0

Setup: Create a PAT at https://app.asana.com/0/my-apps → "Create new token".
Then: secret_set key=<name>-token value=0/...

Typical workflow: me → workspaces → projects → sections → tasks.
All IDs are GID strings (e.g. "1234567890").`

// DefaultAsanaMCPInstructions are shown to AI clients for the Asana MCP proxy type.
const DefaultAsanaMCPInstructions = `Asana MCP — proxy to Asana's official MCP server (OAuth).

Proxies all tools from https://mcp.asana.com/v2/mcp. Provides full access
to the Asana Work Graph with automatically updated tools from Asana.

Setup (requires pre-registered OAuth app):
1. Go to https://app.asana.com/0/my-apps → "Create new app" → select "MCP app"
2. Under "OAuth": set Redirect URL to http://localhost:7700/oauth/callback
3. Under "Manage distribution": add your workspace(s)
4. Store credentials in mux:
   secret_set key=<name>-oauth-client-id value=YOUR_CLIENT_ID
   secret_set key=<name>-oauth-client-secret value=YOUR_CLIENT_SECRET
5. The OAuth browser flow will start automatically on next connection attempt.`

// Asana wraps the Asana REST API as MCP tools.
type Asana struct {
	client *http.Client
	token  string
}

// NewAsana creates an Asana connection from config.
func NewAsana(conn config.Connection, dialer Dialer) (*Asana, error) {
	if conn.Token == "" {
		return nil, fmt.Errorf("asana: personal access token is required")
	}

	transport := &http.Transport{
		ResponseHeaderTimeout: 30 * time.Second,
	}
	if dialer != nil {
		transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, addr)
		}
	}

	return &Asana{
		client: &http.Client{
			Transport: transport,
			Timeout:   30 * time.Second,
		},
		token: conn.Token,
	}, nil
}

// Tools returns the MCP tools for the Asana connection.
func (a *Asana) Tools() []ToolDef {
	return []ToolDef{
		{
			Tool:    mcp.NewTool("me", mcp.WithDescription("Get the current authenticated Asana user.")),
			Handler: a.handleMe,
		},
		{
			Tool:    mcp.NewTool("workspaces", mcp.WithDescription("List all accessible Asana workspaces.")),
			Handler: a.handleWorkspaces,
		},
		{
			Tool: mcp.NewTool("projects",
				mcp.WithDescription("List projects in a workspace."),
				mcp.WithString("workspace", mcp.Required(), mcp.Description("Workspace GID")),
				mcp.WithBoolean("archived", mcp.Description("Include archived projects (default: false)")),
			),
			Handler: a.handleProjects,
		},
		{
			Tool: mcp.NewTool("sections",
				mcp.WithDescription("List sections in a project."),
				mcp.WithString("project", mcp.Required(), mcp.Description("Project GID")),
			),
			Handler: a.handleSections,
		},
		{
			Tool: mcp.NewTool("tasks",
				mcp.WithDescription("List tasks. Provide either project or section GID."),
				mcp.WithString("project", mcp.Description("Project GID (list tasks in project)")),
				mcp.WithString("section", mcp.Description("Section GID (list tasks in section)")),
				mcp.WithString("assignee", mcp.Description("Assignee: 'me' or user GID (requires workspace)")),
				mcp.WithString("workspace", mcp.Description("Workspace GID (required when filtering by assignee)")),
				mcp.WithBoolean("completed", mcp.Description("Filter by completion status")),
			),
			Handler: a.handleTasks,
		},
		{
			Tool: mcp.NewTool("get_task",
				mcp.WithDescription("Get detailed information about a specific task."),
				mcp.WithString("task", mcp.Required(), mcp.Description("Task GID")),
			),
			Handler: a.handleGetTask,
		},
		{
			Tool: mcp.NewTool("create_task",
				mcp.WithDescription("Create a new task in Asana."),
				mcp.WithString("name", mcp.Required(), mcp.Description("Task name")),
				mcp.WithString("workspace", mcp.Description("Workspace GID (required if no project specified)")),
				mcp.WithString("project", mcp.Description("Project GID to add the task to")),
				mcp.WithString("section", mcp.Description("Section GID to place the task in")),
				mcp.WithString("assignee", mcp.Description("Assignee: 'me' or user GID")),
				mcp.WithString("due_on", mcp.Description("Due date in YYYY-MM-DD format")),
				mcp.WithString("notes", mcp.Description("Task description (plain text)")),
			),
			Handler: a.handleCreateTask,
		},
		{
			Tool: mcp.NewTool("update_task",
				mcp.WithDescription("Update an existing task."),
				mcp.WithString("task", mcp.Required(), mcp.Description("Task GID")),
				mcp.WithString("name", mcp.Description("New task name")),
				mcp.WithString("assignee", mcp.Description("New assignee: 'me' or user GID")),
				mcp.WithString("due_on", mcp.Description("Due date in YYYY-MM-DD format")),
				mcp.WithString("notes", mcp.Description("Task description (plain text)")),
				mcp.WithBoolean("completed", mcp.Description("Mark task as completed or not")),
			),
			Handler: a.handleUpdateTask,
		},
		{
			Tool: mcp.NewTool("search",
				mcp.WithDescription("Search tasks in a workspace."),
				mcp.WithString("workspace", mcp.Required(), mcp.Description("Workspace GID")),
				mcp.WithString("text", mcp.Description("Full-text search query")),
				mcp.WithString("assignee", mcp.Description("Filter by assignee: 'me' or user GID")),
				mcp.WithBoolean("completed", mcp.Description("Filter by completion status")),
				mcp.WithString("project", mcp.Description("Filter by project GID")),
				mcp.WithString("due_on_before", mcp.Description("Filter: due on or before YYYY-MM-DD")),
				mcp.WithString("due_on_after", mcp.Description("Filter: due on or after YYYY-MM-DD")),
			),
			Handler: a.handleSearch,
		},
	}
}

// --- Handlers ---

func (a *Asana) handleMe(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	data, status, err := a.doAsana(ctx, http.MethodGet, "/users/me?opt_fields=name,email,workspaces,workspaces.name", nil)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if status != http.StatusOK {
		return mcp.NewToolResultError(fmt.Sprintf("Asana API error (HTTP %d): %s", status, string(data))), nil
	}
	return a.formatData(data)
}

func (a *Asana) handleWorkspaces(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	data, status, err := a.doAsana(ctx, http.MethodGet, "/workspaces?opt_fields=name,is_organization", nil)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if status != http.StatusOK {
		return mcp.NewToolResultError(fmt.Sprintf("Asana API error (HTTP %d): %s", status, string(data))), nil
	}
	return a.formatData(data)
}

func (a *Asana) handleProjects(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	workspace, _ := req.RequireString("workspace")
	if workspace == "" {
		return mcp.NewToolResultError("workspace is required"), nil
	}

	path := fmt.Sprintf("/projects?workspace=%s&opt_fields=name,archived,color,current_status,due_on,owner.name&limit=100", workspace)

	if archived, ok := req.GetArguments()["archived"].(bool); ok && archived {
		path += "&archived=true"
	} else {
		path += "&archived=false"
	}

	data, status, err := a.doAsana(ctx, http.MethodGet, path, nil)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if status != http.StatusOK {
		return mcp.NewToolResultError(fmt.Sprintf("Asana API error (HTTP %d): %s", status, string(data))), nil
	}
	return a.formatData(data)
}

func (a *Asana) handleSections(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project, _ := req.RequireString("project")
	if project == "" {
		return mcp.NewToolResultError("project is required"), nil
	}

	path := fmt.Sprintf("/projects/%s/sections?opt_fields=name", project)
	data, status, err := a.doAsana(ctx, http.MethodGet, path, nil)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if status != http.StatusOK {
		return mcp.NewToolResultError(fmt.Sprintf("Asana API error (HTTP %d): %s", status, string(data))), nil
	}
	return a.formatData(data)
}

func (a *Asana) handleTasks(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	project, _ := args["project"].(string)
	section, _ := args["section"].(string)
	assignee, _ := args["assignee"].(string)
	workspace, _ := args["workspace"].(string)

	optFields := "name,assignee.name,due_on,completed,notes"
	var path string

	switch {
	case section != "":
		path = fmt.Sprintf("/sections/%s/tasks?opt_fields=%s&limit=100", section, optFields)
	case project != "":
		path = fmt.Sprintf("/tasks?project=%s&opt_fields=%s&limit=100", project, optFields)
	case assignee != "" && workspace != "":
		path = fmt.Sprintf("/tasks?assignee=%s&workspace=%s&opt_fields=%s&limit=100", assignee, workspace, optFields)
	default:
		return mcp.NewToolResultError("provide project, section, or assignee+workspace"), nil
	}

	if completed, ok := args["completed"].(bool); ok {
		if completed {
			path += "&completed_since=2000-01-01T00:00:00Z"
		}
	}

	data, status, err := a.doAsana(ctx, http.MethodGet, path, nil)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if status != http.StatusOK {
		return mcp.NewToolResultError(fmt.Sprintf("Asana API error (HTTP %d): %s", status, string(data))), nil
	}
	return a.formatData(data)
}

func (a *Asana) handleGetTask(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	task, _ := req.RequireString("task")
	if task == "" {
		return mcp.NewToolResultError("task is required"), nil
	}

	optFields := "name,assignee.name,due_on,completed,notes,projects.name,memberships.section.name,tags.name,custom_fields,created_at,modified_at,permalink_url"
	path := fmt.Sprintf("/tasks/%s?opt_fields=%s", task, optFields)
	data, status, err := a.doAsana(ctx, http.MethodGet, path, nil)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if status != http.StatusOK {
		return mcp.NewToolResultError(fmt.Sprintf("Asana API error (HTTP %d): %s", status, string(data))), nil
	}
	return a.formatData(data)
}

func (a *Asana) handleCreateTask(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	name, _ := req.RequireString("name")
	if name == "" {
		return mcp.NewToolResultError("name is required"), nil
	}

	args := req.GetArguments()
	taskData := map[string]any{"name": name}

	if v, ok := args["workspace"].(string); ok && v != "" {
		taskData["workspace"] = v
	}
	if v, ok := args["assignee"].(string); ok && v != "" {
		taskData["assignee"] = v
	}
	if v, ok := args["due_on"].(string); ok && v != "" {
		taskData["due_on"] = v
	}
	if v, ok := args["notes"].(string); ok && v != "" {
		taskData["notes"] = v
	}

	project, _ := args["project"].(string)
	section, _ := args["section"].(string)

	if project != "" {
		taskData["projects"] = []string{project}
	}

	payload := map[string]any{"data": taskData}
	data, status, err := a.doAsana(ctx, http.MethodPost, "/tasks", payload)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if status != http.StatusCreated {
		return mcp.NewToolResultError(fmt.Sprintf("Asana API error (HTTP %d): %s", status, string(data))), nil
	}

	// If section specified, move task to section
	if section != "" {
		var created struct {
			Data struct {
				GID string `json:"gid"`
			} `json:"data"`
		}
		if json.Unmarshal(data, &created) == nil && created.Data.GID != "" {
			sectionPayload := map[string]any{"data": map[string]any{"task": created.Data.GID}}
			if _, sStatus, sErr := a.doAsana(ctx, http.MethodPost, fmt.Sprintf("/sections/%s/addTask", section), sectionPayload); sErr != nil || sStatus != http.StatusOK {
				log.Printf("[mux] asana: task %s created but failed to add to section %s: status=%d err=%v", created.Data.GID, section, sStatus, sErr)
			}
		}
	}

	return a.formatData(data)
}

func (a *Asana) handleUpdateTask(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	task, _ := req.RequireString("task")
	if task == "" {
		return mcp.NewToolResultError("task is required"), nil
	}

	args := req.GetArguments()
	taskData := map[string]any{}

	if v, ok := args["name"].(string); ok && v != "" {
		taskData["name"] = v
	}
	if v, ok := args["assignee"].(string); ok && v != "" {
		taskData["assignee"] = v
	}
	if v, ok := args["due_on"].(string); ok && v != "" {
		taskData["due_on"] = v
	}
	if v, ok := args["notes"].(string); ok && v != "" {
		taskData["notes"] = v
	}
	if v, ok := args["completed"].(bool); ok {
		taskData["completed"] = v
	}

	if len(taskData) == 0 {
		return mcp.NewToolResultError("no fields to update"), nil
	}

	payload := map[string]any{"data": taskData}
	data, status, err := a.doAsana(ctx, http.MethodPut, "/tasks/"+task, payload)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if status != http.StatusOK {
		return mcp.NewToolResultError(fmt.Sprintf("Asana API error (HTTP %d): %s", status, string(data))), nil
	}
	return a.formatData(data)
}

func (a *Asana) handleSearch(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	workspace, _ := req.RequireString("workspace")
	if workspace == "" {
		return mcp.NewToolResultError("workspace is required"), nil
	}

	args := req.GetArguments()
	q := url.Values{}
	q.Set("opt_fields", "name,assignee.name,due_on,completed,notes")

	if v, ok := args["text"].(string); ok && v != "" {
		q.Set("text", v)
	}
	if v, ok := args["assignee"].(string); ok && v != "" {
		q.Set("assignee.any", v)
	}
	if v, ok := args["project"].(string); ok && v != "" {
		q.Set("projects.any", v)
	}
	if v, ok := args["completed"].(bool); ok {
		if v {
			q.Set("completed", "true")
		} else {
			q.Set("completed", "false")
		}
	}
	if v, ok := args["due_on_before"].(string); ok && v != "" {
		q.Set("due_on.before", v)
	}
	if v, ok := args["due_on_after"].(string); ok && v != "" {
		q.Set("due_on.after", v)
	}

	path := fmt.Sprintf("/workspaces/%s/tasks/search?%s", workspace, q.Encode())
	data, status, err := a.doAsana(ctx, http.MethodGet, path, nil)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if status != http.StatusOK {
		return mcp.NewToolResultError(fmt.Sprintf("Asana API error (HTTP %d): %s", status, string(data))), nil
	}
	return a.formatData(data)
}

// --- HTTP helper ---

func (a *Asana) doAsana(ctx context.Context, method, path string, body any) ([]byte, int, error) {
	var reqBody io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, 0, fmt.Errorf("marshal request: %w", err)
		}
		reqBody = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, asanaBaseURL+path, reqBody)
	if err != nil {
		return nil, 0, fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+a.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := a.client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, asanaMaxBody))
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("read response: %w", err)
	}

	return data, resp.StatusCode, nil
}

// formatData extracts the "data" field from Asana API responses for cleaner output.
func (a *Asana) formatData(raw []byte) (*mcp.CallToolResult, error) {
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	if json.Unmarshal(raw, &envelope) == nil && len(envelope.Data) > 0 {
		var pretty any
		if json.Unmarshal(envelope.Data, &pretty) == nil {
			out, _ := json.MarshalIndent(pretty, "", "  ")
			return mcp.NewToolResultText(string(out)), nil
		}
	}
	return mcp.NewToolResultText(string(raw)), nil
}
