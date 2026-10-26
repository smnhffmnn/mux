package config

import "fmt"

// SMTP ports for the sending side of the imap connection type.
const (
	// DefaultSMTPPort is mail submission with STARTTLS (RFC 6409).
	DefaultSMTPPort = 587
	// ImplicitTLSSMTPPort is submission over TLS from the first byte (RFC 8314).
	ImplicitTLSSMTPPort = 465
)

// IMAPSendFields are the config keys that only the imap type reads. Set on
// another type they would be stored and never used, which hides a typo in the
// type.
var IMAPSendFields = []string{"smtp_host", "smtp_port", "allow_send", "smtp_saves_sent", "from_name"}

// ValidateIMAPSending checks the sending fields of an imap connection.
// AllowSend is the switch and SMTPHost its precondition: sending enabled
// without a server is a configuration error, not a silent no-op, so the
// connection fails to register with a message that names the gap.
func ValidateIMAPSending(c Connection) error {
	if c.SMTPPort < 0 || c.SMTPPort > 65535 {
		return fmt.Errorf("smtp_port %d is out of range", c.SMTPPort)
	}
	if c.AllowSend && c.SMTPHost == "" {
		return fmt.Errorf("allow_send requires smtp_host")
	}
	return nil
}

// SendEnabled reports whether an imap connection registers its send tools.
func (c *Connection) SendEnabled() bool {
	return c.Type == TypeIMAP && c.AllowSend && c.SMTPHost != ""
}
