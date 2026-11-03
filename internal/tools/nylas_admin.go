package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
)

// Administration of the Nylas application — application mode only, where the
// key sees every grant: the grants themselves (get_grant, delete_grant), the
// provider configurations (list_connectors), the webhooks (list_webhooks) and
// the pass-through pair api_get/api_request for everything the shaped tools
// do not cover, confined to /v3/. Grants are created by OAuth or the
// Dashboard, not here.

// adminReadTools are the reading administration tools (application mode).
func (n *Nylas) adminReadTools() []ToolDef {
	return []ToolDef{
		{
			Tool: mcp.NewTool("get_grant",
				mcp.WithDescription("Read one grant (connected mailbox) of the application in full: address, aliases, provider, status, scope, settings, timestamps. Works on invalid grants too — the status says why a mailbox's tools fail."),
				mcp.WithString("mailbox", mcp.Required(), mcp.Description("The grant's address or id (list_mailboxes without mailbox lists them)")),
				mcp.WithReadOnlyHintAnnotation(true),
			),
			Handler: n.handleGetGrant,
		},
		{
			Tool: mcp.NewTool("list_connectors",
				mcp.WithDescription("List the application's connectors — one per provider (google, microsoft, imap, …): how Nylas authenticates mailboxes of that provider. A provider without a connector cannot be connected."),
				mcp.WithReadOnlyHintAnnotation(true),
			),
			Handler: n.handleListConnectors,
		},
		{
			Tool: mcp.NewTool("list_webhooks",
				mcp.WithDescription("List the application's webhook subscriptions as Nylas reports them (id, trigger types, URL, status, notification addresses). Secrets are never included."),
				mcp.WithReadOnlyHintAnnotation(true),
			),
			Handler: n.handleListWebhooks,
		},
		{
			Tool: mcp.NewTool("api_get",
				mcp.WithDescription("Raw GET against the Nylas API — any /v3/… path, including the query string (calendars, contacts, grants/{id}/…, applications, webhooks/{id}). The JSON comes back as is, under the key's rights. Paths must be normalized (no . or .. segments). The mail tools remain the way to read mail: they carry the ball position and the conversation shape."),
				mcp.WithString("path", mcp.Required(), mcp.Description("Path starting with /v3/, including the query string")),
				mcp.WithString("output_file", mcp.Description("Save the response instead of returning it inline: \"blob\" for a blob you fetch from mux, or an absolute path (supports ~) where you and mux share a disk")),
				mcp.WithReadOnlyHintAnnotation(true),
			),
			Handler: n.handleAPIGet,
		},
	}
}

// adminWriteTools are the administration tools that change something
// (application mode, not read-only).
func (n *Nylas) adminWriteTools() []ToolDef {
	return []ToolDef{
		{
			Tool: mcp.NewTool("delete_grant",
				mcp.WithDescription("Disconnect a mailbox from Nylas: delete its grant. Irreversible there — the mailbox's messages leave Nylas' cache, its ids stop working, and for IMAP grants the stored password leaves Nylas with it; reconnecting creates a new grant. The mail itself stays at the provider. Read with get_grant first."),
				mcp.WithString("mailbox", mcp.Required(), mcp.Description("The grant's address or id. Two valid grants with the same address need the id.")),
				mcp.WithDestructiveHintAnnotation(true),
			),
			Handler: n.handleDeleteGrant,
		},
		{
			Tool: mcp.NewTool("api_request",
				mcp.WithDescription("Raw POST/PUT/PATCH/DELETE against the Nylas API with a JSON body — any /v3/… path (calendars and events, contacts, webhooks, connectors, grant settings). Writes with the key's rights, which reach every grant of the application. Sending mail is refused here — the send tools are the way, with their outcome semantics and the allow_send gate. Read with api_get first."),
				mcp.WithString("method", mcp.Required(), mcp.Description("POST, PUT, PATCH or DELETE"), mcp.Enum("POST", "PUT", "PATCH", "DELETE")),
				mcp.WithString("path", mcp.Required(), mcp.Description("Path starting with /v3/, including the query string")),
				mcp.WithString("body", mcp.Description("JSON body")),
				mcp.WithDestructiveHintAnnotation(true),
			),
			Handler: n.handleAPIRequest,
		},
	}
}

