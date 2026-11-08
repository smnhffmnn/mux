//go:build live

// Live contract test for the bunq type against the bunq sandbox. Runs with:
//
//	go test -tags 'notray live' ./internal/tools -run TestBunqLive -v
//
// It needs no secret: the sandbox hands out a user and an API key on request
// (POST /v1/sandbox-user-person), and test money comes from bunq's
// sugardaddy. The test walks the whole way in — key, installation, device,
// session — reads accounts, payments, events and drafts, files a draft
// payment, exports a statement, uses the pass-through, loses its session on
// purpose and signs in again, and starts a second connection on the same
// secrets to prove the installation is reused. Production is never touched:
// BUNQ_LIVE_URL may point elsewhere, but the sandbox-user call exists only
// there.

package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/smnhffmnn/mux/internal/config"
)

func bunqLiveRequest(args map[string]any) mcp.CallToolRequest {
	var req mcp.CallToolRequest
	req.Params.Arguments = args
	return req
}

// bunqLiveOK turns a handler's (result, error) into the result, failing the
// test on a transport error; f(g()) keeps the call sites on one line.
func bunqLiveOK(t *testing.T) func(*mcp.CallToolResult, error) *mcp.CallToolResult {
	return func(res *mcp.CallToolResult, err error) *mcp.CallToolResult {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
}

func bunqLiveText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	if len(res.Content) == 0 {
		t.Fatal("empty result")
	}
	text, ok := res.Content[0].(mcp.TextContent)
	if !ok {
		t.Fatalf("not text content: %T", res.Content[0])
	}
	if res.IsError {
		t.Fatalf("tool error: %s", text.Text)
	}
	return text.Text
}

func bunqLiveJSON(t *testing.T, res *mcp.CallToolResult) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(bunqLiveText(t, res)), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// newSandboxUser asks the sandbox for a user and returns its API key.
func newSandboxUser(t *testing.T, base string) string {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, base+"/v1/sandbox-user-person", nil)
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("User-Agent", "mux live test")
	req.Header.Set("X-Bunq-Client-Request-Id", bunqRequestID())
	req.Header.Set("X-Bunq-Geolocation", "0 0 0 0 000")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("sandbox-user-person: HTTP %d: %s", resp.StatusCode, data)
	}
	var env bunqEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		t.Fatal(err)
	}
	var ak struct {
		APIKey string `json:"api_key"`
	}
	if err := env.decode("ApiKey", &ak); err != nil || ak.APIKey == "" {
		t.Fatalf("sandbox-user-person: no api key in %s", data)
	}
	return ak.APIKey
}

