package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/smnhffmnn/mux/internal/config"
)

// isolateConfigDir points the config and secret files at a temporary home, so
// connection_add's Save never touches the real config.
func isolateConfigDir(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home+"/.config")
}

func TestConnectionAddIMAPSending(t *testing.T) {
	isolateConfigDir(t)
	ct := NewConfigTools(&config.Config{}, nil)

	add := func(args map[string]any) (string, bool) {
		t.Helper()
		res, err := ct.handleConnectionAdd(context.Background(), toolRequest(args))
		if err != nil {
			t.Fatal(err)
		}
		return resultText(t, res), res.IsError
	}

	if text, isErr := add(map[string]any{"name": "db", "type": config.TypeMariaDB, "host": "x", "allow_send": true}); !isErr || !strings.Contains(text, "allow_send applies to type") {
		t.Errorf("imap field on another type: %q", text)
	}
	if text, isErr := add(map[string]any{"name": "m1", "type": config.TypeIMAP, "host": "imap.x", "user": "me@x", "allow_send": true}); !isErr || !strings.Contains(text, "smtp_host") {
		t.Errorf("allow_send without smtp_host: %q", text)
	}

	text, isErr := add(map[string]any{
		"name": "m2", "type": config.TypeIMAP, "host": "imap.x", "user": "me@x",
		"smtp_host": "smtp.x", "allow_send": true, "smtp_saves_sent": true, "from_name": "Jane Doe",
	})
	if isErr {
		t.Fatal(text)
	}
	c := ct.cfg.FindAnyConnection("m2")
	if c == nil {
		t.Fatal("connection not stored")
	}
	if c.SMTPHost != "smtp.x" || c.SMTPPort != config.DefaultSMTPPort || !c.AllowSend || !c.SMTPSavesSent || c.FromName != "Jane Doe" {
		t.Errorf("stored connection = %+v", *c)
	}

	res, err := ct.handleConnectionList(context.Background(), toolRequest(nil))
	if err != nil {
		t.Fatal(err)
	}
	var listed struct {
		Connections []map[string]any `json:"connections"`
	}
	if err := json.Unmarshal([]byte(resultText(t, res)), &listed); err != nil {
		t.Fatal(err)
	}
	for _, entry := range listed.Connections {
		if entry["name"] == "m2" {
			if entry["smtp_host"] != "smtp.x" || entry["allow_send"] != true || entry["from_name"] != "Jane Doe" {
				t.Errorf("connection_list entry = %v", entry)
			}
			return
		}
	}
	t.Error("m2 missing from connection_list")
}
