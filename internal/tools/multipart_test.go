package tools

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/smnhffmnn/mux/internal/config"
)

// uploadCapture is what a test server saw of one multipart request.
type uploadCapture struct {
	method        string
	header        http.Header
	contentLength int64
	fields        map[string]string
	fileField     string
	fileName      string
	fileType      string
	fileBytes     []byte
	parseErr      error
}

func captureUpload(r *http.Request) uploadCapture {
	c := uploadCapture{
		method:        r.Method,
		header:        r.Header.Clone(),
		contentLength: r.ContentLength,
		fields:        map[string]string{},
	}
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		c.parseErr = err
		return c
	}
	for k, v := range r.MultipartForm.Value {
		c.fields[k] = v[0]
	}
	for field, fhs := range r.MultipartForm.File {
		c.fileField = field
		c.fileName = fhs[0].Filename
		c.fileType = fhs[0].Header.Get("Content-Type")
		f, err := fhs[0].Open()
		if err != nil {
			c.parseErr = err
			return c
		}
		c.fileBytes, _ = io.ReadAll(f)
		f.Close()
	}
	return c
}

// uploadServer records every multipart request it receives and answers with
// status and body.
func uploadServer(t *testing.T, status int, body string) (*httptest.Server, *uploadCapture, *int32) {
	t.Helper()
	var got uploadCapture
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		got = captureUpload(r)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, &got, &calls
}

func writeTestFile(t *testing.T, name string, content []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, content, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

var pngMagic = append([]byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}, []byte("not really a png but enough to sniff")...)

func newTestHTTP(t *testing.T, conn config.Connection) *HTTP {
	t.Helper()
	if conn.Type == "" {
		conn.Type = config.TypeHTTP
	}
	h, err := NewHTTP(conn, nil)
	if err != nil {
		t.Fatalf("NewHTTP: %v", err)
	}
	return h
}

func callRequest(t *testing.T, h *HTTP, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	res, err := h.handleRequest(context.Background(), mcp.CallToolRequest{Params: mcp.CallToolParams{Arguments: args}})
	if err != nil {
		t.Fatalf("handleRequest: %v", err)
	}
	return res
}

func TestHTTP_UploadMultipart(t *testing.T) {
	srv, got, _ := uploadServer(t, http.StatusCreated, `{"id":42}`)
	h := newTestHTTP(t, config.Connection{URL: srv.URL, Token: "secret"})
	file := writeTestFile(t, "banner.png", pngMagic)

	res := callRequest(t, h, map[string]any{
		"method":      "POST",
		"path":        "/api/public-assets",
		"file_path":   file,
		"form_fields": `{"title": "Banner", "width": 1200, "public": true, "ratio": 1.5}`,
	})
	if res.IsError {
		t.Fatalf("expected success, got %s", resultText(t, res))
	}

	if got.parseErr != nil {
		t.Fatalf("server could not parse multipart body: %v", got.parseErr)
	}
	if got.method != http.MethodPost {
		t.Errorf("method = %s, want POST", got.method)
	}
	if ct := got.header.Get("Content-Type"); !strings.HasPrefix(ct, "multipart/form-data; boundary=") {
		t.Errorf("Content-Type = %q, want multipart/form-data with boundary", ct)
	}
	if v := got.header.Get("Authorization"); v != "Bearer secret" {
		t.Errorf("Authorization = %q, want bearer token", v)
	}
	if v := got.header.Get("Accept"); v != "application/json" {
		t.Errorf("Accept = %q, want application/json", v)
	}
	if got.contentLength <= int64(len(pngMagic)) {
		t.Errorf("Content-Length = %d, want an exact length above the file size (no chunked upload)", got.contentLength)
	}

	want := map[string]string{"title": "Banner", "width": "1200", "public": "true", "ratio": "1.5"}
	for k, v := range want {
		if got.fields[k] != v {
			t.Errorf("field %s = %q, want %q", k, got.fields[k], v)
		}
	}
	if got.fileField != "file" {
		t.Errorf("file field = %q, want default \"file\"", got.fileField)
	}
	if got.fileName != "banner.png" {
		t.Errorf("filename = %q, want banner.png", got.fileName)
	}
	if got.fileType != "image/png" {
		t.Errorf("file part Content-Type = %q, want image/png (by extension)", got.fileType)
	}
	if string(got.fileBytes) != string(pngMagic) {
		t.Errorf("file bytes differ: got %d bytes, want %d", len(got.fileBytes), len(pngMagic))
	}

	text := resultText(t, res)
	if !strings.HasPrefix(text, "HTTP 201 ") {
		t.Errorf("result should start with the status line:\n%s", text)
	}
	wantNote := `Uploaded banner.png as "file": ` + strconv.Itoa(len(pngMagic)) + " bytes, image/png"
	if !strings.Contains(text, wantNote) {
		t.Errorf("result lacks upload note %q:\n%s", wantNote, text)
	}
	if !strings.HasSuffix(text, `{"id":42}`) {
		t.Errorf("result should end with the response body:\n%s", text)
	}
}

