package tools

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/smnhffmnn/mux/internal/blob"
	"github.com/smnhffmnn/mux/internal/config"
)

// fakeGraph is enough of Microsoft Graph for the draft tools: drafts, replies,
// forwards, inline attachments and upload sessions. It records what it saw.
type fakeGraph struct {
	srv *httptest.Server

	mu       sync.Mutex
	requests []string // "METHOD path" in order
	drafts   int
	inline   []fakeInline
	sessions map[string]*fakeSession // by token
	attached map[string][]fakeAttachment
	cancels  int

	failInline    map[string]int // name → HTTP status to answer on POST /attachments
	noLocation    bool           // the completing PUT answers 201 without Location
	failRangeAt   int            // PUT index (1-based) that answers 500; 0 = never
	retryAfter    string         // Retry-After on a throttled answer
	throttleOnce  bool           // first inline POST answers 429
	sessionStatus int            // non-zero: createUploadSession answers this instead of 201
	uploadScheme  string         // non-empty: the uploadUrl uses this scheme instead of https
	badNextRange  bool           // a non-final PUT answers 200 with a wrong nextExpectedRanges
	completeEarly bool           // the first PUT answers 201 before the file is complete
	status200Last bool           // the final PUT answers 200 instead of 201
}

type fakeInline struct {
	Draft        string
	ODataType    string `json:"@odata.type"`
	Name         string `json:"name"`
	ContentType  string `json:"contentType"`
	ContentBytes string `json:"contentBytes"`
}

type fakeSession struct {
	draft string
	item  map[string]any
	size  int64
	data  bytes.Buffer
	puts  []fakePut
}

type fakePut struct {
	auth, contentType, contentRange string
	length                          int64
}

type fakeAttachment struct {
	ID, Name string
}

func newFakeGraph(t *testing.T) *fakeGraph {
	t.Helper()
	g := &fakeGraph{sessions: map[string]*fakeSession{}, attached: map[string][]fakeAttachment{}, failInline: map[string]int{}}
	g.srv = httptest.NewTLSServer(http.HandlerFunc(g.serve))
	t.Cleanup(g.srv.Close)
	return g
}

func (g *fakeGraph) client() *MicrosoftGraph {
	return &MicrosoftGraph{
		client:       g.srv.Client(),
		uploadClient: g.srv.Client(),
		baseURL:      g.srv.URL + "/v1.0",
		name:         "test",
		accessToken:  "test-token",
		tokenExpiry:  time.Now().Add(time.Hour),
	}
}

func (g *fakeGraph) record(r *http.Request) {
	g.mu.Lock()
	g.requests = append(g.requests, r.Method+" "+r.URL.Path)
	g.mu.Unlock()
}

