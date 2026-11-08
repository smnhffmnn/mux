package tools

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/smnhffmnn/mux/internal/config"
)

// A fake bunq: the way in (installation, device, session) with the headers
// and signatures the real API checks, and enough of the account, payment,
// event, draft and statement endpoints for the handlers to exercise their
// shaping and paging. Shapes follow what the sandbox returned while the
// type was built; the people and amounts are made up.

const (
	fakeBunqAPIKey = "sandbox_test_key"
	fakeBunqUserID = 42
)

type bunqFake struct {
	mu            sync.Mutex
	srv           *httptest.Server
	pub           *rsa.PublicKey // registered by the installation
	installations int
	devices       int
	sessions      int
	installToken  string
	sessionToken  string
	timeout       int64 // session_timeout reported by the user object

	expireOnce      bool // answer the next session call with 403 once
	statementPolls  int  // reads of a statement that report PENDING before COMPLETED
	paymentRequests int  // payment list requests served so far
	paymentsBreakAt int  // answer payment list requests beyond this count with 429 (0: never)
	requests        []bunqFakeRequest
}

type bunqFakeRequest struct {
	Method  string
	Path    string
	Query   map[string]string
	Headers http.Header
	Body    string
}

func newBunqFake(t *testing.T) *bunqFake {
	t.Helper()
	f := &bunqFake{timeout: 604800}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *bunqFake) recorded(method, suffix string) []bunqFakeRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []bunqFakeRequest
	for _, r := range f.requests {
		if (method == "" || r.Method == method) && strings.HasSuffix(r.Path, suffix) {
			out = append(out, r)
		}
	}
	return out
}

func bunqFakeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func bunqFakeError(w http.ResponseWriter, status int, msg string) {
	bunqFakeJSON(w, status, map[string]any{"Error": []map[string]string{{"error_description": msg, "error_description_translated": msg}}})
}

func bunqFakeResponse(items ...map[string]any) map[string]any {
	return map[string]any{"Response": items}
}

func bunqFakeCreated(daysAgo int) string {
	return time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC).AddDate(0, 0, -daysAgo).Format("2006-01-02 15:04:05.000000")
}

