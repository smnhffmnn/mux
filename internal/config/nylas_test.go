package config

import (
	"strings"
	"testing"
)

func TestNylas_Validate(t *testing.T) {
	cases := []struct {
		name string
		conn Connection
		want string // substring of the error, "" for ok
	}{
		{"other type ignores the fields", Connection{Type: TypeIMAP, Region: "mars"}, ""},
		{"grant id alone", Connection{Type: TypeNylas, GrantID: "g"}, ""},
		{"address alone", Connection{Type: TypeNylas, User: "jane@example.com"}, ""},
		{"eu region", Connection{Type: TypeNylas, GrantID: "g", Region: "eu"}, ""},
		{"unknown region", Connection{Type: TypeNylas, GrantID: "g", Region: "mars"}, "not a Nylas region"},
		{"neither grant nor address", Connection{Type: TypeNylas}, "grant_id"},
		{"user that is no address", Connection{Type: TypeNylas, User: "jane"}, "mailbox address"},
		{"user with display name", Connection{Type: TypeNylas, User: "Jane <jane@example.com>"}, "mailbox address"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateNylas(tc.conn)
			switch {
			case tc.want == "" && err != nil:
				t.Errorf("unexpected error: %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Errorf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestNylas_EnabledDefaultsSending(t *testing.T) {
	if !ValidType(TypeNylas) {
		t.Error("nylas should be a valid type")
	}
	c := Connection{Type: TypeNylas, Token: "k", GrantID: "g"}
	if !c.Enabled() {
		t.Error("token + grant_id must enable the connection")
	}
	c = Connection{Type: TypeNylas, Token: "k", User: "jane@example.com"}
	if !c.Enabled() {
		t.Error("token + address must enable the connection")
	}
	c = Connection{Type: TypeNylas, Token: "k"}
	if c.Enabled() {
		t.Error("without grant or address the connection must not be enabled")
	}
	c = Connection{Type: TypeNylas, GrantID: "g"}
	if c.Enabled() {
		t.Error("without token the connection must not be enabled")
	}

	c = Connection{Type: TypeNylas}
	ApplyConnectionDefaults(&c)
	if c.Region != NylasRegionUS {
		t.Errorf("default region = %q", c.Region)
	}
	c = Connection{Type: TypeNylas, Region: "eu"}
	ApplyConnectionDefaults(&c)
	if c.Region != "eu" {
		t.Errorf("explicit region overwritten: %q", c.Region)
	}

	if NylasBaseURL("") != "https://api.us.nylas.com" || NylasBaseURL("EU") != "https://api.eu.nylas.com" {
		t.Errorf("base URLs: %q %q", NylasBaseURL(""), NylasBaseURL("EU"))
	}

	// allow_send alone decides for nylas; imap keeps its smtp_host precondition.
	c = Connection{Type: TypeNylas, AllowSend: true}
	if !c.SendEnabled() {
		t.Error("nylas with allow_send must send")
	}
	c = Connection{Type: TypeNylas}
	if c.SendEnabled() {
		t.Error("nylas without allow_send must not send")
	}
	c = Connection{Type: TypeIMAP, AllowSend: true}
	if c.SendEnabled() {
		t.Error("imap with allow_send but no smtp_host must not send")
	}
	c = Connection{Type: TypeMariaDB, AllowSend: true}
	if c.SendEnabled() {
		t.Error("a database never sends")
	}
	if !ReadsMailSendFields(TypeIMAP) || !ReadsMailSendFields(TypeNylas) || ReadsMailSendFields(TypeMariaDB) {
		t.Error("ReadsMailSendFields: imap and nylas, nothing else")
	}
}