func (g *fakeGraph) serve(w http.ResponseWriter, r *http.Request) {
	g.record(r)
	p := r.URL.Path
	switch {
	case strings.HasPrefix(p, "/upload/"):
		g.serveUpload(w, r, strings.TrimPrefix(p, "/upload/"))
		return
	case !strings.HasPrefix(p, "/v1.0/"):
		http.Error(w, "unknown path "+p, http.StatusNotFound)
		return
	}
	if r.Header.Get("Authorization") != "Bearer test-token" {
		http.Error(w, "no token", http.StatusUnauthorized)
		return
	}
	p = strings.TrimPrefix(p, "/v1.0")
	body, _ := io.ReadAll(r.Body)
	switch {
	case r.Method == http.MethodGet && p == "/me/mailFolders/inbox/messages":
		graphFakeJSON(w, 200, map[string]any{"value": []map[string]any{{"id": "orig1", "conversationId": "c1", "receivedDateTime": "2026-11-09T10:00:00Z"}}})
	case r.Method == http.MethodPost && p == "/me/messages":
		g.mu.Lock()
		g.drafts++
		id := fmt.Sprintf("draft%d", g.drafts)
		g.mu.Unlock()
		graphFakeJSON(w, 201, map[string]any{"id": id})
	case r.Method == http.MethodPost && p == "/me/messages/orig1/createReply":
		graphFakeJSON(w, 201, map[string]any{"id": "reply1"})
	case r.Method == http.MethodPost && p == "/me/messages/orig1/createForward":
		graphFakeJSON(w, 201, map[string]any{"id": "fwd1"})
	case r.Method == http.MethodPatch && strings.HasPrefix(p, "/me/messages/"):
		graphFakeJSON(w, 200, map[string]any{"id": strings.TrimPrefix(p, "/me/messages/")})
	case r.Method == http.MethodGet && strings.HasSuffix(p, "/attachments"):
		draft := strings.TrimSuffix(strings.TrimPrefix(p, "/me/messages/"), "/attachments")
		g.mu.Lock()
		list := make([]map[string]any, 0)
		for _, a := range g.attached[draft] {
			list = append(list, map[string]any{"id": a.ID, "name": a.Name})
		}
		g.mu.Unlock()
		graphFakeJSON(w, 200, map[string]any{"value": list})
	case r.Method == http.MethodPost && strings.HasSuffix(p, "/attachments/createUploadSession"):
		draft := strings.TrimSuffix(strings.TrimPrefix(p, "/me/messages/"), "/attachments/createUploadSession")
		var req struct {
			AttachmentItem map[string]any `json:"AttachmentItem"`
		}
		if err := json.Unmarshal(body, &req); err != nil || req.AttachmentItem == nil {
			http.Error(w, "bad session body", 400)
			return
		}
		g.mu.Lock()
		if g.sessionStatus != 0 {
			st := g.sessionStatus
			g.mu.Unlock()
			graphFakeJSON(w, st, map[string]any{"error": map[string]any{"code": "ErrorInternalServerError", "message": "no session for you"}})
			return
		}
		size, _ := req.AttachmentItem["size"].(float64)
		token := fmt.Sprintf("s%d", len(g.sessions)+1)
		g.sessions[token] = &fakeSession{draft: draft, item: req.AttachmentItem, size: int64(size)}
		scheme := "https"
		if g.uploadScheme != "" {
			scheme = g.uploadScheme
		}
		g.mu.Unlock()
		uploadURL := g.srv.URL + "/upload/" + token
		if scheme != "https" {
			// A deliberately non-https uploadUrl, to exercise the scheme check.
			uploadURL = scheme + "://example.invalid/upload/" + token
		}
		graphFakeJSON(w, 201, map[string]any{
			"uploadUrl":          uploadURL,
			"expirationDateTime": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
			"nextExpectedRanges": []string{"0-"},
		})
	case r.Method == http.MethodPost && strings.HasSuffix(p, "/attachments"):
		draft := strings.TrimSuffix(strings.TrimPrefix(p, "/me/messages/"), "/attachments")
		var in fakeInline
		if err := json.Unmarshal(body, &in); err != nil {
			http.Error(w, "bad attachment body", 400)
			return
		}
		in.Draft = draft
		g.mu.Lock()
		if g.throttleOnce {
			g.throttleOnce = false
			g.mu.Unlock()
			if g.retryAfter != "" {
				w.Header().Set("Retry-After", g.retryAfter)
			}
			graphFakeJSON(w, 429, map[string]any{"error": map[string]any{"code": "ApplicationThrottled", "message": "slow down"}})
			return
		}
		if status := g.failInline[in.Name]; status != 0 {
			g.mu.Unlock()
			graphFakeJSON(w, status, map[string]any{"error": map[string]any{"code": "ErrorInternalServerError", "message": "boom"}})
			return
		}
		g.inline = append(g.inline, in)
		id := fmt.Sprintf("att-inline-%d", len(g.inline))
		g.attached[draft] = append(g.attached[draft], fakeAttachment{ID: id, Name: in.Name})
		g.mu.Unlock()
		raw, _ := base64.StdEncoding.DecodeString(in.ContentBytes)
		graphFakeJSON(w, 201, map[string]any{"@odata.type": "#microsoft.graph.fileAttachment", "id": id, "name": in.Name, "contentType": in.ContentType, "size": len(raw) + 150})
	default:
		http.Error(w, "unexpected "+r.Method+" "+p, http.StatusNotFound)
	}
}

func (g *fakeGraph) serveUpload(w http.ResponseWriter, r *http.Request, token string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	s := g.sessions[token]
	if s == nil {
		http.Error(w, "no such session", 404)
		return
	}
	switch r.Method {
	case http.MethodDelete:
		g.cancels++
		w.WriteHeader(204)
	case http.MethodPut:
		data, _ := io.ReadAll(r.Body)
		s.puts = append(s.puts, fakePut{auth: r.Header.Get("Authorization"), contentType: r.Header.Get("Content-Type"), contentRange: r.Header.Get("Content-Range"), length: r.ContentLength})
		if g.failRangeAt > 0 && len(s.puts) == g.failRangeAt {
			graphFakeJSON(w, 500, map[string]any{"error": map[string]any{"code": "ErrorInternalServerError", "message": "range lost"}})
			return
		}
		var start, end, total int64
		if _, err := fmt.Sscanf(r.Header.Get("Content-Range"), "bytes %d-%d/%d", &start, &end, &total); err != nil {
			http.Error(w, "bad Content-Range", 400)
			return
		}
		if start != int64(s.data.Len()) || total != s.size || int64(len(data)) != end-start+1 {
			http.Error(w, fmt.Sprintf("range out of order: %s with %d bytes at %d", r.Header.Get("Content-Range"), len(data), s.data.Len()), 400)
			return
		}
		s.data.Write(data)
		if g.completeEarly && len(s.puts) == 1 && int64(s.data.Len()) < s.size {
			// A 201 before the file is complete — mux must reject it.
			w.Header().Set("Location", fmt.Sprintf("https://x/Messages('%s')/Attachments('early')", s.draft))
			w.WriteHeader(201)
			return
		}
		if int64(s.data.Len()) < s.size {
			next := fmt.Sprintf("%d-", s.data.Len())
			if g.badNextRange {
				next = "999999999-" // a byte offset that is not where mux is
			}
			graphFakeJSON(w, 200, map[string]any{"ExpirationDateTime": "2026-11-10T10:00:00Z", "nextExpectedRanges": []string{next}})
			return
		}
		id := "att-session-" + token
		if g.status200Last {
			// A 200 after the last range instead of the completing 201.
			graphFakeJSON(w, 200, map[string]any{"nextExpectedRanges": []string{}})
			return
		}
		name, _ := s.item["name"].(string)
		g.attached[s.draft] = append(g.attached[s.draft], fakeAttachment{ID: id, Name: name})
		if !g.noLocation {
			w.Header().Set("Location", fmt.Sprintf("https://outlook.office.com/api/v2.0/Users('u')/Messages('%s')/Attachments('%s')", s.draft, id))
		}
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(201)
	default:
		http.Error(w, "bad method", 405)
	}
}

func graphFakeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// putBlob stores bytes as a named blob and returns its reference.
func putBlob(t *testing.T, s *blob.Store, name, contentType string, data []byte) *blob.Info {
	t.Helper()
	info, err := s.Put(bytes.NewReader(data), blob.Meta{Name: name, ContentType: contentType, Source: "upload"})
	if err != nil {
		t.Fatal(err)
	}
	return info
}

func blobGone(s *blob.Store, id string) bool {
	_, err := s.Get(id)
	return err != nil
}

type graphDraftOut struct {
	DraftID          string                  `json:"draft_id"`
	Attachments      []graphAttachmentResult `json:"attachments"`
	AttachmentErrors []graphAttachmentError  `json:"attachment_errors"`
	Note             string                  `json:"note"`
}

func decodeGraphDraft(t *testing.T, text string) graphDraftOut {
	t.Helper()
	var out graphDraftOut
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("result is not JSON: %v\n%s", err, text)
	}
	return out
}

func TestGraphCreateDraftAttachmentsInlineAndSession(t *testing.T) {
	store := useTestBlobStore(t)
	g := newFakeGraph(t)
	mg := g.client()

	small := []byte("hello,world\n1,2\n")
	smallBlob := putBlob(t, store, "data.csv", "text/csv", small)
	large := bytes.Repeat([]byte("0123456789abcdef"), (8<<20)/16) // 8 MiB → three ranges
	// A declared type, so the assertion does not depend on the machine's mime
	// table: an undeclared (or octet-stream) blob is typed by extension, and a
	// runner without /etc/mime.types knows no .bin.
	largeBlob := putBlob(t, store, "video.bin", "video/mp4", large)
	dir := t.TempDir()
	pathFile := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(pathFile, []byte("from a path"), 0o600); err != nil {
		t.Fatal(err)
	}

	res, err := mg.handleCreateDraft(t.Context(), toolRequest(map[string]any{
		"to": "a@example.com", "subject": "s", "body": "b",
		"attachments": []any{
			blob.Ref(smallBlob.ID),
			map[string]any{"file": blob.Ref(largeBlob.ID), "name": "Recording 2026-11-09.bin"},
			map[string]any{"file": pathFile, "name": "renamed.txt"},
		},
	}))
	if err != nil {
		t.Fatal(err)
	}
	text := resultText(t, res)
	if res.IsError {
		t.Fatalf("unexpected error result:\n%s", text)
	}
	out := decodeGraphDraft(t, text)
	if out.DraftID != "draft1" || len(out.Attachments) != 3 || len(out.AttachmentErrors) != 0 {
		t.Fatalf("result: %+v", out)
	}
	byName := map[string]graphAttachmentResult{}
	for _, a := range out.Attachments {
		byName[a.Name] = a
	}
	if a := byName["data.csv"]; a.ID != "att-inline-1" || a.Size != int64(len(small)) || a.ContentType != "text/csv" || a.File != blob.Ref(smallBlob.ID) {
		t.Errorf("small: %+v", a)
	}
	if a := byName["Recording 2026-11-09.bin"]; a.ID != "att-session-s1" || a.Size != int64(len(large)) || a.ContentType != "video/mp4" || a.File != blob.Ref(largeBlob.ID) {
		t.Errorf("large: %+v", a)
	}
	if a := byName["renamed.txt"]; a.ID != "att-inline-2" || a.ContentType != "text/plain; charset=utf-8" || a.File != pathFile {
		t.Errorf("path: %+v", a)
	}

	// The inline POST carried the bytes base64-encoded as a fileAttachment.
	if len(g.inline) != 2 || g.inline[0].ODataType != "#microsoft.graph.fileAttachment" || g.inline[0].Draft != "draft1" {
		t.Fatalf("inline posts: %+v", g.inline)
	}
	if raw, _ := base64.StdEncoding.DecodeString(g.inline[0].ContentBytes); !bytes.Equal(raw, small) {
		t.Errorf("inline bytes differ")
	}
	// The session: AttachmentItem with type, name, size and content type;
	// three ranges in order, under 4 MB each, without Authorization, with
	// Content-Range and octet-stream; the bytes arrived whole.
	s := g.sessions["s1"]
	if s == nil {
		t.Fatal("no upload session was opened")
	}
	// Print the item only — the session also holds the 8 MiB buffer, and a
	// failure message of that size is dropped by CI's log.
	if s.item["attachmentType"] != "file" || s.item["name"] != "Recording 2026-11-09.bin" || s.item["size"] != float64(len(large)) || s.item["contentType"] != "video/mp4" {
		t.Fatalf("session item: %+v", s.item)
	}
	if len(s.puts) != 3 {
		t.Fatalf("ranges: %d, want 3: %+v", len(s.puts), s.puts)
	}
	wantRanges := []string{
		fmt.Sprintf("bytes 0-%d/%d", graphUploadRange-1, len(large)),
		fmt.Sprintf("bytes %d-%d/%d", graphUploadRange, 2*graphUploadRange-1, len(large)),
		fmt.Sprintf("bytes %d-%d/%d", 2*graphUploadRange, len(large)-1, len(large)),
	}
	for i, p := range s.puts {
		if p.auth != "" {
			t.Errorf("range %d sent Authorization %q — the upload URL is pre-authenticated", i, p.auth)
		}
		if p.contentType != "application/octet-stream" || p.contentRange != wantRanges[i] || p.length > 4<<20 || p.length <= 0 {
			t.Errorf("range %d: %+v, want %s", i, p, wantRanges[i])
		}
	}
	if sha256.Sum256(s.data.Bytes()) != sha256.Sum256(large) {
		t.Errorf("session bytes differ")
	}
	// Consumed blobs are gone; the draft came first, before any attachment.
	if !blobGone(store, smallBlob.ID) || !blobGone(store, largeBlob.ID) {
		t.Errorf("blobs must be deleted once attached")
	}
	if g.requests[0] != "POST /v1.0/me/messages" {
		t.Errorf("first request %q, want the draft", g.requests[0])
	}
	if g.cancels != 0 {
		t.Errorf("no session should have been cancelled")
	}
}

