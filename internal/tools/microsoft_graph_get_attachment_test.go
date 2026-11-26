package tools

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

// get_attachment against the fake Graph: where the bytes of a received
// attachment end up — a blob, a path on the machine running mux, or inline
// as base64 — and which destinations are refused before anything is fetched.

const pptxType = "application/vnd.openxmlformats-officedocument.presentationml.presentation"

// attachmentBytes is n bytes of a fixed pattern, so a file on disk can be
// compared byte for byte without randomness in the test.
func attachmentBytes(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte((i*31 + 7) % 251)
	}
	return b
}

// receive registers a file attachment of a received mail on the fake.
func (g *fakeGraph) receive(msg, att, name, contentType string, content []byte) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.received[msg+"/"+att] = map[string]any{
		"@odata.type":  "#microsoft.graph.fileAttachment",
		"id":           att,
		"name":         name,
		"contentType":  contentType,
		"size":         len(content) + 180, // Graph's size counts more than the content
		"isInline":     false,
		"contentBytes": base64.StdEncoding.EncodeToString(content),
	}
}

// attachmentFetches counts the requests for single attachments.
func (g *fakeGraph) attachmentFetches() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := 0
	for _, r := range g.requests {
		if strings.HasPrefix(r, "GET /v1.0/me/messages/") && strings.Contains(r, "/attachments/") {
			n++
		}
	}
	return n
}

func getAttachment(t *testing.T, mg *MicrosoftGraph, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	res, err := mg.handleGetAttachment(t.Context(), toolRequest(args))
	if err != nil {
		t.Fatalf("handler returned a Go error: %v", err)
	}
	return res
}

func decodeAttachmentResult(t *testing.T, text string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("result is not JSON: %v\n%s", err, text)
	}
	return out
}

