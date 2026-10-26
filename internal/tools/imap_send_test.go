package tools

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/textproto"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	imaplib "github.com/emersion/go-imap"
	"github.com/emersion/go-imap/backend"
	"github.com/emersion/go-imap/backend/memory"
	"github.com/emersion/go-imap/commands"
	imapserver "github.com/emersion/go-imap/server"
	"github.com/mark3labs/mcp-go/mcp"

	"github.com/smnhffmnn/mux/internal/config"
)

// The tests below run the IMAP connection against go-imap's own server with
// its in-memory backend and against a small SMTP server that records what it
// receives. Both speak TLS with a certificate made for the test.

const (
	testAccount  = "me@example.com"
	testPassword = "secret"
)

// nul is the separator of an AUTH PLAIN response, written without an escape
// sequence so no tool turns it into a raw byte in this file.
var nul = string(rune(0))

// --- TLS ---

func testCertificate(t *testing.T) (*tls.Config, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "mux test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}}}, pool
}

// --- IMAP server ---

// testBackend admits the test account and marks mailboxes with special-use
// attributes, which the memory backend does not know.
type testBackend struct {
	mem      *memory.Backend
	specials map[string]string // mailbox name → attribute
}

func (b *testBackend) Login(ci *imaplib.ConnInfo, user, pass string) (backend.User, error) {
	if user != testAccount || pass != testPassword {
		return nil, errors.New("bad credentials")
	}
	u, err := b.mem.Login(ci, "username", "password")
	if err != nil {
		return nil, err
	}
	return &specialUseUser{User: u, specials: b.specials}, nil
}

type specialUseUser struct {
	backend.User
	specials map[string]string
}

func (u *specialUseUser) ListMailboxes(subscribed bool) ([]backend.Mailbox, error) {
	boxes, err := u.User.ListMailboxes(subscribed)
	for i, mb := range boxes {
		if attr, ok := u.specials[mb.Name()]; ok {
			boxes[i] = &specialUseMailbox{Mailbox: mb, attr: attr}
		}
	}
	return boxes, err
}

type specialUseMailbox struct {
	backend.Mailbox
	attr string
}

func (m *specialUseMailbox) Info() (*imaplib.MailboxInfo, error) {
	info, err := m.Mailbox.Info()
	if err == nil {
		info.Attributes = append(info.Attributes, m.attr)
	}
	return info, err
}

// --- SMTP server ---

type smtpDelivery struct {
	from  string
	rcpts []string
	data  string // dot-decoded, line endings as LF
}

// fakeSMTP is a submission server with the switches the tests need.
type fakeSMTP struct {
	ln          net.Listener
	tlsConfig   *tls.Config
	implicitTLS bool
	noSTARTTLS  bool
	no8BitMIME  bool
	authMechs   string // offered after TLS, default "PLAIN LOGIN"
	refuseData  bool   // answer the end of DATA with 554
	dropOnData  bool   // close the connection instead of answering the end of DATA

	mu         sync.Mutex
	deliveries []smtpDelivery
	logins     int
}

func startFakeSMTP(t *testing.T, cfg *tls.Config, configure func(*fakeSMTP)) *fakeSMTP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeSMTP{ln: ln, tlsConfig: cfg, authMechs: "PLAIN LOGIN"}
	if configure != nil {
		configure(f)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(conn)
		}
	}()
	return f
}

func (f *fakeSMTP) port() int { return f.ln.Addr().(*net.TCPAddr).Port }

func (f *fakeSMTP) got() []smtpDelivery {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]smtpDelivery{}, f.deliveries...)
}

