//go:build live

// Live contract test for the nylas type. Runs only with:
//
//	NYLAS_LIVE_KEY=nyk_… NYLAS_LIVE_GRANT=<grant id> [NYLAS_LIVE_REGION=eu] \
//	  go test -tags 'notray live' ./internal/tools -run TestNylasLive -v
//
// It reads the mailbox, creates a reply draft and deletes it again; nothing is
// sent unless NYLAS_LIVE_SEND=1 — then one forward goes to the account's own
// address. Gated behind the build tag and the environment because it touches
// a real mailbox and counts against the Nylas plan. The log carries counts and
// ids only: in CI it is public.

package tools

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/smnhffmnn/mux/internal/config"
)

func newLiveNylas(t *testing.T) *Nylas {
	t.Helper()
	key := os.Getenv("NYLAS_LIVE_KEY")
	if key == "" {
		t.Skip("NYLAS_LIVE_KEY not set")
	}
	if os.Getenv("NYLAS_LIVE_GRANT") == "" && os.Getenv("NYLAS_LIVE_EMAIL") == "" {
		t.Skip("neither NYLAS_LIVE_GRANT nor NYLAS_LIVE_EMAIL set")
	}
	conn := config.Connection{
		Name:      "nylas-live",
		Type:      config.TypeNylas,
		Token:     key,
		Region:    os.Getenv("NYLAS_LIVE_REGION"),
		GrantID:   os.Getenv("NYLAS_LIVE_GRANT"),
		User:      os.Getenv("NYLAS_LIVE_EMAIL"),
		AllowSend: os.Getenv("NYLAS_LIVE_SEND") == "1",
		FromName:  "mux live test",
	}
	ny, err := NewNylas(conn, nil)
	if err != nil {
		t.Fatal(err)
	}
	return ny
}

