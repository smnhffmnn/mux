package tools

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/smnhffmnn/mux/internal/config"
)

const (
	gtmBaseURL   = "https://tagmanager.googleapis.com/tagmanager/v2"
	gtmTokenURL  = "https://oauth2.googleapis.com/token"
	gtmScope     = "https://www.googleapis.com/auth/tagmanager.edit.containers https://www.googleapis.com/auth/tagmanager.publish"
	gtmMaxBody   = 1024 * 1024 // 1 MB
)

// GoogleTagManager wraps the GTM API v2 as MCP tools.
type GoogleTagManager struct {
	client *http.Client
	sa     serviceAccount

	mu          sync.Mutex
	accessToken string
	tokenExpiry time.Time
}

type serviceAccount struct {
	ClientEmail string `json:"client_email"`
	PrivateKey  string `json:"private_key"`
	TokenURI    string `json:"token_uri"`
}

// NewGoogleTagManager creates a GTM connection from config.
// The service account JSON key must be stored in the keychain under {name}-token.
func NewGoogleTagManager(conn config.Connection, dialer Dialer) (*GoogleTagManager, error) {
	if conn.Token == "" {
		return nil, fmt.Errorf("google-tagmanager: service account JSON key is required (store in keychain as %s-token)", conn.Name)
	}

	var sa serviceAccount
	if err := json.Unmarshal([]byte(conn.Token), &sa); err != nil {
		return nil, fmt.Errorf("google-tagmanager: invalid service account JSON: %w", err)
	}
	if sa.ClientEmail == "" || sa.PrivateKey == "" {
		return nil, fmt.Errorf("google-tagmanager: service account JSON must contain client_email and private_key")
	}
	if sa.TokenURI == "" {
		sa.TokenURI = gtmTokenURL
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

	return &GoogleTagManager{
		client: &http.Client{
			Transport: transport,
			Timeout:   60 * time.Second,
		},
		sa: sa,
	}, nil
}

// Tools returns the MCP tools for GTM.
func (g *GoogleTagManager) Tools() []ToolDef {
	return []ToolDef{
		{
			Tool: mcp.NewTool("list_accounts",
				mcp.WithDescription("List all GTM accounts accessible by the service account."),
			),
			Handler: g.handleListAccounts,
		},
		{
			Tool: mcp.NewTool("list_containers",
				mcp.WithDescription("List containers in a GTM account."),
				mcp.WithString("account_id", mcp.Required(), mcp.Description("GTM account ID (e.g. '6088254685')")),
			),
			Handler: g.handleListContainers,
		},
		{
			Tool: mcp.NewTool("list_workspaces",
				mcp.WithDescription("List workspaces in a GTM container."),
				mcp.WithString("container_path", mcp.Required(), mcp.Description("Container path: accounts/{account_id}/containers/{container_id}")),
			),
			Handler: g.handleListWorkspaces,
		},
		{
			Tool: mcp.NewTool("list_tags",
				mcp.WithDescription("List all tags in a GTM workspace."),
				mcp.WithString("workspace_path", mcp.Required(), mcp.Description("Workspace path: accounts/{id}/containers/{id}/workspaces/{id}")),
			),
			Handler: g.handleListTags,
		},
		{
			Tool: mcp.NewTool("create_tag",
				mcp.WithDescription("Create a new tag in a GTM workspace. The tag_body must be a JSON object following the GTM API Tag resource format."),
				mcp.WithString("workspace_path", mcp.Required(), mcp.Description("Workspace path: accounts/{id}/containers/{id}/workspaces/{id}")),
				mcp.WithString("tag_body", mcp.Required(), mcp.Description("JSON object with tag definition (name, type, parameter[], firingTriggerId[], etc.)")),
			),
			Handler: g.handleCreateTag,
		},
		{
			Tool: mcp.NewTool("update_tag",
				mcp.WithDescription("Update an existing tag in a GTM workspace."),
				mcp.WithString("tag_path", mcp.Required(), mcp.Description("Tag path: accounts/{id}/containers/{id}/workspaces/{id}/tags/{tag_id}")),
				mcp.WithString("tag_body", mcp.Required(), mcp.Description("JSON object with updated tag definition")),
			),
			Handler: g.handleUpdateTag,
		},
		{
			Tool: mcp.NewTool("delete_tag",
				mcp.WithDescription("Delete a tag from a GTM workspace."),
				mcp.WithString("tag_path", mcp.Required(), mcp.Description("Tag path: accounts/{id}/containers/{id}/workspaces/{id}/tags/{tag_id}")),
			),
			Handler: g.handleDeleteTag,
		},
		{
			Tool: mcp.NewTool("list_triggers",
				mcp.WithDescription("List all triggers in a GTM workspace."),
				mcp.WithString("workspace_path", mcp.Required(), mcp.Description("Workspace path: accounts/{id}/containers/{id}/workspaces/{id}")),
			),
			Handler: g.handleListTriggers,
		},
		{
			Tool: mcp.NewTool("create_trigger",
				mcp.WithDescription("Create a new trigger in a GTM workspace."),
				mcp.WithString("workspace_path", mcp.Required(), mcp.Description("Workspace path: accounts/{id}/containers/{id}/workspaces/{id}")),
				mcp.WithString("trigger_body", mcp.Required(), mcp.Description("JSON object with trigger definition (name, type, customEventFilter[], etc.)")),
			),
			Handler: g.handleCreateTrigger,
		},
		{
			Tool: mcp.NewTool("list_variables",
				mcp.WithDescription("List all variables in a GTM workspace."),
				mcp.WithString("workspace_path", mcp.Required(), mcp.Description("Workspace path: accounts/{id}/containers/{id}/workspaces/{id}")),
			),
			Handler: g.handleListVariables,
		},
		{
			Tool: mcp.NewTool("create_variable",
				mcp.WithDescription("Create a new variable in a GTM workspace."),
				mcp.WithString("workspace_path", mcp.Required(), mcp.Description("Workspace path: accounts/{id}/containers/{id}/workspaces/{id}")),
				mcp.WithString("variable_body", mcp.Required(), mcp.Description("JSON object with variable definition (name, type, parameter[], etc.)")),
			),
			Handler: g.handleCreateVariable,
		},
		{
			Tool: mcp.NewTool("create_version",
				mcp.WithDescription("Create a new container version from the current workspace state."),
				mcp.WithString("workspace_path", mcp.Required(), mcp.Description("Workspace path: accounts/{id}/containers/{id}/workspaces/{id}")),
				mcp.WithString("name", mcp.Required(), mcp.Description("Version name")),
				mcp.WithString("notes", mcp.Description("Version description/notes")),
			),
			Handler: g.handleCreateVersion,
		},
		{
			Tool: mcp.NewTool("publish_version",
				mcp.WithDescription("Publish a container version to make it live."),
				mcp.WithString("version_path", mcp.Required(), mcp.Description("Version path: accounts/{id}/containers/{id}/versions/{version_id}")),
			),
			Handler: g.handlePublishVersion,
		},
	}
}

// --- Auth: Service Account JWT → Access Token ---

func (g *GoogleTagManager) getToken(ctx context.Context) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.accessToken != "" && time.Now().Before(g.tokenExpiry) {
		return g.accessToken, nil
	}

	now := time.Now()
	claims := map[string]any{
		"iss":   g.sa.ClientEmail,
		"scope": gtmScope,
		"aud":   g.sa.TokenURI,
		"iat":   now.Unix(),
		"exp":   now.Add(time.Hour).Unix(),
	}

	header := map[string]string{"alg": "RS256", "typ": "JWT"}
	hdrJSON, _ := json.Marshal(header)
	clmJSON, _ := json.Marshal(claims)

	unsigned := base64URLEncode(hdrJSON) + "." + base64URLEncode(clmJSON)

	block, _ := pem.Decode([]byte(g.sa.PrivateKey))
	if block == nil {
		return "", fmt.Errorf("failed to parse PEM block from private key")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return "", fmt.Errorf("parse private key: %w", err)
	}
	rsaKey, ok := key.(*rsa.PrivateKey)
	if !ok {
		return "", fmt.Errorf("private key is not RSA")
	}

	hash := sha256.Sum256([]byte(unsigned))
	sig, err := rsa.SignPKCS1v15(nil, rsaKey, crypto.SHA256, hash[:])
	if err != nil {
		return "", fmt.Errorf("sign JWT: %w", err)
	}

	jwt := unsigned + "." + base64URLEncode(sig)

	// Exchange JWT for access token
	form := url.Values{
		"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"},
		"assertion":  {jwt},
	}
	resp, err := g.client.PostForm(g.sa.TokenURI, form)
	if err != nil {
		return "", fmt.Errorf("token exchange request: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token exchange failed (HTTP %d): %s", resp.StatusCode, body)
	}

	var tokenResp struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return "", fmt.Errorf("parse token response: %w", err)
	}

	g.accessToken = tokenResp.AccessToken
	g.tokenExpiry = now.Add(time.Duration(tokenResp.ExpiresIn-60) * time.Second) // refresh 60s early
	return g.accessToken, nil
}