func TestHTTP_UploadCustomFieldAndSniffedType(t *testing.T) {
	srv, got, _ := uploadServer(t, http.StatusOK, `{}`)
	h := newTestHTTP(t, config.Connection{URL: srv.URL})
	file := writeTestFile(t, "blob", pngMagic) // no extension → sniffed

	res := callRequest(t, h, map[string]any{
		"method":     "PUT",
		"path":       "/upload",
		"file_path":  file,
		"file_field": "image",
	})
	if res.IsError {
		t.Fatalf("expected success, got %s", resultText(t, res))
	}
	if got.parseErr != nil {
		t.Fatalf("server could not parse multipart body: %v", got.parseErr)
	}
	if got.method != http.MethodPut {
		t.Errorf("method = %s, want PUT", got.method)
	}
	if got.fileField != "image" {
		t.Errorf("file field = %q, want image", got.fileField)
	}
	if got.fileType != "image/png" {
		t.Errorf("file part Content-Type = %q, want image/png (sniffed)", got.fileType)
	}
	if len(got.fields) != 0 {
		t.Errorf("unexpected text fields without form_fields: %v", got.fields)
	}
}

func TestHTTP_UploadHomeExpansion(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.WriteFile(filepath.Join(home, "notes.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	srv, got, _ := uploadServer(t, http.StatusOK, `{}`)
	h := newTestHTTP(t, config.Connection{URL: srv.URL})

	res := callRequest(t, h, map[string]any{
		"method":    "POST",
		"path":      "/upload",
		"file_path": "~/notes.txt",
	})
	if res.IsError {
		t.Fatalf("expected success, got %s", resultText(t, res))
	}
	if got.fileName != "notes.txt" || string(got.fileBytes) != "hello" {
		t.Errorf("file not delivered from expanded home path: name=%q bytes=%q", got.fileName, got.fileBytes)
	}
	if !strings.HasPrefix(got.fileType, "text/plain") {
		t.Errorf("file part Content-Type = %q, want text/plain (by extension)", got.fileType)
	}
}

func TestHTTP_UploadRejectsBadArguments(t *testing.T) {
	srv, _, calls := uploadServer(t, http.StatusOK, `{}`)
	h := newTestHTTP(t, config.Connection{URL: srv.URL})
	file := writeTestFile(t, "a.txt", []byte("a"))
	dir := t.TempDir()

	cases := []struct {
		name string
		args map[string]any
		want string
	}{
		{"body with file", map[string]any{"file_path": file, "body": `{"x":1}`}, "mutually exclusive"},
		{"form_fields without file", map[string]any{"form_fields": `{"x":"1"}`}, "form_fields requires file_path"},
		{"file_field without file", map[string]any{"file_field": "image"}, "file_field requires file_path"},
		{"relative path", map[string]any{"file_path": "relative/a.txt"}, "absolute path"},
		{"missing file", map[string]any{"file_path": filepath.Join(dir, "nope.txt")}, "not found"},
		{"directory", map[string]any{"file_path": dir}, "not a regular file"},
		{"nested form field", map[string]any{"file_path": file, "form_fields": `{"meta": {"a": 1}}`}, "form_fields.meta must be a string, number or boolean"},
		{"array form field", map[string]any{"file_path": file, "form_fields": `{"tags": ["a"]}`}, "form_fields.tags must be"},
		{"null form field", map[string]any{"file_path": file, "form_fields": `{"x": null}`}, "form_fields.x is null"},
		{"form_fields not an object", map[string]any{"file_path": file, "form_fields": `[1]`}, "must be a JSON object"},
		{"form_fields null", map[string]any{"file_path": file, "form_fields": `null`}, "must be a JSON object"},
		{"form_fields invalid JSON", map[string]any{"file_path": file, "form_fields": `{"x":`}, "must be a JSON object"},
		{"form_fields trailing garbage", map[string]any{"file_path": file, "form_fields": `{"x":"1"} {"y":"2"}`}, "single JSON object"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args := map[string]any{"method": "POST", "path": "/upload"}
			for k, v := range tc.args {
				args[k] = v
			}
			res := callRequest(t, h, args)
			if !res.IsError {
				t.Fatalf("expected error, got success: %s", resultText(t, res))
			}
			if text := resultText(t, res); !strings.Contains(text, tc.want) {
				t.Errorf("error %q should mention %q", text, tc.want)
			}
		})
	}
	if n := atomic.LoadInt32(calls); n != 0 {
		t.Errorf("%d request(s) reached the server despite invalid arguments", n)
	}
}

func TestHTTP_UploadNotOnGet(t *testing.T) {
	srv, _, calls := uploadServer(t, http.StatusOK, `{}`)
	h := newTestHTTP(t, config.Connection{URL: srv.URL})
	file := writeTestFile(t, "a.txt", []byte("a"))

	res, err := h.handleGet(context.Background(), mcp.CallToolRequest{Params: mcp.CallToolParams{
		Arguments: map[string]any{"path": "/x", "file_path": file},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(resultText(t, res), "not supported on GET") {
		t.Errorf("GET with file_path should be rejected, got %s", resultText(t, res))
	}
	if n := atomic.LoadInt32(calls); n != 0 {
		t.Errorf("%d request(s) reached the server", n)
	}
}

// A 307 to another host must carry the full body again (GetBody) and must
// drop the token_header credential (CheckRedirect) — both on one request.
func TestHTTP_UploadFollowsRedirectWithBody(t *testing.T) {
	target, got, _ := uploadServer(t, http.StatusCreated, `{"ok":true}`)

	var firstKey string
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		firstKey = r.Header.Get("x-api-key")
		// Drain so the client can reuse the connection; the body is replayed
		// from disk for the redirected request either way.
		io.Copy(io.Discard, r.Body)
		http.Redirect(w, r, target.URL+"/final", http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()

	h := newTestHTTP(t, config.Connection{URL: redirector.URL, Token: "k-123", TokenHeader: "x-api-key"})
	file := writeTestFile(t, "doc.pdf", []byte("%PDF-1.4 fake"))

	res := callRequest(t, h, map[string]any{
		"method":      "POST",
		"path":        "/start",
		"file_path":   file,
		"form_fields": `{"name": "doc"}`,
	})
	if res.IsError {
		t.Fatalf("expected success, got %s", resultText(t, res))
	}
	if text := resultText(t, res); !strings.HasPrefix(text, "HTTP 201 ") {
		t.Fatalf("redirect was not followed (GetBody missing?):\n%s", text)
	}

	if firstKey != "k-123" {
		t.Errorf("first hop x-api-key = %q, want the token", firstKey)
	}
	if got.parseErr != nil {
		t.Fatalf("redirected request body unreadable: %v", got.parseErr)
	}
	if v := got.header.Get("x-api-key"); v != "" {
		t.Errorf("token header %q leaked to the redirect target", v)
	}
	if string(got.fileBytes) != "%PDF-1.4 fake" || got.fields["name"] != "doc" {
		t.Errorf("replayed body incomplete: file=%q fields=%v", got.fileBytes, got.fields)
	}
	if got.fileType != "application/pdf" {
		t.Errorf("file part Content-Type = %q, want application/pdf", got.fileType)
	}
}

func TestHTTP_UploadOutputFile(t *testing.T) {
	srv, got, _ := uploadServer(t, http.StatusOK, `{"stored":"yes"}`)
	h := newTestHTTP(t, config.Connection{URL: srv.URL})
	file := writeTestFile(t, "a.txt", []byte("payload"))
	out := filepath.Join(t.TempDir(), "response.json")

	res := callRequest(t, h, map[string]any{
		"method":      "POST",
		"path":        "/upload",
		"file_path":   file,
		"output_file": out,
	})
	if res.IsError {
		t.Fatalf("expected success, got %s", resultText(t, res))
	}
	if got.parseErr != nil {
		t.Fatalf("server could not parse multipart body: %v", got.parseErr)
	}
	saved, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read output_file: %v", err)
	}
	if string(saved) != `{"stored":"yes"}` {
		t.Errorf("output_file content = %q", saved)
	}
	text := resultText(t, res)
	if !strings.Contains(text, "Saved to: "+out) {
		t.Errorf("result lacks saved path:\n%s", text)
	}
	if !strings.Contains(text, `Uploaded a.txt as "file": 7 bytes`) {
		t.Errorf("result lacks upload note:\n%s", text)
	}
}

func TestHTTP_UploadOverridesConfiguredContentType(t *testing.T) {
	srv, got, _ := uploadServer(t, http.StatusOK, `{}`)
	h := newTestHTTP(t, config.Connection{
		URL:     srv.URL,
		Headers: map[string]string{"Content-Type": "application/json", "X-Extra": "1"},
	})
	file := writeTestFile(t, "a.txt", []byte("a"))

	res := callRequest(t, h, map[string]any{"method": "POST", "path": "/upload", "file_path": file})
	if res.IsError {
		t.Fatalf("expected success, got %s", resultText(t, res))
	}
	if got.parseErr != nil {
		t.Fatalf("configured Content-Type header broke the upload: %v", got.parseErr)
	}
	if v := got.header.Get("X-Extra"); v != "1" {
		t.Errorf("other custom headers must still be sent, X-Extra = %q", v)
	}
}

func TestHTTP_JSONBodyUnchangedWithoutFile(t *testing.T) {
	var gotCT, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCT = r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	h := newTestHTTP(t, config.Connection{URL: srv.URL})

	res := callRequest(t, h, map[string]any{"method": "POST", "path": "/x", "body": `{"a":1}`})
	if res.IsError {
		t.Fatalf("expected success, got %s", resultText(t, res))
	}
	if gotCT != "application/json" || gotBody != `{"a":1}` {
		t.Errorf("JSON mode changed: Content-Type=%q body=%q", gotCT, gotBody)
	}
	if text := resultText(t, res); strings.Contains(text, "Uploaded") {
		t.Errorf("JSON mode must not carry an upload note:\n%s", text)
	}
}

func TestClientFor_UploadDropsOverallTimeout(t *testing.T) {
	base := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{}}
	if c := clientFor(base, nil); c != base {
		t.Errorf("JSON mode should use the base client as is")
	}
	up := clientFor(base, &multipartUpload{})
	if up.Timeout != 0 {
		t.Errorf("upload client Timeout = %v, want 0", up.Timeout)
	}
	if up.Transport != base.Transport {
		t.Errorf("upload client must share the transport (dialer, tunnel)")
	}
	if base.Timeout != 30*time.Second {
		t.Errorf("base client must stay untouched, Timeout = %v", base.Timeout)
	}
}

// The dedicated API connectors share the builder; each sends the file under
// its own default field with its own auth header.
func TestConnectors_Upload(t *testing.T) {
	type connector struct {
		name       string
		toolName   string
		wantField  string
		authHeader string
		authValue  string
		build      func(url string) (interface{ Tools() []ToolDef }, error)
	}
	connectors := []connector{
		{"openai", "request", "file", "Authorization", "Bearer k", func(u string) (interface{ Tools() []ToolDef }, error) {
			return NewOpenAI(config.Connection{URL: u, Token: "k"}, nil)
		}},
		{"recraft", "post", "file", "Authorization", "Bearer k", func(u string) (interface{ Tools() []ToolDef }, error) {
			return NewRecraft(config.Connection{URL: u, Token: "k"}, nil)
		}},
		{"ideogram", "post", "image", "Api-Key", "k", func(u string) (interface{ Tools() []ToolDef }, error) {
			return NewIdeogram(config.Connection{URL: u, Token: "k"}, nil)
		}},
	}

	for _, c := range connectors {
		t.Run(c.name, func(t *testing.T) {
			srv, got, _ := uploadServer(t, http.StatusOK, `{"ok":true}`)
			conn, err := c.build(srv.URL)
			if err != nil {
				t.Fatal(err)
			}
			var tool *ToolDef
			for i := range conn.Tools() {
				if conn.Tools()[i].Tool.Name == c.toolName {
					tool = &conn.Tools()[i]
				}
			}
			if tool == nil {
				t.Fatalf("tool %q not found", c.toolName)
			}
			for _, p := range []string{"file_path", "file_field", "form_fields"} {
				if _, ok := tool.Tool.InputSchema.Properties[p]; !ok {
					t.Errorf("tool %q lacks parameter %s", c.toolName, p)
				}
			}

			file := writeTestFile(t, "in.png", pngMagic)
			res, err := tool.Handler(context.Background(), mcp.CallToolRequest{Params: mcp.CallToolParams{
				Arguments: map[string]any{
					"method":      "POST",
					"path":        "/v1/upload",
					"file_path":   file,
					"form_fields": `{"prompt": "hi"}`,
				},
			}})
			if err != nil {
				t.Fatal(err)
			}
			if res.IsError {
				t.Fatalf("expected success, got %s", resultText(t, res))
			}
			if got.parseErr != nil {
				t.Fatalf("server could not parse multipart body: %v", got.parseErr)
			}
			if got.fileField != c.wantField {
				t.Errorf("file field = %q, want %q", got.fileField, c.wantField)
			}
			if v := got.header.Get(c.authHeader); v != c.authValue {
				t.Errorf("%s = %q, want %q", c.authHeader, v, c.authValue)
			}
			if got.fields["prompt"] != "hi" || string(got.fileBytes) != string(pngMagic) {
				t.Errorf("body incomplete: fields=%v file=%d bytes", got.fields, len(got.fileBytes))
			}
			if !strings.Contains(resultText(t, res), "Uploaded in.png as") {
				t.Errorf("result lacks upload note:\n%s", resultText(t, res))
			}

			// body stays a JSON request
			res, err = tool.Handler(context.Background(), mcp.CallToolRequest{Params: mcp.CallToolParams{
				Arguments: map[string]any{"method": "POST", "path": "/v1/x", "body": `{"a":1}`},
			}})
			if err != nil {
				t.Fatal(err)
			}
			if res.IsError {
				t.Fatalf("JSON request failed: %s", resultText(t, res))
			}
		})
	}
}

func TestConnectors_PostNeedsBodyOrFile(t *testing.T) {
	srv, _, calls := uploadServer(t, http.StatusOK, `{}`)
	recraft, err := NewRecraft(config.Connection{URL: srv.URL, Token: "k"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ideogram, err := NewIdeogram(config.Connection{URL: srv.URL, Token: "k"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for name, handler := range map[string]func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error){"recraft": recraft.handlePost, "ideogram": ideogram.handlePost} {
		res, err := handler(context.Background(), mcp.CallToolRequest{Params: mcp.CallToolParams{
			Arguments: map[string]any{"path": "/x"},
		}})
		if err != nil {
			t.Fatal(err)
		}
		if !res.IsError || !strings.Contains(resultText(t, res), "body or file_path is required") {
			t.Errorf("%s: post without body and file_path should be rejected, got %s", name, resultText(t, res))
		}
	}
	if n := atomic.LoadInt32(calls); n != 0 {
		t.Errorf("%d request(s) reached the server", n)
	}
}
