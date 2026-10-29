package tools

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/smnhffmnn/mux/internal/config"
)

// The nylas connection type puts a mailbox behind the Nylas REST API v3 under
// mux' mail vocabulary: an agent sees the tools it knows from imap and
// microsoft-graph (list_conversations, get_conversation, create_reply_draft,
// send_draft …) whether the mailbox is Gmail, Microsoft 365 or plain IMAP.
//
// What the type adds over the other mail types is the ball position. A Nylas
// thread holds both sides of a conversation — the account's own replies
// included — and carries the dates of its latest incoming and outgoing
// message, so every conversation says whether the other side wrote last
// (ball: "me", a reply is due) or this account did (ball: "them").
//
// The contract is the versioned REST API, not Nylas' hosted MCP server: the
// endpoints, parameters and fields the type relies on are listed in
// nylasEndpoints and nylasSchemaFields below and checked against Nylas'
// published OpenAPI document by nylas_spec_test.go (vendored snapshot under
// testdata, live document in the drift workflow).

const (
	nylasThreadPage         = 50               // Nylas caps thread pages at 50
	nylasMessagePage        = 200              // and message pages at 200
	nylasMaxBody            = 16 << 20         // largest JSON response read
	nylasRequestTimeout     = 90 * time.Second // Nylas' own per-request limit; query_imap=true live reads can take most of it
	nylasInlineAttachment   = 1 << 20          // attachments up to this size are returned inline (base64)
	nylasFolderCacheTTL     = 5 * time.Minute  // folder ids are stable per grant; the cache saves a round trip per call
	nylasDefaultListLimit   = 20
	nylasDefaultSearchLimit = 25
	nylasGrantStatusValid   = "valid"
)

// DefaultNylasInstructions provides guidance for LLMs using nylas tools.
const DefaultNylasInstructions = `Mailbox behind the Nylas API — Gmail, Microsoft 365 and IMAP accounts look the same here.
Conversations are Nylas threads and include this account's own replies (Sent), so each conversation carries ball: "me" when the other side wrote last and a reply is due, "them" when this account wrote last and waits; lastFromMe is the underlying fact.
list_conversations reads one folder (default: the inbox), 20 per page in Nylas' order (newest first, not guaranteed on Gmail); pass its nextPageToken as page_token to continue; list_mailboxes shows folders with message and unread counts.
Each message has an id — use it in message_id of the reply, forward and attachment tools; a conversation id is the thread id. When latestMessage.isDraft is true, its id is a pending draft, not a message: take message ids from get_conversation.
search_messages returns conversations with ball; only with live=true (IMAP server search) it returns the matching messages grouped by thread, marked scope: "matches", without ball.
IMAP-backed accounts: Nylas keeps a rolling 90-day cache, and conversations and threads see only that window; search_messages with live=true asks the IMAP server directly (slow, one folder at a time) and reaches older mail.
archive/delete/move act on the conversation's messages in the inbox (or source_folder); delete moves to Trash. On Gmail, archive removes the INBOX label and move swaps labels; other labels stay.
Draft creation saves to the mailbox's Drafts folder and returns a draft_id — it does NOT send.`

// NylasSendInstructions are appended to DefaultNylasInstructions when the
// connection may send (allow_send).
const NylasSendInstructions = `Sending is enabled: send_draft sends a draft by its draft_id; reply_send and forward_send compose and send in one step.
Sent mail cannot be recalled. sent=true means Nylas accepted the message — never send it again, even when warnings follow.`

// Nylas wraps a mailbox behind the Nylas API as MCP tools.
type Nylas struct {
	name      string
	baseURL   string // https://api.{region}.nylas.com
	token     string
	fromName  string // display name in the From header of drafts and sent mail
	allowSend bool
	client    *http.Client

	// mu guards the lazily resolved grant (id, address, provider) and the
	// folder cache. Both come from the API on first use and change rarely.
	mu        sync.Mutex
	grantID   string
	email     string // the account's own address, lower-case
	provider  string // imap, google, microsoft, … as Nylas names them
	folders   []nylasFolder
	foldersAt time.Time

	// now is swappable for tests.
	now func() time.Time
}

// NewNylas creates a Nylas connection from config.
func NewNylas(conn config.Connection, dialer Dialer) (*Nylas, error) {
	if conn.Token == "" {
		return nil, fmt.Errorf("nylas: token (API key) is required")
	}
	if err := config.ValidateNylas(conn); err != nil {
		return nil, fmt.Errorf("nylas: %w", err)
	}
	transport := &http.Transport{}
	if dialer != nil {
		transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, addr)
		}
	}
	return &Nylas{
		name:      conn.Name,
		baseURL:   config.NylasBaseURL(conn.Region),
		token:     conn.Token,
		fromName:  strings.TrimSpace(conn.FromName),
		allowSend: conn.SendEnabled(),
		client:    &http.Client{Transport: transport, Timeout: nylasRequestTimeout},
		grantID:   strings.TrimSpace(conn.GrantID),
		email:     strings.ToLower(strings.TrimSpace(conn.User)),
		now:       time.Now,
	}, nil
}

// SendEnabled reports whether the connection registers its send tools.
func (n *Nylas) SendEnabled() bool {
	return n.allowSend
}

// --- Endpoint registry ---

// nylasEndpoint is one REST call the type makes. Path is the OpenAPI template
// (path-level and operation parameters count alike); Params are the query
// parameters the call may send. The spec test resolves every entry against
// Nylas' OpenAPI document, so a renamed path, a dropped parameter or a removed
// field fails a test instead of a user's call.
type nylasEndpoint struct {
	Method  string
	Path    string
	Params  []string
	Headers []string // request headers beyond auth and content type the call may send
}

// nylasEndpoints is the complete list of REST calls the type can make through
// call. Handlers address entries by key; call refuses anything that is not
// listed, so the spec test sees every path the type can send.
var nylasEndpoints = map[string]nylasEndpoint{
	"grants":              {Method: http.MethodGet, Path: "/v3/grants", Params: []string{"email", "limit"}},
	"grant":               {Method: http.MethodGet, Path: "/v3/grants/{grantId}"},
	"folders":             {Method: http.MethodGet, Path: "/v3/grants/{grant_id}/folders", Params: []string{"limit", "page_token"}},
	"folders.create":      {Method: http.MethodPost, Path: "/v3/grants/{grant_id}/folders"},
	"threads":             {Method: http.MethodGet, Path: "/v3/grants/{grant_id}/threads", Params: []string{"in", "limit", "page_token", "unread", "search_query_native"}},
	"messages":            {Method: http.MethodGet, Path: "/v3/grants/{grant_id}/messages", Params: []string{"thread_id", "in", "limit", "page_token", "search_query_native", "query_imap"}},
	"message":             {Method: http.MethodGet, Path: "/v3/grants/{grant_id}/messages/{message_id}", Params: []string{"fields"}},
	"message.update":      {Method: http.MethodPut, Path: "/v3/grants/{grant_id}/messages/{message_id}"},
	"message.delete":      {Method: http.MethodDelete, Path: "/v3/grants/{grant_id}/messages/{message_id}"},
	"attachment":          {Method: http.MethodGet, Path: "/v3/grants/{grant_id}/attachments/{attachment_id}", Params: []string{"message_id"}},
	"attachment.download": {Method: http.MethodGet, Path: "/v3/grants/{grant_id}/attachments/{attachment_id}/download", Params: []string{"message_id"}},
	"drafts.create":       {Method: http.MethodPost, Path: "/v3/grants/{grant_id}/drafts"},
	"draft.send":          {Method: http.MethodPost, Path: "/v3/grants/{grant_id}/drafts/{draft_id}"},
	"draft.delete":        {Method: http.MethodDelete, Path: "/v3/grants/{grant_id}/drafts/{draft_id}"},
	"messages.send":       {Method: http.MethodPost, Path: "/v3/grants/{grant_id}/messages/send", Headers: []string{"Idempotency-Key"}},
}

