package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/smnhffmnn/mux/internal/config"
)

// The tools of the bunq type and the shapes they return. Reading tools
// first, then — on connections that are not read-only — the one write that
// moves money (as a draft the owner accepts in the app) and the confined
// pass-through.

// Tools returns the MCP tools of the connection.
func (b *Bunq) Tools() []ToolDef {
	tools := []ToolDef{
		{
			Tool: mcp.NewTool("list_accounts",
				mcp.WithDescription("List the accounts the API key sees — id, type (bank, savings, joint, …), description, IBAN, balance, currency, status — and the user they belong to. Every other tool takes account_id from here."),
				mcp.WithBoolean("include_closed", mcp.Description("Also list accounts that are not ACTIVE (default: false)")),
				mcp.WithReadOnlyHintAnnotation(true),
			),
			Handler: b.handleListAccounts,
		},
		{
			Tool: mcp.NewTool("list_payments",
				mcp.WithDescription("Payments (transactions) of an account, newest first: id, created, amount, description, counterparty (IBAN, name), type, balance after. Pages by id: the result's older_id continues towards the past, newer_id towards the present. since stops paging at a date (several pages, at most 20, about one per second because of bunq's rate limit; a page that fails returns what was collected plus older_id)."),
				mcp.WithNumber("account_id", mcp.Required(), mcp.Description("Account id from list_accounts")),
				mcp.WithNumber("count", mcp.Description("Payments per page (default 50, max 200)")),
				mcp.WithString("older_id", mcp.Description("Continue towards the past from this id (the older_id of the previous result)")),
				mcp.WithString("newer_id", mcp.Description("Continue towards the present from this id (the newer_id of the previous result)")),
				mcp.WithString("since", mcp.Description("YYYY-MM-DD: page back until payments older than this day (max 20 pages of count, paced to bunq's limit); the result says whether the day was reached and where to continue")),
				mcp.WithReadOnlyHintAnnotation(true),
			),
			Handler: b.handleListPayments,
		},
		{
			Tool: mcp.NewTool("get_payment",
				mcp.WithDescription("One payment in full as bunq returns it (counterparty, addresses, geolocation, batch and schedule references, fee)."),
				mcp.WithNumber("account_id", mcp.Required(), mcp.Description("Account id")),
				mcp.WithNumber("payment_id", mcp.Required(), mcp.Description("Payment id from list_payments")),
				mcp.WithReadOnlyHintAnnotation(true),
			),
			Handler: b.handleGetPayment,
		},
		{
			Tool: mcp.NewTool("list_events",
				mcp.WithDescription("The user's event timeline, newest first: payments, requests, card transactions, draft payments and more, each with the object it concerns. Pages by id like list_payments."),
				mcp.WithNumber("account_id", mcp.Description("Only events of this account (default: all accounts)")),
				mcp.WithNumber("count", mcp.Description("Events per page (default 50, max 200)")),
				mcp.WithString("older_id", mcp.Description("Continue towards the past from this id")),
				mcp.WithString("newer_id", mcp.Description("Continue towards the present from this id")),
				mcp.WithString("status", mcp.Description("Only events with this status, e.g. FINALIZED or AWAITING_REPLY")),
				mcp.WithReadOnlyHintAnnotation(true),
			),
			Handler: b.handleListEvents,
		},
		{
			Tool: mcp.NewTool("list_draft_payments",
				mcp.WithDescription("Draft payments of an account — the transfers waiting for the owner's acceptance in the bunq app, and the accepted or rejected ones — with their entries (amount, counterparty, description) and status."),
				mcp.WithNumber("account_id", mcp.Required(), mcp.Description("Account id")),
				mcp.WithNumber("count", mcp.Description("Drafts per page (default 50, max 200)")),
				mcp.WithString("older_id", mcp.Description("Continue towards the past from this id")),
				mcp.WithString("newer_id", mcp.Description("Continue towards the present from this id")),
				mcp.WithReadOnlyHintAnnotation(true),
			),
			Handler: b.handleListDrafts,
		},
		{
			Tool: mcp.NewTool("export_statement",
				mcp.WithDescription("bunq's own account statement for a date range as CSV (default), MT940 or PDF. CSV and MT940 come back inline up to 1 MiB unless output_file is set; a PDF needs output_file (\"blob\" or an absolute path). bunq generates the statement on request and keeps a record of it — the tool waits up to 30 s for it."),
				mcp.WithNumber("account_id", mcp.Required(), mcp.Description("Account id")),
				mcp.WithString("date_start", mcp.Required(), mcp.Description("First day, YYYY-MM-DD")),
				mcp.WithString("date_end", mcp.Required(), mcp.Description("Last day, YYYY-MM-DD")),
				mcp.WithString("format", mcp.Description("CSV (default), MT940 or PDF"), mcp.Enum("CSV", "MT940", "PDF")),
				mcp.WithString("regional_format", mcp.Description("Number and date format of a CSV: EUROPEAN (default; semicolons, comma decimals) or UK_US"), mcp.Enum("EUROPEAN", "UK_US")),
				mcp.WithString("output_file", mcp.Description("Save the statement instead of returning it inline: \"blob\" for a blob you fetch from mux, or an absolute path (supports ~) where you and mux share a disk")),
				// No read-only hint: bunq keeps a record of every export.
			),
			Handler: b.handleExportStatement,
		},
		{
			Tool: mcp.NewTool("api_get",
				mcp.WithDescription("Raw GET against the bunq API — any /v1/… path including the query string, under the session of the API key (cards, request inquiries, scheduled payments, the user object, …). {user} in the path stands for the user id. The JSON comes back as bunq sends it. Paths are written plain (no percent-encoding, no . or .. segments)."),
				mcp.WithString("path", mcp.Required(), mcp.Description("Path starting with /v1/, {user} allowed for the user id, query string allowed")),
				mcp.WithString("output_file", mcp.Description("Save the response instead of returning it inline: \"blob\" or an absolute path")),
				mcp.WithReadOnlyHintAnnotation(true),
			),
			Handler: b.handleAPIGet,
		},
	}
	if b.readOnly {
		return tools
	}
	return append(tools,
		ToolDef{
			Tool: mcp.NewTool("create_draft_payment",
				mcp.WithDescription("File a draft payment: a transfer from an account to an IBAN that the account's owner has to accept in the bunq app before any money moves. Returns the draft id; list_draft_payments shows its status (PENDING until accepted or rejected). Nothing is transferred by this call."),
				mcp.WithNumber("account_id", mcp.Required(), mcp.Description("Account to pay from")),
				mcp.WithString("amount", mcp.Required(), mcp.Description("Amount with two decimals, e.g. \"12.34\"")),
				mcp.WithString("counterparty_iban", mcp.Required(), mcp.Description("IBAN to pay to")),
				mcp.WithString("counterparty_name", mcp.Required(), mcp.Description("Name of the account holder")),
				mcp.WithString("description", mcp.Required(), mcp.Description("Payment description (reference), up to 140 characters")),
				mcp.WithString("currency", mcp.Description("ISO currency (default EUR)")),
			),
			Handler: b.handleCreateDraft,
		},
		ToolDef{
			Tool: mcp.NewTool("api_request",
				mcp.WithDescription("Raw POST, PUT or DELETE against the bunq API with a JSON body, signed with the connection's key. Confined to what a draft-only connection may write: POST a draft-payment (number_of_required_accepts 1, no status — a draft is never accepted, changed or cancelled from here), notes (note-text, note-attachment) and exports (customer-statement, export-annual-overview, export-rib). Payments, requests, cards, scheduled payments and everything else that moves money or changes the account are refused — a draft the owner accepts in the app is the way. {user} stands for the user id."),
				mcp.WithString("method", mcp.Required(), mcp.Description("POST, PUT or DELETE"), mcp.Enum("POST", "PUT", "DELETE")),
				mcp.WithString("path", mcp.Required(), mcp.Description("Path starting with /v1/, {user} allowed for the user id")),
				mcp.WithString("body", mcp.Description("JSON body")),
				mcp.WithDestructiveHintAnnotation(true),
			),
			Handler: b.handleAPIRequest,
		},
	)
}

