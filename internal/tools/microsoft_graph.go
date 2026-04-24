package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/smnhffmnn/mux/internal/config"
)

const (
	graphBaseURL = "https://graph.microsoft.com/v1.0"
	graphAuthURL = "https://login.microsoftonline.com/common/oauth2/v2.0"
	// GraphDefaultScopes is the default scope set requested when a
	// microsoft-graph connection has no explicit scopes configured. It covers
	// the Mail tools exposed by mux today. If a connection points at an Azure
	// app that grants additional permissions (Calendars, Files, …), the user
	// must override this via the Scopes connection field to request them.
	GraphDefaultScopes = "https://graph.microsoft.com/User.Read https://graph.microsoft.com/Mail.Read https://graph.microsoft.com/Mail.ReadWrite https://graph.microsoft.com/Mail.Send offline_access"
	graphMaxBody       = 8 * 1024 * 1024 // 8 MB — needed when body (full HTML) is in $select
	graphRefreshBuf    = 5 * time.Minute
)

// MicrosoftGraph wraps the Microsoft Graph REST API as MCP tools.
type MicrosoftGraph struct {
	client   *http.Client
	name     string
	clientID string
	scopes   string

	mu          sync.Mutex // guards accessToken + tokenExpiry
	refreshMu   sync.Mutex // serializes refresh attempts
	accessToken string
	tokenExpiry time.Time
}

// NewMicrosoftGraph creates a Microsoft Graph connection.
// The connection's ClientID (Azure App Registration ID) must be set — there
// is no longer a built-in default. Scopes fall back to GraphDefaultScopes
// (mail-only) if not configured; users with an app granting additional
// permissions should set Scopes explicitly.
func NewMicrosoftGraph(conn config.Connection, dialer Dialer) (*MicrosoftGraph, error) {
	if conn.ClientID == "" {
		return nil, fmt.Errorf("microsoft-graph connection %q: client_id (Azure App Registration ID) is required", conn.Name)
	}

	transport := &http.Transport{
		ResponseHeaderTimeout: 30 * time.Second,
	}
	if dialer != nil {
		transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, addr)
		}
	}

	scopes := conn.Scopes
	if scopes == "" {
		scopes = GraphDefaultScopes
	}

	mg := &MicrosoftGraph{
		client: &http.Client{
			Transport: transport,
			Timeout:   30 * time.Second,
		},
		name:     conn.Name,
		clientID: conn.ClientID,
		scopes:   scopes,
	}

	// Try to eagerly refresh token from keychain
	if rt, err := config.GetSecret(conn.Name + "-oauth-refresh-token"); err == nil && rt != "" {
		if err := mg.refreshAccessToken(context.Background(), rt); err != nil {
			log.Printf("[mux] microsoft-graph %q: eager token refresh failed: %v", conn.Name, err)
		}
	}

	return mg, nil
}