func (f *bunqFake) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	q := map[string]string{}
	for k, v := range r.URL.Query() {
		q[k] = v[0]
	}
	f.mu.Lock()
	f.requests = append(f.requests, bunqFakeRequest{Method: r.Method, Path: r.URL.Path, Query: q, Headers: r.Header.Clone(), Body: string(body)})
	f.mu.Unlock()

	for _, h := range []string{"Cache-Control", "User-Agent", "X-Bunq-Client-Request-Id", "X-Bunq-Geolocation"} {
		if r.Header.Get(h) == "" {
			bunqFakeError(w, 400, "missing header "+h)
			return
		}
	}
	path := strings.TrimPrefix(r.URL.Path, "/v1")

	if path == "/installation" && r.Method == http.MethodPost {
		var in struct {
			Key string `json:"client_public_key"`
		}
		if err := json.Unmarshal(body, &in); err != nil || in.Key == "" {
			bunqFakeError(w, 400, "client_public_key missing")
			return
		}
		block, _ := pem.Decode([]byte(in.Key))
		if block == nil {
			bunqFakeError(w, 400, "client_public_key is not PEM")
			return
		}
		pk, err := x509.ParsePKIXPublicKey(block.Bytes)
		if err != nil {
			bunqFakeError(w, 400, "client_public_key does not parse")
			return
		}
		f.mu.Lock()
		f.pub = pk.(*rsa.PublicKey)
		f.installations++
		f.installToken = fmt.Sprintf("inst-%d", f.installations)
		f.sessionToken = ""
		tok := f.installToken
		f.mu.Unlock()
		bunqFakeJSON(w, 200, bunqFakeResponse(
			map[string]any{"Id": map[string]any{"id": 1000 + f.installations}},
			map[string]any{"Token": map[string]any{"id": 1, "token": tok}},
			map[string]any{"ServerPublicKey": map[string]any{"server_public_key": "-----BEGIN PUBLIC KEY-----\nfake\n-----END PUBLIC KEY-----\n"}},
		))
		return
	}

	// Everything else is signed when it has a body, with the installation's key.
	if len(body) > 0 {
		f.mu.Lock()
		pub := f.pub
		f.mu.Unlock()
		sig, err := base64.StdEncoding.DecodeString(r.Header.Get("X-Bunq-Client-Signature"))
		if pub == nil || err != nil {
			bunqFakeError(w, 400, "The request signature is invalid.")
			return
		}
		sum := sha256.Sum256(body)
		if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, sum[:], sig); err != nil {
			bunqFakeError(w, 400, "The request signature is invalid.")
			return
		}
	}
	auth := r.Header.Get("X-Bunq-Client-Authentication")

	switch {
	case path == "/device-server" && r.Method == http.MethodPost:
		var in struct {
			Description string `json:"description"`
			Secret      string `json:"secret"`
		}
		_ = json.Unmarshal(body, &in)
		f.mu.Lock()
		okTok := auth == f.installToken
		f.mu.Unlock()
		if !okTok {
			bunqFakeError(w, 403, "Insufficient authentication.")
			return
		}
		if in.Secret != fakeBunqAPIKey || in.Description == "" {
			bunqFakeError(w, 400, "Invalid api key.")
			return
		}
		f.mu.Lock()
		f.devices++
		n := f.devices
		f.mu.Unlock()
		bunqFakeJSON(w, 200, bunqFakeResponse(map[string]any{"Id": map[string]any{"id": 5000 + n}}))
		return
	case path == "/session-server" && r.Method == http.MethodPost:
		f.mu.Lock()
		okTok := auth == f.installToken
		f.mu.Unlock()
		if !okTok {
			bunqFakeError(w, 403, "Insufficient authentication.")
			return
		}
		f.mu.Lock()
		f.sessions++
		f.sessionToken = fmt.Sprintf("sess-%d", f.sessions)
		tok, timeout := f.sessionToken, f.timeout
		f.mu.Unlock()
		bunqFakeJSON(w, 200, bunqFakeResponse(
			map[string]any{"Id": map[string]any{"id": 2}},
			map[string]any{"Token": map[string]any{"id": 2, "token": tok}},
			map[string]any{"UserPerson": map[string]any{"id": fakeBunqUserID, "display_name": "J. Doe", "session_timeout": timeout, "status": "ACTIVE"}},
		))
		return
	}

	f.mu.Lock()
	valid := auth != "" && auth == f.sessionToken
	expire := f.expireOnce
	if expire {
		f.expireOnce = false
	}
	f.mu.Unlock()
	if !valid || expire {
		bunqFakeError(w, 403, "Insufficient authentication.")
		return
	}

	userPrefix := fmt.Sprintf("/user/%d", fakeBunqUserID)
	if !strings.HasPrefix(path, userPrefix) {
		bunqFakeError(w, 404, "not found")
		return
	}
	rest := strings.TrimPrefix(path, userPrefix)
	switch {
	case rest == "" && r.Method == http.MethodGet:
		bunqFakeJSON(w, 200, bunqFakeResponse(map[string]any{"UserPerson": map[string]any{"id": fakeBunqUserID, "display_name": "J. Doe"}}))
	case rest == "/monetary-account" && r.Method == http.MethodGet:
		bunqFakeJSON(w, 200, bunqFakeResponse(
			map[string]any{"MonetaryAccountBank": map[string]any{
				"id": 1, "created": bunqFakeCreated(400), "updated": bunqFakeCreated(1), "description": "Main", "status": "ACTIVE", "sub_status": "NONE",
				"currency": "EUR", "balance": map[string]string{"currency": "EUR", "value": "1234.56"},
				"alias":   []map[string]string{{"type": "EMAIL", "value": "j@example.com", "name": "J. Doe"}, {"type": "IBAN", "value": "NL02BUNQ0000000001", "name": "J. Doe"}},
				"user_id": fakeBunqUserID, "overdraft_limit": map[string]string{"currency": "EUR", "value": "0.00"},
			}},
			map[string]any{"MonetaryAccountSavings": map[string]any{
				"id": 2, "created": bunqFakeCreated(300), "description": "Old savings", "status": "CANCELLED", "sub_status": "REDEMPTION_VOLUNTARY",
				"currency": "EUR", "balance": map[string]string{"currency": "EUR", "value": "0.00"},
				"alias": []map[string]string{{"type": "IBAN", "value": "NL02BUNQ0000000002", "name": "J. Doe"}},
			}},
		))
	case rest == "/forbidden" && r.Method == http.MethodGet:
		bunqFakeError(w, 403, "Forbidden.") // a refusal that is not about the session
	case rest == "/monetary-account/1/payment" && r.Method == http.MethodGet:
		f.mu.Lock()
		f.paymentRequests++
		limited := f.paymentsBreakAt > 0 && f.paymentRequests > f.paymentsBreakAt
		f.mu.Unlock()
		if limited {
			bunqFakeError(w, 429, "Too many requests. You can do a maximum of 3 calls per 3 second to this endpoint.")
			return
		}
		f.servePayments(w, q)
	case strings.HasPrefix(rest, "/monetary-account/1/payment/") && r.Method == http.MethodGet:
		id, _ := strconv.Atoi(strings.TrimPrefix(rest, "/monetary-account/1/payment/"))
		if id < 1 || id > 30 {
			bunqFakeError(w, 404, "Payment not found.")
			return
		}
		bunqFakeJSON(w, 200, bunqFakeResponse(map[string]any{"Payment": bunqFakePayment(id)}))
	case rest == "/event" && r.Method == http.MethodGet:
		bunqFakeJSON(w, 200, map[string]any{
			"Response": []map[string]any{
				{"Event": map[string]any{"id": 900, "created": bunqFakeCreated(0), "action": "CREATE", "status": "FINALIZED", "user_id": nil, "monetary_account_id": "1",
					"object": map[string]any{"Payment": bunqFakePayment(30)}}},
				{"Event": map[string]any{"id": 899, "created": bunqFakeCreated(1), "action": "CREATE", "status": "AWAITING_REPLY", "user_id": nil, "monetary_account_id": "1",
					"object": map[string]any{"DraftPayment": map[string]any{"id": 77, "status": "PENDING", "entries": []any{}}}}},
			},
			"Pagination": map[string]any{"older_url": "/v1/user/42/event?count=50&older_id=899", "newer_url": nil, "future_url": "/v1/user/42/event?count=50&newer_id=900"},
		})
	case rest == "/monetary-account/1/draft-payment" && r.Method == http.MethodGet:
		bunqFakeJSON(w, 200, bunqFakeResponse(map[string]any{"DraftPayment": map[string]any{
			"id": 77, "created": bunqFakeCreated(1), "updated": bunqFakeCreated(1), "monetary_account_id": 1, "status": "PENDING",
			"entries": []map[string]any{{"id": 1, "amount": map[string]string{"currency": "EUR", "value": "12.34"}, "description": "Rent",
				"counterparty_alias": map[string]any{"iban": "NL91ABNA0417164300", "display_name": "Landlord", "country": "NL"}}},
		}}))
	case rest == "/monetary-account/1/draft-payment" && r.Method == http.MethodPost:
		bunqFakeJSON(w, 200, bunqFakeResponse(map[string]any{"Id": map[string]any{"id": 78}}))
	case rest == "/monetary-account/1/customer-statement" && r.Method == http.MethodPost:
		bunqFakeJSON(w, 200, bunqFakeResponse(map[string]any{"Id": map[string]any{"id": 9}}))
	case rest == "/monetary-account/1/customer-statement/9" && r.Method == http.MethodGet:
		f.mu.Lock()
		status := "COMPLETED"
		if f.statementPolls > 0 {
			f.statementPolls--
			status = "PENDING"
		}
		f.mu.Unlock()
		bunqFakeJSON(w, 200, bunqFakeResponse(map[string]any{"CustomerStatement": map[string]any{"id": 9, "created": bunqFakeCreated(0), "status": status, "date_start": "2026-10-01", "date_end": "2026-10-31", "statement_format": "CSV", "regional_format": "EUROPEAN"}}))
	case rest == "/monetary-account/1/customer-statement/9/content" && r.Method == http.MethodGet:
		w.Header().Set("Content-Type", "text/csv;charset=UTF-8")
		w.WriteHeader(200)
		_, _ = w.Write([]byte("\"Date\";\"Interest Date\";\"Amount\";\"Account\";\"Counterparty\";\"Name\";\"Description\"\n\"2026-10-02\";\"2026-10-02\";\"-12,34\";\"NL02BUNQ0000000001\";\"NL91ABNA0417164300\";\"Landlord\";\"Rent\"\n"))
	case strings.HasPrefix(rest, "/monetary-account/1/draft-payment/") && r.Method == http.MethodPut:
		// Must never be reached: bunq would accept the draft and move the money.
		bunqFakeJSON(w, 200, bunqFakeResponse(map[string]any{"Id": map[string]any{"id": 77}}))
	case strings.HasSuffix(rest, "/payment") && r.Method == http.MethodPost:
		// Must never be reached: the type has no payment endpoint and the
		// pass-through refuses the path before a request goes out.
		bunqFakeJSON(w, 200, bunqFakeResponse(map[string]any{"Id": map[string]any{"id": 666}}))
	default:
		bunqFakeError(w, 404, "not found")
	}
}

func bunqFakePayment(id int) map[string]any {
	amount := fmt.Sprintf("%d.%02d", id, id)
	if id%3 == 0 {
		amount = "-" + amount
	}
	return map[string]any{
		"id": id, "created": bunqFakeCreated(31 - id), "updated": bunqFakeCreated(31 - id), "monetary_account_id": 1,
		"amount": map[string]string{"currency": "EUR", "value": amount}, "description": fmt.Sprintf("Payment %d", id), "type": "IDEAL", "sub_type": "PAYMENT",
		"alias":                  map[string]any{"iban": "NL02BUNQ0000000001", "display_name": "J. Doe", "country": "NL", "avatar": map[string]any{"uuid": "x"}},
		"counterparty_alias":     map[string]any{"iban": "NL91ABNA0417164300", "display_name": "Landlord", "country": "NL", "avatar": map[string]any{"uuid": "y"}},
		"balance_after_mutation": map[string]string{"currency": "EUR", "value": "1000.00"},
		"payment_fee":            nil,
	}
}