func (f *fakeSMTP) serve(conn net.Conn) {
	defer func() { conn.Close() }()
	secure := false
	if f.implicitTLS {
		conn = tls.Server(conn, f.tlsConfig)
		secure = true
	}
	tp := textproto.NewConn(conn)
	_ = tp.PrintfLine("220 fake ESMTP")

	authed := false
	var cur smtpDelivery
	for {
		line, err := tp.ReadLine()
		if err != nil {
			return
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			_ = tp.PrintfLine("500 empty command")
			continue
		}
		switch strings.ToUpper(fields[0]) {
		case "EHLO", "HELO":
			lines := []string{"fake"}
			if !secure && !f.noSTARTTLS {
				lines = append(lines, "STARTTLS")
			}
			if secure {
				lines = append(lines, "AUTH "+f.authMechs)
			}
			if !f.no8BitMIME {
				lines = append(lines, "8BITMIME")
			}
			lines = append(lines, "SIZE 1000000")
			for i, l := range lines {
				sep := "-"
				if i == len(lines)-1 {
					sep = " "
				}
				_ = tp.PrintfLine("250%s%s", sep, l)
			}
		case "STARTTLS":
			_ = tp.PrintfLine("220 go ahead")
			tlsConn := tls.Server(conn, f.tlsConfig)
			if err := tlsConn.Handshake(); err != nil {
				return
			}
			conn = tlsConn
			tp = textproto.NewConn(conn)
			secure = true
		case "AUTH":
			if len(fields) < 2 {
				_ = tp.PrintfLine("501 mechanism missing")
				continue
			}
			var user, pass string
			switch strings.ToUpper(fields[1]) {
			case "PLAIN":
				resp := ""
				if len(fields) > 2 {
					resp = fields[2]
				} else {
					_ = tp.PrintfLine("334 ")
					resp, _ = tp.ReadLine()
				}
				dec, _ := base64.StdEncoding.DecodeString(resp)
				if parts := strings.Split(string(dec), nul); len(parts) == 3 {
					user, pass = parts[1], parts[2]
				}
			case "LOGIN":
				_ = tp.PrintfLine("334 %s", base64.StdEncoding.EncodeToString([]byte("Username:")))
				u, _ := tp.ReadLine()
				_ = tp.PrintfLine("334 %s", base64.StdEncoding.EncodeToString([]byte("Password:")))
				p, _ := tp.ReadLine()
				du, _ := base64.StdEncoding.DecodeString(u)
				dp, _ := base64.StdEncoding.DecodeString(p)
				user, pass = string(du), string(dp)
			default:
				_ = tp.PrintfLine("504 unsupported mechanism")
				continue
			}
			if user == testAccount && pass == testPassword {
				authed = true
				f.mu.Lock()
				f.logins++
				f.mu.Unlock()
				_ = tp.PrintfLine("235 authenticated")
			} else {
				_ = tp.PrintfLine("535 bad credentials")
			}
		case "MAIL":
			if !authed {
				_ = tp.PrintfLine("530 authentication required")
				continue
			}
			cur = smtpDelivery{from: between(line, "<", ">")}
			_ = tp.PrintfLine("250 ok")
		case "RCPT":
			cur.rcpts = append(cur.rcpts, between(line, "<", ">"))
			_ = tp.PrintfLine("250 ok")
		case "DATA":
			_ = tp.PrintfLine("354 end with <CRLF>.<CRLF>")
			data, err := tp.ReadDotBytes()
			if err != nil {
				return
			}
			if f.dropOnData {
				return
			}
			if f.refuseData {
				_ = tp.PrintfLine("554 refused")
				continue
			}
			cur.data = string(data)
			f.mu.Lock()
			f.deliveries = append(f.deliveries, cur)
			f.mu.Unlock()
			_ = tp.PrintfLine("250 queued")
		case "RSET":
			cur = smtpDelivery{}
			_ = tp.PrintfLine("250 ok")
		case "NOOP":
			_ = tp.PrintfLine("250 ok")
		case "QUIT":
			_ = tp.PrintfLine("221 bye")
			return
		default:
			_ = tp.PrintfLine("502 not implemented")
		}
	}
}

func between(s, open, close string) string {
	i := strings.Index(s, open)
	j := strings.LastIndex(s, close)
	if i < 0 || j <= i {
		return ""
	}
	return s[i+1 : j]
}

// --- Fixture ---

type mailFixture struct {
	t    *testing.T
	im   *IMAP
	user backend.User // the memory backend's user, for seeding and inspection
	smtp *fakeSMTP
}

type fixtureOptions struct {
	smtp      func(*fakeSMTP)
	conn      func(*config.Connection)
	specials  map[string]string // default: "Mein Entwurf" \Drafts, "Sent mail" \Sent
	mailboxes []string          // created besides INBOX; default: the specials' names
}

