package config

import (
	"fmt"
	"net/mail"
	"strings"
)

// SMTP ports for the sending side of the imap connection type.
const (
	// DefaultSMTPPort is mail submission with STARTTLS (RFC 6409).
	DefaultSMTPPort = 587
	// ImplicitTLSSMTPPort is submission over TLS from the first byte (RFC 8314).
	ImplicitTLSSMTPPort = 465
)

// IMAPOnlyFields are the config keys that only the imap type reads. Set on
// another type they would be stored and never used, which hides a typo in the
// type.
var IMAPOnlyFields = []string{"smtp_host", "smtp_port", "smtp_saves_sent"}

// MailSendFields are the config keys the mail types that can send share:
// imap (over SMTP) and nylas (over the Nylas API).
var MailSendFields = []string{"allow_send", "from_name"}

// MailSendTypes are the connection types that read MailSendFields.
var MailSendTypes = []string{TypeIMAP, TypeNylas}

// ReadsMailSendFields reports whether a connection type reads allow_send
// and from_name.
func ReadsMailSendFields(typ string) bool {
	for _, t := range MailSendTypes {
		if t == typ {
			return true
		}
	}
	return false
}

// ValidateIMAPSending checks the sending fields of an imap connection.
// AllowSend is the switch and SMTPHost its precondition: sending enabled
// without a server is a configuration error, not a silent no-op, so the
// connection fails to register with a message that names the gap.
func ValidateIMAPSending(c Connection) error {
	if c.Type != TypeIMAP {
		return nil // other types never read these fields
	}
	if c.SMTPPort < 0 || c.SMTPPort > 65535 {
		return fmt.Errorf("smtp_port %d is out of range", c.SMTPPort)
	}
	if c.AllowSend && c.SMTPHost == "" {
		return fmt.Errorf("allow_send requires smtp_host")
	}
	// The user is the sender address (From, MAIL FROM, the own address left
	// out of reply-all). A login name that is not an address would send
	// mail with a From no recipient can answer.
	if c.AllowSend && !isMailAddress(c.User) {
		return fmt.Errorf("allow_send requires user to be an email address, got %q", c.User)
	}
	return nil
}

// isMailAddress reports whether s is a bare address (local@domain).
func isMailAddress(s string) bool {
	a, err := mail.ParseAddress(s)
	return err == nil && a.Name == "" && a.Address == strings.TrimSpace(s) && strings.Contains(a.Address, "@")
}

// SendEnabled reports whether a mail connection registers its send tools.
// For imap, allow_send needs its precondition smtp_host; for nylas the API
// sends, so allow_send alone decides.
func (c *Connection) SendEnabled() bool {
	switch c.Type {
	case TypeIMAP:
		return c.AllowSend && c.SMTPHost != ""
	case TypeNylas:
		return c.AllowSend
	}
	return false
}