// servePayments pages the thirty payments 30…1 the way bunq does: newest
// first, count per page, older_id/newer_id as exclusive bounds.
func (f *bunqFake) servePayments(w http.ResponseWriter, q map[string]string) {
	count, _ := strconv.Atoi(q["count"])
	if count <= 0 {
		count = 10
	}
	hi, lo := 30, 1
	if v, ok := q["older_id"]; ok {
		n, _ := strconv.Atoi(v)
		hi = n - 1
	}
	if v, ok := q["newer_id"]; ok {
		n, _ := strconv.Atoi(v)
		lo = n + 1
	}
	var items []map[string]any
	last := 0
	for id := hi; id >= lo && len(items) < count; id-- {
		items = append(items, map[string]any{"Payment": bunqFakePayment(id)})
		last = id
	}
	pag := map[string]any{"older_url": nil, "newer_url": nil, "future_url": nil}
	if last > 1 {
		pag["older_url"] = fmt.Sprintf("/v1/user/42/monetary-account/1/payment?count=%d&older_id=%d", count, last)
	}
	if len(items) > 0 && hi < 30 {
		pag["newer_url"] = fmt.Sprintf("/v1/user/42/monetary-account/1/payment?count=%d&newer_id=%d", count, hi)
	}
	bunqFakeJSON(w, 200, map[string]any{"Response": items, "Pagination": pag})
}

// --- Helpers ---

type bunqMemSecrets struct {
	mu   sync.Mutex
	data map[string]string
}

func (m *bunqMemSecrets) Get(key string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.data[key]
	if !ok {
		return "", fmt.Errorf("no secret %s", key)
	}
	return v, nil
}

func (m *bunqMemSecrets) Set(key, value string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.data == nil {
		m.data = map[string]string{}
	}
	m.data[key] = value
	return nil
}

func newTestBunq(t *testing.T, f *bunqFake, secrets *bunqMemSecrets, readOnly bool) *Bunq {
	t.Helper()
	// The config wants an https host; the fake listens on plain loopback,
	// so the host is swapped after construction (the sandbox URL keeps the
	// sandbox flag out of the way of the tests that look at it).
	conn := config.Connection{Name: "bank", Type: config.TypeBunq, URL: config.BunqProductionURL, Token: fakeBunqAPIKey, ReadOnly: readOnly}
	b, err := NewBunq(conn, nil)
	if err != nil {
		t.Fatal(err)
	}
	b.baseURL = f.srv.URL
	b.secrets = secrets
	return b
}

func bunqReq(args map[string]any) mcp.CallToolRequest {
	var req mcp.CallToolRequest
	req.Params.Arguments = args
	return req
}

// bunqCall pairs a handler's result with its error so a call can be passed
// to the helpers in one expression: bunqResultJSON(t, bc(b.handleX(…))).
type bunqCall struct {
	res *mcp.CallToolResult
	err error
}

func bc(res *mcp.CallToolResult, err error) bunqCall { return bunqCall{res, err} }

func bunqResultText(t *testing.T, c bunqCall) (string, bool) {
	t.Helper()
	res, err := c.res, c.err
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Content) == 0 {
		t.Fatal("empty result")
	}
	tc, ok := res.Content[0].(mcp.TextContent)
	if !ok {
		t.Fatalf("not text: %T", res.Content[0])
	}
	return tc.Text, res.IsError
}

func bunqResultJSON(t *testing.T, c bunqCall) map[string]any {
	t.Helper()
	text, isErr := bunqResultText(t, c)
	if isErr {
		t.Fatalf("tool error: %s", text)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, text)
	}
	return out
}

// --- Tests ---

func TestBunq_WayIn(t *testing.T) {
	f := newBunqFake(t)
	secrets := &bunqMemSecrets{}
	b := newTestBunq(t, f, secrets, false)
	ctx := context.Background()

	info, err := b.Verify(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if info.UserID != fakeBunqUserID || info.UserType != "UserPerson" || info.DisplayName != "J. Doe" || info.Accounts != 2 || info.Active != 1 {
		t.Fatalf("verify info %+v", info)
	}
	if f.installations != 1 || f.devices != 1 || f.sessions != 1 {
		t.Fatalf("installations %d devices %d sessions %d", f.installations, f.devices, f.sessions)
	}
	key, err := secrets.Get("bank-private-key")
	if err != nil || !strings.Contains(key, "PRIVATE KEY") {
		t.Fatal("private key not stored")
	}
	if _, err := bunqParseKey(key); err != nil {
		t.Fatalf("stored key does not parse: %v", err)
	}
	raw, err := secrets.Get("bank-api-context")
	if err != nil {
		t.Fatal("api context not stored")
	}
	var c bunqAPIContext
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		t.Fatal(err)
	}
	if c.Host != f.srv.URL || c.InstallationToken != "inst-1" || c.DeviceID != 5001 || c.SessionToken != "sess-1" || c.UserID != fakeBunqUserID || c.SessionExpires.IsZero() {
		t.Fatalf("context %+v", c)
	}
	if want := time.Now().Add(604800 * time.Second); c.SessionExpires.Sub(want) > time.Minute || want.Sub(c.SessionExpires) > time.Minute {
		t.Fatalf("session expiry %s, want about %s", c.SessionExpires, want)
	}
	// The device registration carried the API key and a description, signed.
	dev := f.recorded(http.MethodPost, "/device-server")
	if len(dev) != 1 || !strings.Contains(dev[0].Body, `"secret":"`+fakeBunqAPIKey+`"`) || !strings.Contains(dev[0].Body, `"description":"mux bank"`) {
		t.Fatalf("device registration %+v", dev)
	}
	if dev[0].Headers.Get("X-Bunq-Client-Authentication") != "inst-1" || dev[0].Headers.Get("X-Bunq-Client-Signature") == "" {
		t.Fatalf("device headers %v", dev[0].Headers)
	}
	// Reads carry the session token and no signature.
	acc := f.recorded(http.MethodGet, "/monetary-account")
	if len(acc) != 1 || acc[0].Headers.Get("X-Bunq-Client-Authentication") != "sess-1" || acc[0].Headers.Get("X-Bunq-Client-Signature") != "" {
		t.Fatalf("account read %+v", acc)
	}
}

