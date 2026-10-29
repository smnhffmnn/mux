package tools

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/smnhffmnn/mux/internal/config"
)

// A fake Nylas: enough of the REST API v3 for the handlers to exercise their
// shaping, the ball position, folder resolution, drafts and sending. Answers
// come from the fixtures under testdata/nylas, which follow the shapes a
// production grant returned while the type was built (envelope, thread dates,
// folder attributes, address objects, error bodies). Requests are recorded so
// tests can assert on what went out.

type nylasFake struct {
	mu       sync.Mutex
	requests []nylasFakeRequest
	srv      *httptest.Server

	provider      string // provider reported for grant-1 (default imap)
	bigAttachment bool   // report att-1 as larger than the inline limit
	dropSend      bool   // close the connection on send without answering
}

type nylasFakeRequest struct {
	Method string
	Path   string
	Query  url.Values
	Body   string
}

func nylasFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "nylas", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func newNylasFake(t *testing.T) *nylasFake {
	t.Helper()
	f := &nylasFake{provider: "imap"}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve(t)))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *nylasFake) record(r *http.Request) nylasFakeRequest {
	body, _ := io.ReadAll(r.Body)
	req := nylasFakeRequest{Method: r.Method, Path: r.URL.Path, Query: r.URL.Query(), Body: string(body)}
	f.mu.Lock()
	f.requests = append(f.requests, req)
	f.mu.Unlock()
	return req
}

func (f *nylasFake) recorded(method, pathSuffix string) []nylasFakeRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []nylasFakeRequest
	for _, r := range f.requests {
		if (method == "" || r.Method == method) && strings.HasSuffix(r.Path, pathSuffix) {
			out = append(out, r)
		}
	}
	return out
}

// messages returns the fixture messages of both threads.
func (f *nylasFake) messages(t *testing.T) []nylasMessage {
	var all []nylasMessage
	for _, name := range []string{"messages_t1.json", "messages_t2.json"} {
		var env struct {
			Data []nylasMessage `json:"data"`
		}
		if err := json.Unmarshal(nylasFixture(t, name), &env); err != nil {
			t.Fatal(err)
		}
		all = append(all, env.Data...)
	}
	return all
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	switch b := v.(type) {
	case []byte:
		w.Write(b)
	default:
		json.NewEncoder(w).Encode(v)
	}
}

func (f *nylasFake) serve(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		req := f.record(r)
		if r.Header.Get("Authorization") != "Bearer test-key" {
			writeJSON(w, http.StatusUnauthorized, nylasFixture(t, "error_unauthorized.json"))
			return
		}
		p := strings.TrimPrefix(r.URL.Path, "/v3/grants")
		switch {
		case p == "" && r.Method == http.MethodGet:
			writeJSON(w, 200, nylasFixture(t, "grants.json"))
		case p == "/grant-1" && r.Method == http.MethodGet:
			body := strings.Replace(string(nylasFixture(t, "grant.json")), `"provider":"imap"`, `"provider":"`+f.provider+`"`, 1)
			writeJSON(w, 200, []byte(body))
		case p == "/grant-bad" && r.Method == http.MethodGet:
			writeJSON(w, 200, map[string]any{"request_id": "x", "data": map[string]any{"id": "grant-bad", "grant_status": "invalid", "provider": "imap", "email": "jane@example.com"}})
		case p == "/grant-1/folders" && r.Method == http.MethodGet:
			writeJSON(w, 200, nylasFixture(t, "folders.json"))
		case p == "/grant-1/folders" && r.Method == http.MethodPost:
			writeJSON(w, 200, nylasFixture(t, "folder_created.json"))
		case p == "/grant-1/threads" && r.Method == http.MethodGet:
			writeJSON(w, 200, nylasFixture(t, "threads.json"))
		case p == "/grant-1/messages" && r.Method == http.MethodGet:
			var msgs []nylasMessage
			switch tid := req.Query.Get("thread_id"); {
			case tid != "":
				for _, m := range f.messages(t) {
					if m.ThreadID == tid {
						msgs = append(msgs, m)
					}
				}
			case req.Query.Get("search_query_native") != "":
				msgs = f.messages(t)
			}
			if in := req.Query.Get("in"); in != "" {
				var filtered []nylasMessage
				for _, m := range msgs {
					for _, fo := range m.Folders {
						if fo == in {
							filtered = append(filtered, m)
							break
						}
					}
				}
				msgs = filtered
			}
			if msgs == nil {
				msgs = []nylasMessage{}
			}
			writeJSON(w, 200, map[string]any{"request_id": "x", "data": msgs})
		case strings.HasPrefix(p, "/grant-1/messages/send") && r.Method == http.MethodPost:
			if f.dropSend {
				if hj, ok := w.(http.Hijacker); ok {
					c, _, _ := hj.Hijack()
					c.Close()
					return
				}
			}
			writeJSON(w, 200, nylasFixture(t, "message_sent.json"))
		case strings.HasPrefix(p, "/grant-1/messages/"):
			id, _ := url.PathUnescape(strings.TrimPrefix(p, "/grant-1/messages/"))
			var found *nylasMessage
			for _, m := range f.messages(t) {
				if m.ID == id {
					mm := m
					found = &mm
				}
			}
			if found == nil {
				writeJSON(w, 404, nylasFixture(t, "error_not_found.json"))
				return
			}
			switch r.Method {
			case http.MethodGet, http.MethodPut:
				writeJSON(w, 200, map[string]any{"request_id": "x", "data": found})
			case http.MethodDelete:
				writeJSON(w, 200, map[string]any{"request_id": "x"})
			}
		case strings.HasPrefix(p, "/grant-1/attachments/"):
			rest := strings.TrimPrefix(p, "/grant-1/attachments/")
			if req.Query.Get("message_id") == "" {
				writeJSON(w, 400, map[string]any{"request_id": "x", "error": map[string]any{"type": "api.invalid_request_error", "message": "message_id is required"}})
				return
			}
			if strings.HasSuffix(rest, "/download") {
				w.Header().Set("Content-Type", "application/octet-stream")
				w.WriteHeader(200)
				w.Write([]byte("hello"))
				return
			}
			meta := map[string]any{"id": rest, "grant_id": "grant-1", "filename": "report.pdf", "content_type": "application/pdf", "size": 5, "is_inline": false}
			if f.bigAttachment {
				meta["size"] = nylasInlineAttachment + 1
			}
			writeJSON(w, 200, map[string]any{"request_id": "x", "data": meta})
		case p == "/grant-1/drafts" && r.Method == http.MethodPost:
			writeJSON(w, 200, nylasFixture(t, "draft_created.json"))
		case p == "/grant-1/drafts/draft-404" && r.Method == http.MethodPost:
			writeJSON(w, 404, nylasFixture(t, "error_not_found.json"))
		case strings.HasPrefix(p, "/grant-1/drafts/") && r.Method == http.MethodPost:
			if f.dropSend {
				if hj, ok := w.(http.Hijacker); ok {
					c, _, _ := hj.Hijack()
					c.Close()
					return
				}
			}
			writeJSON(w, 200, nylasFixture(t, "message_sent.json"))
		case strings.HasPrefix(p, "/grant-1/drafts/") && r.Method == http.MethodDelete:
			writeJSON(w, 200, map[string]any{"request_id": "x"})
		default:
			t.Errorf("fake Nylas: unexpected %s %s", r.Method, r.URL.String())
			writeJSON(w, 404, nylasFixture(t, "error_not_found.json"))
		}
	}
}

