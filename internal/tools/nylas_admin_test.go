package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/smnhffmnn/mux/internal/config"
)

// Application mode: one key, every grant of the application. The fake lists
// the fixture's grants (grant-old, invalid, and grant-1) unless a test sets
// its own list; grant-2 is a Google mailbox of jane@example.org.

func newAppNylas(t *testing.T, f *nylasFake, readOnly, allowSend bool) *Nylas {
	t.Helper()
	return newTestNylasConn(t, f, config.Connection{
		Name: "mail", Type: config.TypeNylas, Token: "test-key", Region: "eu",
		FromName: "Jane Doe", AllowSend: allowSend, ReadOnly: readOnly,
	})
}

var twoValidGrants = []nylasGrant{
	{ID: "grant-old", Email: "jane@example.com", Provider: "imap", GrantStatus: "invalid"},
	{ID: "grant-1", Email: "jane@example.com", Provider: "imap", GrantStatus: "valid"},
	{ID: "grant-2", Email: "jane@example.org", Provider: "google", GrantStatus: "valid"},
}

func toolNames(ny *Nylas) string {
	var out []string
	for _, td := range ny.Tools() {
		out = append(out, td.Tool.Name)
	}
	return strings.Join(out, " ")
}

func TestNylasApp_ToolSets(t *testing.T) {
	f := newNylasFake(t)

	grant := newTestNylas(t, f, "grant-1", "", false)
	if names := toolNames(grant); strings.Contains(names, "get_grant") || strings.Contains(names, "api_get") || strings.Contains(names, "delete_grant") {
		t.Errorf("administration tools on a grant connection: %s", names)
	}
	for _, td := range grant.Tools() {
		if _, ok := td.Tool.InputSchema.Properties["mailbox"]; ok {
			t.Errorf("%s takes mailbox in grant mode", td.Tool.Name)
		}
	}

	app := newAppNylas(t, f, false, true)
	names := toolNames(app)
	for _, want := range []string{"list_conversations", "get_conversation", "search_messages", "list_mailboxes", "list_attachments", "get_attachment", "archive_conversation", "delete_conversation", "move_conversation", "create_reply_draft", "create_forward_draft", "send_draft", "reply_send", "forward_send", "get_grant", "delete_grant", "list_connectors", "list_webhooks", "api_get", "api_request"} {
		if !strings.Contains(" "+names+" ", " "+want+" ") {
			t.Errorf("application mode lacks %s: %s", want, names)
		}
	}
	for _, td := range app.Tools() {
		switch td.Tool.Name {
		case "list_connectors", "list_webhooks", "api_get", "api_request":
			continue
		}
		if _, ok := td.Tool.InputSchema.Properties["mailbox"]; !ok {
			t.Errorf("%s takes no mailbox in application mode", td.Tool.Name)
		}
	}

	// Read-only: reading and searching, the reading administration tools,
	// nothing that writes to a mailbox or the application.
	ro := newAppNylas(t, f, true, false)
	names = toolNames(ro)
	for _, want := range []string{"list_conversations", "get_conversation", "search_messages", "list_mailboxes", "list_attachments", "get_attachment", "get_grant", "list_connectors", "list_webhooks", "api_get"} {
		if !strings.Contains(" "+names+" ", " "+want+" ") {
			t.Errorf("read-only application mode lacks %s: %s", want, names)
		}
	}
	for _, never := range []string{"archive_conversation", "delete_conversation", "move_conversation", "create_reply_draft", "create_forward_draft", "send_draft", "reply_send", "forward_send", "delete_grant", "api_request"} {
		if strings.Contains(" "+names+" ", " "+never+" ") {
			t.Errorf("read-only application mode offers %s", never)
		}
	}
	roGrant := newTestNylasConn(t, f, config.Connection{Name: "mail", Type: config.TypeNylas, Token: "test-key", GrantID: "grant-1", ReadOnly: true})
	if names := toolNames(roGrant); strings.Contains(names, "archive_conversation") || strings.Contains(names, "create_reply_draft") || !strings.Contains(names, "search_messages") {
		t.Errorf("read-only grant mode: %s", names)
	}
}