func TestBunq_RestartReusesContext(t *testing.T) {
	f := newBunqFake(t)
	secrets := &bunqMemSecrets{}
	b := newTestBunq(t, f, secrets, false)
	if _, err := b.Verify(context.Background()); err != nil {
		t.Fatal(err)
	}
	again := newTestBunq(t, f, secrets, false)
	if _, err := again.Verify(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.installations != 1 || f.devices != 1 || f.sessions != 1 {
		t.Fatalf("restart: installations %d devices %d sessions %d", f.installations, f.devices, f.sessions)
	}
	if again.installs != 0 {
		t.Fatal("restart registered an installation")
	}
}

func TestBunq_ContextOfOtherHostIsDropped(t *testing.T) {
	f := newBunqFake(t)
	secrets := &bunqMemSecrets{}
	b := newTestBunq(t, f, secrets, false)
	if _, err := b.Verify(context.Background()); err != nil {
		t.Fatal(err)
	}
	raw, _ := secrets.Get("bank-api-context")
	var c bunqAPIContext
	_ = json.Unmarshal([]byte(raw), &c)
	c.Host = config.BunqProductionURL
	data, _ := json.Marshal(c)
	_ = secrets.Set("bank-api-context", string(data))

	again := newTestBunq(t, f, secrets, false)
	if _, err := again.Verify(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.installations != 2 || again.installs != 1 {
		t.Fatalf("a context of another host must not be used: installations %d", f.installations)
	}
}

func TestBunq_SessionRenewal(t *testing.T) {
	f := newBunqFake(t)
	secrets := &bunqMemSecrets{}
	b := newTestBunq(t, f, secrets, false)
	ctx := context.Background()
	if _, err := b.Verify(ctx); err != nil {
		t.Fatal(err)
	}
	// bunq refuses the session once: the call signs in again and repeats,
	// without a new installation.
	f.mu.Lock()
	f.expireOnce = true
	f.mu.Unlock()
	out := bunqResultJSON(t, bc(b.handleListAccounts(ctx, bunqReq(nil))))
	if len(out["accounts"].([]any)) != 1 {
		t.Fatalf("accounts after renewal: %v", out)
	}
	if f.sessions != 2 || f.installations != 1 {
		t.Fatalf("renewal: sessions %d installations %d", f.sessions, f.installations)
	}
	// A session near its expiry is renewed before the call.
	b.now = func() time.Time { return time.Now().Add(604800*time.Second - time.Minute) }
	bunqResultJSON(t, bc(b.handleListAccounts(ctx, bunqReq(nil))))
	if f.sessions != 3 {
		t.Fatalf("proactive renewal: sessions %d", f.sessions)
	}
	// The raw path renews a lost session the same way …
	b.now = time.Now
	f.mu.Lock()
	f.expireOnce = true
	f.mu.Unlock()
	res, err := b.handleAPIGet(ctx, bunqReq(map[string]any{"path": "/v1/user/{user}/monetary-account/1/payment/5"}))
	text, _ := bunqResultText(t, bc(res, err))
	if !strings.Contains(text, `"id": 5`) && !strings.Contains(text, `"id":5`) {
		t.Fatalf("api_get after renewal: %s", text)
	}
	if f.sessions != 4 {
		t.Fatalf("api_get renewal: sessions %d", f.sessions)
	}
	// … and passes a refusal that is not about the session through, once,
	// without signing in again.
	res, err = b.handleAPIGet(ctx, bunqReq(map[string]any{"path": "/v1/user/{user}/forbidden"}))
	if text, isErr := bunqResultText(t, bc(res, err)); !isErr || !strings.Contains(text, "Forbidden.") {
		t.Fatalf("forbidden pass-through: %s", text)
	}
	if f.sessions != 4 || len(f.recorded(http.MethodGet, "/forbidden")) != 1 {
		t.Fatalf("a plain 403 caused a renewal: sessions %d", f.sessions)
	}
}

func TestBunq_FailedDeviceKeepsInstallation(t *testing.T) {
	f := newBunqFake(t)
	secrets := &bunqMemSecrets{}
	b := newTestBunq(t, f, secrets, false)
	b.apiKey = "wrong"
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, err := b.Verify(ctx); err == nil || !strings.Contains(err.Error(), "device registration") {
			t.Fatalf("verify with a wrong key: %v", err)
		}
	}
	if f.installations != 1 || f.devices != 0 {
		t.Fatalf("a refused device must not cost installations: installations %d devices %d", f.installations, f.devices)
	}
	raw, err := secrets.Get("bank-api-context")
	if err != nil || !strings.Contains(raw, `"installation_token":"inst-1"`) {
		t.Fatalf("installation not persisted: %v %s", err, raw)
	}
	// With the right key the stored installation is used.
	b.apiKey = fakeBunqAPIKey
	if _, err := b.Verify(ctx); err != nil {
		t.Fatal(err)
	}
	if f.installations != 1 || f.devices != 1 || f.sessions != 1 {
		t.Fatalf("after the fix: installations %d devices %d sessions %d", f.installations, f.devices, f.sessions)
	}
}

func TestBunq_APIKeyChangeRegistersNewDevice(t *testing.T) {
	f := newBunqFake(t)
	secrets := &bunqMemSecrets{}
	b := newTestBunq(t, f, secrets, false)
	if _, err := b.Verify(context.Background()); err != nil {
		t.Fatal(err)
	}
	// The fake accepts one API key only; a second instance with another key
	// must not ride on the first key's device and session.
	other := newTestBunq(t, f, secrets, false)
	other.apiKey = "rotated"
	_, err := other.Verify(context.Background())
	if err == nil || !strings.Contains(err.Error(), "device registration") {
		t.Fatalf("rotated key reused the old device or session: %v", err)
	}
	if f.installations != 1 || f.sessions != 1 {
		t.Fatalf("rotation: installations %d sessions %d", f.installations, f.sessions)
	}
	devs := f.recorded(http.MethodPost, "/device-server")
	if len(devs) != 2 || !strings.Contains(devs[1].Body, `"secret":"rotated"`) {
		t.Fatalf("device registrations %+v", devs)
	}
}

func TestBunq_UnknownInstallationIsRegisteredAgain(t *testing.T) {
	f := newBunqFake(t)
	secrets := &bunqMemSecrets{}
	b := newTestBunq(t, f, secrets, false)
	ctx := context.Background()
	if _, err := b.Verify(ctx); err != nil {
		t.Fatal(err)
	}
	// bunq forgets the installation (the fake answers its token with 403
	// "Insufficient authentication.", as the sandbox does): the next sign-in
	// registers once more instead of staying stuck.
	f.mu.Lock()
	f.installToken = "revoked"
	f.sessionToken = ""
	f.mu.Unlock()
	b.mu.Lock()
	b.api.SessionToken = ""
	b.mu.Unlock()
	if _, err := b.Verify(ctx); err != nil {
		t.Fatalf("verify after a revoked installation: %v", err)
	}
	if f.installations != 2 || f.devices != 2 || f.sessions != 2 {
		t.Fatalf("re-registration: installations %d devices %d sessions %d", f.installations, f.devices, f.sessions)
	}
	// An IP refusal is not that case: reported, not re-registered.
	if bunqInstallationLost(&bunqHTTPError{Status: 400, Errors: []bunqAPIError{{Description: "User credentials are incorrect. Incorrect API key or IP address."}}}) {
		t.Fatal("an IP refusal must not trigger a new installation")
	}
	if !bunqInstallationLost(&bunqHTTPError{Status: 403, Errors: []bunqAPIError{{Description: "Insufficient authentication."}}}) {
		t.Fatal("an unknown installation token must trigger a new installation")
	}
	if bunqIPRefusal(&bunqHTTPError{Status: 400, Errors: []bunqAPIError{{Description: "Invalid description for recipient."}}}) {
		t.Fatal("\"recipient\" is not an IP refusal")
	}
}

func TestBunq_ShortSessionTimeout(t *testing.T) {
	f := newBunqFake(t)
	f.mu.Lock()
	f.timeout = 120 // two minutes: shorter than the renewal slack
	f.mu.Unlock()
	b := newTestBunq(t, f, &bunqMemSecrets{}, false)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		bunqResultJSON(t, bc(b.handleListAccounts(ctx, bunqReq(nil))))
	}
	if f.sessions != 1 {
		t.Fatalf("a short session timeout made every call a login: sessions %d", f.sessions)
	}
}

