package tools

import (
	"context"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/smnhffmnn/mux/internal/config"
)

// Drafts and sending for the nylas type. Nylas composes the MIME message and
// threads a reply from reply_to_message_id, so mux only decides recipients,
// subject and text — the same decisions the imap type makes — and keeps its
// own send gate: the send tools exist only on connections with allow_send.

// nylasSentMeansDone closes every send tool's description: the rule must
// reach the agent also when the connection's own instructions replace the
// defaults.
const nylasSentMeansDone = "sent=true means Nylas accepted the message — never send it again, even when warnings follow."

// sendTools are registered only on connections that may send.
func (n *Nylas) sendTools() []ToolDef {
	return []ToolDef{
		{
			Tool: mcp.NewTool("send_draft",
				mcp.WithDescription("Send a draft. The email goes out immediately and cannot be recalled; Nylas removes the draft and files the message in Sent. "+nylasSentMeansDone),
				mcp.WithString("draft_id", mcp.Required(), mcp.Description("Draft ID as returned by create_reply_draft or create_forward_draft")),
				mcp.WithDestructiveHintAnnotation(true),
				mcp.WithOpenWorldHintAnnotation(true),
			),
			Handler: n.handleSendDraft,
		},
		{
			Tool: mcp.NewTool("reply_send",
				mcp.WithDescription("Reply to a message of a conversation and send the reply immediately — no draft, cannot be recalled. The reply goes to that message's Reply-To (its sender when it has none) and is threaded on it. "+nylasSentMeansDone),
				mcp.WithString("conversation_id", mcp.Required(), mcp.Description("Conversation ID")),
				mcp.WithString("message_id", mcp.Required(), mcp.Description(nylasMessageIDDescription)),
				mcp.WithString("body", mcp.Required(), mcp.Description("Reply text (plain text)")),
				mcp.WithBoolean("reply_all", mcp.Description("Also address everyone else on the message's To and Cc, except this account (default: false)")),
				mcp.WithOpenWorldHintAnnotation(true),
			),
			Handler: n.handleReplySend,
		},
		{
			Tool: mcp.NewTool("forward_send",
				mcp.WithDescription("Forward a message of a conversation and send it immediately — no draft, cannot be recalled. The message's attachments are forwarded with it unless include_attachments is false. "+nylasSentMeansDone),
				mcp.WithString("conversation_id", mcp.Required(), mcp.Description("Conversation ID")),
				mcp.WithString("message_id", mcp.Required(), mcp.Description(nylasMessageIDDescription)),
				mcp.WithString("to", mcp.Required(), mcp.Description("Recipient email address(es), comma-separated for multiple")),
				mcp.WithString("body", mcp.Description("Optional comment to include above the forwarded message")),
				mcp.WithBoolean("include_attachments", mcp.Description("Forward the message's attachments (default: true)")),
				mcp.WithOpenWorldHintAnnotation(true),
			),
			Handler: n.handleForwardSend,
		},
	}
}

// nylasSendToolNames are the registered names of the send tools of the
// connection — removed before every registration so switching allow_send off
// takes effect without a restart.
func nylasSendToolNames(connName string) []string {
	names := make([]string, 0, 3)
	for _, t := range []string{"send_draft", "reply_send", "forward_send"} {
		names = append(names, config.SanitizeToolName(connName+"_"+t))
	}
	return names
}

// nylasOutgoing is a message before it goes to the API: a draft or a message
// to send. Nylas builds the MIME message from it.
type nylasOutgoing struct {
	To, Cc           []nylasAddress
	Subject          string
	Body             string
	ReplyToMessageID string            // threads the message on the one it answers
	Attachments      []nylasAttachment // forwarded by reference to the original message's attachments
}