func (n *Nylas) handleGetGrant(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	ref := strings.TrimSpace(req.GetString("mailbox", ""))
	if ref == "" {
		return mcp.NewToolResultError("mailbox is required: the grant's address or id"), nil
	}
	g, err := n.findGrant(ctx, ref)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	// The list carries the identity; the grant itself carries scope,
	// settings and aliases, read fresh.
	q := url.Values{}
	q.Set("expose_aliases", "true")
	var full nylasGrant
	if _, err := n.callJSON(ctx, "grant", map[string]string{"grantId": g.ID}, q, nil, &full); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("read grant %s: %v", g.ID, err)), nil
	}
	return jsonResult(nylasMailboxInfoOf(full, true))
}

func (n *Nylas) handleDeleteGrant(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	ref := strings.TrimSpace(req.GetString("mailbox", ""))
	if ref == "" {
		return mcp.NewToolResultError("mailbox is required: the grant's address or id — nothing is deleted by default"), nil
	}
	g, err := n.findGrant(ctx, ref)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if _, err := n.callJSON(ctx, "grant.delete", map[string]string{"grantId": g.ID}, nil, nil, nil); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("delete grant %s (%s): %v", g.ID, g.Email, err)), nil
	}
	n.forgetGrant(g.ID)
	result := map[string]any{
		"deleted":  true,
		"grant_id": g.ID,
		"email":    g.Email,
		"provider": g.Provider,
		"note":     "the mailbox is disconnected from Nylas; its message and conversation ids no longer resolve",
	}
	if nylasIMAPProviders[g.Provider] {
		result["note"] = "the mailbox is disconnected from Nylas and its stored password left with the grant; its message and conversation ids no longer resolve"
	}
	return jsonResult(result)
}

func (n *Nylas) handleListConnectors(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var all []nylasConnector
	for page := 0; page < 10; page++ {
		q := url.Values{}
		q.Set("limit", fmt.Sprint(nylasGrantPage))
		q.Set("offset", fmt.Sprint(page*nylasGrantPage))
		var connectors []nylasConnector
		if _, err := n.callJSON(ctx, "connectors", nil, q, nil, &connectors); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("list connectors: %v", err)), nil
		}
		all = append(all, connectors...)
		if len(connectors) < nylasGrantPage {
			break
		}
	}
	out := make([]map[string]any, 0, len(all))
	for _, c := range all {
		entry := map[string]any{"provider": c.Provider, "name": c.Name}
		if len(c.Scope) > 0 {
			entry["scope"] = c.Scope
		}
		if len(c.Settings) > 0 && string(c.Settings) != "null" {
			entry["settings"] = c.Settings
		}
		if c.ActiveCredentialID != "" {
			entry["activeCredentialId"] = c.ActiveCredentialID
		}
		out = append(out, entry)
	}
	return jsonResult(out)
}

// handleListWebhooks passes the webhook list through: the OpenAPI document
// declares no schema for it, so there is no field table to hold it against.
// A secret, should one ever appear in a listing, is dropped.
func (n *Nylas) handleListWebhooks(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var hooks []map[string]any
	if _, err := n.callJSON(ctx, "webhooks", nil, nil, nil, &hooks); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("list webhooks: %v", err)), nil
	}
	if hooks == nil {
		hooks = []map[string]any{}
	}
	for _, h := range hooks {
		delete(h, "webhook_secret")
	}
	return jsonResult(hooks)
}

// --- Pass-through ---