func TestBunq_ListAccounts(t *testing.T) {
	f := newBunqFake(t)
	b := newTestBunq(t, f, &bunqMemSecrets{}, false)
	ctx := context.Background()
	out := bunqResultJSON(t, bc(b.handleListAccounts(ctx, bunqReq(nil))))
	accounts := out["accounts"].([]any)
	if len(accounts) != 1 || out["not_active"] != float64(1) {
		t.Fatalf("accounts %v", out)
	}
	a := accounts[0].(map[string]any)
	if a["type"] != "bank" || a["iban"] != "NL02BUNQ0000000001" || a["balance"] != "1234.56" || a["description"] != "Main" {
		t.Fatalf("account %v", a)
	}
	if _, ok := a["overdraft_limit"]; ok {
		t.Fatal("zero overdraft limit shown")
	}
	user := out["user"].(map[string]any)
	if user["id"] != float64(fakeBunqUserID) || user["display_name"] != "J. Doe" {
		t.Fatalf("user %v", user)
	}
	all := bunqResultJSON(t, bc(b.handleListAccounts(ctx, bunqReq(map[string]any{"include_closed": true}))))
	if len(all["accounts"].([]any)) != 2 {
		t.Fatalf("include_closed %v", all)
	}
	if all["accounts"].([]any)[1].(map[string]any)["type"] != "savings" {
		t.Fatalf("savings type %v", all)
	}
}

func TestBunq_ListPaymentsPaging(t *testing.T) {
	f := newBunqFake(t)
	b := newTestBunq(t, f, &bunqMemSecrets{}, false)
	ctx := context.Background()
	defer bunqNoSincePause()()

	page := bunqResultJSON(t, bc(b.handleListPayments(ctx, bunqReq(map[string]any{"account_id": 1, "count": 10}))))
	payments := page["payments"].([]any)
	if len(payments) != 10 || page["older_id"] != "21" {
		t.Fatalf("first page %v", page)
	}
	first := payments[0].(map[string]any)
	if first["id"] != float64(30) || first["amount"] != "-30.30" || first["counterparty"].(map[string]any)["iban"] != "NL91ABNA0417164300" || first["balance_after"] != "1000.00" {
		t.Fatalf("first payment %v", first)
	}
	if _, ok := page["newer_id"]; ok {
		t.Fatal("first page has a newer_id")
	}
	next := bunqResultJSON(t, bc(b.handleListPayments(ctx, bunqReq(map[string]any{"account_id": 1, "count": 10, "older_id": "21"}))))
	if next["payments"].([]any)[0].(map[string]any)["id"] != float64(20) || next["older_id"] != "11" || next["newer_id"] != "20" {
		t.Fatalf("second page %v", next)
	}
	last := bunqResultJSON(t, bc(b.handleListPayments(ctx, bunqReq(map[string]any{"account_id": 1, "count": 10, "older_id": "11"}))))
	if _, ok := last["older_id"]; ok {
		t.Fatalf("last page has an older_id: %v", last)
	}

	// since: payment id n was created 31-n days before 2026-11-08 (30 is the
	// newest); the day of id 6 — 25 days back — stops the paging inside the
	// third page with ids 30…6.
	since := bunqResultJSON(t, bc(b.handleListPayments(ctx, bunqReq(map[string]any{"account_id": 1, "count": 10, "since": bunqFakeCreated(25)[:10]}))))
	if since["count"] != float64(25) || since["since_reached"] != true || since["pages"] != float64(3) {
		t.Fatalf("since %v", since)
	}
	// A page that bunq refuses mid-walk (its rate limit) does not throw the
	// pages before it away: they come back with older_id and a note.
	f.mu.Lock()
	f.paymentRequests, f.paymentsBreakAt = 0, 2
	f.mu.Unlock()
	part := bunqResultJSON(t, bc(b.handleListPayments(ctx, bunqReq(map[string]any{"account_id": 1, "count": 10, "since": "2026-01-01"}))))
	if part["count"] != float64(20) || part["older_id"] != "11" || part["since_reached"] != false || !strings.Contains(part["note"].(string), "429") {
		t.Fatalf("partial since %v", part)
	}
	f.mu.Lock()
	f.paymentsBreakAt = 0
	f.mu.Unlock()
	if _, err := b.handleListPayments(ctx, bunqReq(map[string]any{"account_id": 1, "since": "yesterday"})); err != nil {
		t.Fatal(err)
	}
	res, _ := b.handleListPayments(ctx, bunqReq(map[string]any{"account_id": 1, "older_id": "5", "newer_id": "2"}))
	if !res.IsError {
		t.Fatal("older_id with newer_id accepted")
	}
	res, _ = b.handleListPayments(ctx, bunqReq(nil))
	if text, isErr := bunqResultText(t, bc(res, nil)); !isErr || !strings.Contains(text, "account_id") {
		t.Fatalf("missing account_id: %s", text)
	}
	// count is capped at bunq's page size.
	bunqResultJSON(t, bc(b.handleListPayments(ctx, bunqReq(map[string]any{"account_id": 1, "count": 999}))))
	reqs := f.recorded(http.MethodGet, "/payment")
	if got := reqs[len(reqs)-1].Query["count"]; got != "200" {
		t.Fatalf("count sent %s", got)
	}
}

func TestBunq_GetPaymentDropsAvatars(t *testing.T) {
	f := newBunqFake(t)
	b := newTestBunq(t, f, &bunqMemSecrets{}, false)
	out := bunqResultJSON(t, bc(b.handleGetPayment(context.Background(), bunqReq(map[string]any{"account_id": 1, "payment_id": 7}))))
	if out["id"] != float64(7) {
		t.Fatalf("payment %v", out)
	}
	if _, ok := out["alias"].(map[string]any)["avatar"]; ok {
		t.Fatal("avatar kept")
	}
	res, _ := b.handleGetPayment(context.Background(), bunqReq(map[string]any{"account_id": 1, "payment_id": 99}))
	if text, isErr := bunqResultText(t, bc(res, nil)); !isErr || !strings.Contains(text, "404") {
		t.Fatalf("missing payment: %s", text)
	}
}