// The case behind the parameter: a stdio instance (no listener, no store)
// whose agent cannot take a megabyte of base64 from the response, but
// shares a folder with the machine mux runs on.
func TestGraphGetAttachmentToPathWithoutBlobStore(t *testing.T) {
	useTestBlobStore(t)
	SetBlobStore(nil)
	g := newFakeGraph(t)
	mg := g.client()
	content := attachmentBytes(1 << 20)
	g.receive("m1", "a1", "Quarterly review.pptx", pptxType, content)

	// The directory does not exist yet: it is created.
	dest := filepath.Join(t.TempDir(), "shared", "Quarterly review.pptx")
	res := getAttachment(t, mg, map[string]any{"message_id": "m1", "attachment_id": "a1", "output_file": dest})
	text := resultText(t, res)
	if res.IsError {
		t.Fatalf("save to path failed:\n%s", text)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("file not written: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("file content differs: %d bytes on disk, %d sent", len(got), len(content))
	}
	out := decodeAttachmentResult(t, text)
	if out["path"] != dest || out["bytesWritten"] != float64(len(content)) || out["name"] != "Quarterly review.pptx" || out["contentType"] != pptxType {
		t.Errorf("result does not describe the file: %v", out)
	}
	if _, ok := out["contentBytes"]; ok || len(text) > 2048 {
		t.Errorf("the bytes must not come back in the response (%d characters):\n%.300s", len(text), text)
	}
}

// With a store the path still wins: the agent asked for a file, and a blob
// nobody fetches would only take room until it expires.
func TestGraphGetAttachmentToPathWithBlobStore(t *testing.T) {
	store := useTestBlobStore(t)
	g := newFakeGraph(t)
	mg := g.client()
	content := attachmentBytes(4096)
	g.receive("m1", "a1", "invoice.pdf", "application/pdf", content)

	dest := filepath.Join(t.TempDir(), "invoice.pdf")
	res := getAttachment(t, mg, map[string]any{"message_id": "m1", "attachment_id": "a1", "output_file": dest})
	if res.IsError {
		t.Fatalf("save to path failed:\n%s", resultText(t, res))
	}
	if got, _ := os.ReadFile(dest); !bytes.Equal(got, content) {
		t.Fatalf("file content differs")
	}
	if n := len(store.List()); n != 0 {
		t.Errorf("a path must not leave a blob behind, store holds %d", n)
	}
}

// An image saved to a path is a file like any other: no image content in
// the result, which is what output_file is there to avoid.
func TestGraphGetAttachmentImageToPath(t *testing.T) {
	useTestBlobStore(t)
	g := newFakeGraph(t)
	mg := g.client()
	content := append([]byte("\x89PNG\r\n\x1a\n"), attachmentBytes(2048)...)
	g.receive("m1", "img", "photo.png", "image/png", content)

	dest := filepath.Join(t.TempDir(), "photo.png")
	res := getAttachment(t, mg, map[string]any{"message_id": "m1", "attachment_id": "img", "output_file": dest})
	if res.IsError {
		t.Fatalf("save to path failed:\n%s", resultText(t, res))
	}
	if len(res.Content) != 1 {
		t.Errorf("result carries %d content parts, want the text only", len(res.Content))
	}
	if got, _ := os.ReadFile(dest); !bytes.Equal(got, content) {
		t.Fatalf("file content differs")
	}
}

// Without output_file nothing changes: a blob with a store, the image also
// to look at; without a store base64 — now with the way out named.
func TestGraphGetAttachmentDefaults(t *testing.T) {
	store := useTestBlobStore(t)
	g := newFakeGraph(t)
	mg := g.client()
	content := attachmentBytes(3000)
	g.receive("m1", "a1", "notes.txt", "text/plain", content)
	png := append([]byte("\x89PNG\r\n\x1a\n"), attachmentBytes(100)...)
	g.receive("m1", "img", "photo.png", "image/png", png)

	res := getAttachment(t, mg, map[string]any{"message_id": "m1", "attachment_id": "a1"})
	text := resultText(t, res)
	if res.IsError {
		t.Fatalf("default with store failed:\n%s", text)
	}
	out := decodeAttachmentResult(t, text)
	id, _ := out["blob"].(string)
	if id == "" || !bytes.Equal(readBlob(t, store, id), content) {
		t.Fatalf("default with store must be a blob holding the bytes: %v", out)
	}

	res = getAttachment(t, mg, map[string]any{"message_id": "m1", "attachment_id": "img"})
	if res.IsError || len(res.Content) != 2 {
		t.Fatalf("an image comes back as text plus image content, got %d parts", len(res.Content))
	}

	// The same blob answer when "blob" is asked for by name.
	res = getAttachment(t, mg, map[string]any{"message_id": "m1", "attachment_id": "a1", "output_file": "blob"})
	out = decodeAttachmentResult(t, resultText(t, res))
	if id, _ := out["blob"].(string); res.IsError || id == "" || !bytes.Equal(readBlob(t, store, id), content) {
		t.Fatalf("output_file \"blob\" must store a blob: %v", out)
	}

	SetBlobStore(nil)
	res = getAttachment(t, mg, map[string]any{"message_id": "m1", "attachment_id": "a1"})
	text = resultText(t, res)
	if res.IsError {
		t.Fatalf("default without store failed:\n%s", text)
	}
	out = decodeAttachmentResult(t, text)
	if out["contentBytes"] != base64.StdEncoding.EncodeToString(content) {
		t.Errorf("without a store the bytes stay inline as base64")
	}
	if note, _ := out["note"].(string); !strings.Contains(note, "output_file") {
		t.Errorf("the inline answer must name output_file as the way out, note = %q", note)
	}
	// The base64 comes last: a client that shortens a long result keeps the
	// start, and the start has to say what the attachment is and the way out.
	bytesAt := strings.Index(text, `"contentBytes"`)
	for _, key := range []string{`"id"`, `"name"`, `"contentType"`, `"note"`} {
		if at := strings.Index(text, key); at < 0 || at > bytesAt {
			t.Errorf("%s must come before contentBytes (at %d, contentBytes at %d)", key, at, bytesAt)
		}
	}
}

// A destination that cannot work is refused before the attachment is
// downloaded: no round trip, and nothing on disk changes.
func TestGraphGetAttachmentOutputFileCheckedFirst(t *testing.T) {
	useTestBlobStore(t)
	g := newFakeGraph(t)
	mg := g.client()
	g.receive("m1", "a1", "deck.pptx", pptxType, attachmentBytes(100))

	existing := filepath.Join(t.TempDir(), "deck.pptx")
	if err := os.WriteFile(existing, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, outputFile, want string
		noStore                bool
	}{
		{"existing file", existing, "already exists", false},
		{"relative path", "deck.pptx", "must be an absolute path", false},
		{"blob without store", "blob", "no blob store", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.noStore {
				useTestBlobStore(t)
				SetBlobStore(nil)
			}
			before := g.attachmentFetches()
			res := getAttachment(t, mg, map[string]any{"message_id": "m1", "attachment_id": "a1", "output_file": c.outputFile})
			if text := resultText(t, res); !res.IsError || !strings.Contains(text, c.want) {
				t.Fatalf("want an error containing %q, got:\n%s", c.want, text)
			}
			if g.attachmentFetches() != before {
				t.Errorf("the attachment was fetched although the destination was refused")
			}
		})
	}
	if got, _ := os.ReadFile(existing); string(got) != "keep me" {
		t.Errorf("an existing file must never be overwritten, now %q", got)
	}
}