func TestGraphCreateDraftAttachmentsPartialFailureKeepsDraftAndBlob(t *testing.T) {
	store := useTestBlobStore(t)
	g := newFakeGraph(t)
	g.failInline["fail.bin"] = 500
	mg := g.client()

	ok := putBlob(t, store, "ok.txt", "text/plain", []byte("fine"))
	bad := putBlob(t, store, "fail.bin", "", []byte("doomed"))

	res, err := mg.handleCreateDraft(t.Context(), toolRequest(map[string]any{
		"to": "a@example.com", "subject": "s", "body": "b",
		"attachments": []any{blob.Ref(ok.ID), blob.Ref(bad.ID)},
	}))
	if err != nil {
		t.Fatal(err)
	}
	text := resultText(t, res)
	if !res.IsError {
		t.Fatalf("a failed attachment must make the result an error:\n%s", text)
	}
	out := decodeGraphDraft(t, text)
	if out.DraftID != "draft1" {
		t.Errorf("the draft id must still be in the result: %+v", out)
	}
	if len(out.Attachments) != 1 || out.Attachments[0].Name != "ok.txt" {
		t.Errorf("attached: %+v", out.Attachments)
	}
	if len(out.AttachmentErrors) != 1 || out.AttachmentErrors[0].Name != "fail.bin" || !strings.Contains(out.AttachmentErrors[0].Error, "HTTP 500") || !strings.Contains(out.AttachmentErrors[0].Error, "boom") || out.AttachmentErrors[0].File != blob.Ref(bad.ID) {
		t.Errorf("errors: %+v", out.AttachmentErrors)
	}
	if !strings.Contains(out.Note, "1 of 2") || !strings.Contains(out.Note, "Do not send") {
		t.Errorf("note: %q", out.Note)
	}
	if !blobGone(store, ok.ID) {
		t.Errorf("the attached file's blob must be deleted")
	}
	if blobGone(store, bad.ID) {
		t.Errorf("the failed file's blob must be kept for the retry")
	}
}

func TestGraphDraftAttachmentsValidatedBeforeTheDraft(t *testing.T) {
	store := useTestBlobStore(t)
	g := newFakeGraph(t)
	mg := g.client()
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty.txt")
	os.WriteFile(empty, nil, 0o600)
	other := filepath.Join(dir, "other.txt")
	os.WriteFile(other, []byte("other"), 0o600)
	nameless := putBlob(t, store, "", "", []byte("x"))
	named := putBlob(t, store, "a.txt", "", []byte("x"))
	tree, err := store.PutTree(testTarGz(t, map[string]string{"d/f": "x"}), blob.Meta{Name: "d"})
	if err != nil {
		t.Fatal(err)
	}
	huge := filepath.Join(dir, "huge.bin")
	fh, _ := os.Create(huge)
	fh.Truncate(graphAttachmentMax + 1) // sparse: the size is checked, the bytes never read
	fh.Close()

	cases := []struct {
		name string
		arg  any
		want string
	}{
		{"unknown blob", []any{"blob:00000000000000000000000000000000"}, "not found"},
		{"bad blob id", []any{"blob:nope"}, "not a blob reference"},
		{"relative path", []any{"relative/file.txt"}, "must be blob:<id> or an absolute path"},
		{"missing path", []any{filepath.Join(dir, "missing.txt")}, "not found"},
		{"empty file", []any{empty}, "is empty"},
		{"blob without a name", []any{blob.Ref(nameless.ID)}, "has no name"},
		{"a tree", []any{blob.Ref(tree.ID)}, "directory tree"},
		{"duplicate names", []any{blob.Ref(named.ID), map[string]any{"file": other, "name": "A.TXT"}}, "two files are named"},
		{"name with a slash", []any{map[string]any{"file": blob.Ref(named.ID), "name": "x/y.txt"}}, "must not contain"},
		{"unknown key", []any{map[string]any{"file": blob.Ref(named.ID), "size": 3}}, "unknown key"},
		{"object without file", []any{map[string]any{"name": "x"}}, "file is required"},
		{"wrong item type", []any{42}, "must be a string"},
		{"over the cap", []any{huge}, "more than the"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res, err := mg.handleCreateDraft(t.Context(), toolRequest(map[string]any{"to": "a@example.com", "subject": "s", "body": "b", "attachments": c.arg}))
			if err != nil {
				t.Fatal(err)
			}
			if !res.IsError || !strings.Contains(resultText(t, res), c.want) {
				t.Fatalf("want an error containing %q, got isError=%v:\n%s", c.want, res.IsError, resultText(t, res))
			}
		})
	}
	// Nothing reached Graph: a wrong list leaves no draft behind.
	if len(g.requests) != 0 {
		t.Errorf("requests before validation passed: %v", g.requests)
	}
	// Duplicate check is case-insensitive but the blob named a.txt stays, nothing consumed it.
	if blobGone(store, named.ID) {
		t.Errorf("validation must not consume blobs")
	}
}

