package tools

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"sort"
	"strings"
	"time"

	imaplib "github.com/emersion/go-imap"
	imapclient "github.com/emersion/go-imap/client"
	"github.com/mark3labs/mcp-go/mcp"

	"github.com/smnhffmnn/mux/internal/config"
)

const (
	imapFetchLimit  = 200        // page size: newest messages read per list window / lookup page
	imapScanMax     = 5000       // max messages scanned (newest first) when locating a conversation
	imapBodyMaxSize = 256 * 1024 // max raw message size for body extraction
	imapTextMaxSize = 64 * 1024  // max extracted text body size
)

// DefaultIMAPInstructions provides guidance for LLMs using IMAP tools.
const DefaultIMAPInstructions = `IMAP mailbox with conversation threading. Conversations are grouped by subject.
Conversation IDs are stable as long as the subject doesn't change.
list_conversations reads a window of the newest 200 inbox messages; page backwards with offset (0, 200, 400 …).
list_mailboxes reports messages/unseen per folder, so the number of pages can be computed.
get/archive/delete/reply/forward locate a conversation beyond the window (newest 5000 messages, thread completed via subject search).
Each tool call opens a fresh IMAP connection (no persistent session).
Archive/delete operations move messages to Archive/Trash folders.
Draft creation saves to the Drafts folder — it does NOT send.`

// IMAP wraps an IMAP mailbox as MCP tools with conversation threading.
type IMAP struct {
	host     string
	port     int
	user     string
	password string
	domain   string // extracted from user for Message-ID generation
	dialer   Dialer
}

// NewIMAP creates an IMAP connection from config.
func NewIMAP(conn config.Connection, dialer Dialer) (*IMAP, error) {
	if conn.Host == "" || conn.User == "" || conn.Password == "" {
		return nil, fmt.Errorf("imap: host, user, and password are required")
	}
	port := conn.Port
	if port == 0 {
		port = 993
	}

	// Extract domain from email for Message-ID generation (N6)
	domain := "mux.local"
	if idx := strings.LastIndex(conn.User, "@"); idx >= 0 {
		domain = conn.User[idx+1:]
	}

	return &IMAP{
		host:     conn.Host,
		port:     port,
		user:     conn.User,
		password: conn.Password,
		domain:   domain,
		dialer:   dialer,
	}, nil
}

// Tools returns the MCP tools for this IMAP connection.
func (im *IMAP) Tools() []ToolDef {
	return []ToolDef{
		{
			Tool: mcp.NewTool("list_conversations",
				mcp.WithDescription("List IMAP inbox conversations grouped by thread. Reads a window of the newest 200 messages (offset 0); page backwards with offset 200, 400, … until list_mailboxes' message count is reached. Returns conversations with latest message, participants, and message count."),
				mcp.WithNumber("limit", mcp.Description("Max conversations to return (default: 20)")),
				mcp.WithNumber("offset", mcp.Description("Skip this many of the newest inbox messages before reading the 200-message window (default: 0). Counts messages, not conversations — a thread can appear on two adjacent pages.")),
			),
			Handler: im.handleListConversations,
		},
		{
			Tool: mcp.NewTool("get_conversation",
				mcp.WithDescription("Get all messages in an IMAP conversation thread with full body text."),
				mcp.WithString("conversation_id", mcp.Required(), mcp.Description("Conversation ID")),
			),
			Handler: im.handleGetConversation,
		},
		{
			Tool: mcp.NewTool("search_messages",
				mcp.WithDescription("Search IMAP messages by text. Results grouped by conversation, IDs consistent with list_conversations."),
				mcp.WithString("query", mcp.Required(), mcp.Description("Search text (searched in subject and body)")),
				mcp.WithNumber("limit", mcp.Description("Max conversations to return (default: 25)")),
			),
			Handler: im.handleSearchMessages,
		},
		{
			Tool: mcp.NewTool("list_mailboxes",
				mcp.WithDescription("List all IMAP mailbox folders with their message and unseen counts."),
			),
			Handler: im.handleListMailboxes,
		},
		{
			Tool: mcp.NewTool("archive_conversation",
				mcp.WithDescription("Archive an IMAP conversation (move to Archive or Trash folder)."),
				mcp.WithString("conversation_id", mcp.Required(), mcp.Description("Conversation ID")),
			),
			Handler: im.handleArchiveConversation,
		},
		{
			Tool: mcp.NewTool("delete_conversation",
				mcp.WithDescription("Delete an IMAP conversation (move to Trash folder, not permanent)."),
				mcp.WithString("conversation_id", mcp.Required(), mcp.Description("Conversation ID")),
			),
			Handler: im.handleDeleteConversation,
		},
		{
			Tool: mcp.NewTool("create_reply_draft",
				mcp.WithDescription("Create a reply draft for the latest message in an IMAP conversation. Saved to Drafts, does NOT send."),
				mcp.WithString("conversation_id", mcp.Required(), mcp.Description("Conversation ID")),
				mcp.WithString("body", mcp.Required(), mcp.Description("Reply text (plain text)")),
			),
			Handler: im.handleCreateReplyDraft,
		},
		{
			Tool: mcp.NewTool("create_forward_draft",
				mcp.WithDescription("Create a forward draft for the latest message in an IMAP conversation. Saved to Drafts, does NOT send."),
				mcp.WithString("conversation_id", mcp.Required(), mcp.Description("Conversation ID")),
				mcp.WithString("to", mcp.Required(), mcp.Description("Recipient email address")),
				mcp.WithString("body", mcp.Description("Optional comment to include above the forwarded message")),
			),
			Handler: im.handleCreateForwardDraft,
		},
	}
}