// nylasSchemaFields lists, per OpenAPI component schema, the properties the
// type decodes. It is the field table of the contract: the spec test checks
// every name against the schema, so a property Nylas removes or renames fails
// a test before it fails a user.
var nylasSchemaFields = map[string][]string{
	"thread":                {"id", "subject", "snippet", "participants", "message_ids", "draft_ids", "folders", "unread", "starred", "has_attachments", "has_drafts", "earliest_message_date", "latest_message_received_date", "latest_message_sent_date", "latest_draft_or_message"},
	"message":               {"id", "thread_id", "subject", "snippet", "body", "from", "to", "cc", "bcc", "reply_to", "date", "folders", "unread", "starred", "attachments", "in_reply_to", "grant_id"},
	"folder":                {"id", "name", "attributes", "total_count", "unread_count", "system_folder", "parent_id"},
	"attachment":            {"id", "filename", "content_type", "size", "is_inline", "content_id", "grant_id"},
	"draft":                 {"id", "thread_id", "subject", "to", "cc", "folders"},
	"message_send_response": {"id", "thread_id", "subject", "to", "cc", "date"},
}

// --- HTTP ---

// nylasEnvelope is the shape every Nylas response has: data (object or list),
// a cursor on lists, and an error object instead of data on failures.
type nylasEnvelope struct {
	RequestID  string          `json:"request_id"`
	Data       json.RawMessage `json:"data"`
	NextCursor string          `json:"next_cursor"`
	Error      *nylasAPIError  `json:"error"`
	Status     int             `json:"-"` // HTTP status of the answer, set by callJSON
}

// nylasDecodeError is a 2xx answer whose body mux could not read as the
// envelope. The request was carried out; only the answer is lost. The send
// tools report it as sent, so an agent does not send twice.
type nylasDecodeError struct {
	Status int
	Err    error
}

func (e *nylasDecodeError) Error() string {
	return fmt.Sprintf("decode Nylas response (HTTP %d): %v", e.Status, e.Err)
}
func (e *nylasDecodeError) Unwrap() error { return e.Err }

type nylasAPIError struct {
	Type          string `json:"type"`
	Code          string `json:"code"`
	Message       string `json:"message"`
	ProviderError any    `json:"provider_error"`
}

// nylasHTTPError is a non-2xx answer, with what Nylas said about it.
type nylasHTTPError struct {
	Status    int
	RequestID string
	API       *nylasAPIError
	Body      string
}

func (e *nylasHTTPError) Error() string {
	msg := fmt.Sprintf("Nylas API error (HTTP %d)", e.Status)
	if e.API != nil && e.API.Message != "" {
		if e.API.Type != "" {
			msg += ": " + e.API.Type
		}
		msg += ": " + e.API.Message
	} else if e.Body != "" {
		msg += ": " + e.Body
	}
	if e.RequestID != "" {
		msg += " (request " + e.RequestID + ")"
	}
	return msg
}

// nylasTransportError is a request that failed without an HTTP status: the
// server may or may not have acted on it. The send tools report it as
// "unclear whether sent" so an agent does not send twice.
type nylasTransportError struct{ Err error }

func (e *nylasTransportError) Error() string { return e.Err.Error() }
func (e *nylasTransportError) Unwrap() error { return e.Err }

// call performs a registered call. pathParams fill the {placeholders} of the
// endpoint's path template; query carries the parameters listed in Params.
// The registry is the only way to reach the API.
func (n *Nylas) call(ctx context.Context, key string, pathParams map[string]string, query url.Values, body any) (*http.Response, error) {
	return n.callHeaders(ctx, key, pathParams, query, nil, body)
}

// callHeaders is call with extra request headers; each must be listed in the
// endpoint's Headers.
func (n *Nylas) callHeaders(ctx context.Context, key string, pathParams map[string]string, query url.Values, headers map[string]string, body any) (*http.Response, error) {
	ep, ok := nylasEndpoints[key]
	if !ok {
		return nil, fmt.Errorf("nylas: endpoint %q is not registered", key)
	}
	path := ep.Path
	for k, v := range pathParams {
		// Ids come from the API, but they travel through an agent before they
		// come back: a value that would change the path's shape is refused.
		if v == "" || v == "." || v == ".." || strings.ContainsAny(v, "/?#") {
			return nil, fmt.Errorf("nylas: %s %q is not a valid id", k, v)
		}
		path = strings.ReplaceAll(path, "{"+k+"}", url.PathEscape(v))
	}
	if strings.Contains(path, "{") {
		return nil, fmt.Errorf("nylas: endpoint %q has an unfilled path parameter: %s", key, path)
	}
	allowed := map[string]bool{}
	for _, p := range ep.Params {
		allowed[p] = true
	}
	for k := range query {
		if !allowed[k] {
			return nil, fmt.Errorf("nylas: endpoint %q does not take query parameter %q", key, k)
		}
	}
	allowedHeaders := map[string]bool{}
	for _, h := range ep.Headers {
		allowedHeaders[strings.ToLower(h)] = true
	}
	for k := range headers {
		if !allowedHeaders[strings.ToLower(k)] {
			return nil, fmt.Errorf("nylas: endpoint %q does not take header %q", key, k)
		}
	}
	u := n.baseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}

	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal request: %w", err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, ep.Method, u, rdr)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+n.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := n.client.Do(req)
	if err != nil {
		return nil, &nylasTransportError{Err: fmt.Errorf("%s %s: %w", ep.Method, path, err)}
	}
	return resp, nil
}

// callJSON performs a registered call, requires a 2xx status and decodes the
// envelope's data into out (nil out discards it). It returns the envelope for
// the cursor.
func (n *Nylas) callJSON(ctx context.Context, key string, pathParams map[string]string, query url.Values, body, out any) (*nylasEnvelope, error) {
	return n.callJSONHeaders(ctx, key, pathParams, query, nil, body, out)
}

// callJSONHeaders is callJSON with extra request headers.
func (n *Nylas) callJSONHeaders(ctx context.Context, key string, pathParams map[string]string, query url.Values, headers map[string]string, body, out any) (*nylasEnvelope, error) {
	resp, err := n.callHeaders(ctx, key, pathParams, query, headers, body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, nylasMaxBody+1))
	if err != nil {
		return nil, &nylasTransportError{Err: fmt.Errorf("read response: %w", err)}
	}
	ok := resp.StatusCode >= 200 && resp.StatusCode < 300
	if len(data) > nylasMaxBody {
		if !ok {
			return nil, &nylasHTTPError{Status: resp.StatusCode, Body: trimForError(data)}
		}
		return nil, &nylasDecodeError{Status: resp.StatusCode, Err: fmt.Errorf("answer exceeds %d bytes", nylasMaxBody)}
	}
	env := nylasEnvelope{Status: resp.StatusCode}
	if len(bytes.TrimSpace(data)) > 0 {
		if err := json.Unmarshal(data, &env); err != nil {
			if !ok {
				return nil, &nylasHTTPError{Status: resp.StatusCode, Body: trimForError(data)}
			}
			return nil, &nylasDecodeError{Status: resp.StatusCode, Err: err}
		}
	}
	if !ok {
		return nil, &nylasHTTPError{Status: resp.StatusCode, RequestID: env.RequestID, API: env.Error, Body: trimForError(data)}
	}
	if out != nil && len(env.Data) > 0 {
		if err := json.Unmarshal(env.Data, out); err != nil {
			return nil, &nylasDecodeError{Status: resp.StatusCode, Err: fmt.Errorf("data: %w", err)}
		}
	}
	return &env, nil
}

// idempotencyKey derives the Idempotency-Key for a send from the grant and
// the payload: an identical retry within Nylas' one-hour window returns the
// first answer instead of sending again.
func idempotencyKey(grantID string, payload any) string {
	b, _ := json.Marshal(payload)
	sum := sha256.Sum256(append([]byte(grantID+"\n"), b...))
	return hex.EncodeToString(sum[:])
}

func trimForError(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 400 {
		s = s[:400] + "…"
	}
	return s
}

// --- Data models (what the API returns) ---

type nylasAddress struct {
	Name  string `json:"name,omitempty"`
	Email string `json:"email"`
}

type nylasGrant struct {
	ID          string `json:"id"`
	Email       string `json:"email"`
	Provider    string `json:"provider"`
	GrantStatus string `json:"grant_status"`
}

type nylasFolder struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Attributes   []string `json:"attributes"`
	TotalCount   int      `json:"total_count"`
	UnreadCount  int      `json:"unread_count"`
	SystemFolder bool     `json:"system_folder"`
	ParentID     string   `json:"parent_id,omitempty"`
}

