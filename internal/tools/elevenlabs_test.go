package tools

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/smnhffmnn/mux/internal/config"
)

func newTestElevenLabs(t *testing.T, url string) *ElevenLabs {
	t.Helper()
	e, err := NewElevenLabs(config.Connection{
		Name:  "test-elevenlabs",
		Type:  config.TypeElevenLabs,
		URL:   url,
		Token: "xi_test",
	}, nil)
	if err != nil {
		t.Fatalf("NewElevenLabs: %v", err)
	}
	// Never write into the real ~/.mux/output during tests.
	e.outputDir = t.TempDir()
	return e
}

func TestElevenLabs_ToolsExposePost(t *testing.T) {
	e := newTestElevenLabs(t, "")

	names := make(map[string]bool)
	for _, td := range e.Tools() {
		names[td.Tool.Name] = true
	}
	for _, want := range []string{"get", "post"} {
		if !names[want] {
			t.Errorf("missing tool %q — TTS is unreachable without it", want)
		}
	}
}

func TestElevenLabs_PostSendsJSONBodyAndKey(t *testing.T) {
	var (
		gotMethod string
		gotPath   string
		gotHeader http.Header
		gotBody   []byte
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotHeader = r.Method, r.URL.Path, r.Header.Clone()
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	e := newTestElevenLabs(t, srv.URL)
	// Path without a leading slash must still resolve.
	req := toolRequest(map[string]any{
		"path": "v1/text-to-speech/voice123",
		"body": `{"text":"Öl, Ähre, Übung.","model_id":"eleven_multilingual_v2"}`,
	})

	res, err := e.handlePost(context.Background(), req)
	if err != nil {
		t.Fatalf("handlePost: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected success, got %q", resultText(t, res))
	}

	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/v1/text-to-speech/voice123" {
		t.Errorf("path = %q, want leading slash added", gotPath)
	}
	if v := gotHeader.Get("xi-api-key"); v != "xi_test" {
		t.Errorf("xi-api-key = %q, want xi_test", v)
	}
	if v := gotHeader.Get("Content-Type"); v != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", v)
	}
	if v := gotHeader.Get("Accept"); v != "*/*" {
		t.Errorf("Accept = %q, want */* so audio responses are not negotiated away", v)
	}
	if want := `{"text":"Öl, Ähre, Übung.","model_id":"eleven_multilingual_v2"}`; string(gotBody) != want {
		t.Errorf("body = %q, want %q", gotBody, want)
	}

	if text := resultText(t, res); !strings.Contains(text, `{"ok":true}`) {
		t.Errorf("JSON response should be inlined, got:\n%s", text)
	}
}

func TestElevenLabs_AudioWithoutOutputFileGoesToOutputDir(t *testing.T) {
	// Larger than elevenlabsMaxBody: a narration track must not be truncated to
	// the inline limit.
	payload := bytes.Repeat([]byte{0x49, 0x44, 0x33, 0x04}, (elevenlabsMaxBody/4)+1024)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		w.Write(payload)
	}))
	defer srv.Close()

	e := newTestElevenLabs(t, srv.URL)
	res, err := e.handlePost(context.Background(), toolRequest(map[string]any{
		"path": "/v1/text-to-speech/voice123",
		"body": `{"text":"hallo"}`,
	}))
	if err != nil {
		t.Fatalf("handlePost: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected success, got %q", resultText(t, res))
	}

	text := resultText(t, res)
	path := savedPath(t, text)

	if filepath.Ext(path) != ".mp3" {
		t.Errorf("extension = %q, want .mp3 derived from audio/mpeg", filepath.Ext(path))
	}
	if dir := filepath.Dir(path); dir != e.outputDir {
		t.Errorf("directory = %q, want output dir %q", dir, e.outputDir)
	}
	if !strings.Contains(text, "Content-Type: audio/mpeg") {
		t.Errorf("result should report the content type:\n%s", text)
	}

	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read saved file: %v", err)
	}
	if !bytes.Equal(onDisk, payload) {
		t.Errorf("saved %d bytes, want %d — audio must be streamed whole", len(onDisk), len(payload))
	}
}