func TestGraphDraftAttachmentsWithoutBlobStore(t *testing.T) {
	useTestBlobStore(t)
	SetBlobStore(nil)
	g := newFakeGraph(t)
	mg := g.client()
	res, _ := mg.handleCreateDraft(t.Context(), toolRequest(map[string]any{"to": "a@example.com", "subject": "s", "body": "b", "attachments": []any{"blob:0123456789abcdef0123456789abcdef"}}))
	if !res.IsError || !strings.Contains(resultText(t, res), "no blob store") {
		t.Fatalf("a blob reference without a store must say so:\n%s", resultText(t, res))
	}
	// A path still works where agent and mux share a disk.
	p := filepath.Join(t.TempDir(), "f.txt")
	os.WriteFile(p, []byte("x"), 0o600)
	res, _ = mg.handleCreateDraft(t.Context(), toolRequest(map[string]any{"to": "a@example.com", "subject": "s", "body": "b", "attachments": []any{p}}))
	if res.IsError {
		t.Fatalf("a path must work without a store:\n%s", resultText(t, res))
	}
}

func TestGraphReplyAndForwardDraftsTakeAttachments(t *testing.T) {
	store := useTestBlobStore(t)
	g := newFakeGraph(t)
	mg := g.client()

	b1 := putBlob(t, store, "r.txt", "text/plain", []byte("reply file"))
	res, err := mg.handleCreateReplyDraft(t.Context(), toolRequest(map[string]any{"conversation_id": "c1", "body": "thanks", "attachments": []string{blob.Ref(b1.ID)}}))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("reply: %s", resultText(t, res))
	}
	out := decodeGraphDraft(t, resultText(t, res))
	if out.DraftID != "reply1" || len(out.Attachments) != 1 || out.Attachments[0].Name != "r.txt" || g.inline[0].Draft != "reply1" {
		t.Errorf("reply: %+v inline=%+v", out, g.inline)
	}

	b2 := putBlob(t, store, "f.txt", "text/plain", []byte("forward file"))
	res, err = mg.handleCreateForwardDraft(t.Context(), toolRequest(map[string]any{"conversation_id": "c1", "to": "b@example.com", "attachments": blob.Ref(b2.ID)}))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("forward: %s", resultText(t, res))
	}
	out = decodeGraphDraft(t, resultText(t, res))
	if out.DraftID != "fwd1" || len(out.Attachments) != 1 || out.Attachments[0].Name != "f.txt" || g.inline[1].Draft != "fwd1" {
		t.Errorf("forward: %+v inline=%+v", out, g.inline)
	}
	// Recipients were set before the attachment went on.
	var patchAt, attachAt int
	for i, r := range g.requests {
		switch r {
		case "PATCH /v1.0/me/messages/fwd1":
			patchAt = i
		case "POST /v1.0/me/messages/fwd1/attachments":
			attachAt = i
		}
	}
	if patchAt == 0 || attachAt < patchAt {
		t.Errorf("order: %v", g.requests)
	}
	if !blobGone(store, b1.ID) || !blobGone(store, b2.ID) {
		t.Errorf("blobs must be consumed")
	}
}

func TestGraphForwardDraftRefusesBadRecipientsBeforeCreating(t *testing.T) {
	useTestBlobStore(t)
	g := newFakeGraph(t)
	mg := g.client()
	res, _ := mg.handleCreateForwardDraft(t.Context(), toolRequest(map[string]any{"conversation_id": "c1", "to": " , "}))
	if !res.IsError || !strings.Contains(resultText(t, res), "valid email address") {
		t.Fatalf("got %v %s", res.IsError, resultText(t, res))
	}
	if len(g.requests) != 0 {
		t.Errorf("no draft must be created for a call without recipients: %v", g.requests)
	}
}