// bunqConditionalToolNames are the tools whose presence depends on the
// configuration (read_only); they are removed before every registration so
// a reload that switches read_only on leaves none behind.
func bunqConditionalToolNames(connName string) []string {
	always := map[string]bool{}
	for _, td := range (&Bunq{readOnly: true}).Tools() {
		always[td.Tool.Name] = true
	}
	var names []string
	for _, td := range (&Bunq{}).Tools() {
		if !always[td.Tool.Name] {
			names = append(names, config.SanitizeToolName(connName+"_"+td.Tool.Name))
		}
	}
	return names
}

// --- Data models (what the API returns) ---

type bunqAmount struct {
	Value    string `json:"value"`
	Currency string `json:"currency"`
}

type bunqPointer struct {
	Type  string `json:"type"`
	Value string `json:"value"`
	Name  string `json:"name,omitempty"`
}

// bunqLabel is the LabelMonetaryAccount bunq attaches to both sides of a
// payment: the IBAN and the name as the counterparty showed them.
type bunqLabel struct {
	IBAN        string `json:"iban"`
	DisplayName string `json:"display_name"`
	Country     string `json:"country"`
	LabelUser   *struct {
		DisplayName string `json:"display_name"`
	} `json:"label_user"`
}

func (l *bunqLabel) shaped() map[string]any {
	if l == nil {
		return nil
	}
	out := map[string]any{}
	if l.IBAN != "" {
		out["iban"] = l.IBAN
	}
	name := l.DisplayName
	if name == "" && l.LabelUser != nil {
		name = l.LabelUser.DisplayName
	}
	if name != "" {
		out["name"] = name
	}
	if l.Country != "" {
		out["country"] = l.Country
	}
	return out
}

type bunqAccount struct {
	ID             int64         `json:"id"`
	Created        string        `json:"created"`
	Updated        string        `json:"updated"`
	Description    string        `json:"description"`
	Status         string        `json:"status"`
	SubStatus      string        `json:"sub_status"`
	Currency       string        `json:"currency"`
	Balance        bunqAmount    `json:"balance"`
	Alias          []bunqPointer `json:"alias"`
	DisplayName    string        `json:"display_name"`
	UserID         int64         `json:"user_id"`
	OverdraftLimit *bunqAmount   `json:"overdraft_limit"`

	Type string `json:"-"` // from the wrapper key: MonetaryAccountBank → bank
}