type nylasAttachment struct {
	ID          string `json:"id"`
	GrantID     string `json:"grant_id,omitempty"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size"`
	IsInline    bool   `json:"is_inline"`
	ContentID   string `json:"content_id,omitempty"`
}

type nylasMessage struct {
	ID          string            `json:"id"`
	ThreadID    string            `json:"thread_id"`
	Subject     string            `json:"subject"`
	Snippet     string            `json:"snippet"`
	Body        string            `json:"body"`
	From        []nylasAddress    `json:"from"`
	To          []nylasAddress    `json:"to"`
	CC          []nylasAddress    `json:"cc"`
	BCC         []nylasAddress    `json:"bcc"`
	ReplyTo     []nylasAddress    `json:"reply_to"`
	Date        int64             `json:"date"`
	Folders     []string          `json:"folders"`
	Unread      bool              `json:"unread"`
	Starred     bool              `json:"starred"`
	Attachments []nylasAttachment `json:"attachments"`
	InReplyTo   string            `json:"in_reply_to"`
	Object      string            `json:"object"`
}

type nylasThread struct {
	ID                        string         `json:"id"`
	Subject                   string         `json:"subject"`
	Snippet                   string         `json:"snippet"`
	Participants              []nylasAddress `json:"participants"`
	MessageIDs                []string       `json:"message_ids"`
	DraftIDs                  []string       `json:"draft_ids"`
	Folders                   []string       `json:"folders"`
	Unread                    bool           `json:"unread"`
	Starred                   bool           `json:"starred"`
	HasAttachments            bool           `json:"has_attachments"`
	HasDrafts                 bool           `json:"has_drafts"`
	EarliestMessageDate       int64          `json:"earliest_message_date"`
	LatestMessageReceivedDate int64          `json:"latest_message_received_date"`
	LatestMessageSentDate     int64          `json:"latest_message_sent_date"`
	LatestDraftOrMessage      *nylasMessage  `json:"latest_draft_or_message"`
}

// --- Output models (what agents see; the shapes follow the imap type) ---

type nylasConversation struct {
	ConversationID string            `json:"conversationId"`
	Subject        string            `json:"subject"`
	MessageCount   int               `json:"messageCount"`
	MessageIDs     []string          `json:"messageIds"`
	Participants   []string          `json:"participants"`
	LatestMessage  nylasMessageBrief `json:"latestMessage"`
	IsRead         bool              `json:"isRead"`
	IsFlagged      bool              `json:"isFlagged"`
	HasAttachments bool              `json:"hasAttachments"`
	// Ball is "me" when the other side wrote last (a reply is due) and
	// "them" when this account wrote last; LastFromMe is the fact behind it.
	// Both are absent on conversations built from matching messages only
	// (Scope "matches"), where the whole thread is not known.
	Ball           string   `json:"ball,omitempty"`
	LastFromMe     bool     `json:"lastFromMe"`
	LatestReceived string   `json:"latestReceived,omitempty"`
	LatestSent     string   `json:"latestSent,omitempty"`
	Folders        []string `json:"folders"`
	DraftIDs       []string `json:"draftIds,omitempty"`
	// Scope is "matches" when messageIds, messageCount and latestMessage
	// cover only the messages a search matched, not the whole conversation.
	Scope string `json:"scope,omitempty"`
}

type nylasMessageBrief struct {
	ID         string   `json:"id"`
	Sender     string   `json:"sender"`
	SenderName string   `json:"senderName"`
	ReplyTo    []string `json:"replyTo,omitempty"` // only where it differs from the sender
	To         []string `json:"to"`
	CC         []string `json:"cc,omitempty"`
	Date       string   `json:"date"`
	Subject    string   `json:"subject"`
	Snippet    string   `json:"snippet"`
	FromMe     bool     `json:"fromMe"`
	// IsDraft marks a pending draft as the newest item of a thread: its id
	// is a draft id, not a message id.
	IsDraft bool `json:"isDraft,omitempty"`
}

type nylasMessageFull struct {
	ID          string                `json:"id"`
	Sender      string                `json:"sender"`
	SenderName  string                `json:"senderName"`
	ReplyTo     []string              `json:"replyTo,omitempty"`
	To          []string              `json:"to"`
	CC          []string              `json:"cc,omitempty"`
	Date        string                `json:"date"`
	Subject     string                `json:"subject"`
	Body        string                `json:"body"`
	IsRead      bool                  `json:"isRead"`
	IsFlagged   bool                  `json:"isFlagged"`
	FromMe      bool                  `json:"fromMe"`
	Folders     []string              `json:"folders"`
	Attachments []nylasAttachmentInfo `json:"attachments,omitempty"`
}

type nylasAttachmentInfo struct {
	ID          string `json:"id"`
	Filename    string `json:"filename"`
	ContentType string `json:"contentType"`
	Size        int64  `json:"size"`
	IsInline    bool   `json:"isInline"`
}

// --- Tools ---