// newTestNylas builds a connection against the fake. grantID "" with a user
// exercises the lookup by address.
func newTestNylas(t *testing.T, f *nylasFake, grantID, user string, allowSend bool) *Nylas {
	t.Helper()
	ny, err := NewNylas(config.Connection{
		Name: "mail", Type: config.TypeNylas, Token: "test-key", Region: "eu",
		GrantID: grantID, User: user, FromName: "Jane Doe", AllowSend: allowSend,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ny.baseURL = f.srv.URL
	return ny
}

// lastBody decodes the body of the last recorded request into a fresh map —
// json.Unmarshal into a reused map would keep the keys of the previous call.
func lastBody(t *testing.T, reqs []nylasFakeRequest) map[string]any {
	t.Helper()
	if len(reqs) == 0 {
		t.Fatal("no request recorded")
	}
	body := map[string]any{}
	if err := json.Unmarshal([]byte(reqs[len(reqs)-1].Body), &body); err != nil {
		t.Fatalf("request body is not JSON: %v\n%s", err, reqs[len(reqs)-1].Body)
	}
	return body
}

func decodeResult(t *testing.T, text string, out any) {
	t.Helper()
	if err := json.Unmarshal([]byte(text), out); err != nil {
		t.Fatalf("result is not JSON: %v\n%s", err, text)
	}
}

type nylasListPage struct {
	Folder        string              `json:"folder"`
	Conversations []nylasConversation `json:"conversations"`
	NextPageToken string              `json:"nextPageToken"`
}

func TestNylas_ListConversations(t *testing.T) {
	f := newNylasFake(t)
	ny := newTestNylas(t, f, "grant-1", "", false)
	ctx := context.Background()

	res, err := ny.handleListConversations(ctx, toolRequest(map[string]any{"limit": float64(100), "unread": true}))
	if err != nil || res.IsError {
		t.Fatalf("list_conversations: %v / %s", err, resultText(t, res))
	}
	var page nylasListPage
	decodeResult(t, resultText(t, res), &page)

	reqs := f.recorded(http.MethodGet, "/threads")
	if len(reqs) != 1 {
		t.Fatalf("threads requests = %d", len(reqs))
	}
	q := reqs[0].Query
	if q.Get("in") != "v0:grant-1:INBOX" || q.Get("limit") != "50" || q.Get("unread") != "true" {
		t.Errorf("threads query = %v", q)
	}
	if page.Folder != "INBOX" || page.NextPageToken != "cursor-2" || len(page.Conversations) != 4 {
		t.Fatalf("page = folder %q, token %q, %d conversations", page.Folder, page.NextPageToken, len(page.Conversations))
	}
	byID := map[string]nylasConversation{}
	for _, c := range page.Conversations {
		byID[c.ConversationID] = c
	}
	// t1: the other side answered after the account's own message — ball with me.
	c := byID["t1"]
	if c.Ball != "me" || c.LastFromMe || c.LatestMessage.ID != "m-recv-1" || c.LatestMessage.FromMe || !c.HasAttachments || c.MessageCount != 2 {
		t.Errorf("t1 = %+v", c)
	}
	if strings.Join(c.Participants, ",") != "bob@example.net,jane@example.com" || strings.Join(c.Folders, ",") != "INBOX,INBOX.Sent" {
		t.Errorf("t1 participants %v folders %v", c.Participants, c.Folders)
	}
	if c.LatestReceived != "2026-10-28T10:08:37Z" || c.LatestSent != "2026-10-28T10:07:06Z" {
		t.Errorf("t1 dates: received %s sent %s", c.LatestReceived, c.LatestSent)
	}
	if len(c.LatestMessage.ReplyTo) != 0 {
		t.Errorf("t1 latest replyTo = %v, want omitted (same as sender)", c.LatestMessage.ReplyTo)
	}
	// t2: the account wrote last — ball with them.
	c = byID["t2"]
	if c.Ball != "them" || !c.LastFromMe || !c.LatestMessage.FromMe || !c.IsFlagged {
		t.Errorf("t2 = %+v", c)
	}
	// t3: never answered — ball with me, unread.
	c = byID["t3"]
	if c.Ball != "me" || c.LastFromMe || c.IsRead || c.LatestSent != "" {
		t.Errorf("t3 = %+v", c)
	}
	// t4: sent and received in the same second — the sender of the latest
	// message decides, compared case-insensitively against the own address.
	c = byID["t4"]
	if c.Ball != "them" || !c.LastFromMe {
		t.Errorf("t4 = %+v", c)
	}

	// Another folder, by name without the IMAP prefix.
	res, _ = ny.handleListConversations(ctx, toolRequest(map[string]any{"folder": "Later"}))
	if res.IsError {
		t.Fatalf("folder Later: %s", resultText(t, res))
	}
	reqs = f.recorded(http.MethodGet, "/threads")
	if got := reqs[len(reqs)-1].Query.Get("in"); got != "v0:grant-1:INBOX.Later" {
		t.Errorf("folder Later resolved to %q", got)
	}
	res, _ = ny.handleListConversations(ctx, toolRequest(map[string]any{"folder": "Nope"}))
	if !res.IsError || !strings.Contains(resultText(t, res), `folder "Nope" not found`) {
		t.Errorf("unknown folder: %s", resultText(t, res))
	}
	// Folders were listed once and cached across the calls above.
	if n := len(f.recorded(http.MethodGet, "/folders")); n != 1 {
		t.Errorf("folders listed %d times, want 1 (cache)", n)
	}
}

func TestNylas_GetConversation(t *testing.T) {
	f := newNylasFake(t)
	ny := newTestNylas(t, f, "grant-1", "", false)

	res, err := ny.handleGetConversation(context.Background(), toolRequest(map[string]any{"conversation_id": "t1"}))
	if err != nil || res.IsError {
		t.Fatalf("get_conversation: %v / %s", err, resultText(t, res))
	}
	var msgs []nylasMessageFull
	decodeResult(t, resultText(t, res), &msgs)
	if len(msgs) != 2 || msgs[0].ID != "m-sent-1" || msgs[1].ID != "m-recv-1" {
		t.Fatalf("messages = %+v", msgs)
	}
	sent, recv := msgs[0], msgs[1]
	if !sent.FromMe || recv.FromMe || sent.Body != "Plain text body\nwith two lines" {
		t.Errorf("sent = %+v", sent)
	}
	if recv.Body != "Reply from the other side.\nSecond line & entity\n\nRegards" {
		t.Errorf("html body → %q", recv.Body)
	}
	if strings.Join(recv.ReplyTo, ",") != "bob-reply@example.net" || strings.Join(recv.CC, ",") != "carol@example.net" || recv.SenderName != "Bob Roe" {
		t.Errorf("recv = %+v", recv)
	}
	if len(recv.Attachments) != 2 || recv.Attachments[0].ID != "att-1" || recv.Attachments[0].ContentType != "application/pdf" || !recv.Attachments[1].IsInline {
		t.Errorf("attachments = %+v", recv.Attachments)
	}
	if strings.Join(recv.Folders, ",") != "INBOX" || strings.Join(sent.Folders, ",") != "INBOX.Sent" {
		t.Errorf("folders: recv %v sent %v", recv.Folders, sent.Folders)
	}
	if reqs := f.recorded(http.MethodGet, "/messages"); len(reqs) != 1 || reqs[0].Query.Get("thread_id") != "t1" || reqs[0].Query.Get("limit") != "200" {
		t.Errorf("messages requests = %+v", reqs)
	}
}

func TestNylas_SearchMessages(t *testing.T) {
	f := newNylasFake(t)
	ny := newTestNylas(t, f, "grant-1", "", false)
	ctx := context.Background()

	res, err := ny.handleSearchMessages(ctx, toolRequest(map[string]any{"query": `foo "bar"`}))
	if err != nil || res.IsError {
		t.Fatalf("search: %v / %s", err, resultText(t, res))
	}
	reqs := f.recorded(http.MethodGet, "/messages")
	if q := reqs[len(reqs)-1].Query; q.Get("search_query_native") != `TEXT "foo \"bar\""` || q.Get("in") != "" || q.Get("query_imap") != "" {
		t.Errorf("imap search query = %v", q)
	}
	var convs []nylasConversation
	decodeResult(t, resultText(t, res), &convs)
	if len(convs) != 2 || convs[0].ConversationID != "t1" || convs[1].ConversationID != "t2" {
		t.Fatalf("grouped = %+v", convs)
	}
	if convs[0].Ball != "me" || convs[1].Ball != "them" || convs[0].MessageCount != 2 || !convs[0].HasAttachments {
		t.Errorf("grouped shapes: %+v", convs)
	}

	// native passes the query through; live asks the IMAP server in a folder.
	ny.handleSearchMessages(ctx, toolRequest(map[string]any{"query": "FROM bob", "native": true, "live": true}))
	reqs = f.recorded(http.MethodGet, "/messages")
	if q := reqs[len(reqs)-1].Query; q.Get("search_query_native") != "FROM bob" || q.Get("query_imap") != "true" || q.Get("in") != "v0:grant-1:INBOX" {
		t.Errorf("live native query = %v", q)
	}

	// A Gmail grant takes the query as is and has no live mode.
	f2 := newNylasFake(t)
	f2.provider = "google"
	ny2 := newTestNylas(t, f2, "grant-1", "", false)
	ny2.handleSearchMessages(ctx, toolRequest(map[string]any{"query": "from:bob invoice"}))
	reqs = f2.recorded(http.MethodGet, "/messages")
	if q := reqs[len(reqs)-1].Query; q.Get("search_query_native") != "from:bob invoice" {
		t.Errorf("google query = %v", q)
	}
	res, _ = ny2.handleSearchMessages(ctx, toolRequest(map[string]any{"query": "x", "live": true}))
	if !res.IsError || !strings.Contains(resultText(t, res), "IMAP-backed") {
		t.Errorf("live on google: %s", resultText(t, res))
	}
}

func TestNylas_ArchiveDeleteMove(t *testing.T) {
	f := newNylasFake(t)
	ny := newTestNylas(t, f, "grant-1", "", false)
	ctx := context.Background()

	res, _ := ny.handleArchiveConversation(ctx, toolRequest(map[string]any{"conversation_id": "t1"}))
	if res.IsError {
		t.Fatalf("archive: %s", resultText(t, res))
	}
	puts := f.recorded(http.MethodPut, "")
	if len(puts) != 1 || !strings.HasSuffix(puts[0].Path, "/messages/m-recv-1") || !strings.Contains(puts[0].Body, `"folders":["v0:grant-1:INBOX.Archive"]`) {
		t.Errorf("archive PUTs = %+v", puts)
	}
	if text := resultText(t, res); !strings.Contains(text, `"archived": 1`) || !strings.Contains(text, `"folder": "INBOX.Archive"`) {
		t.Errorf("archive result: %s", text)
	}
	// Only inbox messages are read: the Sent copy stays.
	if reqs := f.recorded(http.MethodGet, "/messages"); reqs[len(reqs)-1].Query.Get("in") != "v0:grant-1:INBOX" {
		t.Errorf("archive read messages without in=inbox: %v", reqs[len(reqs)-1].Query)
	}

	res, _ = ny.handleDeleteConversation(ctx, toolRequest(map[string]any{"conversation_id": "t1"}))
	if res.IsError {
		t.Fatalf("delete: %s", resultText(t, res))
	}
	dels := f.recorded(http.MethodDelete, "")
	if len(dels) != 1 || !strings.HasSuffix(dels[0].Path, "/messages/m-recv-1") || dels[0].Query.Get("hard_delete") != "" {
		t.Errorf("delete requests = %+v", dels)
	}
	if text := resultText(t, res); !strings.Contains(text, `"deleted": 1`) || !strings.Contains(text, `"folder": "INBOX.Trash"`) {
		t.Errorf("delete result: %s", text)
	}

	res, _ = ny.handleMoveConversation(ctx, toolRequest(map[string]any{"conversation_id": "t2", "folder": "Later"}))
	if res.IsError {
		t.Fatalf("move: %s", resultText(t, res))
	}
	puts = f.recorded(http.MethodPut, "")
	if len(puts) != 2 || !strings.HasSuffix(puts[1].Path, "/messages/m-t2-a") || !strings.Contains(puts[1].Body, `"v0:grant-1:INBOX.Later"`) {
		t.Errorf("move PUTs = %+v", puts)
	}
	if text := resultText(t, res); !strings.Contains(text, `"moved": 1`) || !strings.Contains(text, `"from": "INBOX"`) || !strings.Contains(text, `"to": "INBOX.Later"`) {
		t.Errorf("move result: %s", text)
	}

	res, _ = ny.handleMoveConversation(ctx, toolRequest(map[string]any{"conversation_id": "t2", "folder": "Keep"}))
	if !res.IsError || !strings.Contains(resultText(t, res), "create=true") {
		t.Errorf("move to missing folder: %s", resultText(t, res))
	}
	res, _ = ny.handleMoveConversation(ctx, toolRequest(map[string]any{"conversation_id": "t2", "folder": "Keep", "create": true, "source_folder": "INBOX.Sent"}))
	if res.IsError {
		t.Fatalf("move with create: %s", resultText(t, res))
	}
	posts := f.recorded(http.MethodPost, "/folders")
	if len(posts) != 1 || !strings.Contains(posts[0].Body, `"name":"Keep"`) {
		t.Errorf("folder creation = %+v", posts)
	}
	puts = f.recorded(http.MethodPut, "")
	if last := puts[len(puts)-1]; !strings.HasSuffix(last.Path, "/messages/m-t2-b") || !strings.Contains(last.Body, `"v0:grant-1:INBOX.Keep"`) {
		t.Errorf("move after create = %+v", last)
	}
	if text := resultText(t, res); !strings.Contains(text, `"from": "INBOX.Sent"`) || !strings.Contains(text, `"to": "INBOX.Keep"`) {
		t.Errorf("move with create result: %s", text)
	}
}

func TestNylas_Drafts(t *testing.T) {
	f := newNylasFake(t)
	ny := newTestNylas(t, f, "grant-1", "", false)
	ctx := context.Background()

	res, _ := ny.handleCreateReplyDraft(ctx, toolRequest(map[string]any{"conversation_id": "t1", "body": "Thanks"}))
	if res.IsError {
		t.Fatalf("reply draft: %s", resultText(t, res))
	}
	posts := f.recorded(http.MethodPost, "/drafts")
	if len(posts) != 1 {
		t.Fatalf("draft POSTs = %d", len(posts))
	}
	body := lastBody(t, posts)
	if fmt.Sprint(body["to"]) != "[map[email:bob-reply@example.net name:Bob (reply)]]" || body["cc"] != nil || body["subject"] != "Re: Fwd: Token expires" ||
		body["reply_to_message_id"] != "m-recv-1" || body["is_plaintext"] != true || body["body"] != "Thanks" || fmt.Sprint(body["from"]) != "[map[email:jane@example.com name:Jane Doe]]" {
		t.Errorf("reply draft body = %v", body)
	}
	if text := resultText(t, res); !strings.Contains(text, `"draft_id": "draft-9"`) || !strings.Contains(text, `"folder": "INBOX.Drafts"`) || !strings.Contains(text, `"in_reply_to": "m-recv-1"`) {
		t.Errorf("reply draft result: %s", text)
	}

	// reply_all adds the others on To and Cc, never the account itself.
	ny.handleCreateReplyDraft(ctx, toolRequest(map[string]any{"conversation_id": "t1", "body": "x", "reply_all": true}))
	body = lastBody(t, f.recorded(http.MethodPost, "/drafts"))
	if fmt.Sprint(body["cc"]) != "[map[email:carol@example.net name:Carol]]" {
		t.Errorf("reply_all cc = %v", body["cc"])
	}

	// A reply to the account's own message answers its recipients.
	ny.handleCreateReplyDraft(ctx, toolRequest(map[string]any{"conversation_id": "t1", "body": "x", "message_id": "m-sent-1"}))
	body = lastBody(t, f.recorded(http.MethodPost, "/drafts"))
	if fmt.Sprint(body["to"]) != "[map[email:bob@example.net]]" || body["reply_to_message_id"] != "m-sent-1" || body["subject"] != "Re: Fwd: Token expires" {
		t.Errorf("reply to own message = %v", body)
	}

	res, _ = ny.handleCreateReplyDraft(ctx, toolRequest(map[string]any{"conversation_id": "t1", "body": "x", "message_id": "m-nope"}))
	if !res.IsError || !strings.Contains(resultText(t, res), "not part of conversation") {
		t.Errorf("unknown message_id: %s", resultText(t, res))
	}

	res, _ = ny.handleCreateForwardDraft(ctx, toolRequest(map[string]any{"conversation_id": "t1", "to": "Carol <carol@example.net>, dave@example.org", "body": "FYI"}))
	if res.IsError {
		t.Fatalf("forward draft: %s", resultText(t, res))
	}
	body = lastBody(t, f.recorded(http.MethodPost, "/drafts"))
	if fmt.Sprint(body["to"]) != "[map[email:carol@example.net name:Carol] map[email:dave@example.org]]" || body["subject"] != "Fwd: Re: Fwd: Token expires" || body["reply_to_message_id"] != nil {
		t.Errorf("forward body = %v", body)
	}
	text, _ := body["body"].(string)
	for _, want := range []string{"FYI\n\n---------- Forwarded message ----------\n", "From: Bob Roe <bob@example.net>\n", "Subject: Re: Fwd: Token expires\n", "To: jane@example.com\n", "Reply from the other side.\nSecond line & entity"} {
		if !strings.Contains(text, want) {
			t.Errorf("forward text lacks %q:\n%s", want, text)
		}
	}
	atts, _ := body["attachments"].([]any)
	if len(atts) != 1 || fmt.Sprint(atts[0].(map[string]any)["id"]) != "att-1" {
		t.Errorf("forward attachments = %v (inline logo must be skipped)", body["attachments"])
	}
	if !strings.Contains(resultText(t, res), `"attachments": 1`) {
		t.Errorf("forward result: %s", resultText(t, res))
	}
	ny.handleCreateForwardDraft(ctx, toolRequest(map[string]any{"conversation_id": "t1", "to": "dave@example.org", "include_attachments": false}))
	body = lastBody(t, f.recorded(http.MethodPost, "/drafts"))
	if body["attachments"] != nil {
		t.Errorf("include_attachments=false still sends %v", body["attachments"])
	}
}

func TestNylas_SendTools(t *testing.T) {
	f := newNylasFake(t)
	ctx := context.Background()

	names := func(ny *Nylas) []string {
		var out []string
		for _, td := range ny.Tools() {
			out = append(out, td.Tool.Name)
		}
		return out
	}
	readOnly := newTestNylas(t, f, "grant-1", "", false)
	if got := strings.Join(names(readOnly), " "); strings.Contains(got, "send_draft") || strings.Contains(got, "reply_send") || strings.Contains(got, "forward_send") {
		t.Errorf("send tools registered without allow_send: %s", got)
	}
	ny := newTestNylas(t, f, "grant-1", "", true)
	if got := strings.Join(names(ny), " "); !strings.Contains(got, "send_draft") || !strings.Contains(got, "reply_send") || !strings.Contains(got, "forward_send") {
		t.Errorf("send tools missing with allow_send: %s", got)
	}
	if got := strings.Join(nylasSendToolNames("mail"), " "); got != "mail_send_draft mail_reply_send mail_forward_send" {
		t.Errorf("nylasSendToolNames = %q", got)
	}

	res, _ := ny.handleSendDraft(ctx, toolRequest(map[string]any{"draft_id": "draft-9"}))
	if res.IsError {
		t.Fatalf("send_draft: %s", resultText(t, res))
	}
	if posts := f.recorded(http.MethodPost, "/drafts/draft-9"); len(posts) != 1 {
		t.Errorf("send POSTs = %+v", posts)
	}
	if text := resultText(t, res); !strings.Contains(text, `"sent": true`) || !strings.Contains(text, `"message_id": "m-sent-9"`) || !strings.Contains(text, `"bob-reply@example.net"`) {
		t.Errorf("send_draft result: %s", text)
	}

	res, _ = ny.handleSendDraft(ctx, toolRequest(map[string]any{"draft_id": "draft-404"}))
	if !res.IsError || !strings.Contains(resultText(t, res), "nothing was sent now") {
		t.Errorf("send missing draft: %s", resultText(t, res))
	}

	res, _ = ny.handleReplySend(ctx, toolRequest(map[string]any{"conversation_id": "t1", "body": "x"}))
	if !res.IsError || !strings.Contains(resultText(t, res), "message_id is required") {
		t.Errorf("reply_send without message_id: %s", resultText(t, res))
	}
	res, _ = ny.handleReplySend(ctx, toolRequest(map[string]any{"conversation_id": "t1", "body": "Thanks", "message_id": "m-recv-1"}))
	if res.IsError {
		t.Fatalf("reply_send: %s", resultText(t, res))
	}
	sends := f.recorded(http.MethodPost, "/messages/send")
	if len(sends) != 1 || !strings.Contains(sends[0].Body, `"reply_to_message_id":"m-recv-1"`) || !strings.Contains(sends[0].Body, `"bob-reply@example.net"`) {
		t.Errorf("reply_send body = %+v", sends)
	}
	if text := resultText(t, res); !strings.Contains(text, `"sent": true`) || !strings.Contains(text, `"message_id": "m-sent-9"`) {
		t.Errorf("reply_send result: %s", text)
	}

	res, _ = ny.handleForwardSend(ctx, toolRequest(map[string]any{"conversation_id": "t1", "message_id": "m-recv-1", "to": "dave@example.org"}))
	if res.IsError {
		t.Fatalf("forward_send: %s", resultText(t, res))
	}
	sends = f.recorded(http.MethodPost, "/messages/send")
	if last := sends[len(sends)-1]; !strings.Contains(last.Body, `"attachments":[{"id":"att-1"`) || !strings.Contains(last.Body, `"subject":"Fwd: Re: Fwd: Token expires"`) {
		t.Errorf("forward_send body = %s", last.Body)
	}

	// A send whose request dies without an answer is reported as unclear,
	// never as a plain error an agent would retry.
	f.dropSend = true
	res, _ = ny.handleSendDraft(ctx, toolRequest(map[string]any{"draft_id": "draft-9"}))
	if !res.IsError || !strings.Contains(resultText(t, res), "unclear whether sent") {
		t.Errorf("dropped send: %s", resultText(t, res))
	}
	res, _ = ny.handleReplySend(ctx, toolRequest(map[string]any{"conversation_id": "t1", "body": "x", "message_id": "m-recv-1"}))
	if !res.IsError || !strings.Contains(resultText(t, res), "unclear whether sent") {
		t.Errorf("dropped reply_send: %s", resultText(t, res))
	}
}

func TestNylas_GrantResolution(t *testing.T) {
	f := newNylasFake(t)
	ny := newTestNylas(t, f, "", "Jane@Example.com", false)
	ctx := context.Background()

	info, err := ny.Verify(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if info.GrantID != "grant-1" || info.Email != "jane@example.com" || info.Provider != "imap" || info.Folders != 6 {
		t.Errorf("verify = %+v", info)
	}
	lookups := f.recorded(http.MethodGet, "/v3/grants")
	if len(lookups) != 1 || lookups[0].Query.Get("email") != "jane@example.com" {
		t.Errorf("grant lookups = %+v", lookups)
	}
	// Resolved once: the second call reads nothing new.
	if _, err := ny.Verify(ctx); err != nil {
		t.Fatal(err)
	}
	if n := len(f.recorded(http.MethodGet, "/v3/grants")); n != 1 {
		t.Errorf("grant looked up %d times", n)
	}

	bad := newTestNylas(t, f, "grant-bad", "", false)
	if _, err := bad.Verify(ctx); err == nil || !strings.Contains(err.Error(), "is invalid") {
		t.Errorf("invalid grant: %v", err)
	}

	// The address from the grant is the account's own, so a reply to the
	// account's message is not addressed to itself.
	byID := newTestNylas(t, f, "grant-1", "", false)
	res, _ := byID.handleCreateReplyDraft(ctx, toolRequest(map[string]any{"conversation_id": "t1", "body": "x", "message_id": "m-sent-1"}))
	if res.IsError || !strings.Contains(resultText(t, res), `"reply_to": "bob@example.net"`) {
		t.Errorf("reply with resolved address: %s", resultText(t, res))
	}
}

func TestNylas_Errors(t *testing.T) {
	f := newNylasFake(t)
	ny := newTestNylas(t, f, "grant-1", "", false)
	ny.token = "wrong"
	res, _ := ny.handleListMailboxes(context.Background(), toolRequest(nil))
	text := resultText(t, res)
	if !res.IsError || !strings.Contains(text, "HTTP 401") || !strings.Contains(text, "api.unauthorized") || !strings.Contains(text, "Invalid or expired API key") || !strings.Contains(text, "req-401") {
		t.Errorf("401 = %s", text)
	}

	// The registry is the only way out.
	if _, err := ny.call(context.Background(), "nope", nil, nil, nil); err == nil || !strings.Contains(err.Error(), "not registered") {
		t.Errorf("unregistered endpoint: %v", err)
	}
	q := url.Values{}
	q.Set("bogus", "1")
	if _, err := ny.call(context.Background(), "folders", map[string]string{"grant_id": "grant-1"}, q, nil); err == nil || !strings.Contains(err.Error(), "does not take query parameter") {
		t.Errorf("unregistered parameter: %v", err)
	}
	if _, err := ny.call(context.Background(), "message", map[string]string{"grant_id": "grant-1"}, nil, nil); err == nil || !strings.Contains(err.Error(), "unfilled path parameter") {
		t.Errorf("unfilled path parameter: %v", err)
	}
}

func TestNylas_Attachments(t *testing.T) {
	f := newNylasFake(t)
	ny := newTestNylas(t, f, "grant-1", "", false)
	ctx := context.Background()

	res, _ := ny.handleListAttachments(ctx, toolRequest(map[string]any{"message_id": "m-recv-1"}))
	if res.IsError {
		t.Fatalf("list_attachments: %s", resultText(t, res))
	}
	var infos []nylasAttachmentInfo
	decodeResult(t, resultText(t, res), &infos)
	if len(infos) != 2 || infos[0].Filename != "report.pdf" || infos[1].ID != "att-inline" {
		t.Errorf("attachments = %+v", infos)
	}

	res, _ = ny.handleGetAttachment(ctx, toolRequest(map[string]any{"message_id": "m-recv-1", "attachment_id": "att-1"}))
	if res.IsError {
		t.Fatalf("get_attachment: %s", resultText(t, res))
	}
	var inline struct {
		Filename     string `json:"filename"`
		ContentType  string `json:"contentType"`
		Size         int    `json:"size"`
		ContentBytes string `json:"contentBytes"`
	}
	decodeResult(t, resultText(t, res), &inline)
	if inline.Filename != "report.pdf" || inline.ContentType != "application/pdf" || inline.Size != 5 || inline.ContentBytes != base64.StdEncoding.EncodeToString([]byte("hello")) {
		t.Errorf("inline attachment = %+v", inline)
	}
	for _, r := range f.recorded(http.MethodGet, "") {
		if strings.Contains(r.Path, "/attachments/") && r.Query.Get("message_id") != "m-recv-1" {
			t.Errorf("attachment request without message_id: %+v", r)
		}
	}

	out := filepath.Join(t.TempDir(), "report.pdf")
	res, _ = ny.handleGetAttachment(ctx, toolRequest(map[string]any{"message_id": "m-recv-1", "attachment_id": "att-1", "output_file": out}))
	if res.IsError {
		t.Fatalf("get_attachment to file: %s", resultText(t, res))
	}
	if data, err := os.ReadFile(out); err != nil || string(data) != "hello" {
		t.Errorf("saved file = %q, %v", data, err)
	}

	f.bigAttachment = true
	res, _ = ny.handleGetAttachment(ctx, toolRequest(map[string]any{"message_id": "m-recv-1", "attachment_id": "att-1"}))
	if !res.IsError || !strings.Contains(resultText(t, res), "pass output_file") {
		t.Errorf("big attachment inline: %s", resultText(t, res))
	}
}

func TestNylas_HTMLToText(t *testing.T) {
	in := `<!DOCTYPE html><html><head><title>x</title><style>p{color:red}</style></head><body>
<!-- comment --><table><tr><td>Hello&nbsp;World</td></tr></table>
<p>First &amp; <b>bold</b><br/>second</p><div>third</div>
<script>alert(1)</script><ul><li>one</li><li>two</li></ul>Tail</body></html>`
	got := htmlToText(in)
	want := "Hello World\n\nFirst & bold\nsecond\n\nthird\n\none\ntwo\n\nTail"
	if got != want {
		t.Errorf("htmlToText =\n%q\nwant\n%q", got, want)
	}
	if nylasBodyText("plain < 10 and > 5\r\nnext") != "plain < 10 and > 5\nnext" {
		t.Errorf("plain text must pass unchanged: %q", nylasBodyText("plain < 10 and > 5\r\nnext"))
	}
	if imapSearchText(`a "b" \c`) != `TEXT "a \"b\" \\c"` {
		t.Errorf("imapSearchText = %q", imapSearchText(`a "b" \c`))
	}
}

func TestNylas_Config(t *testing.T) {
	if _, err := NewNylas(config.Connection{Name: "n", Type: config.TypeNylas, GrantID: "g"}, nil); err == nil || !strings.Contains(err.Error(), "token") {
		t.Errorf("without token: %v", err)
	}
	if _, err := NewNylas(config.Connection{Name: "n", Type: config.TypeNylas, Token: "k"}, nil); err == nil || !strings.Contains(err.Error(), "grant_id") {
		t.Errorf("without grant or user: %v", err)
	}
	if _, err := NewNylas(config.Connection{Name: "n", Type: config.TypeNylas, Token: "k", GrantID: "g", Region: "mars"}, nil); err == nil || !strings.Contains(err.Error(), "region") {
		t.Errorf("bad region: %v", err)
	}
	ny, err := NewNylas(config.Connection{Name: "n", Type: config.TypeNylas, Token: "k", GrantID: "g", Region: "eu", AllowSend: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ny.baseURL != "https://api.eu.nylas.com" || !ny.SendEnabled() {
		t.Errorf("ny = %+v", ny)
	}
	if got := DefaultInstructions(config.Connection{Type: config.TypeNylas, AllowSend: true}); !strings.Contains(got, "ball") || !strings.Contains(got, "Sending is enabled") {
		t.Errorf("instructions with sending: %q", got)
	}
	if got := DefaultInstructions(config.Connection{Type: config.TypeNylas}); strings.Contains(got, "Sending is enabled") {
		t.Errorf("instructions without sending mention sending")
	}
}

func TestConnectionAddNylas(t *testing.T) {
	isolateConfigDir(t)
	ct := NewConfigTools(&config.Config{}, nil)
	add := func(args map[string]any) (string, bool) {
		t.Helper()
		res, err := ct.handleConnectionAdd(context.Background(), toolRequest(args))
		if err != nil {
			t.Fatal(err)
		}
		return resultText(t, res), res.IsError
	}
	if text, isErr := add(map[string]any{"name": "db", "type": config.TypeMariaDB, "host": "x", "region": "eu"}); !isErr || !strings.Contains(text, "region applies to type") {
		t.Errorf("nylas field on another type: %q", text)
	}
	if text, isErr := add(map[string]any{"name": "m0", "type": config.TypeIMAP, "host": "imap.x", "user": "me@x", "grant_id": "g"}); !isErr || !strings.Contains(text, "grant_id applies to type") {
		t.Errorf("grant_id on imap: %q", text)
	}
	if text, isErr := add(map[string]any{"name": "n0", "type": config.TypeNylas, "smtp_host": "smtp.x"}); !isErr || !strings.Contains(text, "smtp_host applies to type") {
		t.Errorf("smtp_host on nylas: %q", text)
	}
	if text, isErr := add(map[string]any{"name": "n1", "type": config.TypeNylas}); !isErr || !strings.Contains(text, "grant_id") {
		t.Errorf("nylas without grant or user: %q", text)
	}
	if text, isErr := add(map[string]any{"name": "n2", "type": config.TypeNylas, "user": "not-an-address"}); !isErr || !strings.Contains(text, "mailbox address") {
		t.Errorf("nylas with a user that is no address: %q", text)
	}
	if text, isErr := add(map[string]any{"name": "n3", "type": config.TypeNylas, "grant_id": "g", "region": "mars"}); !isErr || !strings.Contains(text, "region") {
		t.Errorf("nylas with a bad region: %q", text)
	}
	text, isErr := add(map[string]any{"name": "n4", "type": config.TypeNylas, "grant_id": "g-1", "region": "EU", "allow_send": true, "from_name": "Jane Doe", "user": "jane@example.com"})
	if isErr {
		t.Fatal(text)
	}
	c := ct.cfg.FindAnyConnection("n4")
	if c == nil || c.Region != "eu" || c.GrantID != "g-1" || !c.AllowSend || c.FromName != "Jane Doe" || c.User != "jane@example.com" || !c.SendEnabled() {
		t.Errorf("stored connection = %+v", c)
	}
	text, isErr = add(map[string]any{"name": "n5", "type": config.TypeNylas, "user": "jane@example.com"})
	if isErr {
		t.Fatal(text)
	}
	if c := ct.cfg.FindAnyConnection("n5"); c == nil || c.Region != config.NylasRegionUS || c.SendEnabled() {
		t.Errorf("defaults = %+v", c)
	}

	res, err := ct.handleConnectionList(context.Background(), toolRequest(nil))
	if err != nil {
		t.Fatal(err)
	}
	if text := resultText(t, res); !strings.Contains(text, `"grant_id": "g-1"`) || !strings.Contains(text, `"region": "eu"`) {
		t.Errorf("connection_list lacks the nylas fields: %s", text)
	}
}