// --- IMAP client helpers ---

// connect creates a new IMAP TLS connection, respecting context for cancellation (C10).
func (im *IMAP) connect(ctx context.Context) (*imapclient.Client, error) {
	addr := fmt.Sprintf("%s:%d", im.host, im.port)

	var rawConn net.Conn
	var err error

	if im.dialer != nil {
		rawConn, err = im.dialer.DialContext(ctx, "tcp", addr)
	} else {
		d := &net.Dialer{}
		rawConn, err = d.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", addr, err)
	}

	tlsConn := tls.Client(rawConn, &tls.Config{ServerName: im.host})
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		rawConn.Close()
		return nil, fmt.Errorf("TLS handshake: %w", err)
	}

	c, err := imapclient.New(tlsConn)
	if err != nil {
		tlsConn.Close()
		return nil, fmt.Errorf("IMAP init: %w", err)
	}
	return c, nil
}

// Verify connects and logs in, then logs out — the connection test's way to
// exercise exactly the dial/TLS/login path the live tools use (including the
// tunnel dialer when one was passed to NewIMAP).
func (im *IMAP) Verify(ctx context.Context) error {
	c, err := im.connect(ctx)
	if err != nil {
		return err
	}
	defer c.Logout()

	if err := c.Login(im.user, im.password); err != nil {
		return fmt.Errorf("login as %s: %w", im.user, err)
	}
	return nil
}

// withClient connects, logs in, runs fn, then logs out.
func (im *IMAP) withClient(ctx context.Context, fn func(*imapclient.Client) (*mcp.CallToolResult, error)) (*mcp.CallToolResult, error) {
	c, err := im.connect(ctx)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("IMAP connect failed: %v", err)), nil
	}
	defer c.Logout()

	if err := c.Login(im.user, im.password); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("IMAP login failed: %v", err)), nil
	}

	return fn(c)
}

// envelopeWindow computes the sequence-number range of one page of the newest
// messages: offset skips the newest messages, limit is the page size. ok is false
// when the page lies beyond the mailbox (or the mailbox is empty).
func envelopeWindow(total, offset, limit uint32) (from, to uint32, ok bool) {
	if total == 0 || limit == 0 || offset >= total {
		return 0, 0, false
	}
	to = total - offset
	from = 1
	if to > limit {
		from = to - limit + 1
	}
	return from, to, true
}

// fetchEnvelopes fetches the newest N messages from the given mailbox.
func fetchEnvelopes(c *imapclient.Client, mailbox string, limit uint32) ([]*imaplib.Message, error) {
	msgs, _, err := fetchEnvelopesWindow(c, mailbox, 0, limit)
	return msgs, err
}

// fetchEnvelopesWindow fetches one page of envelopes (see envelopeWindow) and
// returns the mailbox's total message count alongside, so callers can page.
func fetchEnvelopesWindow(c *imapclient.Client, mailbox string, offset, limit uint32) ([]*imaplib.Message, uint32, error) {
	mbox, err := c.Select(mailbox, true)
	if err != nil {
		return nil, 0, fmt.Errorf("SELECT %s: %w", mailbox, err)
	}
	from, to, ok := envelopeWindow(mbox.Messages, offset, limit)
	if !ok {
		return nil, mbox.Messages, nil
	}

	seqSet := new(imaplib.SeqSet)
	seqSet.AddRange(from, to)

	msgs, err := fetchEnvelopeSet(c, seqSet, false)
	return msgs, mbox.Messages, err
}

// fetchEnvelopesByUID fetches envelopes for the given UIDs of the selected mailbox.
func fetchEnvelopesByUID(c *imapclient.Client, uids []uint32) ([]*imaplib.Message, error) {
	if len(uids) == 0 {
		return nil, nil
	}
	seqSet := new(imaplib.SeqSet)
	seqSet.AddNum(uids...)
	return fetchEnvelopeSet(c, seqSet, true)
}

func fetchEnvelopeSet(c *imapclient.Client, seqSet *imaplib.SeqSet, byUID bool) ([]*imaplib.Message, error) {
	items := []imaplib.FetchItem{imaplib.FetchEnvelope, imaplib.FetchUid, imaplib.FetchFlags}

	messages := make(chan *imaplib.Message, 100)
	done := make(chan error, 1)
	go func() {
		if byUID {
			done <- c.UidFetch(seqSet, items, messages)
		} else {
			done <- c.Fetch(seqSet, items, messages)
		}
	}()

	var result []*imaplib.Message
	for msg := range messages {
		result = append(result, msg)
	}
	if err := <-done; err != nil {
		return nil, fmt.Errorf("FETCH: %w", err)
	}

	return result, nil
}