// payload is the JSON body for POST /drafts and POST /messages/send.
func (n *Nylas) payload(m nylasOutgoing) map[string]any {
	body := map[string]any{
		"to":           m.To,
		"subject":      m.Subject,
		"body":         m.Body,
		"is_plaintext": true,
	}
	if len(m.Cc) > 0 {
		body["cc"] = m.Cc
	}
	if m.ReplyToMessageID != "" {
		body["reply_to_message_id"] = m.ReplyToMessageID
	}
	if len(m.Attachments) > 0 {
		body["attachments"] = m.Attachments
	}
	if _, me, _ := n.grant(); n.fromName != "" && me != "" {
		body["from"] = []nylasAddress{{Name: n.fromName, Email: me}}
	}
	return body
}

// replyRecipients picks the addresses a reply to m goes to: its Reply-To
// (Nylas reports the sender there when the header is absent), and with all
// also everyone else on its To and Cc. The account's own address is never a
// recipient; a reply to a message the account sent itself answers that
// message's recipients.
func (n *Nylas) replyRecipients(m nylasMessage, all bool) (to, cc []nylasAddress) {
	_, me, _ := n.grant()
	seen := map[string]bool{}
	if me != "" {
		seen[me] = true
	}
	add := func(list *[]nylasAddress, addrs ...nylasAddress) {
		for _, a := range addrs {
			e := strings.TrimSpace(a.Email)
			key := strings.ToLower(e)
			if e == "" || seen[key] {
				continue
			}
			seen[key] = true
			*list = append(*list, nylasAddress{Name: a.Name, Email: e})
		}
	}
	targets := m.ReplyTo
	if len(targets) == 0 {
		targets = m.From
	}
	add(&to, targets...)
	if len(to) == 0 && len(m.From) > 0 && n.isMe(m.From[0].Email) {
		add(&to, m.To...) // a message the account sent itself
	}
	if all {
		add(&cc, m.To...)
		add(&cc, m.CC...)
	}
	return to, cc
}

// prepareReply builds the reply to a message of a conversation — the one
// with the given id, the latest when messageID is empty.
func (n *Nylas) prepareReply(ctx context.Context, convID, body string, replyAll bool, messageID string) (nylasOutgoing, error) {
	msgs, err := n.threadMessages(ctx, convID, "")
	if err != nil {
		return nylasOutgoing{}, err
	}
	idx, err := pickNylasMessage(msgs, convID, messageID)
	if err != nil {
		return nylasOutgoing{}, err
	}
	target := msgs[idx]
	subject := target.Subject
	if !strings.HasPrefix(strings.ToLower(subject), "re:") {
		subject = "Re: " + subject
	}
	to, cc := n.replyRecipients(target, replyAll)
	if len(to) == 0 {
		return nylasOutgoing{}, fmt.Errorf("message %s of conversation %s names no address to reply to", target.ID, convID)
	}
	return nylasOutgoing{To: to, Cc: cc, Subject: subject, Body: body, ReplyToMessageID: target.ID}, nil
}