func base64URLEncode(data []byte) string {
	return strings.TrimRight(base64.URLEncoding.EncodeToString(data), "=")
}

// --- HTTP helpers ---

func (g *GoogleTagManager) doGTM(ctx context.Context, method, path string, body any) ([]byte, int, error) {
	token, err := g.getToken(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("auth: %w", err)
	}

	var reqBody io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, 0, fmt.Errorf("marshal request: %w", err)
		}
		reqBody = bytes.NewReader(b)
	}

	reqURL := gtmBaseURL + "/" + strings.TrimLeft(path, "/")
	req, err := http.NewRequestWithContext(ctx, method, reqURL, reqBody)
	if err != nil {
		return nil, 0, fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := g.client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, gtmMaxBody))
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("read response: %w", err)
	}
	return data, resp.StatusCode, nil
}

func gtmResult(data []byte, status int, err error) (*mcp.CallToolResult, error) {
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if status >= 400 {
		return mcp.NewToolResultError(fmt.Sprintf("GTM API error (HTTP %d): %s", status, data)), nil
	}
	// Pretty-print JSON
	var buf bytes.Buffer
	if json.Indent(&buf, data, "", "  ") == nil {
		return mcp.NewToolResultText(buf.String()), nil
	}
	return mcp.NewToolResultText(string(data)), nil
}