func TestGraphUploadSessionWithoutLocationReadsTheIDBack(t *testing.T) {
	store := useTestBlobStore(t)
	g := newFakeGraph(t)
	g.noLocation = true
	mg := g.client()
	large := bytes.Repeat([]byte("x"), graphAttachmentInlineMax) // exactly 3 MiB → the session path
	b := putBlob(t, store, "big.bin", "application/octet-stream", large)
	res, err := mg.handleCreateDraft(t.Context(), toolRequest(map[string]any{"to": "a@example.com", "subject": "s", "body": "b", "attachments": []any{blob.Ref(b.ID)}}))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("%s", resultText(t, res))
	}
	out := decodeGraphDraft(t, resultText(t, res))
	if len(out.Attachments) != 1 || out.Attachments[0].ID != "att-session-s1" {
		t.Errorf("id must come from the attachment list: %+v", out.Attachments)
	}
	if len(g.sessions["s1"].puts) != 1 {
		t.Errorf("3 MiB is one range: %d", len(g.sessions["s1"].puts))
	}
	found := false
	for _, r := range g.requests {
		if r == "GET /v1.0/me/messages/draft1/attachments" {
			found = true
		}
	}
	if !found {
		t.Errorf("the list was not read: %v", g.requests)
	}
}

func TestGraphUploadSessionFailureCancelsAndKeepsBlob(t *testing.T) {
	store := useTestBlobStore(t)
	g := newFakeGraph(t)
	g.failRangeAt = 2
	mg := g.client()
	large := bytes.Repeat([]byte("y"), 5<<20)
	b := putBlob(t, store, "big.bin", "", large)
	res, err := mg.handleCreateDraft(t.Context(), toolRequest(map[string]any{"to": "a@example.com", "subject": "s", "body": "b", "attachments": []any{blob.Ref(b.ID)}}))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatalf("a failed range must fail the file:\n%s", resultText(t, res))
	}
	out := decodeGraphDraft(t, resultText(t, res))
	if out.DraftID != "draft1" || len(out.Attachments) != 0 || len(out.AttachmentErrors) != 1 {
		t.Fatalf("%+v", out)
	}
	e := out.AttachmentErrors[0]
	if e.Name != "big.bin" || !strings.Contains(e.Error, "HTTP 500") || !strings.Contains(e.Error, "range lost") || !strings.Contains(e.Error, fmt.Sprintf("at %d", graphUploadRange)) {
		t.Errorf("error: %+v", e)
	}
	if g.cancels != 1 {
		t.Errorf("the session must be cancelled once, got %d", g.cancels)
	}
	if blobGone(store, b.ID) {
		t.Errorf("the blob must be kept")
	}
}

func TestGraphInlineAttachmentRetriesOnceWhenThrottled(t *testing.T) {
	store := useTestBlobStore(t)
	g := newFakeGraph(t)
	g.throttleOnce = true
	g.retryAfter = "1"
	mg := g.client()
	b := putBlob(t, store, "t.txt", "text/plain", []byte("throttled"))
	start := time.Now()
	res, err := mg.handleCreateDraft(t.Context(), toolRequest(map[string]any{"to": "a@example.com", "subject": "s", "body": "b", "attachments": []any{blob.Ref(b.ID)}}))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("one 429 must be retried:\n%s", resultText(t, res))
	}
	if d := time.Since(start); d < time.Second {
		t.Errorf("the retry must wait for Retry-After, waited %s", d)
	}
	posts := 0
	for _, r := range g.requests {
		if r == "POST /v1.0/me/messages/draft1/attachments" {
			posts++
		}
	}
	if posts != 2 {
		t.Errorf("attachment POSTs: %d, want 2", posts)
	}
}