// fetchBodies fetches full message bodies by UID and extracts text content (B4: MIME decoding).
func fetchBodies(c *imapclient.Client, uids []uint32) (map[uint32]string, error) {
	if len(uids) == 0 {
		return nil, nil
	}

	seqSet := new(imaplib.SeqSet)
	for _, uid := range uids {
		seqSet.AddNum(uid)
	}

	// Fetch the entire message for MIME parsing (not just TEXT specifier)
	section := &imaplib.BodySectionName{Peek: true}

	messages := make(chan *imaplib.Message, len(uids))
	done := make(chan error, 1)
	go func() {
		done <- c.UidFetch(seqSet, []imaplib.FetchItem{section.FetchItem(), imaplib.FetchUid}, messages)
	}()

	bodies := make(map[uint32]string)
	for msg := range messages {
		if body := msg.GetBody(section); body != nil {
			text := extractTextFromMessage(body)
			if text != "" {
				bodies[msg.Uid] = text
			}
		}
	}
	if err := <-done; err != nil {
		return bodies, fmt.Errorf("UID FETCH bodies: %w", err)
	}

	return bodies, nil
}

// extractTextFromMessage parses an RFC822 message and extracts the text/plain body.
// Handles multipart MIME messages and content-transfer-encoding.
func extractTextFromMessage(r io.Reader) string {
	// Buffer the entire message first so we can fall back to raw content on parse failure.
	raw, err := io.ReadAll(io.LimitReader(r, imapBodyMaxSize))
	if err != nil || len(raw) == 0 {
		return ""
	}

	msg, err := mail.ReadMessage(strings.NewReader(string(raw)))
	if err != nil {
		// Not a valid RFC822 message — return raw content as fallback
		if len(raw) > imapTextMaxSize {
			raw = raw[:imapTextMaxSize]
		}
		return string(raw)
	}

	ct := msg.Header.Get("Content-Type")
	if ct == "" {
		ct = "text/plain"
	}
	mediaType, params, _ := mime.ParseMediaType(ct)

	if strings.HasPrefix(mediaType, "multipart/") {
		boundary := params["boundary"]
		if boundary != "" {
			if text := extractFromMultipart(msg.Body, boundary); text != "" {
				return text
			}
		}
		// Multipart but no text part found — return empty
		return ""
	}

	// Single-part message
	encoding := msg.Header.Get("Content-Transfer-Encoding")
	reader := decodeTransferEncoding(msg.Body, encoding)
	data, _ := io.ReadAll(io.LimitReader(reader, imapTextMaxSize))
	return string(data)
}

// extractFromMultipart walks MIME parts to find text/plain content.
func extractFromMultipart(r io.Reader, boundary string) string {
	mr := multipart.NewReader(r, boundary)
	var htmlFallback string

	for {
		part, err := mr.NextPart()
		if err != nil {
			break
		}

		ct := part.Header.Get("Content-Type")
		if ct == "" {
			ct = "text/plain"
		}
		mediaType, params, _ := mime.ParseMediaType(ct)

		if strings.HasPrefix(mediaType, "multipart/") {
			// Nested multipart
			if b := params["boundary"]; b != "" {
				if text := extractFromMultipart(part, b); text != "" {
					return text
				}
			}
			continue
		}

		if mediaType == "text/plain" {
			encoding := part.Header.Get("Content-Transfer-Encoding")
			reader := decodeTransferEncoding(part, encoding)
			data, _ := io.ReadAll(io.LimitReader(reader, imapTextMaxSize))
			return string(data)
		}

		if mediaType == "text/html" && htmlFallback == "" {
			encoding := part.Header.Get("Content-Transfer-Encoding")
			reader := decodeTransferEncoding(part, encoding)
			data, _ := io.ReadAll(io.LimitReader(reader, imapTextMaxSize))
			htmlFallback = stripHTMLTags(string(data))
		}
	}

	return htmlFallback
}

// decodeTransferEncoding wraps a reader with content-transfer-encoding decoding.
func decodeTransferEncoding(r io.Reader, encoding string) io.Reader {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "base64":
		return base64.NewDecoder(base64.StdEncoding, r)
	case "quoted-printable":
		return quotedprintable.NewReader(r)
	default:
		return r // 7bit, 8bit, binary — pass through
	}
}

// stripHTMLTags is a basic HTML tag stripper for text fallback.
func stripHTMLTags(s string) string {
	var b strings.Builder
	inTag := false
	for _, r := range s {
		switch {
		case r == '<':
			inTag = true
		case r == '>':
			inTag = false
		case !inTag:
			b.WriteRune(r)
		}
	}
	return strings.TrimSpace(b.String())
}

// --- Threading ---

type imapMsg struct {
	UID       uint32
	MessageID string
	InReplyTo string
	Subject   string
	From      string
	FromName  string
	To        []string
	CC        []string
	Date      time.Time
	Flags     []string
}