// prepareForward builds a forward of a message of a conversation — the one
// with the given id, the latest when messageID is empty — to one or more
// addresses (comma-separated, display names allowed). The original's
// attachments ride along by reference when includeAttachments is set.
func (n *Nylas) prepareForward(ctx context.Context, convID, to, comment, messageID string, includeAttachments bool) (nylasOutgoing, error) {
	list, err := mail.ParseAddressList(to)
	if err != nil {
		return nylasOutgoing{}, fmt.Errorf("invalid recipient %q: %v", to, err)
	}
	var addrs []nylasAddress
	for _, a := range list {
		addrs = append(addrs, nylasAddress{Name: a.Name, Email: a.Address})
	}
	if len(addrs) == 0 {
		return nylasOutgoing{}, fmt.Errorf("no recipient in %q", to)
	}
	msgs, err := n.threadMessages(ctx, convID, "")
	if err != nil {
		return nylasOutgoing{}, err
	}
	idx, err := pickNylasMessage(msgs, convID, messageID)
	if err != nil {
		return nylasOutgoing{}, err
	}
	orig := msgs[idx]
	subject := orig.Subject
	if !strings.HasPrefix(strings.ToLower(subject), "fwd:") {
		subject = "Fwd: " + subject
	}
	from := ""
	if len(orig.From) > 0 {
		from = orig.From[0].Email
		if orig.From[0].Name != "" {
			from = orig.From[0].Name + " <" + orig.From[0].Email + ">"
		}
	}
	var b strings.Builder
	if comment != "" {
		b.WriteString(comment + "\n\n")
	}
	b.WriteString("---------- Forwarded message ----------\n")
	fmt.Fprintf(&b, "From: %s\nDate: %s\nSubject: %s\nTo: %s\n\n",
		from, time.Unix(orig.Date, 0).UTC().Format(time.RFC1123Z), orig.Subject, strings.Join(nylasAddresses(orig.To), ", "))
	b.WriteString(nylasBodyText(orig.Body))
	out := nylasOutgoing{To: addrs, Subject: subject, Body: b.String()}
	if includeAttachments {
		for _, a := range orig.Attachments {
			if a.IsInline {
				continue // inline images belong to the original's HTML, which the plain-text forward does not carry
			}
			out.Attachments = append(out.Attachments, a)
		}
	}
	return out, nil
}

// draftResult is what create_*_draft returns.
func (n *Nylas) draftResult(ctx context.Context, m nylasOutgoing, extra map[string]any) (*mcp.CallToolResult, error) {
	grantID, _, _ := n.grant()
	var draft struct {
		ID       string   `json:"id"`
		ThreadID string   `json:"thread_id"`
		Folders  []string `json:"folders"`
	}
	if _, err := n.callJSON(ctx, "drafts.create", map[string]string{"grant_id": grantID}, nil, n.payload(m), &draft); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("create draft: %v", err)), nil
	}
	folder := "Drafts"
	if folders, err := n.listFolders(ctx, false); err == nil && len(draft.Folders) > 0 {
		if names := folderNames(folders, draft.Folders); len(names) > 0 {
			folder = names[0]
		}
	}
	result := map[string]any{
		"status":   "draft_created",
		"draft_id": draft.ID,
		"folder":   folder,
		"to":       nylasAddresses(m.To),
		"subject":  m.Subject,
	}
	if len(m.Cc) > 0 {
		result["cc"] = nylasAddresses(m.Cc)
	}
	for k, v := range extra {
		result[k] = v
	}
	return jsonResult(result)
}

func (n *Nylas) handleCreateReplyDraft(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	convID, _ := req.RequireString("conversation_id")
	body, _ := req.RequireString("body")
	if convID == "" || body == "" {
		return mcp.NewToolResultError("conversation_id and body are required"), nil
	}
	if err := n.ensureGrant(ctx); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	m, err := n.prepareReply(ctx, convID, body, req.GetBool("reply_all", false), strings.TrimSpace(req.GetString("message_id", "")))
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return n.draftResult(ctx, m, map[string]any{"reply_to": strings.Join(nylasAddresses(m.To), ", "), "in_reply_to": m.ReplyToMessageID})
}

func (n *Nylas) handleCreateForwardDraft(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	convID, _ := req.RequireString("conversation_id")
	to, _ := req.RequireString("to")
	if convID == "" || to == "" {
		return mcp.NewToolResultError("conversation_id and to are required"), nil
	}
	if err := n.ensureGrant(ctx); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	m, err := n.prepareForward(ctx, convID, to, req.GetString("body", ""), strings.TrimSpace(req.GetString("message_id", "")), req.GetBool("include_attachments", true))
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return n.draftResult(ctx, m, map[string]any{"attachments": len(m.Attachments)})
}

// --- Sending ---

// sentMessage is the part of Nylas' send response the result reports.
type sentMessage struct {
	ID       string         `json:"id"`
	ThreadID string         `json:"thread_id"`
	Subject  string         `json:"subject"`
	To       []nylasAddress `json:"to"`
	CC       []nylasAddress `json:"cc"`
	Date     int64          `json:"date"`
}