func newMailFixture(t *testing.T, opts fixtureOptions) *mailFixture {
	t.Helper()
	tlsCfg, roots := testCertificate(t)

	specials := opts.specials
	if specials == nil {
		// Names outside the fallback lists: found only through special-use.
		specials = map[string]string{"Mein Entwurf": specialUseDrafts, "Sent mail": specialUseSent}
	}
	mem := memory.New()
	user, err := mem.Login(nil, "username", "password")
	if err != nil {
		t.Fatal(err)
	}
	boxes := opts.mailboxes
	if boxes == nil {
		for name := range specials {
			boxes = append(boxes, name)
		}
	}
	for _, name := range boxes {
		if err := user.CreateMailbox(name); err != nil {
			t.Fatal(err)
		}
	}

	srv := imapserver.New(&testBackend{mem: mem, specials: specials})
	ln, err := tls.Listen("tcp", "127.0.0.1:0", tlsCfg)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { srv.Close() })

	fs := startFakeSMTP(t, tlsCfg, opts.smtp)

	conn := config.Connection{
		Name:      "mail",
		Type:      config.TypeIMAP,
		Host:      "127.0.0.1",
		Port:      ln.Addr().(*net.TCPAddr).Port,
		User:      testAccount,
		Password:  testPassword,
		FromName:  "Jörg Müller",
		SMTPHost:  "127.0.0.1",
		SMTPPort:  fs.port(),
		AllowSend: true,
	}
	if opts.conn != nil {
		opts.conn(&conn)
	}
	im, err := NewIMAP(conn, nil)
	if err != nil {
		t.Fatal(err)
	}
	im.tlsRoots = roots
	if fs.implicitTLS && im.smtp != nil {
		im.smtp.implicitTLS = true // the test server cannot listen on 465
	}
	return &mailFixture{t: t, im: im, user: user, smtp: fs}
}

// seed stores a message in a mailbox of the test account.
func (f *mailFixture) seed(mailbox, raw string, flags ...string) {
	f.t.Helper()
	mb, err := f.user.GetMailbox(mailbox)
	if err != nil {
		f.t.Fatal(err)
	}
	if err := mb.CreateMessage(flags, time.Now(), bytes.NewBufferString(raw)); err != nil {
		f.t.Fatal(err)
	}
}

// messages returns the raw messages of a mailbox, oldest first.
func (f *mailFixture) messages(mailbox string) []string {
	f.t.Helper()
	mb, err := f.user.GetMailbox(mailbox)
	if err != nil {
		f.t.Fatal(err)
	}
	return rawMessages(f.t, mb.(*memory.Mailbox))
}

func rawMessages(t *testing.T, mb *memory.Mailbox) []string {
	t.Helper()
	var out []string
	for _, m := range mb.Messages {
		if hasFlag(m.Flags, imaplib.DeletedFlag) {
			continue
		}
		out = append(out, string(m.Body))
	}
	return out
}

// call runs a tool handler and decodes its JSON result.
func (f *mailFixture) call(handler func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error), args map[string]any) (map[string]any, string) {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	res, err := handler(ctx, toolRequest(args))
	if err != nil {
		f.t.Fatalf("handler returned a Go error: %v", err)
	}
	text := resultText(f.t, res)
	if res.IsError {
		return nil, text
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		f.t.Fatalf("result is not JSON: %v\n%s", err, text)
	}
	return out, ""
}

const threadMessage = "From: Alice <alice@example.org>\r\n" +
	"Reply-To: list@example.org\r\n" +
	"To: me@example.com, carol@example.org\r\n" +
	"Cc: bob@example.org, ME@example.com\r\n" +
	"Subject: Angebot\r\n" +
	"Date: Mon, 26 Oct 2026 08:00:00 +0100\r\n" +
	"Message-ID: <orig-1@example.org>\r\n" +
	"Content-Type: text/plain; charset=utf-8\r\n" +
	"\r\n" +
	"Hallo, wie sieht es aus?\r\n"

func conversationID(t *testing.T) string {
	t.Helper()
	return makeConversationID(normalizeSubject("Angebot"))
}