// Tools returns the MCP tools for the Microsoft Graph connection.
func (mg *MicrosoftGraph) Tools() []ToolDef {
	tools := []ToolDef{
		{
			Tool:    mcp.NewTool("auth_status", mcp.WithDescription("Check Microsoft Graph authentication status.")),
			Handler: mg.handleAuthStatus,
		},
		{
			Tool:    mcp.NewTool("auth_login", mcp.WithDescription("Start Microsoft OAuth device code flow. Returns a user_code to enter at the verification URL.")),
			Handler: mg.handleAuthLogin,
		},
		{
			Tool: mcp.NewTool("auth_poll",
				mcp.WithDescription("Poll for device code flow completion. Returns: pending, success, declined, or expired."),
				mcp.WithString("device_code", mcp.Required(), mcp.Description("Device code from auth_login")),
			),
			Handler: mg.handleAuthPoll,
		},
		{
			Tool: mcp.NewTool("list_conversations",
				mcp.WithDescription("List conversations (grouped by thread) in a mail folder. Defaults to Inbox."),
				mcp.WithNumber("limit", mcp.Description("Max conversations to return (default: 20)")),
				mcp.WithString("folder_id", mcp.Description("Mail folder ID to list (default: Inbox). Use list_mail_folders to discover folder IDs.")),
			),
			Handler: mg.handleListConversations,
		},
		{
			Tool: mcp.NewTool("search_messages",
				mcp.WithDescription("Search messages using KQL. Examples: subject:audit, from:john@example.com, subject:\"exact phrase\", hasAttachments:true. Results grouped by conversation, ordered by relevance (not date). By default searches all mail folders; pass folder_id to scope to one folder."),
				mcp.WithString("query", mcp.Required(), mcp.Description("KQL search query (do not wrap in quotes — pass raw KQL)")),
				mcp.WithNumber("limit", mcp.Description("Max conversations to return (default: 25)")),
				mcp.WithString("folder_id", mcp.Description("Optional mail folder ID to restrict search to. Default: all folders.")),
			),
			Handler: mg.handleSearchMessages,
		},
		{
			Tool: mcp.NewTool("list_mail_folders",
				mcp.WithDescription("List mail folders in the mailbox. Returns id, displayName, parentFolderId, totalItemCount, unreadItemCount. Use this to discover folder IDs for folder_id parameters on list_conversations and search_messages."),
				mcp.WithBoolean("include_children", mcp.Description("If true, also lists child folders recursively. Default: false (top-level only).")),
			),
			Handler: mg.handleListMailFolders,
		},
		{
			Tool: mcp.NewTool("get_conversation",
				mcp.WithDescription("Get all messages in a conversation."),
				mcp.WithString("conversation_id", mcp.Required(), mcp.Description("Conversation ID")),
			),
			Handler: mg.handleGetConversation,
		},
		{
			Tool: mcp.NewTool("archive_conversation",
				mcp.WithDescription("Move all inbox messages of a conversation to Archive."),
				mcp.WithString("conversation_id", mcp.Required(), mcp.Description("Conversation ID")),
			),
			Handler: mg.handleArchiveConversation,
		},
		{
			Tool: mcp.NewTool("delete_conversation",
				mcp.WithDescription("Delete all inbox messages of a conversation."),
				mcp.WithString("conversation_id", mcp.Required(), mcp.Description("Conversation ID")),
			),
			Handler: mg.handleDeleteConversation,
		},
		{
			Tool: mcp.NewTool("create_reply_draft",
				mcp.WithDescription("Create a reply draft for the latest message in a conversation. Does NOT send."),
				mcp.WithString("conversation_id", mcp.Required(), mcp.Description("Conversation ID")),
				mcp.WithString("body", mcp.Required(), mcp.Description("Reply body. Can be HTML or plain text (newlines are auto-converted to <br>).")),
			),
			Handler: mg.handleCreateReplyDraft,
		},
		{
			Tool: mcp.NewTool("create_draft",
				mcp.WithDescription("Create a new email draft from scratch. Does NOT send."),
				mcp.WithString("to", mcp.Required(), mcp.Description("Recipient email address(es), comma-separated for multiple")),
				mcp.WithString("subject", mcp.Required(), mcp.Description("Email subject")),
				mcp.WithString("body", mcp.Required(), mcp.Description("Email body. Can be HTML or plain text (newlines are auto-converted to <br>).")),
				mcp.WithString("cc", mcp.Description("CC recipient(s), comma-separated (optional)")),
				mcp.WithString("bcc", mcp.Description("BCC recipient(s), comma-separated (optional)")),
			),
			Handler: mg.handleCreateDraft,
		},
		{
			Tool: mcp.NewTool("create_forward_draft",
				mcp.WithDescription("Create a forward draft for the latest message in a conversation. Does NOT send."),
				mcp.WithString("conversation_id", mcp.Required(), mcp.Description("Conversation ID")),
				mcp.WithString("to", mcp.Required(), mcp.Description("Recipient email address(es), comma-separated for multiple")),
				mcp.WithString("body", mcp.Description("Optional comment. Can be HTML or plain text (newlines are auto-converted to <br>).")),
				mcp.WithString("cc", mcp.Description("CC recipient(s), comma-separated (optional)")),
				mcp.WithString("bcc", mcp.Description("BCC recipient(s), comma-separated (optional)")),
			),
			Handler: mg.handleCreateForwardDraft,
		},
		{
			Tool: mcp.NewTool("list_attachments",
				mcp.WithDescription("List attachments of a specific message. Returns name, size, content type, and ID for each attachment."),
				mcp.WithString("message_id", mcp.Required(), mcp.Description("Message ID")),
			),
			Handler: mg.handleListAttachments,
		},
		{
			Tool: mcp.NewTool("get_attachment",
				mcp.WithDescription("Get the content of a specific attachment. Returns base64-encoded content (contentBytes). Only works for file attachments (#microsoft.graph.fileAttachment), not item or reference attachments. Max ~380KB file size due to response limits."),
				mcp.WithString("message_id", mcp.Required(), mcp.Description("Message ID")),
				mcp.WithString("attachment_id", mcp.Required(), mcp.Description("Attachment ID from list_attachments")),
			),
			Handler: mg.handleGetAttachment,
		},
	}
	tools = append(tools, mg.sharePointTools()...)
	return tools
}

// --- Auth handlers ---

func (mg *MicrosoftGraph) handleAuthStatus(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	rt, err := config.GetSecret(mg.name + "-oauth-refresh-token")
	if err != nil || rt == "" {
		return jsonResult(map[string]any{"authenticated": false})
	}

	mg.mu.Lock()
	hasToken := mg.accessToken != "" && time.Now().Before(mg.tokenExpiry)
	mg.mu.Unlock()

	if !hasToken {
		if err := mg.refreshAccessToken(ctx, rt); err != nil {
			return jsonResult(map[string]any{"authenticated": false, "error": "refresh failed: " + err.Error()})
		}
	}

	// Verify by calling /me
	data, status, err := mg.doGraph(ctx, http.MethodGet, "/me?$select=displayName,mail", nil)
	if err != nil {
		return jsonResult(map[string]any{"authenticated": false, "error": "graph API unreachable: " + err.Error()})
	}
	if status != http.StatusOK {
		return jsonResult(map[string]any{"authenticated": false, "error": fmt.Sprintf("graph API returned HTTP %d: %s", status, string(data))})
	}

	var me struct {
		DisplayName string `json:"displayName"`
		Mail        string `json:"mail"`
	}
	if err := json.Unmarshal(data, &me); err != nil {
		return jsonResult(map[string]any{"authenticated": true, "error": "parse /me: " + err.Error()})
	}

	return jsonResult(map[string]any{
		"authenticated": true,
		"displayName":   me.DisplayName,
		"mail":          me.Mail,
	})
}