// Tools returns the MCP tools for this Nylas connection. The send tools are
// registered only when the connection may send.
func (n *Nylas) Tools() []ToolDef {
	tools := []ToolDef{
		{
			Tool: mcp.NewTool("list_conversations",
				mcp.WithDescription("List conversations (Nylas threads) of a folder in Nylas' order (newest first; not guaranteed on Gmail), with this account's own replies included. Each conversation carries ball (\"me\": the other side wrote last, a reply is due; \"them\": this account wrote last) and lastFromMe. latestMessage.id is a message id unless latestMessage.isDraft is set. Returns {folder, conversations, nextPageToken}; pass nextPageToken as page_token to continue, an empty token is the last page."),
				mcp.WithNumber("limit", mcp.Description("Max conversations per page (default: 20, max: 50)")),
				mcp.WithString("page_token", mcp.Description("nextPageToken of the previous page")),
				mcp.WithString("folder", mcp.Description("Folder name or id (default: the inbox); see list_mailboxes")),
				mcp.WithBoolean("unread", mcp.Description("Only conversations with unread messages (default: all)")),
			),
			Handler: n.handleListConversations,
		},
		{
			Tool: mcp.NewTool("get_conversation",
				mcp.WithDescription("Get all messages of a conversation across folders (Sent included) with full body text, oldest first. Each message has the id the reply, forward and attachment tools take as message_id."),
				mcp.WithString("conversation_id", mcp.Required(), mcp.Description("Conversation ID (thread id)")),
			),
			Handler: n.handleGetConversation,
		},
		{
			Tool: mcp.NewTool("search_messages",
				mcp.WithDescription("Search conversations. The query is plain text: on IMAP-backed accounts mux wraps it in IMAP SEARCH (TEXT \"…\") unless native is set; Gmail and Microsoft accounts take their own search syntax as is. Results are conversations as in list_conversations, with ball. Only with live=true (IMAP server search beyond Nylas' cache) the result is the matching messages grouped by thread, marked scope: \"matches\" and without ball."),
				mcp.WithString("query", mcp.Required(), mcp.Description("Search text (or provider-native query with native=true)")),
				mcp.WithNumber("limit", mcp.Description("Max conversations to return (default: 25)")),
				mcp.WithString("folder", mcp.Description("Folder name or id to search in (default: the inbox when live is set, else all folders)")),
				mcp.WithBoolean("live", mcp.Description("IMAP-backed accounts: ask the IMAP server directly instead of Nylas' 90-day cache (slow, one folder at a time; default: false)")),
				mcp.WithBoolean("native", mcp.Description("Pass query to the provider verbatim (IMAP SEARCH, Gmail or Microsoft syntax) instead of plain-text matching (default: false)")),
			),
			Handler: n.handleSearchMessages,
		},
		{
			Tool: mcp.NewTool("list_mailboxes",
				mcp.WithDescription("List all folders (labels) with their message and unread counts."),
			),
			Handler: n.handleListMailboxes,
		},
		{
			Tool: mcp.NewTool("archive_conversation",
				mcp.WithDescription("Archive a conversation: move its inbox messages to the Archive folder (Gmail: remove the INBOX label; other providers without an Archive folder: move to Trash — the result names the folder)."),
				mcp.WithString("conversation_id", mcp.Required(), mcp.Description("Conversation ID")),
			),
			Handler: n.handleArchiveConversation,
		},
		{
			Tool: mcp.NewTool("delete_conversation",
				mcp.WithDescription("Delete a conversation: move its inbox messages to Trash (not permanent)."),
				mcp.WithString("conversation_id", mcp.Required(), mcp.Description("Conversation ID")),
			),
			Handler: n.handleDeleteConversation,
		},
		{
			Tool: mcp.NewTool("move_conversation",
				mcp.WithDescription("Move the messages of a conversation from one folder to another (default source: the inbox; on Gmail the source label is swapped for the target label, other labels stay). Use it to file mail or to bring it back from Trash."),
				mcp.WithString("conversation_id", mcp.Required(), mcp.Description("Conversation ID")),
				mcp.WithString("folder", mcp.Required(), mcp.Description("Target folder name or id")),
				mcp.WithString("source_folder", mcp.Description("Folder to move from, name or id (default: the inbox)")),
				mcp.WithBoolean("create", mcp.Description("Create the target folder when it does not exist (default: false)")),
			),
			Handler: n.handleMoveConversation,
		},
		{
			Tool: mcp.NewTool("list_attachments",
				mcp.WithDescription("List the attachments of a message: id, file name, content type, size, inline flag."),
				mcp.WithString("message_id", mcp.Required(), mcp.Description("Message ID (from get_conversation or latestMessage.id)")),
			),
			Handler: n.handleListAttachments,
		},
		{
			Tool: mcp.NewTool("get_attachment",
				mcp.WithDescription("Download an attachment. With output_file the content is written to that path (absolute, must not exist); without it, attachments up to 1 MiB are returned base64-encoded (contentBytes)."),
				mcp.WithString("message_id", mcp.Required(), mcp.Description("Message ID")),
				mcp.WithString("attachment_id", mcp.Required(), mcp.Description("Attachment ID from list_attachments or get_conversation")),
				mcp.WithString("output_file", mcp.Description("Absolute path to save the attachment to instead of returning it inline")),
			),
			Handler: n.handleGetAttachment,
		},
		{
			Tool: mcp.NewTool("create_reply_draft",
				mcp.WithDescription("Create a reply draft to a message of a conversation (default: the latest). Saved to Drafts, does NOT send. The reply goes to the message's Reply-To (its sender when it has none) and is threaded on it. Returns draft_id."),
				mcp.WithString("conversation_id", mcp.Required(), mcp.Description("Conversation ID")),
				mcp.WithString("body", mcp.Required(), mcp.Description("Reply text (plain text)")),
				mcp.WithBoolean("reply_all", mcp.Description("Also address everyone else on the message's To and Cc, except this account (default: false)")),
				mcp.WithString("message_id", mcp.Description(nylasMessageIDDescription+" (default: the latest message of the conversation)")),
			),
			Handler: n.handleCreateReplyDraft,
		},
		{
			Tool: mcp.NewTool("create_forward_draft",
				mcp.WithDescription("Create a forward draft of a message of a conversation (default: the latest). Saved to Drafts, does NOT send. The message's attachments (up to 3 MB in total) are downloaded and attached unless include_attachments is false. Returns draft_id."),
				mcp.WithString("conversation_id", mcp.Required(), mcp.Description("Conversation ID")),
				mcp.WithString("to", mcp.Required(), mcp.Description("Recipient email address(es), comma-separated for multiple")),
				mcp.WithString("body", mcp.Description("Optional comment to include above the forwarded message")),
				mcp.WithString("message_id", mcp.Description(nylasMessageIDDescription+" (default: the latest message of the conversation)")),
				mcp.WithBoolean("include_attachments", mcp.Description("Forward the message's attachments (default: true)")),
			),
			Handler: n.handleCreateForwardDraft,
		},
	}
	if n.allowSend {
		tools = append(tools, n.sendTools()...)
	}
	return tools
}

// nylasMessageIDDescription documents the message_id argument of the reply
// and forward tools.
const nylasMessageIDDescription = "ID of the message to act on, from list_conversations (latestMessage.id) or get_conversation"

// --- Grant ---

// nylasVerifyInfo is what Verify learned about the mailbox.
type nylasVerifyInfo struct {
	GrantID  string
	Email    string
	Provider string
	Folders  int
}

// Verify resolves the grant and lists the folders — the connection test's way
// to exercise exactly the auth and the two calls every tool starts with. A
// key for the wrong region, an expired key or a revoked grant shows up here
// and not on the first use.
func (n *Nylas) Verify(ctx context.Context) (nylasVerifyInfo, error) {
	if err := n.ensureGrant(ctx); err != nil {
		return nylasVerifyInfo{}, err
	}
	folders, err := n.listFolders(ctx, true)
	if err != nil {
		return nylasVerifyInfo{}, err
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	return nylasVerifyInfo{GrantID: n.grantID, Email: n.email, Provider: n.provider, Folders: len(folders)}, nil
}

// ensureGrant resolves the grant once: by id when configured, else by the
// mailbox address through the grants list. It also fills in the account's own
// address and provider, which the reply logic and the search syntax need. A
// grant Nylas reports as anything but valid is an error — its tools would
// fail one by one otherwise.
func (n *Nylas) ensureGrant(ctx context.Context) error {
	n.mu.Lock()
	grantID, email, provider := n.grantID, n.email, n.provider
	n.mu.Unlock()
	if grantID != "" && email != "" && provider != "" {
		return nil
	}

	var g nylasGrant
	if grantID == "" {
		q := url.Values{}
		q.Set("email", email)
		q.Set("limit", "50")
		var grants []nylasGrant
		if _, err := n.callJSON(ctx, "grants", nil, q, nil, &grants); err != nil {
			return fmt.Errorf("look up the grant of %s: %w", email, err)
		}
		for _, cand := range grants {
			if strings.EqualFold(cand.Email, email) && (g.ID == "" || cand.GrantStatus == nylasGrantStatusValid) {
				g = cand
			}
		}
		if g.ID == "" {
			return fmt.Errorf("no Nylas grant for %s in this application and region — connect the mailbox in the Nylas Dashboard, or set grant_id", email)
		}
	} else {
		if _, err := n.callJSON(ctx, "grant", map[string]string{"grantId": grantID}, nil, nil, &g); err != nil {
			return fmt.Errorf("read grant %s: %w", grantID, err)
		}
	}
	if g.GrantStatus != "" && g.GrantStatus != nylasGrantStatusValid {
		return fmt.Errorf("grant %s (%s) is %s — reconnect the mailbox in the Nylas Dashboard", g.ID, g.Email, g.GrantStatus)
	}

	n.mu.Lock()
	defer n.mu.Unlock()
	n.grantID = g.ID
	grantEmail := strings.ToLower(strings.TrimSpace(g.Email))
	if n.email == "" {
		n.email = grantEmail
	} else if grantEmail != "" && n.email != grantEmail {
		// The configured address wins (it may be an alias Nylas does not
		// report), but a mismatch is worth a line in the log: it drives
		// fromMe, the reply recipients and the From header.
		log.Printf("[nylas] %s: configured address %s differs from the grant's %s", n.name, n.email, grantEmail)
	}
	n.provider = g.Provider
	log.Printf("[nylas] %s: grant %s (%s)", n.name, g.ID, g.Provider)
	return nil
}

// multiLabel reports whether the provider keeps a message in several folders
// at once (Gmail labels). There, folders are a set to edit, not a location to
// replace.
func (n *Nylas) multiLabel() bool {
	_, _, provider := n.grant()
	return provider == "google"
}

// newFolders computes the folder list a message gets when it leaves remove
// and enters add. On single-folder providers that is just the target; on
// Gmail the other labels stay. An empty add on Gmail removes the label only
// (archiving: the message stays in All Mail).
func (n *Nylas) newFolders(m nylasMessage, remove, add string) []string {
	if !n.multiLabel() {
		if add == "" {
			return nil
		}
		return []string{add}
	}
	out := make([]string, 0, len(m.Folders)+1)
	seen := map[string]bool{}
	for _, f := range m.Folders {
		if f == remove || seen[f] {
			continue
		}
		seen[f] = true
		out = append(out, f)
	}
	if add != "" && !seen[add] {
		out = append(out, add)
	}
	return out
}

// grant returns the resolved grant id and the account's address.
func (n *Nylas) grant() (grantID, email, provider string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.grantID, n.email, n.provider
}

// isMe reports whether an address is this account's own.
func (n *Nylas) isMe(addr string) bool {
	_, me, _ := n.grant()
	return me != "" && strings.EqualFold(strings.TrimSpace(addr), me)
}

// --- Folders ---

// listFolders returns the grant's folders, from the cache unless force is set
// or the cache is older than nylasFolderCacheTTL.
func (n *Nylas) listFolders(ctx context.Context, force bool) ([]nylasFolder, error) {
	n.mu.Lock()
	if !force && n.folders != nil && n.now().Sub(n.foldersAt) < nylasFolderCacheTTL {
		folders := n.folders
		n.mu.Unlock()
		return folders, nil
	}
	n.mu.Unlock()

	grantID, _, _ := n.grant()
	var all []nylasFolder
	q := url.Values{}
	q.Set("limit", "200")
	for page := 0; page < 20; page++ {
		var folders []nylasFolder
		env, err := n.callJSON(ctx, "folders", map[string]string{"grant_id": grantID}, q, nil, &folders)
		if err != nil {
			return nil, fmt.Errorf("list folders: %w", err)
		}
		all = append(all, folders...)
		if env.NextCursor == "" || len(folders) == 0 {
			break
		}
		q.Set("page_token", env.NextCursor)
	}
	n.mu.Lock()
	n.folders = all
	n.foldersAt = n.now()
	n.mu.Unlock()
	return all, nil
}

// nylasFolderByAttribute finds the folder carrying a special-use attribute
// ("\Inbox", "\Trash", "\Sent", "\Drafts", "\Archive", "\Junk"), as Nylas
// reports them for every provider.
func nylasFolderByAttribute(folders []nylasFolder, attr string) *nylasFolder {
	for i := range folders {
		for _, a := range folders[i].Attributes {
			if strings.EqualFold(a, attr) {
				return &folders[i]
			}
		}
	}
	return nil
}

// nylasFolderByName finds a folder by name (case-insensitive), trying the
// given names in order, each also under the INBOX. prefix IMAP servers use.
func nylasFolderByName(folders []nylasFolder, names ...string) *nylasFolder {
	for _, name := range names {
		for _, cand := range []string{name, "INBOX." + name, "INBOX/" + name} {
			for i := range folders {
				if strings.EqualFold(folders[i].Name, cand) {
					return &folders[i]
				}
			}
		}
	}
	return nil
}

// resolveFolder finds a folder by id or name as an agent would refer to it.
func resolveFolder(folders []nylasFolder, ref string) *nylasFolder {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil
	}
	for i := range folders {
		if folders[i].ID == ref {
			return &folders[i]
		}
	}
	if strings.EqualFold(ref, "inbox") {
		if f := nylasFolderByAttribute(folders, `\Inbox`); f != nil {
			return f
		}
	}
	return nylasFolderByName(folders, ref)
}