func TestElevenLabs_AudioWithoutContentTypeIsStillWrittenToFile(t *testing.T) {
	// Raw MP3 with an ID3 header, but the server declares no type at all.
	payload := append([]byte("ID3\x04\x00\x00\x00\x00\x00\x00"), bytes.Repeat([]byte{0x11}, 4096)...)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header()["Content-Type"] = nil // suppress Go's own sniffing
		w.WriteHeader(http.StatusOK)
		w.Write(payload)
	}))
	defer srv.Close()

	e := newTestElevenLabs(t, srv.URL)
	res, err := e.handlePost(context.Background(), toolRequest(map[string]any{
		"path": "/v1/text-to-speech/voice123",
		"body": `{"text":"hallo"}`,
	}))
	if err != nil {
		t.Fatalf("handlePost: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected success, got %q", resultText(t, res))
	}

	text := resultText(t, res)
	if strings.Contains(text, "ID3") {
		t.Errorf("raw audio bytes leaked into the tool result:\n%.200s", text)
	}
	onDisk, err := os.ReadFile(savedPath(t, text))
	if err != nil {
		t.Fatalf("read saved file: %v", err)
	}
	if !bytes.Equal(onDisk, payload) {
		t.Errorf("saved %d bytes, want %d", len(onDisk), len(payload))
	}
}

func TestElevenLabs_AudioHonorsOutputFile(t *testing.T) {
	payload := []byte("fake-ogg-bytes")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/ogg")
		w.Write(payload)
	}))
	defer srv.Close()

	target := filepath.Join(t.TempDir(), "nested", "slide-01.ogg")
	e := newTestElevenLabs(t, srv.URL)
	res, err := e.handlePost(context.Background(), toolRequest(map[string]any{
		"path":        "/v1/text-to-speech/voice123",
		"body":        `{"text":"hallo"}`,
		"output_file": target,
	}))
	if err != nil {
		t.Fatalf("handlePost: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected success, got %q", resultText(t, res))
	}

	if got := savedPath(t, resultText(t, res)); got != target {
		t.Errorf("saved to %q, want %q", got, target)
	}
	onDisk, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read output_file: %v", err)
	}
	if !bytes.Equal(onDisk, payload) {
		t.Errorf("content = %q, want %q", onDisk, payload)
	}
}

func TestElevenLabs_ErrorResponseStaysInlineAndWritesNoFile(t *testing.T) {
	cases := []struct {
		name        string
		contentType string
		withTarget  bool
	}{
		{"json error with output_file", "application/json", true},
		{"json error without output_file", "application/json", false},
		// A failing request may still be labelled audio — the body is the error.
		{"audio content type without output_file", "audio/mpeg", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", tc.contentType)
				w.WriteHeader(http.StatusUnauthorized)
				w.Write([]byte(`{"detail":{"status":"invalid_api_key"}}`))
			}))
			defer srv.Close()

			e := newTestElevenLabs(t, srv.URL)
			args := map[string]any{
				"path": "/v1/text-to-speech/voice123",
				"body": `{"text":"hallo"}`,
			}
			target := filepath.Join(t.TempDir(), "should-not-exist.mp3")
			if tc.withTarget {
				args["output_file"] = target
			}

			res, err := e.handlePost(context.Background(), toolRequest(args))
			if err != nil {
				t.Fatalf("handlePost: %v", err)
			}

			text := resultText(t, res)
			if !strings.Contains(text, "401") || !strings.Contains(text, "invalid_api_key") {
				t.Errorf("error body should be inlined verbatim, got:\n%s", text)
			}
			if strings.Contains(text, "Saved to: ") {
				t.Errorf("error response must not be written to a file:\n%s", text)
			}
			if _, err := os.Stat(target); !os.IsNotExist(err) {
				t.Errorf("output_file was created for a non-2xx response (stat err = %v)", err)
			}
			entries, err := os.ReadDir(e.outputDir)
			if err != nil {
				t.Fatalf("read output dir: %v", err)
			}
			if len(entries) != 0 {
				t.Errorf("wrote %d file(s) into the output dir for a failed request", len(entries))
			}
		})
	}
}

func TestElevenLabs_LargeTextResponseIsMarkedTruncated(t *testing.T) {
	// JSON-wrapping endpoints (…/with-timestamps) can exceed the inline limit;
	// the caller has to learn that what it got is incomplete.
	payload := append([]byte(`{"audio_base64":"`), bytes.Repeat([]byte("A"), elevenlabsMaxBody)...)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(payload)
	}))
	defer srv.Close()

	e := newTestElevenLabs(t, srv.URL)
	res, err := e.handlePost(context.Background(), toolRequest(map[string]any{
		"path": "/v1/text-to-speech/voice123/with-timestamps",
		"body": `{"text":"hallo"}`,
	}))
	if err != nil {
		t.Fatalf("handlePost: %v", err)
	}
	text := resultText(t, res)
	if !strings.Contains(text, "truncated at") {
		t.Errorf("truncated response must say so, got tail:\n%.200s", text[max(0, len(text)-200):])
	}
	if !strings.Contains(text, "output_file") {
		t.Errorf("truncation note should point at output_file:\n%.200s", text[max(0, len(text)-200):])
	}
}