func TestBunq_ListEvents(t *testing.T) {
	f := newBunqFake(t)
	b := newTestBunq(t, f, &bunqMemSecrets{}, false)
	out := bunqResultJSON(t, bc(b.handleListEvents(context.Background(), bunqReq(map[string]any{"account_id": 1, "status": "finalized"}))))
	events := out["events"].([]any)
	if len(events) != 2 || out["older_id"] != "899" || out["newer_id"] != "900" {
		t.Fatalf("events %v", out)
	}
	e0 := events[0].(map[string]any)
	if e0["account_id"] != float64(1) || e0["object_type"] != "Payment" || e0["object"].(map[string]any)["amount"] != "-30.30" {
		t.Fatalf("payment event %v", e0)
	}
	e1 := events[1].(map[string]any)
	if e1["object_type"] != "DraftPayment" || e1["object"].(map[string]any)["status"] != "PENDING" {
		t.Fatalf("draft event %v", e1)
	}
	reqs := f.recorded(http.MethodGet, "/event")
	if reqs[0].Query["monetary_account_id"] != "1" || reqs[0].Query["status"] != "FINALIZED" {
		t.Fatalf("event query %v", reqs[0].Query)
	}
}

func TestBunq_Drafts(t *testing.T) {
	f := newBunqFake(t)
	b := newTestBunq(t, f, &bunqMemSecrets{}, false)
	ctx := context.Background()
	list := bunqResultJSON(t, bc(b.handleListDrafts(ctx, bunqReq(map[string]any{"account_id": 1}))))
	d := list["draft_payments"].([]any)[0].(map[string]any)
	if d["status"] != "PENDING" || d["entries"].([]any)[0].(map[string]any)["counterparty"].(map[string]any)["name"] != "Landlord" {
		t.Fatalf("drafts %v", list)
	}
	if _, ok := d["required_accepts"]; ok {
		t.Fatal("required_accepts invented for a listing that does not carry it")
	}

	for _, bad := range []map[string]any{
		{"account_id": 1, "amount": "12", "counterparty_iban": "NL91ABNA0417164300", "counterparty_name": "L", "description": "d"},
		{"account_id": 1, "amount": "0.00", "counterparty_iban": "NL91ABNA0417164300", "counterparty_name": "L", "description": "d"},
		{"account_id": 1, "amount": "-5.00", "counterparty_iban": "NL91ABNA0417164300", "counterparty_name": "L", "description": "d"},
		{"account_id": 1, "amount": "5.00", "counterparty_iban": "not an iban", "counterparty_name": "L", "description": "d"},
		{"account_id": 1, "amount": "5.00", "counterparty_iban": "NL91ABNA0417164300", "counterparty_name": "", "description": "d"},
		{"account_id": 1, "amount": "5.00", "counterparty_iban": "NL91ABNA0417164300", "counterparty_name": "L", "description": strings.Repeat("x", 141)},
		{"account_id": 1, "amount": "5.00", "counterparty_iban": "NL91ABNA0417164300", "counterparty_name": "L", "description": "d", "currency": "euro"},
	} {
		res, err := b.handleCreateDraft(ctx, bunqReq(bad))
		if err != nil || !res.IsError {
			t.Fatalf("accepted %v", bad)
		}
	}
	if len(f.recorded(http.MethodPost, "/draft-payment")) != 0 {
		t.Fatal("a refused draft reached the server")
	}

	out := bunqResultJSON(t, bc(b.handleCreateDraft(ctx, bunqReq(map[string]any{
		"account_id": 1, "amount": "12.34", "counterparty_iban": "nl91 abna 0417 1643 00", "counterparty_name": "Landlord", "description": "Rent November",
	}))))
	if out["draft_id"] != float64(78) || out["status"] != "PENDING" || out["counterparty"].(map[string]any)["iban"] != "NL91ABNA0417164300" {
		t.Fatalf("draft result %v", out)
	}
	posts := f.recorded(http.MethodPost, "/draft-payment")
	if len(posts) != 1 {
		t.Fatalf("%d draft posts", len(posts))
	}
	var body struct {
		Entries []struct {
			Amount            bunqAmount  `json:"amount"`
			CounterpartyAlias bunqPointer `json:"counterparty_alias"`
			Description       string      `json:"description"`
		} `json:"entries"`
		Accepts int `json:"number_of_required_accepts"`
	}
	if err := json.Unmarshal([]byte(posts[0].Body), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Entries) != 1 || body.Entries[0].Amount != (bunqAmount{Value: "12.34", Currency: "EUR"}) || body.Entries[0].CounterpartyAlias != (bunqPointer{Type: "IBAN", Value: "NL91ABNA0417164300", Name: "Landlord"}) || body.Accepts != 1 {
		t.Fatalf("draft body %s", posts[0].Body)
	}
	if posts[0].Headers.Get("X-Bunq-Client-Signature") == "" {
		t.Fatal("draft not signed")
	}
	if posts[0].Headers.Get(bunqIdempotencyKey) == "" {
		t.Fatal("draft without idempotency key")
	}
}

func TestBunq_ReadOnlyHasNoWriteTools(t *testing.T) {
	f := newBunqFake(t)
	ro := newTestBunq(t, f, &bunqMemSecrets{}, true)
	rw := newTestBunq(t, f, &bunqMemSecrets{}, false)
	names := func(b *Bunq) map[string]bool {
		out := map[string]bool{}
		for _, td := range b.Tools() {
			out[td.Tool.Name] = true
		}
		return out
	}
	if n := names(ro); n["create_draft_payment"] || n["api_request"] || !n["api_get"] || !n["list_payments"] {
		t.Fatalf("read-only tools %v", n)
	}
	if n := names(rw); !n["create_draft_payment"] || !n["api_request"] {
		t.Fatalf("read-write tools %v", n)
	}
	cond := bunqConditionalToolNames("bank")
	want := map[string]bool{"bank_create_draft_payment": true, "bank_api_request": true}
	if len(cond) != 2 || !want[cond[0]] || !want[cond[1]] {
		t.Fatalf("conditional tools %v", cond)
	}
	if ins := BunqInstructions(config.Connection{Type: config.TypeBunq, ReadOnly: true}); !strings.Contains(ins, "Read-only") || strings.Contains(ins, "create_draft_payment") {
		t.Fatalf("read-only instructions: %s", ins)
	}
	if ins := BunqInstructions(config.Connection{Type: config.TypeBunq, URL: config.BunqSandboxURL}); !strings.Contains(ins, "sandbox") || !strings.Contains(ins, "create_draft_payment") {
		t.Fatalf("sandbox instructions: %s", ins)
	}
}