func decodeQP(t *testing.T, s string) string {
	t.Helper()
	b, err := io.ReadAll(quotedprintable.NewReader(strings.NewReader(s)))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func stringsOf(v any) []string {
	list, _ := v.([]any)
	var out []string
	for _, x := range list {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// --- Registration ---

func TestIMAPSendToolsNeedTheSwitch(t *testing.T) {
	base := config.Connection{Type: config.TypeIMAP, Host: "imap.x", User: testAccount, Password: testPassword}
	names := func(im *IMAP) []string {
		var out []string
		for _, td := range im.Tools() {
			out = append(out, td.Tool.Name)
		}
		return out
	}

	withHost := base
	withHost.SMTPHost = "smtp.x"
	im, err := NewIMAP(withHost, nil)
	if err != nil {
		t.Fatal(err)
	}
	if im.SendEnabled() || slices.Contains(names(im), "send_draft") {
		t.Errorf("smtp_host without allow_send must not register send tools: %v", names(im))
	}

	enabled := withHost
	enabled.AllowSend = true
	im, err = NewIMAP(enabled, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"send_draft", "reply_send", "forward_send"} {
		if !slices.Contains(names(im), want) {
			t.Errorf("%s missing with allow_send: %v", want, names(im))
		}
	}
	if im.smtp.port != config.DefaultSMTPPort || im.smtp.implicitTLS {
		t.Errorf("default SMTP settings = %+v, want port %d with STARTTLS", im.smtp, config.DefaultSMTPPort)
	}

	implicit := enabled
	implicit.SMTPPort = config.ImplicitTLSSMTPPort
	im, _ = NewIMAP(implicit, nil)
	if !im.smtp.implicitTLS {
		t.Error("port 465 must use implicit TLS")
	}

	noHost := base
	noHost.AllowSend = true
	if _, err := NewIMAP(noHost, nil); err == nil || !strings.Contains(err.Error(), "smtp_host") {
		t.Errorf("allow_send without smtp_host: err = %v, want a message naming smtp_host", err)
	}
}

func TestDefaultInstructionsIMAP(t *testing.T) {
	plain := config.Connection{Type: config.TypeIMAP}
	if strings.Contains(DefaultInstructions(plain), "Sending is enabled") {
		t.Error("a connection that cannot send must not announce sending")
	}
	sending := config.Connection{Type: config.TypeIMAP, SMTPHost: "smtp.x", AllowSend: true}
	if !strings.Contains(DefaultInstructions(sending), "Sending is enabled") {
		t.Error("a connection that can send must say so")
	}
}

// --- Composing ---

func TestReplyRecipients(t *testing.T) {
	im := &IMAP{user: testAccount}
	latest := imapMsg{
		From:    "alice@example.org",
		ReplyTo: []string{"list@example.org"},
		To:      []string{"me@example.com", "carol@example.org"},
		CC:      []string{"bob@example.org", "ME@example.com"},
	}

	to, cc := im.replyRecipients(latest, false)
	if !slices.Equal(to, []string{"list@example.org"}) || len(cc) != 0 {
		t.Errorf("reply: to=%v cc=%v, want the Reply-To only", to, cc)
	}

	to, cc = im.replyRecipients(latest, true)
	if !slices.Equal(to, []string{"list@example.org"}) || !slices.Equal(cc, []string{"carol@example.org", "bob@example.org"}) {
		t.Errorf("reply all: to=%v cc=%v, want the Reply-To and everyone else but this account", to, cc)
	}

	noReplyTo := latest
	noReplyTo.ReplyTo = nil
	if to, _ := im.replyRecipients(noReplyTo, false); !slices.Equal(to, []string{"alice@example.org"}) {
		t.Errorf("without Reply-To: to=%v, want the sender", to)
	}

	own := imapMsg{From: testAccount, ReplyTo: []string{testAccount}, To: []string{"dave@example.org"}}
	if to, _ := im.replyRecipients(own, false); !slices.Equal(to, []string{"dave@example.org"}) {
		t.Errorf("reply to an own message: to=%v, want its recipients", to)
	}
}

func TestComposeEncoding(t *testing.T) {
	im := &IMAP{user: testAccount, fromName: "Jörg Müller", domain: "example.com"}

	raw, id := im.compose(outgoingMail{To: []string{"a@example.org"}, Subject: "Grüße", Body: "Grüße aus Düsseldorf\nZeile zwei"})
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if got := msg.Header.Get("Content-Transfer-Encoding"); got != "quoted-printable" {
		t.Errorf("non-ASCII body: encoding %q, want quoted-printable", got)
	}
	if bytes.ContainsFunc(raw, func(r rune) bool { return r > 127 }) {
		t.Error("the composed message must be 7-bit clean")
	}
	body, _ := io.ReadAll(msg.Body)
	if got := decodeQP(t, string(body)); got != "Grüße aus Düsseldorf\r\nZeile zwei" {
		t.Errorf("decoded body = %q", got)
	}
	from, err := mail.ParseAddress(msg.Header.Get("From"))
	if err != nil || from.Name != "Jörg Müller" || from.Address != testAccount {
		t.Errorf("From = %q (%v), want the display name and the account", msg.Header.Get("From"), err)
	}
	if msg.Header.Get("Message-ID") != id || !strings.HasSuffix(id, ".mux@example.com>") {
		t.Errorf("Message-ID header %q, returned %q", msg.Header.Get("Message-ID"), id)
	}
	if bytes.Contains(bytes.ReplaceAll(raw, []byte("\r\n"), nil), []byte("\n")) {
		t.Error("every line must end in CRLF")
	}

	raw, id2 := im.compose(outgoingMail{To: []string{"a@example.org"}, Subject: "Hello", Body: "plain text"})
	if !bytes.Contains(raw, []byte("Content-Transfer-Encoding: 7bit\r\n")) {
		t.Error("an ASCII body should stay 7bit")
	}
	if id2 == id {
		t.Error("two messages must not share a Message-ID")
	}

	plain := &IMAP{user: testAccount, domain: "example.com"}
	raw, _ = plain.compose(outgoingMail{To: []string{"a@example.org"}, Subject: "x", Body: "y"})
	if !bytes.HasPrefix(raw, []byte("From: <me@example.com>\r\n")) {
		t.Errorf("without a display name From stays the bare address: %q", raw[:40])
	}
}

func TestPrepareDraftForSending(t *testing.T) {
	raw := "From: <me@example.com>\r\n" +
		"To: a@example.org, B <b@example.org>\r\n" +
		"Cc: a@example.org\r\n" +
		"Bcc: hidden@example.org,\r\n" +
		" second@example.org\r\n" +
		"Subject: x\r\n" +
		"Date: Mon, 1 Jan 2024 00:00:00 +0000\r\n" +
		"X-Client-Note: kept\r\n" +
		"\r\n" +
		"Body\r\n"
	now := time.Date(2026, 10, 26, 9, 30, 0, 0, time.UTC)

	out, err := prepareDraftForSending([]byte(raw), now)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"a@example.org", "b@example.org", "hidden@example.org", "second@example.org"}
	if !slices.Equal(out.recipients, want) {
		t.Errorf("recipients = %v, want %v", out.recipients, want)
	}
	transmit := string(out.transmit)
	if strings.Contains(transmit, "Bcc") || strings.Contains(transmit, "second@example.org") {
		t.Errorf("Bcc must not be transmitted, folded line included:\n%s", transmit)
	}
	if !strings.Contains(transmit, "Date: "+now.Format(time.RFC1123Z)+"\r\n") || strings.Contains(transmit, "2024") {
		t.Errorf("Date must be the moment of sending:\n%s", transmit)
	}
	if !strings.Contains(transmit, "X-Client-Note: kept\r\n") || !strings.HasSuffix(transmit, "\r\n\r\nBody\r\n") {
		t.Errorf("other headers and the body must stay:\n%s", transmit)
	}
	if !strings.Contains(string(out.sentCopy), "Bcc: hidden@example.org,\r\n second@example.org\r\n") {
		t.Errorf("the Sent copy keeps Bcc:\n%s", out.sentCopy)
	}

	if _, err := prepareDraftForSending([]byte("Subject: x\r\n\r\nno one"), now); err == nil {
		t.Error("a draft without recipients must not be sent")
	}
	if _, err := prepareDraftForSending([]byte("To: a@example.org"), now); err == nil {
		t.Error("a draft without a header/body separator must be refused")
	}
}

func TestUIDExpungeCommand(t *testing.T) {
	seqSet := new(imaplib.SeqSet)
	seqSet.AddNum(7)
	cmd := (&commands.Uid{Cmd: uidExpunge{seqSet}}).Command()
	if cmd.Name != "UID" || len(cmd.Arguments) != 2 {
		t.Fatalf("command = %+v", cmd)
	}
	if name, ok := cmd.Arguments[0].(imaplib.RawString); !ok || name != "EXPUNGE" {
		t.Errorf("first argument = %#v, want EXPUNGE", cmd.Arguments[0])
	}
	if cmd.Arguments[1] != seqSet {
		t.Errorf("second argument = %#v, want the UID set", cmd.Arguments[1])
	}
}

// --- End to end ---

func TestIMAPReplyDraftThenSendDraft(t *testing.T) {
	f := newMailFixture(t, fixtureOptions{})
	f.seed("INBOX", threadMessage)

	draft, errText := f.call(f.im.handleCreateReplyDraft, map[string]any{
		"conversation_id": conversationID(t),
		"body":            "Passt, danke.\nGrüße",
		"reply_all":       true,
	})
	if errText != "" {
		t.Fatal(errText)
	}
	draftID, _ := draft["draft_id"].(string)
	if draftID == "" || draft["folder"] != "Mein Entwurf" {
		t.Fatalf("draft result = %v, want a draft_id and the special-use Drafts folder", draft)
	}
	if got := stringsOf(draft["cc"]); !slices.Equal(got, []string{"carol@example.org", "bob@example.org"}) {
		t.Errorf("draft cc = %v", got)
	}
	if n := len(f.messages("Mein Entwurf")); n != 1 {
		t.Fatalf("Drafts holds %d messages, want 1", n)
	}

	sent, errText := f.call(f.im.handleSendDraft, map[string]any{"draft_id": draftID})
	if errText != "" {
		t.Fatal(errText)
	}
	if sent["sent"] != true || sent["sent_copy"] != "Sent mail" || sent["warnings"] != nil {
		t.Errorf("send result = %v", sent)
	}

	got := f.smtp.got()
	if len(got) != 1 {
		t.Fatalf("SMTP received %d messages, want 1", len(got))
	}
	d := got[0]
	if d.from != testAccount {
		t.Errorf("MAIL FROM %q, want the account", d.from)
	}
	if !slices.Equal(d.rcpts, []string{"list@example.org", "carol@example.org", "bob@example.org"}) {
		t.Errorf("RCPT TO %v", d.rcpts)
	}
	for _, want := range []string{"Subject: Re: Angebot", "In-Reply-To: <orig-1@example.org>", "Message-ID: " + draftID, "Content-Transfer-Encoding: quoted-printable"} {
		if !strings.Contains(d.data, want) {
			t.Errorf("transmitted message lacks %q:\n%s", want, d.data)
		}
	}

	if n := len(f.messages("Mein Entwurf")); n != 0 {
		t.Errorf("the draft must be gone after sending, Drafts holds %d", n)
	}
	copies := f.messages("Sent mail")
	if len(copies) != 1 || !strings.Contains(copies[0], "Message-ID: "+draftID) {
		t.Errorf("Sent holds %d messages, want the sent draft", len(copies))
	}

	if _, errText := f.call(f.im.handleSendDraft, map[string]any{"draft_id": draftID}); !strings.Contains(errText, "no draft") {
		t.Errorf("sending the same draft twice: %q, want a not-found error", errText)
	}
	if n := len(f.smtp.got()); n != 1 {
		t.Errorf("the second attempt must not reach SMTP, deliveries = %d", n)
	}
}

func TestIMAPReplySendAndForwardSend(t *testing.T) {
	f := newMailFixture(t, fixtureOptions{})
	f.seed("INBOX", threadMessage)

	res, errText := f.call(f.im.handleReplySend, map[string]any{"conversation_id": conversationID(t), "body": "Ja."})
	if errText != "" {
		t.Fatal(errText)
	}
	if res["sent"] != true || !slices.Equal(stringsOf(res["to"]), []string{"list@example.org"}) {
		t.Errorf("reply_send result = %v", res)
	}

	res, errText = f.call(f.im.handleForwardSend, map[string]any{
		"conversation_id": conversationID(t),
		"to":              "x@example.org, Yvonne <y@example.org>",
		"body":            "Zur Info",
	})
	if errText != "" {
		t.Fatal(errText)
	}
	got := f.smtp.got()
	if len(got) != 2 || !slices.Equal(got[1].rcpts, []string{"x@example.org", "y@example.org"}) {
		t.Fatalf("deliveries = %+v", got)
	}
	if !strings.Contains(got[1].data, "Subject: Fwd: Angebot") || !strings.Contains(got[1].data, "Forwarded message") {
		t.Errorf("forward content:\n%s", got[1].data)
	}
	if n := len(f.messages("Mein Entwurf")); n != 0 {
		t.Errorf("direct sending must not leave drafts, Drafts holds %d", n)
	}
	if n := len(f.messages("Sent mail")); n != 2 {
		t.Errorf("Sent holds %d copies, want 2", n)
	}

	if _, errText := f.call(f.im.handleForwardSend, map[string]any{"conversation_id": conversationID(t), "to": "not an address"}); !strings.Contains(errText, "invalid recipient") {
		t.Errorf("invalid recipient: %q", errText)
	}
}

func TestIMAPSendDraftWrittenElsewhere(t *testing.T) {
	f := newMailFixture(t, fixtureOptions{})
	f.seed("Mein Entwurf", "From: Jörg <me@example.com>\r\n"+
		"To: a@example.org\r\n"+
		"Bcc: hidden@example.org\r\n"+
		"Subject: von Hand\r\n"+
		"Message-ID: <hand-1@example.com>\r\n"+
		"\r\n"+
		"Text\r\n", imaplib.DraftFlag)

	res, errText := f.call(f.im.handleSendDraft, map[string]any{"draft_id": "hand-1@example.com"})
	if errText != "" {
		t.Fatal(errText)
	}
	if !slices.Equal(stringsOf(res["recipients"]), []string{"a@example.org", "hidden@example.org"}) {
		t.Errorf("recipients = %v", res["recipients"])
	}
	d := f.smtp.got()[0]
	if strings.Contains(d.data, "Bcc") || !slices.Contains(d.rcpts, "hidden@example.org") {
		t.Errorf("Bcc must be a recipient but not a header:\n%s", d.data)
	}
	if copies := f.messages("Sent mail"); len(copies) != 1 || !strings.Contains(copies[0], "Bcc: hidden@example.org") {
		t.Errorf("the Sent copy keeps Bcc: %v", copies)
	}
}

func TestIMAPSendDraftTwoCopies(t *testing.T) {
	f := newMailFixture(t, fixtureOptions{})
	draft := "To: a@example.org\r\nSubject: x\r\nMessage-ID: <dup@example.com>\r\n\r\nText\r\n"
	f.seed("Mein Entwurf", draft)
	f.seed("Mein Entwurf", draft)

	if _, errText := f.call(f.im.handleSendDraft, map[string]any{"draft_id": "<dup@example.com>"}); !strings.Contains(errText, "2 drafts") {
		t.Errorf("two drafts with one Message-ID: %q", errText)
	}
	if n := len(f.smtp.got()); n != 0 {
		t.Errorf("nothing may be sent when the draft is ambiguous, deliveries = %d", n)
	}
}

func TestIMAPSendFallbackFoldersAndSavesSent(t *testing.T) {
	// No special-use attributes: the common names are used.
	f := newMailFixture(t, fixtureOptions{
		specials:  map[string]string{},
		mailboxes: []string{"Drafts", "Sent"},
		conn:      func(c *config.Connection) { c.SMTPSavesSent = true },
	})
	f.seed("INBOX", threadMessage)

	draft, errText := f.call(f.im.handleCreateReplyDraft, map[string]any{"conversation_id": conversationID(t), "body": "ok"})
	if errText != "" || draft["folder"] != "Drafts" {
		t.Fatalf("draft = %v %s", draft, errText)
	}
	res, errText := f.call(f.im.handleSendDraft, map[string]any{"draft_id": draft["draft_id"]})
	if errText != "" {
		t.Fatal(errText)
	}
	if res["sent_copy"] != "" {
		t.Errorf("smtp_saves_sent: sent_copy = %v, want none", res["sent_copy"])
	}
	if n := len(f.messages("Sent")); n != 0 {
		t.Errorf("smtp_saves_sent: mux stored %d copies itself", n)
	}
	if n := len(f.messages("Drafts")); n != 0 {
		t.Errorf("the draft must be removed, Drafts holds %d", n)
	}
}

func TestIMAPSendWithoutSentFolderWarns(t *testing.T) {
	f := newMailFixture(t, fixtureOptions{specials: map[string]string{"Mein Entwurf": specialUseDrafts}})
	f.seed("INBOX", threadMessage)

	res, errText := f.call(f.im.handleReplySend, map[string]any{"conversation_id": conversationID(t), "body": "ok"})
	if errText != "" {
		t.Fatalf("a missing Sent folder must not turn a sent message into an error: %s", errText)
	}
	if res["sent"] != true || len(stringsOf(res["warnings"])) != 1 {
		t.Errorf("result = %v, want sent with one warning", res)
	}
}

func TestSMTPSessionVariants(t *testing.T) {
	tests := []struct {
		name    string
		smtp    func(*fakeSMTP)
		conn    func(*config.Connection)
		wantErr string // empty: delivered
	}{
		{name: "STARTTLS with PLAIN"},
		{name: "implicit TLS", smtp: func(f *fakeSMTP) { f.implicitTLS = true }},
		{name: "LOGIN only", smtp: func(f *fakeSMTP) { f.authMechs = "LOGIN" }},
		{name: "no STARTTLS", smtp: func(f *fakeSMTP) { f.noSTARTTLS = true }, wantErr: "offers no STARTTLS"},
		{name: "no usable mechanism", smtp: func(f *fakeSMTP) { f.authMechs = "CRAM-MD5" }, wantErr: "neither AUTH PLAIN nor LOGIN"},
		{name: "wrong password", conn: func(c *config.Connection) { c.Password = "wrong" }, wantErr: "SMTP login"},
		{name: "refused message", smtp: func(f *fakeSMTP) { f.refuseData = true }, wantErr: "not sent: the SMTP server refused"},
		{name: "connection lost after the message", smtp: func(f *fakeSMTP) { f.dropOnData = true }, wantErr: "unclear whether sent"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newMailFixture(t, fixtureOptions{smtp: tt.smtp})
			if tt.conn != nil {
				// The IMAP side keeps the right password; only SMTP sees the change.
				c := config.Connection{Password: testPassword}
				tt.conn(&c)
				if c.Password != testPassword {
					f.im.password = c.Password
				}
			}
			raw, _ := f.im.compose(outgoingMail{To: []string{"a@example.org"}, Subject: "x", Body: "y"})
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			err := f.im.submit(ctx, []string{"a@example.org"}, raw)

			if tt.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
				if n := len(f.smtp.got()); n != 1 {
					t.Errorf("deliveries = %d, want 1", n)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("err = %v, want %q", err, tt.wantErr)
			}
			if n := len(f.smtp.got()); n != 0 {
				t.Errorf("deliveries = %d, want none", n)
			}
		})
	}
}

func TestSMTPRefusesEightBitWithout8BITMIME(t *testing.T) {
	f := newMailFixture(t, fixtureOptions{smtp: func(s *fakeSMTP) { s.no8BitMIME = true }})
	f.seed("Mein Entwurf", "To: a@example.org\r\nSubject: x\r\nMessage-ID: <8bit@example.com>\r\nContent-Transfer-Encoding: 8bit\r\n\r\nGrüße\r\n")

	if _, errText := f.call(f.im.handleSendDraft, map[string]any{"draft_id": "<8bit@example.com>"}); !strings.Contains(errText, "8BITMIME") {
		t.Errorf("8-bit draft to a server without 8BITMIME: %q", errText)
	}
	if n := len(f.messages("Mein Entwurf")); n != 1 {
		t.Errorf("an unsent draft must stay, Drafts holds %d", n)
	}

	// What mux composes itself is 7-bit clean and goes through.
	f.seed("INBOX", threadMessage)
	if _, errText := f.call(f.im.handleReplySend, map[string]any{"conversation_id": conversationID(t), "body": "Grüße"}); errText != "" {
		t.Errorf("a composed reply must pass a server without 8BITMIME: %s", errText)
	}
}

func TestIMAPVerifySignsInToSMTP(t *testing.T) {
	f := newMailFixture(t, fixtureOptions{})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := f.im.Verify(ctx); err != nil {
		t.Fatal(err)
	}
	f.smtp.mu.Lock()
	logins := f.smtp.logins
	f.smtp.mu.Unlock()
	if logins != 1 {
		t.Errorf("SMTP logins = %d, want 1", logins)
	}

	f.im.smtp.port = 1 // nothing listens there
	if err := f.im.Verify(ctx); err == nil || !strings.Contains(err.Error(), "SMTP") {
		t.Errorf("Verify with an unreachable SMTP server: %v", err)
	}
}
