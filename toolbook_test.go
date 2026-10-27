package main

import (
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/server"

	"github.com/smnhffmnn/mux/internal/config"
)

// connectionTools lists the registered tools of a connection by prefix.
func connectionTools(s *server.MCPServer, conn string) []string {
	var names []string
	for name := range s.ListTools() {
		if strings.HasPrefix(name, conn+"_") {
			names = append(names, name)
		}
	}
	return names
}

func sendingIMAP() config.Connection {
	return config.Connection{
		Name: "mail", Type: config.TypeIMAP, Source: config.SourceLocal,
		Host: "imap.example.com", User: "me@example.com", Password: "secret",
		SMTPHost: "smtp.example.com", AllowSend: true,
	}
}

// A connection registered at startup, before any reloader existed, is
// removed by the reloader — on delete and on a reload that cannot register it
// again.
func TestStartupToolsAreRemovedByTheReloader(t *testing.T) {
	for _, tt := range []struct {
		name string
		act  func(r *simpleReloader)
	}{
		{"delete", func(r *simpleReloader) { r.UnloadConnection("mail") }},
		{"reload without a password", func(r *simpleReloader) {
			c := sendingIMAP()
			c.Password = ""
			r.ReloadConnection(c)
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := server.NewMCPServer("test", "0", server.WithToolCapabilities(true))
			cfg := &config.Config{Connections: []config.Connection{sendingIMAP()}}
			book := newToolBook()
			registerConnections(s, cfg, nil, book)
			if _, ok := s.ListTools()["mail_send_draft"]; !ok {
				t.Fatalf("startup registration: mail_send_draft missing, have %v", connectionTools(s, "mail"))
			}

			tt.act(newSimpleReloader(s, cfg, nil, book))
			if left := connectionTools(s, "mail"); len(left) > 0 {
				t.Errorf("tools still registered: %v", left)
			}
		})
	}
}

func TestReloadReplacesStartupTools(t *testing.T) {
	s := server.NewMCPServer("test", "0", server.WithToolCapabilities(true))
	cfg := &config.Config{Connections: []config.Connection{sendingIMAP()}}
	book := newToolBook()
	registerConnections(s, cfg, nil, book)

	off := sendingIMAP()
	off.AllowSend = false
	newSimpleReloader(s, cfg, nil, book).ReloadConnection(off)

	if _, ok := s.ListTools()["mail_send_draft"]; ok {
		t.Error("send tools survived switching allow_send off")
	}
	if _, ok := s.ListTools()["mail_list_conversations"]; !ok {
		t.Error("the reading tools must be registered again")
	}
}