func TestGraphAttachmentEntries(t *testing.T) {
	cases := []struct {
		name string
		raw  any
		want []graphAttachmentEntry
		err  string
	}{
		{"strings", []any{"blob:a", " /x/y "}, []graphAttachmentEntry{{file: "blob:a"}, {file: "/x/y"}}, ""},
		{"string slice", []string{"blob:a"}, []graphAttachmentEntry{{file: "blob:a"}}, ""},
		{"objects", []any{map[string]any{"file": "blob:a", "name": "n.txt"}}, []graphAttachmentEntry{{file: "blob:a", name: "n.txt"}}, ""},
		{"one string", "blob:a", []graphAttachmentEntry{{file: "blob:a"}}, ""},
		{"one object", map[string]any{"file": "/p"}, []graphAttachmentEntry{{file: "/p"}}, ""},
		{"json text", `["blob:a", {"file": "/p", "name": "q"}]`, []graphAttachmentEntry{{file: "blob:a"}, {file: "/p", name: "q"}}, ""},
		{"empty string", "  ", nil, ""},
		{"blank entries dropped", []any{"", "blob:a"}, []graphAttachmentEntry{{file: "blob:a"}}, ""},
		{"bad json", "[oops", nil, "not a JSON list"},
		{"number", 3, nil, "must be a list"},
		{"name not a string", []any{map[string]any{"file": "/p", "name": 1}}, nil, "name must be a string"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := graphAttachmentEntries(c.raw)
			if c.err != "" {
				if err == nil || !strings.Contains(err.Error(), c.err) {
					t.Fatalf("want error %q, got %v", c.err, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if fmt.Sprint(got) != fmt.Sprint(c.want) {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

func TestGraphErrorText(t *testing.T) {
	if got := graphErrorText([]byte(`{"error":{"code":"ErrorAttachmentSizeShouldNotBeLessThanMinimumSize","message":"too small"}}`)); got != "ErrorAttachmentSizeShouldNotBeLessThanMinimumSize: too small" {
		t.Errorf("%q", got)
	}
	if got := graphErrorText([]byte(" plain text ")); got != "plain text" {
		t.Errorf("%q", got)
	}
	if got := graphErrorText(bytes.Repeat([]byte("a"), 600)); len(got) != 500+len("…") {
		t.Errorf("long body must be cut: %d", len(got))
	}
}

func TestGraphDraftToolsDeclareAttachments(t *testing.T) {
	mg := &MicrosoftGraph{}
	seen := 0
	for _, td := range mg.Tools() {
		switch td.Tool.Name {
		case "create_draft", "create_reply_draft", "create_forward_draft":
			seen++
			prop, ok := td.Tool.InputSchema.Properties["attachments"].(map[string]any)
			if !ok || prop["type"] != "array" {
				t.Errorf("%s: attachments must be an array property: %v", td.Tool.Name, td.Tool.InputSchema.Properties["attachments"])
				continue
			}
			items, _ := prop["items"].(map[string]any)
			if _, ok := items["anyOf"]; !ok {
				t.Errorf("%s: items must allow strings and objects: %v", td.Tool.Name, items)
			}
			for _, r := range td.Tool.InputSchema.Required {
				if r == "attachments" {
					t.Errorf("%s: attachments must be optional", td.Tool.Name)
				}
			}
		}
	}
	if seen != 3 {
		t.Errorf("found %d draft tools, want 3", seen)
	}
}

// testTarGz builds a small tar.gz for a tree blob.
func testTarGz(t *testing.T, files map[string]string) io.Reader {
	t.Helper()
	var buf bytes.Buffer
	writeTestTarGz(t, &buf, files)
	return &buf
}

// TestNewMicrosoftGraphWiresClients guards the constructor fields the draft
// attachments rely on: without them every request would go to ""+path and a
// session upload would dereference a nil client.
func TestNewMicrosoftGraphWiresClients(t *testing.T) {
	mg, err := NewMicrosoftGraph(config.Connection{Name: "t", ClientID: "app-id"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if mg.baseURL != graphBaseURL {
		t.Errorf("baseURL = %q, want %q", mg.baseURL, graphBaseURL)
	}
	if mg.uploadClient == nil {
		t.Fatal("uploadClient is nil")
	}
	if mg.uploadClient.Timeout != 0 {
		t.Errorf("uploadClient.Timeout = %s, want 0 (a range on a slow line needs no overall deadline)", mg.uploadClient.Timeout)
	}
	if mg.uploadClient == mg.client {
		t.Error("uploadClient must be a separate client from the default one")
	}
	if mg.uploadClient.CheckRedirect == nil {
		t.Error("uploadClient must refuse redirects so a range body is not replayed elsewhere")
	}
}

// TestGraphReplyForwardNoDraftWhenAttachmentInvalid proves the "a wrong list
// leaves no draft behind" promise for the reply and forward tools, not only
// create_draft: a bad entry must fail before createReply/createForward runs.
func TestGraphReplyForwardNoDraftWhenAttachmentInvalid(t *testing.T) {
	useTestBlobStore(t)
	for _, tc := range []struct {
		name string
		call func(mg *MicrosoftGraph, g *fakeGraph) (*mcp.CallToolResult, error)
	}{
		{"reply", func(mg *MicrosoftGraph, g *fakeGraph) (*mcp.CallToolResult, error) {
			return mg.handleCreateReplyDraft(t.Context(), toolRequest(map[string]any{"conversation_id": "c1", "body": "b", "attachments": []any{"blob:00000000000000000000000000000000"}}))
		}},
		{"forward", func(mg *MicrosoftGraph, g *fakeGraph) (*mcp.CallToolResult, error) {
			return mg.handleCreateForwardDraft(t.Context(), toolRequest(map[string]any{"conversation_id": "c1", "to": "b@example.com", "attachments": []any{"blob:00000000000000000000000000000000"}}))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newFakeGraph(t)
			res, err := tc.call(g.client(), g)
			if err != nil {
				t.Fatal(err)
			}
			if !res.IsError || !strings.Contains(resultText(t, res), "not found") {
				t.Fatalf("want a blob-not-found error, got isError=%v:\n%s", res.IsError, resultText(t, res))
			}
			for _, r := range g.requests {
				if strings.Contains(r, "createReply") || strings.Contains(r, "createForward") || r == "POST /v1.0/me/messages" {
					t.Errorf("no draft must be created on a bad attachment list, saw %q (all: %v)", r, g.requests)
				}
			}
		})
	}
}

// TestGraphUploadSessionErrorBranches exercises the upload-session failure
// paths the happy-path fake never reached: a refused session, a non-https
// uploadUrl, a nextExpectedRanges that disagrees with mux, a 201 before the
// end, and a 200 after the last range. Each must fail the file (error result,
// draft kept, blob kept) and, where a session was opened, cancel it.
func TestGraphUploadSessionErrorBranches(t *testing.T) {
	large := bytes.Repeat([]byte("z"), 5<<20) // 5 MiB → session path, two ranges
	cases := []struct {
		name        string
		set         func(*fakeGraph)
		wantErr     string
		wantCancels int
	}{
		{"session refused", func(g *fakeGraph) { g.sessionStatus = 500 }, "upload session for", 0},
		{"non-https uploadUrl", func(g *fakeGraph) { g.uploadScheme = "http" }, "not an https URL", 0},
		{"next range mismatch", func(g *fakeGraph) { g.badNextRange = true }, "expects the next range", 1},
		{"201 before the end", func(g *fakeGraph) { g.completeEarly = true }, "completed the attachment after", 1},
		{"200 after the last range", func(g *fakeGraph) { g.status200Last = true }, "after the last range", 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			store := useTestBlobStore(t)
			g := newFakeGraph(t)
			c.set(g)
			mg := g.client()
			b := putBlob(t, store, "big.bin", "application/octet-stream", large)
			res, err := mg.handleCreateDraft(t.Context(), toolRequest(map[string]any{"to": "a@example.com", "subject": "s", "body": "b", "attachments": []any{blob.Ref(b.ID)}}))
			if err != nil {
				t.Fatal(err)
			}
			if !res.IsError {
				t.Fatalf("want an error result:\n%s", resultText(t, res))
			}
			out := decodeGraphDraft(t, resultText(t, res))
			if out.DraftID != "draft1" || len(out.Attachments) != 0 || len(out.AttachmentErrors) != 1 {
				t.Fatalf("result: %+v", out)
			}
			if !strings.Contains(out.AttachmentErrors[0].Error, c.wantErr) {
				t.Errorf("error %q, want it to contain %q", out.AttachmentErrors[0].Error, c.wantErr)
			}
			if g.cancels != c.wantCancels {
				t.Errorf("cancels = %d, want %d", g.cancels, c.wantCancels)
			}
			if blobGone(store, b.ID) {
				t.Error("a failed file's blob must be kept")
			}
		})
	}
}

// TestGraphInlineNotRetriedOnServerError makes sure a plain 5xx on the
// non-idempotent attachment POST is not retried — one copy, not two.
func TestGraphInlineNotRetriedOnServerError(t *testing.T) {
	store := useTestBlobStore(t)
	g := newFakeGraph(t)
	g.failInline["x.txt"] = 502
	mg := g.client()
	b := putBlob(t, store, "x.txt", "text/plain", []byte("hi"))
	res, err := mg.handleCreateDraft(t.Context(), toolRequest(map[string]any{"to": "a@example.com", "subject": "s", "body": "b", "attachments": []any{blob.Ref(b.ID)}}))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatalf("want an error:\n%s", resultText(t, res))
	}
	posts := 0
	for _, r := range g.requests {
		if r == "POST /v1.0/me/messages/draft1/attachments" {
			posts++
		}
	}
	if posts != 1 {
		t.Errorf("a 502 on a non-idempotent POST must not be retried: %d POSTs, want 1", posts)
	}
	if blobGone(store, b.ID) {
		t.Error("the failed file's blob must be kept")
	}
}

// TestGraphRetryDelay pins the retry policy: a throttle is retried, an
// ambiguous 5xx is not.
func TestGraphRetryDelay(t *testing.T) {
	h := func(kv ...string) http.Header {
		hd := http.Header{}
		for i := 0; i+1 < len(kv); i += 2 {
			hd.Set(kv[i], kv[i+1])
		}
		return hd
	}
	cases := []struct {
		name      string
		status    int
		header    http.Header
		wantRetry bool
		wantDelay time.Duration
	}{
		{"429 no header", 429, h(), true, 2 * time.Second},
		{"429 with retry-after", 429, h("Retry-After", "5"), true, 5 * time.Second},
		{"429 retry-after capped", 429, h("Retry-After", "99"), true, 10 * time.Second},
		{"503 with retry-after", 503, h("Retry-After", "3"), true, 3 * time.Second},
		{"503 without retry-after", 503, h(), false, 0},
		{"502 not retried", 502, h("Retry-After", "1"), false, 0},
		{"504 not retried", 504, h(), false, 0},
		{"201 not retried", 201, h(), false, 0},
		{"429 non-numeric retry-after", 429, h("Retry-After", "Wed, 21 Oct 2026 07:28:00 GMT"), true, 2 * time.Second},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			delay, retry := graphRetryDelay(c.status, c.header)
			if retry != c.wantRetry || (retry && delay != c.wantDelay) {
				t.Errorf("graphRetryDelay(%d) = (%s, %v), want (%s, %v)", c.status, delay, retry, c.wantDelay, c.wantRetry)
			}
		})
	}
}