// inboxFolder returns the inbox: by attribute, else by name.
func inboxFolder(folders []nylasFolder) *nylasFolder {
	if f := nylasFolderByAttribute(folders, `\Inbox`); f != nil {
		return f
	}
	return nylasFolderByName(folders, "INBOX", "Inbox")
}

// archiveFolder returns the Archive folder when the mailbox has one.
func archiveFolder(folders []nylasFolder) *nylasFolder {
	if f := nylasFolderByAttribute(folders, `\Archive`); f != nil {
		return f
	}
	return nylasFolderByName(folders, "Archive", "Archives", "Archiv")
}

// trashFolder returns the Trash folder.
func trashFolder(folders []nylasFolder) *nylasFolder {
	if f := nylasFolderByAttribute(folders, `\Trash`); f != nil {
		return f
	}
	return nylasFolderByName(folders, "Trash", "Deleted Messages", "Deleted Items", "Papierkorb", "[Gmail]/Trash")
}

// folderNames maps folder ids to names for output; an id without a known
// folder is passed through.
func folderNames(folders []nylasFolder, ids []string) []string {
	if len(ids) == 0 {
		return []string{}
	}
	byID := make(map[string]string, len(folders))
	for _, f := range folders {
		byID[f.ID] = f.Name
	}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if name, ok := byID[id]; ok {
			out = append(out, name)
		} else {
			out = append(out, id)
		}
	}
	return out
}

// --- Shaping ---

func nylasTime(epoch int64) string {
	if epoch <= 0 {
		return ""
	}
	return time.Unix(epoch, 0).UTC().Format(time.RFC3339)
}

func nylasAddresses(list []nylasAddress) []string {
	out := make([]string, 0, len(list))
	for _, a := range list {
		if e := strings.TrimSpace(a.Email); e != "" {
			out = append(out, e)
		}
	}
	return out
}

// nylasDistinctReplyTo is a message's Reply-To where it differs from its
// sender — Nylas reports the sender there when the header is absent.
func nylasDistinctReplyTo(m nylasMessage) []string {
	rt := nylasAddresses(m.ReplyTo)
	if len(rt) == 0 {
		return nil
	}
	from := ""
	if len(m.From) > 0 {
		from = m.From[0].Email
	}
	if len(rt) == 1 && strings.EqualFold(rt[0], from) {
		return nil
	}
	return rt
}

func (n *Nylas) messageBrief(m *nylasMessage) nylasMessageBrief {
	if m == nil {
		return nylasMessageBrief{}
	}
	b := nylasMessageBrief{
		ID:      m.ID,
		ReplyTo: nylasDistinctReplyTo(*m),
		To:      nylasAddresses(m.To),
		CC:      nylasAddresses(m.CC),
		Date:    nylasTime(m.Date),
		Subject: m.Subject,
		Snippet: m.Snippet,
	}
	if len(m.From) > 0 {
		b.Sender = m.From[0].Email
		b.SenderName = m.From[0].Name
		b.FromMe = n.isMe(m.From[0].Email)
	}
	if b.To == nil {
		b.To = []string{}
	}
	return b
}

func (n *Nylas) messageFull(m nylasMessage, folders []nylasFolder) nylasMessageFull {
	f := nylasMessageFull{
		ID:        m.ID,
		ReplyTo:   nylasDistinctReplyTo(m),
		To:        nylasAddresses(m.To),
		CC:        nylasAddresses(m.CC),
		Date:      nylasTime(m.Date),
		Subject:   m.Subject,
		Body:      nylasBodyText(m.Body),
		IsRead:    !m.Unread,
		IsFlagged: m.Starred,
		Folders:   folderNames(folders, m.Folders),
	}
	if len(m.From) > 0 {
		f.Sender = m.From[0].Email
		f.SenderName = m.From[0].Name
		f.FromMe = n.isMe(m.From[0].Email)
	}
	if f.To == nil {
		f.To = []string{}
	}
	for _, a := range m.Attachments {
		f.Attachments = append(f.Attachments, nylasAttachmentInfo{ID: a.ID, Filename: a.Filename, ContentType: a.ContentType, Size: a.Size, IsInline: a.IsInline})
	}
	return f
}

