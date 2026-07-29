package tools

import (
	"bytes"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// savedPath extracts the file path a save-to-file result reports.
func savedPath(t *testing.T, text string) string {
	t.Helper()
	for _, line := range strings.Split(text, "\n") {
		if p, ok := strings.CutPrefix(line, "Saved to: "); ok {
			return strings.TrimSpace(p)
		}
	}
	t.Fatalf("no %q line in result:\n%s", "Saved to: ", text)
	return ""
}

func responseWithBody(contentType string, body []byte) *http.Response {
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     http.Header{},
		Body:       io.NopCloser(bytes.NewReader(body)),
	}
	if contentType != "" {
		resp.Header.Set("Content-Type", contentType)
	}
	return resp
}

func TestValidateOutputFile(t *testing.T) {
	existing := filepath.Join(t.TempDir(), "taken.mp3")
	if err := os.WriteFile(existing, []byte("x"), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}

	if _, err := validateOutputFile("relative/path.mp3"); err == nil {
		t.Error("relative path should be rejected before the request is sent")
	}
	if _, err := validateOutputFile(existing); err == nil {
		t.Error("existing file should be rejected before the request is sent")
	}
	fresh := filepath.Join(t.TempDir(), "sub", "new.mp3")
	got, err := validateOutputFile(fresh)
	if err != nil {
		t.Errorf("fresh path rejected: %v", err)
	}
	if got != fresh {
		t.Errorf("cleaned path = %q, want %q", got, fresh)
	}
}

func TestSaveResponseToGeneratedFile_EmptyBodyWritesNothing(t *testing.T) {
	dir := t.TempDir()
	res, err := saveResponseToGeneratedFile(responseWithBody("audio/mpeg", nil), "audio/mpeg", dir, "elevenlabs-*.mp3")
	if err != nil {
		t.Fatalf("saveResponseToGeneratedFile: %v", err)
	}
	if !res.IsError {
		t.Errorf("empty body should be an error result, got %q", resultText(t, res))
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("left %d file(s) behind, want none", len(entries))
	}
}

func TestSaveResponseToGeneratedFile_CreatesDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "output")
	payload := []byte("audio-bytes")

	res, err := saveResponseToGeneratedFile(responseWithBody("audio/mpeg", payload), "audio/mpeg", dir, "elevenlabs-*.mp3")
	if err != nil {
		t.Fatalf("saveResponseToGeneratedFile: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected success, got %q", resultText(t, res))
	}

	path := savedPath(t, resultText(t, res))
	if filepath.Dir(path) != dir {
		t.Errorf("wrote to %q, want directory %q", path, dir)
	}
	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read saved file: %v", err)
	}
	if !bytes.Equal(onDisk, payload) {
		t.Errorf("content = %q, want %q", onDisk, payload)
	}
}

func TestSniffContentType(t *testing.T) {
	mp3 := append([]byte("ID3\x04\x00\x00\x00\x00\x00\x00"), bytes.Repeat([]byte{0x11}, 600)...)

	cases := []struct {
		name        string
		contentType string
		body        []byte
		want        string
	}{
		{"declared type wins", "audio/mpeg", []byte(`{"not":"audio"}`), "audio/mpeg"},
		{"sniffs mp3", "", mp3, "audio/mpeg"},
		{"sniffs text", "", []byte(`{"ok":true}`), "text/plain; charset=utf-8"},
		{"empty body", "", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := responseWithBody(tc.contentType, tc.body)
			if got := sniffContentType(resp); got != tc.want {
				t.Errorf("sniffContentType() = %q, want %q", got, tc.want)
			}
			// Whatever was peeked must still be readable.
			rest, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("read body after sniff: %v", err)
			}
			if !bytes.Equal(rest, tc.body) {
				t.Errorf("body after sniff = %d bytes, want %d", len(rest), len(tc.body))
			}
		})
	}
}

func TestBinaryResponseExt(t *testing.T) {
	cases := []struct {
		contentType string
		wantExt     string
		wantBinary  bool
	}{
		{"audio/mpeg", ".mp3", true},
		{"audio/mpeg; charset=utf-8", ".mp3", true},
		{"AUDIO/MPEG", ".mp3", true},
		{"audio/ogg", ".ogg", true},
		{"audio/x-wav", ".wav", true},
		{"audio/webm", ".webm", true},
		{"application/octet-stream", ".bin", true},
		{"audio/../../etc/passwd", ".bin", true},
		{"application/json", "", false},
		{"application/json; charset=utf-8", "", false},
		// Broken parameter: the media type still parses and must stay textual.
		{`application/json; charset="utf-8`, "", false},
		{"application/json; boundary", "", false},
		{"application/x-ndjson", "", false},
		{"application/yaml", "", false},
		{"application/problem+json", "", false},
		{"text/plain", "", false},
		{"", "", false},
	}
	for _, tc := range cases {
		ext, binary := binaryResponseExt(tc.contentType)
		if binary != tc.wantBinary || ext != tc.wantExt {
			t.Errorf("binaryResponseExt(%q) = (%q, %v), want (%q, %v)",
				tc.contentType, ext, binary, tc.wantExt, tc.wantBinary)
		}
	}
}