func msgsFromIMAP(messages []*imaplib.Message) []imapMsg {
	var result []imapMsg
	for _, msg := range messages {
		if msg.Envelope == nil {
			continue
		}
		env := msg.Envelope
		m := imapMsg{
			UID:       msg.Uid,
			MessageID: normalizeMessageID(env.MessageId),
			InReplyTo: normalizeMessageID(env.InReplyTo),
			Subject:   env.Subject,
			Date:      env.Date,
			Flags:     msg.Flags,
		}
		if len(env.From) > 0 {
			m.From = addrString(env.From[0])
			m.FromName = env.From[0].PersonalName
		}
		for _, a := range env.To {
			if s := addrString(a); s != "" {
				m.To = append(m.To, s)
			}
		}
		for _, a := range env.Cc {
			if s := addrString(a); s != "" {
				m.CC = append(m.CC, s)
			}
		}
		result = append(result, m)
	}
	return result
}

// normalizeMessageID ensures angle brackets on Message-IDs for consistent hashing (N22).
func normalizeMessageID(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return ""
	}
	if !strings.HasPrefix(id, "<") {
		id = "<" + id
	}
	if !strings.HasSuffix(id, ">") {
		id = id + ">"
	}
	return id
}

// addrString formats an IMAP address as "user@host". Returns "" for invalid addresses (N5).
func addrString(a *imaplib.Address) string {
	if a == nil || a.MailboxName == "" || a.HostName == "" {
		return ""
	}
	return a.MailboxName + "@" + a.HostName
}

// threadMessages groups messages into conversations using normalized subject (C6: stable IDs).
// Within each conversation, messages are ordered chronologically.
func threadMessages(msgs []imapMsg) map[string][]imapMsg {
	// Group by normalized subject — this produces stable conversation IDs
	// regardless of which messages are in the fetch window.
	groups := make(map[string][]imapMsg)
	for _, m := range msgs {
		convID := makeConversationID(normalizeSubject(m.Subject))
		groups[convID] = append(groups[convID], m)
	}

	// Sort each group chronologically
	for convID := range groups {
		sort.Slice(groups[convID], func(i, j int) bool {
			return groups[convID][i].Date.Before(groups[convID][j].Date)
		})
	}

	return groups
}

func makeConversationID(normalizedSubject string) string {
	h := sha256.Sum256([]byte(normalizedSubject))
	return hex.EncodeToString(h[:8]) // 16 hex chars
}

// normalizeSubject strips reply/forward prefixes for consistent grouping (N4: German prefixes).
func normalizeSubject(s string) string {
	s = strings.TrimSpace(s)
	for {
		lower := strings.ToLower(s)
		changed := false
		for _, prefix := range []string{
			"re:", "fw:", "fwd:", "aw:", "wg:", "sv:", "betr.:",
			"re[", "fw[", "aw[", // "Re[2]:" style
		} {
			if strings.HasPrefix(lower, prefix) {
				// Find end of prefix (skip past any [...] suffix)
				cut := len(prefix)
				rest := s[cut:]
				if strings.HasPrefix(prefix, "re[") || strings.HasPrefix(prefix, "fw[") || strings.HasPrefix(prefix, "aw[") {
					if idx := strings.Index(rest, "]:"); idx >= 0 {
						cut += idx + 2
					}
				}
				s = strings.TrimSpace(s[cut:])
				changed = true
				break
			}
		}
		if !changed {
			break
		}
	}
	return s
}

// --- Conversation data models ---

type imapConversation struct {
	ConversationID string           `json:"conversationId"`
	Subject        string           `json:"subject"`
	MessageCount   int              `json:"messageCount"`
	MessageUIDs    []uint32         `json:"messageUids"`
	Participants   []string         `json:"participants"`
	LatestMessage  imapMessageBrief `json:"latestMessage"`
	IsRead         bool             `json:"isRead"`
	IsFlagged      bool             `json:"isFlagged"`
}

type imapMessageBrief struct {
	UID        uint32   `json:"uid"`
	Sender     string   `json:"sender"`
	SenderName string   `json:"senderName"`
	To         []string `json:"to"`
	CC         []string `json:"cc,omitempty"`
	Date       string   `json:"date"`
	Subject    string   `json:"subject"`
}

type imapMessageFull struct {
	UID        uint32   `json:"uid"`
	Sender     string   `json:"sender"`
	SenderName string   `json:"senderName"`
	To         []string `json:"to"`
	CC         []string `json:"cc,omitempty"`
	Date       string   `json:"date"`
	Subject    string   `json:"subject"`
	Body       string   `json:"body"`
	IsRead     bool     `json:"isRead"`
	IsFlagged  bool     `json:"isFlagged"`
}

