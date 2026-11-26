package tools

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/smnhffmnn/mux/internal/blob"
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
		"@odata.type": "#microsoft.graph.fileAttachment",
		"id":          att,
		"name":        name,
		"contentType": contentType,
		"size":        len(content) + 180, // Graph's size counts more than the content
	}
	g.receivedContent[msg+"/"+att] = content
}

// valueFetches counts the requests for raw attachment content.
func (g *fakeGraph) valueFetches() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := 0
	for _, r := range g.requests {
		if strings.HasPrefix(r, "GET /v1.0/me/messages/") && strings.HasSuffix(r, "/$value") {
			n++
		}
	}
	return n
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

// The content comes from /$value as a stream, so the size of an attachment
// is no longer bounded by a JSON answer held in memory: 20 MiB to a path,
// where the limit used to be about 6 MB.
func TestGraphGetAttachmentLargeFileToPath(t *testing.T) {
	useTestBlobStore(t)
	SetBlobStore(nil)
	g := newFakeGraph(t)
	mg := g.client()
	content := attachmentBytes(20 << 20)
	g.receive("m1", "big", "Board deck.pptx", pptxType, content)

	dest := filepath.Join(t.TempDir(), "Board deck.pptx")
	res := getAttachment(t, mg, map[string]any{"message_id": "m1", "attachment_id": "big", "output_file": dest})
	text := resultText(t, res)
	if res.IsError {
		t.Fatalf("save failed:\n%s", text)
	}
	got, err := os.ReadFile(dest)
	if err != nil || !bytes.Equal(got, content) {
		t.Fatalf("file differs: %d bytes on disk, %d sent, err %v", len(got), len(content), err)
	}
	if out := decodeAttachmentResult(t, text); out["bytesWritten"] != float64(len(content)) {
		t.Errorf("bytesWritten = %v, want %d", out["bytesWritten"], len(content))
	}
	// The description is asked for without the content, the content once.
	g.mu.Lock()
	queries := append([]string(nil), g.metaQueries...)
	g.mu.Unlock()
	if len(queries) != 1 || !strings.Contains(queries[0], "select=id,name,size,contentType") || strings.Contains(queries[0], "contentBytes") {
		t.Errorf("description requests = %q, want one with $select and without contentBytes", queries)
	}
	if n := g.valueFetches(); n != 1 {
		t.Errorf("content fetched %d times, want once", n)
	}
}

func TestGraphGetAttachmentLargeFileToBlob(t *testing.T) {
	store := useTestBlobStore(t)
	g := newFakeGraph(t)
	mg := g.client()
	content := attachmentBytes(10 << 20)
	g.receive("m1", "big", "export.zip", "application/zip", content)
	// An image too large to look at inline is a blob like any other file.
	photo := append([]byte("\x89PNG\r\n\x1a\n"), attachmentBytes(6<<20)...)
	g.receive("m1", "photo", "scan.png", "image/png", photo)

	res := getAttachment(t, mg, map[string]any{"message_id": "m1", "attachment_id": "big"})
	out := decodeAttachmentResult(t, resultText(t, res))
	id, _ := out["blob"].(string)
	if res.IsError || id == "" || !bytes.Equal(readBlob(t, store, id), content) {
		t.Fatalf("a large attachment must arrive whole in a blob: error=%v blob=%q", res.IsError, id)
	}

	res = getAttachment(t, mg, map[string]any{"message_id": "m1", "attachment_id": "photo"})
	if res.IsError || len(res.Content) != 1 {
		t.Fatalf("a 6 MiB image is text plus blob, no image content: error=%v, %d parts", res.IsError, len(res.Content))
	}
	out = decodeAttachmentResult(t, resultText(t, res))
	if id, _ := out["blob"].(string); id == "" || !bytes.Equal(readBlob(t, store, id), photo) {
		t.Fatalf("the image blob does not hold the bytes")
	}
}

