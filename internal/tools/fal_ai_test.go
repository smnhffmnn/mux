package tools

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/smnhffmnn/mux/internal/config"
)

func TestNewFalAI_RequiresToken(t *testing.T) {
	conn := config.Connection{
		Name: "test-fal",
		Type: config.TypeFalAI,
		URL:  "https://queue.fal.run",
	}
	_, err := NewFalAI(conn, nil)
	if err == nil {
		t.Fatal("expected error when token is empty")
	}
}

func TestNewFalAI_DefaultURL(t *testing.T) {
	conn := config.Connection{
		Name:  "test-fal",
		Type:  config.TypeFalAI,
		Token: "fal_test",
	}
	fa, err := NewFalAI(conn, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fa.baseURL != "https://queue.fal.run" {
		t.Errorf("expected default URL, got %q", fa.baseURL)
	}
}

func TestNewFalAI_CustomURL(t *testing.T) {
	conn := config.Connection{
		Name:  "test-fal",
		Type:  config.TypeFalAI,
		URL:   "https://custom.fal.run/",
		Token: "fal_test",
	}
	fa, err := NewFalAI(conn, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fa.baseURL != "https://custom.fal.run" {
		t.Errorf("expected trailing slash stripped, got %q", fa.baseURL)
	}
}

func TestFalAI_ToolCount(t *testing.T) {
	conn := config.Connection{
		Name:  "test-fal",
		Type:  config.TypeFalAI,
		Token: "fal_test",
	}
	fa, err := NewFalAI(conn, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	tools := fa.Tools()
	if len(tools) != 3 {
		t.Errorf("expected 3 tools, got %d", len(tools))
	}

	names := make(map[string]bool)
	for _, td := range tools {
		names[td.Tool.Name] = true
	}
	for _, expected := range []string{"submit", "status", "result"} {
		if !names[expected] {
			t.Errorf("missing tool %q", expected)
		}
	}
}

func TestFalAI_TypeRegistered(t *testing.T) {
	if !config.ValidType(config.TypeFalAI) {
		t.Error("fal-ai should be a valid connection type")
	}
}

func TestFalAI_EnabledRequiresToken(t *testing.T) {
	conn := config.Connection{Type: config.TypeFalAI}
	if conn.Enabled() {
		t.Error("fal-ai without token should not be enabled")
	}
	conn.Token = "fal_test"
	if !conn.Enabled() {
		t.Error("fal-ai with token should be enabled")
	}
}

func TestFalAI_DefaultsApplied(t *testing.T) {
	conn := config.Connection{Type: config.TypeFalAI}
	config.ApplyConnectionDefaults(&conn)
	if conn.URL != "https://queue.fal.run" {
		t.Errorf("expected default URL, got %q", conn.URL)
	}
}

func TestFalAI_ValidateQueueURL(t *testing.T) {
	fa, err := NewFalAI(config.Connection{Type: config.TypeFalAI, Token: "t"}, nil)
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	cases := []struct {
		name    string
		url     string
		wantErr bool
	}{
		{"same host https", "https://queue.fal.run/reqs/abc", false},
		{"same host case-insensitive", "https://QUEUE.FAL.RUN/reqs/abc", false},
		{"same host http", "http://queue.fal.run/reqs/abc", false},
		{"different host", "https://evil.com/x", true},
		{"ftp scheme", "ftp://queue.fal.run/x", true},
		{"file scheme", "file:///etc/passwd", true},
		{"empty scheme", "//queue.fal.run/x", true},
		{"malformed", "://queue.fal.run", true},
	}
	for _, tc := range cases {
		err := fa.validateQueueURL(tc.url)
		if (err != nil) != tc.wantErr {
			t.Errorf("%s: validateQueueURL(%q) err=%v, wantErr=%v", tc.name, tc.url, err, tc.wantErr)
		}
	}
}

func TestFalAI_ValidateQueueURL_FailsClosedOnEmptyBaseURL(t *testing.T) {
	// Force an empty-host baseURL — must not accept any URL.
	fa := &FalAI{baseURL: "", apiKey: "t"}
	if err := fa.validateQueueURL("https://queue.fal.run/reqs/abc"); err == nil {
		t.Error("expected error when baseURL is empty")
	}
}

func TestFalAI_HandleStatus_AcceptsInProgress(t *testing.T) {
	// fal.ai's queue API returns HTTP 202 while a job is IN_QUEUE or IN_PROGRESS
	// and HTTP 200 only when COMPLETED. Both carry a valid JSON status body.
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"in-progress 202", http.StatusAccepted, `{"status":"IN_PROGRESS","request_id":"x"}`},
		{"completed 200", http.StatusOK, `{"status":"COMPLETED","request_id":"x"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			fa, err := NewFalAI(config.Connection{Type: config.TypeFalAI, Token: "t", URL: srv.URL}, nil)
			if err != nil {
				t.Fatalf("NewFalAI: %v", err)
			}

			req := mcp.CallToolRequest{Params: mcp.CallToolParams{
				Arguments: map[string]any{"status_url": srv.URL + "/status"},
			}}
			res, err := fa.handleStatus(context.Background(), req)
			if err != nil {
				t.Fatalf("handleStatus returned error: %v", err)
			}
			if res.IsError {
				t.Errorf("expected success result, got error: %+v", res)
			}
		})
	}
}

func TestFalAI_ModelRegex(t *testing.T) {
	cases := []struct {
		model string
		valid bool
	}{
		{"bytedance/seedance-2.0/text-to-video", true},
		{"flux/schnell", true},
		{"valid-model_name.v2", true},
		{"a", true},
		{"", false},
		{"foo/../admin", false},
		{"foo//admin", false},
		{"..", false},
		{"/leading-slash", false},
		{"trailing space ", false},
		{"has spaces", false},
		{"has;semicolons", false},
		{"has?query", false},
	}
	for _, tc := range cases {
		matches := falModelRe.MatchString(tc.model)
		hasDotDot := strings.Contains(tc.model, "..")
		hasDoubleSlash := strings.Contains(tc.model, "//")
		accepted := matches && !hasDotDot && !hasDoubleSlash
		if accepted != tc.valid {
			t.Errorf("model %q: got accepted=%v, want %v", tc.model, accepted, tc.valid)
		}
	}
}