func (a *bunqAccount) shaped() map[string]any {
	out := map[string]any{
		"id":          a.ID,
		"type":        a.Type,
		"description": a.Description,
		"status":      a.Status,
		"currency":    a.Currency,
		"balance":     a.Balance.Value,
		"created":     a.Created,
	}
	if a.SubStatus != "" && a.SubStatus != "NONE" {
		out["sub_status"] = a.SubStatus
	}
	if a.DisplayName != "" {
		out["display_name"] = a.DisplayName
	}
	for _, p := range a.Alias {
		if p.Type == "IBAN" {
			out["iban"] = p.Value
			if p.Name != "" {
				out["iban_name"] = p.Name
			}
			break
		}
	}
	if a.OverdraftLimit != nil && a.OverdraftLimit.Value != "" && a.OverdraftLimit.Value != "0.00" {
		out["overdraft_limit"] = a.OverdraftLimit.Value
	}
	return out
}

// bunqAccountType maps the wrapper key of a monetary account to a short
// type name.
func bunqAccountType(wrapper string) string {
	t := strings.TrimPrefix(wrapper, "MonetaryAccount")
	if t == "" || t == wrapper {
		return strings.ToLower(wrapper)
	}
	return strings.ToLower(t)
}

type bunqPayment struct {
	ID                   int64       `json:"id"`
	Created              string      `json:"created"`
	Updated              string      `json:"updated"`
	MonetaryAccountID    int64       `json:"monetary_account_id"`
	Amount               bunqAmount  `json:"amount"`
	Alias                *bunqLabel  `json:"alias"`
	CounterpartyAlias    *bunqLabel  `json:"counterparty_alias"`
	Description          string      `json:"description"`
	Type                 string      `json:"type"`
	SubType              string      `json:"sub_type"`
	MerchantReference    string      `json:"merchant_reference"`
	BatchID              *int64      `json:"batch_id"`
	ScheduledID          *int64      `json:"scheduled_id"`
	BalanceAfterMutation *bunqAmount `json:"balance_after_mutation"`
	PaymentFee           *bunqAmount `json:"payment_fee"`
}

func (p *bunqPayment) shaped() map[string]any {
	out := map[string]any{
		"id":           p.ID,
		"created":      p.Created,
		"amount":       p.Amount.Value,
		"currency":     p.Amount.Currency,
		"description":  p.Description,
		"type":         p.Type,
		"counterparty": p.CounterpartyAlias.shaped(),
	}
	if p.SubType != "" {
		out["sub_type"] = p.SubType
	}
	if p.BalanceAfterMutation != nil && p.BalanceAfterMutation.Value != "" {
		out["balance_after"] = p.BalanceAfterMutation.Value
	}
	if p.MerchantReference != "" {
		out["merchant_reference"] = p.MerchantReference
	}
	if p.BatchID != nil {
		out["batch_id"] = *p.BatchID
	}
	if p.ScheduledID != nil {
		out["scheduled_id"] = *p.ScheduledID
	}
	if p.PaymentFee != nil && p.PaymentFee.Value != "" && p.PaymentFee.Value != "0.00" {
		out["fee"] = p.PaymentFee.Value
	}
	return out
}

// bunqFlexInt reads an integer bunq sends as number or as string (the
// event's monetary_account_id arrives quoted).
type bunqFlexInt int64

func (f *bunqFlexInt) UnmarshalJSON(data []byte) error {
	s := strings.Trim(strings.TrimSpace(string(data)), `"`)
	if s == "" || s == "null" {
		*f = 0
		return nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return err
	}
	*f = bunqFlexInt(n)
	return nil
}

type bunqEvent struct {
	ID                int64                      `json:"id"`
	Created           string                     `json:"created"`
	Updated           string                     `json:"updated"`
	Action            string                     `json:"action"`
	UserID            bunqFlexInt                `json:"user_id"`
	MonetaryAccountID bunqFlexInt                `json:"monetary_account_id"`
	Status            string                     `json:"status"`
	Object            map[string]json.RawMessage `json:"object"`
}

// shaped reduces an event to what a timeline needs: the kind of object it
// concerns and that object's id, amount, description and status where it
// has them. A Payment object is shaped like list_payments.
func (e *bunqEvent) shaped() map[string]any {
	out := map[string]any{
		"id":      e.ID,
		"created": e.Created,
		"action":  e.Action,
		"status":  e.Status,
	}
	if e.MonetaryAccountID != 0 {
		out["account_id"] = int64(e.MonetaryAccountID)
	}
	for typ, raw := range e.Object {
		out["object_type"] = typ
		if typ == "Payment" {
			var p bunqPayment
			if json.Unmarshal(raw, &p) == nil {
				out["object"] = p.shaped()
				break
			}
		}
		var generic struct {
			ID          *int64      `json:"id"`
			Description string      `json:"description"`
			Status      string      `json:"status"`
			Amount      *bunqAmount `json:"amount"`
			AmountInq   *bunqAmount `json:"amount_inquired"`
		}
		if json.Unmarshal(raw, &generic) == nil {
			obj := map[string]any{}
			if generic.ID != nil {
				obj["id"] = *generic.ID
			}
			if generic.Description != "" {
				obj["description"] = generic.Description
			}
			if generic.Status != "" {
				obj["status"] = generic.Status
			}
			switch {
			case generic.Amount != nil && generic.Amount.Value != "":
				obj["amount"] = generic.Amount.Value
				obj["currency"] = generic.Amount.Currency
			case generic.AmountInq != nil && generic.AmountInq.Value != "":
				obj["amount"] = generic.AmountInq.Value
				obj["currency"] = generic.AmountInq.Currency
			}
			out["object"] = obj
		}
		break
	}
	return out
}