// --- Handlers ---

func (g *GoogleTagManager) handleListAccounts(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return gtmResult(g.doGTM(ctx, http.MethodGet, "accounts", nil))
}

func (g *GoogleTagManager) handleListContainers(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	accountID, _ := req.RequireString("account_id")
	return gtmResult(g.doGTM(ctx, http.MethodGet, fmt.Sprintf("accounts/%s/containers", accountID), nil))
}

func (g *GoogleTagManager) handleListWorkspaces(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	path, _ := req.RequireString("container_path")
	return gtmResult(g.doGTM(ctx, http.MethodGet, path+"/workspaces", nil))
}

func (g *GoogleTagManager) handleListTags(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	path, _ := req.RequireString("workspace_path")
	return gtmResult(g.doGTM(ctx, http.MethodGet, path+"/tags", nil))
}

func (g *GoogleTagManager) handleCreateTag(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	path, _ := req.RequireString("workspace_path")
	bodyStr, _ := req.RequireString("tag_body")
	var body any
	if err := json.Unmarshal([]byte(bodyStr), &body); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("invalid tag_body JSON: %v", err)), nil
	}
	return gtmResult(g.doGTM(ctx, http.MethodPost, path+"/tags", body))
}

func (g *GoogleTagManager) handleUpdateTag(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	path, _ := req.RequireString("tag_path")
	bodyStr, _ := req.RequireString("tag_body")
	var body any
	if err := json.Unmarshal([]byte(bodyStr), &body); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("invalid tag_body JSON: %v", err)), nil
	}
	return gtmResult(g.doGTM(ctx, http.MethodPut, path, body))
}

func (g *GoogleTagManager) handleDeleteTag(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	path, _ := req.RequireString("tag_path")
	return gtmResult(g.doGTM(ctx, http.MethodDelete, path, nil))
}

func (g *GoogleTagManager) handleListTriggers(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	path, _ := req.RequireString("workspace_path")
	return gtmResult(g.doGTM(ctx, http.MethodGet, path+"/triggers", nil))
}

func (g *GoogleTagManager) handleCreateTrigger(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	path, _ := req.RequireString("workspace_path")
	bodyStr, _ := req.RequireString("trigger_body")
	var body any
	if err := json.Unmarshal([]byte(bodyStr), &body); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("invalid trigger_body JSON: %v", err)), nil
	}
	return gtmResult(g.doGTM(ctx, http.MethodPost, path+"/triggers", body))
}

func (g *GoogleTagManager) handleListVariables(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	path, _ := req.RequireString("workspace_path")
	return gtmResult(g.doGTM(ctx, http.MethodGet, path+"/variables", nil))
}

func (g *GoogleTagManager) handleCreateVariable(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	path, _ := req.RequireString("workspace_path")
	bodyStr, _ := req.RequireString("variable_body")
	var body any
	if err := json.Unmarshal([]byte(bodyStr), &body); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("invalid variable_body JSON: %v", err)), nil
	}
	return gtmResult(g.doGTM(ctx, http.MethodPost, path+"/variables", body))
}

func (g *GoogleTagManager) handleCreateVersion(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	path, _ := req.RequireString("workspace_path")
	name, _ := req.RequireString("name")
	notes, _ := req.GetArguments()["notes"].(string)
	body := map[string]string{"name": name}
	if notes != "" {
		body["notes"] = notes
	}
	return gtmResult(g.doGTM(ctx, http.MethodPost, path+":create_version", body))
}

func (g *GoogleTagManager) handlePublishVersion(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	path, _ := req.RequireString("version_path")
	return gtmResult(g.doGTM(ctx, http.MethodPost, path+":publish", nil))
}