// conversationFromThread shapes a Nylas thread. The ball position comes from
// the thread's own dates: Nylas tracks the latest incoming and the latest
// outgoing message separately. When both fall on the same second, the sender
// of the latest message decides.
func (n *Nylas) conversationFromThread(t nylasThread, folders []nylasFolder) nylasConversation {
	latestIsDraft := t.LatestDraftOrMessage != nil && t.LatestDraftOrMessage.Object == "draft"
	lastFromMe := false
	switch {
	case t.LatestMessageSentDate > 0 && t.LatestMessageReceivedDate > 0:
		if t.LatestMessageSentDate != t.LatestMessageReceivedDate {
			lastFromMe = t.LatestMessageSentDate > t.LatestMessageReceivedDate
		} else if !latestIsDraft && t.LatestDraftOrMessage != nil && len(t.LatestDraftOrMessage.From) > 0 {
			lastFromMe = n.isMe(t.LatestDraftOrMessage.From[0].Email)
		}
	case t.LatestMessageSentDate > 0:
		lastFromMe = true
	}
	ball := "me"
	if lastFromMe {
		ball = "them"
	}

	pSet := map[string]bool{}
	for _, p := range t.Participants {
		if e := strings.TrimSpace(p.Email); e != "" {
			pSet[e] = true
		}
	}
	participants := make([]string, 0, len(pSet))
	for p := range pSet {
		participants = append(participants, p)
	}
	sort.Strings(participants)

	subject := t.Subject
	latest := n.messageBrief(t.LatestDraftOrMessage)
	latest.IsDraft = latestIsDraft
	if latest.Subject == "" {
		latest.Subject = subject
	}
	if latest.Snippet == "" {
		latest.Snippet = t.Snippet
	}
	ids := t.MessageIDs
	if ids == nil {
		ids = []string{}
	}
	return nylasConversation{
		ConversationID: t.ID,
		Subject:        subject,
		MessageCount:   len(t.MessageIDs),
		MessageIDs:     ids,
		Participants:   participants,
		LatestMessage:  latest,
		IsRead:         !t.Unread,
		IsFlagged:      t.Starred,
		HasAttachments: t.HasAttachments,
		Ball:           ball,
		LastFromMe:     lastFromMe,
		LatestReceived: nylasTime(t.LatestMessageReceivedDate),
		LatestSent:     nylasTime(t.LatestMessageSentDate),
		Folders:        folderNames(folders, t.Folders),
		DraftIDs:       t.DraftIDs,
	}
}

// conversationsFromMessages groups messages by thread — the shape a live
// search takes, where Nylas returns the matching messages, not threads. The
// groups are marked scope "matches" and carry no ball: without the whole
// thread, who wrote last is unknown.
func (n *Nylas) conversationsFromMessages(msgs []nylasMessage, folders []nylasFolder, limit int) []nylasConversation {
	groups := map[string][]nylasMessage{}
	var order []string
	for _, m := range msgs {
		key := m.ThreadID
		if key == "" {
			key = m.ID
		}
		if _, seen := groups[key]; !seen {
			order = append(order, key)
		}
		groups[key] = append(groups[key], m)
	}
	type entry struct {
		id     string
		latest int64
	}
	entries := make([]entry, 0, len(order))
	for _, id := range order {
		group := groups[id]
		sort.Slice(group, func(i, j int) bool { return group[i].Date < group[j].Date })
		groups[id] = group
		entries = append(entries, entry{id, group[len(group)-1].Date})
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].latest > entries[j].latest })

	result := []nylasConversation{}
	for _, e := range entries {
		if len(result) >= limit {
			break
		}
		group := groups[e.id]
		latest := group[len(group)-1]
		pSet := map[string]bool{}
		allRead, anyFlagged, anyAttach := true, false, false
		ids := make([]string, 0, len(group))
		var folderIDs []string
		seenFolder := map[string]bool{}
		for _, m := range group {
			ids = append(ids, m.ID)
			for _, a := range append(append([]nylasAddress{}, m.From...), m.To...) {
				if e := strings.TrimSpace(a.Email); e != "" {
					pSet[e] = true
				}
			}
			if m.Unread {
				allRead = false
			}
			if m.Starred {
				anyFlagged = true
			}
			if len(m.Attachments) > 0 {
				anyAttach = true
			}
			for _, f := range m.Folders {
				if !seenFolder[f] {
					seenFolder[f] = true
					folderIDs = append(folderIDs, f)
				}
			}
		}
		participants := make([]string, 0, len(pSet))
		for p := range pSet {
			participants = append(participants, p)
		}
		sort.Strings(participants)
		result = append(result, nylasConversation{
			ConversationID: e.id,
			Subject:        group[0].Subject,
			MessageCount:   len(group),
			MessageIDs:     ids,
			Participants:   participants,
			LatestMessage:  n.messageBrief(&latest),
			IsRead:         allRead,
			IsFlagged:      anyFlagged,
			HasAttachments: anyAttach,
			Folders:        folderNames(folders, folderIDs),
			Scope:          "matches",
		})
	}
	return result
}

// --- Body text ---

var (
	// reHTMLStructural decides whether a body is HTML: a known tag followed by
	// whitespace, a slash or the closing bracket. A bare "<word" is not enough
	// — plain-text mail carries "<https://…>" and "Bob <bob@example.net>".
	reHTMLStructural = regexp.MustCompile(`(?i)<(!doctype|html|head|body|div|p|br|table|tr|td|th|span|a|font|img|ul|ol|li|h[1-6]|blockquote|pre|style|meta|center)(\s|/?>)`)
	reHTMLLink       = regexp.MustCompile(`(?is)<a\b[^>]*?href\s*=\s*["']([^"']+)["'][^>]*>(.*?)</a\s*>`)
	reHTMLComment    = regexp.MustCompile(`(?s)<!--.*?-->`)
	reHTMLDropElem   = regexp.MustCompile(`(?is)<(script|style|head|title)\b[^>]*>.*?</(script|style|head|title)\s*>`)
	reHTMLParaEnd    = regexp.MustCompile(`(?i)<\s*/?(p|div|h[1-6]|blockquote|pre|table|ul|ol|section|article|header|footer)\b[^>]*>`)
	reHTMLLineEnd    = regexp.MustCompile(`(?i)<\s*(br|hr|/li|/tr)\b[^>]*>`)
	reHTMLAnyTag     = regexp.MustCompile(`<[^>]*>`)
	reBlankLines     = regexp.MustCompile(`\n[ \t]*\n[ \t\n]*`)
	reLineSpace      = regexp.MustCompile(`[ \t\x{00a0}]+`)
)

// nylasBodyText turns a message body into plain text: Nylas hands most bodies
// over as HTML (the provider's rendering), plain-text mail as is.
func nylasBodyText(body string) string {
	s := body
	if reHTMLStructural.MatchString(s) {
		s = htmlToText(s)
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.TrimSpace(s)
	if len(s) > imapTextMaxSize {
		s = s[:imapTextMaxSize]
	}
	return s
}

// htmlToText is a reading conversion, not a renderer: scripts, styles and
// the head go, block elements (paragraphs, divs, lists, tables) are set off
// by blank lines, line breaks and list or table rows become newlines, the
// remaining tags are stripped and entities decoded.
// Good enough for an agent to read a mail; layout tables and images are not
// reconstructed.
func htmlToText(s string) string {
	s = reHTMLComment.ReplaceAllString(s, "")
	s = reHTMLDropElem.ReplaceAllString(s, "")
	// Links keep their target: "text (url)", or just the url when the text
	// is the url already.
	s = reHTMLLink.ReplaceAllStringFunc(s, func(a string) string {
		m := reHTMLLink.FindStringSubmatch(a)
		if len(m) < 3 {
			return a
		}
		href := html.UnescapeString(strings.TrimSpace(m[1]))
		text := strings.TrimSpace(html.UnescapeString(reHTMLAnyTag.ReplaceAllString(m[2], "")))
		if text == "" || text == href || strings.HasPrefix(href, "mailto:") && strings.EqualFold(strings.TrimPrefix(href, "mailto:"), text) {
			return href
		}
		return text + " (" + href + ")"
	})
	s = reHTMLParaEnd.ReplaceAllString(s, "\n\n")
	s = reHTMLLineEnd.ReplaceAllString(s, "\n")
	s = reHTMLAnyTag.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	s = strings.ReplaceAll(s, "\r\n", "\n")
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimSpace(reLineSpace.ReplaceAllString(l, " "))
	}
	s = strings.Join(lines, "\n")
	s = reBlankLines.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}

// --- Thread messages ---

// threadMessages returns the messages of a thread, oldest first, optionally
// confined to one folder.
func (n *Nylas) threadMessages(ctx context.Context, threadID, folderID string) ([]nylasMessage, error) {
	grantID, _, _ := n.grant()
	q := url.Values{}
	q.Set("thread_id", threadID)
	q.Set("limit", fmt.Sprint(nylasMessagePage))
	if folderID != "" {
		q.Set("in", folderID)
	}
	var all []nylasMessage
	for page := 0; page < 10; page++ {
		var msgs []nylasMessage
		env, err := n.callJSON(ctx, "messages", map[string]string{"grant_id": grantID}, q, nil, &msgs)
		if err != nil {
			return nil, fmt.Errorf("read conversation %s: %w", threadID, err)
		}
		all = append(all, msgs...)
		if env.NextCursor == "" || len(msgs) == 0 {
			break
		}
		q.Set("page_token", env.NextCursor)
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].Date < all[j].Date })
	return all, nil
}