func TestBunqLive(t *testing.T) {
	base := os.Getenv("BUNQ_LIVE_URL")
	if base == "" {
		base = config.BunqSandboxURL
	}
	apiKey := newSandboxUser(t, base)
	secrets := &bunqMemSecrets{} // from bunq_test.go
	conn := config.Connection{Name: "bunq-live", Type: config.TypeBunq, URL: base, Token: apiKey}
	bq, err := NewBunq(conn, nil)
	if err != nil {
		t.Fatal(err)
	}
	bq.secrets = secrets
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	ok := bunqLiveOK(t)

	info, err := bq.Verify(ctx)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	t.Logf("user %s %d, %d accounts (%d active), session until %s", info.UserType, info.UserID, info.Accounts, info.Active, info.Expires.Format(time.RFC3339))
	if info.Accounts == 0 || bq.installs != 1 {
		t.Fatalf("expected one account and one installation, got %d accounts, %d installations", info.Accounts, bq.installs)
	}
	if _, err := secrets.Get("bunq-live-private-key"); err != nil {
		t.Fatal("private key not stored")
	}
	if _, err := secrets.Get("bunq-live-api-context"); err != nil {
		t.Fatal("api context not stored")
	}

	accounts := bunqLiveJSON(t, ok(bq.handleListAccounts(ctx, bunqLiveRequest(nil))))
	list := accounts["accounts"].([]any)
	acc := list[0].(map[string]any)
	accountID := int(acc["id"].(float64))
	iban, _ := acc["iban"].(string)
	t.Logf("account %d %s balance %v", accountID, acc["type"], acc["balance"])
	if iban == "" {
		t.Fatal("account without IBAN")
	}

	// Test money: a request to bunq's sugardaddy, granted at once in the
	// sandbox. Through the raw path — the type has no request tool, on
	// purpose — so the payment list has something to show.
	body, _ := json.Marshal(map[string]any{
		"amount_inquired":    map[string]string{"value": "100.00", "currency": "EUR"},
		"counterparty_alias": map[string]string{"type": "EMAIL", "value": "sugardaddy@bunq.com", "name": "Sugar Daddy"},
		"description":        "mux live test",
		"allow_bunqme":       false,
	})
	resp, err := bq.callRaw(ctx, http.MethodPost, fmt.Sprintf("/v1/user/{user}/monetary-account/%d/request-inquiry", accountID), body)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("request-inquiry: HTTP %d", resp.StatusCode)
	}
	time.Sleep(3 * time.Second)

	payments := bunqLiveJSON(t, ok(bq.handleListPayments(ctx, bunqLiveRequest(map[string]any{"account_id": accountID, "count": 10}))))
	t.Logf("payments: %v", payments["count"])
	if payments["count"].(float64) < 1 {
		t.Fatal("no payment after the sugardaddy request")
	}
	first := payments["payments"].([]any)[0].(map[string]any)
	if first["amount"] != "100.00" {
		t.Fatalf("first payment amount %v", first["amount"])
	}
	one := bunqLiveJSON(t, ok(bq.handleGetPayment(ctx, bunqLiveRequest(map[string]any{"account_id": accountID, "payment_id": int(first["id"].(float64))}))))
	if one["id"].(float64) != first["id"].(float64) {
		t.Fatal("get_payment returned another payment")
	}
	since := bunqLiveJSON(t, ok(bq.handleListPayments(ctx, bunqLiveRequest(map[string]any{"account_id": accountID, "since": time.Now().UTC().Format("2006-01-02")}))))
	if since["since_reached"] != true {
		t.Fatalf("since not reached: %v", since)
	}

	events := bunqLiveJSON(t, ok(bq.handleListEvents(ctx, bunqLiveRequest(map[string]any{"account_id": accountID, "count": 10}))))
	t.Logf("events: %v", events["count"])

	draft := bunqLiveJSON(t, ok(bq.handleCreateDraft(ctx, bunqLiveRequest(map[string]any{
		"account_id": accountID, "amount": "1.23", "counterparty_iban": iban, "counterparty_name": "Live Test", "description": "mux live test draft",
	}))))
	t.Logf("draft %v", draft["draft_id"])
	drafts := bunqLiveJSON(t, ok(bq.handleListDrafts(ctx, bunqLiveRequest(map[string]any{"account_id": accountID}))))
	if drafts["count"].(float64) < 1 {
		t.Fatal("draft not listed")
	}
	d := drafts["draft_payments"].([]any)[0].(map[string]any)
	if d["status"] != "PENDING" {
		t.Fatalf("draft status %v", d["status"])
	}
	// What the OpenAPI document does not pin, this test does: the listing
	// carries entries with amount and counterparty, and no acceptance count.
	if _, ok := d["required_accepts"]; ok {
		t.Fatal("bunq's draft listing now reports number_of_required_accepts — update the shape")
	}
	if e := d["entries"].([]any)[0].(map[string]any); e["amount"] != "1.23" || e["counterparty"].(map[string]any)["iban"] != iban {
		t.Fatalf("draft entry %v", e)
	}
	// The acceptance never leaves this connection, whatever the caller asks.
	for _, bad := range []map[string]any{
		{"method": "PUT", "path": fmt.Sprintf("/v1/user/{user}/monetary-account/%d/draft-payment/%v", accountID, draft["draft_id"]), "body": `{"status":"ACCEPTED"}`},
		{"method": "POST", "path": fmt.Sprintf("/v1/user/{user}/monetary-account/%d/draft-payment", accountID), "body": `{"entries":[],"number_of_required_accepts":0}`},
	} {
		if res, err := bq.handleAPIRequest(ctx, bunqLiveRequest(bad)); err != nil || !res.IsError {
			t.Fatalf("api_request did not refuse %v", bad)
		}
	}

	today := time.Now().UTC().Format("2006-01-02")
	statement := bunqLiveText(t, ok(bq.handleExportStatement(ctx, bunqLiveRequest(map[string]any{"account_id": accountID, "date_start": today, "date_end": today}))))
	if !strings.Contains(statement, "\"Date\"") {
		t.Fatalf("statement is not bunq's CSV: %.200s", statement)
	}
	// The read of a statement answers under CustomerStatement (the document
	// calls the schema ExportStatementRead); the handler accepts both, this
	// line notices when bunq changes the wrapper.
	if !strings.Contains(statement, "statement ") {
		t.Fatalf("statement note missing: %.200s", statement)
	}
	t.Logf("statement: %d bytes", len(statement))

	user := bunqLiveText(t, ok(bq.handleAPIGet(ctx, bunqLiveRequest(map[string]any{"path": "/v1/user/{user}"}))))
	if !strings.Contains(user, "UserPerson") {
		t.Fatalf("api_get user: %.200s", user)
	}
	refused, err := bq.handleAPIRequest(ctx, bunqLiveRequest(map[string]any{"method": "POST", "path": fmt.Sprintf("/v1/user/{user}/monetary-account/%d/payment", accountID), "body": "{}"}))
	if err != nil || !refused.IsError {
		t.Fatal("api_request did not refuse a payment")
	}

	// The refusal texts the renewal relies on, as the live API words them.
	resp, err = bq.do(ctx, http.MethodGet, fmt.Sprintf("/v1/user/%d/monetary-account", info.UserID), nil, "bogus-session-token", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	bogus, err := bunqReadEnvelope(resp)
	if err == nil || bogus != nil || !bunqSessionLost(err) {
		t.Fatalf("a bogus session token must read as a lost session, got %v", err)
	}
	t.Logf("bogus session token: %v", err)

	// Lose the session on purpose: the next call must sign in again without
	// a new installation.
	bq.mu.Lock()
	bq.api.SessionToken = "stale"
	bq.mu.Unlock()
	if _, err := bq.Verify(ctx); err != nil {
		t.Fatalf("verify after stale session: %v", err)
	}
	if bq.installs != 1 {
		t.Fatalf("stale session caused %d installations", bq.installs)
	}

	// A second connection on the same secrets (a restart) reuses installation
	// and session.
	again, err := NewBunq(conn, nil)
	if err != nil {
		t.Fatal(err)
	}
	again.secrets = secrets
	if _, err := again.Verify(ctx); err != nil {
		t.Fatalf("verify on restart: %v", err)
	}
	if again.installs != 0 {
		t.Fatalf("restart registered %d installations", again.installs)
	}
	var buf bytes.Buffer
	json.Indent(&buf, []byte(secrets.data["bunq-live-api-context"]), "", " ")
	if !strings.Contains(buf.String(), `"device_id"`) {
		t.Fatalf("context without device: %s", buf.String())
	}
}
