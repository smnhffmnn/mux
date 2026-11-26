package tools

import (
	"bytes"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
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

// captureLog collects what the standard logger writes during the test.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev, flags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(prev)
		log.SetFlags(flags)
	})
	return &buf
}

// failingReader yields some bytes and then an error, like a body that breaks
// off mid-transfer.
type failingReader struct{ sent bool }

func (r *failingReader) Read(p []byte) (int, error) {
	if !r.sent {
		r.sent = true
		return copy(p, "partial"), nil
	}
	return 0, errors.New("connection reset")
}

func TestWriteOutputFile(t *testing.T) {
	logged := captureLog(t)
	dir := t.TempDir()

	// A new file in a directory that does not exist yet, and one log line
	// naming where it went.
	dest := filepath.Join(dir, "new", "report.pdf")
	path, n, err := writeOutputFile(dest, strings.NewReader("content"))
	if err != nil || path != dest || n != 7 {
		t.Fatalf("writeOutputFile = %q, %d, %v", path, n, err)
	}
	if got, _ := os.ReadFile(dest); string(got) != "content" {
		t.Fatalf("file holds %q", got)
	}
	if want := "[output] wrote " + strconv.Quote(dest) + " (7 bytes)\n"; logged.String() != want {
		t.Errorf("log = %q, want %q", logged.String(), want)
	}

	// The write itself refuses an existing file — the early check in
	// validateOutputFile may be seconds old by now.
	logged.Reset()
	if _, _, err := writeOutputFile(dest, strings.NewReader("other")); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("an existing file must be refused, got %v", err)
	}
	if got, _ := os.ReadFile(dest); string(got) != "content" {
		t.Errorf("the existing file was changed: %q", got)
	}

	// A source that breaks off leaves no half-written file behind, so a
	// retry with the same path is not blocked.
	broken := filepath.Join(dir, "broken.bin")
	if _, _, err := writeOutputFile(broken, &failingReader{}); err == nil || !strings.Contains(err.Error(), "write file") {
		t.Fatalf("a failing source must be an error, got %v", err)
	}
	if _, err := os.Stat(broken); err == nil {
		t.Errorf("the half-written file was left behind")
	}

	if _, _, err := writeOutputFile("relative.txt", strings.NewReader("x")); err == nil || !strings.Contains(err.Error(), "absolute path") {
		t.Fatalf("a relative path must be refused, got %v", err)
	}
	if logged.Len() != 0 {
		t.Errorf("nothing was written, yet the log says: %q", logged.String())
	}
}

// A path is free-form text from a tool argument: a line break in it must not
// start a log line of its own.
func TestLogFileWrittenQuotesThePath(t *testing.T) {
	logged := captureLog(t)
	logFileWritten("/tmp/a\n[output] wrote \"/etc/passwd\" (1 bytes)", 3)
	if got := logged.String(); strings.Count(got, "\n") != 1 || !strings.Contains(got, `\n`) {
		t.Errorf("the path must stay on one line, escaped: %q", got)
	}
}