// Inline base64 has a limit of its own: the bytes go through the response.
// Too large is refused and names the way out — before the download when
// Graph's size is far over the limit, on the bytes read otherwise.
func TestGraphGetAttachmentInlineLimit(t *testing.T) {
	useTestBlobStore(t)
	SetBlobStore(nil)
	g := newFakeGraph(t)
	mg := g.client()
	g.receive("m1", "big", "Board deck.pptx", pptxType, attachmentBytes(13<<20))
	atLimit := attachmentBytes(graphInlineAttachmentMax)
	g.receive("m1", "fits", "fits.bin", "application/octet-stream", atLimit)
	// The content is one byte over the limit and Graph's size not far over:
	// the figure alone does not refuse it, the bytes read do.
	g.receive("m1", "edge", "edge.bin", "application/octet-stream", attachmentBytes(graphInlineAttachmentMax+1))
	// Graph's size overstates the content: an attachment whose bytes fit is
	// not refused because its reported size does not.
	g.receive("m1", "overstated", "export.csv", "text/csv", attachmentBytes(5<<20))
	g.mu.Lock()
	g.received["m1/overstated"]["size"] = 7 << 20
	g.mu.Unlock()

	res := getAttachment(t, mg, map[string]any{"message_id": "m1", "attachment_id": "big"})
	if text := resultText(t, res); !res.IsError || !strings.Contains(text, "output_file") || !strings.Contains(text, "too large to return inline") {
		t.Fatalf("want the inline-limit error naming output_file, got:\n%.300s", text)
	}
	if n := g.valueFetches(); n != 0 {
		t.Errorf("a refusal on Graph's size must not download the content (%d fetches)", n)
	}

	res = getAttachment(t, mg, map[string]any{"message_id": "m1", "attachment_id": "fits"})
	if res.IsError {
		t.Fatalf("an attachment at the limit must come back inline:\n%.300s", resultText(t, res))
	}
	if out := decodeAttachmentResult(t, resultText(t, res)); out["contentBytes"] != base64.StdEncoding.EncodeToString(atLimit) {
		t.Errorf("the inline bytes differ")
	}

	before := g.valueFetches()
	res = getAttachment(t, mg, map[string]any{"message_id": "m1", "attachment_id": "edge"})
	if text := resultText(t, res); !res.IsError || !strings.Contains(text, "too large to return inline") {
		t.Fatalf("one byte over the limit must be refused, got:\n%.300s", text)
	}
	if n := g.valueFetches() - before; n != 1 {
		t.Errorf("close to the limit the bytes decide, so the content is fetched once (%d fetches)", n)
	}

	res = getAttachment(t, mg, map[string]any{"message_id": "m1", "attachment_id": "overstated"})
	if res.IsError {
		t.Fatalf("content under the limit must come back inline whatever size Graph reports:\n%.300s", resultText(t, res))
	}
}

// A download that fails leaves nothing behind: no half file that would
// block the retry, no blob holding half an attachment.
func TestGraphGetAttachmentFailedDownloadLeavesNothing(t *testing.T) {
	store := useTestBlobStore(t)
	g := newFakeGraph(t)
	mg := g.client()
	g.receive("m1", "a1", "deck.pptx", pptxType, attachmentBytes(2<<20))
	dir := t.TempDir()

	t.Run("stream breaks off", func(t *testing.T) {
		g.mu.Lock()
		g.valueBreaksOff = true
		g.mu.Unlock()
		defer func() { g.mu.Lock(); g.valueBreaksOff = false; g.mu.Unlock() }()

		dest := filepath.Join(dir, "broken.pptx")
		res := getAttachment(t, mg, map[string]any{"message_id": "m1", "attachment_id": "a1", "output_file": dest})
		// The error names the line, not the disk the bytes were going to,
		// and says that the call can be repeated.
		if text := resultText(t, res); !res.IsError || !strings.Contains(text, "the transfer broke off after") || !strings.Contains(text, "nothing was saved") {
			t.Fatalf("half an attachment must be an error that says what happened:\n%s", text)
		}
		if _, err := os.Stat(dest); err == nil {
			t.Errorf("the half-written file was left behind")
		}
		res = getAttachment(t, mg, map[string]any{"message_id": "m1", "attachment_id": "a1"})
		if !res.IsError {
			t.Fatalf("half an attachment must not become a blob:\n%s", resultText(t, res))
		}
		if n := len(store.List()); n != 0 {
			t.Errorf("store holds %d blobs after a failed download", n)
		}
	})

	t.Run("Graph refuses the content", func(t *testing.T) {
		g.mu.Lock()
		g.valueStatus = http.StatusServiceUnavailable
		g.mu.Unlock()
		defer func() { g.mu.Lock(); g.valueStatus = 0; g.mu.Unlock() }()

		dest := filepath.Join(dir, "refused.pptx")
		res := getAttachment(t, mg, map[string]any{"message_id": "m1", "attachment_id": "a1", "output_file": dest})
		if text := resultText(t, res); !res.IsError || !strings.Contains(text, "HTTP 503") {
			t.Fatalf("want the Graph status in the error, got:\n%s", text)
		}
		if _, err := os.Stat(dest); err == nil {
			t.Errorf("a file was written although Graph sent no content")
		}
	})

	// After both failures the same path works: nothing blocked it.
	dest := filepath.Join(dir, "broken.pptx")
	if res := getAttachment(t, mg, map[string]any{"message_id": "m1", "attachment_id": "a1", "output_file": dest}); res.IsError {
		t.Fatalf("the retry failed:\n%s", resultText(t, res))
	}
}