// limitArg reads a positive whole-number argument, falling back to def and
// capped at max (0: no cap). Fractions and out-of-range numbers fall back
// rather than being truncated to something else.
func limitArg(req mcp.CallToolRequest, key string, def, max int) int {
	v, ok := req.GetArguments()[key].(float64)
	if !ok || v != math.Trunc(v) || v < 1 || v > math.MaxInt32 {
		return def
	}
	n := int(v)
	if max > 0 && n > max {
		return max
	}
	return n
}

// pickNylasMessage returns the index of the message with the given id in a
// conversation, the latest when messageID is empty.
func pickNylasMessage(msgs []nylasMessage, convID, messageID string) (int, error) {
	if len(msgs) == 0 {
		return 0, fmt.Errorf("conversation %s has no messages", convID)
	}
	if messageID == "" {
		return len(msgs) - 1, nil
	}
	for i, m := range msgs {
		if m.ID == messageID {
			return i, nil
		}
	}
	return 0, fmt.Errorf("message %s is not part of conversation %s", messageID, convID)
}

// --- Handlers ---

func (n *Nylas) handleListConversations(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	limit := limitArg(req, "limit", nylasDefaultListLimit, nylasThreadPage)
	if err := n.ensureGrant(ctx); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	folders, err := n.listFolders(ctx, false)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	var folder *nylasFolder
	if ref := req.GetString("folder", ""); ref != "" {
		if folder = resolveFolder(folders, ref); folder == nil {
			return mcp.NewToolResultError(fmt.Sprintf("folder %q not found — list_mailboxes shows the folders", ref)), nil
		}
	} else if folder = inboxFolder(folders); folder == nil {
		return mcp.NewToolResultError("no inbox folder found — list_mailboxes shows the folders"), nil
	}

	grantID, _, _ := n.grant()
	q := url.Values{}
	q.Set("in", folder.ID)
	q.Set("limit", fmt.Sprint(limit))
	if tok := req.GetString("page_token", ""); tok != "" {
		q.Set("page_token", tok)
	}
	if _, ok := req.GetArguments()["unread"]; ok && req.GetBool("unread", false) {
		q.Set("unread", "true")
	}
	var threads []nylasThread
	env, err := n.callJSON(ctx, "threads", map[string]string{"grant_id": grantID}, q, nil, &threads)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	conversations := make([]nylasConversation, 0, len(threads))
	for _, t := range threads {
		conversations = append(conversations, n.conversationFromThread(t, folders))
	}
	return jsonResult(map[string]any{
		"folder":        folder.Name,
		"conversations": conversations,
		"nextPageToken": env.NextCursor,
	})
}

func (n *Nylas) handleGetConversation(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	convID, _ := req.RequireString("conversation_id")
	if convID == "" {
		return mcp.NewToolResultError("conversation_id is required"), nil
	}
	if err := n.ensureGrant(ctx); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	folders, err := n.listFolders(ctx, false)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	msgs, err := n.threadMessages(ctx, convID, "")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if len(msgs) == 0 {
		return mcp.NewToolResultError(fmt.Sprintf("conversation %s has no messages — wrong id (a message id is not a conversation id), or outside Nylas' 90-day window for IMAP accounts", convID)), nil
	}
	out := make([]nylasMessageFull, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, n.messageFull(m, folders))
	}
	return jsonResult(out)
}

// nylasIMAPProviders are the providers Nylas syncs over IMAP; their native
// search syntax is IMAP SEARCH.
var nylasIMAPProviders = map[string]bool{"imap": true, "icloud": true, "yahoo": true}

// imapSearchText wraps plain text as an IMAP SEARCH criterion on the
// message text. Quoted strings escape backslash and double quote and cannot
// carry line breaks or NUL.
func imapSearchText(q string) (string, error) {
	if strings.ContainsAny(q, "\r\n\x00") {
		return "", fmt.Errorf("query must not contain line breaks")
	}
	q = strings.ReplaceAll(q, `\`, `\\`)
	q = strings.ReplaceAll(q, `"`, `\"`)
	return `TEXT "` + q + `"`, nil
}

func (n *Nylas) handleSearchMessages(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	query, _ := req.RequireString("query")
	if strings.TrimSpace(query) == "" {
		return mcp.NewToolResultError("query is required"), nil
	}
	limit := limitArg(req, "limit", nylasDefaultSearchLimit, 0)
	live := req.GetBool("live", false)
	native := req.GetBool("native", false)
	if err := n.ensureGrant(ctx); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	folders, err := n.listFolders(ctx, false)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	grantID, _, provider := n.grant()

	q := url.Values{}
	if nylasIMAPProviders[provider] && !native {
		wrapped, err := imapSearchText(query)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		q.Set("search_query_native", wrapped)
	} else {
		q.Set("search_query_native", query)
	}
	if ref := req.GetString("folder", ""); ref != "" || live {
		var folder *nylasFolder
		if ref != "" {
			folder = resolveFolder(folders, ref)
			if folder == nil {
				return mcp.NewToolResultError(fmt.Sprintf("folder %q not found — list_mailboxes shows the folders", ref)), nil
			}
		} else if folder = inboxFolder(folders); folder == nil {
			return mcp.NewToolResultError("no inbox folder found — live search needs a folder"), nil
		}
		q.Set("in", folder.ID)
	}

	if !live {
		// Threads carry both sides of each conversation, so the result has
		// the same shape and the same ball position as list_conversations.
		page := limit
		if page > nylasThreadPage {
			page = nylasThreadPage
		}
		q.Set("limit", fmt.Sprint(page))
		var threads []nylasThread
		if _, err := n.callJSON(ctx, "threads", map[string]string{"grant_id": grantID}, q, nil, &threads); err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		conversations := make([]nylasConversation, 0, len(threads))
		for _, t := range threads {
			if len(conversations) >= limit {
				break
			}
			conversations = append(conversations, n.conversationFromThread(t, folders))
		}
		return jsonResult(conversations)
	}

	// Live: the IMAP server searches, Nylas returns the matching messages
	// (threads have no query_imap). Grouped by thread, marked as matches.
	if !nylasIMAPProviders[provider] {
		return mcp.NewToolResultError(fmt.Sprintf("live applies to IMAP-backed accounts; this grant's provider is %s", provider)), nil
	}
	q.Set("query_imap", "true")
	q.Set("limit", fmt.Sprint(nylasMessagePage))
	var msgs []nylasMessage
	if _, err := n.callJSON(ctx, "messages", map[string]string{"grant_id": grantID}, q, nil, &msgs); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return jsonResult(n.conversationsFromMessages(msgs, folders, limit))
}

func (n *Nylas) handleListMailboxes(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if err := n.ensureGrant(ctx); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	folders, err := n.listFolders(ctx, true)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	result := make([]map[string]any, 0, len(folders))
	for _, f := range folders {
		attrs := f.Attributes
		if attrs == nil {
			attrs = []string{}
		}
		entry := map[string]any{
			"id":         f.ID,
			"name":       f.Name,
			"attributes": attrs,
			"messages":   f.TotalCount,
			"unseen":     f.UnreadCount,
		}
		if f.SystemFolder {
			entry["systemFolder"] = true
		}
		if f.ParentID != "" {
			entry["parentId"] = f.ParentID
		}
		result = append(result, entry)
	}
	return jsonResult(result)
}

// moveMessages takes every message out of the source folder and into the
// target (see newFolders) and returns how many moved; the first failure stops
// the loop and is returned with the count so far, because a half-moved
// conversation must be reported as such.
func (n *Nylas) moveMessages(ctx context.Context, msgs []nylasMessage, sourceFolderID, targetFolderID string) (int, error) {
	grantID, _, _ := n.grant()
	moved := 0
	for _, m := range msgs {
		folders := n.newFolders(m, sourceFolderID, targetFolderID)
		if folders == nil {
			return moved, fmt.Errorf("move message %s: no target folder", m.ID)
		}
		body := map[string]any{"folders": folders}
		if _, err := n.callJSON(ctx, "message.update", map[string]string{"grant_id": grantID, "message_id": m.ID}, nil, body, nil); err != nil {
			return moved, fmt.Errorf("move message %s: %w", m.ID, err)
		}
		moved++
	}
	return moved, nil
}

func (n *Nylas) handleArchiveConversation(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	convID, _ := req.RequireString("conversation_id")
	if convID == "" {
		return mcp.NewToolResultError("conversation_id is required"), nil
	}
	if err := n.ensureGrant(ctx); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	folders, err := n.listFolders(ctx, false)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	inbox := inboxFolder(folders)
	if inbox == nil {
		return mcp.NewToolResultError("no inbox folder found"), nil
	}
	// Where to: the Archive folder when there is one. Gmail has none — there
	// archiving means removing the INBOX label, the message stays in All
	// Mail. Other providers without an Archive folder fall back to Trash,
	// like the imap type, and the result names the folder.
	target := archiveFolder(folders)
	targetID, targetName := "", ""
	switch {
	case target != nil:
		targetID, targetName = target.ID, target.Name
	case n.multiLabel():
		targetName = "" // label removed, nothing added
	default:
		trash := trashFolder(folders)
		if trash == nil {
			return mcp.NewToolResultError("no Archive or Trash folder found"), nil
		}
		targetID, targetName = trash.ID, trash.Name
	}
	msgs, err := n.threadMessages(ctx, convID, inbox.ID)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if len(msgs) == 0 {
		return mcp.NewToolResultError(fmt.Sprintf("no inbox messages found for conversation %s (wrong id, or already archived)", convID)), nil
	}
	moved, err := n.moveMessages(ctx, msgs, inbox.ID, targetID)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("%v (%d of %d archived)", err, moved, len(msgs))), nil
	}
	result := map[string]any{"archived": moved, "folder": targetName, "removedFrom": inbox.Name}
	if targetID == "" {
		result["note"] = "label removed; the messages stay in All Mail"
	}
	return jsonResult(result)
}