func TestBunq_ExportStatement(t *testing.T) {
	f := newBunqFake(t)
	b := newTestBunq(t, f, &bunqMemSecrets{}, false)
	ctx := context.Background()
	f.mu.Lock()
	f.statementPolls = 2
	f.mu.Unlock()
	// Polling waits between reads; the test's clock does not.
	wait := bunqStatementPollFor(t)
	defer wait()

	res, err := b.handleExportStatement(ctx, bunqReq(map[string]any{"account_id": 1, "date_start": "2026-10-01", "date_end": "2026-10-31"}))
	text, isErr := bunqResultText(t, bc(res, err))
	if isErr || !strings.Contains(text, "\"Rent\"") || !strings.Contains(text, "statement 9") {
		t.Fatalf("statement: %s", text)
	}
	if reads := f.recorded(http.MethodGet, "/customer-statement/9"); len(reads) != 3 {
		t.Fatalf("%d statement reads, want 3 (two PENDING, one COMPLETED)", len(reads))
	}
	posts := f.recorded(http.MethodPost, "/customer-statement")
	if len(posts) != 1 || !strings.Contains(posts[0].Body, `"statement_format":"CSV"`) || !strings.Contains(posts[0].Body, `"regional_format":"EUROPEAN"`) {
		t.Fatalf("statement request %+v", posts)
	}

	// To a file.
	path := filepath.Join(t.TempDir(), "statement.csv")
	res, err = b.handleExportStatement(ctx, bunqReq(map[string]any{"account_id": 1, "date_start": "2026-10-01", "date_end": "2026-10-31", "output_file": path}))
	if text, isErr := bunqResultText(t, bc(res, err)); isErr {
		t.Fatalf("statement to file: %s", text)
	}
	data, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(data), "Landlord") {
		t.Fatalf("file: %v %s", err, data)
	}

	for _, bad := range []map[string]any{
		{"account_id": 1, "date_start": "2026-10-31", "date_end": "2026-10-01"},
		{"account_id": 1, "date_start": "01.10.2026", "date_end": "2026-10-31"},
		{"account_id": 1, "date_start": "2026-10-01", "date_end": "2026-10-31", "format": "XLSX"},
	} {
		res, err := b.handleExportStatement(ctx, bunqReq(bad))
		if err != nil || !res.IsError {
			t.Fatalf("accepted %v", bad)
		}
	}
	// A PDF without a destination and without a blob store is refused
	// before anything is requested.
	before := len(f.recorded(http.MethodPost, "/customer-statement"))
	res, _ = b.handleExportStatement(ctx, bunqReq(map[string]any{"account_id": 1, "date_start": "2026-10-01", "date_end": "2026-10-31", "format": "PDF"}))
	if text, isErr := bunqResultText(t, bc(res, nil)); !isErr || !strings.Contains(text, "output_file") {
		t.Fatalf("pdf without output_file: %s", text)
	}
	if len(f.recorded(http.MethodPost, "/customer-statement")) != before {
		t.Fatal("a refused PDF export reached the server")
	}
}

// bunqStatementPollFor shortens the poll interval for a test and restores it.
func bunqStatementPollFor(t *testing.T) func() {
	t.Helper()
	old := bunqStatementPollInterval
	bunqStatementPollInterval = time.Millisecond
	return func() { bunqStatementPollInterval = old }
}

// bunqNoSincePause drops the pause between since-pages for a test.
func bunqNoSincePause() func() {
	old := bunqSincePause
	bunqSincePause = 0
	return func() { bunqSincePause = old }
}

func TestBunq_ExportStatementOnReadOnly(t *testing.T) {
	f := newBunqFake(t)
	ro := newTestBunq(t, f, &bunqMemSecrets{}, true)
	defer bunqStatementPollFor(t)()
	res, err := ro.handleExportStatement(context.Background(), bunqReq(map[string]any{"account_id": 1, "date_start": "2026-10-01", "date_end": "2026-10-31"}))
	if text, isErr := bunqResultText(t, bc(res, err)); isErr || !strings.Contains(text, "Landlord") {
		t.Fatalf("read-only export: %s", text)
	}
}

func TestBunq_PassThrough(t *testing.T) {
	f := newBunqFake(t)
	b := newTestBunq(t, f, &bunqMemSecrets{}, false)
	ctx := context.Background()

	for _, bad := range []string{"", "user/42", "/v2/user/42", "/v1/user/%7Buser%7D", "/v1/user/../x", "/v1/user//x", "/v1/user/{other}", "/v1/user/42#frag"} {
		if _, err := bunqAPIPath(bad); err == nil {
			t.Errorf("bunqAPIPath(%q) accepted", bad)
		}
	}
	if p, err := bunqAPIPath("/v1/user/{user}/event?count=5&status=FINALIZED"); err != nil || p != "/v1/user/{user}/event?count=5&status=FINALIZED" {
		t.Fatalf("bunqAPIPath: %q %v", p, err)
	}

	text, isErr := bunqResultText(t, bc(b.handleAPIGet(ctx, bunqReq(map[string]any{"path": "/v1/user/{user}"}))))
	if isErr || !strings.HasPrefix(text, "HTTP 200") || !strings.Contains(text, "UserPerson") {
		t.Fatalf("api_get: %s", text)
	}
	if reqs := f.recorded(http.MethodGet, "/user/42"); len(reqs) != 1 {
		t.Fatalf("{user} not replaced: %v", f.requests)
	}

	// Money-moving paths are refused before a request goes out.
	for _, p := range []string{
		"/v1/user/{user}/monetary-account/1/payment",
		"/v1/user/42/monetary-account/1/request-inquiry",
		"/v1/user/42/monetary-account/1/payment/5/note-text",
		"/v1/user/42/card/1",
		"/v1/user/42",
	} {
		res, err := b.handleAPIRequest(ctx, bunqReq(map[string]any{"method": "POST", "path": p, "body": "{}"}))
		if text, isErr := bunqResultText(t, bc(res, err)); !isErr || !strings.Contains(text, "refused") {
			t.Fatalf("api_request %s: %s", p, text)
		}
	}
	if len(f.recorded(http.MethodPost, "/payment")) != 0 || len(f.recorded(http.MethodPost, "/request-inquiry")) != 0 {
		t.Fatal("a refused write reached the server")
	}
	res, err := b.handleAPIRequest(ctx, bunqReq(map[string]any{"method": "PATCH", "path": "/v1/user/42/monetary-account/1/draft-payment", "body": "{}"}))
	if text, isErr := bunqResultText(t, bc(res, err)); !isErr || !strings.Contains(text, "method") {
		t.Fatalf("PATCH: %s", text)
	}
	res, err = b.handleAPIRequest(ctx, bunqReq(map[string]any{"method": "POST", "path": "/v1/user/42/monetary-account/1/draft-payment", "body": "{not json"}))
	if text, isErr := bunqResultText(t, bc(res, err)); !isErr || !strings.Contains(text, "JSON") {
		t.Fatalf("bad body: %s", text)
	}

	// A draft is created through the pass-through, never accepted, changed
	// or filed without a required acceptance.
	for _, bad := range []map[string]any{
		{"method": "PUT", "path": "/v1/user/{user}/monetary-account/1/draft-payment/77", "body": `{"status":"ACCEPTED"}`},
		{"method": "PUT", "path": "/v1/user/{user}/monetary-account/1/draft-payment/77", "body": `{"status":"CANCELLED"}`},
		{"method": "DELETE", "path": "/v1/user/{user}/monetary-account/1/draft-payment/77"},
		{"method": "POST", "path": "/v1/user/{user}/monetary-account/1/draft-payment/77", "body": `{"entries":[],"number_of_required_accepts":1}`},
		{"method": "POST", "path": "/v1/user/{user}/monetary-account/1/draft-payment", "body": `{"entries":[]}`},
		{"method": "POST", "path": "/v1/user/{user}/monetary-account/1/draft-payment", "body": `{"entries":[],"number_of_required_accepts":0}`},
		{"method": "POST", "path": "/v1/user/{user}/monetary-account/1/draft-payment", "body": `{"entries":[],"number_of_required_accepts":1,"status":"ACCEPTED"}`},
		{"method": "POST", "path": "/v1/user/{user}/monetary-account/1/draft-payment", "body": `[1]`},
	} {
		res, err := b.handleAPIRequest(ctx, bunqReq(bad))
		if text, isErr := bunqResultText(t, bc(res, err)); !isErr || !strings.Contains(text, "draft") {
			t.Fatalf("api_request %v accepted: %s", bad, text)
		}
	}
	if len(f.recorded(http.MethodPut, "/draft-payment/77")) != 0 || len(f.recorded(http.MethodPost, "/draft-payment")) != 0 {
		t.Fatal("a refused draft write reached the server")
	}
	text, isErr = bunqResultText(t, bc(b.handleAPIRequest(ctx, bunqReq(map[string]any{"method": "POST", "path": "/v1/user/{user}/monetary-account/1/draft-payment", "body": `{"entries":[],"number_of_required_accepts":1}`}))))
	if isErr || !strings.Contains(text, `"id":78`) && !strings.Contains(text, `"id": 78`) {
		t.Fatalf("api_request draft: %s", text)
	}
	posts := f.recorded(http.MethodPost, "/draft-payment")
	if len(posts) != 1 || posts[0].Headers.Get("X-Bunq-Client-Signature") == "" || posts[0].Body != `{"entries":[],"number_of_required_accepts":1}` {
		t.Fatalf("pass-through draft %+v", posts)
	}
	// Notes and exports keep their methods.
	text, isErr = bunqResultText(t, bc(b.handleAPIRequest(ctx, bunqReq(map[string]any{"method": "DELETE", "path": "/v1/user/{user}/monetary-account/1/customer-statement/9"}))))
	if isErr && !strings.Contains(text, "404") { // the fake has no DELETE; the point is that it went out
		t.Fatalf("statement delete: %s", text)
	}
}