// A redirect on the content request has not been seen from Graph for mail
// attachments, but the client follows one, so the bytes have to arrive
// through it.
func TestGraphGetAttachmentFollowsRedirect(t *testing.T) {
	useTestBlobStore(t)
	g := newFakeGraph(t)
	mg := g.client()
	content := attachmentBytes(4096)
	g.receive("m1", "a1", "invoice.pdf", "application/pdf", content)
	g.mu.Lock()
	g.valueRedirect = true
	g.mu.Unlock()

	dest := filepath.Join(t.TempDir(), "invoice.pdf")
	res := getAttachment(t, mg, map[string]any{"message_id": "m1", "attachment_id": "a1", "output_file": dest})
	if res.IsError {
		t.Fatalf("save through a redirect failed:\n%s", resultText(t, res))
	}
	if got, _ := os.ReadFile(dest); !bytes.Equal(got, content) {
		t.Fatalf("file content differs after the redirect")
	}
}

// A description without @odata.type is taken for a file; one that turns out
// to have no raw content fails with Graph's answer instead of an empty file.
func TestGraphGetAttachmentWithoutTypeAnnotation(t *testing.T) {
	useTestBlobStore(t)
	g := newFakeGraph(t)
	mg := g.client()
	content := attachmentBytes(1000)
	g.receive("m1", "a1", "notes.txt", "text/plain", content)
	g.mu.Lock()
	delete(g.received["m1/a1"], "@odata.type")
	g.received["m1/link"] = map[string]any{"id": "link", "name": "Plan.docx", "size": 0}
	g.mu.Unlock()

	dest := filepath.Join(t.TempDir(), "notes.txt")
	res := getAttachment(t, mg, map[string]any{"message_id": "m1", "attachment_id": "a1", "output_file": dest})
	if res.IsError {
		t.Fatalf("an untyped file attachment must be saved:\n%s", resultText(t, res))
	}
	if got, _ := os.ReadFile(dest); !bytes.Equal(got, content) {
		t.Fatalf("file content differs")
	}

	noFile := filepath.Join(t.TempDir(), "Plan.docx")
	res = getAttachment(t, mg, map[string]any{"message_id": "m1", "attachment_id": "link", "output_file": noFile})
	if text := resultText(t, res); !res.IsError || !strings.Contains(text, "HTTP 405") {
		t.Fatalf("want Graph's refusal for an attachment without raw content, got:\n%s", text)
	}
	if _, err := os.Stat(noFile); err == nil {
		t.Errorf("an empty file was written for an attachment without content")
	}
}

// A redirect to another host gets neither the token nor the Graph URL: Go
// drops Authorization when the host changes, the redirect policy removes
// the Referer. The other host here is the same listener under the second
// name its test certificate carries.
func TestGraphGetAttachmentRedirectToAnotherHost(t *testing.T) {
	useTestBlobStore(t)
	g := newFakeGraph(t)
	mg := g.client()
	content := attachmentBytes(4096)
	g.receive("m1", "a1", "invoice.pdf", "application/pdf", content)
	g.mu.Lock()
	g.valueRedirect, g.redirectHost = true, "example.com"
	g.mu.Unlock()

	tr := g.srv.Client().Transport.(*http.Transport).Clone()
	addr := g.srv.Listener.Addr().String()
	tr.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}
	mg.downloadClient = &http.Client{Transport: tr, CheckRedirect: graphDownloadRedirect}

	dest := filepath.Join(t.TempDir(), "invoice.pdf")
	res := getAttachment(t, mg, map[string]any{"message_id": "m1", "attachment_id": "a1", "output_file": dest})
	if res.IsError {
		t.Fatalf("save through a redirect failed:\n%s", resultText(t, res))
	}
	if got, _ := os.ReadFile(dest); !bytes.Equal(got, content) {
		t.Fatalf("file content differs after the redirect")
	}
	g.mu.Lock()
	h := g.downloadHeaders
	g.mu.Unlock()
	if h == nil {
		t.Fatal("the download URL was never requested")
	}
	if v := h.Get("Authorization"); v != "" {
		t.Errorf("the token went to the other host: %q", v)
	}
	if v := h.Get("Referer"); v != "" {
		t.Errorf("the Graph URL went to the other host as Referer: %q", v)
	}
}

