package tools

import (
	"strings"
	"testing"

	imaplib "github.com/emersion/go-imap"
)

// --- normalizeSubject ---

func TestNormalizeSubject(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		// Basic
		{"Hello", "Hello"},
		{"", ""},
		{"  spaced  ", "spaced"},

		// English prefixes
		{"Re: Hello", "Hello"},
		{"RE: Hello", "Hello"},
		{"Fw: Hello", "Hello"},
		{"FW: Hello", "Hello"},
		{"Fwd: Hello", "Hello"},
		{"FWD: Hello", "Hello"},

		// German prefixes
		{"Aw: Bestellung", "Bestellung"},
		{"AW: Bestellung", "Bestellung"},
		{"WG: Bestellung", "Bestellung"},
		{"wg: Bestellung", "Bestellung"},
		{"Betr.: Thema", "Thema"},

		// Scandinavian
		{"SV: Hej", "Hej"},
		{"sv: Hej", "Hej"},

		// Nested prefixes
		{"Re: Re: Hello", "Hello"},
		{"Fwd: Re: Hello", "Hello"},
		{"AW: WG: AW: Bestellung", "Bestellung"},
		{"Re: Fwd: Re: Aw: Deep", "Deep"},

		// Numbered bracket prefixes
		{"Re[2]: Hello", "Hello"},
		{"Fw[3]: Hello", "Hello"},
		{"Aw[5]: Bestellung", "Bestellung"},

		// Bracket prefix without ]: — strips "re[" prefix (edge case)
		{"Re[broken", "broken"},

		// Subject that starts with prefix-like text but isn't
		{"Research: Topic", "Research: Topic"},
		{"Awesome: Thing", "Awesome: Thing"},
		{"Reward: Points", "Reward: Points"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := normalizeSubject(tt.input)
			if got != tt.want {
				t.Errorf("normalizeSubject(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// --- makeConversationID ---

func TestMakeConversationID(t *testing.T) {
	// Same subject → same ID
	id1 := makeConversationID("Hello")
	id2 := makeConversationID("Hello")
	if id1 != id2 {
		t.Errorf("same input should produce same ID: %q vs %q", id1, id2)
	}

	// Different subject → different ID
	id3 := makeConversationID("Goodbye")
	if id1 == id3 {
		t.Errorf("different input should produce different ID: %q vs %q", id1, id3)
	}

	// Should be 16 hex chars
	if len(id1) != 16 {
		t.Errorf("conversation ID should be 16 chars, got %d: %q", len(id1), id1)
	}
}

// --- threadMessages ---

func TestThreadMessages(t *testing.T) {
	msgs := []imapMsg{
		{UID: 1, Subject: "Project Update", From: "alice@x.com"},
		{UID: 2, Subject: "Re: Project Update", From: "bob@x.com"},
		{UID: 3, Subject: "Different Topic", From: "carol@x.com"},
		{UID: 4, Subject: "AW: Project Update", From: "dave@x.com"},
	}

	groups := threadMessages(msgs)

	// "Project Update", "Re: Project Update", "AW: Project Update" should be one group
	// "Different Topic" should be another
	if len(groups) != 2 {
		t.Fatalf("expected 2 conversation groups, got %d", len(groups))
	}

	// Find the Project Update group
	projectID := makeConversationID("Project Update")
	projectGroup, ok := groups[projectID]
	if !ok {
		t.Fatalf("expected conversation ID %q for 'Project Update'", projectID)
	}
	if len(projectGroup) != 3 {
		t.Errorf("Project Update group should have 3 messages, got %d", len(projectGroup))
	}

	// Different Topic
	diffID := makeConversationID("Different Topic")
	diffGroup, ok := groups[diffID]
	if !ok {
		t.Fatalf("expected conversation ID %q for 'Different Topic'", diffID)
	}
	if len(diffGroup) != 1 {
		t.Errorf("Different Topic group should have 1 message, got %d", len(diffGroup))
	}
	_ = diffGroup
}

// --- normalizeMessageID ---

func TestNormalizeMessageID(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"", ""},
		{"<abc@def>", "<abc@def>"},
		{"abc@def", "<abc@def>"},
		{"<abc@def", "<abc@def>"},
		{"abc@def>", "<abc@def>"},
		{"  <abc@def>  ", "<abc@def>"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := normalizeMessageID(tt.input)
			if got != tt.want {
				t.Errorf("normalizeMessageID(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// --- addrString ---

func TestAddrString(t *testing.T) {
	tests := []struct {
		name    string
		mailbox string
		host    string
		want    string
	}{
		{"normal", "user", "example.com", "user@example.com"},
		{"empty mailbox", "", "example.com", ""},
		{"empty host", "user", "", ""},
		{"both empty", "", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := &imaplib.Address{MailboxName: tt.mailbox, HostName: tt.host}
			got := addrString(a)
			if got != tt.want {
				t.Errorf("addrString(%+v) = %q, want %q", a, got, tt.want)
			}
		})
	}

	// nil address
	if got := addrString(nil); got != "" {
		t.Errorf("addrString(nil) = %q, want empty", got)
	}
}

// --- sanitizeHeaderValue ---

func TestSanitizeHeaderValue(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"normal text", "normal text"},
		{"has\r\nnewline", "hasnewline"},
		{"has\nnewline", "hasnewline"},
		{"has\rreturn", "hasreturn"},
		{"victim@x.com>\r\nBcc: spy@evil.com", "victim@x.com>Bcc: spy@evil.com"},
		{"", ""},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := sanitizeHeaderValue(tt.input)
			if got != tt.want {
				t.Errorf("sanitizeHeaderValue(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// --- encodeHeader ---

func TestEncodeHeader(t *testing.T) {
	// ASCII — no encoding
	ascii := encodeHeader("Hello World")
	if ascii != "Hello World" {
		t.Errorf("ASCII should not be encoded, got %q", ascii)
	}

	// Non-ASCII — should be RFC 2047 encoded
	german := encodeHeader("Bestellung für Büro")
	if !strings.HasPrefix(german, "=?utf-8?") {
		t.Errorf("Non-ASCII should be RFC 2047 encoded, got %q", german)
	}
	if strings.Contains(german, "ü") {
		t.Errorf("Raw non-ASCII chars should not appear in encoded header, got %q", german)
	}
}

// --- normalizeCRLF ---

func TestNormalizeCRLF(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"hello\nworld", "hello\r\nworld"},
		{"hello\r\nworld", "hello\r\nworld"},
		{"hello\rworld", "hello\r\nworld"},
		{"mixed\r\nand\nstuff\r", "mixed\r\nand\r\nstuff\r\n"},
		{"no newlines", "no newlines"},
		{"", ""},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := normalizeCRLF(tt.input)
			if got != tt.want {
				t.Errorf("normalizeCRLF(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// --- formatAddressList ---

func TestFormatAddressList(t *testing.T) {
	// Single bare address
	got := formatAddressList([]string{"user@example.com"})
	if got != "<user@example.com>" {
		t.Errorf("got %q", got)
	}

	// Multiple addresses
	got = formatAddressList([]string{"a@x.com", "b@y.com"})
	if got != "<a@x.com>, <b@y.com>" {
		t.Errorf("got %q", got)
	}

	// Already bracketed
	got = formatAddressList([]string{"<user@example.com>"})
	if got != "<user@example.com>" {
		t.Errorf("got %q", got)
	}

	// CRLF injection attempt should be sanitized
	got = formatAddressList([]string{"victim@x.com>\r\nBcc: spy@evil.com"})
	if strings.Contains(got, "\n") || strings.Contains(got, "\r") {
		t.Errorf("CRLF should be sanitized, got %q", got)
	}
}

// --- extractTextFromMessage ---

func TestExtractTextFromMessage_PlainText(t *testing.T) {
	msg := "From: test@example.com\r\nContent-Type: text/plain\r\n\r\nHello World"
	got := extractTextFromMessage(strings.NewReader(msg))
	if !strings.Contains(got, "Hello World") {
		t.Errorf("expected 'Hello World', got %q", got)
	}
}

func TestExtractTextFromMessage_MultipartAlternative(t *testing.T) {
	msg := "From: test@example.com\r\n" +
		"Content-Type: multipart/alternative; boundary=boundary42\r\n\r\n" +
		"--boundary42\r\n" +
		"Content-Type: text/plain\r\n\r\n" +
		"Plain text body\r\n" +
		"--boundary42\r\n" +
		"Content-Type: text/html\r\n\r\n" +
		"<html><body>HTML body</body></html>\r\n" +
		"--boundary42--\r\n"

	got := extractTextFromMessage(strings.NewReader(msg))
	if !strings.Contains(got, "Plain text body") {
		t.Errorf("expected 'Plain text body', got %q", got)
	}
	if strings.Contains(got, "<html>") {
		t.Errorf("should prefer text/plain over HTML, got %q", got)
	}
}

func TestExtractTextFromMessage_HTMLOnly(t *testing.T) {
	msg := "From: test@example.com\r\n" +
		"Content-Type: multipart/alternative; boundary=b\r\n\r\n" +
		"--b\r\n" +
		"Content-Type: text/html\r\n\r\n" +
		"<html><body><p>HTML only</p></body></html>\r\n" +
		"--b--\r\n"

	got := extractTextFromMessage(strings.NewReader(msg))
	if !strings.Contains(got, "HTML only") {
		t.Errorf("expected stripped HTML text, got %q", got)
	}
	if strings.Contains(got, "<p>") {
		t.Errorf("HTML tags should be stripped, got %q", got)
	}
}

func TestExtractTextFromMessage_Base64(t *testing.T) {
	// "Hello Base64" in base64
	msg := "From: test@example.com\r\n" +
		"Content-Type: text/plain\r\n" +
		"Content-Transfer-Encoding: base64\r\n\r\n" +
		"SGVsbG8gQmFzZTY0\r\n"

	got := extractTextFromMessage(strings.NewReader(msg))
	if !strings.Contains(got, "Hello Base64") {
		t.Errorf("expected decoded base64 'Hello Base64', got %q", got)
	}
}

func TestExtractTextFromMessage_QuotedPrintable(t *testing.T) {
	msg := "From: test@example.com\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"Content-Transfer-Encoding: quoted-printable\r\n\r\n" +
		"Gr=C3=BC=C3=9Fe aus D=C3=BCsseldorf\r\n"

	got := extractTextFromMessage(strings.NewReader(msg))
	if !strings.Contains(got, "Grüße aus Düsseldorf") {
		t.Errorf("expected decoded QP with umlauts, got %q", got)
	}
}

func TestExtractTextFromMessage_InvalidMessage(t *testing.T) {
	// Not a valid RFC822 message — should return raw content
	got := extractTextFromMessage(strings.NewReader("just some random text"))
	if !strings.Contains(got, "just some random text") {
		t.Errorf("invalid message should return raw content, got %q", got)
	}
}

// --- stripHTMLTags ---

func TestStripHTMLTags(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"<p>Hello</p>", "Hello"},
		{"<b>Bold</b> and <i>italic</i>", "Bold and italic"},
		{"No tags", "No tags"},
		{"<div><p>Nested</p></div>", "Nested"},
		{"", ""},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := stripHTMLTags(tt.input)
			if got != tt.want {
				t.Errorf("stripHTMLTags(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

