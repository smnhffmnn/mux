package tools

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"slices"
	"strconv"
	"strings"
	"time"

	imaplib "github.com/emersion/go-imap"
	imapclient "github.com/emersion/go-imap/client"
	"github.com/mark3labs/mcp-go/mcp"

	"github.com/smnhffmnn/mux/internal/config"
)

// smtpTimeout bounds one SMTP session (dial, TLS, login, transmission) when
// the caller's context sets no earlier deadline.
const smtpTimeout = 2 * time.Minute

// sendTools are registered only on connections that may send.
func (im *IMAP) sendTools() []ToolDef {
	return []ToolDef{
		{
			Tool: mcp.NewTool("send_draft",
				mcp.WithDescription("Send a draft from the Drafts folder over SMTP. The email goes out immediately and cannot be recalled. Recipients are the draft's To, Cc and Bcc; Bcc is not transmitted. On success the draft is removed and a copy is stored in Sent."),
				mcp.WithString("draft_id", mcp.Required(), mcp.Description("Message-ID of the draft, as returned by create_reply_draft or create_forward_draft")),
				mcp.WithDestructiveHintAnnotation(true),
				mcp.WithOpenWorldHintAnnotation(true),
			),
			Handler: im.handleSendDraft,
		},
		{
			Tool: mcp.NewTool("reply_send",
				mcp.WithDescription("Reply to the latest message in an IMAP conversation and send the reply immediately over SMTP — no draft, cannot be recalled. Addressed like create_reply_draft; a copy is stored in Sent."),
				mcp.WithString("conversation_id", mcp.Required(), mcp.Description("Conversation ID")),
				mcp.WithString("body", mcp.Required(), mcp.Description("Reply text (plain text)")),
				mcp.WithBoolean("reply_all", mcp.Description("Also address everyone else on the message's To and Cc, except this account (default: false)")),
				mcp.WithOpenWorldHintAnnotation(true),
			),
			Handler: im.handleReplySend,
		},
		{
			Tool: mcp.NewTool("forward_send",
				mcp.WithDescription("Forward the latest message in an IMAP conversation and send it immediately over SMTP — no draft, cannot be recalled. A copy is stored in Sent."),
				mcp.WithString("conversation_id", mcp.Required(), mcp.Description("Conversation ID")),
				mcp.WithString("to", mcp.Required(), mcp.Description("Recipient email address(es), comma-separated for multiple")),
				mcp.WithString("body", mcp.Description("Optional comment to include above the forwarded message")),
				mcp.WithOpenWorldHintAnnotation(true),
			),
			Handler: im.handleForwardSend,
		},
	}
}

// --- Handlers ---

func (im *IMAP) handleSendDraft(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	draftID := normalizeMessageID(req.GetString("draft_id", ""))
	if draftID == "" {
		return mcp.NewToolResultError("draft_id is required"), nil
	}

	return im.withClient(ctx, func(c *imapclient.Client) (*mcp.CallToolResult, error) {
		folder := findSpecialFolder(c, specialUseDrafts, draftsFolderNames...)
		if folder == "" {
			return mcp.NewToolResultError("Drafts folder not found"), nil
		}
		if _, err := c.Select(folder, false); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("SELECT %s: %v", folder, err)), nil
		}

		uid, raw, err := fetchDraft(c, draftID)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("%v in %s", err, folder)), nil
		}
		out, err := prepareDraftForSending(raw, time.Now())
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		if err := im.submit(ctx, out.recipients, out.transmit); err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		// Sent from here on: what fails now is reported as a warning, never as
		// an error an agent might answer by sending again.
		var warnings []string
		sentCopy, warning := im.saveSentCopy(c, out.sentCopy)
		if warning != "" {
			warnings = append(warnings, warning)
		}
		seqSet := new(imaplib.SeqSet)
		seqSet.AddNum(uid)
		if err := removeUIDs(c, seqSet); err != nil {
			warnings = append(warnings, fmt.Sprintf("sent, but removing the draft from %s failed: %v", folder, err))
		}

		return imapJSONResult(map[string]any{
			"sent":       true,
			"draft_id":   draftID,
			"recipients": out.recipients,
			"sent_copy":  sentCopy,
			"warnings":   warnings,
		})
	})
}

func (im *IMAP) handleReplySend(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	convID, _ := req.RequireString("conversation_id")
	body, _ := req.RequireString("body")
	if convID == "" || body == "" {
		return mcp.NewToolResultError("conversation_id and body are required"), nil
	}
	replyAll := req.GetBool("reply_all", false)

	return im.withClient(ctx, func(c *imapclient.Client) (*mcp.CallToolResult, error) {
		m, err := im.prepareReply(c, convID, body, replyAll)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return im.sendComposed(ctx, c, m)
	})
}