type bunqDraftEntry struct {
	ID                *int64     `json:"id,omitempty"`
	Amount            bunqAmount `json:"amount"`
	CounterpartyAlias *bunqLabel `json:"counterparty_alias"`
	Description       string     `json:"description"`
	Type              string     `json:"type,omitempty"`
}

type bunqDraft struct {
	ID                      int64            `json:"id"`
	Created                 string           `json:"created"`
	Updated                 string           `json:"updated"`
	MonetaryAccountID       int64            `json:"monetary_account_id"`
	Status                  string           `json:"status"`
	Entries                 []bunqDraftEntry `json:"entries"`
	NumberOfRequiredAccepts int              `json:"number_of_required_accepts"`
}

func (d *bunqDraft) shaped() map[string]any {
	entries := make([]map[string]any, 0, len(d.Entries))
	for _, e := range d.Entries {
		entry := map[string]any{
			"amount":       e.Amount.Value,
			"currency":     e.Amount.Currency,
			"description":  e.Description,
			"counterparty": e.CounterpartyAlias.shaped(),
		}
		if e.ID != nil {
			entry["id"] = *e.ID
		}
		entries = append(entries, entry)
	}
	out := map[string]any{
		"id":      d.ID,
		"created": d.Created,
		"updated": d.Updated,
		"status":  d.Status,
		"entries": entries,
	}
	if d.NumberOfRequiredAccepts > 0 {
		// Sent on creation; bunq's listing does not report it.
		out["required_accepts"] = d.NumberOfRequiredAccepts
	}
	return out
}

type bunqStatement struct {
	ID              int64  `json:"id"`
	Created         string `json:"created"`
	Status          string `json:"status"`
	DateStart       string `json:"date_start"`
	DateEnd         string `json:"date_end"`
	StatementFormat string `json:"statement_format"`
	RegionalFormat  string `json:"regional_format"`
}

// bunqUserInfo is the user part of list_accounts: who the key belongs to.
type bunqUserInfo struct {
	ID          int64
	Type        string
	DisplayName string
}

// --- Reading ---

// listAccounts lists the user's monetary accounts of every kind and returns
// the session's user with them.
func (b *Bunq) listAccounts(ctx context.Context) ([]bunqAccount, bunqUserInfo, error) {
	q := url.Values{}
	q.Set("count", fmt.Sprint(bunqMaxCount))
	env, err := b.call(ctx, "accounts", nil, q, nil)
	if err != nil {
		return nil, bunqUserInfo{}, err
	}
	var accounts []bunqAccount
	for _, item := range env.Response {
		for wrapper, raw := range item {
			if !strings.HasPrefix(wrapper, "MonetaryAccount") {
				continue
			}
			var a bunqAccount
			if err := json.Unmarshal(raw, &a); err != nil {
				return nil, bunqUserInfo{}, fmt.Errorf("decode %s: %w", wrapper, err)
			}
			a.Type = bunqAccountType(wrapper)
			accounts = append(accounts, a)
		}
	}
	b.mu.Lock()
	user := bunqUserInfo{ID: b.api.UserID, Type: b.api.UserType, DisplayName: b.api.DisplayName}
	b.mu.Unlock()
	return accounts, user, nil
}

func (b *Bunq) handleListAccounts(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	includeClosed := req.GetBool("include_closed", false)
	accounts, user, err := b.listAccounts(ctx)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	out := make([]map[string]any, 0, len(accounts))
	hidden := 0
	for i := range accounts {
		if !includeClosed && accounts[i].Status != "ACTIVE" {
			hidden++
			continue
		}
		out = append(out, accounts[i].shaped())
	}
	result := map[string]any{
		"user":     map[string]any{"id": user.ID, "type": user.Type, "display_name": user.DisplayName},
		"accounts": out,
	}
	if hidden > 0 {
		result["not_active"] = hidden
		result["note"] = fmt.Sprintf("%d account(s) that are not ACTIVE are hidden; include_closed=true lists them", hidden)
	}
	if b.sandbox {
		result["sandbox"] = true
	}
	return jsonResult(result)
}

// pageQuery builds the paging parameters shared by the list tools.
func bunqPageQuery(req mcp.CallToolRequest) (url.Values, int, error) {
	q := url.Values{}
	count := req.GetInt("count", bunqDefaultCount)
	if count <= 0 {
		return nil, 0, fmt.Errorf("count must be positive")
	}
	if count > bunqMaxCount {
		count = bunqMaxCount
	}
	q.Set("count", strconv.Itoa(count))
	older := strings.TrimSpace(req.GetString("older_id", ""))
	newer := strings.TrimSpace(req.GetString("newer_id", ""))
	if older != "" && newer != "" {
		return nil, 0, fmt.Errorf("older_id and newer_id exclude each other — page in one direction")
	}
	for name, v := range map[string]string{"older_id": older, "newer_id": newer} {
		if v == "" {
			continue
		}
		if _, err := strconv.ParseInt(v, 10, 64); err != nil {
			return nil, 0, fmt.Errorf("%s must be a numeric id, got %q", name, v)
		}
		q.Set(name, v)
	}
	return q, count, nil
}

// accountID reads the account_id argument.
func bunqAccountID(req mcp.CallToolRequest) (string, error) {
	id := req.GetInt("account_id", 0)
	if id <= 0 {
		return "", fmt.Errorf("account_id is required — list_accounts names the accounts")
	}
	return strconv.Itoa(id), nil
}

