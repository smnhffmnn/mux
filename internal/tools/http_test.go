package tools

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/smnhffmnn/mux/internal/config"
)

func TestHTTP_CustomHeaders(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	h, err := NewHTTP(config.Connection{
		Type:  config.TypeHTTP,
		URL:   srv.URL,
		Token: "secret-token",
		Headers: map[string]string{
			"Notion-Version": "2022-06-28",
			"Accept":         "application/vnd.custom+json", // overrides the default
			"Authorization":  "should-lose",                 // token header must win
		},
	}, nil)
	if err != nil {
		t.Fatalf("NewHTTP: %v", err)
	}

	req := mcp.CallToolRequest{Params: mcp.CallToolParams{
		Arguments: map[string]any{"path": "/v1/users/me"},
	}}
	res, err := h.handleGet(context.Background(), req)
	if err != nil {
		t.Fatalf("handleGet: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected success, got error: %+v", res)
	}

	if v := got.Get("Notion-Version"); v != "2022-06-28" {
		t.Errorf("Notion-Version = %q, want 2022-06-28", v)
	}
	if v := got.Get("Accept"); v != "application/vnd.custom+json" {
		t.Errorf("Accept = %q, want custom override", v)
	}
	if v := got.Get("Authorization"); v != "Bearer secret-token" {
		t.Errorf("Authorization = %q, want vault-managed token to win", v)
	}
}

func TestHTTP_NoHeadersConfigured(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	h, err := NewHTTP(config.Connection{Type: config.TypeHTTP, URL: srv.URL}, nil)
	if err != nil {
		t.Fatalf("NewHTTP: %v", err)
	}

	req := mcp.CallToolRequest{Params: mcp.CallToolParams{
		Arguments: map[string]any{"path": "/ping"},
	}}
	if _, err := h.handleGet(context.Background(), req); err != nil {
		t.Fatalf("handleGet: %v", err)
	}

	if v := got.Get("Accept"); v != "application/json" {
		t.Errorf("Accept = %q, want application/json default", v)
	}
	if v := got.Get("Authorization"); v != "" {
		t.Errorf("Authorization = %q, want unset without token", v)
	}
}