// nylasSendPath matches the two operations that send mail: POST
// /v3/grants/{id}/messages/send and POST /v3/grants/{id}/drafts/{id}. They
// are refused on api_request, whatever allow_send says: the send tools carry
// the outcome semantics (not sent / unclear / sent) and the idempotency key
// that make a send safe for an agent.
var nylasSendPath = regexp.MustCompile(`^/v3/grants/[^/]+/(messages/send|drafts/[^/]+)/?$`)

// nylasAPIPath confines a pass-through path to the versioned API: an
// absolute path under /v3/, normalized, with its query string. Encoded dots,
// slashes and backslashes are refused before any decoding could turn them
// into separators.
func (n *Nylas) nylasAPIPath(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if strings.Contains(raw, "://") {
		return "", fmt.Errorf("path %q must be an absolute path on the API (/v3/…), not a URL", raw)
	}
	if !strings.HasPrefix(raw, "/") {
		raw = "/" + raw
	}
	lower := strings.ToLower(raw)
	if strings.Contains(lower, "%2e") || strings.Contains(lower, "%2f") || strings.Contains(lower, "%5c") || strings.ContainsRune(raw, '\\') {
		return "", fmt.Errorf("path %q: encoded dots, slashes or backslashes are not accepted", raw)
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "" || u.Host != "" || u.Opaque != "" || u.Path == "" {
		return "", fmt.Errorf("path %q must be an absolute path on the API (/v3/…), not a URL", raw)
	}
	cleaned := path.Clean(u.Path)
	if cleaned != u.Path && cleaned+"/" != u.Path {
		return "", fmt.Errorf("path %q is not normalized (no . or .. segments, no double slashes)", raw)
	}
	if !strings.HasPrefix(cleaned, "/v3/") || cleaned == "/v3/" {
		return "", fmt.Errorf("path must start with /v3/ (the Nylas API v3), got %q", raw)
	}
	full := n.baseURL + cleaned
	if u.RawQuery != "" {
		full += "?" + u.RawQuery
	}
	return full, nil
}

// passThrough sends a raw request to the API with the connection's key.
func (n *Nylas) passThrough(ctx context.Context, method, fullURL string, body io.Reader) (*http.Response, error) {
	httpReq, err := http.NewRequestWithContext(ctx, method, fullURL, body)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+n.token)
	if body != nil {
		httpReq.Header.Set("Content-Type", "application/json")
	}
	resp, err := n.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("request failed: %v", err)
	}
	return resp, nil
}

func (n *Nylas) handleAPIGet(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	p, _ := req.RequireString("path")
	u, err := n.nylasAPIPath(p)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	outputFile := req.GetString("output_file", "")
	if outputFile != "" {
		if _, err := validateOutputFile(outputFile); err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
	}
	resp, err := n.passThrough(ctx, http.MethodGet, u, nil)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	defer resp.Body.Close()
	if outputFile != "" && resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return saveResponseToFile(resp, resp.Header.Get("Content-Type"), outputFile)
	}
	return rawHTTPResult(resp, nylasAPIInlineMax)
}

func (n *Nylas) handleAPIRequest(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	method := strings.ToUpper(strings.TrimSpace(req.GetString("method", "")))
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
	default:
		return mcp.NewToolResultError("method must be POST, PUT, PATCH or DELETE"), nil
	}
	p, _ := req.RequireString("path")
	u, err := n.nylasAPIPath(p)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if method == http.MethodPost && nylasSendPath.MatchString(strings.TrimPrefix(strings.SplitN(u, "?", 2)[0], n.baseURL)) {
		return mcp.NewToolResultError("sending mail is not done through api_request — use send_draft, reply_send or forward_send (they exist on connections with allow_send and report whether the message went out)"), nil
	}
	body, err := stringArg(req, "body")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	var rdr io.Reader
	if body != "" {
		if !json.Valid([]byte(body)) {
			return mcp.NewToolResultError("body must be valid JSON"), nil
		}
		rdr = strings.NewReader(body)
	}
	resp, err := n.passThrough(ctx, method, u, rdr)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	defer resp.Body.Close()
	return rawHTTPResult(resp, nylasAPIInlineMax)
}