var (
	bunqDateRe   = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
	bunqStatusRe = regexp.MustCompile(`^[A-Z_]+$`)
)

// bunqSincePause is the pause between the pages of a since-walk; a
// variable so tests can drop it.
var bunqSincePause = 1100 * time.Millisecond

// bunqDay parses a YYYY-MM-DD argument.
func bunqDay(name, v string) (time.Time, error) {
	v = strings.TrimSpace(v)
	if !bunqDateRe.MatchString(v) {
		return time.Time{}, fmt.Errorf("%s must be YYYY-MM-DD, got %q", name, v)
	}
	t, err := time.Parse("2006-01-02", v)
	if err != nil {
		return time.Time{}, fmt.Errorf("%s: %v", name, err)
	}
	return t, nil
}

// bunqCreated parses bunq's timestamp ("2026-11-08 10:36:30.138235", UTC).
func bunqCreated(s string) (time.Time, bool) {
	for _, layout := range []string{"2006-01-02 15:04:05.000000", "2006-01-02 15:04:05", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

func (b *Bunq) handleListPayments(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	account, err := bunqAccountID(req)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	q, _, err := bunqPageQuery(req)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	var since time.Time
	if s := strings.TrimSpace(req.GetString("since", "")); s != "" {
		if q.Get("newer_id") != "" {
			return mcp.NewToolResultError("since pages towards the past and excludes newer_id"), nil
		}
		if since, err = bunqDay("since", s); err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
	}
	payments := make([]map[string]any, 0)
	var olderID, newerID string
	pages := 0
	reached := since.IsZero()
	var partial error
	for {
		if pages > 0 {
			// bunq allows three GETs per three seconds on an endpoint: one
			// page per second keeps a since-walk under it.
			select {
			case <-ctx.Done():
				return mcp.NewToolResultError(ctx.Err().Error()), nil
			case <-time.After(bunqSincePause):
			}
		}
		env, err := b.call(ctx, "payments", map[string]string{"monetary-accountID": account}, q, nil)
		if err != nil {
			if pages == 0 {
				return mcp.NewToolResultError(err.Error()), nil
			}
			// Pages already collected are worth more than the error: the
			// caller continues from older_id.
			partial = err
			break
		}
		pages++
		if pages == 1 {
			newerID = env.Pagination.newerID()
		}
		olderID = env.Pagination.olderID()
		stop := false
		for _, raw := range env.items("Payment") {
			var p bunqPayment
			if err := json.Unmarshal(raw, &p); err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("decode payment: %v", err)), nil
			}
			if !since.IsZero() {
				if t, ok := bunqCreated(p.Created); ok && t.Before(since) {
					stop, reached = true, true
					break
				}
			}
			payments = append(payments, p.shaped())
		}
		if since.IsZero() || stop || olderID == "" || pages >= bunqMaxPages {
			if olderID == "" {
				reached = true
			}
			break
		}
		q.Set("older_id", olderID)
	}
	result := map[string]any{
		"account_id": account,
		"payments":   payments,
		"count":      len(payments),
	}
	if olderID != "" {
		result["older_id"] = olderID
	}
	if newerID != "" {
		result["newer_id"] = newerID
	}
	if !since.IsZero() {
		result["since"] = since.Format("2006-01-02")
		result["since_reached"] = reached && partial == nil
		result["pages"] = pages
		switch {
		case partial != nil:
			result["note"] = fmt.Sprintf("page %d failed (%v) — the %d pages before it are returned; continue with older_id", pages+1, partial, pages)
		case !reached:
			result["note"] = fmt.Sprintf("stopped after %d pages before reaching %s — continue with older_id", pages, since.Format("2006-01-02"))
		}
	}
	return jsonResult(result)
}

func (b *Bunq) handleGetPayment(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	account, err := bunqAccountID(req)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	id := req.GetInt("payment_id", 0)
	if id <= 0 {
		return mcp.NewToolResultError("payment_id is required"), nil
	}
	env, err := b.call(ctx, "payment", map[string]string{"monetary-accountID": account, "itemId": strconv.Itoa(id)}, nil, nil)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	items := env.items("Payment")
	if len(items) == 0 {
		return mcp.NewToolResultError(fmt.Sprintf("no payment %d in the answer", id)), nil
	}
	var full map[string]any
	if err := json.Unmarshal(items[0], &full); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("decode payment: %v", err)), nil
	}
	// Avatars are image references on every label; they carry nothing a
	// reader needs and a lot of bytes.
	bunqDropAvatars(full)
	return jsonResult(full)
}

// bunqDropAvatars removes "avatar" entries wherever they appear.
func bunqDropAvatars(v any) {
	switch t := v.(type) {
	case map[string]any:
		delete(t, "avatar")
		for _, child := range t {
			bunqDropAvatars(child)
		}
	case []any:
		for _, child := range t {
			bunqDropAvatars(child)
		}
	}
}