// Item and reference attachments have no bytes: output_file is an error
// for them, not a silent JSON answer with no file behind it.
func TestGraphGetAttachmentOutputFileNeedsAFileAttachment(t *testing.T) {
	useTestBlobStore(t)
	g := newFakeGraph(t)
	mg := g.client()
	g.mu.Lock()
	g.received["m1/item"] = map[string]any{"@odata.type": "#microsoft.graph.itemAttachment", "id": "item", "name": "Forwarded mail", "contentType": "message/rfc822", "size": 5120}
	g.received["m1/ref"] = map[string]any{"@odata.type": "#microsoft.graph.referenceAttachment", "id": "ref", "name": "Plan.docx", "size": 0}
	g.mu.Unlock()

	for _, att := range []string{"item", "ref"} {
		for _, outputFile := range []string{filepath.Join(t.TempDir(), att+".bin"), "blob"} {
			res := getAttachment(t, mg, map[string]any{"message_id": "m1", "attachment_id": att, "output_file": outputFile})
			if text := resultText(t, res); !res.IsError || !strings.Contains(text, "not #microsoft.graph.fileAttachment") {
				t.Fatalf("%s to %q: want the not-a-file error, got:\n%s", att, outputFile, text)
			}
			if outputFile != "blob" {
				if _, err := os.Stat(outputFile); err == nil {
					t.Errorf("%s: a file was written for an attachment without bytes", att)
				}
			}
		}
		// Without output_file their JSON still comes back as is.
		res := getAttachment(t, mg, map[string]any{"message_id": "m1", "attachment_id": att})
		if res.IsError {
			t.Fatalf("%s without output_file: %s", att, resultText(t, res))
		}
	}
}

func TestGraphGetAttachmentDeclaresOutputFile(t *testing.T) {
	g := newFakeGraph(t)
	for _, td := range g.client().Tools() {
		if td.Tool.Name != "get_attachment" {
			continue
		}
		if _, ok := td.Tool.InputSchema.Properties["output_file"]; !ok {
			t.Fatal("get_attachment does not declare output_file")
		}
		for _, req := range td.Tool.InputSchema.Required {
			if req == "output_file" {
				t.Fatal("output_file must be optional")
			}
		}
		return
	}
	t.Fatal("get_attachment not among the tools")
}
