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
				mcp.WithDescription("Raw GET against the Nylas API — any /v3/… path, including the query string (grants/{id}/calendars, events and contacts, applications, webhooks/{id}, …). The JSON comes back as is, under the key's rights. Paths are written plain (no percent-encoding, no . or .. segments). The mail tools remain the way to read mail: they carry the ball position and the conversation shape."),
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
				mcp.WithDescription("Raw POST/PUT/PATCH/DELETE against the Nylas API with a JSON body — any /v3/… path (contacts, webhooks, connectors, grant settings, calendars and events). Writes with the key's rights, which reach every grant of the application. The message sends (grants/{id}/messages/send, POST grants/{id}/drafts/{id}, domains/{d}/messages/send) are refused here — the send tools are the way, with their outcome semantics; without allow_send the calendar operations that mail participants are refused too (events with notify_participants, send-rsvp). Read with api_get first."),
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
	g, err := n.findGrant(ctx, ref, false)
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
	// Irreversible, so the rules are stricter than for the mail tools: the
	// list is read fresh, and an address that more than one grant carries —
	// whatever their status — needs the id. "Valid wins" would delete the
	// working mailbox when an agent means the stale one.
	grants, err := n.listGrants(ctx, true)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	var g *nylasGrant
	var same []nylasGrant
	for i := range grants {
		if grants[i].ID == ref {
			g = &grants[i]
			break
		}
		if strings.EqualFold(strings.TrimSpace(grants[i].Email), ref) {
			same = append(same, grants[i])
		}
	}
	switch {
	case g != nil:
	case len(same) == 1:
		g = &same[0]
	case len(same) > 1:
		return mcp.NewToolResultError(fmt.Sprintf("%d grants carry the address %s — pass the grant id of the one to delete: %s", len(same), ref, nylasGrantSummary(same))), nil
	default:
		return mcp.NewToolResultError(fmt.Sprintf("no grant %q in this application — list_mailboxes shows the mailboxes (address, provider, status, id)", ref)), nil
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
		"status":   g.GrantStatus,
		"note":     "the mailbox is disconnected from Nylas; its message and conversation ids no longer resolve",
	}
	if nylasIMAPProviders[g.Provider] {
		result["note"] = "the mailbox is disconnected from Nylas and its stored password left with the grant; its message and conversation ids no longer resolve"
	}
	return jsonResult(result)
}

func (n *Nylas) handleListConnectors(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	// Paged like the grants: by what came back, until a page adds nothing
	// (one connector per provider, so the provider is the key).
	var all []nylasConnector
	seen := map[string]bool{}
	for page, offset := 0, 0; page < 10; page++ {
		q := url.Values{}
		q.Set("limit", fmt.Sprint(nylasGrantPage))
		q.Set("offset", fmt.Sprint(offset))
		var connectors []nylasConnector
		if _, err := n.callJSON(ctx, "connectors", nil, q, nil, &connectors); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("list connectors: %v", err)), nil
		}
		added := 0
		for _, c := range connectors {
			if !seen[c.Provider] {
				seen[c.Provider] = true
				all = append(all, c)
				added++
			}
		}
		if added == 0 {
			break
		}
		offset += len(connectors)
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
// describes it inline (id, description, trigger_types, webhook_url, status,
// notification_email_addresses, compressed_delivery), not as a component
// schema the field table could name, so nothing is decoded and nothing is
// drift-checked. The document shows webhook_secret only on create and
// rotate; it is dropped here in case a listing ever carries it.
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

// nylasMessageSendPath matches the operations that send a message: POST
// /v3/grants/{id}/messages/send, POST /v3/grants/{id}/drafts/{id} (send a
// draft) and POST /v3/domains/{domain}/messages/send (transactional mail with
// the application key). They are refused on api_request, whatever allow_send
// says: the send tools carry the outcome semantics (not sent / unclear /
// sent) and the idempotency key that make a send safe for an agent.
var nylasMessageSendPath = regexp.MustCompile(`^/v3/(grants/[^/]+/(messages/send|drafts/[^/]+)|domains/[^/]+/messages/send)$`)

// nylasEventPath and nylasRSVPPath match the calendar writes that make the
// provider mail participants: creating, updating or deleting an event
// (notify_participants defaults to true in the API) and sending an RSVP.
// Without allow_send they are refused — an event write passes only with
// notify_participants=false in the query.
var (
	nylasEventPath = regexp.MustCompile(`^/v3/grants/[^/]+/events(/[^/]+)?$`)
	nylasRSVPPath  = regexp.MustCompile(`^/v3/grants/[^/]+/events/[^/]+/send-rsvp$`)
	nylasGrantPath = regexp.MustCompile(`^/v3/grants/([^/]+)$`)
)