func buildConversations(groups map[string][]imapMsg, limit int) []imapConversation {
	type entry struct {
		convID string
		latest time.Time
	}
	var entries []entry
	for convID, msgs := range groups {
		latest := msgs[len(msgs)-1].Date
		entries = append(entries, entry{convID, latest})
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].latest.After(entries[j].latest)
	})

	var result []imapConversation
	for _, e := range entries {
		if len(result) >= limit {
			break
		}
		msgs := groups[e.convID]
		latest := msgs[len(msgs)-1]

		pSet := make(map[string]bool)
		allRead := true
		anyFlagged := false
		uids := make([]uint32, len(msgs))

		for i, m := range msgs {
			uids[i] = m.UID
			if m.From != "" {
				pSet[m.From] = true
			}
			for _, to := range m.To {
				pSet[to] = true
			}
			if !hasFlag(m.Flags, imaplib.SeenFlag) {
				allRead = false
			}
			if hasFlag(m.Flags, imaplib.FlaggedFlag) {
				anyFlagged = true
			}
		}

		participants := make([]string, 0, len(pSet))
		for p := range pSet {
			participants = append(participants, p)
		}
		sort.Strings(participants)

		result = append(result, imapConversation{
			ConversationID: e.convID,
			Subject:        msgs[0].Subject,
			MessageCount:   len(msgs),
			MessageUIDs:    uids,
			Participants:   participants,
			LatestMessage: imapMessageBrief{
				UID:        latest.UID,
				Sender:     latest.From,
				SenderName: latest.FromName,
				To:         latest.To,
				CC:         latest.CC,
				Date:       latest.Date.Format(time.RFC3339),
				Subject:    latest.Subject,
			},
			IsRead:    allRead,
			IsFlagged: anyFlagged,
		})
	}

	return result
}

func hasFlag(flags []string, flag string) bool {
	for _, f := range flags {
		if strings.EqualFold(f, flag) {
			return true
		}
	}
	return false
}

// --- Handlers ---

func (im *IMAP) handleListConversations(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	limit := 20
	if v, ok := req.GetArguments()["limit"].(float64); ok && v > 0 {
		limit = int(v)
	}
	var offset uint32
	if v, ok := req.GetArguments()["offset"].(float64); ok && v > 0 {
		offset = uint32(v)
	}

	return im.withClient(ctx, func(c *imapclient.Client) (*mcp.CallToolResult, error) {
		rawMsgs, _, err := fetchEnvelopesWindow(c, "INBOX", offset, imapFetchLimit)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		msgs := msgsFromIMAP(rawMsgs)
		groups := threadMessages(msgs)
		conversations := buildConversations(groups, limit)

		return imapJSONResult(conversations)
	})
}

func (im *IMAP) handleGetConversation(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	convID, _ := req.RequireString("conversation_id")
	if convID == "" {
		return mcp.NewToolResultError("conversation_id is required"), nil
	}

	return im.withClient(ctx, func(c *imapclient.Client) (*mcp.CallToolResult, error) {
		thread, err := im.findConversation(c, convID)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		uids := make([]uint32, len(thread))
		for i, m := range thread {
			uids[i] = m.UID
		}
		bodies, err := fetchBodies(c, uids)
		if err != nil {
			log.Printf("[imap] Warning: body fetch partially failed: %v", err)
		}

		var fullMsgs []imapMessageFull
		for _, m := range thread {
			fullMsgs = append(fullMsgs, imapMessageFull{
				UID:        m.UID,
				Sender:     m.From,
				SenderName: m.FromName,
				To:         m.To,
				CC:         m.CC,
				Date:       m.Date.Format(time.RFC3339),
				Subject:    m.Subject,
				Body:       bodies[m.UID],
				IsRead:     hasFlag(m.Flags, imaplib.SeenFlag),
				IsFlagged:  hasFlag(m.Flags, imaplib.FlaggedFlag),
			})
		}

		return imapJSONResult(fullMsgs)
	})
}

// handleSearchMessages searches messages and groups results by conversation (C7: consistent IDs).
// Fetches all recent messages for threading, then filters to conversations containing search hits.
func (im *IMAP) handleSearchMessages(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	query, _ := req.RequireString("query")
	if query == "" {
		return mcp.NewToolResultError("query is required"), nil
	}
	limit := 25
	if v, ok := req.GetArguments()["limit"].(float64); ok && v > 0 {
		limit = int(v)
	}

	return im.withClient(ctx, func(c *imapclient.Client) (*mcp.CallToolResult, error) {
		// The newest window is threaded completely (consistent with list_conversations);
		// matches older than the window are fetched by UID and threaded among themselves.
		rawMsgs, err := fetchEnvelopes(c, "INBOX", imapFetchLimit)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		// SEARCH runs server-side over the whole mailbox.
		criteria := imaplib.NewSearchCriteria()
		criteria.Text = []string{query}
		matchedUIDs, err := c.UidSearch(criteria)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("SEARCH failed: %v", err)), nil
		}
		if len(matchedUIDs) == 0 {
			return imapJSONResult([]imapConversation{})
		}
		if len(matchedUIDs) > imapScanMax {
			matchedUIDs = matchedUIDs[len(matchedUIDs)-imapScanMax:] // UIDs ascend with age: keep the newest
		}

		matchSet := make(map[uint32]bool, len(matchedUIDs))
		for _, uid := range matchedUIDs {
			matchSet[uid] = true
		}
		inWindow := make(map[uint32]bool, len(rawMsgs))
		for _, m := range rawMsgs {
			inWindow[m.Uid] = true
		}
		var older []uint32
		for _, uid := range matchedUIDs {
			if !inWindow[uid] {
				older = append(older, uid)
			}
		}
		if len(older) > 0 {
			olderMsgs, err := fetchEnvelopesByUID(c, older)
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			rawMsgs = append(rawMsgs, olderMsgs...)
		}

		// Thread everything, then keep conversations with at least one match
		msgs := msgsFromIMAP(rawMsgs)
		groups := threadMessages(msgs)

		matchedGroups := make(map[string][]imapMsg)
		for convID, thread := range groups {
			for _, m := range thread {
				if matchSet[m.UID] {
					matchedGroups[convID] = thread
					break
				}
			}
		}

		conversations := buildConversations(matchedGroups, limit)
		return imapJSONResult(conversations)
	})
}