func TestNylasApp_MailboxResolution(t *testing.T) {
	f := newNylasFake(t)
	ny := newAppNylas(t, f, false, false)
	ctx := context.Background()

	// The fixture has one valid grant: it is the default.
	res, _ := ny.handleListConversations(ctx, toolRequest(map[string]any{"limit": float64(2)}))
	if res.IsError {
		t.Fatalf("default mailbox: %s", resultText(t, res))
	}
	var page struct {
		Mailbox       string              `json:"mailbox"`
		Conversations []nylasConversation `json:"conversations"`
	}
	decodeResult(t, resultText(t, res), &page)
	if page.Mailbox != "jane@example.com" || len(page.Conversations) == 0 {
		t.Errorf("page = %+v", page)
	}
	if reqs := f.recorded(http.MethodGet, "/threads"); len(reqs) != 1 || !strings.HasPrefix(reqs[0].Path, "/v3/grants/grant-1/") {
		t.Errorf("threads read from %+v", reqs)
	}
	// The grant list is read once and cached; GET /v3/grants/{id} is not
	// needed in application mode.
	if n := len(f.recorded(http.MethodGet, "/v3/grants")); n != 1 {
		t.Errorf("grants listed %d times", n)
	}
	ny.handleListConversations(ctx, toolRequest(nil))
	if n := len(f.recorded(http.MethodGet, "/v3/grants")); n != 1 {
		t.Errorf("grants listed %d times after a second call", n)
	}
	if n := len(f.recorded(http.MethodGet, "/v3/grants/grant-1")); n != 0 {
		t.Errorf("grant read %d times by id", n)
	}

	// Two valid grants: mailbox is required, the error lists them.
	f2 := newNylasFake(t)
	f2.grants = twoValidGrants
	ny2 := newAppNylas(t, f2, false, false)
	res, _ = ny2.handleListConversations(ctx, toolRequest(nil))
	if !res.IsError || !strings.Contains(resultText(t, res), "pass mailbox") || !strings.Contains(resultText(t, res), "jane@example.org (google, grant-2)") {
		t.Errorf("two valid grants without mailbox: %s", resultText(t, res))
	}
	// By address: the valid grant wins over the invalid one with the same
	// address; case does not matter.
	res, _ = ny2.handleListConversations(ctx, toolRequest(map[string]any{"mailbox": "Jane@Example.com"}))
	if res.IsError {
		t.Fatalf("by address: %s", resultText(t, res))
	}
	if reqs := f2.recorded(http.MethodGet, "/threads"); len(reqs) != 1 || !strings.HasPrefix(reqs[0].Path, "/v3/grants/grant-1/") {
		t.Errorf("by address read %+v", reqs)
	}
	// By id, the Google mailbox: Gmail folders, its own address for fromMe.
	res, _ = ny2.handleGetConversation(ctx, toolRequest(map[string]any{"conversation_id": "tg", "mailbox": "grant-2"}))
	if res.IsError {
		t.Fatalf("by id: %s", resultText(t, res))
	}
	var msgs []nylasMessageFull
	decodeResult(t, resultText(t, res), &msgs)
	if len(msgs) != 2 || msgs[1].FromMe || !strings.Contains(strings.Join(msgs[0].Folders, ","), "Receipts") {
		t.Errorf("grant-2 conversation = %+v (jane@example.com is not grant-2's own address)", msgs)
	}
	// The same thread through grant-1 (if it had it) would say fromMe: the
	// per-mailbox state is separate — folders too.
	res, _ = ny2.handleListMailboxes(ctx, toolRequest(map[string]any{"mailbox": "grant-2"}))
	if res.IsError || !strings.Contains(resultText(t, res), `"IMPORTANT"`) {
		t.Errorf("grant-2 folders: %s", resultText(t, res))
	}
	res, _ = ny2.handleListMailboxes(ctx, toolRequest(map[string]any{"mailbox": "jane@example.com"}))
	if res.IsError || !strings.Contains(resultText(t, res), `"INBOX.Archive"`) || strings.Contains(resultText(t, res), `"IMPORTANT"`) {
		t.Errorf("grant-1 folders: %s", resultText(t, res))
	}

	// An invalid grant named by id is refused with the status.
	res, _ = ny2.handleListConversations(ctx, toolRequest(map[string]any{"mailbox": "grant-old"}))
	if !res.IsError || !strings.Contains(resultText(t, res), "is invalid") {
		t.Errorf("invalid grant: %s", resultText(t, res))
	}
	// An unknown mailbox: the list is refreshed once, then refused.
	before := len(f2.recorded(http.MethodGet, "/v3/grants"))
	res, _ = ny2.handleListConversations(ctx, toolRequest(map[string]any{"mailbox": "nobody@example.com"}))
	if !res.IsError || !strings.Contains(resultText(t, res), `no grant "nobody@example.com"`) {
		t.Errorf("unknown mailbox: %s", resultText(t, res))
	}
	if n := len(f2.recorded(http.MethodGet, "/v3/grants")); n != before+1 {
		t.Errorf("unknown mailbox refreshed the list %d times, want 1", n-before)
	}
	// A grant connected after the list was cached is found by the refresh.
	f2.grants = append(append([]nylasGrant{}, twoValidGrants...), nylasGrant{ID: "grant-3", Email: "new@example.com", Provider: "imap", GrantStatus: "valid"})
	if g, err := ny2.findGrant(ctx, "new@example.com"); err != nil || g.ID != "grant-3" {
		t.Errorf("fresh grant: %+v, %v", g, err)
	}

	// Two valid grants with the same address need the id.
	f3 := newNylasFake(t)
	f3.grants = []nylasGrant{
		{ID: "grant-1", Email: "jane@example.com", Provider: "imap", GrantStatus: "valid"},
		{ID: "grant-2", Email: "jane@example.com", Provider: "google", GrantStatus: "valid"},
	}
	ny3 := newAppNylas(t, f3, false, false)
	res, _ = ny3.handleListConversations(ctx, toolRequest(map[string]any{"mailbox": "jane@example.com"}))
	if !res.IsError || !strings.Contains(resultText(t, res), "pass the grant id") {
		t.Errorf("ambiguous address: %s", resultText(t, res))
	}
	// No grants at all, and no valid grant.
	f4 := newNylasFake(t)
	f4.grants = []nylasGrant{}
	res, _ = newAppNylas(t, f4, false, false).handleListConversations(ctx, toolRequest(nil))
	if !res.IsError || !strings.Contains(resultText(t, res), "has no grants") {
		t.Errorf("no grants: %s", resultText(t, res))
	}
	f4.grants = []nylasGrant{{ID: "grant-old", Email: "jane@example.com", Provider: "imap", GrantStatus: "invalid"}}
	res, _ = newAppNylas(t, f4, false, false).handleListConversations(ctx, toolRequest(nil))
	if !res.IsError || !strings.Contains(resultText(t, res), "none of the application's 1 grants is valid") {
		t.Errorf("no valid grant: %s", resultText(t, res))
	}

	// Grant mode refuses a mailbox argument that names another mailbox and
	// accepts its own.
	g := newTestNylas(t, f, "grant-1", "", false)
	res, _ = g.handleListConversations(ctx, toolRequest(map[string]any{"mailbox": "other@example.com"}))
	if !res.IsError || !strings.Contains(resultText(t, res), "serves one mailbox (jane@example.com)") {
		t.Errorf("foreign mailbox on a grant connection: %s", resultText(t, res))
	}
	res, _ = g.handleListConversations(ctx, toolRequest(map[string]any{"mailbox": "grant-1"}))
	if res.IsError {
		t.Errorf("own mailbox on a grant connection: %s", resultText(t, res))
	}
}