// sendError shapes a failed send. A request that produced no HTTP status may
// still have been carried out: that is said so, because an agent that reads
// a plain error sends again.
func sendError(what string, err error) *mcp.CallToolResult {
	var transport *nylasTransportError
	if errors.As(err, &transport) {
		return mcp.NewToolResultError(fmt.Sprintf("%s: unclear whether sent — the request failed without an answer from Nylas (%v); check the Sent folder before trying again", what, err))
	}
	return mcp.NewToolResultError(fmt.Sprintf("%s: %v", what, err))
}

func (n *Nylas) handleSendDraft(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	draftID := strings.TrimSpace(req.GetString("draft_id", ""))
	if draftID == "" {
		return mcp.NewToolResultError("draft_id is required"), nil
	}
	if err := n.ensureGrant(ctx); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	grantID, _, _ := n.grant()
	var sent sentMessage
	if _, err := n.callJSON(ctx, "draft.send", map[string]string{"grant_id": grantID, "draft_id": draftID}, nil, map[string]any{}, &sent); err != nil {
		var httpErr *nylasHTTPError
		if errors.As(err, &httpErr) && httpErr.Status == 404 {
			return mcp.NewToolResultError(fmt.Sprintf("draft %s not found — it may have been sent or deleted already; nothing was sent now", draftID)), nil
		}
		return sendError("send draft "+draftID, err), nil
	}
	return jsonResult(map[string]any{
		"sent":       true,
		"draft_id":   draftID,
		"message_id": sent.ID,
		"to":         nylasAddresses(sent.To),
		"cc":         nylasAddresses(sent.CC),
		"subject":    sent.Subject,
		"warnings":   []string{},
	})
}

// sendComposed submits m through POST /messages/send.
func (n *Nylas) sendComposed(ctx context.Context, m nylasOutgoing) (*mcp.CallToolResult, error) {
	grantID, _, _ := n.grant()
	var sent sentMessage
	if _, err := n.callJSON(ctx, "messages.send", map[string]string{"grant_id": grantID}, nil, n.payload(m), &sent); err != nil {
		return sendError("send", err), nil
	}
	messageID := sent.ID
	return jsonResult(map[string]any{
		"sent":       true,
		"message_id": messageID,
		"to":         nylasAddresses(m.To),
		"cc":         nylasAddresses(m.Cc),
		"subject":    m.Subject,
		"warnings":   []string{},
	})
}

func (n *Nylas) handleReplySend(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	convID, _ := req.RequireString("conversation_id")
	body, _ := req.RequireString("body")
	messageID := strings.TrimSpace(req.GetString("message_id", ""))
	if convID == "" || body == "" {
		return mcp.NewToolResultError("conversation_id and body are required"), nil
	}
	if messageID == "" {
		return mcp.NewToolResultError("message_id is required: the id of the message to answer, from list_conversations or get_conversation — a message sent at once needs a fixed target"), nil
	}
	if err := n.ensureGrant(ctx); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	m, err := n.prepareReply(ctx, convID, body, req.GetBool("reply_all", false), messageID)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return n.sendComposed(ctx, m)
}

func (n *Nylas) handleForwardSend(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	convID, _ := req.RequireString("conversation_id")
	to, _ := req.RequireString("to")
	messageID := strings.TrimSpace(req.GetString("message_id", ""))
	if convID == "" || to == "" {
		return mcp.NewToolResultError("conversation_id and to are required"), nil
	}
	if messageID == "" {
		return mcp.NewToolResultError("message_id is required: the id of the message to forward, from list_conversations or get_conversation — a message sent at once needs a fixed target"), nil
	}
	if err := n.ensureGrant(ctx); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	m, err := n.prepareForward(ctx, convID, to, req.GetString("body", ""), messageID, req.GetBool("include_attachments", true))
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return n.sendComposed(ctx, m)
}