func (mg *MicrosoftGraph) handleAuthLogin(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	form := url.Values{
		"client_id": {mg.clientID},
		"scope":     {mg.scopes},
	}

	resp, err := mg.client.PostForm(graphAuthURL+"/devicecode", form)
	if err != nil {
		return mcp.NewToolResultError("device code request failed: " + err.Error()), nil
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, graphMaxBody))
	if resp.StatusCode != http.StatusOK {
		return mcp.NewToolResultError(fmt.Sprintf("device code error (HTTP %d): %s", resp.StatusCode, string(body))), nil
	}

	var result struct {
		DeviceCode      string `json:"device_code"`
		UserCode        string `json:"user_code"`
		VerificationURI string `json:"verification_uri"`
		Message         string `json:"message"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return mcp.NewToolResultError("parse device code response: " + err.Error()), nil
	}

	return jsonResult(map[string]any{
		"device_code":      result.DeviceCode,
		"user_code":        result.UserCode,
		"verification_uri": result.VerificationURI,
		"message":          result.Message,
	})
}

func (mg *MicrosoftGraph) handleAuthPoll(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	deviceCode, _ := req.RequireString("device_code")
	if deviceCode == "" {
		return mcp.NewToolResultError("device_code is required"), nil
	}

	form := url.Values{
		"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
		"client_id":   {mg.clientID},
		"device_code": {deviceCode},
	}

	resp, err := mg.client.PostForm(graphAuthURL+"/token", form)
	if err != nil {
		return mcp.NewToolResultError("token poll failed: " + err.Error()), nil
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, graphMaxBody))

	var tokenResp struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
		Error        string `json:"error"`
	}
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return mcp.NewToolResultError("parse token response: " + err.Error()), nil
	}

	switch tokenResp.Error {
	case "authorization_pending":
		return jsonResult(map[string]any{"status": "pending"})
	case "authorization_declined":
		return jsonResult(map[string]any{"status": "declined"})
	case "expired_token":
		return jsonResult(map[string]any{"status": "expired"})
	case "":
		// Success — store tokens
		if err := config.SaveSecret(mg.name+"-oauth-refresh-token", tokenResp.RefreshToken); err != nil {
			return mcp.NewToolResultError("failed to store refresh token: " + err.Error()), nil
		}

		mg.mu.Lock()
		mg.accessToken = tokenResp.AccessToken
		mg.tokenExpiry = time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second)
		mg.mu.Unlock()

		return jsonResult(map[string]any{"status": "success"})
	default:
		return mcp.NewToolResultError(fmt.Sprintf("auth error: %s — %s", tokenResp.Error, string(body))), nil
	}
}

// --- Mail handlers ---

func (mg *MicrosoftGraph) handleSearchMessages(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	query, _ := req.RequireString("query")
	if query == "" {
		return mcp.NewToolResultError("query is required"), nil
	}

	limit := 25
	if v, ok := req.GetArguments()["limit"].(float64); ok && v > 0 {
		limit = int(v)
	}

	fetchCount := limit * 3
	if fetchCount > 250 {
		fetchCount = 250
	}

	folderID := strings.TrimSpace(req.GetString("folder_id", ""))

	fields := "id,conversationId,subject,from,toRecipients,ccRecipients,bccRecipients,receivedDateTime,isRead,flag,bodyPreview"
	// $search uses KQL; $orderby is not supported with $search.
	// The query is passed as-is to allow raw KQL (subject:text, from:email, etc.).
	// Wrapping in outer quotes is required by the OData $search protocol.
	escaped := strings.ReplaceAll(query, `"`, `\"`)
	searchParam := url.QueryEscape(`"` + escaped + `"`)
	basePath := "/me/messages"
	if folderID != "" {
		basePath = "/me/mailFolders/" + url.PathEscape(folderID) + "/messages"
	}
	path := fmt.Sprintf("%s?$search=%s&$select=%s&$top=%d", basePath, searchParam, fields, fetchCount)

	data, status, err := mg.doGraph(ctx, http.MethodGet, path, nil)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if status != http.StatusOK {
		return mcp.NewToolResultError(fmt.Sprintf("Graph API error (HTTP %d): %s", status, string(data))), nil
	}

	var resp struct {
		Value []graphRawMessage `json:"value"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return mcp.NewToolResultError("parse messages: " + err.Error()), nil
	}

	conversations := groupByConversation(resp.Value, limit)
	return jsonResult(conversations)
}

func (mg *MicrosoftGraph) handleListConversations(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	limit := 20
	if v, ok := req.GetArguments()["limit"].(float64); ok && v > 0 {
		limit = int(v)
	}

	// Fetch more messages than requested to account for multi-message conversations
	fetchCount := limit * 3
	if fetchCount > 250 {
		fetchCount = 250
	}

	folderID := strings.TrimSpace(req.GetString("folder_id", ""))
	if folderID == "" {
		folderID = "inbox" // well-known folder name accepted by Graph
	}

	fields := "id,conversationId,subject,from,toRecipients,ccRecipients,bccRecipients,receivedDateTime,isRead,flag,bodyPreview"
	path := fmt.Sprintf("/me/mailFolders/%s/messages?$top=%d&$orderby=receivedDateTime+desc&$select=%s", url.PathEscape(folderID), fetchCount, fields)

	data, status, err := mg.doGraph(ctx, http.MethodGet, path, nil)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if status != http.StatusOK {
		return mcp.NewToolResultError(fmt.Sprintf("Graph API error (HTTP %d): %s", status, string(data))), nil
	}

	var resp struct {
		Value []graphRawMessage `json:"value"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return mcp.NewToolResultError("parse messages: " + err.Error()), nil
	}

	conversations := groupByConversation(resp.Value, limit)
	return jsonResult(conversations)
}