func (im *IMAP) handleForwardSend(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	convID, _ := req.RequireString("conversation_id")
	to, _ := req.RequireString("to")
	if convID == "" || to == "" {
		return mcp.NewToolResultError("conversation_id and to are required"), nil
	}
	comment := req.GetString("body", "")

	return im.withClient(ctx, func(c *imapclient.Client) (*mcp.CallToolResult, error) {
		m, err := im.prepareForward(c, convID, to, comment)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return im.sendComposed(ctx, c, m)
	})
}

// sendComposed encodes m, submits it and stores the Sent copy.
func (im *IMAP) sendComposed(ctx context.Context, c *imapclient.Client, m outgoingMail) (*mcp.CallToolResult, error) {
	raw, msgID := im.compose(m)
	if err := im.submit(ctx, m.recipients(), raw); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	var warnings []string
	sentCopy, warning := im.saveSentCopy(c, raw)
	if warning != "" {
		warnings = append(warnings, warning)
	}
	return imapJSONResult(map[string]any{
		"sent":       true,
		"message_id": msgID,
		"to":         m.To,
		"cc":         m.Cc,
		"subject":    m.Subject,
		"sent_copy":  sentCopy,
		"warnings":   warnings,
	})
}

// --- Drafts ---

// fetchDraft finds the draft with the given Message-ID in the selected
// mailbox and returns its UID and raw message. Exactly one draft must match:
// two drafts with one Message-ID (a client that copied it) leave the choice
// of what to send open, so neither is sent.
func fetchDraft(c *imapclient.Client, messageID string) (uint32, []byte, error) {
	criteria := imaplib.NewSearchCriteria()
	criteria.Header.Add("Message-Id", messageID)
	uids, err := c.UidSearch(criteria)
	if err != nil {
		return 0, nil, fmt.Errorf("SEARCH: %w", err)
	}

	// SEARCH HEADER is a substring match; keep the drafts whose Message-ID is
	// exactly the one asked for.
	section := &imaplib.BodySectionName{Peek: true}
	var (
		matches []uint32
		raw     []byte
	)
	if len(uids) > 0 {
		seqSet := new(imaplib.SeqSet)
		seqSet.AddNum(uids...)
		messages := make(chan *imaplib.Message, len(uids))
		done := make(chan error, 1)
		go func() {
			done <- c.UidFetch(seqSet, []imaplib.FetchItem{section.FetchItem(), imaplib.FetchUid}, messages)
		}()
		var readErr error
		for msg := range messages {
			r := msg.GetBody(section)
			if r == nil {
				continue
			}
			data, err := io.ReadAll(r)
			if err != nil {
				readErr = err
				continue
			}
			parsed, err := mail.ReadMessage(bytes.NewReader(data))
			if err != nil || normalizeMessageID(parsed.Header.Get("Message-Id")) != messageID {
				continue
			}
			matches = append(matches, msg.Uid)
			raw = data
		}
		if err := <-done; err != nil {
			return 0, nil, fmt.Errorf("UID FETCH: %w", err)
		}
		if readErr != nil {
			return 0, nil, fmt.Errorf("reading the draft: %w", readErr)
		}
	}

	switch len(matches) {
	case 0:
		return 0, nil, fmt.Errorf("no draft with Message-ID %s (already sent or deleted?)", messageID)
	case 1:
		return matches[0], raw, nil
	default:
		return 0, nil, fmt.Errorf("%d drafts carry Message-ID %s — remove the copies, then send again", len(matches), messageID)
	}
}

// draftOut is a draft made ready for submission.
type draftOut struct {
	transmit   []byte   // what goes to the SMTP server: no Bcc header
	sentCopy   []byte   // the copy for Sent: keeps Bcc, so the sender sees it
	recipients []string // To, Cc and Bcc addresses, deduplicated
}