func (b *Bunq) handleListEvents(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	q, _, err := bunqPageQuery(req)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if id := req.GetInt("account_id", 0); id > 0 {
		q.Set("monetary_account_id", strconv.Itoa(id))
	}
	if s := strings.ToUpper(strings.TrimSpace(req.GetString("status", ""))); s != "" {
		if !bunqStatusRe.MatchString(s) {
			return mcp.NewToolResultError(fmt.Sprintf("status %q is not a bunq status name", s)), nil
		}
		q.Set("status", s)
	}
	env, err := b.call(ctx, "events", nil, q, nil)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	events := make([]map[string]any, 0)
	for _, raw := range env.items("Event") {
		var e bunqEvent
		if err := json.Unmarshal(raw, &e); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("decode event: %v", err)), nil
		}
		events = append(events, e.shaped())
	}
	result := map[string]any{"events": events, "count": len(events)}
	if v := env.Pagination.olderID(); v != "" {
		result["older_id"] = v
	}
	if v := env.Pagination.newerID(); v != "" {
		result["newer_id"] = v
	}
	return jsonResult(result)
}

func (b *Bunq) handleListDrafts(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	account, err := bunqAccountID(req)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	q, _, err := bunqPageQuery(req)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	env, err := b.call(ctx, "drafts", map[string]string{"monetary-accountID": account}, q, nil)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	drafts := make([]map[string]any, 0)
	for _, raw := range env.items("DraftPayment") {
		var d bunqDraft
		if err := json.Unmarshal(raw, &d); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("decode draft payment: %v", err)), nil
		}
		drafts = append(drafts, d.shaped())
	}
	result := map[string]any{"account_id": account, "draft_payments": drafts, "count": len(drafts)}
	if v := env.Pagination.olderID(); v != "" {
		result["older_id"] = v
	}
	if v := env.Pagination.newerID(); v != "" {
		result["newer_id"] = v
	}
	return jsonResult(result)
}

func (b *Bunq) handleExportStatement(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	account, err := bunqAccountID(req)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	start, err := bunqDay("date_start", req.GetString("date_start", ""))
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	end, err := bunqDay("date_end", req.GetString("date_end", ""))
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if end.Before(start) {
		return mcp.NewToolResultError("date_end lies before date_start"), nil
	}
	format := strings.ToUpper(strings.TrimSpace(req.GetString("format", "CSV")))
	switch format {
	case "CSV", "MT940", "PDF":
	default:
		return mcp.NewToolResultError(fmt.Sprintf("format must be CSV, MT940 or PDF, got %q", format)), nil
	}
	regional := strings.ToUpper(strings.TrimSpace(req.GetString("regional_format", "EUROPEAN")))
	if regional != "EUROPEAN" && regional != "UK_US" {
		return mcp.NewToolResultError(fmt.Sprintf("regional_format must be EUROPEAN or UK_US, got %q", regional)), nil
	}
	outputFile := strings.TrimSpace(req.GetString("output_file", ""))
	if outputFile == "" && format == "PDF" {
		if blobStore == nil {
			return mcp.NewToolResultError("a PDF statement needs output_file (an absolute path) — this mux has no blob store"), nil
		}
		outputFile = blobOutputSentinel
	}
	if outputFile != "" {
		if _, err := validateOutputFile(outputFile); err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
	}

	body := map[string]any{
		"statement_format":   format,
		"date_start":         start.Format("2006-01-02"),
		"date_end":           end.Format("2006-01-02"),
		"regional_format":    regional,
		"include_attachment": false,
	}
	params := map[string]string{"monetary-accountID": account}
	env, err := b.call(ctx, "statement.create", params, nil, body)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("create statement: %v", err)), nil
	}
	var id struct {
		ID int64 `json:"id"`
	}
	if err := env.decode("Id", &id); err != nil || id.ID == 0 {
		return mcp.NewToolResultError("create statement: no id in the answer"), nil
	}
	statementID := strconv.FormatInt(id.ID, 10)

	// bunq builds the file on request; the read reports COMPLETED when it is
	// there. The sandbox answers at once, production may take a moment.
	deadline := b.now().Add(bunqStatementWait)
	var st bunqStatement
	for {
		env, err := b.call(ctx, "statement", map[string]string{"monetary-accountID": account, "itemId": statementID}, nil, nil)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("read statement %s: %v", statementID, err)), nil
		}
		// bunq answers under CustomerStatement; ExportStatement is the name
		// in the document. Either carries the status.
		if err := env.decode("CustomerStatement", &st); err != nil {
			if err := env.decode("ExportStatement", &st); err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("read statement %s: no statement in the answer", statementID)), nil
			}
		}
		if st.Status == "COMPLETED" || st.Status == "FAILED" {
			break
		}
		if !b.now().Before(deadline) {
			return jsonResult(map[string]any{
				"statement_id": id.ID,
				"status":       st.Status,
				"note":         fmt.Sprintf("bunq has not finished the statement after %s — read it later with api_get /v1/user/{user}/monetary-account/%s/customer-statement/%s/content", bunqStatementWait, account, statementID),
			})
		}
		select {
		case <-ctx.Done():
			return mcp.NewToolResultError(ctx.Err().Error()), nil
		case <-time.After(bunqStatementPollInterval):
		}
	}
	if st.Status == "FAILED" {
		return mcp.NewToolResultError(fmt.Sprintf("bunq reports statement %s as FAILED", statementID)), nil
	}

	// The content is bytes, not the envelope: fetched through the raw path,
	// which renews a lost session like call does.
	_, userID, err := b.ensureSession(ctx)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	contentPath, err := bunqBuildPath(bunqEndpoints["statement.content"], map[string]string{"userID": fmt.Sprint(userID), "monetary-accountID": account, "customer-statementID": statementID})
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	resp, err := b.callRaw(ctx, http.MethodGet, contentPath, nil)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, bunqMaxBody))
		return mcp.NewToolResultError(fmt.Sprintf("statement %s content: HTTP %d: %s — the statement exists; fetch it with api_get /v1/user/{user}/monetary-account/%s/customer-statement/%s/content", statementID, resp.StatusCode, trimForError(data), account, statementID)), nil
	}
	contentType := resp.Header.Get("Content-Type")
	ext := map[string]string{"CSV": "csv", "MT940": "sta", "PDF": "pdf"}[format]
	name := fmt.Sprintf("bunq-%s-%s-%s.%s", account, start.Format("2006-01-02"), end.Format("2006-01-02"), ext)
	note := fmt.Sprintf("statement %d: account %s, %s to %s, %s", id.ID, account, start.Format("2006-01-02"), end.Format("2006-01-02"), format)
	if outputFile != "" {
		return saveResponseToFileNamed(resp, contentType, outputFile, note, name)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, bunqAPIInlineMax+1))
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("read statement content: %v", err)), nil
	}
	text := note + "\n\n"
	if len(data) > bunqAPIInlineMax {
		text += string(data[:bunqAPIInlineMax]) + fmt.Sprintf("\n\n[truncated at %d bytes — pass output_file to get the whole statement]", bunqAPIInlineMax)
	} else {
		text += string(data)
	}
	return mcp.NewToolResultText(text), nil
}