// mailFolderInfo is the minimal shape returned by list_mail_folders.
type mailFolderInfo struct {
	ID               string `json:"id"`
	DisplayName      string `json:"displayName"`
	ParentFolderID   string `json:"parentFolderId,omitempty"`
	TotalItemCount   int    `json:"totalItemCount"`
	UnreadItemCount  int    `json:"unreadItemCount"`
	ChildFolderCount int    `json:"childFolderCount,omitempty"`
}

// maxMailFolderDepth caps recursion in list_mail_folders to guard against
// pathologically deep folder trees. Real Outlook mailboxes rarely exceed 5
// levels; 10 gives plenty of headroom without risking runaway API calls.
const maxMailFolderDepth = 10

func (mg *MicrosoftGraph) handleListMailFolders(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	includeChildren := false
	if v, ok := req.GetArguments()["include_children"].(bool); ok {
		includeChildren = v
	}

	folders, err := mg.fetchMailFolders(ctx, "", includeChildren, 0)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return jsonResult(folders)
}

// fetchMailFolders retrieves folders under parentID (empty = root). If recurse
// is true, children of each folder are fetched too (depth-first, depth-capped
// at maxMailFolderDepth).
func (mg *MicrosoftGraph) fetchMailFolders(ctx context.Context, parentID string, recurse bool, depth int) ([]mailFolderInfo, error) {
	path := "/me/mailFolders?$top=100&$select=id,displayName,parentFolderId,totalItemCount,unreadItemCount,childFolderCount"
	if parentID != "" {
		path = "/me/mailFolders/" + url.PathEscape(parentID) + "/childFolders?$top=100&$select=id,displayName,parentFolderId,totalItemCount,unreadItemCount,childFolderCount"
	}

	data, status, err := mg.doGraph(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("Graph API error (HTTP %d): %s", status, string(data))
	}

	var resp struct {
		Value []mailFolderInfo `json:"value"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("parse mail folders: %w", err)
	}

	result := resp.Value
	if recurse && depth < maxMailFolderDepth {
		for i := range resp.Value {
			if resp.Value[i].ChildFolderCount == 0 {
				continue
			}
			children, err := mg.fetchMailFolders(ctx, resp.Value[i].ID, true, depth+1)
			if err != nil {
				return nil, err
			}
			result = append(result, children...)
		}
	}
	return result, nil
}

func (mg *MicrosoftGraph) handleGetConversation(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	convID, _ := req.RequireString("conversation_id")
	if convID == "" {
		return mcp.NewToolResultError("conversation_id is required"), nil
	}

	msgs, err := mg.getConversationMessages(ctx, convID, "id,subject,from,toRecipients,ccRecipients,bccRecipients,receivedDateTime,isRead,flag,bodyPreview,body,hasAttachments")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if len(msgs) == 0 {
		return mcp.NewToolResultError(fmt.Sprintf("no inbox messages found for conversation %s — it may be older than the search window", convID)), nil
	}

	return jsonResult(msgs)
}

func (mg *MicrosoftGraph) handleArchiveConversation(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	convID, _ := req.RequireString("conversation_id")
	if convID == "" {
		return mcp.NewToolResultError("conversation_id is required"), nil
	}

	archiveID, err := mg.getArchiveFolderID(ctx)
	if err != nil {
		return mcp.NewToolResultError("find archive folder: " + err.Error()), nil
	}

	msgs, err := mg.getConversationMessages(ctx, convID, "id")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if len(msgs) == 0 {
		return mcp.NewToolResultError(fmt.Sprintf("no inbox messages found for conversation %s — it may be older than the search window", convID)), nil
	}

	moved := 0
	for _, m := range msgs {
		payload := map[string]string{"destinationId": archiveID}
		_, status, err := mg.doGraph(ctx, http.MethodPost, "/me/messages/"+m.ID+"/move", payload)
		if err == nil && status == http.StatusCreated {
			moved++
		}
	}

	return jsonResult(map[string]any{"archived": moved, "total": len(msgs)})
}

func (mg *MicrosoftGraph) handleDeleteConversation(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	convID, _ := req.RequireString("conversation_id")
	if convID == "" {
		return mcp.NewToolResultError("conversation_id is required"), nil
	}

	msgs, err := mg.getConversationMessages(ctx, convID, "id")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if len(msgs) == 0 {
		return mcp.NewToolResultError(fmt.Sprintf("no inbox messages found for conversation %s — it may be older than the search window", convID)), nil
	}

	deleted := 0
	for _, m := range msgs {
		_, status, err := mg.doGraph(ctx, http.MethodDelete, "/me/messages/"+m.ID, nil)
		if err == nil && (status == http.StatusNoContent || status == http.StatusOK) {
			deleted++
		}
	}

	return jsonResult(map[string]any{"deleted": deleted, "total": len(msgs)})
}

func (mg *MicrosoftGraph) handleCreateReplyDraft(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	convID, _ := req.RequireString("conversation_id")
	body, _ := req.RequireString("body")
	if convID == "" || body == "" {
		return mcp.NewToolResultError("conversation_id and body are required"), nil
	}

	latest, err := mg.getLatestMessage(ctx, convID)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	payload := map[string]string{"comment": plainToHTML(body)}
	data, status, err := mg.doGraph(ctx, http.MethodPost, "/me/messages/"+latest.ID+"/createReply", payload)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if status != http.StatusCreated {
		return mcp.NewToolResultError(fmt.Sprintf("createReply failed (HTTP %d): %s", status, string(data))), nil
	}

	var draft struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(data, &draft); err != nil {
		return mcp.NewToolResultError("parse reply draft: " + err.Error()), nil
	}

	return jsonResult(map[string]any{"draft_id": draft.ID, "reply_to": latest.ID})
}

func (mg *MicrosoftGraph) handleCreateDraft(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	to, _ := req.RequireString("to")
	subject, _ := req.RequireString("subject")
	body, _ := req.RequireString("body")
	if to == "" || subject == "" || body == "" {
		return mcp.NewToolResultError("to, subject, and body are required"), nil
	}

	toRcpts := parseRecipients(to)
	if len(toRcpts) == 0 {
		return mcp.NewToolResultError("to must contain at least one valid email address"), nil
	}

	payload := map[string]any{
		"subject": subject,
		"body": map[string]string{
			"contentType": "HTML",
			"content":     plainToHTML(body),
		},
		"toRecipients": toRcpts,
	}
	if cc, ok := req.GetArguments()["cc"].(string); ok && cc != "" {
		payload["ccRecipients"] = parseRecipients(cc)
	}
	if bcc, ok := req.GetArguments()["bcc"].(string); ok && bcc != "" {
		payload["bccRecipients"] = parseRecipients(bcc)
	}

	data, status, err := mg.doGraph(ctx, http.MethodPost, "/me/messages", payload)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if status != http.StatusCreated {
		return mcp.NewToolResultError(fmt.Sprintf("create draft failed (HTTP %d): %s", status, string(data))), nil
	}

	var draft struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(data, &draft); err != nil {
		return mcp.NewToolResultError("parse draft: " + err.Error()), nil
	}

	return jsonResult(map[string]any{"draft_id": draft.ID, "to": to, "subject": subject})
}

func (mg *MicrosoftGraph) handleCreateForwardDraft(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	convID, _ := req.RequireString("conversation_id")
	to, _ := req.RequireString("to")
	if convID == "" || to == "" {
		return mcp.NewToolResultError("conversation_id and to are required"), nil
	}

	comment := ""
	if v, ok := req.GetArguments()["body"].(string); ok {
		comment = v
	}

	latest, err := mg.getLatestMessage(ctx, convID)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	// Create forward draft
	payload := map[string]string{"comment": plainToHTML(comment)}
	data, status, err := mg.doGraph(ctx, http.MethodPost, "/me/messages/"+latest.ID+"/createForward", payload)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if status != http.StatusCreated {
		return mcp.NewToolResultError(fmt.Sprintf("createForward failed (HTTP %d): %s", status, string(data))), nil
	}

	var draft struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(data, &draft); err != nil {
		return mcp.NewToolResultError("parse forward draft: " + err.Error()), nil
	}

	// Set recipients on draft
	toRcpts := parseRecipients(to)
	if len(toRcpts) == 0 {
		return mcp.NewToolResultError("to must contain at least one valid email address"), nil
	}
	rcptPayload := map[string]any{
		"toRecipients": toRcpts,
	}
	if cc, ok := req.GetArguments()["cc"].(string); ok && cc != "" {
		rcptPayload["ccRecipients"] = parseRecipients(cc)
	}
	if bcc, ok := req.GetArguments()["bcc"].(string); ok && bcc != "" {
		rcptPayload["bccRecipients"] = parseRecipients(bcc)
	}
	_, status, err = mg.doGraph(ctx, http.MethodPatch, "/me/messages/"+draft.ID, rcptPayload)
	if err != nil {
		return mcp.NewToolResultError("set recipients: " + err.Error()), nil
	}
	if status != http.StatusOK {
		return mcp.NewToolResultError(fmt.Sprintf("set recipients failed (HTTP %d)", status)), nil
	}

	return jsonResult(map[string]any{"draft_id": draft.ID, "forward_from": latest.ID, "to": to})
}

// --- HTTP helpers ---

// ensureAccessToken checks the cached token and refreshes if needed.
// Uses refreshMu to serialize concurrent refresh attempts.
func (mg *MicrosoftGraph) ensureAccessToken(ctx context.Context) error {
	mg.mu.Lock()
	valid := mg.accessToken != "" && time.Now().Add(graphRefreshBuf).Before(mg.tokenExpiry)
	mg.mu.Unlock()
	if valid {
		return nil
	}

	// Serialize refreshes — only one goroutine refreshes at a time
	mg.refreshMu.Lock()
	defer mg.refreshMu.Unlock()

	// Double-check after acquiring refreshMu (another goroutine may have refreshed)
	mg.mu.Lock()
	valid = mg.accessToken != "" && time.Now().Add(graphRefreshBuf).Before(mg.tokenExpiry)
	mg.mu.Unlock()
	if valid {
		return nil
	}

	rt, err := config.GetSecret(mg.name + "-oauth-refresh-token")
	if err != nil || rt == "" {
		return fmt.Errorf("not authenticated — use auth_login")
	}

	return mg.refreshAccessToken(ctx, rt)
}

// refreshAccessToken exchanges a refresh token for a new access token.
// Must NOT be called while mg.mu is held.
func (mg *MicrosoftGraph) refreshAccessToken(ctx context.Context, refreshToken string) error {
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {mg.clientID},
		"refresh_token": {refreshToken},
		"scope":         {mg.scopes},
	}

	resp, err := mg.client.PostForm(graphAuthURL+"/token", form)
	if err != nil {
		return fmt.Errorf("refresh request: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, graphMaxBody))

	var tokenResp struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
		Error        string `json:"error"`
	}
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return fmt.Errorf("parse token response: %w", err)
	}
	if tokenResp.Error != "" {
		return fmt.Errorf("token refresh error: %s", tokenResp.Error)
	}

	// Store rotated refresh token
	if tokenResp.RefreshToken != "" {
		if err := config.SaveSecret(mg.name+"-oauth-refresh-token", tokenResp.RefreshToken); err != nil {
			log.Printf("[mux] microsoft-graph %q: failed to persist refresh token: %v", mg.name, err)
		}
	}

	mg.mu.Lock()
	mg.accessToken = tokenResp.AccessToken
	mg.tokenExpiry = time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second)
	mg.mu.Unlock()

	return nil
}

func (mg *MicrosoftGraph) doGraph(ctx context.Context, method, path string, body any) ([]byte, int, error) {
	if err := mg.ensureAccessToken(ctx); err != nil {
		return nil, 0, err
	}

	var reqBody io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, 0, fmt.Errorf("marshal request: %w", err)
		}
		reqBody = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, graphBaseURL+path, reqBody)
	if err != nil {
		return nil, 0, fmt.Errorf("create request: %w", err)
	}

	mg.mu.Lock()
	token := mg.accessToken
	mg.mu.Unlock()

	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("ConsistencyLevel", "eventual") // Required for $search queries; harmless for other requests
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := mg.client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, graphMaxBody))
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("read response: %w", err)
	}

	return data, resp.StatusCode, nil
}

// --- Conversation helpers ---

func (mg *MicrosoftGraph) getConversationMessages(ctx context.Context, convID, fields string) ([]graphRawMessage, error) {
	// conversationId is not efficiently filterable in Graph API — $filter combined
	// with $orderby returns InefficientFilter error. Fetch inbox messages in pages
	// and filter client-side. Paginates up to maxPages (~1000 messages) to find
	// conversations that may be beyond the first page.
	selectFields := fields + ",conversationId"
	// Reduce page size when fetching full body to avoid hitting response size limits.
	pageSize := 250
	if strings.Contains(fields, "body") {
		pageSize = 25
	}
	path := fmt.Sprintf("/me/mailFolders/inbox/messages?$orderby=receivedDateTime+desc&$select=%s&$top=%d", selectFields, pageSize)

	const maxPages = 4
	var matched []graphRawMessage

	for page := 0; path != "" && page < maxPages; page++ {
		data, status, err := mg.doGraph(ctx, http.MethodGet, path, nil)
		if err != nil {
			return nil, err
		}
		if status != http.StatusOK {
			return nil, fmt.Errorf("Graph API error (HTTP %d): %s", status, string(data))
		}

		var resp struct {
			Value    []graphRawMessage `json:"value"`
			NextLink string            `json:"@odata.nextLink"`
		}
		if err := json.Unmarshal(data, &resp); err != nil {
			return nil, fmt.Errorf("parse messages: %w", err)
		}

		prevCount := len(matched)
		for _, m := range resp.Value {
			if m.ConversationID == convID {
				matched = append(matched, m)
			}
		}

		// If we had matches before this page but found none on it, the
		// conversation's messages are fully collected — stop early.
		if prevCount > 0 && len(matched) == prevCount {
			break
		}

		// Follow pagination
		if resp.NextLink != "" {
			after, ok := strings.CutPrefix(resp.NextLink, graphBaseURL)
			if !ok {
				break
			}
			path = after
		} else {
			path = ""
		}
	}

	// Reverse to chronological order (oldest first) — the API returns newest-first.
	for i, j := 0, len(matched)-1; i < j; i, j = i+1, j-1 {
		matched[i], matched[j] = matched[j], matched[i]
	}

	return matched, nil
}

func (mg *MicrosoftGraph) getLatestMessage(ctx context.Context, convID string) (*graphRawMessage, error) {
	// getConversationMessages returns chronological order (oldest first)
	msgs, err := mg.getConversationMessages(ctx, convID, "id")
	if err != nil {
		return nil, err
	}
	if len(msgs) == 0 {
		return nil, fmt.Errorf("no messages found for conversation %s", convID)
	}
	return &msgs[len(msgs)-1], nil
}

func (mg *MicrosoftGraph) getArchiveFolderID(ctx context.Context) (string, error) {
	// Use the well-known folder name (locale-independent)
	data, status, err := mg.doGraph(ctx, http.MethodGet, "/me/mailFolders/archive", nil)
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("archive folder not found (HTTP %d): %s", status, string(data))
	}

	var folder struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(data, &folder); err != nil {
		return "", fmt.Errorf("parse archive folder: %w", err)
	}
	if folder.ID == "" {
		return "", fmt.Errorf("archive folder ID is empty")
	}

	return folder.ID, nil
}

// --- Data models ---

type graphRawMessage struct {
	ID             string `json:"id"`
	ConversationID string `json:"conversationId"`
	Subject        string `json:"subject"`
	From           struct {
		EmailAddress struct {
			Name    string `json:"name"`
			Address string `json:"address"`
		} `json:"emailAddress"`
	} `json:"from"`
	ToRecipients []struct {
		EmailAddress struct {
			Address string `json:"address"`
		} `json:"emailAddress"`
	} `json:"toRecipients"`
	CcRecipients []struct {
		EmailAddress struct {
			Address string `json:"address"`
		} `json:"emailAddress"`
	} `json:"ccRecipients"`
	BccRecipients []struct {
		EmailAddress struct {
			Address string `json:"address"`
		} `json:"emailAddress"`
	} `json:"bccRecipients"`
	ReceivedDateTime string `json:"receivedDateTime"`
	IsRead           bool   `json:"isRead"`
	Flag             struct {
		FlagStatus string `json:"flagStatus"`
	} `json:"flag"`
	BodyPreview string `json:"bodyPreview"`
	Body        *struct {
		ContentType string `json:"contentType"`
		Content     string `json:"content"`
	} `json:"body,omitempty"`
	HasAttachments bool `json:"hasAttachments"`
}

type graphConversation struct {
	ConversationID string            `json:"conversationId"`
	Subject        string            `json:"subject"`
	MessageCount   int               `json:"messageCount"`
	MessageIDs     []string          `json:"messageIds"`
	Participants   []string          `json:"participants"`
	LatestMessage  graphMessageBrief `json:"latestMessage"`
	IsRead         bool              `json:"isRead"`
	IsFlagged      bool              `json:"isFlagged"`
}

type graphMessageBrief struct {
	ID           string   `json:"id"`
	Sender       string   `json:"sender"`
	SenderName   string   `json:"senderName"`
	To           []string `json:"to"`
	CC           []string `json:"cc"`
	Date         string   `json:"date"`
	BodyPreview  string   `json:"bodyPreview"`
}

func groupByConversation(messages []graphRawMessage, limit int) []graphConversation {
	type convData struct {
		messages []graphRawMessage
		order    int // order of first appearance
	}

	groups := make(map[string]*convData)
	orderCounter := 0

	for _, m := range messages {
		cid := m.ConversationID
		if cid == "" {
			cid = "no-conversation-" + m.ID
		}
		if _, ok := groups[cid]; !ok {
			groups[cid] = &convData{order: orderCounter}
			orderCounter++
		}
		groups[cid].messages = append(groups[cid].messages, m)
	}

	// Sort by first-appearance order (preserves newest-first from API)
	type sortEntry struct {
		cid   string
		order int
	}
	sorted := make([]sortEntry, 0, len(groups))
	for cid, g := range groups {
		sorted = append(sorted, sortEntry{cid, g.order})
	}
	// Sort by order (stable, preserves API order)
	for i := 1; i < len(sorted); i++ {
		for j := i; j > 0 && sorted[j].order < sorted[j-1].order; j-- {
			sorted[j], sorted[j-1] = sorted[j-1], sorted[j]
		}
	}

	var result []graphConversation
	for _, entry := range sorted {
		if len(result) >= limit {
			break
		}

		g := groups[entry.cid]
		msgs := g.messages

		// Latest message is first (messages are sorted newest-first from API)
		latest := msgs[0]

		// Collect unique participants
		participantSet := make(map[string]bool)
		allRead := true
		anyFlagged := false

		for _, m := range msgs {
			if m.From.EmailAddress.Address != "" {
				participantSet[m.From.EmailAddress.Address] = true
			}
			for _, r := range m.ToRecipients {
				participantSet[r.EmailAddress.Address] = true
			}
			if !m.IsRead {
				allRead = false
			}
			if m.Flag.FlagStatus == "flagged" {
				anyFlagged = true
			}
		}

		participants := make([]string, 0, len(participantSet))
		for p := range participantSet {
			participants = append(participants, p)
		}

		to := make([]string, 0, len(latest.ToRecipients))
		for _, r := range latest.ToRecipients {
			to = append(to, r.EmailAddress.Address)
		}
		cc := make([]string, 0, len(latest.CcRecipients))
		for _, r := range latest.CcRecipients {
			cc = append(cc, r.EmailAddress.Address)
		}

		msgIDs := make([]string, len(msgs))
		for i, m := range msgs {
			msgIDs[i] = m.ID
		}

		conv := graphConversation{
			ConversationID: entry.cid,
			Subject:        latest.Subject,
			MessageCount:   len(msgs),
			MessageIDs:     msgIDs,
			Participants:   participants,
			LatestMessage: graphMessageBrief{
				ID:          latest.ID,
				Sender:      latest.From.EmailAddress.Address,
				SenderName:  latest.From.EmailAddress.Name,
				To:          to,
				CC:          cc,
				Date:        latest.ReceivedDateTime,
				BodyPreview: latest.BodyPreview,
			},
			IsRead:    allRead,
			IsFlagged: anyFlagged,
		}
		result = append(result, conv)
	}

	return result
}

// --- Attachment handlers ---

func (mg *MicrosoftGraph) handleListAttachments(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	msgID, _ := req.RequireString("message_id")
	if msgID == "" {
		return mcp.NewToolResultError("message_id is required"), nil
	}

	data, status, err := mg.doGraph(ctx, http.MethodGet, "/me/messages/"+url.PathEscape(msgID)+"/attachments?$select=id,name,size,contentType,isInline", nil)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if status != http.StatusOK {
		return mcp.NewToolResultError(fmt.Sprintf("Graph API error (HTTP %d): %s", status, string(data))), nil
	}

	var resp struct {
		Value []struct {
			ODataType   string `json:"@odata.type"`
			ID          string `json:"id"`
			Name        string `json:"name"`
			Size        int    `json:"size"`
			ContentType string `json:"contentType"`
			IsInline    bool   `json:"isInline"`
		} `json:"value"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return mcp.NewToolResultError("parse attachments: " + err.Error()), nil
	}

	return jsonResult(resp.Value)
}