func (im *IMAP) handleListMailboxes(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return im.withClient(ctx, func(c *imapclient.Client) (*mcp.CallToolResult, error) {
		mailboxes := make(chan *imaplib.MailboxInfo, 50)
		done := make(chan error, 1)
		go func() {
			done <- c.List("", "*", mailboxes)
		}()

		var infos []*imaplib.MailboxInfo
		for mbox := range mailboxes {
			infos = append(infos, mbox)
		}
		if err := <-done; err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("LIST failed: %v", err)), nil
		}

		var result []map[string]any
		for _, mbox := range infos {
			entry := map[string]any{
				"name":       mbox.Name,
				"delimiter":  string(mbox.Delimiter),
				"attributes": mbox.Attributes,
			}
			if !hasFlag(mbox.Attributes, imaplib.NoSelectAttr) {
				// STATUS is cheap and lets callers size their paging (messages / 200 windows).
				if st, err := c.Status(mbox.Name, []imaplib.StatusItem{imaplib.StatusMessages, imaplib.StatusUnseen}); err == nil {
					entry["messages"] = st.Messages
					entry["unseen"] = st.Unseen
				}
			}
			result = append(result, entry)
		}

		return imapJSONResult(result)
	})
}

func (im *IMAP) handleArchiveConversation(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	convID, _ := req.RequireString("conversation_id")
	if convID == "" {
		return mcp.NewToolResultError("conversation_id is required"), nil
	}

	return im.withClient(ctx, func(c *imapclient.Client) (*mcp.CallToolResult, error) {
		thread, err := im.findConversation(c, convID)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		// Find archive folder before re-selecting
		archiveFolder := findFolder(c, "Archive", "INBOX.Archive", "Archives", "Archiv")
		if archiveFolder == "" {
			archiveFolder = findFolder(c, "Trash", "INBOX.Trash", "Deleted Messages")
		}
		if archiveFolder == "" {
			return mcp.NewToolResultError("no Archive or Trash folder found"), nil
		}

		// Re-select INBOX in read-write mode
		if _, err := c.Select("INBOX", false); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("SELECT INBOX (rw): %v", err)), nil
		}

		seqSet := uidSeqSet(thread)

		if err := c.UidCopy(seqSet, archiveFolder); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("COPY to %s failed: %v", archiveFolder, err)), nil
		}

		// Mark as deleted and expunge (C8: only our UIDs are marked)
		item := imaplib.FormatFlagsOp(imaplib.AddFlags, true)
		if err := c.UidStore(seqSet, item, []interface{}{imaplib.DeletedFlag}, nil); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("STORE \\Deleted: %v", err)), nil
		}
		if err := c.Expunge(nil); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("EXPUNGE: %v", err)), nil
		}

		return imapJSONResult(map[string]any{
			"archived": len(thread),
			"folder":   archiveFolder,
		})
	})
}

// handleDeleteConversation moves messages to Trash instead of permanent deletion (C9).
func (im *IMAP) handleDeleteConversation(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	convID, _ := req.RequireString("conversation_id")
	if convID == "" {
		return mcp.NewToolResultError("conversation_id is required"), nil
	}

	return im.withClient(ctx, func(c *imapclient.Client) (*mcp.CallToolResult, error) {
		thread, err := im.findConversation(c, convID)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		// Find Trash folder
		trashFolder := findFolder(c, "Trash", "INBOX.Trash", "Deleted Messages", "Deleted Items", "Papierkorb")
		if trashFolder == "" {
			return mcp.NewToolResultError("Trash folder not found"), nil
		}

		// Re-select INBOX in read-write mode
		if _, err := c.Select("INBOX", false); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("SELECT INBOX (rw): %v", err)), nil
		}

		seqSet := uidSeqSet(thread)

		// Copy to Trash, then remove from INBOX
		if err := c.UidCopy(seqSet, trashFolder); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("COPY to %s failed: %v", trashFolder, err)), nil
		}

		item := imaplib.FormatFlagsOp(imaplib.AddFlags, true)
		if err := c.UidStore(seqSet, item, []interface{}{imaplib.DeletedFlag}, nil); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("STORE \\Deleted: %v", err)), nil
		}
		if err := c.Expunge(nil); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("EXPUNGE: %v", err)), nil
		}

		return imapJSONResult(map[string]any{
			"deleted": len(thread),
			"folder":  trashFolder,
		})
	})
}