func (n *Nylas) handleDeleteConversation(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	convID, _ := req.RequireString("conversation_id")
	if convID == "" {
		return mcp.NewToolResultError("conversation_id is required"), nil
	}
	if err := n.ensureGrant(ctx); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	folders, err := n.listFolders(ctx, false)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	inbox := inboxFolder(folders)
	if inbox == nil {
		return mcp.NewToolResultError("no inbox folder found"), nil
	}
	msgs, err := n.threadMessages(ctx, convID, inbox.ID)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if len(msgs) == 0 {
		return mcp.NewToolResultError(fmt.Sprintf("no inbox messages found for conversation %s (wrong id, or already removed)", convID)), nil
	}
	grantID, _, _ := n.grant()
	deleted := 0
	for _, m := range msgs {
		// No hard_delete: Nylas moves the message to the provider's Trash.
		if _, err := n.callJSON(ctx, "message.delete", map[string]string{"grant_id": grantID, "message_id": m.ID}, nil, nil, nil); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("delete message %s: %v (%d of %d deleted)", m.ID, err, deleted, len(msgs))), nil
		}
		deleted++
	}
	folder := "Trash"
	if t := trashFolder(folders); t != nil {
		folder = t.Name
	}
	return jsonResult(map[string]any{"deleted": deleted, "folder": folder})
}

func (n *Nylas) handleMoveConversation(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	convID, _ := req.RequireString("conversation_id")
	target, _ := req.RequireString("folder")
	if convID == "" || strings.TrimSpace(target) == "" {
		return mcp.NewToolResultError("conversation_id and folder are required"), nil
	}
	if err := n.ensureGrant(ctx); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	folders, err := n.listFolders(ctx, true)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	var source *nylasFolder
	if ref := req.GetString("source_folder", ""); ref != "" {
		if source = resolveFolder(folders, ref); source == nil {
			return mcp.NewToolResultError(fmt.Sprintf("source folder %q not found — list_mailboxes shows the folders", ref)), nil
		}
	} else if source = inboxFolder(folders); source == nil {
		return mcp.NewToolResultError("no inbox folder found"), nil
	}
	dest := resolveFolder(folders, target)
	if dest == nil && !req.GetBool("create", false) {
		return mcp.NewToolResultError(fmt.Sprintf("folder %q not found — pass create=true to create it, or see list_mailboxes", target)), nil
	}
	if dest != nil && dest.ID == source.ID {
		return mcp.NewToolResultError(fmt.Sprintf("source and target are both %s", dest.Name)), nil
	}
	msgs, err := n.threadMessages(ctx, convID, source.ID)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if len(msgs) == 0 {
		return mcp.NewToolResultError(fmt.Sprintf("no messages of conversation %s in %s (wrong id, or not in that folder)", convID, source.Name)), nil
	}
	if dest == nil {
		// Created only now that there is something to move into it.
		grantID, _, _ := n.grant()
		var created nylasFolder
		if _, err := n.callJSON(ctx, "folders.create", map[string]string{"grant_id": grantID}, nil, map[string]any{"name": strings.TrimSpace(target)}, &created); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("create folder %q: %v", target, err)), nil
		}
		n.mu.Lock()
		n.folders = nil // the cache no longer knows every folder
		n.mu.Unlock()
		dest = &created
	}
	moved, err := n.moveMessages(ctx, msgs, source.ID, dest.ID)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("%v (%d of %d moved to %s)", err, moved, len(msgs), dest.Name)), nil
	}
	return jsonResult(map[string]any{"moved": moved, "from": source.Name, "to": dest.Name})
}

func (n *Nylas) handleListAttachments(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	msgID, _ := req.RequireString("message_id")
	if msgID == "" {
		return mcp.NewToolResultError("message_id is required"), nil
	}
	if err := n.ensureGrant(ctx); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	grantID, _, _ := n.grant()
	var m nylasMessage
	if _, err := n.callJSON(ctx, "message", map[string]string{"grant_id": grantID, "message_id": msgID}, nil, nil, &m); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	out := make([]nylasAttachmentInfo, 0, len(m.Attachments))
	for _, a := range m.Attachments {
		out = append(out, nylasAttachmentInfo{ID: a.ID, Filename: a.Filename, ContentType: a.ContentType, Size: a.Size, IsInline: a.IsInline})
	}
	return jsonResult(out)
}

func (n *Nylas) handleGetAttachment(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	msgID, _ := req.RequireString("message_id")
	attID, _ := req.RequireString("attachment_id")
	if msgID == "" || attID == "" {
		return mcp.NewToolResultError("message_id and attachment_id are required"), nil
	}
	outputFile := req.GetString("output_file", "")
	if outputFile != "" {
		if _, err := validateOutputFile(outputFile); err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
	}
	if err := n.ensureGrant(ctx); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	grantID, _, _ := n.grant()
	params := map[string]string{"grant_id": grantID, "attachment_id": attID}
	q := url.Values{}
	q.Set("message_id", msgID)

	var meta nylasAttachment
	if _, err := n.callJSON(ctx, "attachment", params, q, nil, &meta); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if outputFile == "" && meta.Size > nylasInlineAttachment {
		return mcp.NewToolResultError(fmt.Sprintf("attachment %s is %d bytes — pass output_file to save it to disk (inline limit %d bytes)", meta.Filename, meta.Size, nylasInlineAttachment)), nil
	}

	resp, err := n.call(ctx, "attachment.download", params, q, nil)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		return mcp.NewToolResultError((&nylasHTTPError{Status: resp.StatusCode, Body: trimForError(data)}).Error()), nil
	}
	contentType := resp.Header.Get("Content-Type")
	if contentType == "" || strings.HasPrefix(contentType, "application/octet-stream") {
		if meta.ContentType != "" {
			contentType = meta.ContentType
		}
	}
	if outputFile != "" {
		return saveResponseToFile(resp, contentType, outputFile)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, nylasInlineAttachment+1))
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("read attachment: %v", err)), nil
	}
	if len(data) > nylasInlineAttachment {
		return mcp.NewToolResultError(fmt.Sprintf("attachment %s exceeds the inline limit of %d bytes — pass output_file", meta.Filename, nylasInlineAttachment)), nil
	}
	return jsonResult(map[string]any{
		"id":           meta.ID,
		"filename":     meta.Filename,
		"contentType":  contentType,
		"size":         len(data),
		"contentBytes": base64.StdEncoding.EncodeToString(data),
	})
}