func TestNylasApp_GrantPaging(t *testing.T) {
	f := newNylasFake(t)
	for i := 0; i < 230; i++ {
		f.grants = append(f.grants, nylasGrant{ID: fmt.Sprintf("g-%03d", i), Email: fmt.Sprintf("user%03d@example.com", i), Provider: "imap", GrantStatus: "valid"})
	}
	ny := newAppNylas(t, f, false, false)
	grants, err := ny.listGrants(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(grants) != 230 || grants[229].ID != "g-229" {
		t.Errorf("paged grants = %d", len(grants))
	}
	reqs := f.recorded(http.MethodGet, "/v3/grants")
	if len(reqs) != 3 || reqs[1].Query.Get("offset") != "100" || reqs[2].Query.Get("offset") != "200" || reqs[0].Query.Get("limit") != "100" {
		t.Errorf("grant pages = %+v", reqs)
	}
}

func TestNylasApp_ListMailboxesAndGrants(t *testing.T) {
	f := newNylasFake(t)
	f.grants = twoValidGrants
	ny := newAppNylas(t, f, false, false)
	ctx := context.Background()

	res, _ := ny.handleListMailboxes(ctx, toolRequest(nil))
	if res.IsError {
		t.Fatalf("list_mailboxes: %s", resultText(t, res))
	}
	var listed struct {
		Mailboxes []nylasMailboxInfo `json:"mailboxes"`
		Note      string             `json:"note"`
	}
	decodeResult(t, resultText(t, res), &listed)
	if len(listed.Mailboxes) != 3 || listed.Mailboxes[2].Email != "jane@example.org" || listed.Mailboxes[2].Provider != "google" || listed.Mailboxes[0].GrantStatus != "invalid" || listed.Mailboxes[1].ID != "grant-1" || listed.Note == "" {
		t.Errorf("mailboxes = %+v", listed)
	}
	if strings.Contains(resultText(t, res), `"scope"`) || strings.Contains(resultText(t, res), `"settings"`) {
		t.Errorf("mailbox list carries grant details: %s", resultText(t, res))
	}
	// Listing reads the grants fresh every time.
	ny.handleListMailboxes(ctx, toolRequest(nil))
	if n := len(f.recorded(http.MethodGet, "/v3/grants")); n != 2 {
		t.Errorf("grants listed %d times for two list_mailboxes calls", n)
	}

	// get_grant reads the grant itself, aliases exposed; works on invalid
	// grants.
	res, _ = ny.handleGetGrant(ctx, toolRequest(map[string]any{"mailbox": "jane@example.org"}))
	if res.IsError {
		t.Fatalf("get_grant: %s", resultText(t, res))
	}
	var info nylasMailboxInfo
	decodeResult(t, resultText(t, res), &info)
	if info.ID != "grant-2" || info.Name != "Jane Doe" || strings.Join(info.EmailAliases, ",") != "j.doe@example.org" || len(info.Scope) != 1 || info.CreatedAt != "2026-10-28T12:20:00Z" {
		t.Errorf("grant = %+v", info)
	}
	if reqs := f.recorded(http.MethodGet, "/v3/grants/grant-2"); len(reqs) != 1 || reqs[0].Query.Get("expose_aliases") != "true" {
		t.Errorf("grant reads = %+v", reqs)
	}
	res, _ = ny.handleGetGrant(ctx, toolRequest(nil))
	if !res.IsError || !strings.Contains(resultText(t, res), "mailbox is required") {
		t.Errorf("get_grant without mailbox: %s", resultText(t, res))
	}

	// delete_grant: never by default, by address or id, caches forgotten.
	res, _ = ny.handleDeleteGrant(ctx, toolRequest(nil))
	if !res.IsError || !strings.Contains(resultText(t, res), "nothing is deleted by default") || len(f.deleted) != 0 {
		t.Errorf("delete_grant without mailbox: %s", resultText(t, res))
	}
	ny.handleListConversations(ctx, toolRequest(map[string]any{"mailbox": "grant-2"})) // populate the per-grant state
	res, _ = ny.handleDeleteGrant(ctx, toolRequest(map[string]any{"mailbox": "jane@example.org"}))
	if res.IsError {
		t.Fatalf("delete_grant: %s", resultText(t, res))
	}
	if strings.Join(f.deleted, ",") != "grant-2" {
		t.Errorf("deleted = %v", f.deleted)
	}
	if text := resultText(t, res); !strings.Contains(text, `"deleted": true`) || !strings.Contains(text, `"grant_id": "grant-2"`) || !strings.Contains(text, `"email": "jane@example.org"`) || strings.Contains(text, "password") {
		t.Errorf("delete_grant result: %s", text)
	}
	ny.mu.Lock()
	_, kept := ny.mailboxes["grant-2"]
	cached := ny.grants != nil
	ny.mu.Unlock()
	if kept || cached {
		t.Errorf("caches after delete: mailbox kept %v, grant list kept %v", kept, cached)
	}
	// An IMAP grant's note says the password left with it.
	res, _ = ny.handleDeleteGrant(ctx, toolRequest(map[string]any{"mailbox": "grant-old"}))
	if res.IsError || !strings.Contains(resultText(t, res), "password left") {
		t.Errorf("delete imap grant: %s", resultText(t, res))
	}

	res, _ = ny.handleListConnectors(ctx, toolRequest(nil))
	if res.IsError {
		t.Fatalf("list_connectors: %s", resultText(t, res))
	}
	var connectors []map[string]any
	decodeResult(t, resultText(t, res), &connectors)
	if len(connectors) != 2 || connectors[0]["provider"] != "google" || connectors[0]["activeCredentialId"] != "cred-google-1" || connectors[1]["provider"] != "imap" || connectors[1]["activeCredentialId"] != nil {
		t.Errorf("connectors = %+v", connectors)
	}

	res, _ = ny.handleListWebhooks(ctx, toolRequest(nil))
	if res.IsError {
		t.Fatalf("list_webhooks: %s", resultText(t, res))
	}
	var hooks []map[string]any
	decodeResult(t, resultText(t, res), &hooks)
	if len(hooks) != 1 || hooks[0]["id"] != "hook-1" || hooks[0]["webhook_url"] != "https://hooks.example.com/nylas" {
		t.Errorf("webhooks = %+v", hooks)
	}
	if _, leaked := hooks[0]["webhook_secret"]; leaked || strings.Contains(resultText(t, res), "must-not-leak") {
		t.Errorf("webhook secret passed through: %s", resultText(t, res))
	}
}

func TestNylasApp_Verify(t *testing.T) {
	f := newNylasFake(t)
	f.grants = twoValidGrants
	ny := newAppNylas(t, f, false, false)
	info, err := ny.Verify(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !info.Application || len(info.Grants) != 3 || info.Grants[1].GrantStatus != "valid" || info.Email != "" {
		t.Errorf("verify = %+v", info)
	}
	if n := len(f.recorded(http.MethodGet, "/folders")); n != 0 {
		t.Errorf("verify in application mode listed folders %d times", n)
	}
	// An application key that cannot list grants (a grant-bound key, say)
	// fails here, not on the first tool call.
	ny.token = "wrong"
	if _, err := ny.Verify(context.Background()); err == nil || !strings.Contains(err.Error(), "HTTP 401") {
		t.Errorf("verify with a bad key: %v", err)
	}
}

func TestNylasApp_PassThrough(t *testing.T) {
	f := newNylasFake(t)
	ny := newAppNylas(t, f, false, false)
	ctx := context.Background()

	res, _ := ny.handleAPIGet(ctx, toolRequest(map[string]any{"path": "/v3/calendars?limit=5"}))
	if res.IsError || !strings.HasPrefix(resultText(t, res), "HTTP 200 OK\n\n") || !strings.Contains(resultText(t, res), `"query":"limit=5"`) {
		t.Errorf("api_get: %s", resultText(t, res))
	}
	res, _ = ny.handleAPIGet(ctx, toolRequest(map[string]any{"path": "v3/applications"}))
	if res.IsError || !strings.Contains(resultText(t, res), `"path":"/v3/applications"`) {
		t.Errorf("api_get without leading slash: %s", resultText(t, res))
	}
	res, _ = ny.handleAPIRequest(ctx, toolRequest(map[string]any{"method": "patch", "path": "/v3/applications", "body": `{"region":"eu"}`}))
	if res.IsError || !strings.Contains(resultText(t, res), `"method":"PATCH"`) || !strings.Contains(resultText(t, res), `\"region\":\"eu\"`) {
		t.Errorf("api_request: %s", resultText(t, res))
	}
	if reqs := f.recorded(http.MethodPatch, "/v3/applications"); len(reqs) != 1 || reqs[0].Headers.Get("Content-Type") != "application/json" || reqs[0].Headers.Get("Authorization") != "Bearer test-key" {
		t.Errorf("api_request headers = %+v", reqs)
	}

	// Refused before any request goes out.
	before := len(f.requests)
	for path, want := range map[string]string{
		"/v3/../v2/grants":                     "not normalized",
		"/v3//grants":                          "not normalized",
		"/v3/./grants":                         "not normalized",
		"/v3/%2e%2e/grants":                    "encoded",
		"/v3/grants%2Fx":                       "encoded",
		"/v2/grants":                           "must start with /v3/",
		"/v3/":                                 "must start with /v3/",
		"/grants":                              "must start with /v3/",
		"https://api.eu.nylas.com/v3/grants":   "not a URL",
		"//evil.example/v3/grants":             "not a URL",
		"/v3/grants/grant-1/messages/send":     "sending mail is not done through api_request",
		"/v3/grants/grant-1/drafts/draft-9":    "sending mail is not done through api_request",
		"/v3/grants/grant-1/drafts/draft-9/":   "sending mail is not done through api_request",
		"/v3/grants/grant-1/messages/send?x=1": "sending mail is not done through api_request",
	} {
		res, _ = ny.handleAPIRequest(ctx, toolRequest(map[string]any{"method": "POST", "path": path, "body": `{}`}))
		if !res.IsError || !strings.Contains(resultText(t, res), want) {
			t.Errorf("api_request %s: want %q, got %s", path, want, resultText(t, res))
		}
		if strings.Contains(want, "sending") {
			continue // api_get has no send to refuse; the path itself is fine
		}
		res, _ = ny.handleAPIGet(ctx, toolRequest(map[string]any{"path": path}))
		if !res.IsError || !strings.Contains(resultText(t, res), want) {
			t.Errorf("api_get %s: want %q, got %s", path, want, resultText(t, res))
		}
	}
	if len(f.requests) != before {
		t.Fatalf("refused paths must not produce requests, got %d", len(f.requests)-before)
	}
	// Updating a draft (PUT) and deleting one are not sends.
	for _, method := range []string{"PUT", "DELETE"} {
		if u, err := ny.nylasAPIPath("/v3/grants/grant-1/drafts/draft-9"); err != nil || !nylasSendPath.MatchString(strings.TrimPrefix(u, ny.baseURL)) {
			t.Errorf("%s draft path: %s, %v", method, u, err)
		}
	}
	res, _ = ny.handleAPIRequest(ctx, toolRequest(map[string]any{"method": "DELETE", "path": "/v3/grants/grant-1/drafts/draft-9"}))
	if res.IsError {
		t.Errorf("DELETE draft through api_request: %s", resultText(t, res))
	}
	res, _ = ny.handleAPIRequest(ctx, toolRequest(map[string]any{"method": "POST", "path": "/v3/applications", "body": "{not json"}))
	if !res.IsError || !strings.Contains(resultText(t, res), "valid JSON") {
		t.Errorf("bad body: %s", resultText(t, res))
	}
	res, _ = ny.handleAPIRequest(ctx, toolRequest(map[string]any{"method": "GET", "path": "/v3/applications"}))
	if !res.IsError || !strings.Contains(resultText(t, res), "POST, PUT, PATCH or DELETE") {
		t.Errorf("GET on api_request: %s", resultText(t, res))
	}
	// Errors keep the upstream body.
	res, _ = ny.handleAPIGet(ctx, toolRequest(map[string]any{"path": "/v3/grants/grant-1/messages/nope"}))
	if !res.IsError || !strings.HasPrefix(resultText(t, res), "HTTP 404 Not Found") || !strings.Contains(resultText(t, res), "request_id") {
		t.Errorf("api_get 404: %s", resultText(t, res))
	}
}

func TestNylasApp_SendAndDraftsPerMailbox(t *testing.T) {
	f := newNylasFake(t)
	f.grants = twoValidGrants
	ny := newAppNylas(t, f, false, true)
	ctx := context.Background()

	// A reply from grant-2 carries grant-2's address in From and is keyed
	// on grant-2.
	res, _ := ny.handleReplySend(ctx, toolRequest(map[string]any{"mailbox": "grant-2", "conversation_id": "tg", "message_id": "mg-1", "body": "Thanks"}))
	if res.IsError {
		t.Fatalf("reply_send on grant-2: %s", resultText(t, res))
	}
	sends := f.recorded(http.MethodPost, "/messages/send")
	if len(sends) != 1 || !strings.HasPrefix(sends[0].Path, "/v3/grants/grant-2/") {
		t.Fatalf("sends = %+v", sends)
	}
	body := lastBody(t, sends)
	if fmt.Sprint(body["from"]) != "[map[email:jane@example.org name:Jane Doe]]" || fmt.Sprint(body["to"]) != "[map[email:shop@example.net name:Shop]]" {
		t.Errorf("grant-2 reply body = %v", body)
	}
	key2 := sends[0].Headers.Get("Idempotency-Key")
	// The same reply from grant-1 is another message: another key.
	ny.handleReplySend(ctx, toolRequest(map[string]any{"mailbox": "grant-1", "conversation_id": "t1", "message_id": "m-recv-1", "body": "Thanks"}))
	sends = f.recorded(http.MethodPost, "/messages/send")
	if len(sends) != 2 || !strings.HasPrefix(sends[1].Path, "/v3/grants/grant-1/") || sends[1].Headers.Get("Idempotency-Key") == key2 {
		t.Errorf("grant-1 send = %+v", sends)
	}
	// Without mailbox, two valid grants: refused before anything is sent.
	res, _ = ny.handleSendDraft(ctx, toolRequest(map[string]any{"draft_id": "draft-9"}))
	if !res.IsError || !strings.Contains(resultText(t, res), "pass mailbox") || len(f.recorded(http.MethodPost, "/drafts/draft-9")) != 0 {
		t.Errorf("send_draft without mailbox: %s", resultText(t, res))
	}
	res, _ = ny.handleCreateForwardDraft(ctx, toolRequest(map[string]any{"mailbox": "jane@example.org", "conversation_id": "tg", "to": "dave@example.org"}))
	if res.IsError {
		t.Fatalf("forward draft on grant-2: %s", resultText(t, res))
	}
	if posts := f.recorded(http.MethodPost, "/drafts"); len(posts) != 1 || !strings.HasPrefix(posts[0].Path, "/v3/grants/grant-2/") {
		t.Errorf("draft posts = %+v", posts)
	}
	// Archive on Gmail through the application key: label arithmetic as in
	// grant mode.
	res, _ = ny.handleArchiveConversation(ctx, toolRequest(map[string]any{"mailbox": "grant-2", "conversation_id": "tg"}))
	if res.IsError || !strings.Contains(resultText(t, res), "All Mail") {
		t.Errorf("archive on grant-2: %s", resultText(t, res))
	}
	if puts := f.recorded(http.MethodPut, ""); len(puts) != 1 || !strings.HasPrefix(puts[0].Path, "/v3/grants/grant-2/") || !strings.Contains(puts[0].Body, `"folders":["IMPORTANT","Label_12"]`) {
		t.Errorf("archive PUTs = %+v", puts)
	}
}

func TestNylasApp_ConfigAndSummary(t *testing.T) {
	app := config.Connection{Name: "n", Type: config.TypeNylas, Token: "k"}
	if !config.NylasApplicationMode(app) || !app.Enabled() {
		t.Errorf("application mode: mode %v enabled %v", config.NylasApplicationMode(app), app.Enabled())
	}
	grant := config.Connection{Name: "n", Type: config.TypeNylas, Token: "k", GrantID: "g"}
	byUser := config.Connection{Name: "n", Type: config.TypeNylas, Token: "k", User: "jane@example.com"}
	if config.NylasApplicationMode(grant) || config.NylasApplicationMode(byUser) {
		t.Error("grant or user set still reads as application mode")
	}
	noKey := config.Connection{Name: "n", Type: config.TypeNylas}
	if noKey.Enabled() {
		t.Error("enabled without a key")
	}
	ro := config.Connection{Name: "n", Type: config.TypeNylas, Token: "k", ReadOnly: true, AllowSend: true}
	if ro.SendEnabled() {
		t.Error("a read-only connection sends")
	}
	if err := config.ValidateNylas(ro); err == nil {
		t.Error("read_only with allow_send validated")
	}
	if got := json.Valid([]byte(`{}`)); !got {
		t.Fatal("json")
	}
}