// nylasAPIPath confines a pass-through path to the versioned API: an
// absolute, plain path under /v3/ — normalized, without percent-encoding or
// a fragment — plus its query string. It returns the request URL and the
// path the gates are checked against. The path goes into the URL through
// url.URL, so every byte reaches the API as the segment it was written in;
// the gates see the same path, and no decoding on either side can turn a
// refused operation into an allowed one.
func (n *Nylas) nylasAPIPath(raw string) (fullURL, cleaned string, err error) {
	raw = strings.TrimSpace(raw)
	if strings.Contains(raw, "://") {
		return "", "", fmt.Errorf("path %q must be an absolute path on the API (/v3/…), not a URL", raw)
	}
	if !strings.HasPrefix(raw, "/") {
		raw = "/" + raw
	}
	pathPart, query, _ := strings.Cut(raw, "?")
	if strings.ContainsAny(pathPart, "%\\#") {
		return "", "", fmt.Errorf("path %q: write the path plain — no percent-encoding, backslashes or # (ids go in as they are; a query string may be encoded)", raw)
	}
	if strings.Contains(query, "#") {
		return "", "", fmt.Errorf("path %q: a fragment (#…) is not part of an API path", raw)
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "" || u.Host != "" || u.Opaque != "" || u.Path == "" || u.Fragment != "" {
		return "", "", fmt.Errorf("path %q must be an absolute path on the API (/v3/…), not a URL", raw)
	}
	cleaned = path.Clean(u.Path)
	if cleaned != u.Path && cleaned+"/" != u.Path {
		return "", "", fmt.Errorf("path %q is not normalized (no . or .. segments, no double slashes)", raw)
	}
	if !strings.HasPrefix(cleaned, "/v3/") || cleaned == "/v3/" {
		return "", "", fmt.Errorf("path must start with /v3/ (the Nylas API v3), got %q", raw)
	}
	base, err := url.Parse(n.baseURL)
	if err != nil {
		return "", "", fmt.Errorf("base URL %q: %w", n.baseURL, err)
	}
	full := &url.URL{Scheme: base.Scheme, Host: base.Host, Path: cleaned, RawQuery: u.RawQuery}
	return full.String(), cleaned, nil
}

// refuseAPIRequest applies the send gates to a pass-through write: the
// message sends are never passed through; the calendar writes that mail
// participants only with allow_send (or with notify_participants=false).
func (n *Nylas) refuseAPIRequest(method, cleaned, rawQuery string) error {
	if method == http.MethodPost && nylasMessageSendPath.MatchString(cleaned) {
		return fmt.Errorf("sending mail is not done through api_request — use send_draft, reply_send or forward_send (they exist on connections with allow_send and report whether the message went out)")
	}
	if n.allowSend {
		return nil
	}
	if method == http.MethodPost && nylasRSVPPath.MatchString(cleaned) {
		return fmt.Errorf("send-rsvp mails the organizer from the mailbox; this connection has no allow_send")
	}
	if nylasEventPath.MatchString(cleaned) {
		q, _ := url.ParseQuery(rawQuery)
		if q.Get("notify_participants") != "false" {
			return fmt.Errorf("writing an event mails its participants (notify_participants defaults to true); this connection has no allow_send — pass notify_participants=false in the query to write the event without mail")
		}
	}
	return nil
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
	u, _, err := n.nylasAPIPath(p)
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
	return rawHTTPResult(resp, nylasAPIInlineMax, "pass output_file to save the full response")
}

func (n *Nylas) handleAPIRequest(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	method := strings.ToUpper(strings.TrimSpace(req.GetString("method", "")))
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
	default:
		return mcp.NewToolResultError("method must be POST, PUT, PATCH or DELETE"), nil
	}
	p, _ := req.RequireString("path")
	u, cleaned, err := n.nylasAPIPath(p)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	_, rawQuery, _ := strings.Cut(u, "?")
	if err := n.refuseAPIRequest(method, cleaned, rawQuery); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
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
	if m := nylasGrantPath.FindStringSubmatch(cleaned); m != nil && resp.StatusCode >= 200 && resp.StatusCode < 300 && method != http.MethodPost {
		// A grant changed or deleted behind the shaped tools: the caches
		// must not serve it for another five minutes.
		n.forgetGrant(m[1])
	}
	return rawHTTPResult(resp, nylasAPIInlineMax, "")
}