func (mg *MicrosoftGraph) handleGetAttachment(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	msgID, _ := req.RequireString("message_id")
	attID, _ := req.RequireString("attachment_id")
	if msgID == "" || attID == "" {
		return mcp.NewToolResultError("message_id and attachment_id are required"), nil
	}

	// No $select — contentBytes is on the derived fileAttachment type, not the base attachment type.
	data, status, err := mg.doGraph(ctx, http.MethodGet, "/me/messages/"+url.PathEscape(msgID)+"/attachments/"+url.PathEscape(attID), nil)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if status != http.StatusOK {
		return mcp.NewToolResultError(fmt.Sprintf("Graph API error (HTTP %d): %s", status, string(data))), nil
	}
	if len(data) >= graphMaxBody {
		return mcp.NewToolResultError("attachment too large to retrieve (response exceeded size limit)"), nil
	}

	var att struct {
		ODataType    string `json:"@odata.type"`
		ID           string `json:"id"`
		Name         string `json:"name"`
		Size         int    `json:"size"`
		ContentType  string `json:"contentType"`
		ContentBytes string `json:"contentBytes"`
	}
	if err := json.Unmarshal(data, &att); err != nil {
		return mcp.NewToolResultError("parse attachment: " + err.Error()), nil
	}

	return jsonResult(att)
}

// --- Recipient helpers ---

// parseRecipients splits a comma-separated list of email addresses into the
// Graph API recipient format. Whitespace around addresses is trimmed.
func parseRecipients(s string) []map[string]any {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	var out []map[string]any
	for _, p := range parts {
		addr := strings.TrimSpace(p)
		if addr != "" {
			out = append(out, map[string]any{
				"emailAddress": map[string]string{"address": addr},
			})
		}
	}
	return out
}

// --- HTML helpers ---

// reHTMLTag matches actual HTML tags (not bare < in plain text like "price < 10").
var reHTMLTag = regexp.MustCompile(`<[a-zA-Z/!]`)

// plainToHTML converts plain text to HTML if it doesn't already contain HTML tags.
// This ensures that newlines render correctly in Outlook drafts.
func plainToHTML(s string) string {
	if s == "" || reHTMLTag.MatchString(s) {
		return s
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	s = html.EscapeString(s)
	return strings.ReplaceAll(s, "\n", "<br>")
}

// --- Utility ---

func jsonResult(v any) (*mcp.CallToolResult, error) {
	data, _ := json.MarshalIndent(v, "", "  ")
	return mcp.NewToolResultText(string(data)), nil
}