// prepareDraftForSending takes the recipients from the draft's To, Cc and Bcc
// headers, sets Date to the moment of sending and drops Bcc from what is
// transmitted. Everything else — the body, the threading headers, whatever
// another client added — stays as the draft has it.
func prepareDraftForSending(raw []byte, now time.Time) (draftOut, error) {
	sep := bytes.Index(raw, []byte("\r\n\r\n"))
	if sep < 0 {
		return draftOut{}, fmt.Errorf("the draft has no header/body separator")
	}
	head, body := raw[:sep], raw[sep+4:]

	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return draftOut{}, fmt.Errorf("parsing the draft: %w", err)
	}
	var recipients []string
	seen := map[string]bool{}
	for _, field := range []string{"To", "Cc", "Bcc"} {
		list, err := msg.Header.AddressList(field)
		if errors.Is(err, mail.ErrHeaderNotPresent) {
			continue
		}
		if err != nil {
			return draftOut{}, fmt.Errorf("draft header %s: %w", field, err)
		}
		for _, a := range list {
			key := strings.ToLower(a.Address)
			if !seen[key] {
				seen[key] = true
				recipients = append(recipients, a.Address)
			}
		}
	}
	if len(recipients) == 0 {
		return draftOut{}, fmt.Errorf("the draft has no recipients (To, Cc or Bcc)")
	}

	date := "Date: " + now.Format(time.RFC1123Z)
	var transmit, sentCopy bytes.Buffer
	hasDate := false
	for _, field := range headerFields(head) {
		switch strings.ToLower(field.name) {
		case "date":
			hasDate = true
			transmit.WriteString(date + "\r\n")
			sentCopy.WriteString(date + "\r\n")
		case "bcc":
			sentCopy.Write(field.raw)
		default:
			transmit.Write(field.raw)
			sentCopy.Write(field.raw)
		}
	}
	if !hasDate {
		transmit.WriteString(date + "\r\n")
		sentCopy.WriteString(date + "\r\n")
	}
	for _, buf := range []*bytes.Buffer{&transmit, &sentCopy} {
		buf.WriteString("\r\n")
		buf.Write(body)
	}
	return draftOut{transmit: transmit.Bytes(), sentCopy: sentCopy.Bytes(), recipients: recipients}, nil
}

// headerField is one header field with its folded continuation lines, raw
// and CRLF-terminated.
type headerField struct {
	name string
	raw  []byte
}

// headerFields splits a header block (without the blank line that ends it)
// into its fields.
func headerFields(head []byte) []headerField {
	var fields []headerField
	for _, line := range bytes.SplitAfter(append(append([]byte{}, head...), '\r', '\n'), []byte("\r\n")) {
		if len(line) == 0 {
			continue
		}
		if (line[0] == ' ' || line[0] == '\t') && len(fields) > 0 {
			last := &fields[len(fields)-1]
			last.raw = append(last.raw, line...)
			continue
		}
		name := string(line)
		if i := strings.IndexByte(name, ':'); i >= 0 {
			name = name[:i]
		}
		fields = append(fields, headerField{name: strings.TrimSpace(name), raw: append([]byte{}, line...)})
	}
	return fields
}

// saveSentCopy stores raw in the Sent folder, unless the provider files
// submitted mail there itself. It runs after the message went out, so it
// returns a warning instead of an error.
func (im *IMAP) saveSentCopy(c *imapclient.Client, raw []byte) (folder, warning string) {
	if im.smtp.savesSent {
		return "", ""
	}
	folder = findSpecialFolder(c, specialUseSent, sentFolderNames...)
	if folder == "" {
		return "", "sent, but no Sent folder was found — no copy stored"
	}
	if err := c.Append(folder, []string{imaplib.SeenFlag}, time.Now(), newLiteral(raw)); err != nil {
		return "", fmt.Sprintf("sent, but storing the copy in %s failed: %v", folder, err)
	}
	return folder, ""
}

// --- SMTP ---

// submit hands msg to the SMTP server for the given recipients. Until the
// server has the complete message nothing is sent; an error after that point
// — the connection lost while waiting for the final reply — leaves open
// whether the server accepted it, and says so.
func (im *IMAP) submit(ctx context.Context, recipients []string, msg []byte) error {
	c, err := im.dialSMTP(ctx)
	if err != nil {
		return err
	}
	defer c.close()

	if needs8BitMIME(msg) {
		if ok, _ := c.Extension("8BITMIME"); !ok {
			return fmt.Errorf("not sent: the message contains 8-bit text and the SMTP server does not offer 8BITMIME")
		}
	}
	if ok, param := c.Extension("SIZE"); ok {
		if limit, err := strconv.Atoi(strings.TrimSpace(param)); err == nil && limit > 0 && len(msg) > limit {
			return fmt.Errorf("not sent: the message is %d bytes, the SMTP server accepts at most %d", len(msg), limit)
		}
	}

	if err := c.Mail(im.user); err != nil {
		return fmt.Errorf("not sent: MAIL FROM %s: %w", im.user, err)
	}
	for _, r := range recipients {
		if err := c.Rcpt(r); err != nil {
			return fmt.Errorf("not sent: recipient %s refused: %w", r, err)
		}
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("not sent: DATA: %w", err)
	}
	if _, err := w.Write(msg); err != nil {
		return fmt.Errorf("not sent: transmitting the message: %w", err)
	}
	if err := w.Close(); err != nil {
		var refused *textproto.Error
		if errors.As(err, &refused) {
			return fmt.Errorf("not sent: the SMTP server refused the message: %w", err)
		}
		return fmt.Errorf("unclear whether sent: the connection failed after the message was transmitted (%v) — check the recipient or the Sent folder before sending again", err)
	}
	_ = c.Quit()
	return nil
}