func TestNylasLive_Read(t *testing.T) {
	ny := newLiveNylas(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	info, err := ny.Verify(ctx)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	t.Logf("grant %s: provider %s, %d folders", info.GrantID, info.Provider, info.Folders)

	res, err := ny.handleListMailboxes(ctx, toolRequest(nil))
	if err != nil || res.IsError {
		t.Fatalf("list_mailboxes: %v / %s", err, resultText(t, res))
	}

	res, err = ny.handleListConversations(ctx, toolRequest(map[string]any{"limit": float64(5)}))
	if err != nil || res.IsError {
		t.Fatalf("list_conversations: %v / %s", err, resultText(t, res))
	}
	var page struct {
		Folder        string              `json:"folder"`
		Conversations []nylasConversation `json:"conversations"`
		NextPageToken string              `json:"nextPageToken"`
	}
	if err := json.Unmarshal([]byte(resultText(t, res)), &page); err != nil {
		t.Fatal(err)
	}
	t.Logf("folder %s: %d conversations, next page token set: %v", page.Folder, len(page.Conversations), page.NextPageToken != "")
	if len(page.Conversations) == 0 {
		t.Skip("inbox is empty — nothing more to read")
	}
	for i, c := range page.Conversations {
		if c.Ball != "me" && c.Ball != "them" {
			t.Errorf("conversation %d: ball = %q", i, c.Ball)
		}
		t.Logf("  conversation %d: %d messages, ball=%s lastFromMe=%v latestIsDraft=%v", i, c.MessageCount, c.Ball, c.LastFromMe, c.LatestMessage.IsDraft)
	}

	first := page.Conversations[0]
	res, err = ny.handleGetConversation(ctx, toolRequest(map[string]any{"conversation_id": first.ConversationID}))
	if err != nil || res.IsError {
		t.Fatalf("get_conversation: %v / %s", err, resultText(t, res))
	}
	var msgs []nylasMessageFull
	if err := json.Unmarshal([]byte(resultText(t, res)), &msgs); err != nil {
		t.Fatal(err)
	}
	if len(msgs) != first.MessageCount {
		t.Errorf("first conversation: thread lists %d messages, get_conversation returned %d", first.MessageCount, len(msgs))
	}
	for i, m := range msgs {
		if m.Body == "" {
			t.Errorf("message %d has no body text", i)
		}
	}

	res, err = ny.handleSearchMessages(ctx, toolRequest(map[string]any{"query": strings.Fields(first.Subject + " x")[0], "limit": float64(3)}))
	if err != nil || res.IsError {
		t.Fatalf("search_messages: %v / %s", err, resultText(t, res))
	}
	var found []nylasConversation
	if err := json.Unmarshal([]byte(resultText(t, res)), &found); err != nil {
		t.Fatal(err)
	}
	t.Logf("search: %d conversations", len(found))
	for i, c := range found {
		if c.Scope != "" || (c.Ball != "me" && c.Ball != "them") {
			t.Errorf("search result %d: scope %q, ball %q — a cached search must return whole conversations", i, c.Scope, c.Ball)
		}
	}
}

func TestNylasLive_Draft(t *testing.T) {
	ny := newLiveNylas(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if _, err := ny.boundMailbox(ctx); err != nil {
		t.Fatal(err)
	}

	res, err := ny.handleListConversations(ctx, toolRequest(map[string]any{"limit": float64(1)}))
	if err != nil || res.IsError {
		t.Fatalf("list_conversations: %v / %s", err, resultText(t, res))
	}
	var page struct {
		Conversations []nylasConversation `json:"conversations"`
	}
	if err := json.Unmarshal([]byte(resultText(t, res)), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Conversations) == 0 {
		t.Skip("inbox is empty")
	}
	conv := page.Conversations[0]

	res, err = ny.handleCreateReplyDraft(ctx, toolRequest(map[string]any{
		"conversation_id": conv.ConversationID,
		"body":            "mux live test — this draft is deleted by the test right away.",
	}))
	if err != nil || res.IsError {
		t.Fatalf("create_reply_draft: %v / %s", err, resultText(t, res))
	}
	var draft struct {
		DraftID  string   `json:"draft_id"`
		ThreadID string   `json:"thread_id"`
		Warnings []string `json:"warnings"`
	}
	if err := json.Unmarshal([]byte(resultText(t, res)), &draft); err != nil {
		t.Fatal(err)
	}
	if draft.DraftID == "" {
		t.Fatalf("no draft id in %s", resultText(t, res))
	}
	// The real test: Nylas filed the draft in the conversation's thread.
	if draft.ThreadID != "" && draft.ThreadID != conv.ConversationID {
		t.Errorf("draft landed in thread %s, conversation is %s (warnings: %v)", draft.ThreadID, conv.ConversationID, draft.Warnings)
	}
	t.Logf("draft created in the conversation's thread (thread id reported: %v, %d warnings)", draft.ThreadID != "", len(draft.Warnings))

	mb, err := ny.boundMailbox(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ny.callJSON(ctx, "draft.delete", map[string]string{"grant_id": mb.grantID, "draft_id": draft.DraftID}, url.Values{}, nil, nil); err != nil {
		t.Fatalf("delete draft %s: %v", draft.DraftID, err)
	}
	t.Logf("draft %s deleted", draft.DraftID)
}

// TestNylasLive_MessageIDPath reads one message whose id holds "/" through
// GET /v3/grants/{grant}/messages/{id}: the id goes into the path as one
// percent-encoded segment, and the API must take it that way (MUX-56). On an
// IMAP grant the ids are RFC 822 Message-IDs; CI notifications carry "/" in
// them. Skips when the newest messages have none. Nothing is written; the
// log carries counts and, on failure, the HTTP status — not the id, which
// names the sender's project.
func TestNylasLive_MessageIDPath(t *testing.T) {
	ny := newLiveNylas(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	mb, err := ny.boundMailbox(ctx)
	if err != nil {
		t.Fatal(err)
	}
	q := url.Values{}
	q.Set("limit", "50")
	var msgs []nylasMessage
	if _, err := ny.callJSON(ctx, "messages", map[string]string{"grant_id": mb.grantID}, q, nil, &msgs); err != nil {
		t.Fatalf("list messages: %v", err)
	}
	var withSlash *nylasMessage
	for i := range msgs {
		if strings.Contains(msgs[i].ID, "/") {
			withSlash = &msgs[i]
			break
		}
	}
	if withSlash == nil {
		t.Skipf("none of the %d newest messages has a / in its id", len(msgs))
	}
	var got nylasMessage
	if _, err := ny.callJSON(ctx, "message", map[string]string{"grant_id": mb.grantID, "message_id": withSlash.ID}, nil, nil, &got); err != nil {
		// The error text carries the path with the id; log only what kind
		// of answer it was.
		var httpErr *nylasHTTPError
		if errors.As(err, &httpErr) {
			kind := ""
			if httpErr.API != nil {
				kind = " " + httpErr.API.Type
			}
			t.Fatalf("GET message with / in its id (%d characters, %d slashes): HTTP %d%s", len(withSlash.ID), strings.Count(withSlash.ID, "/"), httpErr.Status, kind)
		}
		t.Fatalf("GET message with / in its id (%d characters, %d slashes): %T", len(withSlash.ID), strings.Count(withSlash.ID, "/"), err)
	}
	if got.ID != withSlash.ID {
		t.Fatalf("GET message with / in its id returned another message")
	}
	t.Logf("message with / in its id (%d slashes) read through the path; %d messages listed", strings.Count(withSlash.ID, "/"), len(msgs))
}

// TestNylasLive_SendToSelf forwards the newest inbox message to the account's
// own address — only with NYLAS_LIVE_SEND=1, because it sends mail.
func TestNylasLive_SendToSelf(t *testing.T) {
	if os.Getenv("NYLAS_LIVE_SEND") != "1" {
		t.Skip("NYLAS_LIVE_SEND != 1")
	}
	ny := newLiveNylas(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	mb, err := ny.boundMailbox(ctx)
	if err != nil {
		t.Fatal(err)
	}
	me := mb.email

	res, err := ny.handleListConversations(ctx, toolRequest(map[string]any{"limit": float64(1)}))
	if err != nil || res.IsError {
		t.Fatalf("list_conversations: %v / %s", err, resultText(t, res))
	}
	var page struct {
		Conversations []nylasConversation `json:"conversations"`
	}
	if err := json.Unmarshal([]byte(resultText(t, res)), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Conversations) == 0 {
		t.Skip("inbox is empty")
	}
	conv := page.Conversations[0]
	res, err = ny.handleForwardSend(ctx, toolRequest(map[string]any{
		"conversation_id": conv.ConversationID,
		"message_id":      conv.LatestMessage.ID,
		"to":              me,
		"body":            "mux live test: forward to self",
	}))
	if err != nil || res.IsError {
		t.Fatalf("forward_send: %v / %s", err, resultText(t, res))
	}
	var sent struct {
		Sent     bool     `json:"sent"`
		Warnings []string `json:"warnings"`
	}
	if err := json.Unmarshal([]byte(resultText(t, res)), &sent); err != nil {
		t.Fatal(err)
	}
	t.Logf("forward to self: sent=%v, %d warnings", sent.Sent, len(sent.Warnings))
}

// TestNylasLive_Application exercises the application mode with an
// application key (NYLAS_LIVE_APP_KEY, NYLAS_LIVE_REGION as for the grant
// test): list the grants, read the inbox of the first valid one through the
// mailbox argument, read its grant and the connectors. Nothing is written.
// Run by hand — an application key reaches every mailbox and does not belong
// in CI. The log carries counts and ids, no addresses.
func TestNylasLive_Application(t *testing.T) {
	key := os.Getenv("NYLAS_LIVE_APP_KEY")
	if key == "" {
		t.Skip("NYLAS_LIVE_APP_KEY not set")
	}
	ny, err := NewNylas(config.Connection{
		Name:   "nylas-live-app",
		Type:   config.TypeNylas,
		Token:  key,
		Region: os.Getenv("NYLAS_LIVE_REGION"),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !ny.ApplicationMode() {
		t.Fatal("a connection without grant_id and user must be in application mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	info, err := ny.Verify(ctx)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	valid := 0
	for _, g := range info.Grants {
		if g.GrantStatus == "valid" {
			valid++
		}
	}
	t.Logf("application: %d grants, %d valid", len(info.Grants), valid)

	res, err := ny.handleListMailboxes(ctx, toolRequest(nil))
	if err != nil || res.IsError {
		t.Fatalf("list_mailboxes: %v / %s", err, resultText(t, res))
	}
	var listed struct {
		Mailboxes []nylasMailboxInfo `json:"mailboxes"`
	}
	if err := json.Unmarshal([]byte(resultText(t, res)), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Mailboxes) != len(info.Grants) {
		t.Errorf("list_mailboxes lists %d mailboxes, verify saw %d grants", len(listed.Mailboxes), len(info.Grants))
	}
	var first *nylasMailboxInfo
	for i := range listed.Mailboxes {
		if listed.Mailboxes[i].GrantStatus == "valid" {
			first = &listed.Mailboxes[i]
			break
		}
	}
	if first == nil {
		t.Skip("no valid grant in the application")
	}

	res, err = ny.handleGetGrant(ctx, toolRequest(map[string]any{"mailbox": first.ID}))
	if err != nil || res.IsError {
		t.Fatalf("get_grant: %v / %s", err, resultText(t, res))
	}
	res, err = ny.handleListConversations(ctx, toolRequest(map[string]any{"mailbox": first.ID, "limit": float64(3)}))
	if err != nil || res.IsError {
		t.Fatalf("list_conversations by grant id: %v / %s", err, resultText(t, res))
	}
	var page struct {
		Mailbox       string              `json:"mailbox"`
		Conversations []nylasConversation `json:"conversations"`
	}
	if err := json.Unmarshal([]byte(resultText(t, res)), &page); err != nil {
		t.Fatal(err)
	}
	if page.Mailbox != strings.ToLower(first.Email) {
		t.Errorf("list_conversations reports another mailbox than grant %s", first.ID)
	}
	t.Logf("mailbox %s (%s): %d conversations", first.Provider, first.ID, len(page.Conversations))
	for i, c := range page.Conversations {
		if c.Ball != "me" && c.Ball != "them" {
			t.Errorf("conversation %d: ball = %q", i, c.Ball)
		}
	}
	// By address as well.
	res, err = ny.handleListMailboxes(ctx, toolRequest(map[string]any{"mailbox": first.Email}))
	if err != nil || res.IsError {
		t.Fatalf("list_mailboxes by address: %v / %s", err, resultText(t, res))
	}
	res, err = ny.handleListConnectors(ctx, toolRequest(nil))
	if err != nil || res.IsError {
		t.Fatalf("list_connectors: %v / %s", err, resultText(t, res))
	}
	var connectors []map[string]any
	if err := json.Unmarshal([]byte(resultText(t, res)), &connectors); err != nil {
		t.Fatal(err)
	}
	t.Logf("%d connectors", len(connectors))
	res, err = ny.handleAPIGet(ctx, toolRequest(map[string]any{"path": "/v3/applications"}))
	if err != nil || res.IsError {
		t.Fatalf("api_get /v3/applications: %v / %s", err, resultText(t, res))
	}
}