// A download that goes silent is given up, with nothing left behind — it
// does not hold the call for as long as the peer keeps the connection.
func TestGraphGetAttachmentStalledDownload(t *testing.T) {
	useTestBlobStore(t)
	prev := graphDownloadIdle
	graphDownloadIdle = 200 * time.Millisecond
	t.Cleanup(func() { graphDownloadIdle = prev })
	g := newFakeGraph(t)
	mg := g.client()
	g.receive("m1", "a1", "deck.pptx", pptxType, attachmentBytes(1<<20))
	g.mu.Lock()
	g.valueStalls = true
	g.mu.Unlock()

	dest := filepath.Join(t.TempDir(), "deck.pptx")
	start := time.Now()
	res := getAttachment(t, mg, map[string]any{"message_id": "m1", "attachment_id": "a1", "output_file": dest})
	if text := resultText(t, res); !res.IsError || !strings.Contains(text, "no data from Graph for") || !strings.Contains(text, "nothing was saved") {
		t.Fatalf("want the stall error, got:\n%s", text)
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Errorf("giving up took %s", took)
	}
	if _, err := os.Stat(dest); err == nil {
		t.Errorf("the half-written file was left behind")
	}
}

// The blob store has a cap per blob. Far over it, the attachment is refused
// before the download; over it by less, the stream stops at the cap. Both
// name the path as the way out and leave no blob.
func TestGraphGetAttachmentBlobCap(t *testing.T) {
	useTestBlobStore(t)
	store, err := blob.Open(filepath.Join(t.TempDir(), "small"), blob.Options{BaseURL: "http://127.0.0.1:7700", MaxBlob: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	SetBlobStore(store)
	g := newFakeGraph(t)
	mg := g.client()
	g.receive("m1", "huge", "video.mp4", "video/mp4", attachmentBytes(3<<20))
	g.receive("m1", "over", "export.zip", "application/zip", attachmentBytes(1<<20+4096))

	res := getAttachment(t, mg, map[string]any{"message_id": "m1", "attachment_id": "huge"})
	if text := resultText(t, res); !res.IsError || !strings.Contains(text, "too large for a blob") || !strings.Contains(text, "output_file") {
		t.Fatalf("want the refusal naming output_file, got:\n%s", text)
	}
	if n := g.valueFetches(); n != 0 {
		t.Errorf("far over the cap nothing is downloaded (%d fetches)", n)
	}

	res = getAttachment(t, mg, map[string]any{"message_id": "m1", "attachment_id": "over"})
	if text := resultText(t, res); !res.IsError || !strings.Contains(text, "blob too large") || !strings.Contains(text, "output_file") {
		t.Fatalf("want the cap error naming output_file, got:\n%s", text)
	}
	if n := len(store.List()); n != 0 {
		t.Errorf("store holds %d blobs after a refused attachment", n)
	}
}

// An empty image is a blob and nothing to look at: no image part with no
// bytes in it.
func TestGraphGetAttachmentEmptyImage(t *testing.T) {
	useTestBlobStore(t)
	g := newFakeGraph(t)
	mg := g.client()
	g.receive("m1", "empty", "pixel.png", "image/png", nil)

	for _, args := range []map[string]any{
		{"message_id": "m1", "attachment_id": "empty"},
		{"message_id": "m1", "attachment_id": "empty", "output_file": "blob"},
	} {
		res := getAttachment(t, mg, args)
		if res.IsError || len(res.Content) != 1 {
			t.Fatalf("an empty image must come back as text only: error=%v, %d parts\n%s", res.IsError, len(res.Content), resultText(t, res))
		}
	}
}

// An untyped description whose content request Graph refuses (405, as for a
// reference attachment) returns its JSON when no file was asked for — what a
// typed one does.
func TestGraphGetAttachmentUntypedWithoutContent(t *testing.T) {
	useTestBlobStore(t)
	g := newFakeGraph(t)
	mg := g.client()
	g.mu.Lock()
	g.received["m1/link"] = map[string]any{"id": "link", "name": "Plan.docx", "size": 0}
	g.mu.Unlock()

	res := getAttachment(t, mg, map[string]any{"message_id": "m1", "attachment_id": "link"})
	if res.IsError {
		t.Fatalf("want the JSON of the attachment, got an error:\n%s", resultText(t, res))
	}
	if out := decodeAttachmentResult(t, resultText(t, res)); out["name"] != "Plan.docx" || out["id"] != "link" {
		t.Errorf("result does not describe the attachment: %v", out)
	}
}

func TestGraphDownloadFailure(t *testing.T) {
	// A local failure stays what it is.
	if got := graphDownloadFailure(t.Context(), &graphDownloadBody{}, errors.New("write file: disk full")); got != "write file: disk full" {
		t.Errorf("a local error must pass through, got %q", got)
	}
	// A deadline of the caller is not reported as a stall.
	ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancel()
	if got := graphDownloadFailure(ctx, nil, errors.New("download attachment: context deadline exceeded")); strings.Contains(got, "no data from Graph") {
		t.Errorf("the caller's deadline must not read as a stall, got %q", got)
	}
}