// --- Writing ---

var (
	bunqAmountRe   = regexp.MustCompile(`^\d{1,13}\.\d{2}$`)
	bunqIBANRe     = regexp.MustCompile(`^[A-Z]{2}\d{2}[A-Z0-9]{11,30}$`)
	bunqCurrencyRe = regexp.MustCompile(`^[A-Z]{3}$`)
)

func (b *Bunq) handleCreateDraft(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	account, err := bunqAccountID(req)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	amount := strings.TrimSpace(req.GetString("amount", ""))
	if !bunqAmountRe.MatchString(amount) {
		return mcp.NewToolResultError(fmt.Sprintf("amount must be a positive number with two decimals (\"12.34\"), got %q", amount)), nil
	}
	if amount == "0.00" {
		return mcp.NewToolResultError("amount must be more than 0.00"), nil
	}
	currency := strings.ToUpper(strings.TrimSpace(req.GetString("currency", "EUR")))
	if !bunqCurrencyRe.MatchString(currency) {
		return mcp.NewToolResultError(fmt.Sprintf("currency must be a three-letter ISO code, got %q", currency)), nil
	}
	iban := strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(req.GetString("counterparty_iban", "")), " ", ""))
	if !bunqIBANRe.MatchString(iban) {
		return mcp.NewToolResultError(fmt.Sprintf("counterparty_iban %q does not look like an IBAN", req.GetString("counterparty_iban", ""))), nil
	}
	name := strings.TrimSpace(req.GetString("counterparty_name", ""))
	if name == "" {
		return mcp.NewToolResultError("counterparty_name is required"), nil
	}
	description := strings.TrimSpace(req.GetString("description", ""))
	if description == "" {
		return mcp.NewToolResultError("description is required"), nil
	}
	if len([]rune(description)) > 140 {
		return mcp.NewToolResultError("description is longer than 140 characters"), nil
	}
	body := map[string]any{
		"entries": []map[string]any{{
			"amount":             bunqAmount{Value: amount, Currency: currency},
			"counterparty_alias": bunqPointer{Type: "IBAN", Value: iban, Name: name},
			"description":        description,
		}},
		"number_of_required_accepts": 1,
	}
	// One idempotency key per call: a repeat after a renewed session sends
	// the same key, so bunq files one draft, not two.
	headers := map[string]string{bunqIdempotencyKey: bunqRequestID()}
	env, err := b.callHeaders(ctx, "draft.create", map[string]string{"monetary-accountID": account}, nil, headers, body)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("create draft payment: %v", err)), nil
	}
	var id struct {
		ID int64 `json:"id"`
	}
	if err := env.decode("Id", &id); err != nil || id.ID == 0 {
		return mcp.NewToolResultError("create draft payment: bunq answered without an id — list_draft_payments shows whether it exists"), nil
	}
	return jsonResult(map[string]any{
		"draft_id":     id.ID,
		"account_id":   account,
		"status":       "PENDING",
		"amount":       amount,
		"currency":     currency,
		"counterparty": map[string]any{"iban": iban, "name": name},
		"description":  description,
		"note":         "filed as a draft — nothing is transferred until the account's owner accepts it in the bunq app; list_draft_payments shows its status",
	})
}

// --- Pass-through ---

