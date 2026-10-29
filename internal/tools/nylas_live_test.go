//go:build live

// Live contract test for the nylas type. Runs only with:
//
//	NYLAS_LIVE_KEY=nyk_… NYLAS_LIVE_GRANT=<grant id> [NYLAS_LIVE_REGION=eu] \
//	  go test -tags 'notray live' ./internal/tools -run TestNylasLive -v
//
// It reads the mailbox, creates a reply draft and deletes it again; nothing is
// sent unless NYLAS_LIVE_SEND=1 — then one forward goes to the account's own
// address. Gated behind the build tag and the environment because it touches
// a real mailbox and counts against the Nylas plan.

package tools

import (
	"context"
	"encoding/json"
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
	t.Logf("grant %s: %s (%s), %d folders", info.GrantID, info.Email, info.Provider, info.Folders)

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
	for _, c := range page.Conversations {
		if c.Ball != "me" && c.Ball != "them" {
			t.Errorf("conversation %s: ball = %q", c.ConversationID, c.Ball)
		}
		t.Logf("  %s  ball=%s lastFromMe=%v  %q", c.LatestMessage.Date, c.Ball, c.LastFromMe, c.Subject)
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
		t.Errorf("conversation %s: thread lists %d messages, get_conversation returned %d", first.ConversationID, first.MessageCount, len(msgs))
	}
	for _, m := range msgs {
		if m.Body == "" {
			t.Errorf("message %s has no body text", m.ID)
		}
	}

	res, err = ny.handleSearchMessages(ctx, toolRequest(map[string]any{"query": strings.Fields(first.Subject + " x")[0], "limit": float64(3)}))
	if err != nil || res.IsError {
		t.Fatalf("search_messages: %v / %s", err, resultText(t, res))
	}
}

func TestNylasLive_Draft(t *testing.T) {
	ny := newLiveNylas(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if err := ny.ensureGrant(ctx); err != nil {
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
		DraftID   string `json:"draft_id"`
		InReplyTo string `json:"in_reply_to"`
	}
	if err := json.Unmarshal([]byte(resultText(t, res)), &draft); err != nil {
		t.Fatal(err)
	}
	if draft.DraftID == "" || draft.InReplyTo != conv.LatestMessage.ID {
		t.Errorf("draft = %+v, want a draft threaded on %s", draft, conv.LatestMessage.ID)
	}
	t.Logf("draft %s created in reply to %s", draft.DraftID, draft.InReplyTo)

	grantID, _, _ := ny.grant()
	if _, err := ny.callJSON(ctx, "draft.delete", map[string]string{"grant_id": grantID, "draft_id": draft.DraftID}, url.Values{}, nil, nil); err != nil {
		t.Fatalf("delete draft %s: %v", draft.DraftID, err)
	}
	t.Logf("draft %s deleted", draft.DraftID)
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
	if err := ny.ensureGrant(ctx); err != nil {
		t.Fatal(err)
	}
	_, me, _ := ny.grant()

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
	t.Logf("sent: %s", resultText(t, res))
}