// smtpSession is a signed-in SMTP client bound to the caller's context.
type smtpSession struct {
	*smtp.Client
	release func() bool // unbinds the context
}

// close ends the session; after Quit it only releases the context binding.
func (s *smtpSession) close() {
	s.release()
	_ = s.Client.Close()
}

// dialSMTP connects to the submission server — through the connection's
// tunnel if it has one —, secures the session (implicit TLS on port 465,
// STARTTLS on any other port; the password is never sent unencrypted) and
// signs in with the IMAP credentials. The caller closes the session.
func (im *IMAP) dialSMTP(ctx context.Context) (*smtpSession, error) {
	s := im.smtp
	addr := net.JoinHostPort(s.host, strconv.Itoa(s.port))

	var (
		conn net.Conn
		err  error
	)
	if im.dialer != nil {
		conn, err = im.dialer.DialContext(ctx, "tcp", addr)
	} else {
		d := &net.Dialer{}
		conn, err = d.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return nil, fmt.Errorf("SMTP dial %s: %w", addr, err)
	}
	// net/smtp has no context: the deadline bounds the session, and a
	// cancelled call closes the connection under it.
	deadline := time.Now().Add(smtpTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = conn.SetDeadline(deadline)
	release := context.AfterFunc(ctx, func() { conn.Close() })
	fail := func(err error) (*smtpSession, error) {
		release()
		conn.Close()
		return nil, err
	}

	if s.implicitTLS {
		tlsConn := tls.Client(conn, im.tlsConfig(s.host))
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			return fail(fmt.Errorf("SMTP TLS handshake with %s: %w", addr, err))
		}
		conn = tlsConn
	}

	c, err := smtp.NewClient(conn, s.host)
	if err != nil {
		return fail(fmt.Errorf("SMTP greeting from %s: %w", addr, err))
	}
	if !s.implicitTLS {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return fail(fmt.Errorf("SMTP server %s offers no STARTTLS — mux does not send the password unencrypted (use port %d for implicit TLS)", addr, config.ImplicitTLSSMTPPort))
		}
		if err := c.StartTLS(im.tlsConfig(s.host)); err != nil {
			return fail(fmt.Errorf("SMTP STARTTLS with %s: %w", addr, err))
		}
	}

	auth, err := smtpAuth(c, im.user, im.password, s.host)
	if err != nil {
		return fail(err)
	}
	if err := c.Auth(auth); err != nil {
		return fail(fmt.Errorf("SMTP login as %s: %w", im.user, err))
	}
	return &smtpSession{Client: c, release: release}, nil
}

// smtpAuth picks PLAIN, else LOGIN, from the mechanisms the server offers.
func smtpAuth(c *smtp.Client, user, password, host string) (smtp.Auth, error) {
	ok, mechs := c.Extension("AUTH")
	if !ok {
		return nil, fmt.Errorf("SMTP server %s offers no AUTH", host)
	}
	offered := strings.Fields(strings.ToUpper(mechs))
	switch {
	case slices.Contains(offered, "PLAIN"):
		return smtp.PlainAuth("", user, password, host), nil
	case slices.Contains(offered, "LOGIN"):
		return &loginAuth{user: user, password: password}, nil
	}
	return nil, fmt.Errorf("SMTP server %s offers neither AUTH PLAIN nor LOGIN (it offers %s)", host, mechs)
}

// loginAuth is the LOGIN mechanism, which some servers offer instead of
// PLAIN. Like smtp.PlainAuth it refuses an unencrypted session.
type loginAuth struct {
	user, password string
}

func (a *loginAuth) Start(server *smtp.ServerInfo) (string, []byte, error) {
	if !server.TLS {
		return "", nil, errors.New("unencrypted connection")
	}
	return "LOGIN", nil, nil
}

func (a *loginAuth) Next(fromServer []byte, more bool) ([]byte, error) {
	if !more {
		return nil, nil
	}
	switch strings.ToLower(strings.TrimSpace(string(fromServer))) {
	case "username:", "username", "user name":
		return []byte(a.user), nil
	case "password:", "password":
		return []byte(a.password), nil
	}
	return nil, fmt.Errorf("unexpected LOGIN challenge %q", fromServer)
}

// needs8BitMIME reports whether msg carries bytes outside 7-bit ASCII — a
// draft another client wrote with 8bit encoding. Messages composed by mux
// never do.
func needs8BitMIME(msg []byte) bool {
	for _, b := range msg {
		if b >= 0x80 {
			return true
		}
	}
	return false
}