// bunqAPIPath checks a caller's path: plain, under /v1/, no encoded or
// dot segments, {user} allowed. The query string may be encoded.
func bunqAPIPath(raw string) (string, error) {
	p := strings.TrimSpace(raw)
	if p == "" {
		return "", fmt.Errorf("path is required")
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	pathPart, query, _ := strings.Cut(p, "?")
	if strings.ContainsAny(pathPart, "%#\\") {
		return "", fmt.Errorf("path %q: write it plain — no percent-encoding, no #, no backslash", raw)
	}
	if !strings.HasPrefix(pathPart, bunqVersionPath+"/") {
		return "", fmt.Errorf("path must start with %s/, got %q", bunqVersionPath, raw)
	}
	for _, seg := range strings.Split(strings.TrimPrefix(pathPart, "/"), "/") {
		if seg == "" || seg == "." || seg == ".." {
			return "", fmt.Errorf("path %q has an empty or dot segment", raw)
		}
		if seg != "{user}" && strings.ContainsAny(seg, "{}") {
			return "", fmt.Errorf("path %q: the only placeholder is {user}", raw)
		}
	}
	if query != "" {
		return pathPart + "?" + query, nil
	}
	return pathPart, nil
}

// bunqWritableResources are the resources api_request may write: the
// draft and its notes, statements and exports. Everything else bunq lets a
// key write — payments, requests, batches, schedules, cards, bunq.me,
// direct debits, whitelists, the user and account settings — is refused
// here, whatever the method.
var bunqWritableResources = map[string]bool{
	"draft-payment":          true,
	"note-text":              true,
	"note-attachment":        true,
	"customer-statement":     true,
	"export-annual-overview": true,
	"export-rib":             true,
}

// bunqWriteAllowed reports whether a write to the path touches a writable
// resource only: the last resource segment (ids skipped) must be in the
// list, and so must every resource segment after monetary-account — a
// payment's note is a note, but a note on a payment is still under
// payment, which the registry does not offer.
func bunqWriteAllowed(path string) (string, bool) {
	pathPart, _, _ := strings.Cut(path, "?")
	segs := strings.Split(strings.TrimPrefix(pathPart, bunqVersionPath+"/"), "/")
	var resources []string
	for _, s := range segs {
		if s == "{user}" {
			continue
		}
		if _, err := strconv.ParseInt(s, 10, 64); err == nil {
			continue
		}
		resources = append(resources, s)
	}
	if len(resources) == 0 {
		return "", false
	}
	last := resources[len(resources)-1]
	for _, r := range resources {
		switch r {
		case "user", "monetary-account", "monetary-account-bank", "monetary-account-savings", "monetary-account-joint":
			continue
		}
		if !bunqWritableResources[r] {
			return r, false
		}
	}
	return last, bunqWritableResources[last]
}

func (b *Bunq) handleAPIGet(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	path, err := bunqAPIPath(req.GetString("path", ""))
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	outputFile := strings.TrimSpace(req.GetString("output_file", ""))
	if outputFile != "" {
		if _, err := validateOutputFile(outputFile); err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
	}
	resp, err := b.callRaw(ctx, http.MethodGet, path, nil)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	defer resp.Body.Close()
	if outputFile != "" && resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return saveResponseToFile(resp, resp.Header.Get("Content-Type"), outputFile)
	}
	return rawHTTPResult(resp, bunqAPIInlineMax, "pass output_file to get the whole body")
}

// bunqDraftWriteRule checks a write that reaches a draft-payment: only the
// creation of a draft is allowed, and only one that needs an acceptance.
// bunq lets the API key accept its own draft with a PUT of status ACCEPTED
// — the money moves at once (sandbox, 2026-11-08) — and a draft with zero
// required accepts would need none; neither must ever leave this connection.
func bunqDraftWriteRule(method, path string, body []byte) error {
	if method != http.MethodPost {
		return fmt.Errorf("%s on a draft-payment is refused: a draft is created here and accepted, rejected or cancelled by the owner in the bunq app — never from this connection", method)
	}
	pathPart, _, _ := strings.Cut(path, "?")
	if !strings.HasSuffix(pathPart, "/draft-payment") {
		return fmt.Errorf("POST %s is refused: a draft is created on the draft-payment collection only", pathPart)
	}
	var in struct {
		Status  *json.RawMessage `json:"status"`
		Accepts *int             `json:"number_of_required_accepts"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		return fmt.Errorf("body of a draft-payment must be a JSON object: %v", err)
	}
	if in.Status != nil {
		return fmt.Errorf("a draft-payment body must not carry status — the status is set by the owner in the bunq app")
	}
	if in.Accepts == nil || *in.Accepts < 1 {
		return fmt.Errorf("a draft-payment body must carry number_of_required_accepts of at least 1 — the owner's acceptance in the app is the point of a draft")
	}
	return nil
}

func (b *Bunq) handleAPIRequest(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	method := strings.ToUpper(strings.TrimSpace(req.GetString("method", "")))
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodDelete:
	default:
		return mcp.NewToolResultError("method must be POST, PUT or DELETE"), nil
	}
	path, err := bunqAPIPath(req.GetString("path", ""))
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	resource, ok := bunqWriteAllowed(path)
	if !ok {
		return mcp.NewToolResultError(fmt.Sprintf("%s %s is refused: %q is not a resource this connection writes — only draft-payment (POST), note-text, note-attachment, customer-statement, export-annual-overview and export-rib are; a transfer goes through create_draft_payment and the owner's acceptance in the app", method, path, resource)), nil
	}
	var body []byte
	if raw := strings.TrimSpace(req.GetString("body", "")); raw != "" {
		if !json.Valid([]byte(raw)) {
			return mcp.NewToolResultError("body must be valid JSON"), nil
		}
		body = []byte(raw)
	} else if method != http.MethodDelete {
		body = []byte("{}")
	}
	if resource == "draft-payment" {
		if err := bunqDraftWriteRule(method, path, body); err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
	}
	resp, err := b.callRaw(ctx, method, path, body)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	defer resp.Body.Close()
	return rawHTTPResult(resp, bunqAPIInlineMax, "")
}