func (im *IMAP) handleCreateReplyDraft(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	convID, _ := req.RequireString("conversation_id")
	body, _ := req.RequireString("body")
	if convID == "" || body == "" {
		return mcp.NewToolResultError("conversation_id and body are required"), nil
	}

	return im.withClient(ctx, func(c *imapclient.Client) (*mcp.CallToolResult, error) {
		thread, err := im.findConversation(c, convID)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		latest := thread[len(thread)-1]

		subject := latest.Subject
		if !strings.HasPrefix(strings.ToLower(subject), "re:") {
			subject = "Re: " + subject
		}

		// Build References from thread Message-IDs (B6: proper chain)
		var refs []string
		for _, m := range thread {
			if m.MessageID != "" {
				refs = append(refs, m.MessageID)
			}
		}

		msg := im.buildRFC822Message(
			[]string{latest.From},
			nil,
			subject,
			latest.MessageID,        // In-Reply-To
			strings.Join(refs, " "), // References: full chain
			normalizeCRLF(body),
		)

		draftsFolder := findFolder(c, "Drafts", "INBOX.Drafts", "Draft", "Entwürfe")
		if draftsFolder == "" {
			return mcp.NewToolResultError("Drafts folder not found"), nil
		}

		if err := c.Append(draftsFolder, []string{imaplib.DraftFlag, imaplib.SeenFlag}, time.Now(), msg); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("APPEND to Drafts: %v", err)), nil
		}

		return imapJSONResult(map[string]any{
			"status":   "draft_created",
			"folder":   draftsFolder,
			"reply_to": latest.From,
			"subject":  subject,
		})
	})
}

func (im *IMAP) handleCreateForwardDraft(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	convID, _ := req.RequireString("conversation_id")
	to, _ := req.RequireString("to")
	if convID == "" || to == "" {
		return mcp.NewToolResultError("conversation_id and to are required"), nil
	}

	comment := ""
	if v, ok := req.GetArguments()["body"].(string); ok {
		comment = v
	}

	return im.withClient(ctx, func(c *imapclient.Client) (*mcp.CallToolResult, error) {
		thread, err := im.findConversation(c, convID)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		latest := thread[len(thread)-1]

		// Fetch the body of the latest message to include in forward
		bodies, _ := fetchBodies(c, []uint32{latest.UID})
		originalBody := bodies[latest.UID]

		subject := latest.Subject
		if !strings.HasPrefix(strings.ToLower(subject), "fwd:") {
			subject = "Fwd: " + subject
		}

		fwdBody := ""
		if comment != "" {
			fwdBody = comment + "\r\n\r\n"
		}
		fwdBody += "---------- Forwarded message ----------\r\n"
		fwdBody += fmt.Sprintf("From: %s\r\nDate: %s\r\nSubject: %s\r\nTo: %s\r\n\r\n",
			latest.From, latest.Date.Format(time.RFC1123Z), latest.Subject, strings.Join(latest.To, ", "))
		fwdBody += normalizeCRLF(originalBody)

		// B7: No In-Reply-To on forwards — forwards are not replies
		msg := im.buildRFC822Message(
			[]string{to},
			nil,
			subject,
			"", // no In-Reply-To
			"", // no References
			fwdBody,
		)

		draftsFolder := findFolder(c, "Drafts", "INBOX.Drafts", "Draft", "Entwürfe")
		if draftsFolder == "" {
			return mcp.NewToolResultError("Drafts folder not found"), nil
		}

		if err := c.Append(draftsFolder, []string{imaplib.DraftFlag, imaplib.SeenFlag}, time.Now(), msg); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("APPEND to Drafts: %v", err)), nil
		}

		return imapJSONResult(map[string]any{
			"status":  "draft_created",
			"folder":  draftsFolder,
			"to":      to,
			"subject": subject,
		})
	})
}

// --- Shared helpers ---