func TestBunq_StoredKeyIsUsed(t *testing.T) {
	f := newBunqFake(t)
	secrets := &bunqMemSecrets{}
	first := newTestBunq(t, f, secrets, false)
	if _, err := first.Verify(context.Background()); err != nil {
		t.Fatal(err)
	}
	key, _ := secrets.Get("bank-private-key")
	// A connection that carries the key in its config (provisioning, the
	// desktop form) and starts without a stored context registers a new
	// installation with that same key.
	other := &bunqMemSecrets{}
	conn := config.Connection{Name: "bank", Type: config.TypeBunq, URL: config.BunqProductionURL, Token: fakeBunqAPIKey, PrivateKey: key}
	b, err := NewBunq(conn, nil)
	if err != nil {
		t.Fatal(err)
	}
	b.baseURL = f.srv.URL
	b.secrets = other
	if _, err := b.Verify(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := other.Get("bank-private-key"); err == nil {
		t.Fatal("a configured key was stored again")
	}
	if f.installations != 2 {
		t.Fatalf("installations %d", f.installations)
	}
	fp1 := func(pemText string) string {
		k, _ := bunqParseKey(pemText)
		_, fp, _ := bunqPublicKeyPEM(k)
		return fp
	}
	raw, _ := other.Get("bank-api-context")
	var c bunqAPIContext
	_ = json.Unmarshal([]byte(raw), &c)
	if c.KeyFingerprint != fp1(key) {
		t.Fatal("context fingerprint does not match the configured key")
	}
	// A key that does not parse is reported, not replaced.
	broken := &bunqMemSecrets{data: map[string]string{"bank-private-key": "-----BEGIN PRIVATE KEY-----\nnope\n-----END PRIVATE KEY-----\n"}}
	bb := newTestBunq(t, f, broken, false)
	if _, err := bb.Verify(context.Background()); err == nil || !strings.Contains(err.Error(), "does not parse") {
		t.Fatalf("broken key: %v", err)
	}
}

func TestBunq_NewBunqValidation(t *testing.T) {
	if _, err := NewBunq(config.Connection{Name: "b", Type: config.TypeBunq}, nil); err == nil {
		t.Fatal("no token accepted")
	}
	if _, err := NewBunq(config.Connection{Name: "b", Type: config.TypeBunq, Token: "k", URL: "http://api.bunq.com"}, nil); err == nil {
		t.Fatal("http URL accepted")
	}
	if _, err := NewBunq(config.Connection{Name: "b", Type: config.TypeBunq, Token: "k", URL: "https://api.bunq.com/v1"}, nil); err == nil {
		t.Fatal("URL with path accepted")
	}
	b, err := NewBunq(config.Connection{Name: "b", Type: config.TypeBunq, Token: "k"}, nil)
	if err != nil || b.baseURL != config.BunqProductionURL || b.sandbox {
		t.Fatalf("defaults: %v %+v", err, b)
	}
	b, _ = NewBunq(config.Connection{Name: "b", Type: config.TypeBunq, Token: "k", URL: config.BunqSandboxURL + "/"}, nil)
	if !b.sandbox || b.baseURL != config.BunqSandboxURL {
		t.Fatalf("sandbox: %+v", b)
	}
}

func TestBunq_EnvelopeHelpers(t *testing.T) {
	var env bunqEnvelope
	if err := json.Unmarshal([]byte(`{"Response":[{"Id":{"id":1}},{"Token":{"token":"t"}},{"UserApiKey":{"id":7,"granted_by_user":{"UserPerson":{"id":8,"display_name":"Owner","session_timeout":3600}}}}],"Pagination":{"older_url":"/v1/user/1/event?count=5&older_id=99","newer_url":null}}`), &env); err != nil {
		t.Fatal(err)
	}
	typ, u := env.user()
	if typ != "UserApiKey" || u.ID != 7 || u.DisplayName != "Owner" || u.SessionTimeout != 3600 {
		t.Fatalf("user %s %+v", typ, u)
	}
	if env.Pagination.olderID() != "99" || env.Pagination.newerID() != "" {
		t.Fatalf("pagination %q %q", env.Pagination.olderID(), env.Pagination.newerID())
	}
	he := &bunqHTTPError{Status: 429, Errors: []bunqAPIError{{Description: "Too many requests."}}}
	if !strings.Contains(he.Error(), "rate limit") || !strings.Contains(he.Error(), "Too many requests.") {
		t.Fatalf("error text %s", he.Error())
	}
	if !bunqSessionLost(&bunqHTTPError{Status: 403, Errors: []bunqAPIError{{Description: "Insufficient authentication."}}}) || bunqSessionLost(&bunqHTTPError{Status: 403, Errors: []bunqAPIError{{Description: "Forbidden."}}}) {
		t.Fatal("bunqSessionLost")
	}
	if bunqInstallationLost(&bunqHTTPError{Status: 400, Errors: []bunqAPIError{{Description: "The request originates from an IP address that is not whitelisted."}}}) {
		t.Fatal("an IP refusal must not trigger a new installation")
	}
}