func TestElevenLabs_RejectsUnusableOutputFileBeforeSpendingCredits(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "audio/mpeg")
		w.Write([]byte("audio"))
	}))
	defer srv.Close()

	existing := filepath.Join(t.TempDir(), "taken.mp3")
	if err := os.WriteFile(existing, []byte("old"), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}

	e := newTestElevenLabs(t, srv.URL)
	for _, target := range []string{"relative/slide.mp3", existing} {
		res, err := e.handlePost(context.Background(), toolRequest(map[string]any{
			"path":        "/v1/text-to-speech/voice123",
			"body":        `{"text":"hallo"}`,
			"output_file": target,
		}))
		if err != nil {
			t.Fatalf("handlePost: %v", err)
		}
		if !res.IsError {
			t.Errorf("output_file %q should be rejected, got %q", target, resultText(t, res))
		}
	}
	if calls != 0 {
		t.Errorf("%d request(s) hit the API despite an unusable output_file — credits burned for nothing", calls)
	}
	if content, err := os.ReadFile(existing); err != nil || string(content) != "old" {
		t.Errorf("existing file was touched: content=%q err=%v", content, err)
	}
}

func TestElevenLabs_PostRejectsMissingArguments(t *testing.T) {
	e := newTestElevenLabs(t, "http://127.0.0.1:0")

	cases := []struct {
		name string
		args map[string]any
		want string
	}{
		{"no body", map[string]any{"path": "/v1/text-to-speech/voice123"}, "body is required"},
		{"empty body", map[string]any{"path": "/v1/text-to-speech/voice123", "body": ""}, "body is required"},
		{"no path", map[string]any{"body": `{"text":"hallo"}`}, "path is required"},
		{"neither", map[string]any{}, "path is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := e.handlePost(context.Background(), toolRequest(tc.args))
			if err != nil {
				t.Fatalf("handlePost: %v", err)
			}
			if !res.IsError {
				t.Fatalf("expected an error result, got %q", resultText(t, res))
			}
			if got := resultText(t, res); !strings.Contains(got, tc.want) {
				t.Errorf("error = %q, want it to mention %q", got, tc.want)
			}
		})
	}
}

func TestElevenLabs_GetSupportsOutputFileAndSendsNoBody(t *testing.T) {
	payload := []byte("history-audio")
	var gotBody []byte
	var gotContentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %q, want GET", r.Method)
		}
		gotBody, _ = io.ReadAll(r.Body)
		gotContentType = r.Header.Get("Content-Type")
		w.Header().Set("Content-Type", "audio/mpeg")
		w.Write(payload)
	}))
	defer srv.Close()

	target := filepath.Join(t.TempDir(), "history.mp3")
	e := newTestElevenLabs(t, srv.URL)
	res, err := e.handleGet(context.Background(), toolRequest(map[string]any{
		"path": "/v1/history/abc/audio",
		// A stray body argument must not turn this into a GET with payload.
		"body":        `{"text":"ignored"}`,
		"output_file": target,
	}))
	if err != nil {
		t.Fatalf("handleGet: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected success, got %q", resultText(t, res))
	}
	if len(gotBody) != 0 {
		t.Errorf("GET sent a body: %q", gotBody)
	}
	if gotContentType != "" {
		t.Errorf("GET sent Content-Type %q, want none", gotContentType)
	}
	onDisk, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read output_file: %v", err)
	}
	if !bytes.Equal(onDisk, payload) {
		t.Errorf("content = %q, want %q", onDisk, payload)
	}
}

func TestElevenLabs_RedirectToOtherHostDropsAPIKey(t *testing.T) {
	var gotKey string
	final := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("xi-api-key")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	}))
	defer final.Close()

	// 127.0.0.1 and localhost resolve to the same machine but differ as hosts,
	// which is exactly the cross-host case Go replays custom headers on.
	redirectTarget := strings.Replace(final.URL, "127.0.0.1", "localhost", 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, redirectTarget+"/v1/voices", http.StatusFound)
	}))
	defer srv.Close()

	e := newTestElevenLabs(t, srv.URL)
	res, err := e.handleGet(context.Background(), toolRequest(map[string]any{"path": "/v1/voices"}))
	if err != nil {
		t.Fatalf("handleGet: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected the redirect to be followed, got %q", resultText(t, res))
	}
	if gotKey != "" {
		t.Errorf("API key leaked to the redirect target: %q", gotKey)
	}
}