// findConversation fetches envelopes, threads them, and returns the messages for the given conversation ID.
// findConversation locates a conversation anywhere in the newest imapScanMax inbox
// messages: it scans page by page (newest first) until the thread appears, then
// completes the thread with a server-side subject search, so messages of the same
// conversation older than that page are included too.
func (im *IMAP) findConversation(c *imapclient.Client, convID string) ([]imapMsg, error) {
	var (
		thread []imapMsg
		total  uint32
	)
	for offset := uint32(0); ; offset += imapFetchLimit {
		rawMsgs, mboxTotal, err := fetchEnvelopesWindow(c, "INBOX", offset, imapFetchLimit)
		if err != nil {
			return nil, err
		}
		total = mboxTotal
		if len(rawMsgs) == 0 {
			break
		}
		if t, ok := threadMessages(msgsFromIMAP(rawMsgs))[convID]; ok {
			thread = t
			break
		}
		next := offset + imapFetchLimit
		if next >= total || next >= imapScanMax {
			break
		}
	}
	if len(thread) == 0 {
		scanned := total
		if scanned > imapScanMax {
			scanned = imapScanMax
		}
		return nil, fmt.Errorf("conversation %s not found in the newest %d inbox messages", convID, scanned)
	}

	// SEARCH SUBJECT is a case-insensitive substring match, so the normalized subject
	// finds every Re:/Fwd: variant on any page; re-threading keeps only the messages
	// whose conversation ID matches.
	if core := normalizeSubject(thread[0].Subject); core != "" {
		criteria := imaplib.NewSearchCriteria()
		criteria.Header.Add("Subject", core)
		if uids, err := c.UidSearch(criteria); err == nil && len(uids) > len(thread) {
			if len(uids) > imapScanMax {
				uids = uids[len(uids)-imapScanMax:]
			}
			if raw, err := fetchEnvelopesByUID(c, uids); err == nil {
				if full, ok := threadMessages(msgsFromIMAP(raw))[convID]; ok && len(full) > len(thread) {
					thread = full
				}
			}
		}
	}
	return thread, nil
}

// uidSeqSet creates a UID sequence set from a message slice.
func uidSeqSet(msgs []imapMsg) *imaplib.SeqSet {
	seqSet := new(imaplib.SeqSet)
	for _, m := range msgs {
		seqSet.AddNum(m.UID)
	}
	return seqSet
}

// findFolder checks if any of the given folder names exist on the server.
func findFolder(c *imapclient.Client, names ...string) string {
	for _, name := range names {
		mailboxes := make(chan *imaplib.MailboxInfo, 1)
		done := make(chan error, 1)
		go func() {
			done <- c.List("", name, mailboxes)
		}()
		var found string
		for mbox := range mailboxes {
			found = mbox.Name
		}
		if err := <-done; err != nil {
			log.Printf("[imap] LIST %q failed: %v", name, err)
			continue
		}
		if found != "" {
			return found
		}
	}
	return ""
}

// buildRFC822Message creates a minimal RFC822 message suitable for IMAP APPEND.
// Handles RFC 2047 encoding for non-ASCII headers (B5) and CRLF line endings (B8).
func (im *IMAP) buildRFC822Message(to []string, cc []string, subject, inReplyTo, references, body string) *imapLiteral {
	var b strings.Builder

	b.WriteString("From: <" + sanitizeHeaderValue(im.user) + ">\r\n")
	b.WriteString("To: " + formatAddressList(to) + "\r\n")
	if len(cc) > 0 {
		b.WriteString("Cc: " + formatAddressList(cc) + "\r\n")
	}
	b.WriteString("Subject: " + encodeHeader(sanitizeHeaderValue(subject)) + "\r\n")
	b.WriteString("Date: " + time.Now().Format(time.RFC1123Z) + "\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=UTF-8\r\n")
	b.WriteString("Content-Transfer-Encoding: 8bit\r\n")
	if inReplyTo != "" {
		b.WriteString("In-Reply-To: " + sanitizeHeaderValue(inReplyTo) + "\r\n")
	}
	if references != "" {
		b.WriteString("References: " + sanitizeHeaderValue(references) + "\r\n")
	}
	msgID := fmt.Sprintf("<%d.mux@%s>", time.Now().UnixNano(), im.domain)
	b.WriteString("Message-ID: " + msgID + "\r\n")
	b.WriteString("\r\n")
	b.WriteString(body)

	content := b.String()
	return &imapLiteral{strings.NewReader(content), len(content)}
}

// formatAddressList wraps bare email addresses in angle brackets.
// Sanitizes each address against CRLF header injection.
func formatAddressList(addrs []string) string {
	formatted := make([]string, len(addrs))
	for i, a := range addrs {
		a = sanitizeHeaderValue(a)
		if !strings.Contains(a, "<") {
			formatted[i] = "<" + a + ">"
		} else {
			formatted[i] = a
		}
	}
	return strings.Join(formatted, ", ")
}

// sanitizeHeaderValue strips CR and LF characters to prevent CRLF header injection.
func sanitizeHeaderValue(s string) string {
	return strings.NewReplacer("\r", "", "\n", "").Replace(s)
}

// encodeHeader encodes a header value with RFC 2047 if it contains non-ASCII characters (B5).
func encodeHeader(s string) string {
	for _, r := range s {
		if r > 127 {
			return mime.QEncoding.Encode("utf-8", s)
		}
	}
	return s
}

// normalizeCRLF replaces lone LF with CRLF for RFC 5322 compliance (B8).
func normalizeCRLF(s string) string {
	// First normalize any existing CRLF to LF, then convert all LF to CRLF
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	s = strings.ReplaceAll(s, "\n", "\r\n")
	return s
}

// imapLiteral wraps a string reader to implement imap.Literal.
type imapLiteral struct {
	*strings.Reader
	size int
}

func (l *imapLiteral) Len() int {
	return l.size
}

func imapJSONResult(v any) (*mcp.CallToolResult, error) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("JSON marshal error: %v", err)), nil
	}
	return mcp.NewToolResultText(string(data)), nil
}
